// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file builds and installs the logos-nogo helper and its source digest
// sidecar. The helper module lives at $GOROOT/lib/logos-nogo (module
// logos/nogo) and cannot be built with the normal toolsToInstall path,
// which runs "go install" on packages under $GOROOT/src. Building it as
// cmd/logos-nogo would fail because that package does not exist.
//
// cmd/dist must not import a shared hashing package for this: at bootstrap
// time the host GOROOT (the bootstrap toolchain) has no such package, so any
// cmd/internal helper would be missing. Instead dist builds the host helper
// with the just-built go command, then runs that host helper as
// "logos-nogo -source-hash <moduleDir>" to compute the digest. The digest
// algorithm therefore lives once in lib/logos-nogo/cmd/logos-nogo
// (sourcehash.go) and dist and cmd/go both invoke it through the helper
// binary, never duplicating it in Go.
//
// Layout (absolute paths under $GOROOT):
//
//	pkg/tool/<GOOS>_<GOARCH>/logos-nogo[.exe]          helper binary
//	pkg/tool/<GOOS>_<GOARCH>/logos-nogo[.exe].digest  hex digest + "\n"
//
// The digest covers every non-testdata file under the module, with _go.mod
// normalized back to go.mod so a module-zip install (which renames go.mod
// to _go.mod) matches the build-time manifest. cmd/go runs the prebuilt
// helper with -source-hash over the installed module and uses the prebuilt
// binary only when the recomputed digest equals the sidecar; any source
// edit invalidates it and falls back to a readonly module build. Untouched
// distributed installs therefore never touch the module cache or the
// network.
//
// Module policy. The helper module requires pinned public modules (see its
// go.mod and go.sum), so building it from source is the one dist step that
// needs a module cache. Everything else in the bootstrap stays isolated: dist
// sets GOENV=off, points GOPATH inside $GOROOT and sets GOPROXY=off so that
// toolchain1/2/3 and std/cmd never download into a read-only module cache
// under $GOROOT (golang.org/issue/67463). The helper build re-applies only the
// author's module policy, resolved by the just-built $GOROOT/bin/go from the
// environment dist was started in (GOENV, GOPATH, GOPROXY as captured by
// captureLogosNogoAuthorEnv, everything else as inherited). go_bootstrap
// cannot do this work: it is built with cmd_go_bootstrap, so it has no HTTP
// or file proxy transport and never consults the checksum database.
//
//	GOMODCACHE  the author's real module cache; must be absolute and outside
//	            $GOROOT, never an invented path
//	GOPROXY, GOSUMDB, GONOSUMDB, GOPRIVATE, GONOPROXY, GOINSECURE, GOVCS,
//	GOAUTH      fetch and verification policy, including an intentional
//	            GOPROXY=off
//
// The build keeps -mod=readonly, GOWORK=off and GOTOOLCHAIN=local, so go.mod
// and go.sum are never rewritten and every module is verified against go.sum.
// A missing module under an offline policy, or an unreachable or unverifiable
// one under an online policy, makes dist fail loudly with the go command's own
// error; there is no fallback to the isolated GOPROXY=off environment.

// logosNogoAuthorVar is one environment variable as the author's environment
// had it when dist started: set (possibly to the empty string) or unset.
type logosNogoAuthorVar struct {
	name  string
	value string
	set   bool
}

// logosNogoAuthorEnv holds the variables dist later overrides, captured
// before any override.
var logosNogoAuthorEnv []logosNogoAuthorVar

// logosNogoModuleVars lists the variables whose resolved values form the
// helper build's module policy, in the order logosNogoModuleEnv requests them
// from "go env".
var logosNogoModuleVars = []string{
	"GOMODCACHE",
	"GOPROXY",
	"GOSUMDB",
	"GONOSUMDB",
	"GOPRIVATE",
	"GONOPROXY",
	"GOINSECURE",
	"GOVCS",
	"GOAUTH",
}

