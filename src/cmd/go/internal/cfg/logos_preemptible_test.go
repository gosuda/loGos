// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

import (
	"go/build"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func withPreemptibleState(t *testing.T, logos bool, goarch, userExperiment string) {
	t.Helper()
	oldLogos, oldRaw, oldInit, oldGoarch := Logos, RawGOEXPERIMENT, logosInitGOEXPERIMENT, Goarch
	oldTags := slices.Clone(BuildContext.ToolTags)
	oldEnv := os.Getenv("GOEXPERIMENT")
	t.Cleanup(func() {
		Logos, RawGOEXPERIMENT, logosInitGOEXPERIMENT, Goarch = oldLogos, oldRaw, oldInit, oldGoarch
		BuildContext.ToolTags = oldTags
		os.Setenv("GOEXPERIMENT", oldEnv)
		recomputeExperiment()
	})
	Logos = logos
	Goarch = goarch
	RawGOEXPERIMENT = userExperiment
	logosInitGOEXPERIMENT = userExperiment
	t.Setenv("GOEXPERIMENT", userExperiment)
	recomputeExperiment()
}

func preemptibleTagCount() int {
	n := 0
	for _, tag := range BuildContext.ToolTags {
		if tag == "goexperiment.preemptibleloops" {
			n++
		}
	}
	return n
}

func TestLogosPreemptibleUserState(t *testing.T) {
	for _, tc := range []struct {
		raw               string
		mentioned, enable bool
	}{
		{"", false, false},
		{"nojsonv2", false, false},
		{"preemptibleloops", true, true},
		{"nopreemptibleloops", true, false},
		{"none", true, false},
		{"none,preemptibleloops", true, true},
		{"preemptibleloops,none", true, false},
		{"nojsonv2,nopreemptibleloops", true, false},
	} {
		mentioned, enabled := logosPreemptibleUserState(tc.raw)
		if mentioned != tc.mentioned || enabled != tc.enable {
			t.Errorf("logosPreemptibleUserState(%q) = (%v, %v), want (%v, %v)",
				tc.raw, mentioned, enabled, tc.mentioned, tc.enable)
		}
	}
}

func TestRefreshLogosPreemptibleDefaultsOnWasm(t *testing.T) {
	withPreemptibleState(t, true, "wasm", "")
	if Experiment.PreemptibleLoops {
		t.Fatal("experiment on before refresh with empty GOEXPERIMENT")
	}
	for range 2 {
		RefreshLogosPreemptible()
		if !Experiment.PreemptibleLoops || RawGOEXPERIMENT != "preemptibleloops" {
			t.Fatalf("after refresh: PreemptibleLoops=%v Raw=%q, want the wasm default on",
				Experiment.PreemptibleLoops, RawGOEXPERIMENT)
		}
		if !strings.Contains(CleanGOEXPERIMENT, "preemptibleloops") {
			t.Fatalf("CleanGOEXPERIMENT=%q, want the default in the cache key", CleanGOEXPERIMENT)
		}
		if n := preemptibleTagCount(); n != 1 {
			t.Fatalf("goexperiment.preemptibleloops tool tag appears %d times, want once", n)
		}
		if got := os.Getenv("GOEXPERIMENT"); got != "preemptibleloops" {
			t.Fatalf("process GOEXPERIMENT = %q, want preemptibleloops", got)
		}
	}
}

func TestRefreshLogosPreemptiblePlainGoUnchanged(t *testing.T) {
	withPreemptibleState(t, false, "wasm", "")
	RefreshLogosPreemptible()
	if Experiment.PreemptibleLoops || RawGOEXPERIMENT != "" || preemptibleTagCount() != 0 {
		t.Errorf("plain go wasm build: PreemptibleLoops=%v Raw=%q tags=%d, want unchanged until G3",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT, preemptibleTagCount())
	}
}

func TestRefreshLogosPreemptibleNonWasmUnchanged(t *testing.T) {
	withPreemptibleState(t, true, "amd64", "")
	RefreshLogosPreemptible()
	if Experiment.PreemptibleLoops || RawGOEXPERIMENT != "" || preemptibleTagCount() != 0 {
		t.Errorf("logos amd64 build: PreemptibleLoops=%v Raw=%q tags=%d, want the default off",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT, preemptibleTagCount())
	}
}

func TestRefreshLogosPreemptibleRespectsUserOptOut(t *testing.T) {
	for _, raw := range []string{
		"nopreemptibleloops",
		"none",
		"preemptibleloops,none",
		"nojsonv2,nopreemptibleloops",
	} {
		withPreemptibleState(t, true, "wasm", raw)
		RefreshLogosPreemptible()
		if Experiment.PreemptibleLoops {
			t.Errorf("GOEXPERIMENT=%q: default overrode an explicit user opt-out", raw)
		}
		if RawGOEXPERIMENT != raw {
			t.Errorf("GOEXPERIMENT=%q: Raw=%q, want untouched", raw, RawGOEXPERIMENT)
		}
	}
}

func TestRefreshLogosPreemptibleKeepsExplicitEnable(t *testing.T) {
	withPreemptibleState(t, true, "wasm", "preemptibleloops")
	RefreshLogosPreemptible()
	if !Experiment.PreemptibleLoops || RawGOEXPERIMENT != "preemptibleloops" {
		t.Errorf("explicit preemptibleloops: PreemptibleLoops=%v Raw=%q, want kept without duplication",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT)
	}
	withPreemptibleState(t, true, "amd64", "preemptibleloops")
	RefreshLogosPreemptible()
	if !Experiment.PreemptibleLoops || RawGOEXPERIMENT != "preemptibleloops" {
		t.Errorf("explicit preemptibleloops on amd64: PreemptibleLoops=%v Raw=%q, want kept",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT)
	}
}

func TestRefreshLogosPreemptibleKeepsOtherExperiments(t *testing.T) {
	withPreemptibleState(t, true, "wasm", "nojsonv2")
	RefreshLogosPreemptible()
	if !Experiment.PreemptibleLoops || Experiment.JSONv2 || RawGOEXPERIMENT != "nojsonv2,preemptibleloops" {
		t.Errorf("PreemptibleLoops=%v JSONv2=%v Raw=%q, want the default added to the user's nojsonv2",
			Experiment.PreemptibleLoops, Experiment.JSONv2, RawGOEXPERIMENT)
	}
}

func TestRefreshLogosPreemptibleStripsOnRetarget(t *testing.T) {
	withPreemptibleState(t, true, "wasm", "")
	RefreshLogosPreemptible()
	if !Experiment.PreemptibleLoops {
		t.Fatal("wasm default did not apply")
	}
	Goarch = "amd64"
	RefreshLogosPreemptible()
	if Experiment.PreemptibleLoops || RawGOEXPERIMENT != "" || preemptibleTagCount() != 0 {
		t.Errorf("after retarget: PreemptibleLoops=%v Raw=%q tags=%d, want the wasm default stripped",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT, preemptibleTagCount())
	}
}

// withForceHostGlobals snapshots every global ForceHost rewrites, so a
// host-retarget test restores the suite state exactly. Both ForceHost
// tests share it instead of each listing a subset that can drift.
func withForceHostGlobals(t *testing.T) {
	t.Helper()
	oldGoos, oldGoarch, oldExe := Goos, Goarch, ExeSuffix
	old386, oldAmd64, oldArm, oldArm64 := GO386, GOAMD64, GOARM, GOARM64
	oldMips, oldMips64, oldPpc64, oldRiscv64, oldWasm := GOMIPS, GOMIPS64, GOPPC64, GORISCV64, GOWASM
	oldCtx, oldToolDir := BuildContext, build.ToolDir
	oldGoroot, oldBin, oldPkg, oldSrc := GOROOT, GOROOTbin, GOROOTpkg, GOROOTsrc
	t.Cleanup(func() {
		Goos, Goarch, ExeSuffix = oldGoos, oldGoarch, oldExe
		GO386, GOAMD64, GOARM, GOARM64 = old386, oldAmd64, oldArm, oldArm64
		GOMIPS, GOMIPS64, GOPPC64, GORISCV64, GOWASM = oldMips, oldMips64, oldPpc64, oldRiscv64, oldWasm
		BuildContext, build.ToolDir = oldCtx, oldToolDir
		GOROOT, GOROOTbin, GOROOTpkg, GOROOTsrc = oldGoroot, oldBin, oldPkg, oldSrc
		recomputeExperiment()
	})
}

func TestForceHostDropsWasmDefault(t *testing.T) {
	if runtime.GOARCH == "wasm" {
		t.Skip("host is wasm: ForceHost cannot drop the default")
	}
	withPreemptibleState(t, true, "wasm", "")
	withForceHostGlobals(t)
	RefreshLogosPreemptible()
	if !Experiment.PreemptibleLoops {
		t.Fatal("wasm default did not apply")
	}

	ForceHost()
	if Experiment.PreemptibleLoops || RawGOEXPERIMENT != "" || preemptibleTagCount() != 0 {
		t.Errorf("after ForceHost: PreemptibleLoops=%v Raw=%q tags=%d, want the wasm default gone",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT, preemptibleTagCount())
	}
}

func TestForceHostKeepsExplicitPreemptible(t *testing.T) {
	if runtime.GOARCH == "wasm" {
		t.Skip("host is wasm: nothing to strip")
	}
	withPreemptibleState(t, true, "wasm", "preemptibleloops")
	withForceHostGlobals(t)

	ForceHost()
	if !Experiment.PreemptibleLoops || RawGOEXPERIMENT != "preemptibleloops" {
		t.Errorf("after ForceHost: PreemptibleLoops=%v Raw=%q, want the explicit user experiment kept",
			Experiment.PreemptibleLoops, RawGOEXPERIMENT)
	}
}
