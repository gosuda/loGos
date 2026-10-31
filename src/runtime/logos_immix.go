// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/byteorder"
	"internal/goexperiment"
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
//
// O4 production (M19-S5/S8): occupancy and dirt are separate planes with
// explicit ownership. No allocation decision derives from lines; no marker
// writes occupy or dirty.
//
//   - lines (recompute view): evac selection input rebuilt each cycle by
//     logosBlockLines from authoritative mark bits. The barrier never
//     writes it; unit tests cover the line/hole layer through it.
//   - occupy (plane O): line set iff some slot of this block's span
//     overlapping the line is allocated under the freeindex OR allocBits
//     composite. Writers: span init (exclusive), allocation (mcache
//     exclusivity, O2), sweep rebuild (sweeper sweepgen CAS, O3), STW
//     claimSlot (O4 via allocator-owner helper). The barrier and the
//     marker never write it.
//   - dirty (plane D parity pair): production barrier input. Active table
//     is dirty[epoch&1]; barrier ORs atomically (linearizable byte-OR)
//     and enlists once per epoch; STW start flips epoch to a clean table,
//     STW termination (after selection) clears the incoming table for
//     exactly the previous cycle's dirtied blocks. Never merge post-flip.
//   - epoch: last enlisted global epoch (CAS dedup, at-most-once push).
//     Fresh slots start at ^uint32(0) sentinel so first touch enlists;
//     span init resets to sentinel under exclusive ownership.
//   - dirtyNext: intrusive per-parity stack links for the bounded
//     dirtied-block lists; drained only at STW (no concurrent push).
//   - blockPinned: sticky pin bit; a pinned block never evacuates. Cleared
//     when the block is evaluated unpinned and when its span dies, so a
//     recycled address never inherits a pin.
//   - blockLive: ownership generation state, 0 dead, 1 initializing, 2
//     live. Claimed per generation with a 0-to-1 CAS in BlockFor (creation
//     or retake); the winner resets the epoch, then publishes 2, and a
//     loser waits read-only for 2, so publication always converges.
//     Cleared to 0 when the span dies.
//     Lookup reports only state 2, so a barrier can never observe a
//     half-retaken slot, and recycled heap addresses read as
//     never-created; identity queries (PlanesAt) keep using base alone.
type logosImmixBlock struct {
	_           sys.NotInHeap
	base        uintptr
	lines       logosLineMark
	occupy      logosLineMark
	dirty       [2]logosLineMark
	epoch       uint32
	dirtyNext   [2]unsafe.Pointer
	blockPinned uint8
	_           [3]byte
	blockLive   uint32
}

// logosImmixDirtyEpoch is the global parity selector for the dirty pair.
// Starts at 1 so zeroed fresh slots (epoch 0) mismatch and enlist on first
// touch — no per-creation epoch store, hence no publish-vs-enqueue race.
// Flipped once per cycle at gcStart STW with the world stopped; the active
// table is dirty[epoch&1]. The barrier ORs only (atomic, lossless); no
// CAS-then-clear runs on the hot path, so no mark is lost. Termination STW,
// after selection has read the active table, clears the incoming table
// dirty[(epoch+1)&1] for exactly the blocks enlisted in that parity's
// dirtied-block list (bounded work, no whole-heap scan).
var logosImmixDirtyEpoch uint32 = 1

// logosImmixDirtyHead holds the intrusive per-parity dirtied-block stacks.
// Barrier (world running) pushes to head[epoch&1] via epoch-CAS dedup;
// termination STW drains head[(epoch+1)&1] while the world is stopped, so
// the drained list sees no concurrent push. Off-heap links, never scanned.
var logosImmixDirtyHead [2]unsafe.Pointer

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
//
// A slot whose span died (base kept, state dead) is retaken, not shared:
// base is published first, then exactly one claimant CAS-claims 0 dead to
// 1 initializing, resets the epoch to the unenlisted sentinel, and
// publishes 2 live. Lookup rejects the initializing state, so a barrier
// can neither observe a half-retaken slot nor enlist on a pre-reset epoch.
// No plane is touched here; freeSpan already cleared them, which keeps
// concurrent takeovers free of torn-plane races.
func logosImmixBlockFor(addr uintptr) *logosImmixBlock {
	ri := arenaIndex(addr)
	if !logosImmixInHeapArena(ri) {
		return nil
	}
	b := logosImmixBlockAt(logosImmixArenaFor(ri), addr)
	if atomic.Loaduintptr(&b.base) == 0 {
		atomic.Casuintptr(&b.base, 0, addr&^(logosImmixBlockBytes-1))
	}
	for {
		live := atomic.Load(&b.blockLive)
		if live == 2 {
			break
		}
		if live == 0 {
			if atomic.Cas(&b.blockLive, 0, 1) {
				atomic.Store(&b.epoch, ^uint32(0))
				atomic.Store(&b.blockLive, 2)
				return b
			}
			continue
		}
		procyield(30)
	}
	return b
}

