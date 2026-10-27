// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
)

// Cycle driver for the zgc collector.
//
// Stage 3 schedule (the acceptance shape): marking and copying leave the
// pause. Pauses are mark-start (root snapshot, flip) and mark termination
// (fixpoint tail, selection, registration). Copying runs on background
// workers and the mutator deep path. The next gcStart finishes in-flight
// relocation, rewrites every reachable slot, releases, flips, and
// snapshots before marking.

// zgcMarkParity selects the mark color of the next cycle.
var zgcMarkParity uint32

// zgcRelocating counts the pages of the in-flight relocation.
var zgcRelocating atomic.Int32

// zgcInit probes hardware tagging and configures the color encoding
// before user code runs. ARM64 TBI covers the tag byte; x86 LAM at 7
// bits fails with EINVAL on Linux (LAM_U57 only) and falls back to the
// software encoding. Stage 2/3 run with real colors from here; the
// Z2-asm wrappers remain the documented residual for unaudited assembly,
// and the mode gate stays shut until the parent's runtime probes pass.
func zgcInit() {
	zgcInitPageRegistry()
	zgcInitSelect()
	zgcInitForeignRoots()
	if ok, _ := zgcEnableHardwareTagging(); ok {
		zgcConfigure(zgcEncodingHardware, zgcColorMark0)
		return
	}
	zgcConfigure(zgcEncodingSoftware, zgcColorMark0)
}

// zgcAsmBoundaryComplete reports whether the Z2-asm wrappers landed. The
// assembly audit artifact owns the wrapper set; the Go-side untag and
// canonicalization boundaries land with this lane and the audit residual
// is recorded, not claimed.
func zgcAsmBoundaryComplete() bool {
	return zgcBoundaryWrappersLanded()
}

// zgcFlipMark alternates the good color between mark0 and mark1.
func zgcFlipMark() {
	if zgcMarkParity&1 == 0 {
		zgcFlip(zgcColorMark0)
	} else {
		zgcFlip(zgcColorMark1)
	}
	zgcMarkParity++
}

// zgcGCStartHook finishes in-flight relocation, rewrites every reachable
// slot, releases completed pages, flips the good color, and snapshots
// the stacks before marking begins. The world is stopped.
func zgcGCStartHook() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	// Whole body runs on the system stack. The retained eps-child
	// hang died in this hook's morestack prologue (worker PC on the
	// trampoline back to entry): growing the worker stack inside
	// the STW pause blocks on locks the stopped world denies, and
	// the prologue runs before the cycle gate above could return,
	// so even a disabled cycle strands goroutine 1 in Gosched. g0
	// never grows here. Every callee is already pause-safe;
	// MarkStartRoots' inner systemstack nests as a direct call on
	// g0 by design, channel sends are non-blocking, and the
	// load-barrier behavior is unchanged.
	systemstack(func() {
		zgcFinishRelocation()
		pages := zgcPagesSnapshot()
		live := pages[:0]
		for _, pg := range pages {
			if pg.released.Load() == 0 {
				live = append(live, pg)
			}
		}
		if len(live) == 0 {
			// Nothing unreleased: no relocation is in flight (any
			// in-flight page is registered and unreleased).
			zgcRelocating.Store(0)
		} else {
			// A thread parked in foreign code can name an unpublished
			// object through an untyped argument block the rewrite
			// cannot visit, and the kernel or C follows pointers from
			// that block, including words it snapshotted at entry. Run
			// the transitive closure before the rewrite below: after
			// it, ordinary-heap edges name copies and no longer retain
			// the sources the snapshots still address. The retained
			// prefix of live stays unreleased and the next cycle
			// retries it.
			foreignRetained := zgcForeignRetainLive(live)
			logosWalkAllWith(zgcFixSlot, logosWalkPolicy{skipSpan: zgcSpanQuarantined, useAllocBits: true})
			if debug.gccheckmark > 0 {
				zgcWalkAllLiveForCheck()
			}
			// The running goroutine's own stack was skipped by the
			// precise rewrite. Conservatively pin what it may still
			// address and defer those pages to the next cycle instead
			// of releasing storage it can still name.
			selfPins := zgcNewPinEv()
			defer selfPins.pinned.release()
			zgcPinSelfStack(&selfPins)
			complete := true
			for i, pg := range live {
				// A deferred page is unfinished: it stays
				// unreleased, so relocation is not complete. The
				// next cycle's rewrite heals the typed holders and
				// retries the release.
				if selfPins.pinned.has(pg.start) {
					complete = false
					continue
				}
				if i < foreignRetained || zgcPageHasActiveHold(pg) {
					complete = false
					continue
				}
				// ReleasePage returns exactly the page's prepared
				// source ranges (never the rest of its 2 MB region)
				// and refuses while any live byte lacks a published
				// copy or an exposed object pins the region; only a
				// released page joins the retire queue, and the flag
				// clears only when no page is left unfinished.
				if !zgcReleasePage(pg) {
					complete = false
					continue
				}
				zgcRetirePendingAppend(pg)
			}
			if complete {
				zgcRelocating.Store(0)
			}
		}
		// Flips run only once colors are active: with no relocation ever
		// selected the masks stay zero and every barrier stays identity.
		// The stack snapshot runs every cycle: load-barrier marking shades
		// each thread's held pointers before it can store them through a
		// plain store.
		if len(pages) != 0 || atomic.Loaduintptr(&zgcColorMaskBits) != 0 {
			zgcFlipMark()
		}
		zgcMarkStartRoots()
	})
}

