// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// scanFunc records the parameters of f that may specialize and classifies
// every use of them in the body. A function that cannot clone is marked
// bad. An interface parameter specializes to a concrete type; a slice of an
// interface (a const view after lowering) specializes to a slice of the
// concrete element type.
func (s *specializer) scanFunc(f *specFunc) {
	sig := f.obj.Signature()
	fields := f.decl.Type.ParamList
	if sig.Params().Len() != len(fields) {
		f.bad = true
		return
	}
	f.params = make([]specParam, len(fields))
	tracked := make(map[*types2.Var]*specParam)
	for i, fld := range fields {
		p := &f.params[i]
		p.fn, p.field = f, fld
		if fld.Name == nil || fld.Name.Value == "_" || (sig.Variadic() && i == len(fields)-1) {
			continue
		}
		v, _ := s.info.Defs[fld.Name].(*types2.Var)
		if v == nil {
			continue
		}
		if specIsIface(v.Type()) {
			p.iface = true
		} else if elem, ok := specSliceOfIface(v.Type()); ok {
			p.slice, p.elem = true, elem
		} else {
			continue
		}
		p.v, p.ok = v, true
		tracked[v] = p
	}
	if len(tracked) == 0 {
		f.bad = true
		return
	}
	var stack []syntax.Node
	syntax.Inspect(f.decl.Body, func(n syntax.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *syntax.FuncLit, *syntax.CallStmt, *syntax.TypeDecl:
			f.bad = true
		case *syntax.RangeClause:
			s.scanRange(f, n, tracked)
		case *syntax.Name:
			if v, _ := s.info.Uses[n].(*types2.Var); v != nil && tracked[v] != nil {
				s.classifyUse(tracked[v], n, stack)
			}
		}
		return true
	})
}

// scanRange rejects range-over-func loops, which build closures, and
// tracks the element variable of a range over a slice parameter.
func (s *specializer) scanRange(f *specFunc, rc *syntax.RangeClause, tracked map[*types2.Var]*specParam) {
	if t := rc.X.GetTypeInfo().Type; t != nil {
		if _, ok := t.Underlying().(*types2.Signature); ok {
			f.bad = true
			return
		}
	}
	x, ok := rc.X.(*syntax.Name)
	if !ok {
		return
	}
	v, _ := s.info.Uses[x].(*types2.Var)
	sp := tracked[v]
	if sp == nil || !sp.slice {
		return
	}
	lhs, ok := rc.Lhs.(*syntax.ListExpr)
	if !ok || len(lhs.ElemList) != 2 {
		return
	}
	val, ok := lhs.ElemList[1].(*syntax.Name)
	if !ok {
		sp.ok = false
		return
	}
	if val.Value == "_" {
		return
	}
	ev, _ := s.info.Defs[val].(*types2.Var)
	if ev == nil || !rc.Def {
		sp.ok = false
		return
	}
	tracked[ev] = &specParam{fn: sp.fn, root: sp, v: ev, ok: true}
}

// classifyUse checks one use of a tracked name: a parameter, or the element
// variable of a range over a slice parameter. Any use outside the allowed
// set clears ok on the parameter.
func (s *specializer) classifyUse(p *specParam, n *syntax.Name, stack []syntax.Node) {
	target := p
	if p.root != nil {
		target = p.root
	}
	var allowed bool
	switch {
	case p.slice:
		allowed = s.sliceUse(target, n, stack)
	default:
		allowed = s.operandUse(target, p.root != nil, n, stack)
	}
	if !allowed {
		target.ok = false
	}
}

// sliceUse allows len and cap calls, range loops, and an index expression
// whose element is used as an operand. Slicing, appending, storing,
// comparing, and passing the slice on all fall outside it.
func (s *specializer) sliceUse(target *specParam, n *syntax.Name, stack []syntax.Node) bool {
	switch par := stack[len(stack)-2].(type) {
	case *syntax.RangeClause:
		return par.X == syntax.Expr(n)
	case *syntax.CallExpr:
		fun, ok := par.Fun.(*syntax.Name)
		return ok && par.Fun.GetTypeInfo().IsBuiltin() && (fun.Value == "len" || fun.Value == "cap") &&
			len(par.ArgList) == 1 && par.ArgList[0] == syntax.Expr(n)
	case *syntax.IndexExpr:
		return par.X == syntax.Expr(n) && s.operandUse(target, true, par, stack[:len(stack)-1])
	}
	return false
}

