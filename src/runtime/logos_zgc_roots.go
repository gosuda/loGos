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

// ZGC roots and rewrites over the shared walk core (logos_evac_fix.go).
// The fixer writes uncolored canonical words in every stage (contract
// color-write rule): only the -lb load barrier's self-heal CAS writes
// colored words. Collector-fixed slots include runtime-owned words that
// plain code dereferences, so a colored word in any of them faults under
// software encoding.

// zgcFixSlot rewrites a reference to a relocated object to its current
// canonical address, uncolored. Interior pointers keep their offset.
// It has the plain func type so it serves as a logosSlotVisitor.
func zgcFixSlot(slot *uintptr) {
	*slot = zgcCanonical(*slot)
}

// zgcVerifySlot throws when a reference to a relocated object remains.
func zgcVerifySlot(slot *uintptr) {
	if v := *slot; zgcRemap(zgcAddr(v)) != zgcAddr(v) {
		print("runtime: slot ", slot, " still holds relocated pointer ", hex(v), "\n")
		throw("logos zgc: stale pointer to relocated object")
	}
}

// zgcFixRoots rewrites the root slots that must be current before the
// world restarts: globals, specials, finalizer and cleanup queues, and
// precise stacks. Heap slots heal lazily through the barrier.
func zgcFixRoots(v logosSlotVisitor) {
	assertWorldStopped()
	logosWalkGlobals(v)
	logosWalkSpecials(v)
	logosWalkQueues(v)
	logosWalkStacks(v)
}

// logosZgcAttachSpecial canonicalizes p before addspecial offset math so
// offset math selects the copy's span. Under colors a stale or colored
// word is resolved through the forwarding table and unmasked first.
func logosZgcAttachSpecial(p unsafe.Pointer) unsafe.Pointer {
	if !zgcCycleEnabled() {
		return p
	}
	return unsafe.Pointer(zgcCanonical(uintptr(p)))
}

// Foreign retention for pages addressed from blocking syscalls and cgo
// calls: a transitive closure, not a direct-word scan.
//
// A thread parked in foreign code holds its argument block on its Go
// stack, but the words inside that block have no Go type layout: the
// precise rewrite cannot visit them, and a call that starts mid-cycle
// can name a source object the evacuators copy while the call runs. The
// kernel or C then follows pointers from that block (writev-style iovec
// arrays, argv indirection, embedded ioctl pointers), so retaining only
// the directly named page still releases a second page the call
// traverses: source A keeps pointing at source B, because heap on
// quarantined spans is skipped by the rewrite, and MADV_DONTNEED would
// zero storage still read through source addresses. A published copy
// does not save such a call: C and the kernel keep addressing the
// source address they were given.
//
// zgcForeignRetainLive computes the reachable set and partitions live so
// the retained prefix is deferred to the next cycle instead of released
// underneath the call. Goroutines running Go code need no check: the
// gcStart rewrite canonicalized every typed slot, and every word a
// barrier publishes afterwards is canonical. The world is stopped.
//
// Seeds are conservative and read-only: every word of each _Gsyscall
// stack resolves to a live object base without rewriting anything, so
// an integer that happens to look like a heap address only retains,
// never corrupts a C argument, and an unmarked (garbage) word retains
// nothing. Edges are precise: reached heap objects expand through their
// actual type masks (logosWalkObject, the same primitive the collector
// rewrites with), never by scanning words as pointers.
//
// Termination needs no object cap and no overflow valve: visitation is
// tracked per page in two bitsets sized to the page directory itself, so
// every heap address in range maps to exactly one bit and capacity is
// proven against heap limits rather than tuned to graph shapes. Object
// cycles terminate on the visited bits, each page's objects expand at
// most twice, and passes stop when no pass visits a new page. The
// retained set is a prefix partition of the caller's live slice, so the
// pause allocates nothing and cannot overflow. Mark bits stay the
// liveness predicate on every span kind: quarantine keeps them readable
// on source spans and to-space claims set them on copies.

// zgcForeignPageWords covers every page the directory can name:
// 1 << (zgcAddrBits - zgcPageShift) pages, one bit each.
const zgcForeignPageWords = 1 << (zgcAddrBits - zgcPageShift - 6)

// zgcForeignVisited marks pages already reached from foreign roots;
// zgcForeignExpanded marks pages whose live objects already had their
// edges followed. Both are stop-the-world exclusive to the gcStart
// foreign closure.
var zgcForeignVisited, zgcForeignExpanded []uint64

// zgcInitForeignRoots preallocates the foreign-closure visitation while
// the world runs. Pause paths must not grow Go heap memory.
func zgcInitForeignRoots() {
	if zgcForeignVisited == nil {
		zgcForeignVisited = make([]uint64, zgcForeignPageWords)
	}
	if zgcForeignExpanded == nil {
		zgcForeignExpanded = make([]uint64, zgcForeignPageWords)
	}
}

