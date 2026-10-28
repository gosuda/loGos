// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"strings"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// selfAssignment is suspicious::self_assignment (LINT-RULES.md Sec 1.2):
// detects assignments where left-hand and right-hand expressions resolve to
// identical storage without side effects (x = x, s.field = s.field).
var selfAssignment = Rule{
	ID:    "suspicious::self_assignment",
	Group: Suspicious,
	Go:    true,
	Check: checkSelfAssignment,
}

func checkSelfAssignment(pass *Pass) {
	for _, f := range pass.Files {
		syntax.Inspect(f, func(n syntax.Node) bool {
			as, ok := n.(*syntax.AssignStmt)
			if !ok || as.Op != 0 || as.Rhs == nil {
				return true
			}
			lhsList := unpackExprList(as.Lhs)
			rhsList := unpackExprList(as.Rhs)
			if len(lhsList) != len(rhsList) {
				return true
			}
			var exprs []string
			for i := range lhsList {
				lhs := lhsList[i]
				rhs := rhsList[i]
				if isSelfAssignment(pass.Info, lhs, rhs) {
					exprs = append(exprs, syntax.String(lhs))
				}
			}
			if len(exprs) > 0 {
				pass.Reportf(as, "self-assignment of %s", strings.Join(exprs, ", "))
			}
			return true
		})
	}
}

func unpackExprList(e syntax.Expr) []syntax.Expr {
	if list, ok := e.(*syntax.ListExpr); ok {
		return list.ElemList
	}
	return []syntax.Expr{e}
}

func isSelfAssignment(info *types2.Info, lhs, rhs syntax.Expr) bool {
	if isMapIndex(lhs) || isMapIndex(rhs) {
		return false
	}
	if !hasNoEffects(info, lhs) || !hasNoEffects(info, rhs) {
		return false
	}
	if syntax.String(lhs) != syntax.String(rhs) {
		return false
	}
	return sameStorage(info, lhs, rhs)
}

func isMapIndex(e syntax.Expr) bool {
	idx, ok := unparen(e).(*syntax.IndexExpr)
	if !ok {
		return false
	}
	t := TypeOf(idx.X)
	if t == nil {
		return false
	}
	_, isMap := t.Underlying().(*types2.Map)
	return isMap
}

func hasNoEffects(info *types2.Info, e syntax.Expr) bool {
	safe := true
	syntax.Inspect(e, func(n syntax.Node) bool {
		if !safe {
			return false
		}
		switch node := n.(type) {
		case *syntax.CallExpr:
			if tv, ok := info.Types[node.Fun]; !ok || !tv.IsType() {
				safe = false
				return false
			}
		case *syntax.Operation:
			if node.Op == syntax.Recv {
				safe = false
				return false
			}
		}
		return true
	})
	return safe
}

func sameStorage(info *types2.Info, a, b syntax.Expr) bool {
	a = unparen(a)
	b = unparen(b)
	switch x := a.(type) {
	case *syntax.Name:
		y, ok := b.(*syntax.Name)
		if !ok {
			return false
		}
		objA := info.Uses[x]
		objB := info.Uses[y]
		if objA != nil && objB != nil {
			return objA == objB
		}
		return x.Value == y.Value

	case *syntax.SelectorExpr:
		y, ok := b.(*syntax.SelectorExpr)
		if !ok || x.Sel.Value != y.Sel.Value {
			return false
		}
		return sameStorage(info, x.X, y.X)

	case *syntax.IndexExpr:
		y, ok := b.(*syntax.IndexExpr)
		if !ok {
			return false
		}
		if syntax.String(x.Index) != syntax.String(y.Index) {
			return false
		}
		return sameStorage(info, x.X, y.X) && sameStorage(info, x.Index, y.Index)

	case *syntax.BasicLit:
		y, ok := b.(*syntax.BasicLit)
		return ok && x.Value == y.Value
	}
	return false
}