// captureLogosNogoAuthorEnv records the author's GOENV, GOPATH and GOPROXY.
// xinit calls it before setting GOENV=off; cmdbootstrap overrides GOPATH and
// GOPROXY later, so this one early capture sees all three untouched.
func captureLogosNogoAuthorEnv() {
	logosNogoAuthorEnv = logosNogoAuthorEnv[:0]
	for _, name := range []string{"GOENV", "GOPATH", "GOPROXY"} {
		value, set := os.LookupEnv(name)
		logosNogoAuthorEnv = append(logosNogoAuthorEnv, logosNogoAuthorVar{name, value, set})
	}
}

// logosNogoAuthorGOENVOff reports whether GOENV was "off" when dist started.
// make.bat and make.rc always arrange that through "dist env", and make.bash
// does when GOENV is exported; the author's "go env -w" file is then not
// visible to dist.
func logosNogoAuthorGOENVOff() bool {
	for _, v := range logosNogoAuthorEnv {
		if v.name == "GOENV" {
			return v.set && v.value == "off"
		}
	}
	return false
}

// setLogosNogoEnv sets or unsets each variable in the process environment.
func setLogosNogoEnv(vars []logosNogoAuthorVar) {
	for _, v := range vars {
		if v.set {
			os.Setenv(v.name, v.value)
		} else {
			os.Unsetenv(v.name)
		}
	}
}

// logosNogoAuthorEnviron returns the process environment with the author's
// GOENV, GOPATH and GOPROXY put back, leaving dist's own environment intact.
func logosNogoAuthorEnviron() []string {
	dist := make([]logosNogoAuthorVar, len(logosNogoAuthorEnv))
	for i, v := range logosNogoAuthorEnv {
		dist[i].name = v.name
		dist[i].value, dist[i].set = os.LookupEnv(v.name)
	}
	setLogosNogoEnv(logosNogoAuthorEnv)
	env := os.Environ()
	setLogosNogoEnv(dist)
	return env
}

