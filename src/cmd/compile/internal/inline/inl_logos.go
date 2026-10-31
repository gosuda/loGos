// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package inline

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
)

// logosRecursiveMarks records the functions of every recursive call-graph
// cycle that any CanInlineFuncs pass of this compile has seen, so the -O
// tier recursive budget and the recursive-cycle stack-buffer threshold
// (LogosVariableMakeThreshold) apply to them. The map only grows, and a
// pass never clears it: noder/reader.go runs CanInlineFuncs again on
// freshly instantiated generic functions after every inlined body that
// instantiates one, in the middle of inlining the package, so a per-pass
// reset dropped the marks of the package's own functions before the
// backend read them. Passes run on the single frontend goroutine and the
// map is read-only once the backend starts, so concurrent function
// compilation never races on it.
var logosRecursiveMarks = make(map[*ir.Func]bool)

// logosNoteSCC records one recursive call-graph cycle.
func logosNoteSCC(funcs []*ir.Func) {
	for _, fn := range funcs {
		logosRecursiveMarks[fn] = true
	}
}

// logosBudgetFor returns the inline budget for fn: the recursive-cycle
// budget inside recursive cycles, the -O tier budget outside them, or the
// upstream default when no tier flag is set. A recursive cycle under a
// raised tier that sets no recursive budget keeps the upstream default:
// a raised budget inlined into a caller of its own cycle collapses the
// recursive frames profile tests expect (runtime/pprof
// TestCPUProfileRecursion). The runtime package always keeps the
// upstream default: its nowritebarrier-checked code is validated at
// that budget, and a higher tier budget inlines write barriers into
// callers that forbid them.
func logosBudgetFor(fn *ir.Func, def int32) int32 {
	if base.Debug.InlBudget == 0 {
		return def
	}
	if base.Flag.CompilingRuntime {
		return def
	}
	if logosRecursiveMarks[fn] {
		if base.Debug.InlRecursiveBudget != 0 {
			return int32(base.Debug.InlRecursiveBudget)
		}
		return def
	}
	return int32(base.Debug.InlBudget)
}

// logosConservativeBudget returns the inline budget to use for a conservative
// call-cost retry for fn under higher -O tiers, or 0 if no retry should occur.
//
// In O1 (default Go, base.Debug.InlBudget == 0), upstream inlining budgets apply
// and large callees are not inlined, so calls to them incur only extraCost (57).
// In O2/O3, the higher inline budget makes those callees inlinable, which
// expands callee.Inl.Cost into the caller. If callee.Inl.Cost exceeds extraCost,
// a wrapper function (such as crypto/sha256.New or net.(*UDPConn).ReadFromUDP)
// can exceed the tier budget, causing an inlining inversion where a wrapper that
// was inlinable in O1 becomes non-inlinable in O2. That wrapper failure prevents
// inlining wrapper call sites and causes heap allocations.
//
// Retrying with conservative call cost at the upstream O1 budget (inlineMaxBudget)
// restores O1 admission for wrappers without granting broad O2 admission to
// non-wrapper functions. Under O1 (base.Debug.InlBudget == 0) or when compiling
// runtime, no retry is performed.
func logosConservativeBudget(fn *ir.Func) int32 {
	if base.Debug.InlBudget == 0 || base.Flag.CompilingRuntime || fn == nil {
		return 0
	}
	return int32(inlineMaxBudget)
}

// logosConservativeCallCost returns the call cost capped at extraCost when
// calleeCost exceeds extraCost, used during conservative retry to prevent
// inlining inversion for wrappers.
func logosConservativeCallCost(calleeCost, extraCost int32) int32 {
	if calleeCost > extraCost {
		return extraCost
	}
	return calleeCost
}

// logosNoLBFence reports whether inlining callee into callerfn would mix
// load-barrier policies under -lb: one side keeps plain loads while the
// other takes barriers, so the inlined body would compile under the wrong
// policy. Either direction fences; both plain and both barriered inline
// unchanged. Each side is classified by its own package, file and name,
// so a non-runtime file never counts as excluded.
func logosNoLBFence(callerfn, callee *ir.Func) bool {
	if !base.LogosLoadBarrier {
		return false
	}
	caller := base.LogosNoLB(base.Ctxt.Pkgpath, base.Ctxt.PosTable.Pos(callerfn.Pos()).Filename(), callerfn.Sym().Name)
	calleeNoLB := base.LogosNoLB(callee.Sym().Pkg.Path, base.Ctxt.PosTable.Pos(callee.Pos()).Filename(), callee.Sym().Name)
	return caller != calleeNoLB
}
