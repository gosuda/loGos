// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"unsafe"
)

// LogosImmixMovingExperiment reports the compile-time moving-GC experiment.
func LogosImmixMovingExperiment() bool { return goexperiment.LogosGCMoving }

// LogosImmixStoreBarrier exercises the production barrier path (active
// dirty parity + bounded enlist). Tests force logosImmixOn via
// LogosImmixSetOnForTest to simulate the immix build's cached mode.
func LogosImmixStoreBarrier(dst, size uintptr) {
	logosImmixStoreBarrier(dst, size)
}

// logosImmixSetOnForTest is the shared implementation for forcing the
// cached immix mode in barrier tests.
func logosImmixSetOnForTest(on byte) byte {
	prev := logosImmixOn
	logosImmixOn = on
	return prev
}

// LogosImmixForceOnForTest simulates the immix build's cached mode,
// returning the previous value for defer-restore.
func LogosImmixForceOnForTest() byte { return logosImmixSetOnForTest(1) }

// LogosImmixRestoreOnForTest restores a value saved by ForceOn.
func LogosImmixRestoreOnForTest(prev byte) { logosImmixSetOnForTest(prev) }

// LogosImmixDirtyEpoch reports the current parity selector.
func LogosImmixDirtyEpoch() uint32 { return logosImmixDirtyEpoch }

// Test seam for M19-S8: opaque wrappers over the immix block and line
// layer for package runtime_test, which cannot name unexported members.

const (
	LogosImmixBlockBytes = logosImmixBlockBytes
	LogosImmixLineBytes  = logosImmixLineBytes
	LogosImmixLines      = logosImmixLines
)

type (
	LogosLineMark       = logosLineMark
	LogosImmixBlock     = logosImmixBlock
	LogosImmixAllocator = logosImmixAllocator
)

var logosImmixTestRegions [][]byte

// LogosImmixNewBlocks returns the base of n consecutive 32 KiB blocks of
// fresh heap memory that no block metadata has been created for yet. The
// memory stays reachable for the life of the process.
func LogosImmixNewBlocks(n int) uintptr {
	buf := make([]byte, (n+1)*logosImmixBlockBytes)
	logosImmixTestRegions = append(logosImmixTestRegions, buf)
	return alignUp(uintptr(unsafe.Pointer(&buf[0])), logosImmixBlockBytes)
}

// LogosImmixNewBlock creates the metadata of a fresh heap block.
func LogosImmixNewBlock() *LogosImmixBlock {
	return logosImmixBlockFor(LogosImmixNewBlocks(1))
}

func LogosImmixBlockFor(addr uintptr) *LogosImmixBlock { return logosImmixBlockFor(addr) }

func LogosImmixBlockLookup(addr uintptr) *LogosImmixBlock { return logosImmixBlockLookup(addr) }

func LogosImmixPinAddr(addr uintptr) bool { return logosImmixPinAddr(addr) }

// LogosImmixInHeap reports whether addr lies in a span known to the heap.
func LogosImmixInHeap(addr uintptr) bool { return spanOf(addr) != nil }

// LogosImmixHasAVX2 reports whether findHole takes the AVX2 path.
func LogosImmixHasAVX2() bool { return logosHoleScanUsable() }

func (b *logosImmixBlock) Base() uintptr { return b.base }

func (b *logosImmixBlock) Addr() uintptr { return uintptr(unsafe.Pointer(b)) }

func (b *logosImmixBlock) Pinned() bool { return b.blockPinned != 0 }

func (b *logosImmixBlock) Lines() *LogosLineMark { return &b.lines }

func (b *logosImmixBlock) MarkRange(lo, hi uintptr) { b.markRange(lo, hi) }

func (b *logosImmixBlock) ClearLines() { b.clearLines() }

func (b *logosImmixBlock) FreeLines() int { return b.freeLines() }

func (b *logosImmixBlock) DirtyActive() *LogosLineMark { return b.dirtyActive() }

func (b *logosImmixBlock) ClearDirtyAll() { b.clearDirtyAll() }

func (b *logosImmixBlock) MarkDirtyAtomic(lo, hi uintptr) { b.markDirtyAtomicRange(lo, hi) }

func (lm *logosLineMark) FindHole(from int) (start, end int, ok bool) { return lm.findHole(from) }

// FindHoleGo runs findHole through the portable Go scan.
func (lm *logosLineMark) FindHoleGo(from int) (start, end int, ok bool) {
	h := logosHolesGo(lm)
	return h.next(from)
}

// FindHoleAVX2 runs findHole through the AVX2 scan; callers must check
// LogosImmixHasAVX2 first.
func (lm *logosLineMark) FindHoleAVX2(from int) (start, end int, ok bool) {
	h := logosHoleScanAVX2(lm)
	return h.next(from)
}

func (a *logosImmixAllocator) Reset(b *LogosImmixBlock) { a.reset(b) }

func (a *logosImmixAllocator) Alloc(size uintptr) uintptr { return a.alloc(size) }

func (a *logosImmixAllocator) Cursor() uintptr { return a.cursor }

func (a *logosImmixAllocator) Limit() uintptr { return a.limit }

