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

// logosZgcPageOf registers the zgc page holding addr, unless pages
// already holds it, and returns it.
func logosZgcPageOf(t *testing.T, pages map[uintptr]*LogosZgcPage, addr uintptr) *LogosZgcPage {
	t.Helper()
	start := addr &^ (LogosZgcPageSize - 1)
	if pg, ok := pages[start]; ok {
		return pg
	}
	pg := LogosZgcNewPage(start, 0, 16)
	pages[start] = pg
	t.Cleanup(func() { LogosZgcDropPage(pg) })
	return pg
}

var logosZgcSink []any

func logosZgcAddrOf[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

// TestLogosZgcHeapInAddressField: under the experiment the heap base is
// not randomized, so heap objects, small and large, have addresses that
// fit the 42-bit canonical address field of a colored pointer.
func TestLogosZgcHeapInAddressField(t *testing.T) {
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
func TestLogosZgcRemapInteriorPointer(t *testing.T) {
	pages := make(map[uintptr]*LogosZgcPage)
	objs := make([]*[6]uintptr, 32)
	for i := range objs {
		objs[i] = new([6]uintptr)
	}
	big, bigDst := make([]byte, 128<<10), make([]byte, 128<<10)
	src, neighbor, dst := objs[10], objs[11], new([6]uintptr)
	logosZgcSink = []any{objs, big, bigDst, dst}
	defer func() { logosZgcSink = nil }()
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	LogosZgcForwardInsert(logosZgcPageOf(t, pages, a), a, d)
	bigA, bigD := logosZgcAddrOf(&big[0]), logosZgcAddrOf(&bigDst[0])
	LogosZgcForwardInsert(logosZgcPageOf(t, pages, bigA), bigA, bigD)
	logosZgcPageOf(t, pages, logosZgcAddrOf(neighbor))

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
	if n := logosZgcAddrOf(neighbor); LogosZgcRemap(n+8) != n+8 {
		t.Errorf("an unmoved neighbor in the same span remapped to %#x", LogosZgcRemap(n+8))
	}
}

// TestLogosZgcSlowPathLeavesForeignWords: a word whose canonical address
// is on no zgc page is not a colored heap pointer. Its high bits are
// address bits (C and mmap memory sits above 4 TB, the PIE and brk range
// near 0x5555... reads as mark0|mark1|final in the software layout), so
// the slow path returns it unchanged, leaves its slot alone, and counts
// no heal.
func TestLogosZgcSlowPathLeavesForeignWords(t *testing.T) {
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()
	heap := new([4]uintptr)
	for _, w := range []uintptr{
		0,
		0x5555_0000_1000,
		0x7f12_3456_7890,
		logosZgcAddrOf(heap),
		logosZgcAddrOf(heap) + uintptr(LogosZgcSoftBits(LogosZgcMark0)),
	} {
		slot := new(uintptr)
		*slot = w
		before := LogosZgcHeals()
		if got := LogosZgcLoadSlow(slot, w); got != w || *slot != w || LogosZgcHeals() != before {
			t.Errorf("foreign word %#x: load %#x, slot %#x, heals %d; want it unchanged", w, got, *slot, LogosZgcHeals()-before)
		}
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
