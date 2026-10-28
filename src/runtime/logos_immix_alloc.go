// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"internal/runtime/sys"
)

// O1 substrate + O2 serving + O3 rebuild (M19-S5/S8).
//
// Eligibility, geometry, cached mode, and span init/free/reset come from
// O1; O2 adds live span serving (bump + verified hole reuse) at the fast
// allocation funnels with occupancy set on every served slot; O3 rebuilds
// occupancy from the post-swap composite at sweep and resets the cursor.
// O4 owns the barrier epoch flip and evac merge rule. No mode gate is
// flipped here.
//
// State model (slot-preserving, mspan object/type/bitmap/accounting kept):
//
//   - Eligible spans are exactly logosImmixBlockBytes (32 KiB) aligned,
//     logosPagesPerBlock pages (4 at the current 8 KiB page), one span per
//     block. Block base == span base for every active span; every other
//     span has logosImmixActive == false and logosImmixBlock == nil.
//   - Occupancy (plane O, block.occupy) is the freeindex OR allocBits
//     composite: a line is set iff some slot overlapping it satisfies
//     slot < freeindex or allocBits[slot] != 0. Sweep swaps
//     allocBits = gcmarkBits and resets freeindex = 0 (mgcsweep.go), so O3
//     rebuilds occupy from the post-swap composite; O1 clears it at init
//     and free under exclusive ownership.
//   - Dirty (plane D parity pair, block.dirty) is barrier-only. The active
//     table is dirty[logosImmixDirtyEpoch&1]; O4 flips the global once per
//     cycle at STW and clears the incoming table at termination STW while
//     the world is stopped. The barrier ORs only (atomic byte-OR), never
//     CAS-then-clear, so no mark is lost.
//   - Cursor state (logosImmixCursor/Limit/Next) is byte offsets from
//     base() (block == span, so no base stored) plus the next hole-hunt
//     line index. O1 zeroes it; O2 bumps within [cursor, limit) and hunts
//     holes via plane O; O3 resets it on sweep rebuild.
//
// Exclusions (honest compatibility, preserved by construction):
//
//   - sizeclass 0 (large): never enters mcentral small lists, never
//     line-allocated. logosSpanMovable already rejects it.
//   - sizeclass 1-2 (8/16 B, tiny): nextFreeFastTiny and the generated
//     tiny paths hardcode the 8192-byte span math; covering them needs
//     _mkmalloc regeneration, named as follow-up. Excluded here.
//   - GreenTea inline-mark classes (16..512 B with heapBitsInSpan) when
//     GOEXPERIMENT=greenteagc: the fixed 128 B spanInlineMarkBits tail at
//     the span end does not scale to 32 KiB spans (marks [63]uint8 = 504
//     bits < 512 slots at e=64). Excluded only when GreenTeaGC is on; the
//     default immix build (no greentea) covers all classes >= 3.
//   - Specials/Pinner/large: slots, findObject/objIndex, markBits,
//     allocBits, pinnerBits, specials, logosNoReloc/logosPinned, and the
//     block-overlap exclusion in evalBlock are untouched: slot stride is
//     unchanged, so every bitmap consumer keeps working.
//
// Hot-path discipline: every hook below early-returns on the
// goexperiment.LogosGCMoving const, and every call site guards with the
// same const, so ordinary builds eliminate the branch at compile time.
// Per-span fields live in mspan (always present, like the existing logos
// fields) but are touched only when the experiment is on; line tables live
// off-heap in block metadata, so ordinary spans pay no line-table cost.

// logosImmixOn caches logosImmixMode() once in mallocinit before mcaches
// exist. Hot paths test one byte instead of a string compare.
var logosImmixOn byte

// logosImmixSetEnabled caches the link-time mode. Called once from
// mallocinit (startup hook only); O1 never flips a mode gate.
//
//go:systemstack
func logosImmixSetEnabled() {
	if goexperiment.LogosGCMoving && logosImmixMode() {
		logosImmixOn = 1
	}
}

// logosImmixEnabled reports the cached immix mode.
//
//go:nosplit
func logosImmixEnabled() bool {
	return logosImmixOn != 0
}

