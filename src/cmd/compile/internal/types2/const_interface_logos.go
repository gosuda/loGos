// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

import (
	"cmd/compile/internal/syntax"
	. "internal/types/errors"
)

// Readonly interface obligations for loGo/s (SPEC Sec 2.1, T-201 Qual).
//
// An abstract interface method declared as `interface { const Name() ... }`
// carries a readonly obligation: every implementer must be provably
// readonly. For concrete methods the Func bit is a checked capability,
// never a speculative promise: it is set only after an actual proof
// (local body scan) or from a trusted imported fact. Unknown effects fail
// closed.
//
// Satisfaction (lookup.go missingMethod) enforces obligations for
// concrete providers via a body proof; local bodies that are not yet
// checked defer to a package-end cleaner that reports const_view_mutation
// at the actual assignment/call operand, so declaration order cannot
// change legality. Direct const-view calls (logosConstSelect) reject plain
// pointer receivers before any scan; only an explicit const receiver is
// admitted directly. Qualified interface satisfaction is a separate policy
// operation: it defers pointer as well as value bodies and scans them, so
// pure pointer getters satisfy while direct calls still reject.

// logosInterfaceHasReadonly reports whether interface T declares any
// readonly method obligations.
func logosInterfaceHasReadonly(T *Interface) bool {
	if T == nil {
		return false
	}
	for _, m := range T.typeSet().methods {
		if m.LogosReadonly() {
			return true
		}
	}
	return false
}

// logosReadonlyObligation reports whether abstract method m requires a
// readonly implementer.
func logosReadonlyObligation(m *Func) bool {
	return m != nil && m.LogosReadonly()
}

// logosInterfaceReadonlyMismatch is the interface-to-interface obligation
// check: a required readonly method m must be matched by a providing
// readonly obligation f.
func logosInterfaceReadonlyMismatch(f, m *Func) bool {
	if !logosReadonlyObligation(m) {
		return false
	}
	return !logosReadonlyObligation(f)
}

// logosQualifiedIface returns the underlying interface of T when it carries
// a readonly obligation, or nil otherwise.
func logosQualifiedIface(T Type) *Interface {
	if T == nil {
		return nil
	}
	u, _ := Unalias(T).Underlying().(*Interface)
	if u != nil && logosInterfaceHasReadonly(u) {
		return u
	}
	return nil
}

// Assertion boundary (SPEC Sec 2.1, T-201 Qual).
//
// Runtime Go erases the readonly bit, so `x.(T)` and a type switch case T
// cannot check purity dynamically: any dynamic type reaching `any` or an
// ordinary interface may mutate through a method of the same name and
// signature. An interface (or constraint-interface type parameter) target T
// that requires readonly methods is therefore admitted only when the source
// interface V already guarantees each of them statically: V's type set must
// contain a readonly method with the same Id (name, and package when
// unexported) and an identical signature. Ordinary additional methods of T
// need no static proof; the runtime itself checks them. A concrete target
// is untouched: the concrete type assertion is runtime-checkable and the
// later assignment to a qualified interface is proved by the body scan in
// missingMethod (lookup.go). Both assertion entries (the AssertExpr,
// including comma-ok forms, and every type switch case, including type
// switch expressions) flow through Checker.typeAssertion, which asks
// assertableTo; the exported AssertableTo asks newAssertableTo.

// logosAssertReadonlyGap returns the first readonly method m required by
// the interface assertion target T that the interface source V does not
// guarantee, together with the source method f of the same Id when one
// exists (nil when V does not declare m at all). It returns (nil, nil) when
// T requires no readonly method or V implies every one. A source that is not
// an interface carries no contract and fails closed.
func logosAssertReadonlyGap(V, T Type) (*Func, *Func) {
	tu, _ := T.Underlying().(*Interface)
	if tu == nil {
		return nil, nil
	}
	var vset *_TypeSet
	for _, m := range tu.typeSet().methods {
		ro := logosReadonlyObligation(m)
		if !ro && !logosSigErasableQualified(m.typ, nil) {
			continue
		}
		if vset == nil {
			vu, _ := V.Underlying().(*Interface)
			if vu == nil {
				return m, nil
			}
			vset = vu.typeSet()
		}
		_, f := vset.LookupMethod(m.pkg, m.name, false)
		if f == nil || !Identical(f.typ, m.typ) || (ro && !logosReadonlyObligation(f)) {
			return m, f
		}
	}
	return nil, nil
}

// logosAssertReadonly reports whether asserting a value of interface type V
// to interface (or constraint-interface type parameter) T needs no readonly
// proof beyond what V's own contracts carry. If the result is false and
// cause is not nil, *cause describes the unproven method in parentheses.
// check may be nil (exported AssertableTo).
func (check *Checker) logosAssertReadonly(V, T Type, cause *string) bool {
	if check != nil {
		if logosContainsTypeParam(T, nil) {
			return true // Recorded target is checked with concrete arguments after monomorphization.
		}
	}
	m, f := logosAssertReadonlyGap(V, T)
	if m == nil {
		return true
	}
	if cause != nil {
		switch {
		case f == nil:
			*cause = check.sprintf("(readonly method %s is not declared by %s)", m.Name(), V)
		case !logosReadonlyObligation(f):
			*cause = check.sprintf("(method %s is not readonly in %s)", m.Name(), V)
		default:
			*cause = check.sprintf("(readonly method %s has a different signature in %s)", m.Name(), V)
		}
	}
	return false
}

