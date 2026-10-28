// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Quarantine, to-space publication, release, and retire.
//
// A selected page's source spans are quarantined: they stay mSpanInUse
// (spanOfHeap gates on it) so forwarding metadata and span lookups keep
// working while the mutator heals through the table. The sweeper must
// never claim them, the allocator must never reuse them, and release
// (MADV_DONTNEED) happens only after every live byte has a published
// copy. Retire frees them through the ordinary freeSpanLocked mSpanInUse
// arm once a later remap pass healed every stale slot.

// zgcQuarantineEntry tracks one quarantined source span.
type zgcQuarantineEntry struct {
	span *mspan
	page *zgcPage
}

var (
	zgcQuarantineLock mutex
	zgcQuarantine     []zgcQuarantineEntry
	zgcRetirePending  []*zgcPage
)

// zgcPageSpans tracks the quarantined source spans and reserved
// to-space spans of each registered page. The lists live in ordinary
// Go memory: zgcPage carries sys.NotInHeap, so a slice header inside
// it would hide its backing array from the collector and dangle.
type zgcPageSpans struct {
	mu      mutex
	spans   []*mspan
	toSpace []*mspan
	cursor  uintptr
	next    *zgcPageSpans
	prev    *zgcPageSpans
}

var (
	zgcPageSpansLock  mutex
	zgcPageSpansMap   map[*zgcPage]*zgcPageSpans
	zgcPageStatesHead *zgcPageSpans
	zgcStateFree      [128]*zgcPageSpans
	zgcStateFreeN     int
	zgcEmptyPageSpans zgcPageSpans
)

// zgcInitPageRegistry preallocates the page registry and a pool of page
// states while the world runs. Pause paths must not grow Go heap memory:
// an allocation that trips the heap goal would start a collection and
// deadlock against the stopped world.
func zgcInitPageRegistry() {
	lock(&zgcPageSpansLock)
	defer unlock(&zgcPageSpansLock)
	if zgcPageSpansMap != nil {
		return
	}
	zgcPageSpansMap = make(map[*zgcPage]*zgcPageSpans, 128)
	for i := range zgcStateFree {
		// A 2 MB page overlaps at most 256 spans; the reservation
		// is one span per source span. Pre-sized once so pause
		// appends never grow.
		zgcStateFree[i] = &zgcPageSpans{
			spans:   make([]*mspan, 0, 256),
			toSpace: make([]*mspan, 0, 256),
		}
	}
	zgcStateFreeN = len(zgcStateFree)
	// Pause-time append targets, sized past one cycle's budget so the
	// pauses never grow them: 32 pages of 256 spans at the 64 MB cap.
	zgcQuarantine = make([]zgcQuarantineEntry, 0, 8192)
	zgcRetirePending = make([]*zgcPage, 0, 128)
	zgcAllPages = make([]*zgcPage, 0, 128)
}

func zgcPageState(pg *zgcPage) *zgcPageSpans {
	if pg == nil {
		return &zgcEmptyPageSpans
	}
	lock(&zgcPageSpansLock)
	if zgcPageSpansMap == nil {
		unlock(&zgcPageSpansLock)
		return &zgcEmptyPageSpans
	}
	st := zgcPageSpansMap[pg]
	if st == nil {
		if zgcStateFreeN != 0 {
			zgcStateFreeN--
			st = zgcStateFree[zgcStateFreeN]
			zgcStateFree[zgcStateFreeN] = nil
		} else {
			st = &zgcPageSpans{}
		}
		zgcPageSpansMap[pg] = st
		st.next = zgcPageStatesHead
		if zgcPageStatesHead != nil {
			zgcPageStatesHead.prev = st
		}
		st.prev = nil
		zgcPageStatesHead = st
	}
	unlock(&zgcPageSpansLock)
	return st
}

func zgcPageForget(pg *zgcPage) {
	if pg == nil {
		return
	}
	lock(&zgcPageSpansLock)
	if zgcPageSpansMap != nil {
		if st := zgcPageSpansMap[pg]; st != nil {
			if st.prev != nil {
				st.prev.next = st.next
			} else {
				zgcPageStatesHead = st.next
			}
			if st.next != nil {
				st.next.prev = st.prev
			}
			st.next = nil
			st.prev = nil
			// Keep the pre-sized backing arrays; only the lengths and
			// the cursor reset so the next pause reuses them. Under the
			// entry mutex so a concurrent to-space bypass scan cannot
			// tear the list.
			lock(&st.mu)
			st.spans = st.spans[:0]
			st.toSpace = st.toSpace[:0]
			st.cursor = 0
			unlock(&st.mu)
			if zgcStateFreeN < len(zgcStateFree) {
				zgcStateFree[zgcStateFreeN] = st
				zgcStateFreeN++
			}
		}
		delete(zgcPageSpansMap, pg)
	}
	unlock(&zgcPageSpansLock)
}

