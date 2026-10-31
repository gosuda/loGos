// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-license
// file in the LICENSE file.

package ctfe

// Spread boundary attacks (public Eval seam only).
//
// Independent Go truth (installed toolchain, not this package):
//   - doc/go_spec.html:4339-4343: f invoked with no actual args for
//     ...T binds nil (Greeting("nobody") => who == nil).
//   - doc/go_spec.html:4365-4368: final arg assignable to []T followed
//     by ... passes unchanged for ...T (no new slice).
//   - doc/go_spec.html:7441-7444: "As a special case, append also
//     accepts" []byte plus string... (append-only expansion).
//   - src/go/types/builtins.go:94-96: "append also accepts ... string
//     type followed by ..." (first arg assignable to []byte).
//   - src/go/types/builtins_test.go:25: `append(s, "foo"...)` has type
//     `func([]byte, string...) []byte` (only append, never ordinary).
// Ordinary ...byte/...int/...rune calls with string... are therefore
// rejections, not expansions. Typed nil []T... must bind the nil slice
// (len 0, == nil); an untyped string literal must expand for append
// exactly like a typed string.
//
// Current evalArgs (evalcall.go) rejects typed nil (v.slice == nil),
// matches only KString (missing KUntypedString), and expands a typed
// string for any variadic element type. Each test below pins one
// boundary through Eval plus exported accessors; no evaluator
// internals and no error-text assertions.

import "testing"

// TestSpreadTypedNilSliceBindsZeroLen attacks the nil-slice boundary:
// a typed nil []int spread must bind len 0, not fail with "cannot
// spread non-slice". The nil slice is built with `var s []int` inside
// the const func, so no host Value is constructed by hand.
func TestSpreadTypedNilSliceBindsZeroLen(t *testing.T) {
	intT := BasicType("int", 8)
	sliceT := &Type{Kind: KSlice, Elem: intT}

	h := newFakeHost(t, 8)
	fn := parseFunc(t, "package p\nfunc VLen(xs ...int) int { return len(xs) }",
		true, []*Type{sliceT}, []*Type{intT})
	fn.Variadic = true
	h.funcs["VLen"] = fn
	caller := parseFunc(t, "package p\nfunc CallNilLen() int { var s []int; return VLen(s...) }",
		true, nil, []*Type{intT})
	h.funcs[caller.Name] = caller

	v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, `CallNilLen()`))
	if got := v.Int64(); got != 0 {
		t.Fatalf("CallNilLen() = %d, want 0", got)
	}
}

// TestSpreadNilVsEmptyIdentity attacks nil identity through spread: a
// typed nil spread must stay == nil while an empty (non-nil) spread
// must stay != nil. Both have len 0; only the nil bit differs. Both
// operands are built inside const funcs (`var s []int` vs `[]int{}`),
// matching value_edges_regress_test.go:412-428.
func TestSpreadNilVsEmptyIdentity(t *testing.T) {
	intT := BasicType("int", 8)
	sliceT := &Type{Kind: KSlice, Elem: intT}
	boolT := BasicType("bool", 8)

	h := newFakeHost(t, 8)
	fn := parseFunc(t, "package p\nfunc IsNil(xs ...int) bool { return xs == nil }",
		true, []*Type{sliceT}, []*Type{boolT})
	fn.Variadic = true
	h.funcs["IsNil"] = fn
	nilCaller := parseFunc(t, "package p\nfunc CallNilIsNil() bool { var s []int; return IsNil(s...) }",
		true, nil, []*Type{boolT})
	h.funcs[nilCaller.Name] = nilCaller
	emptyCaller := parseFunc(t, "package p\nfunc CallEmptyIsNil() bool { return IsNil([]int{}...) }",
		true, nil, []*Type{boolT})
	h.funcs[emptyCaller.Name] = emptyCaller

	cfg := DefaultConfig()
	v, _ := evalOne(t, h, cfg, parseExpr(t, `CallNilIsNil()`))
	if !v.Bool() {
		t.Fatalf("CallNilIsNil() = false, want true")
	}
	v, _ = evalOne(t, h, cfg, parseExpr(t, `CallEmptyIsNil()`))
	if v.Bool() {
		t.Fatalf("CallEmptyIsNil() = true, want false")
	}
}

