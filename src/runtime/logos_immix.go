// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/byteorder"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// The immix block and line layer (M19-S8, spec Sec 19.3) cuts the heap
// address space into 32 KiB blocks of 256 lines of 128 B. Each block has a
// 256-byte line header, one byte per line, kept off the Go heap so that
// marking lines never writes into the heap it describes.
const (
	logosImmixBlockShift = 15
	logosImmixBlockBytes = 1 << logosImmixBlockShift
	logosImmixLineShift  = 7
	logosImmixLineBytes  = 1 << logosImmixLineShift
	logosImmixLines      = logosImmixBlockBytes / logosImmixLineBytes

	logosImmixAllocAlign  = 16
	logosImmixMaskWords   = logosImmixLines / 64
	logosImmixArenaBlocks = heapArenaBytes / logosImmixBlockBytes
)

// logosLineMark is the 256-byte line header: one byte per line, 0 is a
// free line and nonzero is a live line.
type logosLineMark [logosImmixLines]uint8

// logosImmixBlock is the off-heap metadata of one 32 KiB-aligned block of
// the Go heap address space. A zero base marks metadata that was never
// created.
type logosImmixBlock struct {
	_           sys.NotInHeap
	base        uintptr
	lines       logosLineMark
	blockPinned uint8
}

// logosImmixArena is the metadata of every block of one heap arena. It is
// mapped zeroed on first use, so the pages of blocks that are never touched
// stay unbacked.
type logosImmixArena struct {
	_      sys.NotInHeap
	blocks [logosImmixArenaBlocks]logosImmixBlock
}

// logosImmixL2 is the second level of the arena metadata index. It mirrors
// the shape of mheap_.arenas.
type logosImmixL2 [1 << arenaL2Bits]unsafe.Pointer

// logosImmixIndex is the first level of the arena metadata index. Slots are
// published once with a compare-and-swap and never change afterwards.
var logosImmixIndex [1 << arenaL1Bits]unsafe.Pointer

// logosImmixInRange reports whether ri indexes logosImmixIndex and the
// second-level table it leads to.
//
//go:nosplit
func logosImmixInRange(ri arenaIdx) bool {
	return ri.l1() < uint(len(logosImmixIndex)) && ri.l2() < 1<<arenaL2Bits
}

// logosImmixInHeapArena reports whether the heap owns the arena of ri.
//
//go:nosplit
func logosImmixInHeapArena(ri arenaIdx) bool {
	if !logosImmixInRange(ri) {
		return false
	}
	l2 := mheap_.arenas[ri.l1()]
	return l2 != nil && l2[ri.l2()] != nil
}

// logosImmixPublish returns the zeroed mapping of size bytes that slot
// holds, creating it on first use. Racing creators agree on one mapping and
// the losers unmap theirs.
func logosImmixPublish(slot *unsafe.Pointer, size uintptr) unsafe.Pointer {
	if p := atomic.Loadp(unsafe.Pointer(slot)); p != nil {
		return p
	}
	p := sysAlloc(size, &memstats.gcMiscSys, "logos immix metadata")
	if p == nil {
		throw("runtime: cannot allocate immix block metadata")
	}
	if atomic.Casp1(slot, nil, p) {
		return p
	}
	sysFree(p, size, &memstats.gcMiscSys)
	return atomic.Loadp(unsafe.Pointer(slot))
}

// logosImmixArenaFor returns the block metadata of the arena of ri,
// creating it on first use. ri must satisfy logosImmixInRange.
func logosImmixArenaFor(ri arenaIdx) *logosImmixArena {
	l2 := (*logosImmixL2)(logosImmixPublish(&logosImmixIndex[ri.l1()], unsafe.Sizeof(logosImmixL2{})))
	return (*logosImmixArena)(logosImmixPublish(&l2[ri.l2()], unsafe.Sizeof(logosImmixArena{})))
}

// logosImmixArenaLookup returns the block metadata of the arena of ri, or
// nil when none was created.
//
//go:nosplit
func logosImmixArenaLookup(ri arenaIdx) *logosImmixArena {
	if !logosImmixInRange(ri) {
		return nil
	}
	l2 := (*logosImmixL2)(atomic.Loadp(unsafe.Pointer(&logosImmixIndex[ri.l1()])))
	if l2 == nil {
		return nil
	}
	return (*logosImmixArena)(atomic.Loadp(unsafe.Pointer(&l2[ri.l2()])))
}

