// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"testing"
	"unsafe"

	. "runtime"
)

type logosCgoSliceHeader struct {
	array unsafe.Pointer
	len   int
	cap   int
}

type logosCgoStringHeader struct {
	str unsafe.Pointer
	len int
}

func logosCgoCanonSetup(t *testing.T) {
	t.Helper()
	restore := LogosCgoCanonTestMode()
	t.Cleanup(func() {
		LogosCgoCanonDropSink()
		restore()
	})
}

// logosCgoCanonRequireHeap fails the fixture when v names a word the
// canonical helper may not strip, so a stack-escaped or foreign fixture
// cannot pass by measuring the foreign-word pass-through instead of
// canonicalization. It skips when v names heap the linked build placed
// outside the 42-bit color field (an immix-linked binary reserves high):
// flipping the collector mode in the setup cannot move that memory, so
// no such word can exercise canonicalization, and the skip reports the
// environment limit instead of failing it.
func logosCgoCanonRequireHeap(t *testing.T, what string, v uintptr) {
	t.Helper()
	if uint64(v) > LogosZgcAddrMask {
		t.Skipf("%s %#x names heap outside the 42-bit color field of this binary: the canonical helper must leave it alone, so run under -gc=zgc", what, v)
	}
	if !LogosCgoCanonStrippable(LogosCgoCanonBad(v)) {
		t.Fatalf("%s %#x is not heap-resident: the canonical helper must leave it alone", what, v)
	}
}

func TestLogosCgoCanonStructFields(t *testing.T) {
	logosCgoCanonSetup(t)
	type S struct {
		A *int
		B *int
		Pad int
	}
	a, b := LogosCgoCanonHeapInt(1), LogosCgoCanonHeapInt(2)
	pa, pb := uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b))
	logosCgoCanonRequireHeap(t, "struct field A", pa)
	logosCgoCanonRequireHeap(t, "struct field B", pb)
	v := S{A: a, B: b}
	*(*uintptr)(unsafe.Pointer(&v.A)) = LogosCgoCanonBad(pa)
	*(*uintptr)(unsafe.Pointer(&v.B)) = LogosCgoCanonBad(pb)
	LogosCgoCanonicalizeSlot(any(v), unsafe.Pointer(&v))
	if got := uintptr(unsafe.Pointer(v.A)); got != pa {
		t.Fatalf("struct field A = %#x, want %#x", got, pa)
	}
	if got := uintptr(unsafe.Pointer(v.B)); got != pb {
		t.Fatalf("struct field B = %#x, want %#x", got, pb)
	}
	if *v.A != 1 || *v.B != 2 {
		t.Fatalf("struct fields do not name their objects")
	}
}

func TestLogosCgoCanonSliceElements(t *testing.T) {
	logosCgoCanonSetup(t)
	s := LogosCgoCanonHeapSlice(2, 0)
	pa := uintptr(unsafe.Pointer(s[0]))
	pb := uintptr(unsafe.Pointer(s[1]))
	sh := (*logosCgoSliceHeader)(unsafe.Pointer(&s))
	plain := uintptr(sh.array)
	logosCgoCanonRequireHeap(t, "slice backing", plain)
	logosCgoCanonRequireHeap(t, "slice element 0", pa)
	logosCgoCanonRequireHeap(t, "slice element 1", pb)
	*(*uintptr)(unsafe.Pointer(plain)) = LogosCgoCanonBad(pa)
	*(*uintptr)(unsafe.Pointer(plain + unsafe.Sizeof(plain))) = LogosCgoCanonBad(pb)
	sh.array = unsafe.Pointer(LogosCgoCanonBad(plain))
	LogosCgoCanonicalizeSlot(any(s), unsafe.Pointer(&s))
	if got := uintptr(sh.array); got != plain {
		t.Fatalf("slice header = %#x, want %#x", got, plain)
	}
	got0 := *(*uintptr)(unsafe.Pointer(plain))
	got1 := *(*uintptr)(unsafe.Pointer(plain + unsafe.Sizeof(got0)))
	if got0 != pa || got1 != pb {
		t.Fatalf("slice elements = %#x,%#x want %#x,%#x", got0, got1, pa, pb)
	}
	if *s[0] != 1 || *s[1] != 2 {
		t.Fatalf("slice elements do not name their objects")
	}
}

