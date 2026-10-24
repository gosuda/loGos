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

type logosZgcForeignHolder struct {
	next *logosZgcForeignHolder
}

func logosZgcForeignGran(p *logosZgcForeignHolder) uintptr {
	return uintptr(unsafe.Pointer(p)) &^ (LogosZgcPageSize - 1)
}

func logosZgcForeignGranAddr(addr uintptr) uintptr {
	return addr &^ (LogosZgcPageSize - 1)
}

// TestLogosZgcForeignRetainLive proves that the foreign-root closure follows
// a heap edge from a stack-seeded object, while an unmarked dead stack word
// retains nothing. The !foreign status branch is uncovered by construction:
// this fixture deliberately supplies only _Gsyscall.
func TestLogosZgcForeignRetainLive(t *testing.T) {
	LogosZgcForeignInit()

	a := new(logosZgcForeignHolder)
	fillers := make([][]byte, 0, 8)
	var b *logosZgcForeignHolder
	for range cap(fillers) {
		b = new(logosZgcForeignHolder)
		if logosZgcForeignGran(a) != logosZgcForeignGran(b) {
			break
		}
		fillers = append(fillers, make([]byte, 1<<20))
	}
	if b == nil || logosZgcForeignGran(a) == logosZgcForeignGran(b) {
		t.Fatal("could not place A and B in distinct granules")
	}

	// Place D before the quiescing collection. Mirror B's proven placement
	// pattern: each rejected candidate is followed by a retained 1 MiB filler.
	var dFillers [][]byte
	var d *logosZgcForeignHolder
	var dAddr uintptr
	placed := false
	for range cap(fillers) {
		candidate := new(logosZgcForeignHolder)
		dAddr = uintptr(unsafe.Pointer(candidate))
		dGran := logosZgcForeignGranAddr(dAddr)
		if dGran != logosZgcForeignGran(a) && dGran != logosZgcForeignGran(b) {
			d = candidate
			placed = true
			break
		}
		dFillers = append(dFillers, make([]byte, 1<<20))
	}
	if !placed {
		t.Skipf("cannot place D in a distinct granule: A=%#x B=%#x last D=%#x", logosZgcForeignGran(a), logosZgcForeignGran(b), dAddr)
	}
	a.next = b
	b.next = nil
	GC()
	if uint64(uintptr(unsafe.Pointer(a))) > LogosZgcAddrMask || uint64(uintptr(unsafe.Pointer(b))) > LogosZgcAddrMask || uint64(dAddr) > LogosZgcAddrMask {
		t.Skip("heap object lies outside the ZGC address field")
	}

	a.next = b
	b.next = nil
	pages := make(map[uintptr]*LogosZgcPage)
	pgA := logosZgcPageOf(t, pages, uintptr(unsafe.Pointer(a)))
	pgB := logosZgcPageOf(t, pages, uintptr(unsafe.Pointer(b)))
	pgD := logosZgcPageOf(t, pages, dAddr)

	h := LogosZgcForeignNewG(8)
	mark := []uintptr{uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b))}
	live := []*LogosZgcPage{pgA, pgB, pgD}
	check := func(got, want int, label string) {
		t.Helper()
		if got == -1 {
			t.Fatalf("%s: fixture address did not resolve", label)
		}
		if got == -2 {
			t.Fatalf("%s: D slot was recycled and became marked", label)
		}
		if got != want {
			t.Fatalf("%s: retained prefix length = %d, want %d", label, got, want)
		}
	}

	// P-empty: the closure's empty-live early return.
	pEmpty := LogosZgcForeignRetain(nil, h, mark, nil)
	if pEmpty == -1 {
		t.Skipf("P-empty fixture address did not resolve: D=%#x", dAddr)
	}
	check(pEmpty, 0, "P-empty")

	// P-silence: all zero stack words make seeded deterministically false;
	// allgs replacement prevents ambient _Gsyscall stacks from participating.
	for i := range 8 {
		LogosZgcForeignGSet(h, i, 0)
	}
	before := append([]*LogosZgcPage(nil), live...)
	check(LogosZgcForeignRetain(live, h, mark, nil), 0, "P-silence")
	if len(live) != len(before) {
		t.Fatalf("P-silence: live length changed from %d to %d", len(before), len(live))
	}
	for i := range live {
		if live[i] != before[i] {
			t.Fatalf("P-silence: live[%d] changed", i)
		}
	}

	// P-transitive: A is stack-reachable, B is reachable only through A, and
	// D is a registered but unmarked dead word.
	LogosZgcForeignGSet(h, 0, uintptr(unsafe.Pointer(a)))
	LogosZgcForeignGSet(h, 1, dAddr)
	live = []*LogosZgcPage{pgA, pgB, pgD}
	check(LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr}), 2, "P-transitive")
	if live[0] != pgA || live[1] != pgB || live[2] != pgD {
		t.Fatalf("P-transitive: live pages were not [A B D] after prefix closure")
	}

	// P-dead-only: D alone is unmarked and must not join the retained prefix.
	LogosZgcForeignGSet(h, 0, dAddr)
	LogosZgcForeignGSet(h, 1, 0)
	live = []*LogosZgcPage{pgA, pgB, pgD}
	check(LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr}), 0, "P-dead-only")
	if live[0] != pgA || live[1] != pgB || live[2] != pgD {
		t.Fatalf("P-dead-only: live pages changed")
	}

	// Keep the rooted objects and allocation fillers alive through the final
	// phase; D intentionally remains reachable only by dAddr.
	KeepAlive(d)
	KeepAlive(a)
	KeepAlive(b)
	KeepAlive(fillers)
	KeepAlive(dFillers)
}
