// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/goarch"
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"unsafe"
)

// The evacuation driver for immix (M19-S5 phase 1). The logosEvac state it
// runs on is declared once, untagged, in logos_evac.go; the pin pass, the
// rewrite walk, and the off-heap containers are shared with the other moving
// collectors. Every name below is immix-only.

// logosEv is the single evacuation state, reused from cycle to cycle and
// cleared before each run. Only the immix driver drives it today; the zgc
// collector uses local pin-only states (zgcNewPinEv).
var logosEv logosEvac

// logosEvScratch holds the line marks of the block under evaluation. It is a
// package-level value rather than a logosEvac field because a logosImmixBlock
// embeds sys.NotInHeap, and a not-in-heap field would make the shared state
// struct itself not-in-heap. Only the stopped pass reads and writes it.
var logosEvScratch logosImmixBlock

func (ev *logosEvac) candidate(i uintptr) *mspan {
	return (*mspan)(unsafe.Pointer(ev.cand.get(i)))
}

// logosEvacuate runs one evacuation. The world must be stopped and marking
// complete.
func logosEvacuate() {
	assertWorldStopped()
	if !goexperiment.LogosGCMoving {
		return
	}
	ev := &logosEv
	memclrNoHeapPointers(unsafe.Pointer(ev), unsafe.Sizeof(*ev))
	// The pin pass is shared with the other moving collectors; an immix
	// exclusion granule is one block, hashed by the block shift.
	ev.granule = logosImmixBlockBytes
	ev.pinned.shift = logosImmixBlockShift
	if ev.collectPins() {
		ev.run()
	}
	ev.finish()
}

// logosMovingMarkTermination runs the stop-the-world evacuation when the
// immix moving collector is selected. The world is stopped and marking is
// complete. Other moving collectors and the plain collector provide their own
// definition under the complementary build tag.
func logosMovingMarkTermination() {
	if logosImmixMode() {
		logosEvacuate()
	}
}

// run selects the candidate blocks, moves their objects, and rewrites every
// reference to a moved object.
func (ev *logosEvac) run() {
	ev.selectBlocks()
	if ev.cand.n == 0 {
		return
	}
	ev.assignForwarding()
	ev.buildPool()
	for i := uintptr(0); i < ev.cand.n; i++ {
		ev.evacuateSpan(ev.candidate(i))
	}
	if ev.moved == 0 {
		return
	}
	logosWalkAll(logosFixSlot)
	if debug.gccheckmark > 0 {
		logosWalkAll(logosVerifySlot)
	}
}

// finish releases the evacuation's off-heap memory and publishes its totals.
func (ev *logosEvac) finish() {
	for i := uintptr(0); i < ev.cand.n; i++ {
		ev.candidate(i).logosFwd = 0
	}
	if ev.fwd != 0 {
		sysFree(unsafe.Pointer(ev.fwd), ev.fwdBytes, &memstats.other_sys)
	}
	if debug.gctrace > 0 {
		print("logos evac: blocks=", ev.seen, " pinned=", ev.pinnedBlks, " fixed=", ev.fixedBlks, " dense=", ev.denseBlks, " candidates=", ev.blocks, " pins=", ev.pinned.count, " moved=", ev.moved, "\n")
	}
	ev.pinned.release()
	ev.cand.release()
	ev.pool.release()
	logosEvacTotals.cycles++
	logosEvacTotals.blocks += uint64(ev.blocks)
	logosEvacTotals.objects += uint64(ev.moved)
	logosEvacTotals.bytes += uint64(ev.movedBytes)
}

// selectBlocks marks every evacuable, fragmented block as a candidate.
func (ev *logosEvac) selectBlocks() {
	for _, ai := range mheap_.heapArenas {
		ha := mheap_.arenas[ai.l1()][ai.l2()]
		base := arenaBase(ai)
		for off := uintptr(0); off < heapArenaBytes; off += logosImmixBlockBytes {
			ev.evalBlock(ha, base+off)
		}
	}
}

// logosBlockSpans collects the in-use spans that overlap the block at bb.
func logosBlockSpans(ha *heapArena, bb uintptr, out *[logosPagesPerBlock]*mspan) int {
	n := 0
	for pg := uintptr(0); pg < logosPagesPerBlock; pg++ {
		addr := bb + pg*pageSize
		s := ha.spans[(addr/pageSize)%pagesPerArena]
		if s == nil || s.state.get() != mSpanInUse || addr < s.base() || addr >= s.base()+s.npages*pageSize {
			continue
		}
		if n > 0 && out[n-1] == s {
			continue
		}
		out[n] = s
		n++
	}
	return n
}

// logosSpanMovable reports whether the objects of s may move out of the
// block at bb. A span that holds a finalizer or cleanup special now or ever,
// a span with pinned objects, and a span that extends beyond the block stay
// in place (#159, #160, #175). So does a span in an mcache: it counts its
// allocations in MemStats only when it leaves the cache, as the difference of
// allocCount and allocCountBeforeCache, and moveObject lowers allocCount.
func logosSpanMovable(s *mspan, bb uintptr) bool {
	if s.spanclass.sizeclass() == 0 || s.isUserArenaChunk || s.sweepgen != mheap_.sweepgen {
		return false
	}
	if s.logosNoReloc || s.specials != nil || logosSpanPinned(s) {
		return false
	}
	return s.base() >= bb && s.base()+s.npages*pageSize <= bb+logosImmixBlockBytes
}

