// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

import (
	"cmd/compile/internal/syntax"
	"go/constant"
	. "internal/types/errors"
)

// LoGo/s M7-S1 `const if` checking (SPEC Sec 2.5, T-205). The condition
// must be a constant boolean; both branches type-check, so an undefined
// name in the dead branch is an error. Lowering keeps the live branch.
// Dead-branch statements are recorded in info.LogosConstIfDead for the
// sibling M7-S2/S3 comptime checks.

const logosNotConstant = "[comptime::not_constant]"

// logosConstIf checks a `const if` statement: the condition must be a
// constant boolean, and both branches check in the enclosing context.
// The live branch index feeds the dead-branch map before either branch
// is checked, because a statement's deferred checks run at its end,
// inside the branch; an invalid condition, and a condition that waits
// for an instantiation, still check both branches without marking
// either dead.
func (check *Checker) logosConstIf(ctxt stmtContext, s *syntax.ConstIfStmt) {
	live := check.logosConstIfLive(s)
	if live >= 0 {
		check.logosMarkConstIfDead(s, live)
	}
	inner := ctxt &^ (fallthroughOk | finalSwitchCase | inTypeSwitch)
	check.stmt(inner, s.Then)
	if s.Else != nil {
		check.stmt(inner, s.Else)
	}
}

// logosConstIfLive checks the condition of s and returns the index of
// the live branch: 0 for Then, 1 for Else, -1 when the condition is
// invalid or only deferred predicates keep it from being constant (the
// comptime predicates of a comptime generic answer per instantiation).
func (check *Checker) logosConstIfLive(s *syntax.ConstIfStmt) int {
	var x operand
	outer := check.logosPred
	check.logosPred = logosPredState{depth: 1}
	check.expr(nil, &x, s.Cond)
	deferred := check.logosPred.calls
	check.logosPred = outer
	switch {
	// Invariant (invalid): a broken condition still checks both branches with no dead proof.
	case !x.isValid():
	// Invariant (per-instantiation): deferred comptime predicates keep
	// the condition unmarked so copies answer per instantiation.
	case x.val == nil && check.logosPredOnly(s.Cond, deferred):
	// Invariant (constant obligation): a live non-constant condition
	// fails without weakening the diagnostic text.
	case x.val == nil:
		check.errorf(s.Cond, InvalidCond, "%s const if condition is not constant", logosNotConstant)
	// Invariant (boolean obligation): a constant non-boolean condition
	// keeps the upstream non-boolean diagnostic.
	case !allBoolean(x.typ()):
		check.error(s.Cond, InvalidCond, "non-boolean condition in const if statement")
	// Invariant (live then): a constant true condition keeps Then.
	case constant.BoolVal(x.val):
		return 0
	// Invariant (live else): a constant false condition keeps Else.
	default:
		return 1
	}
	return -1
}

// logosMarkConstIfDead records every statement of the dead branch of s
// in info.LogosConstIfDead: live 0 keeps Then and kills Else, live 1
// keeps Else (or nothing when Else is absent) and kills Then.
func (check *Checker) logosMarkConstIfDead(s *syntax.ConstIfStmt, live int) {
	if check.Info.LogosConstIfDead == nil {
		check.Info.LogosConstIfDead = make(map[syntax.Stmt]bool)
	}
	var dead syntax.Stmt
	if live == 0 {
		dead = s.Else
	} else {
		dead = s.Then
	}
	markDeadStmts(check.Info.LogosConstIfDead, dead)
}

// logosTerminatingConstIf reports whether a `const if` terminates: the
// live branch decides when the condition is constant, and both branches
// must terminate otherwise.
func (check *Checker) logosTerminatingConstIf(s *syntax.ConstIfStmt) bool {
	// Invariant (recorded proof): the constant value is re-read from the
	// recorded type information, never a Checker transient.
	if v, ok := check.constIfValue(s.Cond); ok {
		// Invariant (live decides): a constant condition terminates iff the live branch does.
		if v {
			return check.isTerminating(s.Then, "")
		}
		return s.Else != nil && check.isTerminating(s.Else, "")
	}
	// Invariant (undecided): without a constant proof both branches must terminate.
	return s.Else != nil && check.isTerminating(s.Then, "") && check.isTerminating(s.Else, "")
}

// constIfValue reads the constant boolean value of a checked condition.
func (check *Checker) constIfValue(e syntax.Expr) (bool, bool) {
	_, val, ok := check.logosChecked(e)
	if !ok || val == nil || val.Kind() != constant.Bool {
		return false, false
	}
	return constant.BoolVal(val), true
}

// logosHasBreakConstIf reports whether a `const if` contains a break
// matching the label search.
func logosHasBreakConstIf(s *syntax.ConstIfStmt, label string, implicit bool) bool {
	if hasBreak(s.Then, label, implicit) {
		return true
	}
	return s.Else != nil && hasBreak(s.Else, label, implicit)
}

// markDeadStmts records dead and every statement nested in it, so a
// comptime check on any nested node can test membership directly.
func markDeadStmts(dead map[syntax.Stmt]bool, s syntax.Stmt) {
	if s == nil {
		return
	}
	dead[s] = true
	syntax.Inspect(s, func(n syntax.Node) bool {
		if st, ok := n.(syntax.Stmt); ok && st != s {
			dead[st] = true
		}
		return true
	})
}
