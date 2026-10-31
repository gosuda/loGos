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
//
// It stores only a changed word. A slot inside a heap object that a
// published foreign frame exposes is swapped with a compare-and-swap
// from the word read to the canonical word instead of a plain store: C
// reads (and may write) that object where it sits while the world is
// stopped for Go only, so a plain store computed from an earlier read
// could overwrite a concurrent C store, and the CAS loses to it. The
// swap is never skipped. Admission canonicalized every edge it admitted,
// so a stale word in an exposed object is an edge a Go store added
// afterwards; its target is not exposed and can relocate, and no load
// barrier writes exposed slots (zgcSelfHeal), so leaving it would keep a
// source address alive past the retire that frees its forwarding entry.
// The word written is canonical and uncolored, so C never sees a color.
// The exact object query decides, so unrelated objects on a region that
// holds an exposed neighbour take the plain store. There is no page
// prefilter: the registry keys a page pin by the region of the object's
// base, so a slot of a large exposed object past the first 2 MB region
// would miss it. Slots outside the heap (globals, stacks, special and
// queue records) resolve to no span and take the plain store.
func zgcFixSlot(slot *uintptr) {
	v := *slot
	zgcPageTransientEnter()
	c := zgcCanonical(v)
	zgcPageTransientLeave()
	if c == v {
		return
	}
	if zgcSlotInExposedObject(slot) {
		atomic.Casuintptr(slot, v, c)
		return
	}
	*slot = c
}

// zgcMarkForeignRoots greys every object a published foreign frame
// exposes and forces each one to be scanned. The global exposure table
// is the union of every frame's admitted bases (refcounted per frame), so
// one pass over its keys visits each exposed object once, whatever the
// frame count. The world is stopped (gcStart).
//
// Greying is not enough here: admission sets the mark bit directly,
// without queueing the object, in every GC phase. An object admitted
// after the last sweep therefore reaches the next gcStart already marked
// and unscanned, and greyobject and tryDeferToSpanScan both return at a
// set bit, so edges stored into it between admission and the cycle
// would never be followed. Clearing the bit first lets the ordinary
// root path mark and queue it (a span queue for inline-mark spans, a
// work buffer otherwise); the bit is checked set afterwards because an
// exposed object left unmarked is freed under C. Noscan objects need
// only the mark. Frames admitted after this pass mark their whole closure
// at admission, with edges stored later shaded by the load barrier, so
// no mark-termination rescan is required.
func zgcMarkForeignRoots(gcw *gcWork) {
	if zgcForeignActive.Load() == 0 {
		return
	}
	t := &zgcForeignObjects
	for i := uintptr(0); i < t.cap; i++ {
		if base := zgcForeignSetKeyAt(t, i); base != 0 && base != ^uintptr(0) {
			zgcMarkForeignBase(base, gcw)
		}
	}
}

// zgcMarkForeignBase marks and queues one exposed object base.
func zgcMarkForeignBase(base uintptr, gcw *gcWork) {
	s := spanOfHeap(base)
	if s == nil || zgcSpanQuarantined(s) {
		throw("logos zgc: foreign root names no live heap object")
	}
	if logosGenSkip(s) {
		// A mature span is not marked or scanned by a minor cycle:
		// greyobject and tryDeferToSpanScan return without touching the
		// bit, so clearing it here would leave the object unmarked for
		// the sweep. The generation's own cards cover its edges.
		return
	}
	idx := uintptr(0)
	if s.spanclass.sizeclass() != 0 {
		idx = s.objIndex(base)
		if idx >= uintptr(s.nelems) || base != s.base()+idx*s.elemsize {
			throw("logos zgc: foreign root is not an object base")
		}
	}
	mbits := s.markBitsForIndex(idx)
	if !s.spanclass.noscan() {
		mbits.clearMarked()
	}
	if !tryDeferToSpanScan(base, gcw) {
		greyobject(base, 0, 0, s, gcw, idx)
	}
	if !mbits.isMarked() {
		throw("logos zgc: foreign root left unmarked")
	}
}

// zgcSlotInExposedObject reports whether slot lies inside a heap object
// that a published foreign frame currently exposes. The world is stopped.
// The exact object query resolves the slot to its object base itself and
// returns at once while no frame is published.
func zgcSlotInExposedObject(slot *uintptr) bool {
	return zgcForeignObjectExposedLocked(uintptr(unsafe.Pointer(slot)))
}