func TestLogosCgoCanonSliceSpareCapacity(t *testing.T) {
	logosCgoCanonSetup(t)
	s := LogosCgoCanonHeapSlice(1, 1)
	full := s[:cap(s)]
	full[1] = LogosCgoCanonHeapInt(2)
	sh := (*logosCgoSliceHeader)(unsafe.Pointer(&s))
	plain := uintptr(sh.array)
	spare := uintptr(unsafe.Pointer(full[1]))
	logosCgoCanonRequireHeap(t, "spare element", spare)
	*(*uintptr)(unsafe.Pointer(plain + unsafe.Sizeof(plain))) = LogosCgoCanonBad(spare)
	LogosCgoCanonicalizeSlot(any(s), unsafe.Pointer(&s))
	if got := *(*uintptr)(unsafe.Pointer(plain + unsafe.Sizeof(plain))); got != spare {
		t.Fatalf("element in spare capacity = %#x, want %#x", got, spare)
	}
}

func TestLogosCgoCanonStringData(t *testing.T) {
	logosCgoCanonSetup(t)
	v := LogosCgoCanonHeapString(9)
	ss := (*logosCgoStringHeader)(unsafe.Pointer(&v))
	plain := uintptr(ss.str)
	logosCgoCanonRequireHeap(t, "string backing", plain)
	ss.str = unsafe.Pointer(LogosCgoCanonBad(plain))
	LogosCgoCanonicalizeSlot(any(v), unsafe.Pointer(&v))
	if got := uintptr(ss.str); got != plain {
		t.Fatalf("string data = %#x, want %#x", got, plain)
	}
	if len(v) != 9 || v[0] != 'a' {
		t.Fatalf("string data does not name its bytes")
	}
}

func TestLogosCgoCanonInterfaceData(t *testing.T) {
	logosCgoCanonSetup(t)
	type P struct{ X *int }
	type S struct{ W any }
	x := LogosCgoCanonHeapInt(7)
	inner := &P{X: x}
	LogosCgoCanonPin(inner)
	v := S{W: inner}
	px := uintptr(unsafe.Pointer(x))
	pin := uintptr(unsafe.Pointer(inner))
	logosCgoCanonRequireHeap(t, "interface data", pin)
	logosCgoCanonRequireHeap(t, "interface referent", px)
	*(*uintptr)(unsafe.Pointer(&inner.X)) = LogosCgoCanonBad(px)
	dp := (*[2]unsafe.Pointer)(unsafe.Pointer(&v.W))
	dp[1] = unsafe.Pointer(LogosCgoCanonBad(pin))
	LogosCgoCanonicalizeSlot(any(v), unsafe.Pointer(&v))
	if got := uintptr(dp[1]); got != pin {
		t.Fatalf("interface data word = %#x, want %#x", got, pin)
	}
	if got := uintptr(unsafe.Pointer(inner.X)); got != px {
		t.Fatalf("interface dynamic referent = %#x, want %#x", got, px)
	}
	if *inner.X != 7 {
		t.Fatalf("interface referent does not name its object")
	}
}

func TestLogosCgoCanonSliceInStruct(t *testing.T) {
	logosCgoCanonSetup(t)
	type S struct {
		N int
		E []*int
	}
	e := LogosCgoCanonHeapSlice(1, 0)
	pa := uintptr(unsafe.Pointer(e[0]))
	v := S{N: 3, E: e}
	sh := (*logosCgoSliceHeader)(unsafe.Pointer(&v.E))
	plain := uintptr(sh.array)
	logosCgoCanonRequireHeap(t, "nested backing", plain)
	logosCgoCanonRequireHeap(t, "nested element", pa)
	*(*uintptr)(unsafe.Pointer(plain)) = LogosCgoCanonBad(pa)
	sh.array = unsafe.Pointer(LogosCgoCanonBad(plain))
	LogosCgoCanonicalizeSlot(any(v), unsafe.Pointer(&v))
	if got := uintptr(sh.array); got != plain {
		t.Fatalf("slice-in-struct header = %#x, want %#x", got, plain)
	}
	if got := *(*uintptr)(unsafe.Pointer(plain)); got != pa {
		t.Fatalf("slice-in-struct element = %#x, want %#x", got, pa)
	}
	if *v.E[0] != 1 {
		t.Fatalf("slice-in-struct element does not name its object")
	}
}

