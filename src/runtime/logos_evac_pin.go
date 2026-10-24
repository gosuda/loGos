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

// Shared pin pass, ported from fix-M19ImmixComplete logos_evac_pin.go.
// Parameterization vs the immix original: the exclusion granule comes
// from ev.granule (blocks for immix, pages for zgc) instead of the
// hardcoded block size, and candidate words resolve through
// zgcCanonical first so colored or stale words pin the page of the
// object they name once colors are active. The defer-record pinning is
// a stage-3 addition (P8): defer records are read by runtime asm
// without barriers. Merge note: the immix lane sets ev.granule to its
// block size and ev.pinned.shift to its block shift before collecting;
// the traversal below is otherwise identical.

// pinPtr keeps the granule holding the heap object containing p in place
// for this cycle. Words that are not heap pointers are ignored.
func (ev *logosEvac) pinPtr(p uintptr) {
	if v := zgcCanonical(p); spanOfHeap(v) != nil {
		ev.pinned.add(v &^ (ev.granule - 1))
	}
}

// pinRange pins the target of every word in [lo, hi), the conservative way.
func (ev *logosEvac) pinRange(lo, hi uintptr) {
	for p := lo &^ (goarch.PtrSize - 1); p+goarch.PtrSize <= hi; p += goarch.PtrSize {
		ev.pinPtr(*(*uintptr)(unsafe.Pointer(p)))
	}
}

// logosSpanPinned reports whether any object of s feeds an address hash:
// either the sticky logosPinned flag or a set bit in the span's pinner
// bits. Shared by the immix block selector and the zgc page selector.
func logosSpanPinned(s *mspan) bool {
	if s.logosPinned {
		return true
	}
	pb := (*gcBits)(unsafe.Pointer(s.getPinnerBits()))
	if pb == nil {
		return false
	}
	for i := uintptr(0); i < divRoundUp(uintptr(s.nelems)*2, 8); i++ {
		if *pb.bytep(i) != 0 {
			return true
		}
	}
	return false
}

// collectPins builds the set of granules whose objects are referenced from
// places relocation cannot rewrite: conservatively scanned stacks, cgo and
// system call frames, runtime structures that hide addresses in uintptrs or
// off-heap memory, and the g, m, and p structures themselves. It reports false
// when a goroutine is in a state that relocation does not understand; the
// caller then moves nothing.
func (ev *logosEvac) collectPins() bool {
	for _, gp := range allgs {
		if !ev.pinGoroutine(gp) {
			return false
		}
	}
	for mp := allm; mp != nil; mp = mp.alllink {
		ev.pinPtr(uintptr(unsafe.Pointer(mp)))
		ev.pinPtr(uintptr(unsafe.Pointer(mp.g0)))
		ev.pinPtr(uintptr(unsafe.Pointer(mp.gsignal)))
		// Integ adaptation: the fast-cgo frame record lives on the M
		// (m.fastCgoFrame, logos_fastcall.go), not on the P as in the
		// lane (pp.cgoPinnedPtrs, which integ does not have).
		for _, w := range mp.fastCgoFrame {
			ev.pinPtr(w)
		}
	}
	for _, pp := range allp {
		ev.pinP(pp)
	}
	ev.pinItabs()
	for i := range semtable {
		ev.pinTreap(semtable[i].root.treap)
	}
	ev.pinMarkWorkers()
	ev.pinArenaChunks()
	return true
}

// pinArenaChunks pins every object a word of a live user arena chunk may
// point to. The evacuation does not walk arena memory, so its references must
// not move. A quarantined chunk is set to fault and made noscan, so only
// scannable chunks are read.
func (ev *logosEvac) pinArenaChunks() {
	for _, s := range mheap_.allspans {
		if s.isUserArenaChunk && s.state.get() == mSpanInUse && !s.isUnusedUserArenaChunk() && s.markBitsForIndex(0).isMarked() {
			ev.pinRange(s.base(), s.limit)
		}
	}
}