// zgcForeignPageBit maps a heap address to its visitation bit. It reports
// false for addresses outside the directory range (C, mmap, and stack
// words above the 4 TB field, which never name heap).
func zgcForeignPageBit(addr uintptr) (uint64, uint64, bool) {
	if addr > zgcAddrMask {
		return 0, 0, false
	}
	i := addr >> zgcPageShift
	return uint64(i >> 6), uint64(1) << (i & 63), true
}

// zgcForeignObjectBase resolves w to the base of the live heap object it
// names, or reports false. Colors strip with the active mask only: with
// colors inert the word is used as is, so a foreign high address never
// truncates into the heap range. Interior pointers resolve to their
// object base, which is what the closure tracks. Dead (unmarked) words
// name garbage the release may reclaim, so they resolve to nothing.
func zgcForeignObjectBase(w uintptr) (uintptr, bool) {
	if w == 0 {
		return 0, false
	}
	a := w
	if atomic.Loaduintptr(&zgcColorMaskBits) != 0 {
		a = w & zgcAddrMask
	}
	s := spanOfHeap(a)
	if s == nil {
		return 0, false
	}
	// One path for every span: large spans hold a single object, so the
	// index is 0 and the base is the span base; small spans resolve
	// through their size class. The bounds guard also covers span tail
	// padding beyond the last object.
	idx := s.objIndex(a)
	if idx >= uintptr(s.nelems) {
		return 0, false
	}
	if !s.markBitsForIndex(idx).isMarked() {
		return 0, false
	}
	return s.base() + idx*s.elemsize, true
}

// Foreign-closure scratch, stop-the-world exclusive to
// zgcForeignRetainLive: the gcStart hook runs on one thread with the
// world stopped. Named package-level functions below replace capturing
// closures, so the pause path is allocation-free by construction instead
// of by escape-analysis argument: no function values are built, no heap
// pointers are stored, and the bitsets partition preallocated storage.
var zgcForeignLive []*zgcPage
var zgcForeignRetained int
var zgcForeignProgress bool

// zgcForeignNotePage marks the page of addr visited and, when addr lies
// on a page awaiting a release decision, moves that page into the
// retained prefix of zgcForeignLive. It reports whether the page is newly
// visited.
func zgcForeignNotePage(addr uintptr) bool {
	wi, bit, ok := zgcForeignPageBit(addr)
	if !ok {
		return false
	}
	if zgcForeignVisited[wi]&bit != 0 {
		return false
	}
	zgcForeignVisited[wi] |= bit
	live := zgcForeignLive
	for i := zgcForeignRetained; i < len(live); i++ {
		if pg := live[i]; addr >= pg.start && addr < pg.start+pg.size {
			live[zgcForeignRetained], live[i] = live[i], live[zgcForeignRetained]
			zgcForeignRetained++
			break
		}
	}
	return true
}

// zgcForeignVisitSlot resolves one heap slot into the closure. It serves
// as the logosSlotVisitor for the expansion pass: read-only, no
// allocation, no capture.
func zgcForeignVisitSlot(slot *uintptr) {
	if base, ok := zgcForeignObjectBase(*slot); ok {
		if zgcForeignNotePage(base) {
			zgcForeignProgress = true
		}
	}
}

// zgcForeignRetainLive partitions live in place so live[:n] holds every
// registered page reachable from a goroutine blocked in a syscall or cgo
// call, and returns n. The caller defers the prefix and releases from
// the suffix. With no foreign thread in flight it returns 0 after one
// status read per goroutine.
func zgcForeignRetainLive(live []*zgcPage) int {
	assertWorldStopped()
	if len(live) == 0 {
		return 0
	}
	foreign := false
	for _, gp := range allgs {
		if readgstatus(gp)&^_Gscan == _Gsyscall {
			foreign = true
			break
		}
	}
	if !foreign {
		return 0
	}
	for i := range zgcForeignVisited {
		zgcForeignVisited[i] = 0
		zgcForeignExpanded[i] = 0
	}
	zgcForeignLive = live
	zgcForeignRetained = 0
	// Seeds: every stack word of every foreign thread, conservative and
	// read-only. Unregistered pages are visited the same way so an
	// A -> X -> B chain through ordinary heap still retains B; only
	// registered pages join the retained prefix.
	seeded := false
	for _, gp := range allgs {
		if readgstatus(gp)&^_Gscan != _Gsyscall {
			continue
		}
		for p := gp.stack.lo; p < gp.stack.hi; p += goarch.PtrSize {
			if base, ok := zgcForeignObjectBase(*(*uintptr)(unsafe.Pointer(p))); ok {
				if zgcForeignNotePage(base) {
					seeded = true
				}
			}
		}
	}
	if !seeded {
		zgcForeignLive = nil
		return 0
	}
	// Fixpoint: expand the live objects of each newly visited page
	// through their typed pointer fields. A page expands at most twice
	// (once in the pass that visits it, once more for objects scanned
	// before the visit); the expanded bits update only between passes.
	for {
		zgcForeignProgress = false
		// One path for every span: large spans iterate a single index
		// and expand through their large type inside logosWalkObject,
		// small spans through their size class.
		for _, s := range mheap_.allspans {
			if s.state.get() != mSpanInUse || s.spanclass.noscan() || s.isUserArenaChunk {
				continue
			}
			for i := range uintptr(s.nelems) {
				if !s.markBitsForIndex(i).isMarked() {
					continue
				}
				base := s.base() + i*s.elemsize
				wi, bit, ok := zgcForeignPageBit(base)
				if !ok || zgcForeignVisited[wi]&bit == 0 || zgcForeignExpanded[wi]&bit != 0 {
					continue
				}
				logosWalkObject(s, base, zgcForeignVisitSlot)
			}
		}
		for i := range zgcForeignVisited {
			zgcForeignExpanded[i] |= zgcForeignVisited[i]
		}
		if !zgcForeignProgress {
			n := zgcForeignRetained
			zgcForeignLive = nil
			return n
		}
	}
}