// logosNogoWithin reports whether path is root or lies below it, comparing
// both lexically and after resolving symlinks in the longest existing prefix.
func logosNogoWithin(root, path string) bool {
	for _, r := range []string{filepath.Clean(root), logosNogoResolve(root)} {
		for _, p := range []string{filepath.Clean(path), logosNogoResolve(path)} {
			rel, err := filepath.Rel(r, p)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// logosNogoResolve resolves symlinks in the longest existing prefix of path
// and appends the nonexistent remainder; it returns path if nothing resolves.
func logosNogoResolve(path string) string {
	path = filepath.Clean(path)
	var rest []string
	for p := path; ; {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// logosNogoModuleEnv resolves the author's module policy with goBinary and
// returns it as NAME=value assignments for the helper build, plus the module
// cache path for reporting. It fatals if go cannot resolve the policy or the
// module cache is empty, relative, or inside $GOROOT.
func logosNogoModuleEnv(goBinary, moduleDir string) (env []string, modCache string) {
	cmd := exec.Command(goBinary, append([]string{"env"}, logosNogoModuleVars...)...)
	cmd.Env = append(logosNogoAuthorEnviron(), "GOTOOLCHAIN=local")
	setDir(cmd, moduleDir)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		fatalf("logos-nogo: cannot resolve the author's module policy with %s env: %v\n%s", goBinary, err, stderr.String())
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(values) != len(logosNogoModuleVars) {
		fatalf("logos-nogo: %s env %s printed %d lines, want %d:\n%s", goBinary, strings.Join(logosNogoModuleVars, " "), len(values), len(logosNogoModuleVars), out)
	}
	for i, name := range logosNogoModuleVars {
		values[i] = strings.TrimSuffix(values[i], "\r")
		env = append(env, name+"="+values[i])
	}
	modCache = values[0]
	switch {
	case modCache == "":
		fatalf("logos-nogo: the author's module cache is unresolved; set GOMODCACHE, or GOPATH and HOME, in the environment make was started from")
	case !filepath.IsAbs(modCache):
		fatalf("logos-nogo: the author's GOMODCACHE %q is not an absolute path", modCache)
	case logosNogoWithin(goroot, modCache):
		fatalf("logos-nogo: the author's GOMODCACHE %s is inside GOROOT %s; point GOMODCACHE (or GOPATH) at a writable directory outside GOROOT", modCache, goroot)
	}
	return env, modCache
}

// logosNogoModuleDir returns the helper module directory.
func logosNogoModuleDir() string {
	return pathf("%s/lib/logos-nogo", goroot)
}

// logosNogoToolPath returns the installed helper binary path for a platform.
func logosNogoToolPath(targetGOOS, targetGOARCH string) string {
	suffix := ""
	if targetGOOS == "windows" {
		suffix = ".exe"
	}
	return pathf("%s/pkg/tool/%s_%s/logos-nogo%s", goroot, targetGOOS, targetGOARCH, suffix)
}

// logosNogoDigestPath returns the sidecar digest path for a helper binary.
func logosNogoDigestPath(toolPath string) string {
	return toolPath + ".digest"
}

// logosNogoBuild builds the helper module for one platform with the
// just-built go command in readonly mode under the author's module policy
// (modEnv, from logosNogoModuleEnv). Readonly mode verifies every module
// against go.sum, so the shipped helper always matches the pins in
// go.mod; the vendored copy is not consulted here, because nothing checks
// it against those pins and a drifted vendor tree would ship silently.
// Failures are loud: runEnv with CheckExit calls fatalf, never a silent
// fallback, so a build without a module cache reports the missing fetch
// rather than quietly changing which sources it compiles.
func logosNogoBuild(goBinary, moduleDir, outPath, targetGOOS, targetGOARCH string, modEnv []string) {
	xmkdirall(filepath.Dir(outPath))
	var env []string
	if !mustLinkExternal(targetGOOS, targetGOARCH, false) {
		env = append(env, "CGO_ENABLED=0")
	}
	env = append(env, modEnv...)
	env = append(env,
		"GOOS="+targetGOOS,
		"GOARCH="+targetGOARCH,
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=readonly -buildvcs=false",
	)
	runEnv(moduleDir, ShowOutput|CheckExit, env, goBinary, "build", "-mod=readonly", "-o", outPath, "./cmd/logos-nogo")
}

// logosNogoDigestVia runs the host helper to hash the module directory.
func logosNogoDigestVia(hostHelper, moduleDir string) string {
	out := runEnv("", CheckExit, nil, hostHelper, "-source-hash", moduleDir)
	digest := strings.TrimSpace(out)
	if digest == "" {
		fatalf("logos-nogo: empty source digest from %s", hostHelper)
	}
	return digest
}

// installLogosNogo builds the host helper with goBinary, the just-built
// $GOROOT/bin/go, records its source digest, and installs the target helper
// when cross-compiling. The same digest is stored beside both binaries; it
// describes sources, not binaries, so host and target share it.
func installLogosNogo(goBinary string) {
	moduleDir := logosNogoModuleDir()
	if !isdir(moduleDir) {
		fatalf("logos-nogo module missing: %s", moduleDir)
	}
	if vflag > 0 {
		xprintf("Building logos-nogo for host %s/%s.\n", gohostos, gohostarch)
	}
	modEnv, modCache := logosNogoModuleEnv(goBinary, moduleDir)
	xprintf("Building logos-nogo from source with the author's module policy (GOMODCACHE=%s).\n", modCache)
	if logosNogoAuthorGOENVOff() {
		xprintf("logos-nogo: GOENV=off was set when dist started, so 'go env -w' settings are not applied; the module policy comes from environment variables only.\n")
	}
	hostTool := logosNogoToolPath(gohostos, gohostarch)
	logosNogoBuild(goBinary, moduleDir, hostTool, gohostos, gohostarch, modEnv)
	digest := logosNogoDigestVia(hostTool, moduleDir)
	writefile(digest+"\n", logosNogoDigestPath(hostTool), 0)
	if goos != gohostos || goarch != gohostarch {
		if vflag > 0 {
			xprintf("Building logos-nogo for target %s/%s.\n", goos, goarch)
		}
		targetTool := logosNogoToolPath(goos, goarch)
		logosNogoBuild(goBinary, moduleDir, targetTool, goos, goarch, modEnv)
		writefile(digest+"\n", logosNogoDigestPath(targetTool), 0)
	}
}