// logosImmixSpanPages is the geometry of an immix span: exactly one block.
// Derived from logosPagesPerBlock (logos_evac_shared.go), not a second
// constant, so a page-size change cannot drift the two apart.
func logosImmixSpanPages() uintptr {
	return logosPagesPerBlock
}

// logosImmixSpanBytes is the byte size of an immix span: exactly one block.
func logosImmixSpanBytes() uintptr {
	return logosImmixBlockBytes
}

// logosImmixSpanClass reports whether spans of spc are immix-managed.
// Large (0), tiny (1-2), and GreenTea inline-mark classes stay classic.
//
//go:nosplit
func logosImmixSpanClass(spc spanClass) bool {
	if !goexperiment.LogosGCMoving {
		return false
	}
	if logosImmixOn == 0 {
		return false
	}
	sc := spc.sizeclass()
	if sc < 3 {
		return false
	}
	if int(sc) >= len(gc.SizeClassToSize) {
		return false
	}
	if goexperiment.GreenTeaGC {
		if gcUsesSpanInlineMarkBits(uintptr(gc.SizeClassToSize[sc])) {
			return false
		}
	}
	return true
}

// logosImmixNeedsAlignedSpan reports whether allocSpan must bypass the page
// cache and allocate a 32 KiB-aligned block span for spc/typ.
func logosImmixNeedsAlignedSpan(spc spanClass, typ spanAllocType) bool {
	if !goexperiment.LogosGCMoving {
		return false
	}
	if typ != spanAllocHeap {
		return false
	}
	return logosImmixSpanClass(spc)
}

// logosImmixCacheSpanBytes returns the sweep-credit bytes for a cacheSpan
// of spc: the block size for immix classes, the class span size otherwise.
// Keeps sweep pacing consistent when immix spans are 4 pages while the
// class table says 1.
func logosImmixCacheSpanBytes(spc spanClass) uintptr {
	if logosImmixSpanClass(spc) {
		return logosImmixBlockBytes
	}
	return uintptr(gc.SizeClassToNPages[spc.sizeclass()]) * pageSize
}

// logosImmixGrowNPages returns the npages mcentral.grow must request for
// spc: logosPagesPerBlock for immix classes, the class table otherwise.
func logosImmixGrowNPages(spc spanClass) uintptr {
	if logosImmixSpanClass(spc) {
		return logosPagesPerBlock
	}
	return uintptr(gc.SizeClassToNPages[spc.sizeclass()])
}

// logosImmixInitSpan attaches block metadata to a fresh heap span and
// resets its allocator state. Called from mheap.initSpan on the system
// stack while the span is still unpublished (no concurrent barrier or
// allocator possible). Non-immix spans (ineligible class, wrong size,
// misaligned base, non-heap type, metadata outside heap arenas) are left
// with Active == false and Block == nil; callers must check Active before
// touching cursor or planes.
//
//go:systemstack
func logosImmixInitSpan(s *mspan) {
	if !goexperiment.LogosGCMoving {
		return
	}
	if s.spanclass.sizeclass() == 0 || s.isUserArenaChunk {
		return
	}
	if !logosImmixSpanClass(s.spanclass) {
		return
	}
	if s.npages != logosPagesPerBlock {
		return
	}
	base := s.base()
	if base&(logosImmixBlockBytes-1) != 0 {
		return
	}
	b := logosImmixBlockFor(base)
	if b == nil {
		return
	}
	// Exclusive: span unpublished, no mutator can store into it, so no
	// barrier is in flight for this block range. Clearing both planes
	// cannot lose a mark.
	b.clearOccupy()
	b.clearDirtyAll()
	// Sentinel, never a live epoch, unless the block is already enlisted
	// this cycle: init must not look enlisted without a side-list push
	// (else first-cycle dirt skips enqueue and is never cleared), but it
	// must not break a live list link either (a duplicate push of the
	// same block to the same parity would corrupt the intrusive chain).
	// Init is exclusive (unpublished span: no barrier can name this
	// range), so plain access is sufficient.
	if b.epoch != logosImmixDirtyEpoch {
		b.epoch = ^uint32(0)
	}
	s.logosImmixBlock = b
	s.logosImmixActive = true
	s.logosImmixCursor = 0
	s.logosImmixLimit = 0
	s.logosImmixNext = 0
}

