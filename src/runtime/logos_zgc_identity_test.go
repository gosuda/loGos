// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime/debug"
	"testing"
	"unsafe"

	. "runtime"
)

type logosZgcKeyNode struct {
	val  int
	next *logosZgcKeyNode
}

// logosZgcMove relocates the real heap object src onto the real heap
// object dst of the same size class, as the collector would: copy, then
// publish under the identity lock. dst stands for the to-space slot.
func logosZgcMove(t *testing.T, pages map[uintptr]*LogosZgcPage, src, dst *logosZgcKeyNode) {
	t.Helper()
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	if got := LogosZgcRelocate(logosZgcPageOf(t, pages, a), a, unsafe.Sizeof(*src), d); got != d {
		t.Fatalf("relocation of %#x published %#x, want %#x", a, got, d)
	}
}

// TestLogosZgcIdentityKeysAcrossRelocation is the M19-S7 pointer-key
// attack through the runtime hashers that serve interface keys and
// reflect.MapOf maps (typehash, efaceeq): insert by the source address,
// relocate, look up by the copy's address, for object-start and interior
// keys. The maps still hold the source words, because the runtime reads
// them without a barrier.
func TestLogosZgcIdentityKeysAcrossRelocation(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer LogosZgcReset()
	LogosZgcIdentityKeysOn()
	defer LogosZgcIdentityKeysOff()
	LogosZgcSelectSoftware(LogosZgcRemapped)

	src, dst := &logosZgcKeyNode{val: 7}, new(logosZgcKeyNode)
	others := make([]*logosZgcKeyNode, 200)
	for i := range others {
		others[i] = &logosZgcKeyNode{val: i}
	}
	logosZgcSink = []any{src, dst, others}
	defer func() { logosZgcSink = nil }()

	// dst stands for an unused to-space slot: nothing hashes or keys it
	// before the move.
	sp, dp, op := src, dst, others[0]
	srcHash := LogosZgcTypeHash(&sp, 99)
	if LogosZgcTypeHash(&op, 99) == srcHash {
		t.Fatalf("distinct objects share hash %#x", srcHash)
	}

	byIface := map[any]int{src: 1, &src.val: 2}
	for i, o := range others {
		byIface[o] = 100 + i
	}

	pages := make(map[uintptr]*LogosZgcPage)
	logosZgcMove(t, pages, src, dst)
	if dst.val != 7 {
		t.Fatalf("copy holds %d, want 7", dst.val)
	}

	if got := LogosZgcTypeHash(&dp, 99); got != srcHash {
		t.Fatalf("hash of the copy %#x, want the source's %#x", got, srcHash)
	}
	if !LogosZgcEfaceEqual(any(src), any(dst)) || LogosZgcEfaceEqual(any(dst), any(others[0])) {
		t.Fatal("interface equality does not resolve the relocated source to its copy")
	}
	if v, ok := byIface[dst]; !ok || v != 1 {
		t.Fatalf("interface map lookup by the copy = %d, %v; want 1", v, ok)
	}
	if v, ok := byIface[&dst.val]; !ok || v != 2 {
		t.Fatalf("interface map lookup of the interior key by the copy = %d, %v; want 2", v, ok)
	}
	n := len(byIface)
	byIface[dst] = 3
	if len(byIface) != n || byIface[src] != 3 {
		t.Fatalf("assign through the copy added a key (%d -> %d) instead of updating it", n, len(byIface))
	}
	for i, o := range others {
		if byIface[o] != 100+i {
			t.Fatalf("unmoved key %d lost its entry", i)
		}
	}
}

// TestLogosZgcIdentityForgottenOnFree: the sweeper drops the identity of
// a freed object, so the table does not grow with dead keys and a later
// object at the reused address starts fresh.
func TestLogosZgcIdentityForgottenOnFree(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcIdentityKeysOn()
	defer LogosZgcIdentityKeysOff()
	LogosZgcSelectSoftware(LogosZgcRemapped)

	const n = 512
	addrs := make([]uintptr, n)
	func() {
		objs := make([]*logosZgcKeyNode, n)
		for i := range objs {
			objs[i] = new(logosZgcKeyNode)
			p := objs[i]
			LogosZgcTypeHash(&p, 1)
			addrs[i] = logosZgcAddrOf(p)
			if !LogosZgcHasIdentity(addrs[i]) {
				t.Fatalf("hashing object %d assigned no identity", i)
			}
		}
	}()
	GC()
	GC()
	kept := 0
	for _, a := range addrs {
		if LogosZgcHasIdentity(a) {
			kept++
		}
	}
	// Conservative stack scanning or a reallocation that hashed again may
	// keep a few; a missing sweep hook keeps all of them.
	if kept > n/8 {
		t.Fatalf("%d of %d freed objects kept their identity", kept, n)
	}
}

// TestLogosZgcKeyHashForeignAndInterior: words outside the heap never
// move and hash by address; interior words of one object differ by
// offset and agree with themselves.
func TestLogosZgcKeyHashForeignAndInterior(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcRemapped)
	before := LogosZgcIdentityLen()
	for _, w := range []uintptr{0, 0x7f12_3456_7890, 0x5555_0000_1000} {
		if LogosZgcKeyHash(w, 3) != LogosZgcKeyHash(w, 3) {
			t.Fatalf("foreign word %#x hashes unstably", w)
		}
	}
	if LogosZgcIdentityLen() != before {
		t.Fatal("hashing foreign words created identity entries")
	}
	o := new([4]uintptr)
	logosZgcSink = []any{o}
	defer func() { logosZgcSink = nil }()
	a := logosZgcAddrOf(o)
	if LogosZgcKeyHash(a, 3) == LogosZgcKeyHash(a+8, 3) || LogosZgcKeyHash(a+8, 3) != LogosZgcKeyHash(a+8, 3) {
		t.Fatal("interior words of one object do not hash by offset")
	}
	if !LogosZgcPtrEqual(a+8, a+8) || LogosZgcPtrEqual(a, a+8) {
		t.Fatal("pointer equality broken for unmoved words")
	}
}