// zgcNewPinEv returns a page-granular pin set over the shared pin core.
func zgcNewPinEv() logosEvac {
	var ev logosEvac
	ev.granule = zgcPageSize
	ev.pinned.shift = zgcPageShift
	return ev
}

// zgcAttachResolve finds the object containing p for finalizer and
// cleanup attachment. When the cycle is enabled p is canonicalized
// first: under colors a stale or colored word resolves through the
// forwarding table and is unmasked before span lookup, so offset math
// selects the copy's span. Source spans of registered pages are
// returned rather than skipped: the caller copies the object into
// to-space first (zgcAttachEnsureCopy) and attaches to the copy.
// Callers treat base == 0 as "not in an allocated block", exactly as
// with findObject.
func zgcAttachResolve(p unsafe.Pointer) (base uintptr, s *mspan) {
	v := uintptr(p)
	if !zgcCycleEnabled() {
		base, s, _ = findObject(v, 0, 0)
		return base, s
	}
	addr := zgcCanonical(v)
	s = spanOfHeap(addr)
	if s == nil {
		return 0, nil
	}
	if s.spanclass.sizeclass() == 0 {
		return s.base(), s
	}
	base = s.base() + s.objIndex(addr)*s.elemsize
	return base, s
}

// zgcAttachEnsureCopy copies the object at p into its page's to-space
// when p names an unpublished object on a registered page, and returns
// the address to attach to. Published, ordinary, and non-heap words
// pass through unchanged. Large objects are never copied; they pass
// through and the caller pins the span through the normal
// logosExcludeRelocation hook. Copy failure means the to-space
// reservation is exhausted, which is out of memory: attaching to the
// source would dangle the special, since the in-flight evacuation
// copies regardless of span flags and release frees on published ==
// live.
//
// The copy goes through zgcRelocateOneLocked, the same race-safe path
// the load barrier uses: a concurrent barrier copy of the same object
// returns the one published copy through the forwarding table, so the
// attach cannot fork two copies. Callers run it before taking span or
// special locks: the refill it may need runs in preemptible context
// only. Only finalizer and cleanup attaches copy: those records keep
// their object stable, while weak handles must not resurrect and
// profile, secret, and debug records ride the pre-existing span
// lifecycle through canonicalization alone.
func zgcAttachEnsureCopy(p unsafe.Pointer) unsafe.Pointer {
	if !zgcCycleEnabled() {
		return p
	}
	addr := zgcCanonical(uintptr(p))
	pg := zgcPageFor(addr)
	if pg == nil {
		return unsafe.Pointer(addr)
	}
	base := addr
	if s := spanOfHeap(addr); s != nil && s.spanclass.sizeclass() != 0 {
		base = s.base() + s.objIndex(addr)*s.elemsize
	}
	if to, ok := pg.forward(base); ok {
		// Published: resolving to the copy needs no allocation and
		// is safe in any context.
		return unsafe.Pointer(to + (addr - base))
	}
	if !zgcCanRelocateDeep() {
		// Unpublished and no preemptible context to copy in (locked,
		// g0, signal, or P-less): the refill would allocate where it
		// must not. Fail fast; the hoisted pre-lock copies cover
		// every in-runtime caller, so this fires only on a missed
		// hoist or an alien context.
		throw("logos zgc: cannot attach finalizer/cleanup in non-relocatable context")
	}
	if to, ok := zgcRelocateOneLocked(pg, addr); ok {
		return unsafe.Pointer(to)
	}
	throw("logos zgc: out of memory copying object for finalizer/cleanup attach")
	return nil
}

// zgcPinSelfStack pins every heap object a word of the calling
// goroutine's own stack may point to. The gcStart hook runs it on curg,
// whose stack cannot be walked precisely while it runs: a stale slot
// left unmapped could otherwise be released while still referenced.
// Conservative pinning only ever over-pins, and every word is resolved
// through canonicalization first.
func zgcPinSelfStack(ev *logosEvac) {
	if gp := getg().m.curg; gp != nil {
		ev.pinRange(gp.stack.lo, gp.stack.hi)
	}
}