// zgcSpanQuarantined reports whether s is a quarantined source span. It
// runs on the read path of the walk core and the sweep guard.
func zgcSpanQuarantined(s *mspan) bool {
	if s == nil || atomic.Load(&s.sweepgen) != mheap_.sweepgen+2 {
		return false
	}
	return zgcQuarantineHas(s)
}

// zgcQuarantineHas checks membership under the quarantine lock.
func zgcQuarantineHas(s *mspan) bool {
	lock(&zgcQuarantineLock)
	found := false
	for i := range zgcQuarantine {
		if zgcQuarantine[i].span == s {
			found = true
			break
		}
	}
	unlock(&zgcQuarantineLock)
	return found
}

// zgcQuarantineBypass reports whether the sweeper must leave s alone
// without waiting: s is a quarantined source span that never rejoins
// the sweep, or a reserved to-space span that is swept before it can
// be attached to. Membership-gated, so it holds across sweepgen
// settles. The to-space arm is the attach handshake's sweep half: an
// addspecial on a copy reaches ensureSwept on its destination span,
// and waiting for that span's sweep would spin forever.
func zgcQuarantineBypass(s *mspan) bool {
	if s == nil || !zgcCycleEnabled() || atomic.Load(&s.sweepgen) != mheap_.sweepgen+2 {
		// Neither a quarantine entry nor a to-space reservation can
		// name s: both pin the pre-bump sweepgen (quarantine.go:283,
		// concurrent.go:145) and the refresh rule re-applies it
		// every pause. Ordinary and pre-first-quarantine spans —
		// including every bootstrap span — skip the locks, matching
		// zgcSpanQuarantined and zgcToSpaceBypass.
		return false
	}
	return zgcQuarantineHas(s) || zgcToSpaceBypass(s)
}

// zgcToSpaceBypass reports whether s is a reserved to-space span of a
// registered page. The sweepgen pre-check keeps ordinary spans out
// without taking locks; only spans the session pinned past the sweeper
// pay for the registry scan. Registry reads take the per-entry mutex,
// never the map lock alone, so a concurrent retire cannot tear the
// list mid-scan.
func zgcToSpaceBypass(s *mspan) bool {
	if s == nil || atomic.Load(&s.sweepgen) != mheap_.sweepgen+2 {
		return false
	}
	lock(&zgcPageSpansLock)
	defer unlock(&zgcPageSpansLock)
	for st := zgcPageStatesHead; st != nil; st = st.next {
		lock(&st.mu)
		found := false
		for _, t := range st.toSpace {
			if t == s {
				found = true
				break
			}
		}
		unlock(&st.mu)
		if found {
			return true
		}
	}
	return false
}

// zgcSweepGuard is defense in depth at the entry of sweepLocked.sweep:
// a quarantined span stores the swept sweepgen itself and is reported as
// swept without requeueing, so a sweeper that reached it through any
// path other than tryAcquire still cannot free or recycle it.
//
//go:nowritebarrierrec
func zgcSweepGuard(s *mspan) bool {
	if !zgcCycleEnabled() || !(zgcQuarantineHas(s) || zgcToSpaceBypass(s)) {
		return false
	}
	atomic.Store(&s.sweepgen, mheap_.sweepgen)
	return true
}