// operandUse checks one use of e, the value of an interface parameter or
// one element of a slice parameter, at the top of stack. The allowed uses
// are a direct method call on it, a direct argument to a non-generic call
// whose parameter is an interface, a result of interface type, and (for a
// parameter, not an element) an assertion or type switch on it.
func (s *specializer) operandUse(target *specParam, element bool, e syntax.Expr, stack []syntax.Node) bool {
	f := target.fn
	switch par := stack[len(stack)-2].(type) {
	case *syntax.SelectorExpr:
		return par.X == e && s.methodCallUse(target, par, stack)
	case *syntax.CallExpr:
		return s.passOnUse(par, e)
	case *syntax.Operation:
		// Interface parameters compared in an operation, including a
		// comparison with nil, cannot be safely specialized.
		return false
	case *syntax.AssertExpr:
		if element || par.X != e {
			return false
		}
		target.asserts = append(target.asserts, specAssert{node: par, parent: stack[len(stack)-3], fold: foldable(stack)})
		if foldable(stack) {
			target.benefit = true
		}
		return true
	case *syntax.TypeSwitchGuard:
		if element || par.X != e {
			return false
		}
		target.asserts = append(target.asserts, specAssert{node: par})
		return true
	case *syntax.ReturnStmt:
		return par.Results == e && specResultIface(f, 0, 1)
	case *syntax.ListExpr:
		if len(stack) < 3 {
			return false
		}
		rs, ok := stack[len(stack)-3].(*syntax.ReturnStmt)
		if !ok || rs.Results != syntax.Expr(par) {
			return false
		}
		for i, x := range par.ElemList {
			if x == e && specResultIface(f, i, len(par.ElemList)) {
				return true
			}
		}
	}
	return false
}

// methodCallUse records a call e.M(...) through the interface and reports
// whether sel is one. The selector sits below e on the stack and its call
// below the selector.
func (s *specializer) methodCallUse(target *specParam, sel *syntax.SelectorExpr, stack []syntax.Node) bool {
	sel2 := s.info.Selections[sel]
	if sel2 == nil || sel2.Kind() != types2.MethodVal || len(stack) < 3 {
		return false
	}
	call, ok := stack[len(stack)-3].(*syntax.CallExpr)
	if !ok || call.Fun != syntax.Expr(sel) {
		return false
	}
	m, ok := sel2.Obj().(*types2.Func)
	if !ok {
		return false
	}
	target.methods = append(target.methods, m)
	target.benefit = true
	return true
}

// foldable reports whether the assertion below the top of stack (the
// operand is the top, its assertion the one below) is a comma-ok form that
// the clone folds to constants: v, ok := x.(T) or var v, ok = x.(T).
func foldable(stack []syntax.Node) bool {
	if len(stack) < 3 {
		return false
	}
	assert := stack[len(stack)-2]
	switch par := stack[len(stack)-3].(type) {
	case *syntax.AssignStmt:
		list, ok := par.Lhs.(*syntax.ListExpr)
		return ok && len(list.ElemList) == 2 && par.Rhs == assert
	case *syntax.VarDecl:
		return len(par.NameList) == 2 && par.Values == assert
	}
	return false
}

// passOnUse reports whether e is a direct argument of a call to a
// non-generic function value or method whose parameter at that position is
// an interface type, so the clone converts the concrete value exactly as the
// original converted the interface value.
func (s *specializer) passOnUse(call *syntax.CallExpr, e syntax.Expr) bool {
	idx := -1
	for i, a := range call.ArgList {
		if a == e {
			idx = i
			break
		}
	}
	if idx < 0 || call.HasDots {
		return false
	}
	tv := call.Fun.GetTypeInfo()
	if tv.Type == nil || tv.IsType() || tv.IsBuiltin() || s.genericCallee(call.Fun) {
		return false
	}
	sig, ok := tv.Type.Underlying().(*types2.Signature)
	if !ok {
		return false
	}
	params := sig.Params()
	switch {
	case sig.Variadic() && idx >= params.Len()-1:
		sl, ok := params.At(params.Len() - 1).Type().(*types2.Slice)
		return ok && specIsIface(sl.Elem())
	case idx < params.Len():
		return specIsIface(params.At(idx).Type())
	}
	return false
}

// genericCallee reports whether fun names a generic function, explicitly or
// by inference: retyping an argument would change what the call infers.
func (s *specializer) genericCallee(fun syntax.Expr) bool {
	var name *syntax.Name
	switch f := syntax.Unparen(fun).(type) {
	case *syntax.Name:
		name = f
	case *syntax.SelectorExpr:
		name = f.Sel
	case *syntax.IndexExpr:
		return true
	}
	if name == nil {
		return false
	}
	_, inst := s.info.Instances[name]
	return inst
}

