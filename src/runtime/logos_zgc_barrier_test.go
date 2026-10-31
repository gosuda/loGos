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

// logosZgcPageOf registers the zgc page holding addr, unless pages
// already holds it, and returns it. It is for a fixture that must put a
// page over live heap memory because the mechanism under test reads span
// extents from it. A fixture that only needs a relocation source and its
// copy uses newLogosZgcPage: the heap never owns that region, so neither
// a collector page nor an earlier test's can collide with it.
func logosZgcPageOf(t *testing.T, pages map[uintptr]*LogosZgcPage, addr uintptr) *LogosZgcPage {
	t.Helper()
	// A page over a heap granule needs the zgc low heap window; check
	// the mode before touching the directory so a wrong-mode run skips
	// instead of tripping the registration throw.
	logosZgcRequireHeapWindow(t)
	start := addr &^ (LogosZgcPageSize - 1)
	if pg, ok := pages[start]; ok {
		return pg
	}
	if pg := LogosZgcPageFor(addr); pg != nil {
		// An earlier test or a live cycle still owns this granule, so
		// reuse its page rather than registering a second one. Resolve
		// by the source address, not by the granule start: a page's
		// registered ranges can hold the start while the forwarded
		// source sits in a gap, and a start-resolved page would then be
		// handed a source outside its ranges.
		pages[start] = pg
		return pg
	}
	pg := LogosZgcNewPage(start, 0, 16)
	pages[start] = pg
	return pg
}

// newLogosZgcPage maps one region the heap never allocates from and
// registers a single page over it, so no granule the collector owns or
// can claim is involved: the directory slot and every byte of the region
// belong to this test alone. It returns the page and the region base,
// which is LogosZgcPageSize aligned and inside the 4 TB address field.
//
// The page owns the whole region as its one source range, so any address
// in it is a relocation source. Cleanup drops the page and then unmaps,
// in that order: the page must leave the directory before its memory
// goes away, and dropping it here also keeps the registry from listing a
// page over memory that is about to be released.
func newLogosZgcPage(t *testing.T) (pg *LogosZgcPage, base uintptr) {
	t.Helper()
	// Mode-independent mapped fixture: ensure registry capacity for the
	// registration below. An immix-linked binary never runs zgcInit, so
	// without this the first registration throws "page registry
	// overflow" and aborts the binary. Heap-address fixtures keep
	// skipping through logosZgcRequireHeapWindow instead.
	LogosZgcInit()
	base, raw := LogosZgcMapPagesRaw(1)
	// A fresh mapping lies past every earlier one, so its granule has no
	// page. Check it anyway: if that ever stopped holding, registering
	// would throw from inside the runtime and take the whole binary down,
	// and a failed test states the cause instead.
	if !LogosZgcPageSlotFree(base) {
		LogosZgcUnmapPages(raw, 1)
		t.Fatalf("mapped region %#x landed on a granule that already holds a page", base)
	}
	pg = LogosZgcNewPage(base, 0, 16)
	t.Cleanup(func() {
		LogosZgcDropPage(pg)
		LogosZgcUnmapPages(raw, 1)
	})
	return pg, base
}

// logosZgcSpanPairOnFreeGranule reports whether a and b are distinct heap
// objects in one in-use span, on a granule no page owns, so a single page
// registered over that granule covers both sources and remap can reach
// each one's object start through the span extents. Sharing a span is what
// makes the interior assertion mean what it says: the neighbor is "in the
// same span" only when the extents really bound both objects. Sharing the
// granule is just as necessary, because one page covers exactly one
// granule and a span straddling a boundary could not be covered by either.
func logosZgcSpanPairOnFreeGranule(a, b *[6]uintptr) bool {
	x, y := logosZgcAddrOf(a), logosZgcAddrOf(b)
	if x == y {
		return false
	}
	sx, okx := LogosZgcSpanStart(x)
	sy, oky := LogosZgcSpanStart(y)
	if !okx || !oky || sx != sy {
		return false
	}
	// The pair must also share the granule. This fixture allocates 48-byte
	// objects, whose spans are one runtime page, and a page never crosses a
	// 2 MB boundary; a larger span could, so the check is on the addresses
	// rather than on that property. One page cannot cover a pair that two
	// granules hold.
	if x&^(LogosZgcPageSize-1) != y&^(LogosZgcPageSize-1) {
		return false
	}
	return logosZgcGranuleRegistrable(x)
}

