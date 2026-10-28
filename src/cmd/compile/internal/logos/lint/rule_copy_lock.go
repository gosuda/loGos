// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"bytes"
	"sync"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// copyLock is suspicious::copy_lock (LINT-RULES.md Sec 1.2): detects
// copying values containing embedded or direct locks. Copying an active lock
// duplicates synchronization state, causing deadlocks or unsynchronized access.
var copyLock = Rule{
	ID:    "suspicious::copy_lock",
	Group: Suspicious,
	Go:    true,
	Check: checkCopyLock,
}

func checkCopyLock(pass *Pass) {
	for _, f := range pass.Files {
		syntax.Inspect(f, func(n syntax.Node) bool {
			switch x := n.(type) {
			case *syntax.FuncDecl:
				checkCopyLocksFunc(pass, x.Name.Value, x.Recv, x.Type)
			case *syntax.FuncLit:
				checkCopyLocksFunc(pass, "func", nil, x.Type)
			case *syntax.CallExpr:
				checkCopyLocksCallExpr(pass, x)
			case *syntax.AssignStmt:
				checkCopyLocksAssign(pass, x)
			case *syntax.VarDecl:
				checkCopyLocksVarDecl(pass, x)
			case *syntax.ReturnStmt:
				checkCopyLocksReturnStmt(pass, x)
			case *syntax.CompositeLit:
				checkCopyLocksCompositeLit(pass, x)
			case *syntax.ForStmt:
				if r, ok := x.Init.(*syntax.RangeClause); ok {
					checkCopyLocksRange(pass, r)
				}
			}
			return true
		})
	}
}

func checkCopyLocksFunc(pass *Pass, name string, recv *syntax.Field, typ *syntax.FuncType) {
	if recv != nil && recv.Type != nil {
		if path := lockPath(TypeOf(recv.Type), nil); path != nil {
			pass.Reportf(recv.Type, "%s passes lock by value: %s", name, formatTypePath(path))
		}
	}
	if typ != nil {
		for _, p := range typ.ParamList {
			if p.Type != nil {
				if path := lockPath(TypeOf(p.Type), nil); path != nil {
					pass.Reportf(p.Type, "%s passes lock by value: %s", name, formatTypePath(path))
				}
			}
		}
	}
}

func checkCopyLocksCallExpr(pass *Pass, call *syntax.CallExpr) {
	if fn := calleeFunc(pass, call); fn != nil {
		if fn.Pkg() == nil {
			switch fn.Name() {
			case "len", "cap":
				return
			}
		}
		if fn.Pkg() != nil && fn.Pkg().Path() == "unsafe" {
			switch fn.Name() {
			case "Sizeof", "Offsetof", "Alignof":
				return
			}
		}
	}
	funStr := syntax.String(call.Fun)
	for _, arg := range call.ArgList {
		if path := lockPathRhs(pass, arg); path != nil {
			pass.Reportf(arg, "call of %s copies lock value: %s", funStr, formatTypePath(path))
		}
	}
}

func checkCopyLocksAssign(pass *Pass, assign *syntax.AssignStmt) {
	lhss := exprList(assign.Lhs)
	rhss := exprList(assign.Rhs)
	for i, rhs := range rhss {
		if path := lockPathRhs(pass, rhs); path != nil {
			lhsName := "variable"
			if i < len(lhss) {
				lhsName = syntax.String(lhss[i])
			}
			pass.Reportf(rhs, "assignment copies lock value to %s: %s", lhsName, formatTypePath(path))
		}
	}
}

func checkCopyLocksVarDecl(pass *Pass, decl *syntax.VarDecl) {
	if decl.Values == nil {
		return
	}
	values := exprList(decl.Values)
	for i, val := range values {
		if path := lockPathRhs(pass, val); path != nil {
			varName := "variable"
			if i < len(decl.NameList) {
				varName = decl.NameList[i].Value
			}
			pass.Reportf(val, "variable declaration copies lock value to %s: %s", varName, formatTypePath(path))
		}
	}
}

func checkCopyLocksReturnStmt(pass *Pass, ret *syntax.ReturnStmt) {
	if ret.Results == nil {
		return
	}
	for _, res := range exprList(ret.Results) {
		if path := lockPathRhs(pass, res); path != nil {
			pass.Reportf(res, "return copies lock value: %s", formatTypePath(path))
		}
	}
}

