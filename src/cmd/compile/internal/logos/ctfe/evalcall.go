// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ctfe

// This file implements M5-S2 calls, loops, switches, and builtins
// (SPEC Sec 2.1, T-201).

import (
	"cmd/compile/internal/syntax"
	"math/big"
	"unicode/utf8"
)

// evalCallMulti evaluates a call, reporting whether it produced several
// values (a multi-valued call). Single-valued calls return one value.
func (e *evaluator) evalCallMulti(fr *frame, c *syntax.CallExpr) ([]Value, bool, error) {
	if err := e.step(posOf(c)); err != nil {
		return nil, false, err
	}
	fun := syntax.Unparen(c.Fun)
	// Method calls never evaluate in const context.
	if sel, ok := fun.(*syntax.SelectorExpr); ok {
		if name, ok := sel.X.(*syntax.Name); ok {
			if ent := e.h.Resolve(name); ent.Kind == EntPackage {
				return e.entityCall(fr, e.h.ResolveSel(sel), c)
			}
		}
		return nil, false, e.impurity(fr, posOf(c), "method call in const func")
	}
	// Conversions carry a type in function position.
	if name, ok := fun.(*syntax.Name); ok {
		ent := e.h.Resolve(name)
		if ent.Kind == EntType {
			v, err := e.evalConversion(fr, c)
			if err != nil {
				return nil, false, err
			}
			return []Value{v}, false, nil
		}
		return e.entityCall(fr, ent, c)
	}
	switch fun.(type) {
	case *syntax.SliceType, *syntax.ArrayType, *syntax.MapType:
		v, err := e.evalConversion(fr, c)
		if err != nil {
			return nil, false, err
		}
		return []Value{v}, false, nil
	}
	if _, ok := fun.(*syntax.IndexExpr); ok {
		return nil, false, e.impurity(fr, posOf(c), "generic call in const func")
	}
	if lit, ok := fun.(*syntax.FuncLit); ok {
		_ = lit
		return nil, false, e.impurity(fr, posOf(c), "function literal in const func")
	}
	return nil, false, e.fail(posOf(c), "unsupported call in const evaluation")
}

// entityCall dispatches a resolved call target.
func (e *evaluator) entityCall(fr *frame, ent Entity, c *syntax.CallExpr) ([]Value, bool, error) {
	switch ent.Kind {
	case EntBuiltin:
		v, err := e.evalBuiltin(fr, ent.Builtin, c)
		if err != nil {
			return nil, false, err
		}
		return []Value{v}, false, nil
	case EntConstFunc, EntPlainFunc:
		return e.evalUserCall(fr, ent.Fn, c)
	case EntImport:
		return nil, false, e.impurity(fr, posOf(c), "call to imported function %s in const func", ent.Name)
	case EntPackageVar:
		return nil, false, e.impurity(fr, posOf(c), "call to package variable %s in const func", ent.Name)
	}
	return nil, false, e.fail(posOf(c), "unsupported call in const evaluation")
}

