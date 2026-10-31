// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ctfe

// Host-ingress behavioral attacks (public seam only).
//
// The evaluator copies every container read from the host afresh: two
// reads of one const never alias, each distinct copied container costs
// one MaxSteps step, backing capacity (including the tail valueSize
// never counts) is pre-billed against MaxValueBytes, and the finished
// copy must still fit the live value cap together with the frames.
//
// All host graphs below are built through the public seam: scalar
// constructors are not needed because containers are bootstrapped with
// Eval of literals/make under a generous budget and then installed as
// host consts. Observations use only Eval results and the exported
// Value accessors (Int64) plus len/cap builtins evaluated the same way,
// never private storage.
//
// Limitation: a recursive host graph (a storage reaching itself) cannot
// be wired through any public constructor or literal evaluation, which
// only builds trees. No behavioral claim about cycles is made here;
// that shape needs private storage to construct, so it is omitted
// rather than echoed.

import (
	"strings"
	"testing"
)

// bootstrapHost evaluates src under a generous budget and installs the
// resulting value as host const name. It exercises only the public Eval
// seam; the caller never touches private storage.
func bootstrapHost(t *testing.T, h *fakeHost, name, src string) {
	t.Helper()
	v, _, err := Eval(h, Config{MaxSteps: 1 << 20, MaxValueBytes: 1 << 20, WordSize: 8}, parseExpr(t, src))
	if err != nil {
		t.Fatalf("bootstrap %s (%s): %v", name, src, err)
	}
	h.vars[name] = v
}

// TestHostIngressTwoReadsIsolated attacks per-read isolation: mutating
// one read of a host const must leave a second read in the same
// evaluation untouched, and must leave the host's stored value pristine
// for later reads.
func TestHostIngressTwoReadsIsolated(t *testing.T) {
	boot := newFakeHost(t, 8)
	big := Config{MaxSteps: 1 << 20, MaxValueBytes: 1 << 20, WordSize: 8}
	sliceHost, _, err := Eval(boot, big, parseExpr(t, `[]int{1, 2, 3}`))
	if err != nil {
		t.Fatalf("bootstrap slice: %v", err)
	}
	mapHost, _, err := Eval(boot, big, parseExpr(t, `map[string]int{"a": 1}`))
	if err != nil {
		t.Fatalf("bootstrap map: %v", err)
	}

	h := newFakeHost(t, 8)
	h.vars["C"] = sliceHost
	h.vars["M"] = mapHost
	intT := BasicType("int", 8)
	cfg := DefaultConfig()

	sliceFn := parseFunc(t, `package p
func TwoReadsSlice() int {
	a := C
	b := C
	a[0] = 99
	return b[0]
}`, true, nil, []*Type{intT})
	h.funcs[sliceFn.Name] = sliceFn
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `TwoReadsSlice()`)); v.Int64() != 1 {
		t.Fatalf("TwoReadsSlice() = %d, want 1 (second read isolated)", v.Int64())
	}

	mapFn := parseFunc(t, `package p
func TwoReadsMap() int {
	a := M
	b := M
	a["a"] = 99
	return b["a"]
}`, true, nil, []*Type{intT})
	h.funcs[mapFn.Name] = mapFn
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `TwoReadsMap()`)); v.Int64() != 1 {
		t.Fatalf("TwoReadsMap() = %d, want 1 (second read isolated)", v.Int64())
	}

	// Host storage stays pristine for later evaluations.
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `C[0]`)); v.Int64() != 1 {
		t.Fatalf("host C[0] after mutation = %d, want 1", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `M["a"]`)); v.Int64() != 1 {
		t.Fatalf(`host M["a"] after mutation = %d, want 1`, v.Int64())
	}
}

// TestHostIngressSingleReadAliasShares attacks the other side of the
// isolation rule: aliases derived from a single ingested read must
// observe each other's writes, matching Go reference semantics.
func TestHostIngressSingleReadAliasShares(t *testing.T) {
	h := newFakeHost(t, 8)
	bootstrapHost(t, h, "C", `[]int{1, 2, 3}`)
	bootstrapHost(t, h, "M", `map[string]int{"k": 1}`)
	bootstrapHost(t, h, "A", `[3]int{1, 2, 3}`)
	intT := BasicType("int", 8)
	cfg := DefaultConfig()

	sameSlice := parseFunc(t, `package p
func SameReadSlice() int {
	s := C
	u := s
	s[0] = 99
	return u[0]
}`, true, nil, []*Type{intT})
	h.funcs[sameSlice.Name] = sameSlice
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `SameReadSlice()`)); v.Int64() != 99 {
		t.Fatalf("SameReadSlice() = %d, want 99 (single read shares)", v.Int64())
	}

	reslice := parseFunc(t, `package p
func SameReadReslice() int {
	s := C
	u := s[1:3]
	s[1] = 77
	return u[0]
}`, true, nil, []*Type{intT})
	h.funcs[reslice.Name] = reslice
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `SameReadReslice()`)); v.Int64() != 77 {
		t.Fatalf("SameReadReslice() = %d, want 77 (reslice of one read shares)", v.Int64())
	}

	sameMap := parseFunc(t, `package p
func SameReadMap() int {
	m := M
	n := m
	m["k"] = 42
	return n["k"]
}`, true, nil, []*Type{intT})
	h.funcs[sameMap.Name] = sameMap
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `SameReadMap()`)); v.Int64() != 42 {
		t.Fatalf("SameReadMap() = %d, want 42 (single map read shares)", v.Int64())
	}

	// Two slices cut from one ingested array view the same copied
	// storage, so a write through one is visible through the other.
	arraySlices := parseFunc(t, `package p
func ArraySlicesShare() int {
	a := A
	s1 := a[0:2]
	s2 := a[1:3]
	s1[1] = 99
	return s2[0]
}`, true, nil, []*Type{intT})
	h.funcs[arraySlices.Name] = arraySlices
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `ArraySlicesShare()`)); v.Int64() != 99 {
		t.Fatalf("ArraySlicesShare() = %d, want 99 (array-derived slices share)", v.Int64())
	}
}

