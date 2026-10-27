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
	_    [1 << 20]byte
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
	fillers := make([][]byte, 0, 64)
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

// TestLogosZgcForeignHoldTransition verifies the bounded hold protocol:
// 1. A goroutine in _Grunning without a hold is not retained (proves the gap in pause-only scan).
// 2. Publishing an entry hold via LogosZgcForeignHoldEnter protects its roots and transitive closure.
// 3. Exiting the hold via LogosZgcForeignHoldExit unblocks release on subsequent pauses.
func TestLogosZgcForeignHoldTransition(t *testing.T) {
	LogosZgcForeignInit()
	LogosZgcForeignResetHolds()
	defer LogosZgcForeignResetHolds()

	a := new(logosZgcForeignHolder)
	fillers := make([][]byte, 0, 64)
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
		t.Skipf("cannot place D in a distinct granule")
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
	gpPtr := *(*unsafe.Pointer)(unsafe.Pointer(h))
	// Set status to _Grunning to model a mid-cycle or raw-syscall thread not in _Gsyscall.
	LogosZgcForeignGSetStatus(gpPtr, LogosZgcGRunning)

	mark := []uintptr{uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b))}
	live := []*LogosZgcPage{pgA, pgB, pgD}

	// Step 1: In _Grunning without an entry hold, transitive retention returns 0.
	LogosZgcForeignGSet(h, 0, uintptr(unsafe.Pointer(a)))
	LogosZgcForeignGSet(h, 1, dAddr)
	before := append([]*LogosZgcPage(nil), live...)
	got := LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr})
	if got != 0 {
		t.Fatalf("expected 0 retained without hold in _Grunning, got %d", got)
	}

	// Step 2: Publish hold at entry time.
	slot := LogosZgcForeignHoldEnter(gpPtr, uintptr(unsafe.Pointer(a)))
	if slot < 0 {
		t.Fatalf("failed to enter foreign hold: slot %d", slot)
	}
	if depth := LogosZgcForeignHoldDepth(gpPtr); depth != 1 {
		t.Fatalf("hold depth = %d, want 1", depth)
	}
	if count := LogosZgcForeignHoldCount(); count != 1 {
		t.Fatalf("hold count = %d, want 1", count)
	}

	// Step 3: Now transitive retention must retain both pgA and pgB through the hold.
	live = append([]*LogosZgcPage(nil), before...)
	got = LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr})
	if got != 2 {
		t.Fatalf("retained prefix with hold = %d, want 2 (A and B)", got)
	}
	if live[0] != pgA || live[1] != pgB || live[2] != pgD {
		t.Fatalf("live pages mismatch: want [pgA pgB pgD], got %v", live)
	}
	if !LogosZgcPageHasActiveHold(unsafe.Pointer(pgA)) {
		t.Fatalf("expected pgA to have active hold")
	}
	if !LogosZgcPageHasActiveHold(unsafe.Pointer(pgB)) {
		t.Fatalf("expected pgB to have active hold via transitive closure")
	}
	if LogosZgcPageHasActiveHold(unsafe.Pointer(pgD)) {
		t.Fatalf("pgD should not have active hold")
	}

	// Step 4: Return from foreign execution (exit hold).
	LogosZgcForeignHoldExit(gpPtr)
	if depth := LogosZgcForeignHoldDepth(gpPtr); depth != 0 {
		t.Fatalf("post-exit hold depth = %d, want 0", depth)
	}
	if count := LogosZgcForeignHoldCount(); count != 0 {
		t.Fatalf("post-exit hold count = %d, want 0", count)
	}

	// Step 5: Post-return retention returns 0, permitting unhindered release.
	live = append([]*LogosZgcPage(nil), before...)
	got = LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr})
	if got != 0 {
		t.Fatalf("expected 0 retained after hold exit, got %d", got)
	}

	KeepAlive(d)
	KeepAlive(a)
	KeepAlive(b)
	KeepAlive(fillers)
	KeepAlive(dFillers)
}