// zgcMarkStartRoots snapshots every stack before marking begins: each
// thread's held pointers are shaded before it can store them into an
// unscanned object through a plain store. New goroutines start from
// shaded args or black allocations, so the snapshot covers the cycle.
// Stacks keep gcscandone clear so the concurrent markroot pass still
// scans them; shading twice is idempotent.
//
// Every object a published foreign frame exposes is a root of this
// cycle too, and C keeps reading it while it runs, so nothing else would
// reach it. zgcMarkForeignRoots forces each one to be scanned even when
// admission already set its mark bit without queueing it. Frames
// admitted after this point mark their whole closure at admission (the
// registry marks every admitted base, in every phase, with the world
// stopped).
//
// Edges stored into an exposed object after its scan rest on the
// invariant every plain store in zgc already rests on, not on a rescan:
// a pointer a mutator can store was loaded through the barrier (which
// greys its target), came from a root scanned here, or is a black
// allocation. Compiled pointer stores stay plain (the compiler emits no
// store barrier under zgc); bulk moves shade their sources through the
// runtime bulk barrier and the sync/atomic pointer operations through
// atomicwb, while marking is active. A mark-termination rescan could not add
// anything: the hook runs after marking has finished with no drain left
// to run, and it cannot see stores made during the sweep window. The
// raw non-barriered pointer stores in this tree (zgcAtomicStorepNoWB and
// zgcAtomicCasp1 callers) all target runtime-owned or off-heap
// metadata, never a user object a frame can expose; a new caller that
// stores into user memory must shade its target itself.
func zgcMarkStartRoots() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	// suspendG throws when the calling M's user goroutine is still
	// _Grunning ("suspendG from non-preemptible goroutine"), so a
	// caller on the system stack can never be suspended back at an
	// unsafe point. gcStart reaches this hook on the triggering
	// goroutine, which is still _Grunning, while the standard
	// collector only ever suspends from a parked caller: mark
	// workers park as waitReasonGCWorkerActive before draining
	// markroot jobs, and gcMarkTermination parks as
	// waitReasonGarbageCollection before the STW mark. Park this
	// goroutine as waitReasonGarbageCollectionScan before entering
	// the system stack for the same reason. The world is stopped,
	// so nothing else runs until it is resumed below.
	curg := getg().m.curg
	casGToWaitingForSuspendG(curg, _Grunning, waitReasonGarbageCollectionScan)
	systemstack(func() {
		pp := getg().m.p.ptr()
		gcw := &pp.gcw
		zgcMarkForeignRoots(gcw)
		forEachG(func(gp *g) {
			switch readgstatus(gp) &^ _Gscan {
			case _Gdead, _Gdeadextra:
				return
			}
			// The caller is parked above, so suspendG's
			// caller-side guard passes even when gp == curg:
			// suspendG then claims the _Gscan bit
			// scanstack requires (markroot follows this
			// same park-then-suspendG shape for self
			// scans). A direct scanstack here would throw
			// "scanstack - bad status" on the parked self
			// stack, which suspendG has not yet marked.
			stopped := suspendG(gp)
			if stopped.dead {
				return
			}
			scanstack(gp, gcw)
			resumeG(stopped)
		})
	})
	casgstatus(curg, _Gwaiting, _Grunning)
}