// zgcVerifySlot throws when a reference to a relocated object remains.
// It checks every slot zgcFixSlot visits, exposed objects included: the
// fixup heals those too, so a stale word left in one is a real defect.
func zgcVerifySlot(slot *uintptr) {
	v := *slot
	zgcPageTransientEnter()
	remapped := zgcRemap(zgcAddr(v))
	zgcPageTransientLeave()
	if remapped != zgcAddr(v) {
		print("runtime: slot ", slot, " still holds relocated pointer ", hex(v), "\n")
		throw("logos zgc: stale pointer to relocated object")
	}
}

// zgcFixRoots rewrites the root slots that must be current before the
// world restarts: globals, specials, finalizer and cleanup queues, and
// precise stacks. Heap slots heal lazily through the barrier. The
// visitor decides ownership per slot (zgcFixSlot swaps slots of exactly
// the objects a foreign frame exposes with a compare-and-swap), so the
// finalizer special's own object walk obeys the same rule as the heap
// rewrite.
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
	zgcPageTransientEnter()
	canonical := zgcCanonical(uintptr(p))
	zgcPageTransientLeave()
	return unsafe.Pointer(canonical)
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
	for i := range zgcForeignVisited {
		zgcForeignVisited[i] = 0
		zgcForeignExpanded[i] = 0
	}
	if len(live) == 0 {
		return 0
	}
	foreign := false
	if zgcForeignHoldCount.Load() > 0 {
		foreign = true
	} else {
		for _, gp := range logosGCCanonicalGs(allgs) {
			// Plain load in a NoLB file: a colored word an -lb heal left
			// in a backing-array entry must resolve to the canonical g
			// before the status dereference.
			gp = (*g)(unsafe.Pointer(zgcCanonical(uintptr(unsafe.Pointer(gp)))))
			if readgstatus(gp)&^_Gscan == _Gsyscall {
				foreign = true
				break
			}
		}
	}
	if !foreign {
		return 0
	}
	zgcForeignLive = live
	zgcForeignRetained = 0
	// Seeds: every stack word of every foreign thread and active hold, conservative
	// and read-only, plus published hold roots. Unregistered pages are visited the
	// same way so an A -> X -> B chain through ordinary heap still retains B; only
	// registered pages join the retained prefix.
	seeded := false
	for _, gp := range logosGCCanonicalGs(allgs) {
		gp = (*g)(unsafe.Pointer(zgcCanonical(uintptr(unsafe.Pointer(gp))))) // same NoLB strip as above
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
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		if slot.active.Load() == 0 {
			continue
		}
		gBits := slot.gp.Load()
		if gBits == 0 || slot.depth.Load() == 0 {
			continue
		}
		if root := slot.root.Load(); root != 0 {
			if base, ok := zgcForeignObjectBase(root); ok {
				if zgcForeignNotePage(base) {
					seeded = true
				}
			}
		}
		holdG := (*g)(unsafe.Pointer(gBits))
		if holdG != nil && holdG.stack.lo != 0 && holdG.stack.hi > holdG.stack.lo {
			for p := holdG.stack.lo; p < holdG.stack.hi; p += goarch.PtrSize {
				if base, ok := zgcForeignObjectBase(*(*uintptr)(unsafe.Pointer(p))); ok {
					if zgcForeignNotePage(base) {
						seeded = true
					}
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
	zgcPageTransientEnter()
	addr := zgcCanonical(v)
	zgcPageTransientLeave()
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
// logosExcludeRelocation hook.
//
// The copy goes through zgcRelocateOneResult, the single typed
// implementation the load barrier shares: a concurrent barrier copy of
// the same object returns the one published copy through the forwarding
// table, so the attach cannot fork two copies. Only Done adopts a copy;
// NoMemory alone throws out of memory (true to-space exhaustion);
// NotHeap returns the source as ordinary identity under the caller's
// lifetime (unregistered sources are never moved by workers, so the
// M-held commit in addspecialStable is stable); Refused returns the
// source for the in-place pin below (a frozen foreign-exposed source at
// commit), never retried until unexposure and never misthrown as OOM:
// retrying until a same-G callback scope ends would self-deadlock,
// because the scope cannot end until the callback returns. The commit
// veto in the copy path stops post-exposure worker moves once the pin
// commits; the narrow claim-before-pin race plus the missing gate-held
// forward recheck in the pin commit stays the page/pinner owner's
// bounded remaining work, coordinated through the existing gate and
// source-stability APIs, with no copy under M and no M-held wait here.
// Only preemptible callers run it, and never under a pinned M or on g0:
// the refill it may need runs in preemptible context only. Those callers
// are SetFinalizer's attach loop (outside its systemstack) and
// addCleanup's (before it pins the M); addspecialStable never copies, it
// reports specialAttachUnstable and the loop copies again. Only finalizer
// and cleanup attaches copy: those records keep their object stable,
// while weak handles must not resurrect and profile, secret, and debug
// records ride the pre-existing span lifecycle through canonicalization
// alone. The old boolean wrapper stays live for barrier users that only
// need success; this helper is its typed companion for attach.
func zgcAttachEnsureCopy(p unsafe.Pointer) unsafe.Pointer {
	if !zgcCycleEnabled() {
		return p
	}
	zgcPageTransientEnter()
	addr := zgcCanonical(uintptr(p))
	zgcPageTransientLeave()
	pg := zgcPageAcquire(addr)
	if pg == nil {
		// NotHeap fast path: unregistered ordinary identity under the
		// caller's lifetime (a live Go reference plus the M-held
		// commit below keeps it stable; workers never move it).
		return unsafe.Pointer(addr)
	}
	base := addr
	if s := spanOfHeap(addr); s != nil && s.spanclass.sizeclass() != 0 {
		base = s.base() + s.objIndex(addr)*s.elemsize
	}
	if to, ok := pg.forward(base); ok {
		zgcPageRelease(pg)
		// Published (Done): resolving to the copy needs no allocation
		// and is safe in any context.
		return unsafe.Pointer(to + (addr - base))
	}
	if !zgcCanRelocateDeep() {
		zgcPageRelease(pg)
		// Unpublished and no preemptible context to copy in (locked,
		// g0, signal, or P-less): the refill would allocate where it
		// must not. Fail fast; the attach loops copy at their own
		// preemptible boundary, so this fires only on an alien
		// context calling the public entries from a locked, g0, or
		// signal context.
		throw("logos zgc: cannot attach finalizer/cleanup in non-relocatable context")
	}
	to, st := zgcRelocateOneResult(pg, addr)
	zgcPageRelease(pg)
	switch st {
	case zgcRelocateDone:
		return unsafe.Pointer(to)
	case zgcRelocateNoMemory:
		throw("logos zgc: out of memory copying object for finalizer/cleanup attach")
	case zgcRelocateNotHeap:
		// Ordinary identity under the caller's lifetime: nil page or
		// span only (defensive; pg is non-nil above, so a heap race
		// that no longer resolves). Use the source; the M-held
		// commit below is stable for it.
		return unsafe.Pointer(addr)
	default:
		// Refused: frozen source at commit (foreign-exposed or pinned).
		// Arm the sticky veto here, preemptible with a P (never on
		// g0): gate-held exposure recheck plus the pin-bit read the
		// copy commit vetoes on, then set the span sticky the commit
		// reads lock-free. Never retry until unexposure and never
		// misthrow as OOM. The g0 commit below stays lock-free.
		if s := spanOfHeap(addr); s != nil {
			zgcForeignGateLock()
			exposed := zgcForeignObjectExposedLocked(addr)
			pinned := s.logosPinned.Load() || isPinned(unsafe.Pointer(addr))
			if exposed || pinned {
				s.logosPinned.Store(true)
			}
			zgcForeignGateUnlock()
		}
		return unsafe.Pointer(addr)
	}
	return nil
}

// zgcAttachStable reports whether the object at p, on span s, may take a
// finalizer or cleanup special right now. It is the attach handshake's
// commit-time half and #175's phase 2 prerequisite: zgcAttachEnsureCopy
// and logosZgcAttachSpecial run before the attacher becomes
// non-preemptible, so a mark-termination pause can select the copy's own
// page in that window, and a special spliced onto a quarantined span would
// be lost when the page is released.
//
// addspecialStable runs it with its M non-preemptible (acquirem) after
// ensureSwept and before taking speciallock. On false it releases the M
// and reports specialAttachUnstable with nothing spliced; the caller
// (SetFinalizer's loop, addCleanup's loop) returns to a preemptible
// point, runs zgcAttachEnsureCopy again and retries. While the M cannot
// be preempted no pause can start, so quarantine state cannot change
// between this check and the splice, and a true answer stays true until
// the splice arms the pin veto (span.logosPinned, which the copy
// preparation and gate-held commit refuse) alongside logosNoReloc; the
// pin veto is the commit safety, not NoReloc, which only steers future
// selection. Consistency: the exposure/pin veto is linearizable under
// the gate in EnsureCopy's preemptible arm; the commit reads the armed
// pin flag lock-free (causal atomic publication), and the M-held span
// resolution is a stable snapshot (no pause can intervene). False
// means: p is not canonical (a published copy exists, or a stale color
// remains), its span is quarantined and unfrozen (no veto armed, so no
// record could survive there), or its page is registered for relocation
// and not yet copied. A frozen quarantined span (vetoed: exposed or
// pinned) IS a home: selection quarantines source spans, so a Refused
// source is quarantined by construction, and refusing it would spin
// EnsureCopy (source) -> Stable (false) forever; the armed veto keeps
// published < live so the page never releases while the unpublished
// source is pending, and the splice re-arms it idempotently. Retry
// budget is bounded: each false makes progress (a published copy is
// adopted, or the veto is armed), so loops settle in at most one re-arm
// round with no pause and no wait for unexposure. A non-quarantined
// large object never moves (zgcAttachEnsureCopy returns it in place),
// so it is always home; a quarantined large is accepted only when
// frozen, by the same lock-free pin signal.
func zgcAttachStable(s *mspan, p uintptr) bool {
	if !zgcCycleEnabled() {
		return true
	}
	zgcPageTransientEnter()
	canonical := zgcCanonical(p)
	zgcPageTransientLeave()
	if canonical != p {
		return false
	}
	if s.spanclass.sizeclass() == 0 {
		// Large spans are never selected, hence never quarantined in
		// practice; a quarantined large is defensive-only. A frozen
		// (vetoed) quarantined large is accepted: the pin veto keeps
		// published < live so the page never releases while the
		// unpublished source is pending, and the splice re-arms the
		// veto idempotently. Lock-free pin read, no gate on g0.
		if zgcSpanQuarantined(s) {
			return s.logosPinned.Load() || isPinned(unsafe.Pointer(p))
		}
		return true
	}
	if zgcSpanQuarantined(s) {
		// A frozen (vetoed: exposed or pinned) quarantined source is
		// accepted, not retried: selection quarantines source spans,
		// so a Refused source is quarantined by construction and
		// refusing it would spin EnsureCopy (source) -> Stable
		// (false) forever, self-deadlocking a same-G callback whose
		// scope cannot end until the callback returns. The pin veto
		// keeps published < live, so the page never releases while
		// the unpublished source is pending, and the splice re-arms
		// the veto idempotently. Lock-free pin read, no gate on g0;
		// a fresh exposure takes one retry round to arm (never a
		// wait for unexposure). Unfrozen quarantined storage is
		// never a home: false routes back through EnsureCopy's
		// forward recheck, which adopts a published copy when one
		// exists.
		return s.logosPinned.Load() || isPinned(unsafe.Pointer(p))
	}
	if !zgcPageRegisteredNoSplit(p) {
		return true
	}
	// Registered, unquarantined, unpublished source. A frozen source
	// (Refused at commit) pins in place: true lets the splice arm the
	// pin veto (span.logosPinned, which the copy preparation and
	// gate-held commit both refuse) alongside logosNoReloc under
	// speciallock with the M held. The preemptible EnsureCopy arm
	// narrows the pre-M window; a fresh exposure takes one retry
	// round to arm, never a wait for unexposure. Lock-free pin read,
	// no gate on g0. Anything else still needs a copy: false routes
	// back through EnsureCopy's forward recheck (plus Result), and
	// each such false the loops observe follows real pause/copy
	// progress; the narrow claim-before-pin race stays the
	// page/pinner owner's bounded work, coordinated read-only here.
	return s.logosPinned.Load() || isPinned(unsafe.Pointer(p))
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