// logosImmixFreeSpan clears the block planes covered by a dying immix span.
// Called from mheap.freeSpanLocked with the heap lock held; the span is
// dead (allocCount == 0, state checked by the caller) so no allocator,
// sweeper, or barrier can be writing the block range. Block == span, so a
// whole-block clear is exact. Non-immix spans do nothing.
//
//go:systemstack
func logosImmixFreeSpan(s *mspan) {
	if !goexperiment.LogosGCMoving {
		return
	}
	if !s.logosImmixActive {
		return
	}
	b := s.logosImmixBlock
	if b == nil {
		return
	}
	b.clearOccupy()
	b.clearDirtyAll()
	// Retire the metadata with the span: a recycled block address must
	// not inherit the dead span's pin or liveness. Lookup rejects
	// non-live blocks, so clearing here makes the death transition the
	// struct comment promises; BlockFor retakes the slot (0->1->2 with
	// the epoch sentinel) when the address is reused.
	atomic.Store8(&b.blockPinned, 0)
	atomic.Store(&b.blockLive, 0)
	s.logosImmixBlock = nil
	s.logosImmixActive = false
	s.logosImmixCursor = 0
	s.logosImmixLimit = 0
	s.logosImmixNext = 0
}

// logosImmixResetCursor zeroes the bump/hole-hunt cursor. O2 calls it on
// refill after cacheSpan; O3 calls it on sweep rebuild. O1 zeroes at init;
// this helper keeps the reset shape in one place for O2/O3.
//
//go:nosplit
func logosImmixResetCursor(s *mspan) {
	s.logosImmixCursor = 0
	s.logosImmixLimit = 0
	s.logosImmixNext = 0
}

// logosImmixOccupySlot marks the lines overlapping the slot at addr in the
// span's block. Every served slot passes through here (bump, hole,
// nextFree fallback, freegc reuse, O4 STW claimSlot): plane O is the
// freeindex OR allocBits composite, so allocation only ever sets lines,
// never clears. Sweeps rebuild (O3) and span free clears. Safe for STW
// claimSlot use: no metadata creation, block cached on the span.
//
//go:nosplit
func logosImmixOccupySlot(s *mspan, addr uintptr) {
	if !goexperiment.LogosGCMoving {
		return
	}
	if !s.logosImmixActive {
		return
	}
	b := s.logosImmixBlock
	if b == nil {
		return
	}
	e := s.elemsize
	if e == 0 {
		return
	}
	b.markOccupyRange(addr, addr+e)
}