// logosImmixStoreBarrier records the lines dirtied by a pointer store before
// the store publishes its value. Production O4 path: atomic OR into the
// active dirty parity table plus bounded enlist for STW clearing.
// Span initialization publishes metadata before mutator stores can occur.
// The barrier uses only existing metadata on the system stack.
//
//go:nowritebarrierrec
//go:nosplit
func logosImmixStoreBarrier(dst, size uintptr) {
	if !goexperiment.LogosGCMoving || logosImmixOn == 0 || size == 0 {
		return
	}
	systemstack(func() {
		logosImmixMarkStoreRange(dst, size)
	})
}

// logosImmixMarkStoreRange maps a destination byte range to its block dirty
// lines in the active parity table. The caller must be on the system stack.
// Metadata is initialized before span publication. Lossless: atomic byte-OR only;
// enlist is epoch-CAS dedup (at-most-once per block per cycle, idempotent
// clear), so a mark racing a STW clear cannot be lost — the clear runs with
// the world stopped, when no barrier is in flight (causal handoff).
//
//go:systemstack
func logosImmixMarkStoreRange(dst, size uintptr) {
	for size != 0 {
		n := min(size, logosImmixBlockBytes-dst%logosImmixBlockBytes)
		if b := logosImmixBlockLookup(dst); b != nil {
			b.markDirtyAtomicRange(dst, dst+n)
			logosImmixNoteDirty(b)
		}
		dst += n
		size -= n
	}
}

// logosImmixNoteDirty enlists b in the current epoch's dirtied-block stack,
// once per block per epoch. Idempotent: concurrent first-touches agree via
// epoch CAS; exactly one winner pushes. The push is a Treiber-stack CAS on
// the parity head; the pop runs only at STW with no concurrent push to the
// drained parity (partition-free STW gives linearizable drain).
func logosImmixNoteDirty(b *logosImmixBlock) {
	g := logosImmixDirtyEpoch
	for {
		old := atomic.Load(&b.epoch)
		if old == g {
			return
		}
		if atomic.Cas(&b.epoch, old, g) {
			idx := g & 1
			for {
				head := atomic.Loadp(unsafe.Pointer(&logosImmixDirtyHead[idx]))
				// Off-heap publication: the block lives in sysAlloc
				// metadata (sys.NotInHeap, never scanned), so no GC
				// edge needs a barrier. The plain pointer store
				// carries a write barrier and is forbidden under the
				// nowritebarrierrec StoreBarrier above; the NoWB
				// store preserves the exact Treiber-stack publish
				// with no barrier and no ordering change.
				atomic.StorepNoWB(unsafe.Pointer(&b.dirtyNext[idx]), head)
				if atomic.Casp1(&logosImmixDirtyHead[idx], head, unsafe.Pointer(b)) {
					return
				}
			}
		}
	}
}

// logosImmixFlipDirtyEpoch moves to a clean active table at cycle start.
// Call with the world stopped (gcStart STW, after stopTheWorldWithSema).
// The incoming table was cleared at the previous termination, so the flip
// is lossless. Plain increment is safe: mutators are parked (snapshot
// isolation of the epoch value for the coming mark window).
func logosImmixFlipDirtyEpoch() {
	if !goexperiment.LogosGCMoving || logosImmixOn == 0 {
		return
	}
	logosImmixDirtyEpoch++
}

// logosImmixClearIncomingDirty clears the incoming table for exactly the
// blocks dirtied in the previous cycle. Call with the world stopped after
// selection has read the active table (termination STW). Bounded: work is
// proportional to the dirtied-block count, never a whole-heap scan. Stale
// dirt can only over-pin (underselect), never free live lines.
func logosImmixClearIncomingDirty() {
	if !goexperiment.LogosGCMoving || logosImmixOn == 0 {
		return
	}
	idx := (logosImmixDirtyEpoch + 1) & 1
	head := atomic.Loadp(unsafe.Pointer(&logosImmixDirtyHead[idx]))
	logosImmixDirtyHead[idx] = nil
	for p := head; p != nil; {
		b := (*logosImmixBlock)(p)
		p = b.dirtyNext[idx&1]
		b.dirtyNext[idx&1] = nil
		b.clearDirtyTable(idx & 1)
	}
}

