// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"strings"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// This file binds the operands of a group `go f(args)` child in the
// calling goroutine, as the Go spec orders for a go statement (SPEC
// Sec 2.6; Go spec, "Go statements"): the callee or receiver and every
// argument evaluate in source order before Group.Go, and the child
// closure calls the bound values. The call keeps its source in place;
// each bound operand moves to a short declaration ahead of Group.Go and
// leaves a temporary name where it stood, so the comma, ellipsis, and
// comment layout of the call stays untouched. moveRegion carries the
// edits that earlier passes recorded inside an operand, and the edits
// later passes record there, to the copy in the declaration.

// groupOperand is one callee or argument of a group go call. A callee
// or argument bound in the parent carries names, one fresh temporary per
// value; an operand that stays in the child call carries none.
type groupOperand struct {
	expr  syntax.Expr
	span  span
	names []string
	decl  types2.Type // declared type of the single temporary, nil for a short declaration
}

// groupCapture binds the operands of call in the parent. It returns the
// native declarations and the eject text that precede Group.Go, and it
// rewrites call in place so the child closure calls the temporaries.
// Temporaries are allocated here, before the child lowering carries the
// temp counters across.
func (fc *funcLower) groupCapture(pos syntax.Pos, call *syntax.CallExpr) ([]syntax.Stmt, string) {
	fl := fc.fl
	fun, args := fc.groupOperands(call)
	var native []syntax.Stmt
	var text strings.Builder
	var list []syntax.Expr
	for _, op := range append([]groupOperand{fun}, args...) {
		list = append(list, groupUses(op)...)
		if len(op.names) == 0 {
			continue
		}
		mark := fl.moveRegion(op.span, strings.Join(op.names, ", "))
		stmts, stmtText := fc.groupBind(pos, op, mark)
		native = append(native, stmts...)
		text.WriteString(stmtText)
		scanForLiterals(fl, op.expr)
	}
	call.Fun, call.ArgList = list[0], list[1:]
	return native, text.String()
}

// groupUses returns what the child call spells for op: the operand
// itself when it stays, else one temporary name per value it binds.
func groupUses(op groupOperand) []syntax.Expr {
	if len(op.names) == 0 {
		return []syntax.Expr{op.expr}
	}
	uses := make([]syntax.Expr, len(op.names))
	for i, name := range op.names {
		uses[i] = nameOf(syntax.StartPos(op.expr), name)
	}
	return uses
}

// groupOperands maps the callee and arguments of call to their source
// spans and decides which ones bind in the parent.
func (fc *funcLower) groupOperands(call *syntax.CallExpr) (groupOperand, []groupOperand) {
	fl := fc.fl
	src := fl.src
	lparen := fl.off(call.Pos())
	split, ok := splitCall(src, lparen)
	if !ok || len(split.args) != len(call.ArgList) {
		base.Fatalf("logos: cannot split the operands of the group call at %s", call.Pos().String())
	}
	start := fl.off(leftmostOf(call.Fun))
	fun := groupOperand{expr: call.Fun, span: span{start: start, end: lastTokenEnd(src, start, lparen)}}
	if !isBuiltinExpr(fc, call.Fun) && !fc.groupStaticFunc(call.Fun) {
		fun.names = []string{fc.nextFun()}
	}
	args := make([]groupOperand, len(call.ArgList))
	for i, raw := range call.ArgList {
		arg := groupOperand{expr: raw, span: trimSpan(src, split.args[i])}
		switch kind := argKind(fc, raw); {
		case kind == argInline || fc.groupStaticFunc(raw):
		case kind == argMulti:
			arg.names = fc.multiNames(raw)
		default:
			arg.names = []string{fc.nextArg()}
			arg.decl = fc.groupDeclaredType(raw)
		}
		args[i] = arg
	}
	return fun, args
}

// groupStaticFunc reports whether e names a declared function: a bare
// or package-qualified function, with or without explicit type
// arguments. Such a name has no evaluation effect and cannot change
// between the go statement and the call, and a generic function without
// type arguments has no value of its own to bind, so it stays in the
// child call where inference sees the bound arguments.
func (fc *funcLower) groupStaticFunc(e syntax.Expr) bool {
	info := fc.fl.pkg.info
	switch n := syntax.Unparen(e).(type) {
	case *syntax.Name:
		_, ok := info.ObjectOf(n).(*types2.Func)
		return ok
	case *syntax.SelectorExpr:
		if _, selected := info.Selections[n]; selected {
			return false
		}
		_, ok := info.ObjectOf(n.Sel).(*types2.Func)
		return ok
	case *syntax.IndexExpr:
		return fc.groupStaticFunc(n.X)
	}
	return false
}

// groupDeclaredType returns the type a bound argument must declare
// when a short declaration would drop it: an untyped non-constant
// operand (a comparison, a logical combination, or a shift of a
// constant) takes its type from the parameter it feeds, which a short
// declaration would replace by the default type. Every other operand is
// already typed, so the short declaration keeps it exactly.
func (fc *funcLower) groupDeclaredType(raw syntax.Expr) types2.Type {
	info := fc.fl.pkg.info
	typ := info.TypeOf(raw)
	if typ == nil {
		return nil
	}
	op, ok := syntax.Unparen(raw).(*syntax.Operation)
	if !ok {
		return nil
	}
	var natural types2.Type
	switch {
	case op.Y == nil && op.Op == syntax.Not:
		natural = types2.Typ[types2.UntypedBool]
	case op.Y != nil && (op.Op == syntax.Eql || op.Op == syntax.Neq || op.Op == syntax.Lss ||
		op.Op == syntax.Leq || op.Op == syntax.Gtr || op.Op == syntax.Geq ||
		op.Op == syntax.AndAnd || op.Op == syntax.OrOr):
		natural = types2.Typ[types2.UntypedBool]
	case op.Y != nil && (op.Op == syntax.Shl || op.Op == syntax.Shr):
		if _, constant := typeConst(info, op.X); !constant {
			return nil
		}
		natural = types2.Typ[types2.UntypedInt]
	default:
		return nil
	}
	if types2.Identical(types2.Default(typ), types2.Default(natural)) {
		return nil
	}
	return typ
}

// groupBind builds the declaration of one bound operand: native and
// eject text. mark stands for the operand text, which moved here.
func (fc *funcLower) groupBind(pos syntax.Pos, op groupOperand, mark string) ([]syntax.Stmt, string) {
	list := strings.Join(op.names, ", ")
	lhs := make([]syntax.Expr, len(op.names))
	for i, name := range op.names {
		lhs[i] = nameOf(pos, name)
	}
	if op.decl != nil {
		if typ, spelled, ok := fc.fl.spellTypeBoth(op.decl, pos); ok {
			native := []syntax.Stmt{
				typedVarDecl(pos, nameOf(pos, op.names[0]), typ),
				assignOf(pos, lhs, []syntax.Expr{op.expr}),
			}
			return native, "var " + list + " " + spelled + "; " + list + " = " + mark + "; "
		}
	}
	return []syntax.Stmt{defineOf(pos, lhs, []syntax.Expr{op.expr})}, list + " := " + mark + "; "
}
