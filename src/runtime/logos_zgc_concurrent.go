// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
)

// Concurrent mark and relocation deep paths.
//
// zgcBarrierMark shades a load target during _GCmark: every deep-path
// load that heals or resolves a word also greys the target through the
// current mark assist. Plain stores stay sound without a write barrier:
// a stored pointer was produced by a barriered load (shaded), comes from
// a root (scanned), or is a fresh black allocation. Bulk copies shade
// their sources through the write-barrier move path and the runtime bulk
// barrier while marking is active.
//
// zgcBarrierRelocate lets a loading thread relocate an unpublished object
// itself from its page's to-space reservation instead of waiting for a
// worker. Publication reuses the zgcRelocateObject CAS protocol, so two
// threads racing one unpublished object keep one copy and the loser
// unclaims its slot.

// zgcPToSpaceCache is a bump cursor into one reserved to-space span. The
// reservation happens off the mutator path (mark-termination pause or a
// preemptible refill); the bump itself is lock-free and allocation-free.
// Caches live in the page registry, one per span class, serialized by the
// page mutex.
type zgcPToSpaceCache struct {
	span *mspan
	next uintptr
}

// zgcRefillPToSpace reserves a fresh to-space span of class sc for pg and
// installs it as the page's cache. Allocation runs outside the page
// mutex; only the list install holds it. It runs in preemptible context
// only; nosplit and signal contexts never reach it.
func zgcRefillPToSpace(pg *zgcPage, sc spanClass) *zgcPToSpaceCache {
	ns := zgcReserveToSpace(sc, pageSize)
	if ns == nil {
		return nil
	}
	st := zgcPageState(pg)
	lock(&st.mu)
	st.toSpace = append(st.toSpace, ns)
	unlock(&st.mu)
	return &zgcPToSpaceCache{span: ns}
}

// zgcCanRelocateDeep reports whether the current context may run the
// relocation deep path: preemptible, with a P, off the g0 and signal
// stacks. Nosplit and signal contexts get the source address instead;
// the source stays valid until release, so reads stay sound and only
// racing writes are residual.
func zgcCanRelocateDeep() bool {
	gp := getg()
	mp := gp.m
	if mp == nil || mp.locks != 0 {
		return false
	}
	if gp == mp.g0 || gp == mp.gsignal {
		return false
	}
	if mp.p.ptr() == nil {
		return false
	}
	return true
}

// zgcBarrierMark greys the load target during _GCmark. It runs in the
// barrier deep path after the word resolved to a heap object.
func zgcBarrierMark(target uintptr) {
	if gcphase != _GCmark {
		return
	}
	mp := getg().m
	if mp == nil || mp.p.ptr() == nil {
		return
	}
	gcw := &mp.p.ptr().gcw
	if obj, span, objIndex := findObject(target, 0, 0); obj != 0 {
		greyobject(obj, 0, 0, span, gcw, objIndex)
	}
}

// zgcBarrierRelocate relocates the object holding addr on the registered
// page pg and returns its copy. Claims serialize on the page mutex inside
// the copy path; the copy and publication run outside it. It reports
// false when addr names no relocatable object.
func zgcBarrierRelocate(pg *zgcPage, addr uintptr) (uintptr, bool) {
	if pg == nil || !zgcCanRelocateDeep() {
		return 0, false
	}
	return zgcRelocateOneLocked(pg, addr)
}

// zgcRelocateOneLocked copies the single object holding addr into the
// page's to-space reservation and publishes it. Large spans are pinned,
// never copied.
func zgcRelocateOneLocked(pg *zgcPage, addr uintptr) (uintptr, bool) {
	s := spanOfHeap(addr)
	if s == nil {
		return 0, false
	}
	if s.spanclass.sizeclass() == 0 {
		return zgcRelocateLargeLocked(pg, s)
	}
	idx := s.objIndex(addr)
	base := s.base() + idx*s.elemsize
	if to, ok := pg.forward(base); ok {
		return to + (addr - base), true
	}
	if won := zgcCopyOne(pg, s, base, s.elemsize); won != 0 {
		return won + (addr - base), true
	}
	return 0, false
}

// zgcRelocateLargeLocked publishes the large object of span s without
// copying: large objects never move, and the entry pins the address.
func zgcRelocateLargeLocked(pg *zgcPage, s *mspan) (uintptr, bool) {
	base := s.base()
	if to, ok := pg.forward(base); ok {
		return to, true
	}
	return base, true
}

// zgcReservePageToSpace pre-reserves one to-space span per source span of
// the page, off the mutator path in the pause. Reserved spans take the
// pre-bump sweepgen so the sweeper treats them as swept until the cycle
// that fills them publishes them onto the swept sets. Failure abandons
// the page.
func zgcReservePageToSpace(pg *zgcPage) bool {
	st := zgcPageState(pg)
	for _, s := range st.spans {
		ns := zgcReserveToSpace(s.spanclass, s.npages*pageSize)
		if ns == nil {
			return false
		}
		atomic.Store(&ns.sweepgen, mheap_.sweepgen+2)
		st.toSpace = append(st.toSpace, ns)
	}
	return true
}

// zgcAbandonPage unregisters a page whose to-space reservation failed and
// frees its metadata. Nothing copied, nothing published, no source span
// quarantined: the page stays ordinary heap.
func zgcAbandonPage(pg *zgcPage) {
	st := zgcPageState(pg)
	st.spans = nil
	for _, ns := range st.toSpace {
		lock(&mheap_.lock)
		atomic.Store(&ns.sweepgen, mheap_.sweepgen)
		mheap_.freeSpanLocked(ns, spanAllocHeap)
		unlock(&mheap_.lock)
	}
	st.toSpace = nil
	zgcPageForget(pg)
	zgcFreePage(pg)
}

// zgcDeepRelocate runs the relocation deep path for a barrier slow-path
// word: when the canonical address lies on a registered page with no
// published entry yet, the loading thread relocates the object itself.
// It reports whether the object is now published.
func zgcDeepRelocate(pg *zgcPage, addr uintptr) bool {
	if pg == nil || !zgcCanRelocateDeep() {
		return false
	}
	if pg.published.Load() == pg.live {
		return true
	}
	_, ok := zgcRelocateOneLocked(pg, addr)
	return ok
}