func (a *logosImmixAllocator) Next() int { return a.next }

// Read-only production inspectors (M19-S5/S8). They copy real allocator,
// span and plane state for package runtime_test; none of them writes a
// flag, bit, plane or cursor, and none creates metadata, so a test cannot
// seed the state it then observes.

// LogosImmixPagesPerBlock is the page count of one immix block.
const LogosImmixPagesPerBlock = logosPagesPerBlock

// LogosImmixOn reports the cached link-time immix mode that the allocation
// hooks and the store barrier test. It is the mode the linker chose, not a
// forced one.
func LogosImmixOn() bool { return logosImmixOn != 0 }

// LogosGCMarkActive reports whether the write barrier is on, which is the
// mark and mark-termination window of a cycle.
func LogosGCMarkActive() bool { return writeBarrier.enabled }

// LogosGCCycles reports the number of cycles started so far.
func LogosGCCycles() uint32 { return work.cycles.Load() }

// LogosImmixSpan is a copy of the production state of the in-use heap span
// that holds an address.
type LogosImmixSpan struct {
	Base, NPages, ElemSize uintptr
	// Limit is the end of the object area of the span (mspan.limit).
	Limit uintptr
	// HeapBitsLo and HeapBitsHi bound the inline heap-bits carve at the end
	// of a pointer-bearing span of small objects, and are zero otherwise.
	HeapBitsLo, HeapBitsHi uintptr
	// BlockBase is the base the span's block metadata records; it is zero
	// when the span has no block.
	BlockBase                     uintptr
	NElems, FreeIndex, AllocCount uint16
	// Next is the hole-hunt line cursor of the immix allocator.
	Next uint16
	// Cursor and CursorLimit are the immix bump cursor and limit, byte
	// offsets from Base.
	Cursor, CursorLimit uint32
	SizeClass           int8
	NoScan              bool
	// Active is the span's immix flag; HasBlock is whether it points at
	// block metadata; BlockRegistered is whether that metadata is the one
	// the arena index holds for Base.
	Active, HasBlock, BlockRegistered bool
	Specials, NoReloc, Pinned         bool
	// Cached is whether the span is swept and held by an mcache.
	Cached bool
}

// LogosImmixSpanOf copies the state of the in-use span that holds p. The
// caller keeps p reachable, so the span cannot be freed under the read.
func LogosImmixSpanOf(p unsafe.Pointer) (st LogosImmixSpan, ok bool) {
	s := spanOf(uintptr(p))
	if s == nil || s.state.get() != mSpanInUse {
		return st, false
	}
	st.Base, st.NPages, st.ElemSize, st.Limit = s.base(), s.npages, s.elemsize, s.limit
	st.NElems, st.FreeIndex, st.AllocCount = s.nelems, s.freeindex, s.allocCount
	st.Next, st.Cursor, st.CursorLimit = s.logosImmixNext, s.logosImmixCursor, s.logosImmixLimit
	st.SizeClass = int8(s.spanclass.sizeclass())
	st.NoScan = s.spanclass.noscan()
	st.Active, st.HasBlock = s.logosImmixActive, s.logosImmixBlock != nil
	if b := s.logosImmixBlock; b != nil {
		st.BlockBase = b.base
		st.BlockRegistered = b == logosImmixBlockLookup(s.base())
	}
	st.Specials, st.NoReloc = s.specials != nil, s.logosNoReloc
	st.Pinned = s.logosPinned.Load()
	if pb := (*gcBits)(unsafe.Pointer(s.getPinnerBits())); pb != nil {
		for i := uintptr(0); i < divRoundUp(uintptr(s.nelems)*2, 8); i++ {
			if *pb.bytep(i) != 0 {
				st.Pinned = true
				break
			}
		}
	}
	st.Cached = s.sweepgen == mheap_.sweepgen+3
	if sc := s.spanclass; sc.sizeclass() != 0 && !sc.noscan() && heapBitsInSpan(s.elemsize) {
		lo, size := spanHeapBitsRange(s.base(), s.npages*pageSize, s.elemsize)
		st.HeapBitsLo, st.HeapBitsHi = lo, lo+size
	}
	return st, true
}

// LogosImmixPlanes copies the occupancy plane, both dirty parity tables,
// the enlisted epoch and the pin bit of the block that holds p. ok is false
// when no block metadata exists for p.
func LogosImmixPlanes(p unsafe.Pointer) (occupy LogosLineMark, dirty [2]LogosLineMark, epoch uint32, pinned, ok bool) {
	return LogosImmixPlanesAt(uintptr(p))
}

// LogosImmixPlanesAt is LogosImmixPlanes for a raw address. A test that
// observes the planes while a mark window is open uses it, so that it holds
// no pointer to the objects it is about to leave unreachable.
func LogosImmixPlanesAt(addr uintptr) (occupy LogosLineMark, dirty [2]LogosLineMark, epoch uint32, pinned, ok bool) {
	b := logosImmixBlockLookup(addr)
	if b == nil {
		return occupy, dirty, 0, false, false
	}
	return b.occupy, b.dirty, b.epoch, b.blockPinned != 0, true
}