// logosZgcGranuleRegistrable reports whether the granule holding addr can
// take a page at all: its slot is free, and the whole 2 MB granule lies
// inside the 4 TB address field, since the header covers one granule and
// registration refuses a page that runs past the field. A heap granule at
// the very top of the window fails the second test.
func logosZgcGranuleRegistrable(addr uintptr) bool {
	granule := addr &^ (LogosZgcPageSize - 1)
	return uint64(granule) <= LogosZgcAddrMask-(LogosZgcPageSize-1) && LogosZgcPageSlotFree(granule)
}

var logosZgcSink []any

func logosZgcAddrOf[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

// TestLogosZgcHeapInAddressField: under the experiment the heap base is
// not randomized, so heap objects, small and large, have addresses that
// fit the 42-bit canonical address field of a colored pointer. The claim
// is about the heap itself, so these are real objects and no page is
// registered over either of them.
func TestLogosZgcHeapInAddressField(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	small, large := new([2]uintptr), make([]byte, 1<<20)
	logosZgcSink = []any{small, large}
	defer func() { logosZgcSink = nil }()
	for _, a := range []uintptr{logosZgcAddrOf(small), logosZgcAddrOf(&large[0])} {
		if uint64(a) > LogosZgcAddrMask {
			t.Errorf("heap address %#x is above the 4 TB address field", a)
		}
	}
}

// TestLogosZgcRemapInteriorPointer is the M19-S7 interior pointer attack:
// the forwarding table is keyed by object start, and a pointer into the
// middle of a relocated heap object remaps to the same offset in the copy
// through the extents of its span. A neighbor in the same span that did
// not move stays put.
//
// The sources stay on real heap memory because only a span tells remap
// which object start an interior address belongs to; mapped memory has no
// extents and would resolve only the exact addresses the table holds,
// which is a different claim. Their pages therefore sit over heap
// granules, and each is placed on a granule whose directory slot is free:
// a collector page already there cannot be registered over, and a
// narrowed one would not resolve for these sources anyway. The heap is
// held still meanwhile so no cycle can claim a granule in between. The
// copies are mapped, so the heap is never their home.
func TestLogosZgcRemapInteriorPointer(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	// Restoring the GC percent is a cleanup, not a defer: cleanups run
	// last-in-first-out, so registering it before any page means the
	// pages leave the directory while the heap is still held still, and a
	// cycle cannot claim a granule this test registered a page over.
	oldGCPercent := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(oldGCPercent) })
	LogosZgcDropTestPages()

	// Two same-size objects in one span, on a granule no page owns.
	//
	// 48-byte objects come from spans of one runtime page, so the first few
	// allocations all land on the granule the allocator is filling. If a
	// page already owns that granule every one of them is rejected, so the
	// search keeps allocating: consecutive same-class allocations exhaust
	// the current span and continue into the next page and, once 2 MB is
	// used up, into the next granule. Only consecutive allocations are
	// compared, because a span hands out its objects in address order, so
	// the first accepted pair is adjacent objects of one span. The cap is
	// far more than any grader needs (~6 MB of objects over three
	// granules); nothing is freed before the scan, so no candidate can move.
	var src, neighbor *[6]uintptr
	const maxPool = 128 << 10
	var pool []*[6]uintptr
	// &pool, not pool: the slice header is copied, so only a pointer to it
	// keeps the growing backing array reachable from the sink.
	logosZgcSink = []any{&pool}
	defer func() { logosZgcSink = nil }()
	for len(pool) < maxPool && src == nil {
		p := new([6]uintptr)
		pool = append(pool, p)
		if len(pool) >= 2 {
			if prev := pool[len(pool)-2]; logosZgcSpanPairOnFreeGranule(prev, p) {
				src, neighbor = prev, p
			}
		}
	}
	if src == nil {
		t.Fatal("no two heap objects share a span on a granule no page owns")
	}
	a := logosZgcAddrOf(src)
	granule := a &^ (LogosZgcPageSize - 1)

	// The 128 KB source needs a granule of its own: one page per granule,
	// and the whole object must stay inside it so every interior offset
	// below is still a source of that page. Its own span holds the single
	// object, so remap resolves all four offsets to this start. A rejected
	// candidate stays anchored, pushing the next allocation elsewhere.
	var big []byte
	bigA := uintptr(0)
	for range 64 {
		cand := make([]byte, 128<<10)
		logosZgcSink = append(logosZgcSink, cand)
		at := logosZgcAddrOf(&cand[0])
		if at&^(LogosZgcPageSize-1) == granule ||
			(at+128<<10-1)&^(LogosZgcPageSize-1) != at&^(LogosZgcPageSize-1) ||
			!logosZgcGranuleRegistrable(at) {
			continue
		}
		big, bigA = cand, at
		break
	}
	if big == nil {
		t.Fatal("no 128 KB object fits one granule no page owns")
	}

	// The copies live in memory the heap never allocates from.
	_, dstBase := newLogosZgcPage(t)
	d, bigD := dstBase+0x100, dstBase+0x80000

	pg := LogosZgcNewPage(granule, 0, 16)
	t.Cleanup(func() { LogosZgcDropPage(pg) })
	LogosZgcForwardInsert(pg, a, d)
	bigPG := LogosZgcNewPage(bigA&^(LogosZgcPageSize-1), 0, 16)
	t.Cleanup(func() { LogosZgcDropPage(bigPG) })
	LogosZgcForwardInsert(bigPG, bigA, bigD)

	// The neighbor has to sit on the relocating page for its assertion
	// to say anything: the page owns the granule, and an address on it
	// that was not forwarded comes back unchanged.
	n := logosZgcAddrOf(neighbor)
	if LogosZgcPageFor(n) != pg {
		t.Fatalf("neighbor %#x is on page %v, want the relocating source page", n, LogosZgcPageFor(n))
	}

	for _, off := range []uintptr{0, 8, 40} {
		if got := LogosZgcRemap(a + off); got != d+off {
			t.Errorf("remap(src+%d) = %#x, want %#x", off, got, d+off)
		}
	}
	for _, off := range []uintptr{0, 1, 70000, 128<<10 - 1} {
		if got := LogosZgcRemap(bigA + off); got != bigD+off {
			t.Errorf("remap(large+%d) = %#x, want %#x", off, got, bigD+off)
		}
	}
	if LogosZgcRemap(n+8) != n+8 {
		t.Errorf("an unmoved neighbor in the same span remapped to %#x", LogosZgcRemap(n+8))
	}
}