// logosImmixBlockLookup is like logosImmixBlockFor but returns nil instead
// of creating the metadata. It also returns nil for a block whose span
// died: base preserves identity for evacuated-address queries, but only
// live blocks answer Lookup and the barrier, so recycled heap addresses
// never inherit stale metadata.
//
//go:nosplit
func logosImmixBlockLookup(addr uintptr) *logosImmixBlock {
	b := logosImmixBlockIdentity(addr)
	if b == nil || atomic.Load(&b.blockLive) != 2 {
		return nil
	}
	return b
}

// logosImmixBlockIdentity returns the metadata slot of the block containing
// addr by identity alone, without creating it and without consulting
// liveness. Only evacuated-address observation (PlanesAt) may use it: the
// barrier, selection, and freshness checks must go through Lookup so a
// recycled address never inherits a dead span's tables or pin.
//
//go:nosplit
func logosImmixBlockIdentity(addr uintptr) *logosImmixBlock {
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

// markOccupyRange sets the occupy byte of every line that [lo, hi)
// overlaps, after clipping to the block. Caller must hold span ownership
// (init exclusive, mcache exclusivity, sweeper sweepgen CAS, or STW):
// plane O is never written concurrently.
//
//go:nosplit
func (b *logosImmixBlock) markOccupyRange(lo, hi uintptr) {
	lo = max(lo, b.base)
	hi = min(hi, b.base+logosImmixBlockBytes)
	if lo >= hi {
		return
	}
	last := (hi - 1 - b.base) >> logosImmixLineShift
	for i := (lo - b.base) >> logosImmixLineShift; i <= last; i++ {
		b.occupy[i] = 1
	}
}

// clearOccupyRange clears the occupy byte of every line that [lo, hi)
// overlaps, after clipping to the block. Caller must hold span ownership.
//
//go:nosplit
func (b *logosImmixBlock) clearOccupyRange(lo, hi uintptr) {
	lo = max(lo, b.base)
	hi = min(hi, b.base+logosImmixBlockBytes)
	if lo >= hi {
		return
	}
	last := (hi - 1 - b.base) >> logosImmixLineShift
	for i := (lo - b.base) >> logosImmixLineShift; i <= last; i++ {
		b.occupy[i] = 0
	}
}

// clearOccupy marks every occupy line of the block free. Call only with
// exclusive ownership (span init/free, sweep rebuild).
//
//go:nosplit
func (b *logosImmixBlock) clearOccupy() {
	b.occupy = logosLineMark{}
}

// occupyFreeLines returns the number of free occupy lines of the block.
// O2/O3 hole reuse derives from occupy, never from lines.
//
//go:nosplit
func (b *logosImmixBlock) occupyFreeLines() int {
	h := b.occupy.holes()
	n := 0
	for _, w := range h {
		n += sys.OnesCount64(w)
	}
	return n
}

// dirtyActive returns dirty[global&1] for mutator barrier use.
// O4 STW merge must use dirty[old&1] captured before flip, not this
// accessor post-flip.
//
//go:nosplit
func (b *logosImmixBlock) dirtyActive() *logosLineMark {
	return &b.dirty[logosImmixDirtyEpoch&1]
}

// markDirtyAtomicRange ORs 1 into the active dirty table for every line
// that [lo, hi) overlaps, after clipping to the block. Lossless: atomic
// byte-OR only, no CAS, no clear racing a set. For O4 barrier use; O1
// never calls it on the hot path.
//
//go:nosplit
func (b *logosImmixBlock) markDirtyAtomicRange(lo, hi uintptr) {
	lo = max(lo, b.base)
	hi = min(hi, b.base+logosImmixBlockBytes)
	if lo >= hi {
		return
	}
	active := &b.dirty[logosImmixDirtyEpoch&1]
	last := (hi - 1 - b.base) >> logosImmixLineShift
	for i := (lo - b.base) >> logosImmixLineShift; i <= last; i++ {
		atomic.Or8(&active[i], 1)
	}
}

// clearDirtyTable clears dirty[idx]. Call only at STW (O4 termination,
// clearing the incoming table) or under exclusive ownership (span
// init/free). Idx must be 0 or 1.
//
//go:nosplit
func (b *logosImmixBlock) clearDirtyTable(idx uint32) {
	b.dirty[idx&1] = logosLineMark{}
}

// clearDirtyAll clears both dirty tables. Call only under exclusive
// ownership (span init/free).
//
//go:nosplit
func (b *logosImmixBlock) clearDirtyAll() {
	b.dirty[0] = logosLineMark{}
	b.dirty[1] = logosLineMark{}
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