// TestLogosZgcForeignNestedReachability verifies multi-level transitive closure
// (A -> B -> C across distinct granules) and reentrant nested foreign hold calls.
func TestLogosZgcForeignNestedReachability(t *testing.T) {
	LogosZgcForeignInit()
	LogosZgcForeignResetHolds()
	defer LogosZgcForeignResetHolds()

	a := new(logosZgcForeignHolder)
	fillers := make([][]byte, 0, 64)
	var b *logosZgcForeignHolder
	for range cap(fillers) {
		candidate := new(logosZgcForeignHolder)
		if logosZgcForeignGran(a) != logosZgcForeignGran(candidate) {
			b = candidate
			break
		}
		fillers = append(fillers, make([]byte, 1<<20))
	}
	if b == nil {
		t.Fatal("could not place B in distinct granule from A")
	}

	var c *logosZgcForeignHolder
	for range cap(fillers) {
		candidate := new(logosZgcForeignHolder)
		if logosZgcForeignGran(candidate) != logosZgcForeignGran(a) && logosZgcForeignGran(candidate) != logosZgcForeignGran(b) {
			c = candidate
			break
		}
		fillers = append(fillers, make([]byte, 1<<20))
	}
	if c == nil {
		t.Fatal("could not place C in distinct granule from A and B")
	}

	var dFillers [][]byte
	var d *logosZgcForeignHolder
	var dAddr uintptr
	for range cap(fillers) {
		candidate := new(logosZgcForeignHolder)
		dAddr = uintptr(unsafe.Pointer(candidate))
		dGran := logosZgcForeignGranAddr(dAddr)
		if dGran != logosZgcForeignGran(a) && dGran != logosZgcForeignGran(b) && dGran != logosZgcForeignGran(c) {
			d = candidate
			break
		}
		dFillers = append(dFillers, make([]byte, 1<<20))
	}
	if d == nil {
		t.Skipf("cannot place D in a distinct granule")
	}

	a.next = b
	b.next = c
	c.next = nil
	GC()
	if uint64(uintptr(unsafe.Pointer(a))) > LogosZgcAddrMask ||
		uint64(uintptr(unsafe.Pointer(b))) > LogosZgcAddrMask ||
		uint64(uintptr(unsafe.Pointer(c))) > LogosZgcAddrMask ||
		uint64(dAddr) > LogosZgcAddrMask {
		t.Skip("heap object lies outside the ZGC address field")
	}

	a.next = b
	b.next = c
	c.next = nil

	pages := make(map[uintptr]*LogosZgcPage)
	pgA := logosZgcPageOf(t, pages, uintptr(unsafe.Pointer(a)))
	pgB := logosZgcPageOf(t, pages, uintptr(unsafe.Pointer(b)))
	pgC := logosZgcPageOf(t, pages, uintptr(unsafe.Pointer(c)))
	pgD := logosZgcPageOf(t, pages, dAddr)

	h := LogosZgcForeignNewG(8)
	gpPtr := *(*unsafe.Pointer)(unsafe.Pointer(h))
	LogosZgcForeignGSetStatus(gpPtr, LogosZgcGRunning)

	mark := []uintptr{uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(c))}
	live := []*LogosZgcPage{pgA, pgB, pgC, pgD}

	// 1. Enter outer foreign hold with root A.
	LogosZgcForeignHoldEnter(gpPtr, uintptr(unsafe.Pointer(a)))
	if d := LogosZgcForeignHoldDepth(gpPtr); d != 1 {
		t.Fatalf("outer hold depth = %d, want 1", d)
	}

	// Transitive closure should reach A -> B -> C across 3 distinct granules.
	got := LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr})
	if got != 3 {
		t.Fatalf("transitive reachability retained prefix = %d, want 3 (A, B, C)", got)
	}
	if live[0] != pgA || live[1] != pgB || live[2] != pgC || live[3] != pgD {
		t.Fatalf("pages order mismatch: %v", live)
	}

	// 2. Reentrant / nested foreign call (e.g. C calling back into Go, then calling C again).
	LogosZgcForeignHoldEnter(gpPtr, 0)
	if d := LogosZgcForeignHoldDepth(gpPtr); d != 2 {
		t.Fatalf("nested hold depth = %d, want 2", d)
	}

	// Inner return: depth drops to 1, but hold remains active.
	LogosZgcForeignHoldExit(gpPtr)
	if d := LogosZgcForeignHoldDepth(gpPtr); d != 1 {
		t.Fatalf("after inner return hold depth = %d, want 1", d)
	}
	live = []*LogosZgcPage{pgA, pgB, pgC, pgD}
	got = LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr})
	if got != 3 {
		t.Fatalf("retention while inner hold exited = %d, want 3", got)
	}

	// Outermost return: depth drops to 0, hold is fully released.
	LogosZgcForeignHoldExit(gpPtr)
	if d := LogosZgcForeignHoldDepth(gpPtr); d != 0 {
		t.Fatalf("after outermost return hold depth = %d, want 0", d)
	}
	live = []*LogosZgcPage{pgA, pgB, pgC, pgD}
	got = LogosZgcForeignRetain(live, h, mark, []uintptr{dAddr})
	if got != 0 {
		t.Fatalf("retention after outermost return = %d, want 0", got)
	}

	KeepAlive(d)
	KeepAlive(a)
	KeepAlive(b)
	KeepAlive(c)
	KeepAlive(fillers)
	KeepAlive(dFillers)
}