// zgcQuarantineSpan quarantines the source span s of page pg. The world
// is stopped. Quarantine drops the span's mark and page-use bits and
// takes the pre-bump sweepgen so tryAcquire never claims it. It performs
// no accounting: the source keeps its allocCount and each published copy
// transfers one count to its destination, netting to zero.
func zgcQuarantineSpan(s *mspan, pg *zgcPage) {
	if s.state.get() != mSpanInUse {
		throw("zgc: quarantining a span that is not in use")
	}
	// The span will never be swept, so this loop performs the sweep's
	// dead-object accounting early: slots the mark phase left unmarked
	// are freed here, and allocCount is reset to the live count the
	// per-copy transfer then drains to zero. Only non-destructive hooks
	// run: the source memory stays the copy source until release, so
	// clobbering or poisoning it would corrupt relocation.
	var live, dead uintptr
	mbits := s.markBitsForBase()
	abits := s.allocBitsForIndex(0)
	for i := uintptr(0); i < uintptr(s.nelems); i++ {
		if mbits.isMarked() {
			live++
		} else if abits.index < uintptr(s.freeindex) || abits.isMarked() {
			x := s.base() + i*s.elemsize
			if traceAllocFreeEnabled() {
				trace := traceAcquire()
				if trace.ok() {
					trace.HeapObjectFree(x)
					traceRelease(trace)
				}
			}
			if raceenabled && !s.isUserArenaChunk {
				racefree(unsafe.Pointer(x), s.elemsize)
			}
			dead++
		}
		mbits.advance()
		abits.advance()
	}
	if live > uintptr(s.allocCount) {
		print("runtime: nelems=", s.nelems, " live=", live, " previous allocCount=", s.allocCount, "\n")
		throw("zgc: quarantine found more marks than allocations")
	}
	if dead != 0 {
		if s.spanclass.sizeclass() != 0 {
			stats := memstats.heapStats.acquire()
			atomic.Xadd64(&stats.smallFreeCount[s.spanclass.sizeclass()], int64(dead))
			memstats.heapStats.release()
		}
		gcController.totalFree.Add(int64(dead) * int64(s.elemsize))
	}
	s.allocCount = uint16(live)
	if gcUsesSpanInlineMarkBits(s.elemsize) {
		// Inline-mark spans keep their live set in the span's own
		// first page, which no mark-bit arena recycle touches, and
		// every reader (markBitsForIndex, scannedBitsForIndex)
		// addresses that storage, never s.gcmarkBits. Leave it
		// intact for evacuation and only de-alias the heap pointer.
		// Release zeroes the page once every object is published.
		s.gcmarkBits = newMarkBits(uintptr(s.nelems))
	} else {
		// Heap mark bits live in recyclable arenas, so copy the live
		// set into a fresh disjoint extent before the arena turns
		// over: evacuation reads these bits to find live objects.
		kept := newMarkBits(uintptr(s.nelems))
		zgcCopyMarkBits(kept, s.gcmarkBits, uintptr(s.nelems))
		s.gcmarkBits = kept
	}
	arena, pageIdx, pageMask := pageIndexOf(s.base())
	atomic.And8(&arena.pageInUse[pageIdx], ^pageMask)
	if gcUsesSpanInlineMarkBits(s.elemsize) {
		atomic.And8(&arena.pageUseSpanInlineMarkBits[pageIdx], ^pageMask)
	}
	atomic.Store(&s.sweepgen, mheap_.sweepgen+2)
	lock(&zgcQuarantineLock)
	zgcQuarantine = append(zgcQuarantine, zgcQuarantineEntry{span: s, page: pg})
	unlock(&zgcQuarantineLock)
}

// zgcCopyMarkBits copies the first nelems mark bits from src to dst.
// Both ranges address the same object indices; the copy keeps the live
// set readable after the source bitmap's arena is recycled.
func zgcCopyMarkBits(dst, src *gcBits, nelems uintptr) {
	for i := uintptr(0); i < divRoundUp(nelems, 8); i++ {
		*dst.bytep(i) = *src.bytep(i)
	}
}

// zgcIdentityForgetRange drops the identity hashes of every object in
// the page's address range, so a later object at a reused address starts
// with a fresh identity and the table does not grow with dead objects.
func zgcIdentityForgetRange(pg *zgcPage) {
	t := &zgcIdentity
	lock(&t.lock)
	if t.entries != nil {
		for i := uintptr(0); i <= t.mask; i++ {
			e := t.slot(i)
			if e.addr >= pg.start && e.addr < pg.start+pg.size {
				e.addr = zgcIdentTomb
				t.live--
			}
		}
	}
	unlock(&t.lock)
}