// evalUserCall binds arguments and runs a function body.
func (e *evaluator) evalUserCall(fr *frame, fn *Func, c *syntax.CallExpr) ([]Value, bool, error) {
	if fn.Generic {
		return nil, false, e.impurity(fr, posOf(c), "generic call in const func")
	}
	if fn.Body == nil {
		return nil, false, e.fail(posOf(c), "call to function %s without body in const evaluation", fn.Name)
	}
	args, err := e.evalArgs(fr, c)
	if err != nil {
		return nil, false, err
	}
	params := fn.Params
	if fn.Variadic {
		if len(args) < len(params)-1 {
			return nil, false, e.fail(posOf(c), "too few arguments in call to %s", fn.Name)
		}
	} else if len(args) != len(params) {
		return nil, false, e.fail(posOf(c), "wrong number of arguments in call to %s", fn.Name)
	}
	nfr := &frame{fn: fn, scopes: []map[string]Value{{}}}
	e.frames = append(e.frames, nfr)
	defer func() { e.frames = e.frames[:len(e.frames)-1] }()
	for i, p := range params {
		if fn.Variadic && i == len(params)-1 {
			var elemT *Type
			if p.Type != nil && p.Type.Kind == KSlice {
				elemT = p.Type.Elem
			}
			elems := make([]Value, 0, len(args)-i)
			for _, a := range args[i:] {
				v := copyArg(a)
				if elemT != nil {
					var err error
					v, err = e.convert(v, elemT, posOf(c))
					if err != nil {
						return nil, false, err
					}
				}
				elems = append(elems, withVar(copyValue(v)))
			}
			e.declare(nfr, p.Decl.Value, sliceValue(elemT, elems, len(elems)))
			break
		}
		v := copyArg(args[i])
		if p.Type != nil {
			if v, err = e.convert(v, p.Type, posOf(c)); err != nil {
				return nil, false, err
			}
		}
		e.declare(nfr, p.Decl.Value, withVar(v))
	}
	// Named results start as zero values.
	for i, name := range fn.ResultNames {
		if name == "" || i >= len(fn.Results) || fn.Results[i] == nil {
			continue
		}
		e.declare(nfr, name, zeroValue(fn.Results[i]))
	}
	if err := e.step(posOf(c)); err != nil {
		return nil, false, err
	}
	e.depth++
	f, err := e.execBlock(nfr, fn.Body)
	e.depth--
	if err != nil {
		return nil, false, err
	}
	if !f.returned {
		if len(fn.Results) == 0 {
			return nil, false, nil
		}
		// Fell off the end with named results: read them back.
		vals := make([]Value, 0, len(fn.Results))
		for _, name := range fn.ResultNames {
			if name == "" {
				return nil, false, e.fail(posOf(c), "missing return values in const evaluation")
			}
			v, ok := e.lookup(nfr, name)
			if !ok {
				return nil, false, e.fail(posOf(c), "missing return values in const evaluation")
			}
			vals = append(vals, v)
		}
		return vals, len(vals) > 1, nil
	}
	return f.values, len(f.values) > 1, nil
}