// logosImmixBlockAt returns the metadata slot of the block holding addr.
//
//go:nosplit
func logosImmixBlockAt(a *logosImmixArena, addr uintptr) *logosImmixBlock {
	return &a.blocks[(addr>>logosImmixBlockShift)%logosImmixArenaBlocks]
}

// logosImmixBlockFor returns the metadata of the block containing addr,
// creating it with all lines free and the pin bit clear on first use. It
// returns nil when addr lies outside the heap arenas. It is safe for
// concurrent callers.
func logosImmixBlockFor(addr uintptr) *logosImmixBlock {
	ri := arenaIndex(addr)
	if !logosImmixInHeapArena(ri) {
		return nil
	}
	b := logosImmixBlockAt(logosImmixArenaFor(ri), addr)
	if atomic.Loaduintptr(&b.base) == 0 {
		atomic.Casuintptr(&b.base, 0, addr&^(logosImmixBlockBytes-1))
	}
	return b
}

// logosImmixStoreBarrier records the lines occupied by a pointer store before
// the store publishes its value. Metadata creation may allocate, so it runs on
// the system stack and only when the immix collector is selected.
//
//go:nowritebarrierrec
//go:nosplit
func logosImmixStoreBarrier(dst, size uintptr) {
	if !logosImmixMode() || size == 0 {
		return
	}
	systemstack(func() {
		logosImmixMarkStoreRange(dst, size)
	})
}

// logosImmixMarkStoreRange maps a destination byte range to its block lines.
// The caller must be on the system stack because first use can allocate metadata.
//
//go:systemstack
func logosImmixMarkStoreRange(dst, size uintptr) {
	for size != 0 {
		n := min(size, logosImmixBlockBytes-dst%logosImmixBlockBytes)
		if b := logosImmixBlockFor(dst); b != nil {
			b.markRange(dst, dst+n)
		}
		dst += n
		size -= n
	}
}

// logosImmixBlockLookup is like logosImmixBlockFor but returns nil instead
// of creating the metadata.
//
//go:nosplit
func logosImmixBlockLookup(addr uintptr) *logosImmixBlock {
	a := logosImmixArenaLookup(arenaIndex(addr))
	if a == nil {
		return nil
	}
	b := logosImmixBlockAt(a, addr)
	if atomic.Loaduintptr(&b.base) == 0 {
		return nil
	}
	return b
}

// logosImmixPinAddr sets the pin bit of the block containing addr, creating
// its metadata, and reports whether addr was inside the heap. A pinned block
// never evacuates.
func logosImmixPinAddr(addr uintptr) bool {
	b := logosImmixBlockFor(addr)
	if b == nil {
		return false
	}
	atomic.Store8(&b.blockPinned, 1)
	return true
}

// markRange sets the line byte of every line that [lo, hi) overlaps, after
// clipping the range to the block.
//
//go:nosplit
func (b *logosImmixBlock) markRange(lo, hi uintptr) {
	lo = max(lo, b.base)
	hi = min(hi, b.base+logosImmixBlockBytes)
	if lo >= hi {
		return
	}
	last := (hi - 1 - b.base) >> logosImmixLineShift
	for i := (lo - b.base) >> logosImmixLineShift; i <= last; i++ {
		b.lines[i] = 1
	}
}

// clearLines marks every line of the block free.
//
//go:nosplit
func (b *logosImmixBlock) clearLines() {
	b.lines = logosLineMark{}
}

// freeLines returns the number of free lines of the block.
//
//go:nosplit
func (b *logosImmixBlock) freeLines() int {
	h := b.lines.holes()
	n := 0
	for _, w := range h {
		n += sys.OnesCount64(w)
	}
	return n
}

// logosHoles is the free-line bit set of a line header: bit i%64 of word
// i/64 is set when line i is zero.
type logosHoles [logosImmixMaskWords]uint64