func checkCopyLocksCompositeLit(pass *Pass, cl *syntax.CompositeLit) {
	for _, elt := range cl.ElemList {
		val := elt
		if kv, ok := elt.(*syntax.KeyValueExpr); ok {
			val = kv.Value
		}
		if path := lockPathRhs(pass, val); path != nil {
			pass.Reportf(val, "literal copies lock value from %s: %s", syntax.String(val), formatTypePath(path))
		}
	}
}

func checkCopyLocksRange(pass *Pass, r *syntax.RangeClause) {
	if r.Lhs == nil {
		return
	}
	for _, v := range exprList(r.Lhs) {
		if isBlank(v) {
			continue
		}
		if t := TypeOf(v); t != nil {
			if path := lockPath(t, nil); path != nil {
				pass.Reportf(v, "range var %s copies lock: %s", syntax.String(v), formatTypePath(path))
			}
		}
	}
}

func lockPathRhs(pass *Pass, x syntax.Expr) []string {
	if x == nil {
		return nil
	}
	x = unparen(x)
	if _, ok := x.(*syntax.CompositeLit); ok {
		return nil
	}
	if _, ok := x.(*syntax.CallExpr); ok {
		return nil
	}
	if op, ok := x.(*syntax.Operation); ok && op.Op == syntax.Mul && op.Y == nil {
		if _, ok := unparen(op.X).(*syntax.CallExpr); ok {
			return nil
		}
	}
	t := TypeOf(x)
	if t != nil {
		return lockPath(t, nil)
	}
	return nil
}

func unparen(e syntax.Expr) syntax.Expr {
	for {
		p, ok := e.(*syntax.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

var (
	lockerType     *types2.Interface
	initLockerOnce sync.Once
)

func getLockerType() *types2.Interface {
	initLockerOnce.Do(func() {
		sig := types2.NewSignatureType(nil, nil, nil, nil, nil, false)
		methods := []*types2.Func{
			types2.NewFunc(syntax.Pos{}, nil, "Lock", sig),
			types2.NewFunc(syntax.Pos{}, nil, "Unlock", sig),
		}
		lockerType = types2.NewInterfaceType(methods, nil)
	})
	return lockerType
}

func lockPath(typ types2.Type, seen map[types2.Type]bool) []string {
	if typ == nil {
		return nil
	}
	typ = types2.Unalias(typ)
	if seen[typ] {
		return nil
	}
	if seen == nil {
		seen = make(map[types2.Type]bool)
	}
	seen[typ] = true

	if tpar, ok := typ.(*types2.TypeParam); ok {
		if tpar.Constraint() != nil {
			return lockPath(tpar.Constraint(), seen)
		}
		return nil
	}

	for {
		atyp, ok := typ.Underlying().(*types2.Array)
		if !ok {
			break
		}
		typ = types2.Unalias(atyp.Elem())
	}

	if ttyp, ok := typ.Underlying().(*types2.Tuple); ok {
		for i := range ttyp.Len() {
			sub := lockPath(ttyp.At(i).Type(), seen)
			if sub != nil {
				return append(sub, typ.String())
			}
		}
		return nil
	}

	styp, ok := typ.Underlying().(*types2.Struct)
	if !ok {
		return nil
	}

	locker := getLockerType()
	if types2.Implements(types2.NewPointer(typ), locker) && !types2.Implements(typ, locker) {
		return []string{typ.String()}
	}

	if named, ok := typ.(*types2.Named); ok {
		if named.Obj() != nil && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "sync" && named.Obj().Name() == "noCopy" {
			return []string{typ.String()}
		}
	}

	for i := range styp.NumFields() {
		ftyp := styp.Field(i).Type()
		sub := lockPath(ftyp, seen)
		if sub != nil {
			return append(sub, typ.String())
		}
	}

	return nil
}

func formatTypePath(path []string) string {
	var buf bytes.Buffer
	for i := len(path) - 1; i >= 0; i-- {
		if buf.Len() > 0 {
			buf.WriteString(" contains ")
		}
		buf.WriteString(path[i])
	}
	return buf.String()
}