// evalArgs evaluates call arguments, spreading a trailing slice or
// string over ... .
func (e *evaluator) evalArgs(fr *frame, c *syntax.CallExpr) ([]Value, error) {
	var out []Value
	for i, a := range c.ArgList {
		v, err := e.evalExpr(fr, a)
		if err != nil {
			return nil, err
		}
		if c.HasDots && i == len(c.ArgList)-1 {
			if v.typ != nil && v.typ.Kind == KString {
				for _, r := range []byte(v.sval) {
					out = append(out, byteValue(r))
				}
				continue
			}
			if v.slice == nil {
				return nil, e.fail(posOf(a), "cannot spread non-slice in const evaluation")
			}
			out = append(out, sliceElems(v)...)
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// execFor runs classic, condition-only, infinite, and range loops.
// label names this loop when a LabeledStmt wraps it; a matching
// continue runs the post statement and starts the next iteration
// instead of leaving the loop.
func (e *evaluator) execFor(fr *frame, s *syntax.ForStmt, label string) (flow, error) {
	if rc, ok := s.Init.(*syntax.RangeClause); ok {
		return e.execRange(fr, s, rc, label)
	}
	// A loop body is never a direct switch-clause statement, so a
	// fallthrough inside one is out of place even when the loop sits
	// in a clause. A switch nested in the loop re-arms the gate itself.
	savedSwitch := e.inSwitch
	e.inSwitch = 0
	defer func() { e.inSwitch = savedSwitch }()
	e.pushScope(fr)
	defer e.popScope(fr)
	if s.Init != nil {
		f, err := e.execSimple(fr, s.Init)
		if err != nil {
			return flow{}, err
		}
		if f.returned || f.brk != "" || f.cont != "" {
			return f, nil
		}
	}
	for {
		if s.Cond != nil {
			cond, err := e.evalExpr(fr, s.Cond)
			if err != nil {
				return flow{}, err
			}
			b, err := e.truth(cond, posOf(s.Cond))
			if err != nil {
				return flow{}, err
			}
			if !b {
				return flow{}, nil
			}
		}
		if err := e.step(posOf(s)); err != nil {
			return flow{}, err
		}
		f, err := e.execBlock(fr, s.Body)
		if err != nil {
			return flow{}, err
		}
		if f.returned {
			return f, nil
		}
		if f.brk != "" {
			if f.brk == unlabeled || (label != "" && f.brk == label) {
				return flow{}, nil
			}
			return f, nil
		}
		if f.cont != "" && f.cont != unlabeled {
			if label != "" && f.cont == label {
				// Matching labeled continue: run post and iterate.
			} else {
				return f, nil
			}
		}
		if s.Post != nil {
			pf, err := e.execSimple(fr, s.Post)
			if err != nil {
				return flow{}, err
			}
			if pf.returned || pf.brk != "" || pf.cont != "" {
				return pf, nil
			}
		}
	}
}

// execRange runs for-range over slices, arrays, strings, and integers.
// Ranging over a map is impure (nondeterministic order).
func (e *evaluator) execRange(fr *frame, s *syntax.ForStmt, rc *syntax.RangeClause, label string) (flow, error) {
	// A range body is never a direct switch-clause statement, so a
	// fallthrough inside one is out of place even when the range sits
	// in a clause. A switch nested in the body re-arms the gate itself.
	savedSwitch := e.inSwitch
	e.inSwitch = 0
	defer func() { e.inSwitch = savedSwitch }()
	xv, err := e.evalExpr(fr, rc.X)
	if err != nil {
		return flow{}, err
	}
	if xv.typ != nil && xv.typ.Kind == KMap {
		return flow{}, e.impurity(fr, posOf(rc.X), "range over map in const func")
	}
	e.pushScope(fr)
	defer e.popScope(fr)
	var lhs []syntax.Expr
	if rc.Lhs != nil {
		lhs = unpackList(rc.Lhs)
	}
	for _, le := range lhs {
		if _, ok := syntax.Unparen(le).(*syntax.Name); !ok {
			return flow{}, e.fail(posOf(le), "unsupported range variables in const evaluation")
		}
	}
	setVar := func(name string, val Value) {
		if name == "_" {
			return
		}
		val = withVar(copyValue(val))
		if rc.Def {
			fr.scopes[len(fr.scopes)-1][name] = val
		} else {
			e.assignName(fr, name, val)
		}
	}
	// loop runs body once per element with the given index/value.
	// A second loop variable binds only when the clause names two.
	// The counter is a big.Int so counts wider than int64 (such as
	// uint64(1)<<63) iterate faithfully: break and return still exit
	// early, and e.step enforces MaxSteps on unbounded loops.
	loop := func(idx func(i *big.Int) Value, val func(i *big.Int) Value, n *big.Int) (flow, error) {
		for i := new(big.Int); i.Cmp(n) < 0; i.Add(i, big.NewInt(1)) {
			if err := e.step(posOf(s)); err != nil {
				return flow{}, err
			}
			if len(lhs) > 0 {
				if n0, ok := syntax.Unparen(lhs[0]).(*syntax.Name); ok {
					setVar(n0.Value, withVar(idx(new(big.Int).Set(i))))
				}
			}
			if len(lhs) > 1 {
				if n1, ok := syntax.Unparen(lhs[1]).(*syntax.Name); ok {
					setVar(n1.Value, withVar(val(new(big.Int).Set(i))))
				}
			}
			f, err := e.execBlock(fr, s.Body)
			if err != nil {
				return flow{}, err
			}
			if f.returned {
				return f, nil
			}
			if f.brk != "" {
				if f.brk == unlabeled || (label != "" && f.brk == label) {
					return flow{}, nil
				}
				return f, nil
			}
			if f.cont != "" && f.cont != unlabeled {
				if label == "" || f.cont != label {
					return f, nil
				}
			}
		}
		return flow{}, nil
	}
	intIdx := func(i *big.Int) Value { return intValue(new(big.Int).Set(i), &Type{Kind: KInt}, false) }
	switch {
	case xv.slice != nil:
		elems := sliceElems(xv)
		return loop(intIdx, func(i *big.Int) Value { return elems[i.Int64()] }, big.NewInt(int64(len(elems))))
	case xv.array != nil:
		snap := make([]Value, len(xv.array))
		for i, el := range xv.array {
			snap[i] = copyValue(el)
		}
		return loop(intIdx, func(i *big.Int) Value { return snap[i.Int64()] }, big.NewInt(int64(len(snap))))
	case xv.typ != nil && isStringLike(xv.typ.Kind):
		// String literals evaluate untyped (KUntypedString); range
		// covers them exactly like typed strings, yielding UTF-8
		// byte offsets as indices and U+FFFD for invalid bytes.
		s := xv.sval
		var runes []rune
		var offs []int
		for off := 0; off < len(s); {
			r, sz := utf8.DecodeRuneInString(s[off:])
			offs = append(offs, off)
			runes = append(runes, r)
			off += sz
		}
		return loop(func(i *big.Int) Value {
			return intValue(big.NewInt(int64(offs[i.Int64()])), &Type{Kind: KInt}, false)
		}, func(i *big.Int) Value {
			return runeValue(runes[i.Int64()])
		}, big.NewInt(int64(len(runes))))
	case xv.typ != nil && isInteger(xv.typ.Kind):
		// A negative count runs zero times. A positive count that
		// overflows int64 (so Int64 would wrap it negative) still
		// iterates faithfully through the big-counter loop above.
		if xv.ival.Sign() < 0 {
			return flow{}, nil
		}
		// Integer-range indices take the operand's type (an untyped
		// count defaults to int), matching gc: `range uint64(5)`
		// binds uint64 indices, `range 5` binds int.
		idxT := &Type{Kind: KInt}
		if !isUntyped(xv.typ.Kind) {
			idxT = cloneType(xv.typ)
		}
		intIdxT := func(i *big.Int) Value { return intValue(new(big.Int).Set(i), idxT, false) }
		return loop(intIdxT, intIdxT, new(big.Int).Set(xv.ival))
	}
	return flow{}, e.fail(posOf(rc.X), "cannot range in const evaluation")
}

// assignName assigns to a visible binding.
func (e *evaluator) assignName(fr *frame, name string, v Value) {
	for i := len(fr.scopes) - 1; i >= 0; i-- {
		if _, ok := fr.scopes[i][name]; ok {
			fr.scopes[i][name] = v
			return
		}
	}
	fr.scopes[len(fr.scopes)-1][name] = v
}

// execSwitch runs expression switches.
func (e *evaluator) execSwitch(fr *frame, s *syntax.SwitchStmt) (flow, error) {
	e.pushScope(fr)
	defer e.popScope(fr)
	if s.Init != nil {
		f, err := e.execSimple(fr, s.Init)
		if err != nil {
			return flow{}, err
		}
		if f.returned || f.brk != "" || f.cont != "" {
			return f, nil
		}
	}
	var tag Value
	hasTag := s.Tag != nil
	if hasTag {
		if _, ok := syntax.Unparen(s.Tag).(*syntax.TypeSwitchGuard); ok {
			return flow{}, e.fail(posOf(s.Tag), "type switch in const func not supported")
		}
		var err error
		tag, err = e.evalExpr(fr, s.Tag)
		if err != nil {
			return flow{}, err
		}
	} else {
		tag = Value{typ: &Type{Kind: KBool}, bval: true, constant: true}
	}
	// Select the first matching explicit case; the default runs only
	// when no explicit case matches. Fallthrough then chains forward
	// from the selected clause in source order.
	matchIdx := -1
	defaultIdx := -1
	for i, cc := range s.Body {
		if cc.Cases == nil {
			if defaultIdx < 0 {
				defaultIdx = i
			}
			continue
		}
		hit := false
		for _, cx := range unpackList(cc.Cases) {
			cv, err := e.evalExpr(fr, cx)
			if err != nil {
				return flow{}, err
			}
			eq, err := e.equal(tag, cv, posOf(cx))
			if err != nil {
				return flow{}, err
			}
			if eq {
				hit = true
				break
			}
		}
		if hit {
			matchIdx = i
			break
		}
	}
	start := matchIdx
	if start < 0 {
		start = defaultIdx
	}
	if start < 0 {
		return flow{}, nil
	}
	// Arm the fallthrough gate only around direct clause statements:
	// execBlock propagates flow{fall} out, and the chain below
	// consumes it by entering the next clause in source order.
	e.inSwitch++
	defer func() { e.inSwitch-- }()
	falling := false
	for i := start; i < len(s.Body); i++ {
		cc := s.Body[i]
		if i != start && !falling {
			break
		}
		if i != start {
			falling = false
		}
		f, err := e.execBlock(fr, &syntax.BlockStmt{List: cc.Body})
		if err != nil {
			return flow{}, err
		}
		if f.returned {
			return f, nil
		}
		if f.fall {
			falling = true
			continue
		}
		if f.brk != "" {
			if f.brk == unlabeled {
				return flow{}, nil
			}
			return f, nil
		}
		if f.cont != "" {
			return f, nil
		}
		return flow{}, nil
	}
	return flow{}, nil
}
