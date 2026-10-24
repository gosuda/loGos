// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// httpResponse is correctness::httpresponse:
// detects defer resp.Body.Close() executed immediately after an HTTP call
// before checking whether an error occurred.
var httpResponse = Rule{
	ID:    "correctness::httpresponse",
	Group: Correctness,
	Go:    true,
	Check: checkHTTPResponse,
}

func checkHTTPResponse(pass *Pass) {
	if !packageImportsHTTP(pass.Pkg) {
		return
	}
	for _, f := range pass.Files {
		syntax.Inspect(f, func(n syntax.Node) bool {
			block, ok := n.(*syntax.BlockStmt)
			if !ok {
				return true
			}
			stmts := block.List
			for i := range max(0, len(stmts)-1) {
				asg, ok := stmts[i].(*syntax.AssignStmt)
				if !ok {
					continue
				}
				call := assignmentCall(asg)
				if call == nil || !isHTTPFuncOrClientMethod(call) {
					continue
				}
				respName := responseIdent(asg)
				if respName == nil {
					continue
				}
				def, ok := stmts[i+1].(*syntax.CallStmt)
				if !ok || def.Tok != syntax.Defer {
					continue
				}
				defCall, ok := unparen(def.Call).(*syntax.CallExpr)
				if !ok {
					continue
				}
				root := rootIdent(defCall.Fun)
				if root != nil && root.Value == respName.Value {
					pass.Reportf(def, "using %s before checking for errors", respName.Value)
				}
			}
			return true
		})
	}
}

func packageImportsHTTP(pkg *types2.Package) bool {
	if pkg == nil {
		return false
	}
	for _, imp := range pkg.Imports() {
		if imp.Path() == "net/http" {
			return true
		}
	}
	return false
}

func assignmentCall(asg *syntax.AssignStmt) *syntax.CallExpr {
	rhs := unpackExprList(asg.Rhs)
	if len(rhs) != 1 {
		return nil
	}
	call, _ := unparen(rhs[0]).(*syntax.CallExpr)
	return call
}

func responseIdent(asg *syntax.AssignStmt) *syntax.Name {
	lhs := unpackExprList(asg.Lhs)
	if len(lhs) == 0 {
		return nil
	}
	name, _ := unparen(lhs[0]).(*syntax.Name)
	return name
}

func rootIdent(expr syntax.Expr) *syntax.Name {
	switch e := unparen(expr).(type) {
	case *syntax.Name:
		return e
	case *syntax.SelectorExpr:
		return rootIdent(e.X)
	}
	return nil
}

func isHTTPFuncOrClientMethod(call *syntax.CallExpr) bool {
	sel, ok := unparen(call.Fun).(*syntax.SelectorExpr)
	if !ok {
		return false
	}
	sig, ok := TypeOf(call.Fun).(*types2.Signature)
	if !ok || sig.Results().Len() != 2 {
		return false
	}
	if !isHTTPResponsePointer(sig.Results().At(0).Type()) || !isErrorType(sig.Results().At(1).Type()) {
		return false
	}
	if id, ok := sel.X.(*syntax.Name); ok && id.Value == "http" {
		return true
	}
	t := TypeOf(sel.X)
	if t != nil {
		if ptr, ok := t.(*types2.Pointer); ok {
			t = ptr.Elem()
		}
		if named, ok := t.(*types2.Named); ok && named.Obj() != nil {
			if named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == "Client" {
				return true
			}
		}
	}
	return false
}

func isHTTPResponsePointer(t types2.Type) bool {
	ptr, ok := t.(*types2.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types2.Named)
	if !ok || named.Obj() == nil {
		return false
	}
	return named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == "Response"
}