// TestLogosZgcMappedPageForwarding runs the directory, page and forwarding
// path with no heap granule involved at all: the source and the copy both
// live in regions the heap never allocates from, each with its own page.
// The forwarding table is keyed by object start, and memory with no heap
// span has no extents, so exactly the addresses the table holds remap: the
// source start maps to the copy, and any other address of the same region
// stays put instead of being guessed at. The neighbor assertion is the same
// one the span-based interior test makes, in the form this memory supports.
func TestLogosZgcMappedPageForwarding(t *testing.T) {
	srcPG, srcBase := newLogosZgcPage(t)
	dstPG, dstBase := newLogosZgcPage(t)

	// Both regions are the test's alone: no in-use heap span covers either
	// end, so the heap holds none of their bytes and no granule of theirs
	// can be claimed by a collector page.
	for _, base := range []uintptr{srcBase, dstBase} {
		for _, a := range []uintptr{base, base + LogosZgcPageSize - 1} {
			if _, live := LogosZgcSpanStart(a); live {
				t.Fatalf("mapped region address %#x lies in an in-use heap span", a)
			}
		}
	}
	if LogosZgcPageFor(srcBase) != srcPG || LogosZgcPageFor(dstBase) != dstPG {
		t.Fatalf("mapped regions resolve to %v and %v, want their own pages",
			LogosZgcPageFor(srcBase), LogosZgcPageFor(dstBase))
	}

	from, to := srcBase+0x40, dstBase+0x80
	if won := LogosZgcForwardInsert(srcPG, from, to); won != to {
		t.Fatalf("insert %#x -> %#x returned %#x", from, to, won)
	}
	if got := LogosZgcRemap(from); got != to {
		t.Errorf("remap of the forwarded start %#x = %#x, want %#x", from, got, to)
	}
	// Unforwarded addresses of the same region are not relocation sources
	// of any entry, and with no span to name their object start the remap
	// must return them unchanged rather than return a neighbour's copy.
	for _, off := range []uintptr{0, 8, 0x40 + 8, LogosZgcPageSize - 8} {
		if got := LogosZgcRemap(srcBase + off); got != srcBase+off {
			t.Errorf("remap of unforwarded source+%#x = %#x, want it unchanged", off, got)
		}
	}
}

