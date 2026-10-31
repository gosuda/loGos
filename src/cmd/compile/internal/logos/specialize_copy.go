// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"reflect"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// specCopier deep-copies syntax trees. memo maps each original node to its
// copy, so nodes shared in the original (the type of a grouped parameter
// list, a branch target) stay shared in the copy. Recorded type
// information is cleared: the recheck sets it again.
type specCopier struct {
	memo   map[syntax.Node]syntax.Node
	groups map[*syntax.Group]*syntax.Group
}

func newSpecCopier() *specCopier {
	return &specCopier{
		memo:   make(map[syntax.Node]syntax.Node),
		groups: make(map[*syntax.Group]*syntax.Group),
	}
}

// node returns the copy of n, copying it first when needed.
func (c *specCopier) node(n syntax.Node) syntax.Node {
	if cp, ok := c.memo[n]; ok {
		return cp
	}
	src := reflect.ValueOf(n).Elem()
	dst := reflect.New(src.Type())
	dst.Elem().Set(src)
	cp := dst.Interface().(syntax.Node)
	c.memo[n] = cp
	c.fields(dst.Elem())
	if e, ok := cp.(syntax.Expr); ok {
		e.SetTypeInfo(syntax.TypeAndValue{})
	}
	return cp
}

// fields replaces every exported node-valued field of the struct v with a
// copy. Unexported fields (positions, recorded types) keep the struct copy.
func (c *specCopier) fields(v reflect.Value) {
	for i := range v.NumField() {
		if f := v.Field(i); f.CanSet() {
			c.value(f)
		}
	}
}

func (c *specCopier) value(v reflect.Value) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		if n, ok := v.Interface().(syntax.Node); ok {
			v.Set(reflect.ValueOf(c.node(n)))
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		switch p := v.Interface().(type) {
		case syntax.Node:
			v.Set(reflect.ValueOf(c.node(p)))
		case *syntax.Group:
			v.Set(reflect.ValueOf(c.group(p)))
		}
	case reflect.Slice:
		if v.IsNil() {
			return
		}
		dup := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(dup, v)
		for i := range dup.Len() {
			c.value(dup.Index(i))
		}
		v.Set(dup)
	case reflect.Array:
		for i := range v.Len() {
			c.value(v.Index(i))
		}
	}
}

func (c *specCopier) group(g *syntax.Group) *syntax.Group {
	if cp, ok := c.groups[g]; ok {
		return cp
	}
	cp := new(syntax.Group)
	c.groups[g] = cp
	return cp
}

// build copies the callee of c under the clone's name, retypes the
// specialized parameters, and rewrites the assertions on them. Sites are
// rewritten afterwards through the returned copier.
func (s *specializer) build(c *specClone) (*specCopier, *syntax.FuncDecl) {
	f := c.fn
	dup := newSpecCopier()
	decl := dup.node(f.decl).(*syntax.FuncDecl)
	decl.Name = nameOf(f.decl.Name.Pos(), c.name)
	for i, t := range c.types {
		if t == nil {
			continue
		}
		p := &f.params[i]
		spelled, ok := s.spell(f.file, t, p.field.Type.Pos())
		if !ok {
			base.Fatalf("logos specialize: cannot spell %s for %s", types2.TypeString(t, nil), c.name)
		}
		decl.Type.ParamList[i].Type = spelled
		for _, a := range p.asserts {
			s.rewriteAssert(dup, p, a, t)
		}
	}
	return dup, decl
}

func specBool(pos syntax.Pos, value bool) syntax.Expr {
	op := syntax.Neq
	if value {
		op = syntax.Eql
	}
	return at(&syntax.Operation{
		Op: op,
		X:  litOf(pos, "0", syntax.IntLit),
		Y:  litOf(pos, "0", syntax.IntLit),
	}, pos)
}

// rewriteAssert repairs one assertion or type switch on a retyped parameter
// in the clone. A comma-ok assertion folds to the constants the concrete
// type decides; every other form converts the parameter back to its
// declared interface first, which keeps the original result and panic.
func (s *specializer) rewriteAssert(dup *specCopier, p *specParam, a specAssert, c types2.Type) {
	declared := func() syntax.Expr {
		return newSpecCopier().node(p.field.Type).(syntax.Expr)
	}
	switch orig := a.node.(type) {
	case *syntax.TypeSwitchGuard:
		g := dup.memo[orig].(*syntax.TypeSwitchGuard)
		g.X = specConvert(declared(), g.X)
	case *syntax.AssertExpr:
		ae := dup.memo[orig].(*syntax.AssertExpr)
		if !foldAssert(dup, a, orig, ae, c) {
			ae.X = specConvert(declared(), ae.X)
		}
	}
}

// foldAssert replaces `v, ok := x.(T)` (or the var form) on a parameter of
// concrete type c with the pair the type decides: (T(x), true) when c
// implements the interface T or equals the concrete T, and (T(nil), false)
// for an interface T that c does not implement. Any other target keeps the
// dynamic assertion.
func foldAssert(dup *specCopier, a specAssert, orig, ae *syntax.AssertExpr, c types2.Type) bool {
	if !a.fold {
		return false
	}
	asserted := orig.Type.GetTypeInfo().Type
	if asserted == nil {
		return false
	}
	pos := ae.Pos()
	var value syntax.Expr
	var holds bool
	if iface, ok := asserted.Underlying().(*types2.Interface); ok {
		holds = types2.Implements(c, iface)
		operand := ae.X
		if !holds {
			operand = nilOf(pos)
		}
		value = specConvert(ae.Type, operand)
	} else if types2.Identical(c, asserted) {
		value, holds = ae.X, true
	} else {
		return false
	}
	pair := at(&syntax.ListExpr{ElemList: []syntax.Expr{value, specBool(pos, holds)}}, pos)
	switch par := dup.memo[a.parent].(type) {
	case *syntax.AssignStmt:
		par.Rhs = pair
	case *syntax.VarDecl:
		par.Values = pair
	default:
		return false
	}
	return true
}

// specConvert builds (typ)(x) at the position of x.
func specConvert(typ, x syntax.Expr) syntax.Expr {
	pos := x.Pos()
	return at(&syntax.CallExpr{Fun: at(&syntax.ParenExpr{X: typ}, pos), ArgList: []syntax.Expr{x}}, pos)
}