// logosFreeByteMask returns eight bits, bit k set when byte k of w is zero.
//
//go:nosplit
func logosFreeByteMask(w uint64) uint64 {
	const low7 = 0x7f7f7f7f7f7f7f7f
	live := ((w & low7) + low7) | w
	free := ^live & 0x8080808080808080
	return ((free >> 7) * 0x0102040810204080) >> 56
}

// logosHolesGo is the portable word-at-a-time scan of lm.
//
//go:nosplit
func logosHolesGo(lm *logosLineMark) (h logosHoles) {
	for w := range h {
		for k := range 8 {
			h[w] |= logosFreeByteMask(byteorder.LEUint64(lm[w*64+k*8:])) << (k * 8)
		}
	}
	return h
}

// holes scans the line header with AVX2 when the CPU has it and with the
// portable scan otherwise; both give identical results.
//
//go:nosplit
func (lm *logosLineMark) holes() logosHoles {
	if logosHoleScanUsable() {
		return logosHoleScanAVX2(lm)
	}
	return logosHolesGo(lm)
}

// logosFirstBit returns the index of the first line at or after from whose
// bit in h, xor invert, is set, and whether there is one; without one it
// returns logosImmixLines. invert is 0 to find a free line and ^0 to find
// a live line. from must be in [0, logosImmixLines).
//
//go:nosplit
func logosFirstBit(h *logosHoles, invert uint64, from int) (int, bool) {
	w := from >> 6
	v := (h[w] ^ invert) &^ (1<<uint(from&63) - 1)
	for {
		if v != 0 {
			return w<<6 + sys.TrailingZeros64(v), true
		}
		w++
		if w == logosImmixMaskWords {
			return logosImmixLines, false
		}
		v = h[w] ^ invert
	}
}

// next returns the first run [start, end) of free lines whose start is at or
// after from. A run that begins before from is reported from from. It
// returns ok=false when there is none, and for from outside [0, 256).
//
//go:nosplit
func (h *logosHoles) next(from int) (start, end int, ok bool) {
	if uint(from) >= logosImmixLines {
		return 0, 0, false
	}
	start, ok = logosFirstBit(h, 0, from)
	if !ok {
		return 0, 0, false
	}
	end, _ = logosFirstBit(h, ^uint64(0), start)
	return start, end, true
}

// findHole returns the first run [start, end) of zero line bytes with
// start >= from, where a run that begins before from is reported from from.
// It returns ok=false when there is none.
//
//go:nosplit
func (lm *logosLineMark) findHole(from int) (start, end int, ok bool) {
	h := lm.holes()
	return h.next(from)
}

// logosImmixAllocator is the bump allocator over the holes of one block.
// next is the line index where the next hole search starts.
type logosImmixAllocator struct {
	block         *logosImmixBlock
	cursor, limit uintptr
	next          int
}

// reset points the allocator at b with no hole selected.
//
//go:nosplit
func (a *logosImmixAllocator) reset(b *logosImmixBlock) {
	*a = logosImmixAllocator{block: b}
}

// alloc returns size bytes, rounded up to 16, from the current hole, or
// from the next hole that is large enough, and returns 0 when the block has
// no fitting hole. A size of 0 or over one block also returns 0.
//
//go:nosplit
func (a *logosImmixAllocator) alloc(size uintptr) uintptr {
	if size-1 >= logosImmixBlockBytes {
		return 0
	}
	size = (size + logosImmixAllocAlign - 1) &^ (logosImmixAllocAlign - 1)
	if p := a.cursor; p+size <= a.limit {
		a.cursor = p + size
		return p
	}
	return a.allocFromHole(size)
}

// allocFromHole abandons the current hole, takes the next hole of at least
// size bytes at or after a.next as the current hole, and allocates from it.
// It leaves the allocator unchanged when no hole fits.
//
//go:nosplit
func (a *logosImmixAllocator) allocFromHole(size uintptr) uintptr {
	holes := a.block.lines.holes()
	need := int((size + logosImmixLineBytes - 1) >> logosImmixLineShift)
	for from := a.next; ; {
		start, end, ok := holes.next(from)
		if !ok {
			return 0
		}
		if end-start >= need {
			base := a.block.base
			p := base + uintptr(start)<<logosImmixLineShift
			a.cursor = p + size
			a.limit = base + uintptr(end)<<logosImmixLineShift
			a.next = end
			return p
		}
		from = end
	}
}