// logosAssertReadonlyError reports the const_view_mutation diagnostic for an
// assertion (typeSwitch false) or type switch case (typeSwitch true) that
// assertableTo rejected, and reports true. An interface target can be
// rejected only by a readonly obligation, so cause then comes from
// logosAssertReadonly. It reports false for a concrete target, leaving the
// upstream impossible-assertion diagnostic to the caller. e is the assertion
// or case type expression, for the position.
func (check *Checker) logosAssertReadonlyError(e syntax.Expr, x *operand, T Type, typeSwitch bool, cause string) bool {
	if !IsInterface(T) {
		if !logosErasableQualified(T, nil) {
			return false
		}
		what := "type assertion"
		if typeSwitch {
			what = "type switch case"
		}
		check.errorf(e, ImpossibleAssert, "%s unchecked qualified %s: %s\n\t%s does not guarantee an identity-checkable %s", logosConstMutation, what, e, x, T)
		return true
	}
	what := "type assertion"
	if typeSwitch {
		what = "type switch case"
	}
	check.errorf(e, ImpossibleAssert, "%s unchecked qualified %s: %s\n\t%s does not guarantee %s %s", logosConstMutation, what, e, x, T, cause)
	return true
}

// logosConcreteReadonlySatisfied reports whether concrete method f satisfies
// the readonly obligation of required method m, registering a package-end
// deferred check with unknown position when a body proof is not yet
// available. Prefer logosConcreteReadonlySatisfiedAt when the use site is
// known; it captures the position at defer time.
func (check *Checker) logosConcreteReadonlySatisfied(V Type, f, m *Func) bool {
	return check.logosConcreteReadonlySatisfiedAt(V, f, m, nopos)
}

// logosConcreteReadonlySatisfiedAt is the use-site-aware satisfaction check.
// pos is the actual assignment/call operand position; deferred checks
// capture it at defer time and report there, never at the interface
// declaration. Callers on owned paths with an operand (logosConstAssign,
// covariance element checks) pass the operand position; upstream
// missingMethod passes nopos and falls back to the concrete method.
func (check *Checker) logosConcreteReadonlySatisfiedAt(V Type, f, m *Func, pos syntax.Pos) bool {
	if !logosReadonlyObligation(m) {
		return true
	}
	if f == nil {
		return false
	}
	if check == nil {
		return logosConcreteReadonlySatisfiedNil(f, m)
	}
	sig, ok := f.Type().(*Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
		return false
	}
	if rp := sig.RecvTypeParams(); rp != nil && rp.Len() > 0 {
		return false
	}
	if rt0 := sig.Recv().Type(); rt0 != nil {
		base := Unalias(rt0)
		if p, ok := base.(*Pointer); ok {
			base = Unalias(p.Elem())
		}
		if n, ok := base.(*Named); ok {
			if tp := n.TypeParams(); tp != nil && tp.Len() > 0 {
				return false
			}
		}
	}
	rt := sig.Recv().Type()
	local := f.Pkg() == check.pkg
	if !local {
		return check.logosImportedReadonly(f)
	}
	if f.Origin() != f {
		return false
	}
	if logosAsConstView(rt) != nil {
		f.SetLogosReadonly(true)
		return true
	}
	if logosReferenceFree(rt) {
		f.SetLogosReadonly(true)
		return true
	}
	check.needsCleanup(&logosSatisfyReadonlyDeferred{check: check, V: V, f: f, m: m, pos: pos})
	return true
}

// logosSatisfyReadonlyDeferred is a package-end check for one local concrete
// method that had to satisfy a readonly obligation before its proof was
// available.
type logosSatisfyReadonlyDeferred struct {
	check *Checker
	V     Type
	f, m  *Func
	pos   syntax.Pos
}

func (d *logosSatisfyReadonlyDeferred) cleanup() {
	check := d.check
	if check == nil || d.f == nil || d.m == nil {
		return
	}
	if d.f.LogosReadonly() {
		return
	}
	if check.logosReceiverReadonly(d.f) {
		d.f.SetLogosReadonly(true)
		return
	}
	pos := d.pos
	if !pos.IsKnown() {
		pos = d.f.Pos()
	}
	var at poser = noposer{pos: pos}
	check.errorf(at, IncompatibleAssign, "%s %s does not implement readonly method %s (method %s is not readonly)", logosConstMutation, d.V, d.m.Name(), d.f.Name())
}

// noposer adapts a syntax.Pos to the Checker poser interface.
type noposer struct {
	pos syntax.Pos
}

func (n noposer) Pos() syntax.Pos { return n.pos }

// logosEmbedReadonlyConflict reports whether two embedded methods with the
// same Id and identical signatures carry incompatible readonly obligations.
func logosEmbedReadonlyConflict(a, b *Func) bool {
	if a == nil || b == nil {
		return false
	}
	return a.LogosReadonly() != b.LogosReadonly()
}
