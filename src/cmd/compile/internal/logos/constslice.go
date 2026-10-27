// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"fmt"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// This file lowers the M13-S3 const slice covariance rule (#121, SPEC
// T-203.4). In a loGo/s call, an argument of type []C may flow into a
// `const []I` parameter when C implements the interface I: the checker
// (types2/const_logos.go, logosConstCovariant) records every such
// argument in Info.LogosConstCov with its target interface. Const views
// are erased by lowering, so the callee's parameter becomes a plain
// []I, which a []C cannot take in Go. This pass supplies the missing
// conversion at the argument itself:
//
//	Names(ms)  ->  Names(_logos_rt.CovSlice[Creature](ms))
//
// CovSlice (logos/rt/constslice.go) is a real element-wise copy: a nil
// source gives nil, a non-nil empty source gives a non-nil empty slice,
// each element converts through its dynamic value (a nil interface
// element stays a nil I, a nil pointer element stays a nil pointer
// inside a non-nil I), and the result owns a fresh backing array. The
// call wraps the argument expression itself, so the argument is
// evaluated once, left to right, exactly as before.
//
// The clone specializer (specialize.go) is the only consumer that may
// drop the copy: for a callee whose parameter qualifies it recognizes
// the CovSlice call, re-proves that the operand's element type
// implements the interface, and passes the operand to a clone typed
// with that element. Everywhere else (-O=1, ineligible callees,
// function values) the copy stays. Nothing here reads the optimization
// level, so one lowering serves every level.
//
// The parent wires one call into lower.go, ahead of the passes that
// rewrite call arguments:
//
//	fl.lowerConstSlices() // M13-S3 hook: []C arguments to const []I parameters.

// lowerConstSlices wraps every recorded covariant argument of one file
// in an rt.CovSlice call and records the matching eject splices: a
// zero-width insert before the argument and one after it, which
// compose with every splice nested in the argument and with a
// replacement of the argument as a whole.
func (fl *fileLower) lowerConstSlices() {
	cov := fl.pkg.info.LogosConstCov
	if len(cov) == 0 {
		return
	}
	var calls []*syntax.CallExpr
	syntax.Inspect(fl.file, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		for _, a := range call.ArgList {
			if _, ok := cov[a]; ok {
				calls = append(calls, call)
				break
			}
		}
		return true
	})
	if len(calls) == 0 {
		return
	}
	for _, call := range calls {
		for i, a := range call.ArgList {
			iface, ok := cov[a]
			if !ok {
				continue
			}
			call.ArgList[i] = fl.covSliceWrap(a, iface)
		}
	}
	fl.needRT = true
	fl.flushTypeImports()
}

// covSliceWrap builds `_logos_rt.CovSlice[I](a)` in place of the
// argument a and records its eject splices. iface is the target element
// interface I the checker recorded for a.
func (fl *fileLower) covSliceWrap(a syntax.Expr, iface types2.Type) syntax.Expr {
	pos := leftmostOf(a)
	if c, ok := syntax.Unparen(a).(*syntax.CompositeLit); ok && c.Type != nil {
		pos = syntax.StartPos(c)
	}
	text, ok := fl.spellType(iface)
	if !ok {
		base.Fatalf("logos: const slice element %s of %s cannot be spelled in this file", types2.TypeString(iface, nil), fl.baseName)
	}
	typ, ok := typeExprAt(text, pos)
	if !ok {
		base.Fatalf("logos: const slice element %q of %s is not a type", text, fl.baseName)
	}
	fun := at(&syntax.IndexExpr{X: rtSel(pos, "CovSlice"), Index: typ}, pos)
	start := fl.off(pos)
	end := fl.exprEndOf(a)
	fl.add(start, start, fmt.Sprintf("%s.CovSlice[%s](", rtAlias, text))
	fl.add(end, end, ")")
	return callOf(pos, fun, []syntax.Expr{a})
}