func logosMarkBlockPinned(bb uintptr) {
	if blk := logosImmixBlockFor(bb); blk != nil {
		blk.blockPinned = 1
	}
}

func logosMarkBlockUnpinned(bb uintptr) {
	if blk := logosImmixBlockLookup(bb); blk != nil {
		blk.blockPinned = 0
	}
}

// logosBlockLines recomputes the line marks of blk from the mark bits of its
// spans and returns the number of marked objects.
func logosBlockLines(blk *logosImmixBlock, spans []*mspan) int {
	blk.clearLines()
	live := 0
	for _, s := range spans {
		blk.markRange(s.limit, s.base()+s.npages*pageSize)
		for i := uintptr(0); i < uintptr(s.nelems); i++ {
			if s.markBitsForIndex(i).isMarked() {
				b := s.base() + i*s.elemsize
				blk.markRange(b, b+s.elemsize)
				live++
			}
		}
	}
	return live
}

func (ev *logosEvac) evalBlock(ha *heapArena, bb uintptr) {
	var spans [logosPagesPerBlock]*mspan
	n := logosBlockSpans(ha, bb, &spans)
	if n == 0 {
		return
	}
	ev.seen++
	pinned, flagged := false, false
	for _, s := range spans[:n] {
		pinned = pinned || logosSpanPinned(s)
		flagged = flagged || s.logosNoReloc
	}
	if pinned {
		ev.pinnedBlks++
		logosMarkBlockPinned(bb)
		return
	}
	logosMarkBlockUnpinned(bb)
	if flagged || ev.pinned.has(bb) {
		ev.fixedBlks++
		return
	}
	blk := &logosEvScratch
	blk.base = bb
	if logosBlockLines(blk, spans[:n]) == 0 || blk.freeLines() <= logosImmixLines/2 {
		ev.denseBlks++
		return
	}
	ev.pushMovable(bb, spans[:n])
}

// pushMovable makes every individually movable span of the fragmented block
// at bb a candidate.
func (ev *logosEvac) pushMovable(bb uintptr, spans []*mspan) {
	pushed := false
	for _, s := range spans {
		if logosSpanMovable(s, bb) {
			ev.cand.push(uintptr(unsafe.Pointer(s)))
			pushed = true
		}
	}
	if pushed {
		ev.blocks++
	}
}

// assignForwarding gives every candidate span a zeroed forwarding table.
func (ev *logosEvac) assignForwarding() {
	var total uintptr
	for i := uintptr(0); i < ev.cand.n; i++ {
		total += uintptr(ev.candidate(i).nelems) * goarch.PtrSize
	}
	fwd := sysAlloc(total, &memstats.other_sys, "logos evac")
	if fwd == nil {
		throw("logos evacuation: out of memory")
	}
	ev.fwd = uintptr(fwd)
	ev.fwdBytes = total
	off := uintptr(0)
	for i := uintptr(0); i < ev.cand.n; i++ {
		s := ev.candidate(i)
		s.logosFwd = ev.fwd + off
		off += uintptr(s.nelems) * goarch.PtrSize
	}
}

// logosDestEligible reports whether free slots of s may receive evacuated
// objects. A span with specials is excluded because an unmarked slot of it
// may hold an object that the sweep resurrects for its finalizer.
func logosDestEligible(s *mspan) bool {
	if s.state.get() != mSpanInUse || s.logosFwd != 0 || s.spanclass.sizeclass() == 0 || s.isUserArenaChunk {
		return false
	}
	return s.sweepgen == mheap_.sweepgen && s.specials == nil && !s.logosNoReloc
}

// buildPool groups the destination spans by span class.
func (ev *logosEvac) buildPool() {
	var counts, fill [gc.NumSizeClasses << 1]uintptr
	for _, s := range mheap_.allspans {
		if logosDestEligible(s) {
			counts[s.spanclass]++
		}
	}
	total := uintptr(0)
	for c := range counts {
		ev.cursors[c] = logosDestCursor{cur: total, end: total + counts[c]}
		fill[c] = total
		total += counts[c]
	}
	if total == 0 {
		return
	}
	ev.pool.grow(total)
	ev.pool.n = total
	for _, s := range mheap_.allspans {
		if logosDestEligible(s) {
			ev.pool.set(fill[s.spanclass], uintptr(unsafe.Pointer(s)))
			fill[s.spanclass]++
		}
	}
}

// allocDest returns the next free slot of a destination span of class spc.
func (ev *logosEvac) allocDest(spc spanClass) (ds *mspan, di uintptr, ok bool) {
	c := &ev.cursors[spc]
	for c.cur < c.end {
		ds = (*mspan)(unsafe.Pointer(ev.pool.get(c.cur)))
		if !c.open {
			c.budget, c.open = logosSpareSlots(ds), true
		}
		for c.budget > 0 && c.obj < uintptr(ds.nelems) {
			di = c.obj
			c.obj++
			if !ds.markBitsForIndex(di).isMarked() {
				c.budget--
				return ds, di, true
			}
		}
		c.cur++
		c.obj, c.open = 0, false
	}
	return nil, 0, false
}