func logosStackConservative(gp *g) bool {
	return gp.syscallsp != 0 || gp.asyncSafePoint || readgstatus(gp)&^_Gscan == _Gsyscall
}

func (ev *logosEvac) pinGoroutine(gp *g) bool {
	ev.pinPtr(uintptr(unsafe.Pointer(gp)))
	switch readgstatus(gp) &^ _Gscan {
	case _Gdead, _Gdeadextra:
		return true
	case _Grunnable, _Gsyscall, _Gwaiting, _Gleaked, _Gpreempted:
	default:
		return false
	}
	ev.pinPtr(uintptr(gp.param))
	// P8: heap-allocated defer records are read by runtime asm without
	// barriers; pin their referents until the Z2-asm wrappers land.
	for d := gp._defer; d != nil; d = d.link {
		// d.fn is a func value: its word names the closure's funcval
		// (entry PC plus captured environment), read with the same
		// representation idiom as heapdump. Pin the funcval so the
		// closure environment cannot move; the entry PC itself is not
		// a heap pointer. d.fn is nil for open-coded defers.
		if d.fn != nil {
			ev.pinPtr(uintptr(unsafe.Pointer(*(**funcval)(unsafe.Pointer(&d.fn)))))
		}
		ev.pinPtr(uintptr(unsafe.Pointer(d.link)))
	}
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		ev.pinPtr(sg.c.uintptr())
		ev.pinPtr(sg.elem.uintptr())
	}
	if logosStackConservative(gp) {
		ev.pinStack(gp)
	}
	return true
}

// pinStack pins every object a word of gp's conservative stack may point to.
func (ev *logosEvac) pinStack(gp *g) {
	sp := gp.syscallsp
	if sp == 0 {
		sp = gp.sched.sp
	}
	if sp < gp.stack.lo || sp >= gp.stack.hi {
		sp = gp.stack.lo
	}
	ev.pinRange(sp, gp.stack.hi)
	ev.pinPtr(uintptr(gp.sched.ctxt))
	ev.pinXRegs(gp)
}

func (ev *logosEvac) pinP(pp *p) {
	if pp == nil {
		return
	}
	ev.pinPtr(uintptr(unsafe.Pointer(pp)))
	if c := pp.mcache; c != nil {
		ev.pinPtr(c.tiny)
	}
}

// pinItabs pins the types that interface tables name. The tables live in
// persistent memory, which the collector does not scan.
func (ev *logosEvac) pinItabs() {
	t := (*itabTableType)(atomic.Loadp(unsafe.Pointer(&itabTable)))
	if t == nil {
		return
	}
	for i := uintptr(0); i < t.size; i++ {
		m := *(**itab)(add(unsafe.Pointer(&t.entries), i*goarch.PtrSize))
		if m != nil {
			ev.pinPtr(uintptr(unsafe.Pointer(m.Type)))
			ev.pinPtr(uintptr(unsafe.Pointer(m.Inter)))
		}
	}
}

// pinTreap pins the semaphore addresses that waiters are keyed by. The tree
// orders its waiters by address, so a waiter's semaphore must not move.
func (ev *logosEvac) pinTreap(s *sudog) {
	for ; s != nil; s = s.next {
		ev.pinPtr(s.elem.uintptr())
		for w := s.waitlink; w != nil; w = w.waitlink {
			ev.pinPtr(w.elem.uintptr())
		}
		ev.pinTreap(s.prev)
	}
}

// pinMarkWorkers pins the idle mark worker nodes, which the lock-free pool
// links through packed addresses.
func (ev *logosEvac) pinMarkWorkers() {
	for v := atomic.Load64((*uint64)(unsafe.Pointer(&gcBgMarkWorkerPool))); v != 0; {
		n := lfstackUnpack(v)
		if n == nil {
			return
		}
		ev.pinPtr(uintptr(unsafe.Pointer(n)))
		v = n.next
	}
}