// TestHostIngressSharedBackingPreserved attacks host-internal sharing:
// two map entries built from one slice must still alias after ingress.
// The shared graph is built through the public seam by evaluating a
// const func, then installed as a host const; a single read must observe
// writes across the entries while the host stays pristine. Derived
// aliases above never exercise this path because each is created after
// ingress; only a shared host graph runs the copy's backing dedup.
func TestHostIngressSharedBackingPreserved(t *testing.T) {
	intT := BasicType("int", 8)
	mapT := &Type{Kind: KMap, Key: BasicType("string", 8), Elem: &Type{Kind: KSlice, Elem: intT}}
	cfg := DefaultConfig()

	build := newFakeHost(t, 8)
	share := parseFunc(t, `package p
func Share() map[string][]int {
	s := []int{1, 2}
	return map[string][]int{"a": s, "b": s}
}`, true, nil, []*Type{mapT})
	build.funcs[share.Name] = share
	shared, _, err := Eval(build, Config{MaxSteps: 1 << 20, MaxValueBytes: 1 << 20, WordSize: 8}, parseExpr(t, `Share()`))
	if err != nil {
		t.Fatalf("Share(): %v", err)
	}

	h := newFakeHost(t, 8)
	h.vars["H"] = shared
	check := parseFunc(t, `package p
func CheckShared() int {
	h := H
	h["a"][0] = 99
	return h["b"][0]
}`, true, nil, []*Type{intT})
	h.funcs[check.Name] = check
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `CheckShared()`)); v.Int64() != 99 {
		t.Fatalf("CheckShared() = %d, want 99 (host-internal backing shares)", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `H["a"][0]`)); v.Int64() != 1 {
		t.Fatalf("host H[\"a\"][0] = %d, want 1", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `H["b"][0]`)); v.Int64() != 1 {
		t.Fatalf("host H[\"b\"][0] = %d, want 1", v.Int64())
	}
}

// TestHostIngressCapTailCountsTowardBudget attacks the capacity tail:
// a host slice with empty visible length but spare capacity must still
// bill its whole backing against MaxValueBytes on ingress. A len-only
// accounting would let it through a tiny budget.
func TestHostIngressCapTailCountsTowardBudget(t *testing.T) {
	boot := newFakeHost(t, 8)
	big := Config{MaxSteps: 1 << 20, MaxValueBytes: 1 << 20, WordSize: 8}
	tail, _, err := Eval(boot, big, parseExpr(t, `make([]byte, 0, 8)`))
	if err != nil {
		t.Fatalf("bootstrap make: %v", err)
	}

	h := newFakeHost(t, 8)
	h.vars["C"] = tail

	// Sanity under a generous budget: the copy preserves length and
	// capacity through the public len/cap builtins.
	if v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, `len(C)`)); v.Int64() != 0 {
		t.Fatalf("len(C) = %d, want 0", v.Int64())
	}
	if v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, `cap(C)`)); v.Int64() != 8 {
		t.Fatalf("cap(C) = %d, want 8", v.Int64())
	}

	// Eight backing slots cannot fit a 7-byte budget even though the
	// visible length is zero.
	small := Config{MaxSteps: 100000, MaxValueBytes: 7, WordSize: 8}
	_, _, err = Eval(h, small, parseExpr(t, `C`))
	if err == nil {
		t.Fatalf("read of len-0 cap-8 host slice with 7-byte cap succeeded, want CodeTooLarge")
	}
	cerr, ok := err.(*Error)
	if !ok || cerr.Code != CodeTooLarge {
		t.Fatalf("want CodeTooLarge, got %v", err)
	}
	// Slots fire first and count the whole backing: with zero visible
	// bytes this must be the slot charge, not the byte charge.
	if !strings.Contains(cerr.Detail, "copy of 8 elements") {
		t.Fatalf("Detail = %q, want slot charge %q", cerr.Detail, "copy of 8 elements")
	}
}

