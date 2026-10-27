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
// zgcCanRelocateDeep reports whether the current context may relocate or
// heal: preemptible, with a P, off the g0 and signal stacks. Other
// contexts resolve through the forwarding table only, which keeps reads
// sound while the source stays valid until release.
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

// zgcRelocateStatus is the one core typed outcome for single-object
// relocation. Done names a stable copy (already-forwarded, large in place,
// or a winning commit). NotHeap names only a missing heap object for the
// relocation (nil pg or nil span). NoMemory names only allocator-observed
// exhaustion (the to-space reservation returned nil). Refused names every
// other lawful stay: gate-observed freezes (foreign-exposed source at
// commit) plus concurrent unregister, out-of-range, and holds races that
// commit maps to 0 and that collapse here so no false OOM ever surfaces.
// More precise reasons are unnecessary: callers branch Done (use the copy),
// NotHeap (use the source via the lifetime protocol), NoMemory (throw),
// Refused (pin the source in place, never retry until unexposure, never
// throw OOM).
type zgcRelocateStatus uint8

const (
	zgcRelocateDone zgcRelocateStatus = iota
	zgcRelocateNotHeap
	zgcRelocateNoMemory
	zgcRelocateRefused
)

// zgcRelocateOneResult copies the single object holding addr into the
// page's to-space reservation and publishes it, reporting the typed outcome.
// Large spans never copy and report Done in place (see LargeLocked).
// Forwarded objects report Done with the published copy plus interior offset.
// Claim failure reports NoMemory; commit 0 reports Refused; missing heap or
// page reports NotHeap. This is the single implementation; the boolean
// wrapper below stays live for barrier and foreign-attach users and shares
// it, so there is no second pipeline.
func zgcRelocateOneResult(pg *zgcPage, addr uintptr) (uintptr, zgcRelocateStatus) {
	if pg == nil {
		return 0, zgcRelocateNotHeap
	}
	s := spanOfHeap(addr)
	if s == nil {
		return 0, zgcRelocateNotHeap
	}
	if s.spanclass.sizeclass() == 0 {
		to, _ := zgcRelocateLargeLocked(pg, s)
		return to, zgcRelocateDone
	}
	idx := s.objIndex(addr)
	base := s.base() + idx*s.elemsize
	if to, ok := pg.forward(base); ok {
		return to + (addr - base), zgcRelocateDone
	}
	won, st := zgcCopyOneResult(pg, s, base, s.elemsize)
	if st != zgcRelocateDone {
		return 0, st
	}
	return won + (addr - base), zgcRelocateDone
}

// zgcRelocateLargeLocked pins the large object of span s without copying:
// large objects never move, so the source freeze holds without a commit.
// The locking page query is exact world-running or in a pause; no copy,
// no identity move and no accounting publish, hence no gate-held commit.
func zgcRelocateLargeLocked(pg *zgcPage, s *mspan) (uintptr, bool) {
	base := s.base()
	if to, ok := pg.forward(base); ok {
		return to, true
	}
	if zgcForeignPagePinned(base) {
		return base, true
	}
	return base, true
}

// zgcReservePageToSpace pre-reserves one to-space span per source span of
// the page, off the mutator path in the pause. Reserved spans take the
// pre-bump sweepgen so the sweeper treats them as swept until the cycle
// that fills them publishes them onto the swept sets. Failure abandons
// the page.
func zgcReservePageToSpace(pg *zgcPage) bool {
	if zgcForeignPagePinnedSTW(pg.start) {
		return false
	}
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
