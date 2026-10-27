// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"internal/testenv"
)

// Built-program behavioral tests for the elastic pacer (M19-S2). This file
// has no restrictive build tag so the raw-linker, plain-Go and non-greentea
// cases stay available in every build, including the inactive wasm build
// that omits the elastic internals. It never references the elastic seam
// (LogosElastic* helpers or the PLAN curve in the active test file); the
// small curve it checks against is defined here for the host trace check.

const (
	logosProgramFloor = 0.25
	logosProgramRise  = 0.80
	logosProgramTop   = 0.95
)

func logosProgramPlan(u float64) float64 {
	switch {
	case u < logosProgramRise:
		return logosProgramFloor
	case u >= logosProgramTop:
		return 0.75
	}
	return logosProgramFloor + (0.75-logosProgramFloor)*(u-logosProgramRise)/(logosProgramTop-logosProgramRise)
}

func logosProgramNearly(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

var logosElasticLine = regexp.MustCompile(`pacer: elastic u=(\S+) util=(\S+) workers=(\d+)\+(\S+)`)

func logosPacerRun(t *testing.T, prog string, flags ...string) string {
	t.Helper()
	exe, err := buildTestProg(t, "testprog", flags...)
	if err != nil {
		t.Fatal(err)
	}
	out := runBuiltTestProg(t, exe, prog, "GODEBUG=gcpacertrace=1", "GOGC=100", "GOMEMLIMIT=off")
	if !strings.Contains(out, "OK\n") {
		t.Fatalf("helper program %s did not finish:\n%s", prog, out)
	}
	if !strings.Contains(out, "pacer: assist ratio=") {
		t.Fatalf("helper program %s printed no pacer trace:\n%s", prog, out)
	}
	return out
}

func logosPacerTrace(t *testing.T, flags ...string) string {
	t.Helper()
	return logosPacerRun(t, "LogosPacerElastic", flags...)
}

func logosFloat(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("bad number %q in pacer trace: %v", s, err)
	}
	return v
}

func logosCheckElasticTrace(t *testing.T, out string) {
	t.Helper()
	lines := logosElasticLine.FindAllStringSubmatch(out, -1)
	if len(lines) < 3 {
		t.Fatalf("pacer trace has %d elastic lines, want at least 3:\n%s", len(lines), out)
	}
	peak := 0.0
	for _, m := range lines {
		u, util := logosFloat(t, m[1]), logosFloat(t, m[2])
		dedicated, fractional := logosFloat(t, m[3]), logosFloat(t, m[4])
		if want := logosProgramPlan(u); !logosProgramNearly(util, want, 1e-5) {
			t.Errorf("trace u=%v: utilization %v, want %v", u, util, want)
		}
		if got := (dedicated + fractional*(4-dedicated)) / 4; !logosProgramNearly(got, util, 1e-5) {
			t.Errorf("trace u=%v: workers %v+%v give utilization %v, want %v", u, dedicated, fractional, got, util)
		}
		peak = math.Max(peak, util)
	}
	if peak <= logosProgramFloor+1e-3 {
		t.Errorf("no cycle started above the floor utilization; trace:\n%s", out)
	}
}

func TestLogosElasticLinkerModeInBuiltProgram(t *testing.T) {
	logosCheckElasticTrace(t, logosPacerTrace(t, "-ldflags=-X=runtime.logosGCMode=greentea"))
}

// TestLogosElasticGCFlagInBuiltProgram: go build -gc=greentea of a Go helper
// program runs the elastic pacing (T-301.2).
func TestLogosElasticGCFlagInBuiltProgram(t *testing.T) {
	logosCheckElasticTrace(t, logosPacerTrace(t, "-gc=greentea"))
}

func TestLogosElasticAssistsRunInBuiltProgram(t *testing.T) {
	out := logosPacerRun(t, "LogosPacerAssist", "-ldflags=-X=runtime.logosGCMode=greentea")
	if !logosElasticLine.MatchString(out) {
		t.Fatalf("the program did not pace elastically:\n%s", out)
	}
}

// TestLogosPlainBuildPacesElastically: a go build with no -gc paces
// elastically through the empty production-controller mode.
func TestLogosPlainBuildPacesElastically(t *testing.T) {
	logosCheckElasticTrace(t, logosPacerTrace(t))
}

// TestLogosOtherGCFlagKeepsUpstreamPacingInBuiltProgram: a mode other than
// greentea keeps the upstream pacing even though it names a collector mode.
func TestLogosOtherGCFlagKeepsUpstreamPacingInBuiltProgram(t *testing.T) {
	out := logosPacerRun(t, "LogosPacerForced", "-gc=batch")
	if logosElasticLine.MatchString(out) {
		t.Fatalf("-gc=batch paces elastically:\n%s", out)
	}
}

func TestLogosElasticForcedCycleInBuiltProgram(t *testing.T) {
	out := logosPacerRun(t, "LogosPacerForced", "-gc=greentea")
	if got := len(logosElasticLine.FindAllString(out, -1)); got < 2 {
		t.Fatalf("two forced collections printed %d elastic lines, want at least 2:\n%s", got, out)
	}
}

// Wasm execution coverage for the pacer specialization. Each case builds a
// tiny main package for GOOS=js GOARCH=wasm with the named command and flags
// and runs it under node with the existing lib/wasm executor. Without node
// the run is blocked (A-NODE) and the test skips; the command plan check
// still runs because it needs no engine.

const logosProgramWasmMain = `package main

import (
	"fmt"
	"runtime"
)

func main() {
	runtime.GC()
	runtime.GC()
	fmt.Println("done")
}
`