// TestLogosZgcSlowPathLeavesForeignWords: a word with no heap span behind
// its canonical address is not a colored heap pointer. Its high bits are
// address bits (C and mmap memory sits above 4 TB, the PIE and brk range
// near 0x5555... reads as mark0|mark1|final in the software layout), so
// the slow path returns it unchanged, leaves its slot alone, and counts
// no heal. A stale-colored word into ordinary span heap (on no zgc page,
// so no forwarding entry can exist for it) instead strips to the canonical
// address: the flag must not escape to a dereference. The slot is still
// left alone with no heal counted.
func TestLogosZgcSlowPathLeavesForeignWords(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	// The premise is "ordinary heap with no page": a collection running
	// between the scan below and the probe could select a page over the
	// chosen object, which would give the slow path a forwarding table and
	// turn the expected no-heal into a heal. Hold the heap still while the
	// object is chosen and probed. The restore is a cleanup so it runs
	// after this test's own teardown.
	oldGCPercent := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(oldGCPercent) })
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()
	// The fixture registers no page, so no granule can collide here. What
	// it needs instead is an ordinary heap object that no page owns: the
	// stale row's premise is ordinary span heap with no forwarding entry
	// possible, and a page over the object would give the slow path a
	// table to consult. That is the common case, so the first object is
	// normally accepted and the loop stops. A page can own a whole granule,
	// and 32-byte objects come from spans of one runtime page, so an
	// occupied granule rejects a run of candidates; consecutive same-class
	// allocations keep going into later pages and, past 2 MB, into later
	// granules. Nothing is freed while the scan runs.
	var heap *[4]uintptr
	const maxPool = 128 << 10
	var pool []*[4]uintptr
	logosZgcSink = []any{&pool}
	defer func() { logosZgcSink = nil }()
	for len(pool) < maxPool && heap == nil {
		cand := new([4]uintptr)
		pool = append(pool, cand)
		addr := logosZgcAddrOf(cand)
		if _, ok := LogosZgcSpanStart(addr); ok && LogosZgcPageFor(addr) == nil {
			heap = cand
		}
	}
	if heap == nil {
		t.Fatal("no ordinary heap object lies on a page that could forward it")
	}
	heapAddr := logosZgcAddrOf(heap)
	for _, w := range []uintptr{
		0,
		0x5555_0000_1000,
		0x7f12_3456_7890,
		heapAddr,
	} {
		slot := new(uintptr)
		*slot = w
		before := LogosZgcHeals()
		if got := LogosZgcLoadSlow(slot, w); got != w || *slot != w || LogosZgcHeals() != before {
			t.Errorf("foreign word %#x: load %#x, slot %#x, heals %d; want it unchanged", w, got, *slot, LogosZgcHeals()-before)
		}
	}
	stale := heapAddr + uintptr(LogosZgcSoftBits(LogosZgcMark0))
	slot := new(uintptr)
	*slot = stale
	before := LogosZgcHeals()
	got := LogosZgcLoadSlow(slot, stale)
	if got != heapAddr || *slot != stale || LogosZgcHeals() != before {
		t.Errorf("ordinary-heap stale word %#x: load %#x, slot %#x, heals %d; want canonical %#x with slot alone and no heal",
			stale, got, *slot, LogosZgcHeals()-before, heapAddr)
	}
	KeepAlive(heap)
}

// TestLogosZgcUnmaskWord pins the word compiled barriers AND with: all
// ones before zgc is configured, so the barrier keeps every word, and the
// complement of the color bits once it is, so one AND strips the good
// color in either encoding.
func TestLogosZgcUnmaskWord(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcReset()
	if LogosZgcUnmaskBits() != ^uintptr(0) || LogosZgcColorMaskBits() != 0 {
		t.Fatalf("unconfigured: unmask %#x color mask %#x, want all ones and zero", LogosZgcUnmaskBits(), LogosZgcColorMaskBits())
	}
	for _, sel := range []func(uint8){LogosZgcSelectSoftware, LogosZgcSelectHardware} {
		sel(LogosZgcMark1)
		if u, m := LogosZgcUnmaskBits(), LogosZgcColorMaskBits(); u != ^m || LogosZgcColored(0x1234_5678, LogosZgcMark1)&u != 0x1234_5678 {
			t.Fatalf("unmask %#x, color mask %#x: the AND does not strip the good color", u, m)
		}
	}
}