// TestAppendUntypedStringSpread attacks the untyped-string boundary:
// append([]byte{65}, "hi"...) must expand the literal exactly like a
// typed string, yielding [65 'h' 'i'].
func TestAppendUntypedStringSpread(t *testing.T) {
	intT := BasicType("int", 8)

	h := newFakeHost(t, 8)
	fn := parseFunc(t, "package p\nfunc AppendUntyped() int {\n\ts := append([]byte{65}, \"hi\"...)\n\treturn int(s[1])\n}",
		true, nil, []*Type{intT})
	h.funcs[fn.Name] = fn

	v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, `AppendUntyped()`))
	if got := v.Int64(); got != 104 {
		t.Fatalf("AppendUntyped() = %d, want 104 ('h')", got)
	}
}

// TestAppendTypedStringSpreadControl is the passing pair for the
// KString-only gap (evalcall.go:187): a typed string already flattens
// to bytes for append today, so this control must stay green while the
// untyped literal above exposes the missing KUntypedString case.
func TestAppendTypedStringSpreadControl(t *testing.T) {
	intT := BasicType("int", 8)

	h := newFakeHost(t, 8)
	fn := parseFunc(t, "package p\nfunc AppendTyped() int {\n\ts := append([]byte{65}, string(\"hi\")...)\n\treturn int(s[2])\n}",
		true, nil, []*Type{intT})
	h.funcs[fn.Name] = fn

	v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, `AppendTyped()`))
	if got := v.Int64(); got != 105 {
		t.Fatalf("AppendTyped() = %d, want 105 ('i')", got)
	}
}

// TestSpreadTypedStringToIntRuneRejected attacks false acceptance: a
// typed string spread into ordinary ...int or ...rune (non-append)
// must be rejected. Only append has the string... special case.
func TestSpreadTypedStringToIntRuneRejected(t *testing.T) {
	intT := BasicType("int", 8)
	int32T := BasicType("int32", 8)

	for _, tc := range []struct {
		name  string
		elem  *Type
		param string
	}{
		{"Int", intT, "int"},
		{"Rune", int32T, "rune"},
	} {
		h := newFakeHost(t, 8)
		sliceT := &Type{Kind: KSlice, Elem: tc.elem}
		fn := parseFunc(t, "package p\nfunc VS"+tc.name+"(xs ..."+tc.param+") int { return len(xs) }",
			true, []*Type{sliceT}, []*Type{intT})
		fn.Variadic = true
		h.funcs[fn.Name] = fn

		cerr := evalErr(t, h, DefaultConfig(), parseExpr(t, fn.Name+`(string("hi")...)`))
		if cerr.Code != CodeEval {
			t.Fatalf("%s: code = %v, want CodeEval", tc.name, cerr.Code)
		}
	}
}

// TestSpreadTypedStringToOrdinaryByteRejected attacks the append-only
// boundary: even with a matching element type, an ordinary (non-append)
// ...byte callee must reject string... Only append expands strings.
func TestSpreadTypedStringToOrdinaryByteRejected(t *testing.T) {
	intT := BasicType("int", 8)
	uint8T := BasicType("uint8", 8)
	sliceT := &Type{Kind: KSlice, Elem: uint8T}

	h := newFakeHost(t, 8)
	fn := parseFunc(t, "package p\nfunc VByte(xs ...byte) int { return len(xs) }",
		true, []*Type{sliceT}, []*Type{intT})
	fn.Variadic = true
	h.funcs["VByte"] = fn

	cerr := evalErr(t, h, DefaultConfig(), parseExpr(t, `VByte(string("hi")...)`))
	if cerr.Code != CodeEval {
		t.Fatalf("code = %v, want CodeEval", cerr.Code)
	}
}
