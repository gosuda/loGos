// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package base

import (
	"flag"
	"internal/buildcfg"
	"log"
)

// LogosEject is the -logos.eject output directory. After checkFiles
// succeeds, the compiler writes one ejected Go file per logos source by
// applying the lowering edits to the original bytes. Empty means no eject.
var LogosEject string

// Logos reports whether the package being compiled came from loGo/s
// sources. The archive writer uses it to mark the source language.
// Go packages can also carry analysis facts in the __.LOGOS sidecar.
var Logos bool

// LogosMode selects a compiler helper mode: next rewrites Go to loGo/s,
// context prints one symbol context, hints writes the editor hints of the
// package. Empty compiles normally.
var LogosMode string

// LogosHintsOut is the -logos.hintsout file. With -logos.mode=hints the
// compiler writes one JSON report there after escape analysis and stops
// before SSA and the backend, so no object file is written. Hints mode
// requires it.
var LogosHintsOut string

// LogosNextOut is the -logos.nextout sidecar path. After checkFiles
// succeeds with -logos.mode=next, the compiler appends one JSON line
// describing the package migration rewrites to this file. Empty means
// no report.
var LogosNextOut string

// LogosCfg is the -logos.cfg file: the logos.toml [lints] policy as JSON,
// written by the go command. A loGo/s package runs its lints on every
// compile when it is set; a Go package runs them only with LogosLintOnly.
// Empty means no lints.
var LogosCfg string

// LogosJSON is the -logos.json flag: every compiler diagnostic prints as
// one JSON object per line (SPEC Sec 16) instead of text.
var LogosJSON bool

// LogosLintOnly is the -logos.lintonly flag, passed only by logos lint:
// run the lints of a Go or loGo/s package, run escape analysis for the
// escape rules, and stop before SSA and the backend.
var LogosLintOnly bool

// LogosComptimeNodes caps the expanded nodes of one comptime expansion
// (SPEC Sec 2.5, T-205, #150). The selected-limit tests use the selected
// value; logos.toml [lints.thresholds] max_comptime_ast_nodes wires to
// this flag in M9.
var LogosComptimeNodes = 4096

// logosRegisterFlags registers the loGo/s compiler flags.
func logosRegisterFlags() {
	flag.StringVar(&LogosCfg, "logos.cfg", "", "read the lint policy from `file`")
	flag.BoolVar(&LogosJSON, "logos.json", false, "print diagnostics as JSON lines")
	flag.BoolVar(&LogosLintOnly, "logos.lintonly", false, "lint the package and stop after escape analysis")
	flag.StringVar(&LogosEject, "logos.eject", "", "write ejected Go sources to `directory`")
	flag.StringVar(&LogosMode, "logos.mode", "", "run a logos helper mode (`next`, `context`, or `hints`)")
	flag.StringVar(&LogosHintsOut, "logos.hintsout", "", "write the logos editor hints of the package to `file`")
	flag.StringVar(&LogosNextOut, "logos.nextout", "", "append logos next scan results to `file`")
	flag.IntVar(&LogosComptimeNodes, "logos.comptime_nodes", 4096, "cap expanded nodes per comptime expansion")
	flag.BoolVar(&LogosLoadBarrier, "lb", false, "emit zgc load barriers on pointer loads")
}

// LogosLoadBarrier is the -lb flag, which the go command passes under
// -gc=zgc (SPEC Sec 8.4, M19-S5 phase 2): every pointer load tests the
// color of the loaded word and calls runtime.zgcLoadBarrierSlow on a
// mismatch. The runtime half exists only under GOEXPERIMENT=logosgcmoving
// on amd64 and arm64.
var LogosLoadBarrier bool

// logosCheckFlags checks the loGoS compiler flags once CompilingRuntime is
// known. Under -gc=zgc the runtime and internal/runtime/maps compile with
// -lb too: most of their pointer loads take the barrier while the
// excluded files (barrier implementation, GC scanners, allocator
// bit-manipulation, signal delivery, stack growth) and the named
// runtime syscall entries keep plain loads with explicit
// canonicalization. Write barriers in the runtime stay on; only user
// packages compile with -wb=false.
func logosCheckFlags() {
	if !LogosLoadBarrier {
		return
	}
	if !buildcfg.Experiment.LogosGCMoving || (buildcfg.GOARCH != "amd64" && buildcfg.GOARCH != "arm64") {
		log.Fatalf("-lb requires GOEXPERIMENT=logosgcmoving on amd64 or arm64")
	}
}

// LogosNoLB reports whether the named function of a package keeps plain
// loads under -lb: the runtime files whose pointer loads stay plain (the
// barrier implementation itself, the GC scanner files, the allocator
// bit-manipulation files, signal delivery files, stack growth mechanics,
// and the debug-call machinery), and the named runtime syscall, cgo and
// crash-stack entries whose slow path must never appear there, plus the
// source-proven C-static sigaction exception and the C-static munmap,
// libInit and stack-only sigaction.func1 leaves. A non-runtime package is
// never excluded, whatever its file is called. Extend the set only on
// audit rows or a link-time nosplit-limit error.
func LogosNoLB(pkg, file, name string) bool {
	if logosNoLBFunc(pkg, name) {
		return true
	}
	return pkg == "runtime" && logosNoLBFile(file)
}

var logosNoLBFiles = [...]string{
	"logos_zgc",
	"mgcmark",
	"mbitmap",
	"mgcsweep",
	"malloc",
	"mcache",
	"mcentral",
	"mheap",
	"mpagealloc",
	"mspanset",
	"signal_",
	"sigaction",
	"sigqueue",
	"stack",
	"debugcall",
}

func logosNoLBFunc(pkg, name string) bool {
	if pkg != "runtime" {
		return false
	}
	switch name {
	case "entersyscall", "reentersyscall", "entersyscallblock",
		"exitsyscall", "cgocall", "cgocallbackg",
		"sigaction", "sigaction.func1",
		"munmap", "libInit",
		"switchToCrashStack", "badmorestackg0", "badmorestackg0.func1":
		return true
	}
	return false
}

func logosNoLBFile(name string) bool {
	file := name
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			file = name[i+1:]
			break
		}
	}
	for _, prefix := range logosNoLBFiles {
		if len(file) >= len(prefix) && file[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