// zgcScanSelf shades the stack of the running goroutine through the
// self-scan protocol shared with markroot: park it in _Gwaiting so the
// scan sees a stopped thread, then resume it.
func zgcScanSelf(gp *g, gcw *gcWork) {
	userG := getg().m.curg
	if gp != userG || readgstatus(userG) != _Grunning {
		return
	}
	casGToWaitingForSuspendG(userG, _Grunning, waitReasonGarbageCollectionScan)
	castogscanstatus(userG, _Gwaiting, _Gscanwaiting)
	scanstack(gp, gcw)
	casgstatus(userG, _Gscanwaiting, _Gwaiting)
	casgstatus(userG, _Gwaiting, _Grunning)
}

// zgcMarkTerminationHook runs the relocation phase opening in the
// mark-termination pause: retire the previous generation, refresh
// deferred spans, select and register pages, reserve to-space, fix the
// roots into the relocation set, flip the good color, and start the
// background workers. No object copying happens in the pause.
func zgcMarkTerminationHook() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	// Whole pause runs on the system stack: an STW pause must not grow
	// the worker stack; g0 needs no growth.
	systemstack(func() {
		zgcRemapPassComplete()
		zgcRetirePendingPages()
		zgcPublishReadyToSpace()
		zgcPagesLock()
		before := len(zgcAllPages)
		zgcPagesUnlock()
		n := zgcSelectPages(0)
		if n == 0 {
			return
		}
		pages := zgcPagesSnapshot()[before:]
		// Vet every selected page before any to-space is reserved: a
		// reservation allocates spans, and an ineligible page must not be
		// able to receive one into a gap the vet is about to rule out.
		// A page that fails the vet owns no to-space yet, so discarding it
		// is forget plus free; zgcAbandonPage would also nil the state's
		// presized span lists, which the next pause's selection appends to.
		eligible := pages[:0]
		for _, pg := range pages {
			if !zgcPageEligible(pg) {
				zgcPageForget(pg)
				zgcFreePage(pg)
				continue
			}
			eligible = append(eligible, pg)
		}
		pages = eligible
		kept := pages[:0]
		for _, pg := range pages {
			if !zgcReservePageToSpace(pg) {
				zgcAbandonPage(pg)
				continue
			}
			for _, s := range zgcPageState(pg).spans {
				zgcQuarantineSpan(s, pg)
			}
			kept = append(kept, pg)
		}
		if len(kept) == 0 {
			return
		}
		zgcFixRoots(zgcFixSlot)
		zgcFlip(zgcColorRemapped)
		zgcRelocating.Store(int32(len(kept)))
		zgcWakeWorkers()
	})
}

// zgcRemapPassComplete records that a remap pass finished: a full mark
// that loaded every reachable slot through the barrier, healing every
// stale word from a page released before it began. Only the collector
// calls it.
func zgcRemapPassComplete() {
	zgcRemapEpoch.Add(1)
}

// zgcFinishRelocation helps in-flight relocation to completion while the
// world is stopped: every page reaches published == live before the
// rewrite, so no mutator ever receives a source address of a released
// page.
func zgcFinishRelocation() {
	if zgcRelocating.Load() == 0 {
		return
	}
	assertWorldStopped()
	for {
		progress := false
		pages := zgcPagesSnapshot()
		for _, pg := range pages {
			if pg.published.Load() == pg.live {
				continue
			}
			if pg.published.Load() != pg.live && zgcEvacuatePage(pg) {
				progress = true
			}
		}
		if !progress {
			return
		}
	}
}

// zgcAllPages tracks the registered pages for gcStart-driven completion.
// The directory stays the resolution path; this list only drives helping.
var (
	zgcPagesMu  mutex
	zgcAllPages []*zgcPage
)

func zgcPagesLock()   { lock(&zgcPagesMu) }
func zgcPagesUnlock() { unlock(&zgcPagesMu) }

// zgcPagesSnapshot returns the registered pages in zgcScratchPages.
// Stop-the-world only. The scratch shares the zgcMaxPages cap, which the
// registered list cannot exceed, so the append under the lock never
// allocates and can never start a collection the stopped world could
// not finish. The holder cannot be async-preempted mid-section (holding
// a runtime mutex sets m.locks, which the preemption predicate requires
// to be zero), and the section parks nothing: no channel ops, no nested
// locks, no syscalls.
func zgcPagesSnapshot() []*zgcPage {
	assertWorldStopped()
	zgcPagesLock()
	if cap(zgcScratchPages) < len(zgcAllPages) {
		throw("zgc: page snapshot overflow")
	}
	zgcScratchPages = append(zgcScratchPages[:0], zgcAllPages...)
	pages := zgcScratchPages
	zgcPagesUnlock()
	return pages
}