func logosProgramWasmNode(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found: the wasm run is blocked (A-NODE)")
	}
	return p
}

func logosProgramToolPath(t *testing.T, tool string) string {
	t.Helper()
	goTool := testenv.GoToolPath(t)
	if tool == "go" {
		return goTool
	}
	p := filepath.Join(filepath.Dir(goTool), "logos"+filepath.Ext(goTool))
	if _, err := os.Stat(p); err != nil {
		t.Skipf("logos command missing next to go: %v", err)
	}
	return p
}

func logosProgramBuildWasm(t *testing.T, tool string, env []string, flags ...string) string {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module wasmpacer\n\ngo 1.27\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(logosProgramWasmMain), 0o666); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "app.wasm")
	args := append([]string{"build", "-o", out}, flags...)
	args = append(args, ".")
	cmd := testenv.Command(t, logosProgramToolPath(t, tool), args...)
	cmd.Dir = dir
	base := testenv.CleanCmdEnv(cmd).Env
	// Keep the parent environment without collector knobs, then apply the
	// case environment (GOOS/GOARCH and any GODEBUG for the build).
	var kept []string
	for _, kv := range base {
		switch {
		case strings.HasPrefix(kv, "GOGC="), strings.HasPrefix(kv, "GOMEMLIMIT="),
			strings.HasPrefix(kv, "GODEBUG="), strings.HasPrefix(kv, "GOMAXPROCS="),
			strings.HasPrefix(kv, "GOOS="), strings.HasPrefix(kv, "GOARCH="):
		default:
			kept = append(kept, kv)
		}
	}
	cmd.Env = append(kept, env...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", tool, strings.Join(args, " "), err, b)
	}
	return out
}

func logosProgramRunWasm(t *testing.T, wasm string, env []string) string {
	t.Helper()
	node := logosProgramWasmNode(t)
	host := filepath.Join(testenv.GOROOT(t), "lib", "wasm", "wasm_exec_node.js")
	cmd := testenv.Command(t, node, "--stack-size=8192", host, wasm)
	var base []string
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "GOGC="), strings.HasPrefix(kv, "GOMEMLIMIT="),
			strings.HasPrefix(kv, "GODEBUG="), strings.HasPrefix(kv, "GOMAXPROCS="):
		default:
			base = append(base, kv)
		}
	}
	cmd.Env = append(base, env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("node %s: %v\n%s", wasm, err, out.String())
	}
	return out.String()
}

func logosProgramWasmEnv() []string {
	return []string{"GOOS=js", "GOARCH=wasm", "GOEXPERIMENT=", "GOTOOLCHAIN=local", "GOPROXY=off"}
}

func TestLogosPacerWasmDefaultLogosKeepsUpstream(t *testing.T) {
	env := logosProgramWasmEnv()
	wasm := logosProgramBuildWasm(t, "logos", env)
	out := logosProgramRunWasm(t, wasm, []string{"GODEBUG=gcpacertrace=1", "GOGC=100", "GOMEMLIMIT=off"})
	if !strings.Contains(out, "done") {
		t.Fatalf("wasm program did not finish:\n%s", out)
	}
	if logosElasticLine.MatchString(out) {
		t.Fatalf("logos default wasm paces elastically:\n%s", out)
	}
}

// TestLogosPacerWasmPlainGoPacesElastically: plain go wasm has an empty
// production-controller mode so it paces elastically; only the logos wasm
// default (mode wasm) keeps the upstream pacing.
func TestLogosPacerWasmPlainGoPacesElastically(t *testing.T) {
	env := logosProgramWasmEnv()
	wasm := logosProgramBuildWasm(t, "go", env)
	out := logosProgramRunWasm(t, wasm, []string{"GODEBUG=gcpacertrace=1", "GOGC=100", "GOMEMLIMIT=off"})
	if !strings.Contains(out, "done") {
		t.Fatalf("wasm program did not finish:\n%s", out)
	}
	if !logosElasticLine.MatchString(out) {
		t.Fatalf("plain go wasm did not pace elastically:\n%s", out)
	}
}

func TestLogosPacerWasmGreenteaPacesElastically(t *testing.T) {
	env := logosProgramWasmEnv()
	wasm := logosProgramBuildWasm(t, "go", env, "-gc=greentea")
	out := logosProgramRunWasm(t, wasm, []string{"GODEBUG=gcpacertrace=1", "GOGC=100", "GOMEMLIMIT=off"})
	if !strings.Contains(out, "done") {
		t.Fatalf("wasm program did not finish:\n%s", out)
	}
	if !logosElasticLine.MatchString(out) {
		t.Fatalf("explicit greentea wasm did not pace elastically:\n%s", out)
	}
}

func TestLogosPacerWasmRawOverrideWinsOverGCFlag(t *testing.T) {
	env := logosProgramWasmEnv()
	// Contradictory selection: -gc=wasm names the upstream wasm mode while
	// the raw linker flag names greentea. The linker keeps the last -X, so
	// the raw flag wins and the program must pace elastically. The custom
	// ldflags also force the full implementation to be kept.
	wasm := logosProgramBuildWasm(t, "go", env, "-gc=wasm", "-ldflags=-X=runtime.logosGCMode=greentea")
	out := logosProgramRunWasm(t, wasm, []string{"GODEBUG=gcpacertrace=1", "GOGC=100", "GOMEMLIMIT=off"})
	if !strings.Contains(out, "done") {
		t.Fatalf("wasm program did not finish:\n%s", out)
	}
	if !logosElasticLine.MatchString(out) {
		t.Fatalf("raw -X=greentea over -gc=wasm did not pace elastically:\n%s", out)
	}
}