// logosImmixNextFreeFast serves one slot from an active immix span: an
// in-order bump replicating nextFreeFast bookkeeping plus occupancy, else
// a verified hole slot. Returns 0 when the span has no quickly servable
// slot; the caller falls through to the classic path (which may refill).
//
// Invariants (slot-preserving; bitmap authority):
//   - Bump serves exactly the slot the classic scan would serve
//     (freeindex + trailing-zero run over ~allocBits, with the same
//     64-boundary deferral), advances freeindex/allocCache/allocCount
//     identically, and additionally sets plane O and the byte cursor.
//   - Hole slots are occupancy-hint only: every candidate is verified
//     free against freeindex OR allocBits, and when a free slot precedes
//     the candidate this call returns 0 so the classic scan serves the
//     earlier slot — freeindex never advances past a free slot. Served
//     hole slots join allocBits (never freeindex) with allocCount++ and
//     a coherent allocCache bit clear, so the classic scan, countAlloc,
//     and the sweep composite all agree.
//   - Addresses are always base + slot*elemsize (slot stride), never raw
//     line-bump bytes: 24/40/80-byte classes would otherwise misalign.
//   - The object-area clamp (nelems*elemsize) keeps service out of the
//     span-end heap-bits carve.
func logosImmixNextFreeFast(s *mspan) gclinkptr {
	if !goexperiment.LogosGCMoving {
		return 0
	}
	if !s.logosImmixActive {
		return 0
	}
	b := s.logosImmixBlock
	if b == nil {
		return 0
	}
	e := s.elemsize
	if e == 0 {
		return 0
	}
	if s.allocCache != 0 {
		theBit := sys.TrailingZeros64(s.allocCache)
		result := s.freeindex + uint16(theBit)
		if result < s.nelems {
			freeidx := result + 1
			if !(freeidx%64 == 0 && freeidx != s.nelems) {
				s.allocCache >>= uint(theBit + 1)
				s.freeindex = freeidx
				s.allocCount++
				addr := s.base() + uintptr(result)*e
				b.markOccupyRange(addr, addr+e)
				s.logosImmixCursor = uint32(uintptr(freeidx) * e)
				return gclinkptr(addr)
			}
		}
	}
	if s.freeindex >= s.nelems {
		return 0
	}
	base := s.base()
	nelems := uintptr(s.nelems)
	objEnd := base + nelems*e
	holes := b.occupy.holes()
	for from := int(s.logosImmixNext); ; {
		start, end, ok := holes.next(from)
		if !ok {
			s.logosImmixNext = 0
			return 0
		}
		holeBase := base + uintptr(start)<<logosImmixLineShift
		holeEnd := base + uintptr(end)<<logosImmixLineShift
		if holeEnd > objEnd {
			holeEnd = objEnd
		}
		// Slot-stride candidates from the block base: holeBase is
		// line-aligned, not slot-aligned.
		a := holeBase
		if r := (a - base) % e; r != 0 {
			a += e - r
		}
		for ; a+e <= holeEnd; a += e {
			slot := (a - base) / e
			if slot >= nelems {
				break
			}
			if slot < uintptr(s.freeindex) {
				continue
			}
			if s.allocBitsForIndex(slot).isMarked() {
				continue
			}
			// Freeindex-invariant rule: a free slot in
			// [freeindex, slot) belongs to the classic scan.
			earlier := false
			for i := uintptr(s.freeindex); i < slot; i++ {
				if !s.allocBitsForIndex(i).isMarked() {
					earlier = true
					break
				}
			}
			if earlier {
				s.logosImmixNext = uint16(start)
				return 0
			}
			s.allocBitsForIndex(slot).setMarkedNonAtomic()
			s.allocCount++
			if d := slot - uintptr(s.freeindex); d < 64 {
				s.allocCache &^= uint64(1) << d
			}
			b.markOccupyRange(a, a+e)
			// Resume in the same hole: later slots of [start, end)
			// stay servable; advancing to end would strand them
			// until Next wraps, forcing premature refills.
			s.logosImmixNext = uint16(start)
			return gclinkptr(a)
		}
		from = end
	}
}

// logosImmixRebuildOccupancy rebuilds plane O from the post-swap freeindex
// OR allocBits composite and resets the bump/hole cursor. Called from
// sweepLocked.sweep after allocBits = gcmarkBits and freeindex = 0, under
// sweeper ownership (sweepgen CAS); active spans only. Fully freed spans
// skip this via the freeSpanLocked path, which clears the planes instead.
func logosImmixRebuildOccupancy(s *mspan) {
	if !goexperiment.LogosGCMoving {
		return
	}
	if !s.logosImmixActive {
		return
	}
	b := s.logosImmixBlock
	if b == nil {
		return
	}
	b.clearOccupy()
	base := s.base()
	e := s.elemsize
	nelems := uintptr(s.nelems)
	if e == 0 {
		logosImmixResetCursor(s)
		return
	}
	freeindex := uintptr(s.freeindex)
	for i := uintptr(0); i < nelems; i++ {
		if i < freeindex || s.allocBitsForIndex(i).isMarked() {
			a := base + i*e
			b.markOccupyRange(a, a+e)
		}
	}
	logosImmixResetCursor(s)
	s.logosImmixLimit = uint32(nelems * e)
}