// covSliceInner returns the operand of a conversion call rt.CovSlice[I](x),
// the call the const-slice lowering emits where a slice of concrete
// elements flows into a const slice of an interface. The name alone proves
// nothing about x: every use of the result checks the operand's element type
// against the interface itself.
func (s *specializer) covSliceInner(a syntax.Expr) (syntax.Expr, bool) {
	call, ok := syntax.Unparen(a).(*syntax.CallExpr)
	if !ok || len(call.ArgList) != 1 || call.HasDots {
		return nil, false
	}
	fun := syntax.Unparen(call.Fun)
	if ix, ok := fun.(*syntax.IndexExpr); ok {
		fun = ix.X
	}
	var name *syntax.Name
	switch f := fun.(type) {
	case *syntax.SelectorExpr:
		name = f.Sel
	case *syntax.Name:
		name = f
	default:
		return nil, false
	}
	fn, _ := s.info.Uses[name].(*types2.Func)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != rtImportPath || fn.Name() != "CovSlice" {
		return nil, false
	}
	return call.ArgList[0], true
}

// covSliceArgUnaliased reports whether the CovSlice conversion in argument
// i of call may drop its copy: the operand inner must be provably
// unaliased at this call site, so the clone's direct view of the backing
// cannot be observed through any other channel. Nil has no backing; a
// composite literal or make call owns a fresh one. A plain name qualifies
// only when it is function-scoped (never package-level or imported),
// bound once to a fresh or nil value (or a signature binding), used
// nowhere else in the file, and sharing the call with no argument that
// could reference the same backing. Every other shape (selectors on
// package variables and imported names, index and slice expressions,
// calls that may hand back shared state, and anything stored, captured,
// or passed twice) keeps the O1 CovSlice snapshot.
func (s *specializer) covSliceArgUnaliased(file *syntax.File, call *syntax.CallExpr, i int, inner syntax.Expr) bool {
	inner = syntax.Unparen(inner)
	if inner.GetTypeInfo().IsNil() {
		return true
	}
	switch e := inner.(type) {
	case *syntax.CompositeLit:
		return s.covSliceFreshType(inner)
	case *syntax.CallExpr:
		fun, ok := syntax.Unparen(e.Fun).(*syntax.Name)
		if !ok || !e.Fun.GetTypeInfo().IsBuiltin() || fun.Value != "make" {
			return false
		}
		return s.covSliceFreshType(inner)
	case *syntax.Name:
		return s.covSliceNameUnaliased(file, call, i, e)
	default:
		return false
	}
}

// covSliceFreshType reports whether e, a composite literal or make call
// used as a CovSlice operand, builds a fresh slice or array value whose
// backing no other expression can name.
func (s *specializer) covSliceFreshType(e syntax.Expr) bool {
	t := e.GetTypeInfo().Type
	if t == nil {
		return false
	}
	switch t.Underlying().(type) {
	case *types2.Slice, *types2.Array:
		return true
	}
	return false
}

// covSliceNameUnaliased reports whether the variable named by e, the
// operand of the CovSlice conversion in argument i of call, is provably
// unaliased at this call site: function-scoped, bound exactly once to a
// fresh or nil value (or a signature binding, whose backing arrives
// through this one channel), referenced nowhere else in the file, and
// sharing the call with no argument that could reach the same backing.
// Package-level variables and imported names always fail: their backing
// may be named from anywhere.
func (s *specializer) covSliceNameUnaliased(file *syntax.File, call *syntax.CallExpr, i int, e *syntax.Name) bool {
	v, _ := s.info.Uses[e].(*types2.Var)
	if v == nil || v.Pkg() == nil || v.Pkg() != s.pkg {
		return false
	}
	if s.pkg.Scope().Lookup(v.Name()) == v {
		return false
	}
	const (
		bindNone = iota
		bindSig
		bindFresh
		bindOther
	)
	bind := bindNone
	var (
		defs    int
		uses    int
		usesOwn bool
		stack   []syntax.Node
	)
	syntax.Inspect(file, func(n syntax.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		name, ok := n.(*syntax.Name)
		if !ok {
			return true
		}
		if d, ok := s.info.Defs[name]; ok && d == v {
			defs++
			switch par := stack[len(stack)-2].(type) {
			case *syntax.Field:
				bind = bindSig
			case *syntax.VarDecl:
				if len(par.NameList) == 1 && par.NameList[0] == name && s.covSliceFreshInit(par.Values) {
					bind = bindFresh
				} else {
					bind = bindOther
				}
			case *syntax.AssignStmt:
				if par.Op == syntax.Def && covSliceSingleName(par.Lhs, name) && s.covSliceFreshInit(par.Rhs) {
					bind = bindFresh
				} else {
					bind = bindOther
				}
			default:
				bind = bindOther
			}
			return true
		}
		if u, ok := s.info.Uses[name]; ok && u == v {
			uses++
			usesOwn = usesOwn || name == e
		}
		return true
	})
	if defs != 1 || uses != 1 || !usesOwn || (bind != bindSig && bind != bindFresh) {
		return false
	}
	return s.covSliceSiblingsFresh(call, i)
}

