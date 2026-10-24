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
	zgcInitWorkers()
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
	zgcFinishRelocation()
	zgcPagesLock()
	zgcScratchPages = append(zgcScratchPages[:0], zgcAllPages...)
	pages := zgcScratchPages
	zgcPagesUnlock()
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
			if i < foreignRetained {
				complete = false
				continue
			}
			// ReleasePage refuses while any live byte lacks a
			// published copy; only a released page joins the
			// retire queue, and the flag clears only when no
			// page is left unfinished.
			if !zgcReleasePage(pg) {
				complete = false
				continue
			}
			lock(&zgcQuarantineLock)
			zgcRetirePending = append(zgcRetirePending, pg)
			unlock(&zgcQuarantineLock)
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
}

// zgcMarkStartRoots snapshots every stack before marking begins: each
// thread's held pointers are shaded before it can store them into an
// unscanned object through a plain store. New goroutines start from
// shaded args or black allocations, so the snapshot covers the cycle.
// Stacks keep gcscandone clear so the concurrent markroot pass still
// scans them; shading twice is idempotent.
func zgcMarkStartRoots() {
	if !zgcCycleEnabled() {
		return
	}
	assertWorldStopped()
	systemstack(func() {
		pp := getg().m.p.ptr()
		gcw := &pp.gcw
		curg := getg().m.curg
		forEachG(func(gp *g) {
			switch readgstatus(gp) &^ _Gscan {
			case _Gdead, _Gdeadextra:
				return
			}
			if gp == curg {
				zgcScanSelf(gp, gcw)
				return
			}
			stopped := suspendG(gp)
			if stopped.dead {
				return
			}
			scanstack(gp, gcw)
			resumeG(stopped)
		})
	})
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
	scanstack(gp, gcw)
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
	zgcPagesLock()
	zgcScratchPages = append(zgcScratchPages[:0], zgcAllPages[before:]...)
	pages := zgcScratchPages
	zgcPagesUnlock()
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
		zgcPagesLock()
		zgcScratchPages = append(zgcScratchPages[:0], zgcAllPages...)
		pages := zgcScratchPages
		zgcPagesUnlock()
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