func TestLogosCgoCanonTransitiveUnknown(t *testing.T) {
	logosCgoCanonSetup(t)
	type Inner struct{ P *int }
	type Outer struct{ In *Inner }
	x := LogosCgoCanonHeapInt(9)
	in := &Inner{P: x}
	out := &Outer{In: in}
	LogosCgoCanonPin(in)
	LogosCgoCanonPin(out)
	px := uintptr(unsafe.Pointer(x))
	pin := uintptr(unsafe.Pointer(in))
	logosCgoCanonRequireHeap(t, "outer link", pin)
	logosCgoCanonRequireHeap(t, "descendant", px)
	*(*uintptr)(unsafe.Pointer(&out.In)) = LogosCgoCanonBad(pin)
	*(*uintptr)(unsafe.Pointer(&in.P)) = LogosCgoCanonBad(px)
	LogosCgoCanonicalizeUnknown(unsafe.Pointer(out))
	if got := uintptr(unsafe.Pointer(out.In)); got != pin {
		t.Fatalf("outer link = %#x, want %#x", got, pin)
	}
	if got := uintptr(unsafe.Pointer(in.P)); got != px {
		t.Fatalf("transitive descendant = %#x, want %#x", got, px)
	}
	if *in.P != 9 {
		t.Fatalf("transitive descendant does not name its object")
	}
}

func TestLogosCgoCanonCyclicUnknown(t *testing.T) {
	logosCgoCanonSetup(t)
	type Node struct {
		Next *Node
		Val  *int
	}
	x := LogosCgoCanonHeapInt(4)
	a := &Node{Val: x}
	b := &Node{Val: x}
	a.Next, b.Next = b, a
	LogosCgoCanonPin(a)
	LogosCgoCanonPin(b)
	physical, pbn := uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b))
	logosCgoCanonRequireHeap(t, "cycle node", physical)
	*(*uintptr)(unsafe.Pointer(&a.Next)) = LogosCgoCanonBad(pbn)
	*(*uintptr)(unsafe.Pointer(&b.Next)) = LogosCgoCanonBad(physical)
	LogosCgoCanonicalizeUnknown(unsafe.Pointer(a))
	if got := uintptr(unsafe.Pointer(a.Next)); got != pbn {
		t.Fatalf("cycle forward link = %#x, want %#x", got, pbn)
	}
	if got := uintptr(unsafe.Pointer(b.Next)); got != physical {
		t.Fatalf("cycle back link = %#x, want %#x", got, physical)
	}
}

func TestLogosCgoCanonStaleSourceForwards(t *testing.T) {
	logosCgoCanonSetup(t)
	type S struct{ A *int }
	base, free := logosZgcPlace(t)
	defer free()
	const objSize = 16
	from := base + 0x40
	to := base + LogosZgcPageSize + 0x40
	pg := LogosZgcNewPage(base, LogosZgcPageSize, 4)
	defer LogosZgcDropPage(pg)
	LogosZgcStore(from, 0x1111)
	if got := LogosZgcRelocate(pg, from, objSize, to); got != to {
		t.Fatalf("relocation published %#x, want %#x", got, to)
	}
	if !LogosCgoCanonStrippable(LogosCgoCanonBad(from)) {
		t.Fatalf("source %#x is not strippable: the synthetic page is not registered", from)
	}
	v := S{A: (*int)(unsafe.Pointer(LogosCgoCanonBad(from)))}
	LogosCgoCanonicalizeSlot(any(v), unsafe.Pointer(&v))
	if got := uintptr(unsafe.Pointer(v.A)); got != to {
		t.Fatalf("stale source = %#x, want copy %#x", got, to)
	}
}

func TestLogosCgoCanonMapForbidden(t *testing.T) {
	// The map verdict belongs to the checker, which is mode- and
	// field-independent, so this test needs no zgc setup and stays
	// covered in every binary configuration.
	m := map[string]int{"k": 1}
	if !LogosCgoMapCheckPanics(any(m)) {
		t.Fatalf("map argument did not panic")
	}
}
