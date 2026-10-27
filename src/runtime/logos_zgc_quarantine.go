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
// (MADV_DONTNEED over exactly the quarantined spans, never the 2 MB
// region around them) happens only after every live byte has a published
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
	zgcPageSpansRankOnce atomic.Uint32
)

// zgcInitPageRegistry preallocates the page registry and a pool of page
// states while the world runs. Pause paths must not grow Go heap memory:
// an allocation that trips the heap goal would start a collection and
// deadlock against the stopped world.
func zgcInitPageRegistry() {
	// Rank zgcPageSpansLock once before its first acquisition: 0 uninit,
	// 1 initializing, 2 ready. First call may run in gcinit→zgcInit
	// before runtime doInit (package init too late); first call may also
	// come from parallel LogosZgcInit on non-zgc startup, so a bare
	// CAS(0→1)+lockInit publishes 1 before rank ready and a loser would
	// skip init and acquire unranked leaf. Initializer publishes 2 with
	// release after lockInit; losers spin on atomics only (no alloc, no
	// lock) until 2. Never re-mutated.
	for zgcPageSpansRankOnce.Load() != 2 {
		if zgcPageSpansRankOnce.CompareAndSwap(0, 1) {
			lockInit(&zgcPageSpansLock, lockRankZgcPageSpans)
			zgcPageSpansRankOnce.Store(2)
		} else {
			for zgcPageSpansRankOnce.Load() == 1 {
				procyield(30)
			}
		}
	}
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
	// pauses rarely grow them: 32 pages of 256 spans at the 64 MB cap.
	// Page lists are additionally capped at the directory bound, which
	// registration cannot exceed. The quarantine list alone may grow
	// past its seed when unretired backlog accumulates across cycles;
	// that growth runs in the selection pause on the system stack, where
	// a triggered collection cannot start (gcStart returns early without
	// a P), so it cannot deadlock against the stopped world the way a
	// world-running holder's growth could.
	zgcQuarantine = make([]zgcQuarantineEntry, 0, 8192)
	zgcRetirePending = make([]*zgcPage, 0, zgcMaxPages)
	zgcAllPages = make([]*zgcPage, 0, zgcMaxPages)
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

// zgcPageSourceSpans returns the source spans recorded for pg without
// creating a state entry for a page that has none. The list changes only
// in pauses (selection, abandon, retire), so the header read here stays
// valid for the pause that asked for it.
func zgcPageSourceSpans(pg *zgcPage) []*mspan {
	var spans []*mspan
	lock(&zgcPageSpansLock)
	if zgcPageSpansMap != nil {
		if st := zgcPageSpansMap[pg]; st != nil {
			spans = st.spans
		}
	}
	unlock(&zgcPageSpansLock)
	return spans
}

// zgcPageEligible reports whether the freshly selected page pg may enter
// relocation, and fixes what it owns. Selection proved every overlapping
// in-use span movable and inside the page, so the page owns exactly those
// source spans: the rest of the 2 MB region (free pages, goroutine stacks
// and other manual spans) is not its to move or release and stays out of
// its ranges, whatever the mutator or the to-space reservation later puts
// there. prepareSources records the ranges once, before any to-space is
// reserved and before any mutator can see the page. No published foreign
// frame may expose an object anywhere in the region at selection: release
// refuses a pinned region, so registering it would only strand its
// quarantined spans; the registry's exact page query, never a global frame
// count, decides. A page whose spans leave gaps in the region (a free
// page, goroutine stack or other manual span between them) is eligible: it
// owns exactly its ranges and releases exactly them. The world is stopped.
func zgcPageEligible(pg *zgcPage) bool {
	return !zgcForeignPagePinnedSTW(pg.start) && pg.prepareSources(zgcPageSourceSpans(pg))
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
// Discipline: the holder cannot be async-preempted mid-section (holding a
// runtime mutex sets m.locks, which the preemption predicate requires to
// be zero), and the section parks nothing: a range scan only, no channel
// ops, no nested locks, no syscalls, no allocation.
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
	// Discipline: same non-preemptible section as above (m.locks while
	// holding the mutex); the scan parks nothing and allocates nothing.
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
	//
	// Over-mark detection uses stop-the-world bitmap truth only, never
	// the running count. A mark bit may name a slot the allocator never
	// allocated: a sweep that freed the slot can race a concurrent late
	// mark, and a stale heap reference can mark a slot an earlier sweep
	// freed. Such conservatively shaded but unallocated slots are
	// counted as stale; any nonzero stale count is real over-marking and
	// throws below. The running allocCount is deliberately not consulted:
	// every observed deficit decomposes exactly into allocated marks that
	// match allocCount plus stale marks (e.g. live=47 stale=1 alloc=46,
	// live=13 stale=13 alloc=0), so the counter cannot distinguish a
	// real mismatch from a benign sweep/mark race that leaves it one
	// short of stop-the-world truth, as in the old live=53 vs
	// allocCount=52 dump. The bitmap check below is exact either way.
	var live, dead, stale uintptr
	mbits := s.markBitsForBase()
	abits := s.allocBitsForIndex(0)
	for i := uintptr(0); i < uintptr(s.nelems); i++ {
		if mbits.isMarked() {
			live++
			if !(abits.index < uintptr(s.freeindex) || abits.isMarked()) {
				stale++
			}
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
	if stale > 0 {
		print("runtime: nelems=", s.nelems, " live=", live, " stale=", stale, " previous allocCount=", s.allocCount, "\n")
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
	// Pause-only (selection pause): the world is stopped, so no
	// concurrent holder can contend, and the pauser runs non-preemptible.
	// Growth on unretired backlog runs on the system stack, where a
	// triggered collection cannot start (gcStart returns early without a
	// P), so it cannot deadlock against the stopped world.
	lock(&zgcQuarantineLock)
	zgcQuarantine = append(zgcQuarantine, zgcQuarantineEntry{span: s, page: pg})
	unlock(&zgcQuarantineLock)
}

// zgcRetirePendingAppend queues pg for retire. Stop-the-world only. The
// queue shares the zgcMaxPages cap, which the registered list cannot
// exceed, so the append under the lock never allocates and can never
// start a collection the stopped world could not finish. The holder
// cannot be async-preempted mid-section (m.locks while holding the
// mutex), and the section parks nothing.
func zgcRetirePendingAppend(pg *zgcPage) {
	assertWorldStopped()
	lock(&zgcQuarantineLock)
	if len(zgcRetirePending) == cap(zgcRetirePending) {
		throw("zgc: retire queue overflow")
	}
	zgcRetirePending = append(zgcRetirePending, pg)
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
// the page's source ranges, so a later object at a reused address starts
// with a fresh identity and the table does not grow with dead objects.
// It forgets exactly the source ranges, not the 2 MB region: a live object
// the mutator allocated into a gap of the region, or a to-space copy the
// page's own reservation placed there, keeps its identity.
func zgcIdentityForgetRange(pg *zgcPage) {
	t := &zgcIdentity
	lock(&t.lock)
	if t.entries != nil {
		for i := uintptr(0); i <= t.mask; i++ {
			e := t.slot(i)
			if e.addr > zgcIdentTomb && pg.holdsSource(e.addr) {
				e.addr = zgcIdentTomb
				t.live--
			}
		}
	}
	unlock(&t.lock)
}

// zgcUnquarantineSpan drops s from the quarantine set without freeing it.
// The retire path calls it after freeSpanLocked; the refresh path never
// calls it. Pause-only (retire, under mheap_.lock): swap-remove only,
// non-preemptible like every other holder of this lock.
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
// refresh the span would read as sweepable again. Pause-only: atomic
// stores only, non-preemptible like every other holder of this lock.
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
	pages := zgcPagesSnapshot()
	for _, pg := range pages {
		zgcPublishToSpaceSpans(pg)
	}
	zgcScratchPages = zgcScratchPages[:0]
}

// zgcScrubToSpaceMarks enforces mark/alloc consistency on a to-space span
// before it enters the swept sets. Claims mark a slot before its allocation
// metadata completes, and a span's pages can carry older mark state; the
// publication below pins sweepgen without rotating the bits, so a marked-
// but-unallocated slot would read as live at the next selection and trip
// the quarantine stale check, while the sweep's zombie check would fire on
// it first if a sweep ever ran before that selection. Clearing the leak
// here is safe: completed claims carry allocBits and keep their marks, and
// a claim paused mid-protocol (mark set, allocation pending) re-marks
// through zgcClaimSlotBitsUnder on resume. Pause-only: the world is
// stopped, so the bit loops below race with nothing.
func zgcScrubToSpaceMarks(s *mspan) {
	inline := gcUsesSpanInlineMarkBits(s.elemsize)
	for i := uintptr(0); i < uintptr(s.nelems); i++ {
		mb := s.markBitsForIndex(i)
		alloc := i < uintptr(s.freeindex) || s.allocBitsForIndex(i).isMarked()
		if alloc {
			if !mb.isMarked() {
				mb.setMarkedNonAtomic()
			}
			if inline {
				if sc := s.scannedBitsForIndex(i); !sc.isMarked() {
					sc.setMarkedNonAtomic()
				}
			}
			continue
		}
		if mb.isMarked() {
			mb.clearMarked()
		}
		if inline {
			if sc := s.scannedBitsForIndex(i); sc.isMarked() {
				sc.clearMarked()
			}
		}
	}
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
		zgcScrubToSpaceMarks(s)
		atomic.Store(&s.sweepgen, mheap_.sweepgen)
		if s.allocCount == 0 {
			mheap_.central[s.spanclass].mcentral.fullSwept(mheap_.sweepgen).push(s)
		} else {
			mheap_.central[s.spanclass].mcentral.partialSwept(mheap_.sweepgen).push(s)
		}
	}
	// Length zero, capacity kept: the page state returns to the pool
	// with its presized array (see zgcInitPageRegistry).
	lock(&st.mu)
	st.toSpace = st.toSpace[:0]
	unlock(&st.mu)
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
	// The forwarding table and the source spans are what a holder of a
	// source-side address resolves through. A published foreign
	// exposure on the page's region (exact page query, not a frame
	// count) keeps them until its frame ends; the page waits in the
	// retire queue instead of freeing metadata a holder may still read.
	if zgcForeignPagePinnedSTW(pg.start) {
		return false
	}
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
	}
	// zgcPageForget resets the lists to length zero under the entry
	// mutex and keeps the presized backing arrays for the next pause.
	// Nilling them here would hand the pool states with no capacity and
	// make the next selection append (allocate) inside a stopped world.
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
	// In-place filter over the shared buffer: entries kept stay within
	// the capacity the take below vacated, so no append here allocates.
	// The retire loop runs unlocked but stop-the-world exclusive, so no
	// other pause can append while it filters. Pause-only throughout, so
	// every holder is the non-preemptible pauser itself.
	lock(&zgcQuarantineLock)
	pending := zgcRetirePending
	zgcRetirePending = zgcRetirePending[:0]
	unlock(&zgcQuarantineLock)
	for _, pg := range pending {
		if !zgcRetirePage(pg) {
			zgcRetirePending = append(zgcRetirePending, pg)
		}
	}
}
