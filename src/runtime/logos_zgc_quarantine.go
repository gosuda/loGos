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
}

var (
	zgcPageSpansLock     mutex
	zgcPageStates        []*zgcPageSpans
	zgcStateHoles        []uintptr
	zgcStateFree         [128]*zgcPageSpans
	zgcStateFreeN        int
	zgcEmptyPageSpans    zgcPageSpans
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
	if zgcPageStates != nil {
		return
	}
	zgcPageStates = make([]*zgcPageSpans, 0, len(zgcStateFree))
	zgcStateHoles = make([]uintptr, 0, len(zgcStateFree))
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

// zgcPageState returns the span state of pg, creating a pooled one when
// the page has none. The registry lock covers only the lookup and the
// creation: the returned state stays valid because reclamation gates
// zgcPageForget — the only pool return — on refs==0 and a drained
// acquisition domain. A caller holding a lease on pg therefore holds
// the state for the whole lease; STW callers are protected by the
// world stop instead.
func zgcPageState(pg *zgcPage) *zgcPageSpans {
	if pg == nil {
		return &zgcEmptyPageSpans
	}
	lock(&zgcPageSpansLock)
	if zgcPageStates == nil {
		unlock(&zgcPageSpansLock)
		return &zgcEmptyPageSpans
	}
	if i := pg.stateIdx; i != 0 {
		st := zgcPageStates[i-1]
		unlock(&zgcPageSpansLock)
		return st
	}
	var st *zgcPageSpans
	if zgcStateFreeN != 0 {
		zgcStateFreeN--
		st = zgcStateFree[zgcStateFreeN]
		zgcStateFree[zgcStateFreeN] = nil
	} else {
		st = &zgcPageSpans{
			spans:   make([]*mspan, 0, 256),
			toSpace: make([]*mspan, 0, 256),
		}
	}
	if n := len(zgcStateHoles); n != 0 {
		i := zgcStateHoles[n-1]
		zgcStateHoles = zgcStateHoles[:n-1]
		zgcPageStates[i-1] = st
		pg.stateIdx = i
	} else {
		pg.stateIdx = uintptr(len(zgcPageStates)) + 1
		zgcPageStates = append(zgcPageStates, st)
	}
	unlock(&zgcPageSpansLock)
	return st
}

// zgcPageStateIfAny returns the span state of pg without creating one,
// or nil. Gate reader for the stop-the-world reclaim path, where the
// world stop keeps the header and the registry stable and creating a
// state would be a defect, not a service.
func zgcPageStateIfAny(pg *zgcPage) *zgcPageSpans {
	if pg == nil {
		return nil
	}
	lock(&zgcPageSpansLock)
	var st *zgcPageSpans
	if zgcPageStates != nil {
		if i := pg.stateIdx; i != 0 {
			st = zgcPageStates[i-1]
		}
	}
	unlock(&zgcPageSpansLock)
	return st
}

// zgcPageSourceSpans returns the source spans recorded for pg without
// creating a state entry for a page that has none. The list changes only
// in pauses (selection, abandon, retire), so the header read here stays
// valid for the pause that asked for it, or for a caller holding a
// lease on pg: the retire path that would free the span metadata gates
// on refs==0 after the acquisition domain drained.
func zgcPageSourceSpans(pg *zgcPage) []*mspan {
	if pg == nil {
		return nil
	}
	lock(&zgcPageSpansLock)
	var spans []*mspan
	if i := pg.stateIdx; i != 0 {
		spans = zgcPageStates[i-1].spans
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
	if i := pg.stateIdx; i != 0 {
		st := zgcPageStates[i-1]
		// The slot becomes a hole instead of a swap-remove: a page
		// keeps its index for as long as it owns a state, so no other
		// page's index is ever rewritten.
		zgcPageStates[i-1] = nil
		zgcStateHoles = append(zgcStateHoles, i)
		pg.stateIdx = 0
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
// never the registry lock alone, so a concurrent retire cannot tear the
// list mid-scan.
func zgcToSpaceBypass(s *mspan) bool {
	if s == nil || atomic.Load(&s.sweepgen) != mheap_.sweepgen+2 {
		return false
	}
	// Discipline: same non-preemptible section as above (m.locks while
	// holding the mutex); the scan parks nothing and allocates nothing.
	lock(&zgcPageSpansLock)
	defer unlock(&zgcPageSpansLock)
	for _, st := range zgcPageStates {
		if st == nil {
			continue
		}
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

// Per-span reservation ledger for the quarantine stale-mark hunt.
//
// Established: failing source spans arrive with every mark bit set and a
// running count far below the mark total (e.g. nelems=126 live=126
// stale=26 allocCount=100, and nelems=45 live=18 stale=17 allocCount=1,
// both with freeindex=0), no marking funnel records a mark on them, and a
// process-wide reserved/committed/unclaimed tally always cancels out
// (reserved equals committed in every run). The global tally cannot
// discriminate: a balanced total hides a per-span leak the same way a
// matching allocCount once hid a bitmap-level mismatch. This ledger keys
// the same three events by span base so the failure path can print the
// history of exactly the span it is about to judge.
//
// Events: destination role (each recorded at its single choke point in
// logos_zgc_evac.go) and source role (recorded in zgcCopyOneResult,
// which owns the claim and sees both the source slot and its outcome,
// so no plumbing threads the source through the claim path):
//
//	reserve: zgcReserveToSpace returned a span (destination role). Covers
//	  both the pause pre-reserve (zgcReservePageToSpace) and refill
//	  claims: both funnel through it. One reserve per mheap grant.
//	commit: zgcClaimSlotBitsUnder joined a destination slot to the count
//	  (fresh, allocCount++). The refill slot-0 claim and every scan claim
//	  finish here, so every committed slot is counted.
//	unclaim: zgcUnclaimOne dropped a destination slot's allocation
//	  (allocCount--). Unclaims that only clear a mark (no alloc bit set)
//	  move no running count and are not counted.
//	srcReserve: one slot was claimed to copy an object of the source span
//	  (zgcClaimOne succeeded in zgcCopyOneResult). One per claimed slot,
//	  keyed by the source, not the destination.
//	srcCommit: that copy won publication (zgcRelocateObject returned the
//	  claimed address).
//	srcUnclaim: that copy lost publication or was refused, and its slot
//	  was released through zgcUnclaimOne.
//
// Every claimed slot resolves exactly once on the same thread (win feeds
// srcCommit, loss or refusal feeds srcUnclaim), so the source tally is an
// exact control-flow invariant, untouched by sweep or mutator counts.
//
// Debug gate: zgcSpanLedgerDebug is false in normal builds, so every call
// site of the form
//
//	if zgcSpanLedgerDebug { ... }
//
// compiles away byte-for-byte and adds no per-claim cost. Flip it to true
// in a debug build to record and dump. The throw below stays on either
// way: the ledger only annotates the failure, it never downgrades it.
//
// Reading the dump (failure path only, next to the per-slot bitmap dump).
// The as-source tally is exact: the world is stopped at the dump, so no
// claim is in flight, and sweep/mutator activity never touches it.
//
//	as-source CLOSING (srcReserved == srcCommitted + srcUnclaimed):
//	  every claim made for this span's objects resolved exactly once, so
//	  the evacuation bookkeeping for the judged span balanced; the extra
//	  marks did not come from an unresolved claim for this span.
//	as-source OPEN: a claim for this span's objects never resolved (or
//	  resolved without a claim): the defect is in the claim/commit/
//	  unclaim control flow for the judged span itself.
//	The destination tally is the span's prior to-space life at this
//	address (absent if it never served as to-space): dst CLOSING
//	(committed minus unclaimed equals the running allocCount at the same
//	nelems) means the claim books from that life balanced and the extra
//	marks sit on slots the claim protocol never committed: a
//	stray-marking defect, not a claim leak. dst OPEN means that prior
//	life leaked or double counted: the defect is in the
//	reservation/commit/unclaim path at this address.
//
// Caveats, read before over-claiming: entries are cumulative per base. A
// retired source clears its entry in zgcRetirePage, but a span freed by
// the ordinary sweep or by the abandon path keeps its counters, so an
// address reused across frees merges incarnations (each mheap grant adds
// one reserve; the dump prints the nelems seen at the last event beside
// the current nelems so a size-class change flags the merge). Past
// publication the mutator can allocate free slots and a sweep can free
// dead ones, both moving allocCount outside the ledger, so on a span with
// sweep or post-publication history (freeindex != 0) only a closing
// ledger is conclusive and a non-closing one needs the reuse check first.
// The hunt's failing spans all show freeindex=0, i.e. no bump-pointer or
// sweep history, so for them the dst equality is exact either way.
const zgcSpanLedgerDebug = false

const zgcSpanLedgerCap = 1024

type zgcSpanLedgerEntry struct {
	base         uintptr
	nelems       uintptr
	reserved     uintptr
	committed    uintptr
	unclaimed    uintptr
	srcReserved  uintptr
	srcCommitted uintptr
	srcUnclaimed uintptr
}

var (
	zgcSpanLedger         [zgcSpanLedgerCap]zgcSpanLedgerEntry
	zgcSpanLedgerN        int
	zgcSpanLedgerOverflow bool
	zgcSpanLedgerLock     mutex
)

// zgcSpanLedgerAdd records one event for the span at base: destination
// role reserve (kind 0), commit (kind 1) or unclaim (kind 2), or source
// role reserve (kind 3), commit (kind 4) or unclaim (kind 5). Leaf: takes only the ledger
// lock, never nests under or over any other lock (every call site runs
// with no other lock held), allocates nothing, and stores no pointers,
// so it is safe from claim, reserve, retire-clear and pause paths alike.
// On a full table it sets the overflow flag and drops the event rather
// than throwing: a debug hunt must not die on bookkeeping.
func zgcSpanLedgerAdd(base, nelems uintptr, kind uintptr) {
	lock(&zgcSpanLedgerLock)
	eid := -1
	for i := 0; i < zgcSpanLedgerN; i++ {
		if zgcSpanLedger[i].base == base {
			eid = i
			break
		}
	}
	if eid < 0 {
		if zgcSpanLedgerN == zgcSpanLedgerCap {
			zgcSpanLedgerOverflow = true
			unlock(&zgcSpanLedgerLock)
			return
		}
		eid = zgcSpanLedgerN
		zgcSpanLedgerN++
		zgcSpanLedger[eid].base = base
	}
	zgcSpanLedger[eid].nelems = nelems
	switch kind {
	case 0:
		zgcSpanLedger[eid].reserved++
	case 1:
		zgcSpanLedger[eid].committed++
	case 2:
		zgcSpanLedger[eid].unclaimed++
	case 3:
		zgcSpanLedger[eid].srcReserved++
	case 4:
		zgcSpanLedger[eid].srcCommitted++
	case 5:
		zgcSpanLedger[eid].srcUnclaimed++
	}
	unlock(&zgcSpanLedgerLock)
}

// zgcSpanLedgerReserve records one mheap grant of s as to-space.
func zgcSpanLedgerReserve(s *mspan) {
	zgcSpanLedgerAdd(s.base(), uintptr(s.nelems), 0)
}

// zgcSpanLedgerCommit records one claim-protocol commit of ds.
func zgcSpanLedgerCommit(ds *mspan) {
	zgcSpanLedgerAdd(ds.base(), uintptr(ds.nelems), 1)
}

// zgcSpanLedgerUnclaim records one claim-protocol unclaim of ds.
func zgcSpanLedgerUnclaim(ds *mspan) {
	zgcSpanLedgerAdd(ds.base(), uintptr(ds.nelems), 2)
}

// zgcSpanLedgerSrcReserve records one slot claimed to copy an object of
// the source span s.
func zgcSpanLedgerSrcReserve(s *mspan) {
	zgcSpanLedgerAdd(s.base(), uintptr(s.nelems), 3)
}

// zgcSpanLedgerSrcCommit records one winning publication of a copy of s.
func zgcSpanLedgerSrcCommit(s *mspan) {
	zgcSpanLedgerAdd(s.base(), uintptr(s.nelems), 4)
}

// zgcSpanLedgerSrcUnclaim records one lost or refused copy of s whose
// slot was released.
func zgcSpanLedgerSrcUnclaim(s *mspan) {
	zgcSpanLedgerAdd(s.base(), uintptr(s.nelems), 5)
}

// zgcSpanLedgerClear drops the entry for base when its incarnation ends
// at retire, so a later span reusing the address starts clean. Called
// after the heap lock is released, holding no other lock.
func zgcSpanLedgerClear(base uintptr) {
	lock(&zgcSpanLedgerLock)
	for i := 0; i < zgcSpanLedgerN; i++ {
		if zgcSpanLedger[i].base == base {
			zgcSpanLedgerN--
			zgcSpanLedger[i] = zgcSpanLedger[zgcSpanLedgerN]
			zgcSpanLedger[zgcSpanLedgerN] = zgcSpanLedgerEntry{}
			break
		}
	}
	unlock(&zgcSpanLedgerLock)
}

// zgcSpanLedgerDump prints the ledger history of the span quarantine is
// about to reject. Failure path only: the world is stopped and the caller
// holds no locks. The entry is copied under the lock and printed after
// it, so the leaf lock never spans a print.
func zgcSpanLedgerDump(s *mspan) {
	lock(&zgcSpanLedgerLock)
	found := false
	overflow := zgcSpanLedgerOverflow
	var e zgcSpanLedgerEntry
	for i := 0; i < zgcSpanLedgerN; i++ {
		if zgcSpanLedger[i].base == s.base() {
			e = zgcSpanLedger[i]
			found = true
			break
		}
	}
	unlock(&zgcSpanLedgerLock)
	if !found {
		print("  span ledger: absent for base=", hex(s.base()), " (no claim activity recorded for this span)\n")
		if overflow {
			print("  span ledger: table overflowed, absence inconclusive\n")
		}
		return
	}
	print("  span ledger as-dest: base=", hex(e.base), " nelemsAtEvent=", e.nelems, " reserved=", e.reserved, " committed=", e.committed, " unclaimed=", e.unclaimed, " allocCount=", s.allocCount, "\n")
	if overflow {
		print("  span ledger: table overflowed, counts may be short\n")
	}
	if e.reserved+e.committed+e.unclaimed == 0 {
		print("  span ledger as-dest: absent (span never served as to-space)\n")
	} else if e.nelems != uintptr(s.nelems) {
		print("  span ledger as-dest: OPEN (nelems changed since the recorded events: address reuse across frees)\n")
	} else if e.unclaimed > e.committed {
		print("  span ledger as-dest: OPEN (more unclaims than commits: double unclaim at this address)\n")
	} else if e.committed-e.unclaimed != uintptr(s.allocCount) {
		print("  span ledger as-dest: OPEN (committed-unclaimed=", e.committed-e.unclaimed, " != allocCount=", s.allocCount, ")\n")
	} else {
		print("  span ledger as-dest: CLOSING (prior to-space books balance; extra marks sit on slots the claim protocol never committed)\n")
	}
	print("  span ledger as-source: reserved=", e.srcReserved, " committed=", e.srcCommitted, " unclaimed=", e.srcUnclaimed, "\n")
	if e.srcReserved != e.srcCommitted+e.srcUnclaimed {
		print("  span ledger as-source: OPEN (a claim for this span's objects never resolved: defect in the claim/commit/unclaim control flow)\n")
	} else {
		print("  span ledger as-source: CLOSING (every claim for this span resolved exactly once)\n")
	}
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
	// Invariant zgcQuarantineMarksSubsetAllocs: every mark bit on a
	// quarantined source span names an allocated slot (index < freeindex
	// or alloc bit set). The loop below checks exactly this bitmap truth
	// and stays fail-loud: any nonzero stale count throws.
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
		// Print the counts and the first stale slots before throwing:
		// this fires only on failure and is what names whether a stale
		// entry is a mark on memory the allocator never handed out.
		print("runtime: nelems=", s.nelems, " live=", live, " stale=", stale, " previous allocCount=", s.allocCount, " freeindex=", s.freeindex, " base=", hex(s.base()), "\n")
		// Show the first stale slots with their own bitmap state. A
		// stale slot whose alloc bit is clear is a mark on memory the
		// allocator never handed out; a stale slot whose index merely
		// sits above freeindex while its alloc bit is clear is the same
		// thing, but the pair separates that from an index watermark
		// that never advanced.
		mbits = s.markBitsForBase()
		abits = s.allocBitsForIndex(0)
		for i, shown := uintptr(0), 0; i < uintptr(s.nelems) && shown < 10; i++ {
			if mbits.isMarked() && !(abits.index < uintptr(s.freeindex) || abits.isMarked()) {
				print("  stale slot idx=", i, " allocBit=", abits.isMarked(), " belowFreeindex=", i < uintptr(s.freeindex), "\n")
				shown++
			}
			mbits.advance()
			abits.advance()
		}
		if zgcSpanLedgerDebug {
			zgcSpanLedgerDump(s)
		}
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
// preallocated queue holds every deferred header until its metadata free
// actually ran: pages awaiting their source-retire gates, and unpublished
// descriptors whose drain sample failed. Every descriptor keeps ownership
// of its directory granule for its whole pending life — a released page
// still has its header in the slot, while an unpublished descriptor has
// the retiring tombstone — so selection cannot create a second descriptor
// for that granule. Thus the queue has at most one entry per granule and
// cannot exceed its zgcMaxPages capacity; a full queue is a thrown
// invariant, never a silent leak. The holder cannot be async-preempted
// mid-section (m.locks while holding the mutex), and the section parks
// nothing.
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
// The retire path calls it after freeSpanLocked, under mheap_.lock and on
// the system stack; the refresh path never calls it. It stays inside that
// window so a freed span is never left in the set for a later
// zgcRefreshDeferredSpans to stamp. Swap-remove only; the lock is a leaf,
// so taking it under mheap_.lock is legal and no holder nests the other
// way.
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
// spans at every pause while their page awaits retire, and gcSweep calls
// it immediately after its sweepgen bump, so the stamp holds
// sweepgen == mheap_.sweepgen+2 at every reader across the whole cycle,
// not only between pauses. Pause-only: atomic stores only,
// non-preemptible like every other holder of this lock.
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

// zgcEnsureDestRegistered ensures the dest slot of a winning commit stays
// marked. It never creates an allocation: free slots are left alone, so
// held test spans keep allocCount zero and free cleanly.
func zgcEnsureDestRegistered(to uintptr) {
	s := spanOfHeap(to)
	if s == nil || s.state.get() != mSpanInUse || s.isUserArenaChunk {
		return
	}
	if s.allocBits == nil {
		return
	}
	inline := gcUsesSpanInlineMarkBits(s.elemsize)
	if !inline && s.gcmarkBits == nil {
		return
	}
	idx := s.objIndex(to)
	if idx >= uintptr(s.nelems) || s.base()+idx*s.elemsize != to {
		return
	}
	if idx >= uintptr(s.freeindex) && !s.allocBitsForIndex(idx).isMarked() {
		return
	}
	if !s.markBitsForIndex(idx).isMarked() {
		s.markBitsForIndex(idx).setMarked()
	}
	if inline {
		if sc := s.scannedBitsForIndex(idx); !sc.isMarked() {
			sc.setMarked()
		}
	}
}

// zgcPublishToSpaceSpans pushes the filled to-space spans of pg onto the
// swept span sets with the current sweepgen, exactly like freshly swept
// spans: after the bump the sweeper claims each span, frees nothing, and
// rotates the bits.
//
// Lease gate: a page with a live copy or selection lease keeps its
// destination list this pause. Publication scrubs and pushes spans a
// still-running claim protocol may not have finished, so a leased page
// is skipped whole and its deferred list retained for the next pause;
// retire is likewise blocked until the list drains.
func zgcPublishToSpaceSpans(pg *zgcPage) {
	if !zgcCycleEnabled() || pg == nil {
		return
	}
	assertWorldStopped()
	if pg.refs.Load() != 0 {
		return
	}
	st := zgcPageState(pg)
	if st == nil || st == &zgcEmptyPageSpans {
		return
	}
	// Detach under the entry mutex so a concurrent forget cannot tear
	// the list, and reset the cursor with it: the detached snapshot is
	// what this pause publishes, and the page starts a fresh private
	// list on its next reservation. No seal and no counter reset: the
	// empty list is exactly the state a fresh refill would create.
	lock(&st.mu)
	if pg.refs.Load() != 0 {
		unlock(&st.mu)
		return
	}
	pending := st.toSpace[:len(st.toSpace):len(st.toSpace)]
	st.toSpace = st.toSpace[:0]
	st.cursor = 0
	unlock(&st.mu)
	// Scrub and push outside the entry mutex, still stop-the-world: no
	// refill can append into the reset list before the push below
	// finishes, so the snapshot cannot alias a live reservation.
	for _, s := range pending {
		if s == nil || s.state.get() != mSpanInUse {
			continue
		}
		zgcScrubToSpaceMarks(s)
		atomic.Store(&s.sweepgen, mheap_.sweepgen)
		// A destination span with no free slot left belongs on fullSwept.
		// cacheSpan pops partialSwept expecting a free object and throws
		// "span has no free objects" when there is none, which is what a
		// to-space span filled to nelems would do. The ordinary sweep
		// makes the same split on nalloc == nelems.
		if s.allocCount == 0 || s.allocCount == s.nelems || s.freeindex == s.nelems {
			mheap_.central[s.spanclass].mcentral.fullSwept(mheap_.sweepgen).push(s)
		} else {
			mheap_.central[s.spanclass].mcentral.partialSwept(mheap_.sweepgen).push(s)
		}
	}
}

// zgcRetirePage retires a released page once a remap pass completed after
// its release, and reports whether it did. Reclamation is split:
// unpublish first — the descriptor leaves the registered list and its
// slot becomes a retiring tombstone, so no new transient reader can
// resolve the page while the granule remains reserved — then the drain
// sample. An open acquisition domain or a live lease requeues the
// descriptor with every piece of metadata intact: the source bytes are
// already madvised by the earlier release, but the mspan metadata, state
// and header are still read by transient remap readers and lease holders,
// and refs==0 alone does not exclude a transient reader. Only a drained
// page frees anything: its source spans (assert allocCount zero, settle
// the sweepgen, free through the ordinary freeSpanLocked mSpanInUse arm,
// pagesInUse decrements once there), then the state pool return, the
// identity forget and the header mapping. Re-entry is idempotent: the
// tombstoned descriptor re-samples the gates and finds its tombstone and
// no list entry.
//
// The free and the quarantine withdrawal run under systemstack, so the
// function is answerable from an ordinary goroutine (the LogosZgcRetire
// test seam) as well as from the pause, which is already on g0 and pays
// no switch for the nesting. The vet, the page forget, the identity
// forget and the metadata free stay on the caller's stack.
func zgcRetirePage(pg *zgcPage) bool {
	// An unreleased page that was not intentionally abandoned is an
	// ordinary live descriptor: retire reports false and leaves it and
	// its source spans exactly as they are.
	if pg.released.Load() == 0 && !pg.abandoned {
		return false
	}
	// An abandoned page — its to-space reservation failed in the pause
	// that registered it, or the selection vet refused it — never
	// released: no remap epoch to await, no foreign-published
	// relocation, no quarantined source spans, and no drained-destination
	// requirement: its reservation drains as part of the reclaim below.
	abandoned := pg.abandoned
	if !abandoned && zgcRemapEpoch.Load() == pg.releaseEpoch {
		return false
	}
	assertWorldStopped()
	// The forwarding table and the source spans are what a holder of a
	// source-side address resolves through. A published foreign
	// exposure on the page's region (exact page query, not a frame
	// count) keeps them until its frame ends; the page waits in the
	// retire queue instead of freeing metadata a holder may still read.
	if !abandoned && zgcForeignPagePinnedSTW(pg.start) {
		return false
	}
	// A live lease (worker, pinner, attach, self-heal) blocks the whole
	// attempt before anything is unpublished.
	if pg.refs.Load() != 0 {
		return false
	}
	// Publish-before-retire, released pages only: a deferred destination
	// list keeps the page queued until the pause that publishes it
	// drains the list, so the spans never outlive their listed
	// destination state. An abandoned descriptor skips this gate — its
	// reservation is reclaimed, not required to be published first.
	if !abandoned {
		if st := zgcPageStateIfAny(pg); st != nil && st != &zgcEmptyPageSpans && len(st.toSpace) != 0 {
			return false
		}
	}
	// Unpublish: from here on, fresh lookups see a retiring miss and no
	// reader can gain a new lease on the page; its granule remains reserved
	// until the eventual metadata free clears the tombstone.
	zgcPageUnpublish(pg)
	// Drain sample. Transient readers (signal and no-P remap) announce
	// through the acquisition domain, leases through refs; neither is
	// held stable by the world stop — signal readers run during it — so
	// both are sampled explicitly, unpublish first so no new domain can
	// open against this page. A failed sample requeues the intact
	// descriptor; the next pause retries from the idempotent gates.
	if zgcPageAcqDomain.Load() != 0 || pg.refs.Load() != 0 {
		return false
	}
	if abandoned {
		zgcAbandonDrain(pg)
		return true
	}
	spans := zgcPageSourceSpans(pg)
	// Vet every span before the first free, so a page that still has a
	// live allocation retires nothing at all, not even partially. A nil
	// entry is a broken list, not a span to skip: zgcReservePageToSpace
	// already dereferences every entry of the same list, and a skip here
	// would hide the defect while freeing the rest of the page.
	for _, s := range spans {
		if s == nil {
			throw("zgc: retiring a page with a nil source span")
		}
		if s.allocCount != 0 {
			throw("zgc: retiring a span with live allocations")
		}
	}
	// The spans go back to the page allocator on the system stack, and the
	// quarantine entry leaves in the same window. The reason is the heap
	// lock, not the world stop: mheap_.lock may only be acquired on g0,
	// because growing a g stack while holding it self-deadlocks, and both
	// pageAlloc.free and freeMSpanLocked are //go:systemstack. That holds
	// on every path, including the LogosZgcRetire test seam, which calls
	// here with the world running. The settle belongs inside because
	// freeSpanLocked requires s.sweepgen == mheap_.sweepgen at the free.
	// The withdrawal belongs inside because an entry that outlived the heap
	// lock could name a span the allocator handed out again:
	// zgcRefreshDeferredSpans below writes sweepgen through every entry
	// without consulting the span, so it would stamp a live span with
	// mheap_.sweepgen+2 and the sweep guard would then skip it. Holding the
	// heap lock over both keeps the entry unobservable: no allocator can
	// reuse the span before it is withdrawn.
	//
	// Nothing else belongs in here. No allocation and no growth-triggering
	// call: g0 is fixed size, and a collection triggered on it cannot be
	// recovered from. No lock beyond mheap_.lock and zgcQuarantineLock,
	// which is a leaf, so the order is legal; the throw path, the page
	// forget, the identity forget and zgcFreePage stay outside.
	systemstack(func() {
		lock(&mheap_.lock)
		for _, s := range spans {
			atomic.Store(&s.sweepgen, mheap_.sweepgen)
			mheap_.freeSpanLocked(s, spanAllocHeap)
			zgcUnquarantineSpan(s)
		}
		unlock(&mheap_.lock)
	})
	// The ledger entry for a retired source ends with its incarnation:
	// clearing here (past the heap lock, holding no other lock) keeps a
	// later span reusing the address from merging histories. Spans freed
	// by the ordinary sweep or the abandon path keep their counters; see
	// the ledger caveats above.
	if zgcSpanLedgerDebug {
		for _, s := range spans {
			zgcSpanLedgerClear(s.base())
		}
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

// zgcRetirePendingDrain retires or requeues every queued descriptor and
// is the one pending-queue consumer: the pause reaches it after the
// refresh rule, and the lifetime test seam reaches it directly. An
// entry whose gates or drain sample fail is appended back intact — the
// unpublished descriptor keeps its source spans, state and header until
// a drained pass frees it, exactly once.
func zgcRetirePendingDrain() {
	assertWorldStopped()
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

// zgcRetirePendingPages retires every released page whose remap pass has
// completed. It runs in the pause, after the refresh rule.
func zgcRetirePendingPages() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	zgcRefreshDeferredSpans()
	zgcRetirePendingDrain()
}