// TestHostIngressLiveFrameCheckRetained attacks the post-copy live
// check: a host value that fits its own budget must still fail when
// the frames already hold enough live bytes that the finished copy
// overflows the cap.
func TestHostIngressLiveFrameCheckRetained(t *testing.T) {
	h := newFakeHost(t, 8)
	bootstrapHost(t, h, "C", `[20]byte{}`)
	intT := BasicType("int", 8)

	fn := parseFunc(t, `package p
func LiveCheck() int {
	s := make([]byte, 90)
	u := C
	return len(s) + len(u)
}`, true, nil, []*Type{intT})
	h.funcs[fn.Name] = fn

	// Each side fits 100 bytes alone (90 and 20); together they do not.
	tight := Config{MaxSteps: 100000, MaxValueBytes: 100, WordSize: 8}
	_, _, err := Eval(h, tight, parseExpr(t, `LiveCheck()`))
	if err == nil {
		t.Fatalf("LiveCheck with 100-byte cap succeeded, want CodeTooLarge from live frames + copy")
	}
	cerr, ok := err.(*Error)
	if !ok || cerr.Code != CodeTooLarge {
		t.Fatalf("want CodeTooLarge, got %v", err)
	}

	roomy := Config{MaxSteps: 100000, MaxValueBytes: 1 << 20, WordSize: 8}
	if v, _ := evalOne(t, h, roomy, parseExpr(t, `LiveCheck()`)); v.Int64() != 110 {
		t.Fatalf("LiveCheck() = %d, want 110", v.Int64())
	}
}

// TestHostIngressContainerStepBoundaries checks the per-container step
// charge at N-1/N/N+1. Two one-container host reads in one expression
// cost exactly the two ingress steps; the test measures the count with
// an unbounded run so it stays aligned with the step model, then probes
// the boundary. MaxSteps zero means the default, so the two-container
// shape is what lets N-1 stay a real (nonzero) limit.
func TestHostIngressContainerStepBoundaries(t *testing.T) {
	h := newFakeHost(t, 8)
	bootstrapHost(t, h, "C1", `[]int{7}`)
	bootstrapHost(t, h, "C2", `[]int{8}`)

	measure := func() int {
		hh := newFakeHost(t, 8)
		// Re-bootstrap per measurement so each Eval sees fresh hosts
		// under the same generous bytes; only MaxSteps varies below.
		bootstrapHost(t, hh, "C1", `[]int{7}`)
		bootstrapHost(t, hh, "C2", `[]int{8}`)
		_, st, err := Eval(hh, Config{MaxSteps: 1 << 30, MaxValueBytes: 1 << 20, WordSize: 8}, parseExpr(t, `C1[0] + C2[0]`))
		if err != nil {
			t.Fatalf("measure: %v", err)
		}
		if st.Steps <= 1 {
			t.Fatalf("measured steps = %d, want at least 2 for two containers", st.Steps)
		}
		return st.Steps
	}
	steps := measure()

	at := func(limit int) *Error {
		hh := newFakeHost(t, 8)
		bootstrapHost(t, hh, "C1", `[]int{7}`)
		bootstrapHost(t, hh, "C2", `[]int{8}`)
		_, _, err := Eval(hh, Config{MaxSteps: limit, MaxValueBytes: 1 << 20, WordSize: 8}, parseExpr(t, `C1[0] + C2[0]`))
		if err == nil {
			return nil
		}
		cerr, ok := err.(*Error)
		if !ok {
			t.Fatalf("want *Error, got %T", err)
		}
		return cerr
	}
	if cerr := at(steps - 1); cerr == nil || cerr.Code != CodeStepLimit {
		t.Fatalf("limit N-1 err = %v, want step limit", cerr)
	}
	if cerr := at(steps); cerr != nil {
		t.Fatalf("limit N err = %v, want success", cerr)
	}
	if cerr := at(steps + 1); cerr != nil {
		t.Fatalf("limit N+1 err = %v, want success", cerr)
	}

	measureSingle := func(src string) int {
		hh := newFakeHost(t, 8)
		bootstrapHost(t, hh, "W", src)
		_, st, err := Eval(hh, Config{MaxSteps: 1 << 30, MaxValueBytes: 1 << 20, WordSize: 8}, parseExpr(t, `W`))
		if err != nil {
			t.Fatalf("measure %s: %v", src, err)
		}
		return st.Steps
	}
	if got, want := measureSingle(`make([]int, 64)`), measureSingle(`[]int{7}`); got != want {
		t.Fatalf("wide steps = %d, narrow steps = %d, want equal", got, want)
	}

	// The summed value itself is observable through the public seam.
	v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, `C1[0] + C2[0]`))
	if v.Int64() != 15 {
		t.Fatalf("C1[0]+C2[0] = %d, want 15", v.Int64())
	}
}
