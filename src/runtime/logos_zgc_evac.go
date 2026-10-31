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
// x. It mirrors the write at the end of writeHeapBitsSmall. The write is
// a plain read-modify-write of a bitmap word that adjacent slots share,
// so it must run under the page mutex of the span's page state; the
// caller is zgcClaimSlotBitsUnder.
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

// zgcReserveToSpace reserves a to-space span of class sc with at least
// need bytes free. Failure returns nil and the page is skipped.
// The reservation feeds the per-span ledger (see logos_zgc_quarantine.go):
// one reserve per mheap grant, recorded past the allocation holding no
// lock, so the leaf ledger lock never nests.
func zgcReserveToSpace(sc spanClass, need uintptr) *mspan {
	npages := uintptr(1)
	if need > pageSize {
		npages = divRoundUp(need, pageSize)
	}
	ns := mheap_.alloc(npages, sc)
	if ns != nil {
		// The same initialization mcentral.grow gives a fresh span:
		// the method's own class guards skip noscan and large
		// (malloc-header) spans, and greentea spans get their inline
		// mark bits cleared. To-space slots claim against this bitmap
		// before any allocation lands in it, so stale words from a
		// recycled page must not survive the reservation.
		ns.initHeapBits()
	}
	if zgcSpanLedgerDebug && ns != nil {
		zgcSpanLedgerReserve(ns)
	}
	return ns
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

// zgcCopyOneResult claims a to-space slot of the source's class and commits
// it through the single authoritative zgcRelocateObject commit, reporting the
// typed outcome. Claiming and slot finishing — the pointer bitmap
// included — are preparation outside the gate and may allocate or
// safepoint; the commit revalidates under
// the gate, performs the one memmove, and publishes forwarding, identity and
// accounting. Any outside pre-check is racy.
//
// Claim failure reports only NoMemory observed at the allocator: the
// reservation returned nil. Commit 0 reports Refused observed under the gate:
// a frozen (pinned or exposed) source, or a concurrent unregister/holds race
// that also leaves the source stable. A lost publication race unclaims its
// slot and reports Done with the winner. Geometry and the private claim
// protocol are unchanged.
//
// Copy veto for durable pins: refuse when the source object carries a pin
// bit or its span carries the sticky flag, so a worker or barrier does not
// copy a source after its exposure ends and the pin has committed. This
// closes the post-exposure hole for the in-place pin below; the narrow
// claim-then-commit race (claim before a concurrent pin sets its bits,
// commit after) stays open because the check is outside the gate. The exact
// gate-held pin-bit recheck belongs in zgcRelocateObject
// (logos_zgc_page.go) and is reported as remaining work, not pretended as
// closed. Claim failure below is only NoMemory observed at the allocator;
// commit 0 below is only Refused observed under the gate.
//
// The caller's lease on pg covers the whole attempt — state acquisition,
// claim, refill allocation, NoMemory exit, commit, unclaim and the final
// publication outcome — so no suspend point between them can strand the
// state against a pool return: zgcPageForget is gated on refs==0 after
// the acquisition domain drained. No per-call counter exists and none is
// needed.
func zgcCopyOneResult(pg *zgcPage, s *mspan, src, size uintptr) (uintptr, zgcRelocateStatus) {
	if pg == nil || s == nil {
		return 0, zgcRelocateNotHeap
	}
	if s.logosPinned.Load() || isPinned(unsafe.Pointer(src)) {
		return 0, zgcRelocateRefused
	}
	st := zgcPageState(pg)
	ds, di, claimed := zgcClaimOne(st, s.spanclass, s.npages)
	if !claimed {
		return 0, zgcRelocateNoMemory
	}
	// One reservation per claimed slot, keyed by the source the slot was
	// claimed for (see logos_zgc_quarantine.go). No lock held here.
	if zgcSpanLedgerDebug {
		zgcSpanLedgerSrcReserve(s)
	}
	dst := ds.base() + di*ds.elemsize
	// Mirroring the immix claim: a fresh destination slot joins the
	// destination count, and a published copy leaves the source count.
	// The source keeps its own count because quarantine deliberately
	// does not settle it: only the sweep frees, and it frees exactly
	// the objects that died.
	fresh := zgcClaimSlotBitsUnder(st, ds, di, s, src)
	won := zgcRelocateObject(pg, src, size, dst)
	if won != dst {
		zgcUnclaimOne(pg, st, ds, di)
		// The claim resolved by release; closes the source reservation
		// whether the loss was a race or a refusal. No lock held here.
		if zgcSpanLedgerDebug {
			zgcSpanLedgerSrcUnclaim(s)
		}
		if won == 0 {
			return 0, zgcRelocateRefused
		}
		return won, zgcRelocateDone
	}
	if fresh {
		lock(&st.mu)
		s.allocCount--
		unlock(&st.mu)
	}
	// The claim resolved by publication; closes the source reservation
	// whether or not the slot joined the count. No lock held here.
	if zgcSpanLedgerDebug {
		zgcSpanLedgerSrcCommit(s)
	}
	return won, zgcRelocateDone
}

// zgcCopyOne claims a to-space slot of the source's class and commits it
// through the single authoritative zgcRelocateObject commit. It stays live
// for evac workers and shares the Result implementation above, preserving
// worker semantics: a winning copy returns its address (the loser unclaims),
// any non-Done outcome returns 0 so the page stops for this pass and the
// remaining sources are left for barrier relocation or a later pass.
func zgcCopyOne(pg *zgcPage, s *mspan, src, size uintptr) uintptr {
	won, st := zgcCopyOneResult(pg, s, src, size)
	if st != zgcRelocateDone {
		return 0
	}
	return won
}

// zgcClaimOne scans the page's to-space spans of class sc for a free
// slot, reserving a fresh span sized by the source (npages) when
// exhausted. The reservation allocates outside the page mutex. It
// reports whether a slot was claimed; the claim bits are set before
// return. False means the heap allocation returned nil.
func zgcClaimOne(st *zgcPageSpans, sc spanClass, npages uintptr) (*mspan, uintptr, bool) {
	if ds, di, ok := zgcScanClaim(st, sc); ok {
		return ds, di, true
	}
	return zgcRefillClaim(st, sc, npages)
}

// zgcRefillClaim reserves a fresh to-space span of class sc sized for
// npages source pages and claims its slot 0 for the caller. The span is
// still private, so the claim needs no CAS: the mark bit is set before
// the span is appended under the page mutex, and the span is published
// with the claim owned. Allocator, pointer and inline bits are set later
// by zgcClaimSlotBitsUnder; a lost publication race unclaims through
// zgcUnclaimOne. It returns false only when the heap allocation
// returns nil.
func zgcRefillClaim(st *zgcPageSpans, sc spanClass, npages uintptr) (*mspan, uintptr, bool) {
	ns := zgcReserveToSpace(sc, npages*pageSize)
	if ns == nil {
		return nil, 0, false
	}
	lock(&st.mu)
	ns.markBitsForIndex(0).setMarkedNonAtomic()
	st.toSpace = append(st.toSpace, ns)
	st.cursor = 1
	unlock(&st.mu)
	return ns, 0, true
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
			if ds.markBitsForIndex(di).isMarked() {
				continue
			}
			// A slot below freeindex with a clear alloc bit is a hole
			// left by an unclaimed copy: the sweep never handed it out,
			// so marking it would count a live slot against no
			// allocation and the quarantine vet would see more marks
			// than allocations. Skip holes; the span refills when no
			// allocatable slot is left.
			if di < uintptr(ds.freeindex) && !ds.allocBitsForIndex(di).isMarked() {
				continue
			}
			ds.markBitsForIndex(di).setMarkedNonAtomic()
			st.cursor = next
			return ds, di, true
		}
		next = 0
	}
	st.cursor = 0
	return nil, 0, false
}

