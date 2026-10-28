// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/logos"
	"cmd/compile/internal/syntax"
)

func (w *writer) materializeConstInit(expr syntax.Expr) bool {
	v, ok := logos.LookupConstMatInit(w.p.curpkg.Path(), expr)
	if !ok {
		return false
	}
	tv, ok := w.p.maybeTypeAndValue(expr)
	if !ok || tv.Type == nil {
		w.p.fatalf(expr, "logos: materialized initializer is missing its checked type")
	}
	if v.IsNilValue() {
		w.Code(exprZero)
		w.pos(expr)
		w.typ(tv.Type)
		return true
	}
	lit := logos.BuildMatLiteral(v, tv.Type, expr.Pos())
	if lit == nil {
		w.p.fatalf(expr, "logos: cannot materialize imported const value of type %v", tv.Type)
	}
	w.expr(lit)
	return true
}