// covSliceFreshInit reports whether init, the initializer of a local
// CovSlice operand, builds a fresh or nil slice value: nil, a composite
// literal or make call of slice or array type. A missing initializer is
// the zero value, a nil slice. Anything else (another variable, a call, a
// selector) may name shared state.
func (s *specializer) covSliceFreshInit(init syntax.Expr) bool {
	if init == nil {
		return true
	}
	init = syntax.Unparen(init)
	if init.GetTypeInfo().IsNil() {
		return true
	}
	switch e := init.(type) {
	case *syntax.CompositeLit:
		return s.covSliceFreshType(init)
	case *syntax.CallExpr:
		fun, ok := syntax.Unparen(e.Fun).(*syntax.Name)
		if !ok || !e.Fun.GetTypeInfo().IsBuiltin() || fun.Value != "make" {
			return false
		}
		return s.covSliceFreshType(init)
	default:
		return false
	}
}

// covSliceSingleName reports whether lhs is exactly the name being defined,
// either bare or wrapped in a one-element list.
func covSliceSingleName(lhs syntax.Expr, name *syntax.Name) bool {
	switch l := syntax.Unparen(lhs).(type) {
	case *syntax.Name:
		return l == name
	case *syntax.ListExpr:
		return len(l.ElemList) == 1 && l.ElemList[0] == syntax.Expr(name)
	}
	return false
}

// covSliceSiblingsFresh reports whether every argument of call other than
// i provably cannot reference the CovSlice operand's backing: nil, a fresh
// composite literal or make call, or a value of basic type. Anything else
// (another slice or reference, a call result, a selector) may alias the
// operand through the caller, so the site keeps its snapshot.
func (s *specializer) covSliceSiblingsFresh(call *syntax.CallExpr, i int) bool {
	for j, arg := range call.ArgList {
		if j == i {
			continue
		}
		a := syntax.Unparen(arg)
		tv := a.GetTypeInfo()
		if tv.IsNil() {
			continue
		}
		if t := tv.Type; t != nil {
			if _, ok := t.Underlying().(*types2.Basic); ok {
				continue
			}
		}
		switch e := a.(type) {
		case *syntax.CallExpr:
			fun, ok := syntax.Unparen(e.Fun).(*syntax.Name)
			if !ok || !e.Fun.GetTypeInfo().IsBuiltin() || fun.Value != "make" {
				return false
			}
		case *syntax.CompositeLit:
			if !s.covSliceLiteralFresh(e) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// covSliceLiteralFresh reports whether a composite literal sibling names no
// variable, so its value cannot alias the CovSlice operand's backing
// through the caller. Type and field names are fine; any variable
// reference blocks.
func (s *specializer) covSliceLiteralFresh(e *syntax.CompositeLit) bool {
	fresh := true
	syntax.Inspect(e, func(n syntax.Node) bool {
		name, ok := n.(*syntax.Name)
		if !ok {
			return true
		}
		if w, ok := s.info.Uses[name].(*types2.Var); ok && !w.IsField() {
			fresh = false
		}
		return true
	})
	return fresh
}

// specResultIface reports whether result i of a return with count values
// has an interface type in f's signature.
func specResultIface(f *specFunc, i, count int) bool {
	results := f.obj.Signature().Results()
	return results.Len() == count && specIsIface(results.At(i).Type())
}

// specIsIface reports whether t is an interface type other than a type
// parameter.
func specIsIface(t types2.Type) bool {
	t = types2.Unalias(t)
	if _, ok := t.(*types2.TypeParam); ok {
		return false
	}
	_, ok := t.Underlying().(*types2.Interface)
	return ok
}

// specSliceOfIface returns the element type of an unnamed slice whose
// elements have an interface type.
func specSliceOfIface(t types2.Type) (types2.Type, bool) {
	sl, ok := types2.Unalias(t).(*types2.Slice)
	if !ok || !specIsIface(sl.Elem()) {
		return nil, false
	}
	return sl.Elem(), true
}
