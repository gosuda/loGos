// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/logos/wasmyield"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/bio"
	"cmd/internal/obj"
	"internal/abi"
	"internal/buildcfg"
)

// This file holds the loGo/s object-emission hooks: the facts-sidecar
// writer (M3-S1), the wasm finish hook (M16-S1), and the moving-GC
// foreign-function marker. Upstream files carry only hook lines; all
// logic lives here.
//
// writeLogosFacts appends the __.LOGOS member right after __.PKGDEF in
// compiler-object mode. The member binds to the __.PKGDEF linker
// fingerprint that WriteExports just stored in base.Ctxt.Fingerprint, so an
// importer rejects stale facts cleanly. The linker already skips non-.o
// members (cmd/link/internal/ld/lib.go), so no linker change is needed.
// Until S2-S7 populate records through facts.SetCurrent, the member
// carries the header and fingerprint only. The member is emitted only
// for loGo/s packages and, for the MayYield facts of the WASM profile
// (M16-S1, T-Sec6), for every package built for GOARCH=wasm: a plain-Go
// archive on any other target stays byte-identical to upstream, and
// every member-counting reader (pack's object check, nm, objdump) sees
// the same stream it always has.
// hasLogosAssertFacts reports whether the current facts carry any generic
// assertion-effect record. Such members are written even for plain-Go
// packages so the types2 consumer can fail closed on unknown effects.
func hasLogosAssertFacts() bool {
	f := facts.Current()
	if f == nil {
		return false
	}
	for i := range f.Fns {
		if f.Fns[i].HasAssert {
			return true
		}
	}
	return false
}

func writeLogosFacts(bout *bio.Writer) {
	if !base.Logos && !wasmyield.Active() && !hasLogosAssertFacts() {
		return
	}
	start := startArchiveEntry(bout)
	bout.Write(facts.Encode(base.Ctxt.Fingerprint, facts.Current()))
	finishArchiveEntry(bout, start, facts.MemberName)
}

// logosWasmFinish is the M16-S1 hook after the backend loop: it solves the
// MayYield facts of the package and assembles the functions held for them.
func logosWasmFinish() {
	wasmyield.Finish()
}

// markUintptrForeignFuncs sets abi.FuncFlagUintptrForeign on the ABIInternal
// TEXT symbol of every function whose types.Sym group carries
// //go:uintptrescapes or //go:uintptrkeepalive.
//
// The pragma lives on the declaration (explicitly, or implicitly on bodyless
// declarations via escape) and is never copied to compiler-generated ABI
// wrappers: makeABIWrapper builds a fresh ir.Func without Pragma. Wrappers
// share the declaration's *types.Sym, so grouping by Sym marks exactly the
// ABIInternal symbol that every function value references (both OCFUNC
// lowering and WriteFuncSyms Fatalf unless the target is ABIInternal). Raw
// ABI0 assembly bodies are never function-value targets and stay unmarked.
//
// Runs after escape and GenABIWrappers with final pragmas, immediately
// before the linker object is written. Gated to moving-GC amd64/arm64 so
// ordinary builds emit byte-identical objects.
// Only the generated ABIInternal wrapper carries the flag. Raw ABI0
// assembly bodies are never function-value targets and stay unmarked,
// so the runtime indirect gate rejects unmarked asm stubs.
func markUintptrForeignFuncs() {
	if !logosForeignFlagEnabled() {
		return
	}
	const mask = ir.UintptrKeepAlive | ir.UintptrEscapes
	// Only declaration metadata authorizes a foreign target: the pragma
	// lives on the declaration (explicitly, or implicitly on bodyless
	// declarations via escape, which tags every bodyless function), and
	// the Sym grouping below carries it to the generated ABIInternal
	// wrapper that shares the declaration's Sym. Call-site shapes
	// (kept-alive arguments, uintptr conversions) must never authorize a
	// target: ordinary address consumers take such arguments without
	// ever accepting foreign pointers, and the runtime treats the flag
	// as a target property only.
	views := make([]logosForeignFuncView, 0, len(typecheck.Target.Funcs))
	for _, fn := range typecheck.Target.Funcs {
		views = append(views, logosForeignFuncView{Sym: fn.Sym(), Pragma: fn.Pragma})
	}
	foreign := logosForeignMarkedSyms(views)
	for sym := range foreign {
		logosForeignApplyFlag(sym)
	}
}

// logosForeignApplyFlag sets FuncFlagUintptrForeign on exactly the
// ABIInternal flavor of sym: the symbol every function value references
// (both OCFUNC lowering and WriteFuncSyms resolve through ABIInternal,
// faulting otherwise). Raw ABI0 assembly bodies are never function-value
// targets and stay unmarked, so the runtime indirect gate rejects
// unmarked asm stubs. A symbol with no assembled TEXT (Func() == nil)
// is skipped.
func logosForeignApplyFlag(sym *types.Sym) {
	lsym := sym.LinksymABI(obj.ABIInternal)
	if lsym == nil || lsym.Func() == nil {
		return
	}
	lsym.Func().FuncFlag |= abi.FuncFlagUintptrForeign
}

// logosForeignFuncView is the minimal input to the propagation
// decision: the function's symbol group and its declaration pragma. A
// generated ABIInternal wrapper shares its declaration's Sym but never
// copies its pragma, so grouping by Sym is what carries the flag to the
// wrapper.
type logosForeignFuncView struct {
	Sym    *types.Sym
	Pragma ir.PragmaFlag
}

// logosForeignMarkedSyms returns the symbol groups authorized as foreign
// targets: any group with a //go:uintptrescapes or
// //go:uintptrkeepalive declaration pragma. Marking then writes exactly
// the ABIInternal flavor of each group symbol, so bodyless wrappers
// carry the flag while raw ABI0 assembly bodies stay unmarked.
func logosForeignMarkedSyms(funcs []logosForeignFuncView) map[*types.Sym]bool {
	const mask = ir.UintptrKeepAlive | ir.UintptrEscapes
	foreign := make(map[*types.Sym]bool)
	for _, fn := range funcs {
		if fn.Sym != nil && fn.Pragma&mask != 0 {
			foreign[fn.Sym] = true
		}
	}
	return foreign
}

// logosForeignFlagEnabled reports whether the moving-GC foreign-target
// flag is emitted for this build. The experiment gates all builds; the
// arch gate restricts emission to amd64/arm64, where the moving
// collector and the ABIInternal-wrapper calling convention are
// supported. Other architectures stay byte-identical to upstream by
// design: no flag, no wrapper marking, no runtime consult.
func logosForeignFlagEnabled() bool {
	return logosForeignFlagArchEnabled(buildcfg.GOARCH) && buildcfg.Experiment.LogosGCMoving
}

// logosForeignFlagArchEnabled is the testable arch half of the gate.
// amd64 and arm64 carry the flag; every other GOARCH is excluded with
// the reason above (unsupported moving-collector wrapper shape).
func logosForeignFlagArchEnabled(goarch string) bool {
	return goarch == "amd64" || goarch == "arm64"
}