// zgcUnquarantineSpan drops s from the quarantine set without freeing it.
// The retire path calls it after freeSpanLocked; the refresh path never
// calls it.
func zgcUnquarantineSpan(s *mspan) {
	lock(&zgcQuarantineLock)
	for i := range zgcQuarantine {
		if zgcQuarantine[i].span == s {
			zgcQuarantine[i] = zgcQuarantine[len(zgcQuarantine)-1]
			zgcQuarantine = zgcQuarantine[:len(zgcQuarantine)-1]
			break
		}
	}
	unlock(&zgcQuarantineLock)
}

// zgcRefreshDeferredSpans re-applies the pre-bump sweepgen to quarantined
// spans at every pause while their page awaits retire. A gcSweep cycle
// between quarantine and retire bumps mheap_.sweepgen; without the
// refresh the span would read as sweepable again.
func zgcRefreshDeferredSpans() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	lock(&zgcQuarantineLock)
	for i := range zgcQuarantine {
		atomic.Store(&zgcQuarantine[i].span.sweepgen, mheap_.sweepgen+2)
	}
	unlock(&zgcQuarantineLock)
}

// zgcPublishReadyToSpace pushes the filled to-space spans of every
// registered page onto the swept span sets. It runs in the pause before
// selection, so newly reserved spans stay invisible to this sweep.
func zgcPublishReadyToSpace() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	zgcPagesLock()
	zgcScratchPages = append(zgcScratchPages[:0], zgcAllPages...)
	zgcPagesUnlock()
	for _, pg := range zgcScratchPages {
		zgcPublishToSpaceSpans(pg)
	}
	zgcScratchPages = zgcScratchPages[:0]
}

// zgcPublishToSpaceSpans pushes the filled to-space spans of pg onto the
// swept span sets with the current sweepgen, exactly like freshly swept
// spans: after the bump the sweeper claims each span, frees nothing, and
// rotates the bits.
func zgcPublishToSpaceSpans(pg *zgcPage) {
	if !zgcCycleEnabled() || pg == nil {
		return
	}
	assertWorldStopped()
	st := zgcPageState(pg)
	if st == nil || st == &zgcEmptyPageSpans {
		return
	}
	for _, s := range st.toSpace {
		if s == nil || s.state.get() != mSpanInUse {
			continue
		}
		atomic.Store(&s.sweepgen, mheap_.sweepgen)
		if s.allocCount == 0 {
			mheap_.central[s.spanclass].mcentral.fullSwept(mheap_.sweepgen).push(s)
		} else {
			mheap_.central[s.spanclass].mcentral.partialSwept(mheap_.sweepgen).push(s)
		}
	}
	st.toSpace = nil
}

// zgcRetirePage retires a released page once a remap pass completed after
// its release, and reports whether it did. Per span: assert allocCount
// is zero, settle the sweepgen, free through the ordinary freeSpanLocked
// mSpanInUse arm (pagesInUse decrements once there), then forget the
// page's identities and free the page metadata.
func zgcRetirePage(pg *zgcPage) bool {
	if pg.released.Load() == 0 || zgcRemapEpoch.Load() == pg.releaseEpoch {
		return false
	}
	assertWorldStopped()
	st := zgcPageState(pg)
	if st != nil && st != &zgcEmptyPageSpans {
		lock(&mheap_.lock)
		for _, s := range st.spans {
			if s == nil {
				continue
			}
			if s.allocCount != 0 {
				unlock(&mheap_.lock)
				throw("zgc: retiring a span with live allocations")
			}
			atomic.Store(&s.sweepgen, mheap_.sweepgen)
			mheap_.freeSpanLocked(s, spanAllocHeap)
			zgcUnquarantineSpan(s)
		}
		unlock(&mheap_.lock)
		st.spans = nil
		st.toSpace = nil
	}
	zgcPageForget(pg)
	zgcIdentityForgetRange(pg)
	zgcFreePage(pg)
	return true
}

// zgcRetirePendingPages retires every released page whose remap pass has
// completed. It runs in the pause, after the refresh rule.
func zgcRetirePendingPages() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	zgcRefreshDeferredSpans()
	lock(&zgcQuarantineLock)
	pending := zgcRetirePending
	zgcRetirePending = nil
	unlock(&zgcQuarantineLock)
	var keep []*zgcPage
	for _, pg := range pending {
		if !zgcRetirePage(pg) {
			keep = append(keep, pg)
		}
	}
	if len(keep) != 0 {
		lock(&zgcQuarantineLock)
		zgcRetirePending = append(zgcRetirePending, keep...)
		unlock(&zgcQuarantineLock)
	}
}
