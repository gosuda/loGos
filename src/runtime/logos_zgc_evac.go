// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"unsafe"
)

// Copy and publication for the zgc cycle.
//
// Per selected page: reserve to-space spans of the same spanclass via
// mheap.alloc (failure skips the page). Per marked object: memmove,
// forwardPublish plus identity moveLocked in one critical section (the
// existing zgcRelocateObject race-safe protocol: the CAS loser discards
// its copy); interior offsets preserved; whole 16 B tiny blocks; claim
// slots as immix claimSlot: allocBits, gcmarkBits, greentea scannedBits
// where inline, allocCount, page-allocator pageMarks. The pause asserts
// published == live per page.
//
// Locking: the page mutex serializes slot claims only. Copying and
// publication run outside it; publication reuses the race-safe CAS
// protocol and unclaims on loss. Allocation never happens under the page
// mutex: a refill drops the mutex first, so an allocation-triggered GC
// can never deadlock against the relocation finish hook.

// zgcSetSmallBits stores the pointer bitmap bits for the object slot at
// x. It mirrors the write at the end of writeHeapBitsSmall.
func (span *mspan) zgcSetSmallBits(x, src uintptr) {
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

// zgcClaimSlotBits makes slot di of ds a live, allocated object: it is
// marked like an object allocated black. The page mutex is held.
// A slot that no allocation had counted joins allocBits and allocCount,
// so the sweep neither reports a zombie nor loses track of it, and
// zgcClaimSlotBits reports fresh. A slot that an earlier allocation
// counted and that the mark found dead is already in both, and
// zgcClaimSlotBits reports taken.
//
// Moving an object allocates and frees nothing, so MemStats must not
// change. The sweep adds allocCount-popcount(marks) of each span to
// Frees, and every allocation is already in Mallocs. A move into a
// counted dead slot leaves the sweep's total alone: the source slot
// counts as freed and the destination slot no longer does. A move into
// an uncounted slot raises the destination's allocCount, and the caller
// lowers the source's by the same one, so the sweep frees exactly the
// objects that died and Mallocs needs no credit.
func zgcClaimSlotBits(ds *mspan, di uintptr) (fresh bool) {
	ds.markBitsForIndex(di).setMarkedNonAtomic()
	if gcUsesSpanInlineMarkBits(ds.elemsize) {
		ds.scannedBitsForIndex(di).setMarkedNonAtomic()
	}
	if di >= uintptr(ds.freeindex) && !ds.allocBitsForIndex(di).isMarked() {
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

// zgcReserveToSpace reserves a to-space span of class sc with at least
// need bytes free. Failure returns nil and the page is skipped.
func zgcReserveToSpace(sc spanClass, need uintptr) *mspan {
	npages := uintptr(1)
	if need > pageSize {
		npages = divRoundUp(need, pageSize)
	}
	return mheap_.alloc(npages, sc)
}

// zgcEvacuateObject copies the marked object at (s, i) into a to-space
// slot claimed for the page and publishes the forwarding entry. It
// returns the winning address; a caller that lost the race discards its
// copy and unclaims its slot.
func zgcEvacuateObject(pg *zgcPage, s *mspan, i uintptr) uintptr {
	src := s.base() + i*s.elemsize
	size := s.elemsize
	if s.spanclass.noscan() && s.spanclass.sizeclass() == 0 {
		size = s.npages * pageSize
	}
	return zgcCopyOne(pg, s, src, size)
}

// zgcCopyOne claims a to-space slot of the source's class, copies the
// size-byte object at src, and publishes it. The claim holds the page
// mutex; the copy and the publication run outside it.
func zgcCopyOne(pg *zgcPage, s *mspan, src, size uintptr) uintptr {
	st := zgcPageState(pg)
	ds, di, claimed := zgcClaimOne(pg, st, s.spanclass)
	if !claimed {
		return 0
	}
	dst := ds.base() + di*ds.elemsize
	memmove(unsafe.Pointer(dst), unsafe.Pointer(src), size)
	if !s.spanclass.noscan() && heapBitsInSpan(s.elemsize) {
		ds.zgcSetSmallBits(dst, s.heapBitsSmallForAddr(src))
	}
	// Mirroring the immix claim: a fresh destination slot joins the
	// destination count, and a published copy leaves the source count.
	// The source keeps its own count because quarantine deliberately
	// does not settle it: only the sweep frees, and it frees exactly
	// the objects that died.
	fresh := zgcClaimSlotBitsUnder(st, ds, di)
	won := zgcRelocateObject(pg, src, size, dst)
	if won != dst {
		zgcUnclaimOne(pg, st, ds, di)
		return won
	}
	if fresh {
		lock(&st.mu)
		s.allocCount--
		unlock(&st.mu)
	}
	return won
}

// zgcClaimOne scans the page's to-space spans of class sc for a free
// slot, refilling the reservation when exhausted. The reservation refill
// allocates outside the page mutex. It reports whether a slot was
// claimed; the claim bits are set before return.
func zgcClaimOne(pg *zgcPage, st *zgcPageSpans, sc spanClass) (*mspan, uintptr, bool) {
	if ds, di, ok := zgcScanClaim(st, sc); ok {
		return ds, di, true
	}
	if zgcRefillPToSpace(pg, sc) == nil {
		return nil, 0, false
	}
	return zgcScanClaim(st, sc)
}

// zgcScanClaim claims a free to-space slot of class sc. It takes the
// page mutex.
func zgcScanClaim(st *zgcPageSpans, sc spanClass) (*mspan, uintptr, bool) {
	lock(&st.mu)
	ds, di, ok := zgcScanClaimLocked(st, sc)
	unlock(&st.mu)
	return ds, di, ok
}

func zgcScanClaimLocked(st *zgcPageSpans, sc spanClass) (*mspan, uintptr, bool) {
	next := st.cursor
	for _, ds := range st.toSpace {
		if ds.spanclass != sc {
			continue
		}
		n := uintptr(ds.nelems)
		for k := uintptr(0); k < n; k++ {
			di := next
			if di >= n {
				di = 0
			}
			next = di + 1
			if !ds.markBitsForIndex(di).isMarked() {
				ds.markBitsForIndex(di).setMarkedNonAtomic()
				st.cursor = next
				return ds, di, true
			}
		}
		next = 0
	}
	st.cursor = 0
	return nil, 0, false
}

// zgcClaimSlotBitsUnder finishes a scanned claim: scanned mark bits are
// already set by the scan; this sets the inline, alloc, and accounting
// state. It reports fresh as zgcClaimSlotBits does.
func zgcClaimSlotBitsUnder(st *zgcPageSpans, ds *mspan, di uintptr) (fresh bool) {
	lock(&st.mu)
	if gcUsesSpanInlineMarkBits(ds.elemsize) {
		ds.scannedBitsForIndex(di).setMarkedNonAtomic()
	}
	if di >= uintptr(ds.freeindex) && !ds.allocBitsForIndex(di).isMarked() {
		ds.allocBitsForIndex(di).setMarkedNonAtomic()
		ds.allocCount++
		fresh = true
	}
	arena, pageIdx, pageMask := pageIndexOf(ds.base())
	if arena.pageMarks[pageIdx]&pageMask == 0 {
		atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
	}
	unlock(&st.mu)
	return fresh
}

// zgcUnclaimOne releases a destination slot claimed for a copy that lost
// the publication race: the slot holds no published object, so it must
// read free again instead of leaking as permanently marked.
func zgcUnclaimOne(pg *zgcPage, st *zgcPageSpans, ds *mspan, di uintptr) {
	lock(&st.mu)
	ds.markBitsForIndex(di).clearMarked()
	if gcUsesSpanInlineMarkBits(ds.elemsize) {
		ds.scannedBitsForIndex(di).clearMarked()
	}
	if ds.allocBitsForIndex(di).isMarked() {
		ds.allocBitsForIndex(di).clearMarked()
		ds.allocCount--
	}
	unlock(&st.mu)
}

// zgcEvacuatePage copies every marked object of the page's spans and
// publishes their forwarding entries. It reports whether every live
// byte of the page won a published copy.
func zgcEvacuatePage(pg *zgcPage) bool {
	if !zgcCycleEnabled() {
		return false
	}
	for _, s := range zgcPageState(pg).spans {
		if s.spanclass.sizeclass() == 0 {
			if s.markBitsForIndex(0).isMarked() {
				if _, ok := pg.forward(s.base()); !ok {
					if zgcEvacuateObject(pg, s, 0) == 0 {
						return false
					}
				}
			}
			continue
		}
		for i := uintptr(0); i < uintptr(s.nelems); i++ {
			if !s.markBitsForIndex(i).isMarked() {
				continue
			}
			if _, ok := pg.forward(s.base() + i*s.elemsize); ok {
				continue
			}
			if zgcEvacuateObject(pg, s, i) == 0 {
				return false
			}
		}
	}
	return pg.published.Load() == pg.live
}

// zgcPublishToSpace is the pause-end publication of a fully relocated
// page: quarantine its source spans, publish its to-space spans onto the
// swept sets, and queue it for background release.
func zgcPublishToSpace(pg *zgcPage) {
	assertWorldStopped()
	for _, s := range zgcPageState(pg).spans {
		zgcQuarantineSpan(s, pg)
	}
	zgcPublishToSpaceSpans(pg)
}
