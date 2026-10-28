// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"cmd/compile/internal/syntax"
)

// deferInLoop is suspicious::defer_in_loop (LINT-RULES.md Sec 1.2):
// detects defer statements directly inside loop bodies where the cleanup
// does not execute until the enclosing function returns.
var deferInLoop = Rule{
	ID:    "suspicious::defer_in_loop",
	Group: Suspicious,
	Go:    true,
	Check: checkDeferInLoop,
}

func checkDeferInLoop(pass *Pass) {
	for _, f := range pass.Files {
		syntax.Walk(f, &loopDeferVisitor{pass: pass, loopDepth: 0})
	}
}

type loopDeferVisitor struct {
	pass      *Pass
	loopDepth int
}

func (v *loopDeferVisitor) Visit(node syntax.Node) syntax.Visitor {
	if node == nil {
		return nil
	}
	switch n := node.(type) {
	case *syntax.FuncDecl:
		if n.Body != nil {
			inner := &loopDeferVisitor{pass: v.pass, loopDepth: 0}
			syntax.Walk(n.Body, inner)
		}
		return nil

	case *syntax.FuncLit:
		if n.Body != nil {
			inner := &loopDeferVisitor{pass: v.pass, loopDepth: 0}
			syntax.Walk(n.Body, inner)
		}
		return nil

	case *syntax.LambdaExpr:
		if block, ok := n.Body.(*syntax.BlockStmt); ok && block != nil {
			inner := &loopDeferVisitor{pass: v.pass, loopDepth: 0}
			syntax.Walk(block, inner)
			return nil
		}
		return v

	case *syntax.ForStmt:
		if n.Init != nil {
			syntax.Walk(n.Init, v)
		}
		if n.Cond != nil {
			syntax.Walk(n.Cond, v)
		}
		if n.Post != nil {
			syntax.Walk(n.Post, v)
		}
		if n.Body != nil {
			inner := &loopDeferVisitor{pass: v.pass, loopDepth: v.loopDepth + 1}
			syntax.Walk(n.Body, inner)
		}
		return nil

	case *syntax.CallStmt:
		if n.Tok == syntax.Defer && v.loopDepth > 0 {
			v.pass.Reportf(n, "defer in loop delays resource release until function exit")
		}
		return v
	}
	return v
}
