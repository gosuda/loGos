// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ctfe

import (
	"math/big"
	"testing"
)

// TestSemanticConstRecordedHostIsolationDirect verifies that reading a
// compiler-owned recorded constant from the host (EntConst via Resolve)
// isolates the evaluator from the host's stored value. A const function
// mutating s := C must not corrupt the host's recorded slice backing.
func TestSemanticConstRecordedHostIsolationDirect(t *testing.T) {
	intT := BasicType("int", 8)

	h := newFakeHost(t, 8)
	// Recorded const C := []int{1, 2, 3}
	cBacking := []Value{
		intValue(big.NewInt(1), intT, true),
		intValue(big.NewInt(2), intT, true),
		intValue(big.NewInt(3), intT, true),
	}
	h.vars["C"] = sliceValue(intT, cBacking, 3)

	fn := parseFunc(t, `package p
func DirectMutate() int {
	s := C
	s[0] = 99
	return s[0]
}`, true, nil, []*Type{intT})
	h.funcs[fn.Name] = fn

	cfg := DefaultConfig()
	res, _ := evalOne(t, h, cfg, parseExpr(t, `DirectMutate()`))
	if got := res.Int64(); got != 99 {
		t.Fatalf("DirectMutate() = %d, want 99", got)
	}

	// Host's recorded constant must remain pristine.
	hostC := h.vars["C"]
	vis := hostC.SliceVisible()
	if len(vis) != 3 {
		t.Fatalf("host C len = %d, want 3", len(vis))
	}
	if got := vis[0].Int64(); got != 1 {
		t.Fatalf("host C[0] corrupted: got %d, want 1", got)
	}
}

// TestSemanticConstRecordedHostIsolationArg controls for argument passing:
// calling Mutate(C) passes the hosted constant as a call argument.
// In-interpreter argument passing must not reach the host's stored constant.
func TestSemanticConstRecordedHostIsolationArg(t *testing.T) {
	intT := BasicType("int", 8)
	sliceT := &Type{Kind: KSlice, Elem: intT}

	h := newFakeHost(t, 8)
	cBacking := []Value{
		intValue(big.NewInt(1), intT, true),
		intValue(big.NewInt(2), intT, true),
		intValue(big.NewInt(3), intT, true),
	}
	h.vars["C"] = sliceValue(intT, cBacking, 3)

	mutFn := parseFunc(t, `package p
func Mutate(s []int) int {
	s[0] = 99
	return s[0]
}`, true, []*Type{sliceT}, []*Type{intT})
	h.funcs[mutFn.Name] = mutFn

	callFn := parseFunc(t, `package p
func CallMutate() int {
	return Mutate(C)
}`, true, nil, []*Type{intT})
	h.funcs[callFn.Name] = callFn

	cfg := DefaultConfig()
	res, _ := evalOne(t, h, cfg, parseExpr(t, `CallMutate()`))
	if got := res.Int64(); got != 99 {
		t.Fatalf("CallMutate() = %d, want 99", got)
	}

	hostC := h.vars["C"]
	vis := hostC.SliceVisible()
	if got := vis[0].Int64(); got != 1 {
		t.Fatalf("host C[0] corrupted via arg call: got %d, want 1", got)
	}
}

// TestSemanticConstLocalArgAliasing verifies that calling a local function
// with a slice argument shares its backing store, adhering to Go sharing
// semantics.
func TestSemanticConstLocalArgAliasing(t *testing.T) {
	intT := BasicType("int", 8)
	sliceT := &Type{Kind: KSlice, Elem: intT}

	h := newFakeHost(t, 8)
	setFn := parseFunc(t, `package p
func Set(s []int, i int, v int) {
	s[i] = v
}`, true, []*Type{sliceT, intT, intT}, nil)
	h.funcs[setFn.Name] = setFn

	localFn := parseFunc(t, `package p
func LocalSlice() int {
	s := []int{0, 0}
	Set(s, 0, 42)
	return s[0]
}`, true, nil, []*Type{intT})
	h.funcs[localFn.Name] = localFn

	cfg := DefaultConfig()
	res, _ := evalOne(t, h, cfg, parseExpr(t, `LocalSlice()`))
	if got := res.Int64(); got != 42 {
		t.Fatalf("LocalSlice() = %d, want 42", got)
	}
}

// TestSemanticConstArrayCopyControl verifies that arrays copy by value
// when passed to a function call.
func TestSemanticConstArrayCopyControl(t *testing.T) {
	intT := BasicType("int", 8)
	arrT := &Type{Kind: KArray, Len: 2, Elem: intT}

	h := newFakeHost(t, 8)
	setArrFn := parseFunc(t, `package p
func SetArr(a [2]int) {
	a[0] = 99
}`, true, []*Type{arrT}, nil)
	h.funcs[setArrFn.Name] = setArrFn

	localFn := parseFunc(t, `package p
func LocalArray() int {
	a := [2]int{1, 2}
	SetArr(a)
	return a[0]
}`, true, nil, []*Type{intT})
	h.funcs[localFn.Name] = localFn

	cfg := DefaultConfig()
	res, _ := evalOne(t, h, cfg, parseExpr(t, `LocalArray()`))
	if got := res.Int64(); got != 1 {
		t.Fatalf("LocalArray() = %d, want 1", got)
	}
}

// TestSemanticConstVariadicSpread verifies that f(xs...) binds xs directly
// without repacking into a fresh slice, preserving slice backing and capacity.
func TestSemanticConstVariadicSpread(t *testing.T) {
	intT := BasicType("int", 8)
	sliceT := &Type{Kind: KSlice, Elem: intT}

	h := newFakeHost(t, 8)
	bumpFn := parseFunc(t, `package p
func BumpFirst(xs ...int) {
	xs[0] = 7
}`, true, []*Type{sliceT}, nil)
	bumpFn.Variadic = true
	h.funcs[bumpFn.Name] = bumpFn

	spreadFn := parseFunc(t, `package p
func LocalSpread() int {
	s := []int{1, 2}
	BumpFirst(s...)
	return s[0]
}`, true, nil, []*Type{intT})
	h.funcs[spreadFn.Name] = spreadFn

	cfg := DefaultConfig()
	res, _ := evalOne(t, h, cfg, parseExpr(t, `LocalSpread()`))
	if got := res.Int64(); got != 7 {
		t.Fatalf("LocalSpread() = %d, want 7", got)
	}
}