// logosSpareSlots returns how many unmarked slots of ds the evacuation may
// claim. It leaves one: a span the central lists hold as partial must still
// have a free slot after the next sweep.
func logosSpareSlots(ds *mspan) uintptr {
	free := uintptr(0)
	for i := uintptr(0); i < uintptr(ds.nelems); i++ {
		if !ds.markBitsForIndex(i).isMarked() {
			free++
		}
	}
	if free == 0 {
		return 0
	}
	return free - 1
}

// evacuateSpan moves the marked objects of s while destination slots last.
func (ev *logosEvac) evacuateSpan(s *mspan) {
	for i := uintptr(0); i < uintptr(s.nelems); i++ {
		if !s.markBitsForIndex(i).isMarked() {
			continue
		}
		ds, di, ok := ev.allocDest(s.spanclass)
		if !ok {
			return
		}
		ev.moveObject(s, i, ds, di)
	}
}

func (ev *logosEvac) moveObject(s *mspan, i uintptr, ds *mspan, di uintptr) {
	src := s.base() + i*s.elemsize
	dst := ds.base() + di*ds.elemsize
	memmove(unsafe.Pointer(dst), unsafe.Pointer(src), s.elemsize)
	if !s.spanclass.noscan() && heapBitsInSpan(s.elemsize) {
		ds.logosSetSmallBits(dst, s.heapBitsSmallForAddr(src))
	}
	if ev.claimSlot(ds, di) {
		s.allocCount--
	}
	*s.logosFwdSlot(i) = dst
	s.markBitsForIndex(i).clearMarked()
	if goexperiment.GreenTeaGC && gcUsesSpanInlineMarkBits(s.elemsize) {
		s.scannedBitsForIndex(i).clearMarked()
	}
	ev.moved++
	ev.movedBytes += s.elemsize
}

// logosSetSmallBits stores the pointer bitmap bits for the object slot at x.
// It mirrors the write at the end of writeHeapBitsSmall.
func (span *mspan) logosSetSmallBits(x, src uintptr) {
	dstBase, _ := spanHeapBitsRange(span.base(), span.npages*pageSize, span.elemsize)
	dst := unsafe.Pointer(dstBase)
	o := (x - span.base()) / goarch.PtrSize
	i := o / ptrBits
	j := o % ptrBits
	bits := span.elemsize / goarch.PtrSize
	if j+bits > ptrBits {
		bits0 := ptrBits - j
		bits1 := bits - bits0
		dst0 := (*uintptr)(add(dst, (i+0)*goarch.PtrSize))
		dst1 := (*uintptr)(add(dst, (i+1)*goarch.PtrSize))
		*dst0 = (*dst0)&(^uintptr(0)>>bits0) | (src << j)
		*dst1 = (*dst1)&^((1<<bits1)-1) | (src >> bits0)
		return
	}
	d := (*uintptr)(add(dst, i*goarch.PtrSize))
	*d = (*d)&^(((1<<bits)-1)<<j) | (src << j)
}

// claimSlot makes slot di of ds a live, allocated object: it is marked like an
// object allocated black. A slot that no allocation had counted joins allocBits
// and allocCount, so that the sweep neither reports a zombie nor loses track
// of it, and claimSlot reports true. A slot that an earlier allocation counted
// and that the mark found dead is already in both, and claimSlot reports false.
//
// Moving an object allocates and frees nothing, so MemStats must not change.
// The sweep adds allocCount-popcount(marks) of each span to Frees, and every
// allocation is already in Mallocs. A move into a counted dead slot leaves the
// sweep's total alone: the source slot counts as freed and the destination slot
// no longer does. A move into an uncounted slot raises the destination's
// allocCount, and the caller lowers the source's by the same one, so that the
// sweep frees exactly the objects that died and Mallocs needs no credit.
func (ev *logosEvac) claimSlot(ds *mspan, di uintptr) (fresh bool) {
	ds.markBitsForIndex(di).setMarkedNonAtomic()
	if goexperiment.GreenTeaGC && gcUsesSpanInlineMarkBits(ds.elemsize) {
		ds.scannedBitsForIndex(di).setMarkedNonAtomic()
	}
	// A clear alloc bit means the slot is uncounted, wherever it sits.
	// The comment above requires exactly that case to raise allocCount so
	// the caller can lower the source's by one. Restricting this to slots
	// at or above freeindex left a destination mark with no allocation in
	// every swept hole below the watermark, which is the state the sweep's
	// zombie check reports as a pointer to a free object.
	if !ds.allocBitsForIndex(di).isMarked() {
		ds.allocBitsForIndex(di).setMarkedNonAtomic()
		ds.allocCount++
		fresh = true
	}
	arena, pageIdx, pageMask := pageIndexOf(ds.base())
	if arena.pageMarks[pageIdx]&pageMask == 0 {
		atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
	}
	return fresh
}