// zgcClaimSlotBitsUnder finishes a scanned claim: scanned mark bits are
// already set by the scan; this sets the mark, inline, alloc, and
// accounting state, and copies the source pointer bitmap into the
// destination slot. s and src name the source object the slot was
// claimed for; when its class keeps pointers in the span bitmap, the
// destination slot's bits are written from the source bits here, under
// the page mutex. The bitmap write rides this critical section because
// zgcSetSmallBits is a plain read-modify-write of a word that adjacent
// slots share: two concurrent claims of neighbouring slots racing
// outside the mutex lose each other's bits. It reports whether the slot
// joined the count (fresh). The mark bit is set here as well as by the
// scan: a pause-time publication scrub may clear a mark claimed but not
// yet finished, so the finishing claim re-marks its slot before
// committing.
func zgcClaimSlotBitsUnder(st *zgcPageSpans, ds *mspan, di uintptr, s *mspan, src uintptr) (fresh bool) {
	lock(&st.mu)
	if !s.spanclass.noscan() && heapBitsInSpan(s.elemsize) {
		ds.zgcSetSmallBits(ds.base()+di*ds.elemsize, s.heapBitsSmallForAddr(src))
	}
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
	unlock(&st.mu)
	// Past the page mutex, holding no lock: the leaf ledger lock never
	// nests. Only fresh finishes move the running count, so only they
	// enter the committed tally.
	if zgcSpanLedgerDebug && fresh {
		zgcSpanLedgerCommit(ds)
	}
	return fresh
}

// zgcUnclaimOne releases a destination slot claimed for a copy that lost
// the publication race: the slot holds no published object, so it must
// read free again instead of leaking as permanently marked.
func zgcUnclaimOne(pg *zgcPage, st *zgcPageSpans, ds *mspan, di uintptr) {
	dropped := false
	lock(&st.mu)
	ds.markBitsForIndex(di).clearMarked()
	if gcUsesSpanInlineMarkBits(ds.elemsize) {
		ds.scannedBitsForIndex(di).clearMarked()
	}
	if ds.allocBitsForIndex(di).isMarked() {
		ds.allocBitsForIndex(di).clearMarked()
		ds.allocCount--
		dropped = true
	}
	unlock(&st.mu)
	// Past the page mutex, holding no lock: the leaf ledger lock never
	// nests. Only unclaims that drop an allocation move the running
	// count, so only they enter the unclaimed tally.
	if zgcSpanLedgerDebug && dropped {
		zgcSpanLedgerUnclaim(ds)
	}
}

// zgcEvacuatePage copies every marked object of the page's spans and
// publishes their forwarding entries. It reports whether every live
// byte of the page won a published copy.
//
// There is no page-level foreign exclusion here: the exact commit in
// zgcRelocateObject is authoritative. A source object currently exposed to
// foreign code commits to 0, its claimed slot is unclaimed and this pass
// stops there; the remaining sources are copied by barrier relocation or a
// later pass once the exposure ends. A foreign exposure of some other
// object in the region (a gap object, a to-space copy) is not a source and
// does not stop any source from moving. Release stays gated on the region
// query (zgcReleasePage), so such a pin only delays release.
//
// Lease ownership: the caller owns the page lease. The relocation worker
// holds the ref zgcNextUnfinishedPage gained under the registry lock and
// releases it after the attempt; the stop-the-world finish hook and the
// test seams call in under the world stop, where reclamation is excluded
// by the pause itself. zgcEvacuatePage neither gains nor drops a ref on
// any path.
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
