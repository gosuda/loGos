// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Syscall foreign-admission lane (owner H-runtime).
//
// A moving collector relocates heap objects concurrently with mutators,
// so a syscall whose Go referent is not registry-exposed can lose kernel
// writes (released from-space is dropped; the kernel keeps writing the
// old address). This lane admits the exact cells the call reads before
// uintptr erasure, retains the complete pointer-bearing closure, and
// pairs the invocation lifetime. Scalar integer bits and the public
// syscall result/error ABI are untouched.
//
// Wire shape (bound with the compiler lane; n counts PAIRS):
//
//   - rows points to 2*n words: [Slot, Type] pairs. Slot is the address
//     of the actual unsafe.Pointer temp whose uintptr conversion the
//     call reads. Type is the *_type word of the referent value, or nil
//     for a single opaque word. n == 0 admits nothing and returns the
//     invalid token; the compiler skips emission entirely then.
//   - The token travels as (frame, gen) halves: the existing two-word
//     logosForeignToken layout, unboxed for builtin-stub compatibility.
//     (nil, 0) is invalid: no ownership, End pairs with nothing.
//
// Type words never cross into logosForeignRoot. Every registry
// descriptor built here is an Unknown single-word root over a proven
// cell (the temp, or a mask-proven discovered cell); kinds and sizes
// are supplied by this lane, never inferred from Type. Backings are
// retained at object granularity through the normal closure, so spare
// capacity is covered exactly. The temp root rewrites the temp to the
// canonical copy at commit; heap closure expansion is authoritative via
// heap spans.
//
// Consistency: the stop holder is linearizable and exclusive (worldsema
// exclusion, the same basis as the registry core), so discovery,
// commit, and publish are a single atomic section with no concurrent
// admission. Token ownership is thread-confined to the admitting g, the
// same single-owner discipline as the registry. The commit applier is
// idempotent: re-admission of a canonical cell is skipped, never
// double-stored.
//
// Admission runs with the world stopped on the system stack: caller
// slots are frozen between record and commit, and mask prebuilds happen
// while the world still runs. Between the post-stop capture and the
// systemstack switch there are no calls of any kind (intrinsics and
// plain stores only, then an asm switch), so the stack provably cannot
// grow in that window; a stale capture would indicate corruption, and
// the core asserts that impossible state instead of retrying.
// Discovery is iterative breadth-first over off-heap worklists (no
// recursion, no caps; exhaustion fails closed and loud). Stack layout
// comes from the compiler's own stack-object records via the
// collector's own walker (logosWalkStackObject, the same function the
// collector scans stacks with); global layout from the module masks of
// each queued module (only modules containing queued global referents
// are ever walked, transitively). Heap layout is always
// span-authoritative. Unprovable words (untyped referents in no recorded
// object, mmap and scalar addresses) are never guessed: only
// mask-proven cells become roots. A call with no preflight through a
// legal context still relies on the pre-existing conservative _Gsyscall
// pin as backstop, never on this lane guessing.

// zgcSyscallMaxPairs bounds row counts against address-space overflow.
// It is a corruption guard, not a capacity limit: lawful calls name a
// handful of conversions.
const zgcSyscallMaxPairs = ^uintptr(0) >> 5

// zgcSyscallSlotAt returns the temp-cell address of pair i.
//
//go:nosplit
func zgcSyscallSlotAt(rows unsafe.Pointer, i uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(add(rows, i*2*unsafe.Sizeof(uintptr(0))))
}

// zgcSyscallTypeAt returns the referent *_type word of pair i (nil for a
// single opaque word).
//
//go:nosplit
func zgcSyscallTypeAt(rows unsafe.Pointer, i uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(add(rows, i*2*unsafe.Sizeof(uintptr(0))+unsafe.Sizeof(uintptr(0))))
}

// zgcSyscallAdmit admits n [Slot, Type] pairs and returns the ownership
// halves. The invalid token means the cycle is disabled or n == 0.
// Malformed descriptors and off-heap exhaustion throw fail-loud after
// the world restarts; no partial admission ever reaches foreign code.
//
//go:noinline
func zgcSyscallAdmit(rows unsafe.Pointer, n uintptr) (frame unsafe.Pointer, gen uint64) {
	// Noinline: the capture below must name this frame's caller. Inlined
	// into generated code, GetCallerPC/SP would skip the syscall site
	// frame and the walk would start one frame too high (same reason as
	// the noinline on the sched capture in proc.go).
	if !zgcCycleEnabled() {
		return nil, 0
	}
	if n == 0 {
		return nil, 0
	}
	if rows == nil {
		throw("logos zgc: syscall admit with nil rows")
	}
	if n > zgcSyscallMaxPairs {
		throw("logos zgc: syscall admit row count overflows the address space")
	}
	gp := getg()
	ptrSize := unsafe.Sizeof(uintptr(0))
	for i := uintptr(0); i < n; i++ {
		if zgcSyscallSlotAt(rows, i) == nil {
			throw("logos zgc: syscall admit with nil slot")
		}
		if n > ^uintptr(0)/(2*ptrSize)-i {
			throw("logos zgc: syscall admit row range overflows the address space")
		}
	}
	// Build every declared referent bitmap while the world runs, so the
	// stop below never spins on an on-demand mask build.
	for i := uintptr(0); i < n; i++ {
		if typ := zgcSyscallTypeAt(rows, i); typ != nil {
			getGCMask((*_type)(typ))
		}
	}
	w := stopTheWorld(stwUnknown)
	// Capture on the frozen stack: intrinsics only, no calls. The
	// staging below is written after the stop and consumed once,
	// exclusively, by this stop holder: no other goroutine runs while
	// the world is stopped, so the staging needs no lock and no barrier
	// concerns beyond ordinary mutator stores. Pointer fields are
	// cleared after the restart so the staging never pins memory.
	zgcSyscallSTWArgs.gp = gp
	zgcSyscallSTWArgs.rows = rows
	zgcSyscallSTWArgs.n = n
	zgcSyscallSTWArgs.pc = sys.GetCallerPC()
	zgcSyscallSTWArgs.sp = sys.GetCallerSP()
	zgcSyscallSTWArgs.tok = logosForeignInvalidToken
	zgcSyscallSTWArgs.ok = false
	systemstack(func() {
		zgcSyscallOnSystemstack()
	})
	tok := zgcSyscallSTWArgs.tok
	ok := zgcSyscallSTWArgs.ok
	zgcSyscallSTWArgs.gp = nil
	zgcSyscallSTWArgs.rows = nil
	startTheWorld(w)
	if !ok {
		throw("logos zgc: syscall admission exhausted off-heap storage")
	}
	return tok.Frame, tok.Gen
}

// zgcSyscallSTWArgs stages the admission across the systemstack switch
// without a capturing closure (which would heap-allocate inside the
// stop), and holds the discovery scratch the same way the registry
// holds its admit scratch: package state, STW-exclusive, off-heap
// contents only. No stack state is ever referenced through here, so
// nothing escapes.
var zgcSyscallSTWArgs struct {
	gp   *g
	rows unsafe.Pointer
	n    uintptr
	pc   uintptr
	sp   uintptr
	tok  logosForeignToken
	ok   bool
	// Discovery scratch: off-heap proven root cells (which become
	// Unknown registry roots), off-heap expansion referents, and
	// off-heap queued modules, each deduplicated. Breadth-first and
	// iterative: no recursion, no caps; exhaustion fails closed.
	roots uintptr
	nr    uintptr
	cr    uintptr
	refs  uintptr
	nw    uintptr
	cw    uintptr
	mods  uintptr
	nm    uintptr
	cm    uintptr
	seenR zgcForeignSet
	seenW zgcForeignSet
	seenM zgcForeignSet
}

// zgcSyscallAdmitIndirect admits only when code carries the foreign
// FuncFlag. Unmarked targets (ordinary address/hash-value consumers,
// unmarked asm stubs) and nil code return the invalid token with
// strictly zero mutation: no slot is read for retention and no cell is
// rewritten. The flag is a target property only, never evidence that
// integer arguments are pointers.
func zgcSyscallAdmitIndirect(code unsafe.Pointer, rows unsafe.Pointer, n uintptr) (frame unsafe.Pointer, gen uint64) {
	if !zgcCycleEnabled() {
		return nil, 0
	}
	if n == 0 {
		return nil, 0
	}
	if code == nil {
		return nil, 0
	}
	fi := findfunc(uintptr(code))
	if !fi.valid() || fi._func.flag&abi.FuncFlagUintptrForeign == 0 {
		return nil, 0
	}
	return zgcSyscallAdmit(rows, n)
}

// zgcSyscallEnd releases exactly the invocation named by (frame, gen).
// The invalid token is a no-op. LIFO, owner, and generation violations
// throw via the single-owner registry path. Must run on a preemptible
// stack: it locks and frees.
func zgcSyscallEnd(frame unsafe.Pointer, gen uint64) {
	logosZgcForeignEnd(logosForeignToken{Frame: frame, Gen: gen})
}

// zgcSyscallAdmitNoSplit is the entry point the compiler calls from
// nosplit contexts. It is a pure coverage check and never mints
// ownership: rows the caller already owns (or that an enclosing
// preemptible admission already covers) return the invalid token, so
// no ownership is minted and EndNoSplit pairs with nothing. Anything
// else throws fail-loud as bad producer protocol: the typed caller must
// have preflighted at a preemptible site through an enclosing Begin.
// The leaf takes no locks, allocates nothing, stops no world, and grows
// no stack; in particular it never delegates to the full admission,
// which is illegal on a nosplit stack, in a no-P context, and on
// fork-poisoned stacks.
//
//go:nosplit
func zgcSyscallAdmitNoSplit(rows unsafe.Pointer, n uintptr) (frame unsafe.Pointer, gen uint64) {
	if !zgcCycleEnabled() {
		return nil, 0
	}
	if n == 0 {
		return nil, 0
	}
	if rows == nil {
		throw("logos zgc: syscall nosplit admit with nil rows")
	}
	if n > zgcSyscallMaxPairs {
		throw("logos zgc: syscall nosplit admit row count overflows the address space")
	}
	gp := getg()
	st := zgcSyscallVerifyCoverage(gp, rows, n)
	if st == zgcSyscallCovered {
		// Already owned: pure check, no new ownership. Token-free;
		// pairs EndNoSplit with nothing.
		return nil, 0
	}
	if st == zgcSyscallUncoveredHeap {
		throw("logos zgc: syscall nosplit admit of an uncovered heap referent; producer preflight missing (admit through an enclosing Begin)")
	}
	if zgcSyscallOuterLive(gp) {
		// Undiscoverable from the leaf, but an enclosing preemptible
		// admission already covers the closure; trust it transitively
		// and mint nothing.
		return nil, 0
	}
	throw("logos zgc: syscall nosplit admit of an undiscoverable referent with no outer frame; producer preflight missing (declare types or admit through an enclosing Begin)")
	return nil, 0
}

const (
	// zgcSyscallCovered means every resolvable heap word is already
	// owned by the caller's own frames: the leaf may return token-free.
	zgcSyscallCovered = iota
	// zgcSyscallUncoveredHeap means a resolvable heap word is not
	// owned: the leaf throws fail-loud as bad producer protocol.
	zgcSyscallUncoveredHeap
	// zgcSyscallNeedFull means a referent needs layout discovery the
	// leaf cannot perform: an untyped non-heap word, or a type whose
	// on-demand mask is not built. The leaf trusts a live outer
	// preemptible admission transitively, and throws without one.
	zgcSyscallNeedFull
)

// zgcSyscallVerifyCoverage proves what the leaf can prove without
// allocating, stopping, or unwinding: direct heap words and typed
// cells resolve through span bounds and verify against the caller's
// own frames (CoveredBySelf); untyped non-heap referents and unbuilt
// masks report needFull for the caller to route. Only malformed
// descriptors throw here; coverage verdicts return.
//
//go:nosplit
func zgcSyscallVerifyCoverage(gp *g, rows unsafe.Pointer, n uintptr) int {
	for i := uintptr(0); i < n; i++ {
		slot := zgcSyscallSlotAt(rows, i)
		if slot == nil {
			throw("logos zgc: syscall nosplit admit with nil slot")
		}
		typ := zgcSyscallTypeAt(rows, i)
		w := *(*uintptr)(slot)
		if base, ok := zgcForeignHeapBaseNoSplit(w); ok {
			if !zgcForeignCoveredBySelf(gp, base) {
				return zgcSyscallUncoveredHeap
			}
			continue
		}
		if typ == nil {
			if k := zgcSyscallClassify(gp, w); k == zgcSyscallRefStack || k == zgcSyscallRefGlobal {
				return zgcSyscallNeedFull
			}
			continue
		}
		t := (*_type)(typ)
		if t.Size_ == 0 {
			continue
		}
		if t.Size_ > ^uintptr(0)-w {
			throw("logos zgc: syscall nosplit referent extent overflows the address space")
		}
		if !zgcSyscallMaskReady(t) {
			return zgcSyscallNeedFull
		}
		limit := w + t.Size_
		tp := zgcForeignTypeCellsNoSplit(t, w)
		for {
			var cell uintptr
			tp, cell = tp.next(limit)
			if cell == 0 {
				break
			}
			cw := *(*uintptr)(unsafe.Pointer(cell))
			if cbase, ok := zgcForeignHeapBaseNoSplit(cw); ok {
				if !zgcForeignCoveredBySelf(gp, cbase) {
					return zgcSyscallUncoveredHeap
				}
			}
		}
	}
	return zgcSyscallCovered
}

// zgcSyscallMaskReady reports whether a referent type's GC bitmap is
// already built, mirroring the unbuilt-mask check in
// zgcForeignTypeCellsNoSplit without throwing: the leaf routes unbuilt
// masks to full admission (legal P) or the outer preflight gate
// (restricted) instead of failing here.
//
//go:nosplit
func zgcSyscallMaskReady(t *_type) bool {
	if t.PtrBytes == 0 {
		return true
	}
	if t.TFlag&abi.TFlagGCMaskOnDemand == 0 {
		return true
	}
	p := atomic.Loadp(unsafe.Pointer(t.GCData))
	return p != nil && p != unsafe.Pointer(&inProgress)
}

// zgcSyscallOuterLive reports whether gp's frame chain holds a live
// published admission: the explicit prepared frame whose closure the
// restricted path trusts transitively. The walk is bounded by the
// chain depth exactly like zgcForeignCoveredBySelf. Pure reads,
// lock-free, nosplit-safe.
//
//go:nosplit
func zgcSyscallOuterLive(gp *g) bool {
	steps := gp.logosForeignDepth
	for top := gp.logosForeignTop; top != 0; {
		f := (*zgcForeignFrame)(unsafe.Pointer(top))
		if f.published == 1 {
			return true
		}
		top = f.parent
		if steps == 0 {
			return false
		}
		steps--
	}
	return false
}

// zgcSyscallEndNoSplit pairs the nosplit leaf. The leaf never mints
// ownership, so the only lawful token here is the invalid one, which
// pairs with nothing. A valid token would mean a full admission paired
// with the wrong End; that throws instead of taking the registry gate
// (locks), which is illegal in exactly the contexts this leaf serves.
//
//go:nosplit
func zgcSyscallEndNoSplit(frame unsafe.Pointer, gen uint64) {
	if frame == nil || gen == 0 {
		return
	}
	throw("logos zgc: syscall nosplit End of live admission; pair full admission with End")
}

// Referent classification for discovery. Colors are stripped only for
// the heap test; stack and global ranges match the raw word so high
// address bits of non-heap addresses are never corrupted.
const (
	zgcSyscallRefOther = iota
	zgcSyscallRefHeap
	zgcSyscallRefStack
	zgcSyscallRefGlobal
)

// zgcSyscallClassify reports what a referent word names. Pure loads and
// arithmetic; safe on the system stack with the world stopped.
//
//go:nosplit
func zgcSyscallClassify(gp *g, w uintptr) int {
	a := w &^ atomic.Loaduintptr(&zgcColorMaskBits)
	if a != 0 && a <= zgcAddrMask && spanOfHeap(a) != nil {
		return zgcSyscallRefHeap
	}
	if w >= gp.stack.lo && w < gp.stack.hi {
		return zgcSyscallRefStack
	}
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		if w >= datap.data && w < datap.edata {
			return zgcSyscallRefGlobal
		}
		if w >= datap.bss && w < datap.ebss {
			return zgcSyscallRefGlobal
		}
	}
	return zgcSyscallRefOther
}

// zgcSyscallVisit records one mask-proven pointer cell and follows its
// target toward layout discovery. Heap targets need nothing further:
// the registry closure expands them authoritatively from the recorded
// root.
func zgcSyscallVisit(slot *uintptr) {
	a := &zgcSyscallSTWArgs
	if !a.ok {
		return
	}
	cell := uintptr(unsafe.Pointer(slot))
	if isNew, ok := zgcForeignSetAddRef(&a.seenR, cell); !ok {
		a.ok = false
		return
	} else if !isNew {
		return
	}
	zgcSyscallAppendRoot(cell)
	if !a.ok {
		return
	}
	zgcSyscallFollow(*slot)
}

// zgcSyscallFollow routes a proven pointer target toward its layout:
// stack targets queue referent expansion, global targets queue their
// containing module. Heap and foreign targets need nothing further.
func zgcSyscallFollow(t uintptr) {
	a := &zgcSyscallSTWArgs
	switch zgcSyscallClassify(a.gp, t) {
	case zgcSyscallRefStack:
		zgcSyscallAppendRef(t, nil)
	case zgcSyscallRefGlobal:
		zgcSyscallQueueModule(t)
	}
}

// appendRoot records a proven cell address.
func zgcSyscallAppendRoot(cell uintptr) {
	a := &zgcSyscallSTWArgs
	if a.nr >= a.cr && !zgcSyscallGrow(&a.roots, &a.cr, a.nr, unsafe.Sizeof(uintptr(0))) {
		a.ok = false
		return
	}
	*(*uintptr)(add(unsafe.Pointer(a.roots), a.nr*unsafe.Sizeof(uintptr(0)))) = cell
	a.nr++
}

// appendRef queues a referent for layout expansion, deduplicated by
// address. Seeds run before the breadth-first loop, so a typed seed
// always wins over a later untyped re-queue of the same address.
func zgcSyscallAppendRef(addr uintptr, typ unsafe.Pointer) {
	a := &zgcSyscallSTWArgs
	if isNew, ok := zgcForeignSetAddRef(&a.seenW, addr); !ok {
		a.ok = false
		return
	} else if !isNew {
		return
	}
	if a.nw >= a.cw && !zgcSyscallGrow(&a.refs, &a.cw, a.nw, 2*unsafe.Sizeof(uintptr(0))) {
		a.ok = false
		return
	}
	p := add(unsafe.Pointer(a.refs), a.nw*2*unsafe.Sizeof(uintptr(0)))
	*(*uintptr)(p) = addr
	*(*uintptr)(add(p, unsafe.Sizeof(uintptr(0)))) = uintptr(typ)
	a.nw++
}

// queueModule queues the module containing a proven global pointer for
// a bounded mask walk. Modules are deduplicated; every global cell of
// each queued module is visited, so cross-module global-to-global-to-
// heap chains close without ever scanning modules that hold no queued
// referent.
func zgcSyscallQueueModule(addr uintptr) {
	a := &zgcSyscallSTWArgs
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		if (addr >= datap.data && addr < datap.edata) || (addr >= datap.bss && addr < datap.ebss) {
			if isNew, ok := zgcForeignSetAddRef(&a.seenM, uintptr(unsafe.Pointer(datap))); !ok {
				a.ok = false
				return
			} else if !isNew {
				return
			}
			if a.nm >= a.cm && !zgcSyscallGrow(&a.mods, &a.cm, a.nm, unsafe.Sizeof(uintptr(0))) {
				a.ok = false
				return
			}
			*(*uintptr)(add(unsafe.Pointer(a.mods), a.nm*unsafe.Sizeof(uintptr(0)))) = uintptr(unsafe.Pointer(datap))
			a.nm++
			return
		}
	}
}

// zgcSyscallGrow doubles off-heap scratch, preserving contents. False
// reports exhaustion with nothing else mutated; the admission fails
// closed. Only address-space overflow and allocation failure stop
// growth: there is no arbitrary capacity, so lawful closures of any
// size admit.
func zgcSyscallGrow(base *uintptr, cap *uintptr, n, elem uintptr) bool {
	if *cap > (^uintptr(0)/elem)/2 {
		return false
	}
	newCap := *cap * 2
	if newCap == 0 {
		newCap = 64
	}
	raw := sysAlloc(newCap*elem, &memstats.other_sys, "logos zgc syscall work")
	if raw == nil {
		return false
	}
	for i := uintptr(0); i < n; i++ {
		*(*uintptr)(add(unsafe.Pointer(raw), i*elem)) = *(*uintptr)(add(unsafe.Pointer(*base), i*elem))
	}
	if *cap != 0 {
		sysFree(unsafe.Pointer(*base), *cap*elem, &memstats.other_sys)
	}
	*base = uintptr(raw)
	*cap = newCap
	return true
}

// zgcSyscallWorkFree releases all discovery scratch. The registry roots
// built from it were consumed synchronously by the admission core, so
// nothing here is retained afterwards.
func zgcSyscallWorkFree() {
	a := &zgcSyscallSTWArgs
	ptrSize := unsafe.Sizeof(uintptr(0))
	if a.cr != 0 {
		sysFree(unsafe.Pointer(a.roots), a.cr*ptrSize, &memstats.other_sys)
		a.roots, a.nr, a.cr = 0, 0, 0
	}
	if a.cw != 0 {
		sysFree(unsafe.Pointer(a.refs), a.cw*2*ptrSize, &memstats.other_sys)
		a.refs, a.nw, a.cw = 0, 0, 0
	}
	if a.cm != 0 {
		sysFree(unsafe.Pointer(a.mods), a.cm*ptrSize, &memstats.other_sys)
		a.mods, a.nm, a.cm = 0, 0, 0
	}
	zgcForeignSetDestroy(&a.seenR)
	zgcForeignSetDestroy(&a.seenW)
	zgcForeignSetDestroy(&a.seenM)
}

// zgcSyscallFrameAbandon pops an unpublished frame after a failed
// admission, mirroring the unwind in zgcForeignBeginLocked. The
// admission core already rolled back any partial entries.
func zgcSyscallFrameAbandon(gp *g, f *zgcForeignFrame) {
	gp.logosForeignTop = f.parent
	gp.logosForeignDepth--
	zgcForeignFrameDestroyOwned(f)
	if gp.logosForeignSpare == 0 {
		*f = zgcForeignFrame{}
		gp.logosForeignSpare = uintptr(unsafe.Pointer(f))
	} else {
		sysFree(unsafe.Pointer(f), unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys)
	}
}

// zgcSyscallOnSystemstack runs the stopped-world core: freshness
// assert, frame setup, chain discovery, single-owner admission,
// publish. It reports through the staging; the caller throws fail-loud
// after the restart when ok is false. No locks are taken: worldsema
// exclusion covers the shared sets, and off-heap scratch is
// frame-private.
func zgcSyscallOnSystemstack() {
	a := &zgcSyscallSTWArgs
	gp, rows, n := a.gp, a.rows, a.n
	if a.sp < gp.stack.lo || a.sp >= gp.stack.hi {
		// Unreachable by construction: no calls separate the
		// post-stop capture from this switch, so this stack cannot
		// have moved. Assert the impossible state instead of
		// retrying or covering it up.
		throw("logos zgc: syscall admission stack moved during setup")
	}
	var f *zgcForeignFrame
	if gp.logosForeignSpare != 0 {
		f = (*zgcForeignFrame)(unsafe.Pointer(gp.logosForeignSpare))
		gp.logosForeignSpare = 0
		*f = zgcForeignFrame{}
	} else {
		f = zgcForeignFrameAlloc()
		if f == nil {
			return
		}
	}
	f.owner = uintptr(unsafe.Pointer(gp))
	f.gen = zgcForeignNextGen()
	f.parent = gp.logosForeignTop
	gp.logosForeignTop = uintptr(unsafe.Pointer(f))
	gp.logosForeignDepth++
	a.ok = true
	if !zgcSyscallDiscover(rows, n) {
		zgcSyscallFrameAbandon(gp, f)
		zgcSyscallWorkFree()
		a.ok = false
		return
	}
	total := n + a.nr
	if total < n {
		zgcSyscallFrameAbandon(gp, f)
		zgcSyscallWorkFree()
		a.ok = false
		return
	}
	rsz := unsafe.Sizeof(logosForeignRoot{})
	if total > ^uintptr(0)/rsz {
		zgcSyscallFrameAbandon(gp, f)
		zgcSyscallWorkFree()
		a.ok = false
		return
	}
	rbase := sysAlloc(total*rsz, &memstats.other_sys, "logos zgc syscall roots")
	if rbase == nil {
		zgcSyscallFrameAbandon(gp, f)
		zgcSyscallWorkFree()
		a.ok = false
		return
	}
	for i := uintptr(0); i < n; i++ {
		*(*logosForeignRoot)(add(rbase, i*rsz)) = logosForeignRoot{Slot: zgcSyscallSlotAt(rows, i), Kind: logosForeignRootUnknown}
	}
	for j := uintptr(0); j < a.nr; j++ {
		cell := *(*uintptr)(add(unsafe.Pointer(a.roots), j*unsafe.Sizeof(uintptr(0))))
		*(*logosForeignRoot)(add(rbase, (n+j)*rsz)) = logosForeignRoot{Slot: unsafe.Pointer(cell), Kind: logosForeignRootUnknown}
	}
	ok := zgcForeignAdmitSTW(f, (*logosForeignRoot)(rbase), total)
	sysFree(rbase, total*rsz, &memstats.other_sys)
	if !ok {
		zgcSyscallFrameAbandon(gp, f)
		zgcSyscallWorkFree()
		a.ok = false
		return
	}
	zgcForeignPublish(f)
	zgcSyscallWorkFree()
	a.tok = logosForeignToken{Frame: unsafe.Pointer(f), Gen: f.gen}
	a.ok = true
}

// zgcSyscallDiscover seeds temp roots plus typed expansion work, then
// drains the referent and module worklists breadth-first. Every
// recorded cell is mask-proven (temp by construction, the rest by
// object, type, or module bitmaps); heap referents need no walk
// because the registry closure expands them authoritatively from their
// roots.
func zgcSyscallDiscover(rows unsafe.Pointer, n uintptr) bool {
	a := &zgcSyscallSTWArgs
	for i := uintptr(0); i < n && a.ok; i++ {
		slot := zgcSyscallSlotAt(rows, i)
		cell := uintptr(slot)
		if isNew, ok := zgcForeignSetAddRef(&a.seenR, cell); !ok {
			a.ok = false
			break
		} else if isNew {
			zgcSyscallAppendRoot(cell)
		}
		if !a.ok {
			break
		}
		if word := *(*uintptr)(slot); true {
			if k := zgcSyscallClassify(a.gp, word); k == zgcSyscallRefStack || k == zgcSyscallRefGlobal {
				zgcSyscallAppendRef(word, zgcSyscallTypeAt(rows, i))
			}
		}
	}
	ptrSize := unsafe.Sizeof(uintptr(0))
	wi, mi := uintptr(0), uintptr(0)
	for a.ok && (wi < a.nw || mi < a.nm) {
		for wi < a.nw && a.ok {
			p := add(unsafe.Pointer(a.refs), wi*2*ptrSize)
			addr := *(*uintptr)(p)
			typ := unsafe.Pointer(*(*uintptr)(add(p, ptrSize)))
			wi++
			switch zgcSyscallClassify(a.gp, addr) {
			case zgcSyscallRefHeap:
				// Covered by the registry closure; nothing to walk.
			case zgcSyscallRefStack:
				if !zgcSyscallContained(addr) && typ != nil {
					zgcSyscallTyped(addr, typ)
				}
			case zgcSyscallRefGlobal:
				if typ != nil {
					zgcSyscallTyped(addr, typ)
				} else {
					zgcSyscallQueueModule(addr)
				}
			}
		}
		for mi < a.nm && a.ok {
			md := (*moduledata)(unsafe.Pointer(*(*uintptr)(add(unsafe.Pointer(a.mods), mi*ptrSize))))
			mi++
			zgcSyscallScanModule(md)
		}
	}
	return a.ok
}

// zgcSyscallContained walks the caller's frozen frames for the stack
// object containing addr and visits its pointer cells through the
// collector's own object walker. Unwinding starts at the staged caller
// PC/SP (the callers()-proven pattern) with silent errors, and
// terminates exactly like the collector's stack walk: no frame bound,
// because lawful stacks always bottom out at goexit. A first hit wins;
// objects never overlap. Corrupt funcdata fails the admission closed,
// never silently truncates.
func zgcSyscallContained(addr uintptr) bool {
	a := &zgcSyscallSTWArgs
	var u unwinder
	u.initAt(a.pc, a.sp, 0, a.gp, unwindSilentErrors)
	ptrSize := unsafe.Sizeof(uintptr(0))
	hit := false
	for u.valid() && a.ok {
		p := funcdata(u.frame.fn, abi.FUNCDATA_StackObjects)
		if p == nil || u.frame.varp == 0 {
			u.next()
			continue
		}
		nrec := *(*uintptr)(p)
		if nrec > 1<<20 {
			a.ok = false
			return false
		}
		rp := add(p, ptrSize)
		rsz := unsafe.Sizeof(stackObjectRecord{})
		for i := uintptr(0); i < nrec; i++ {
			r := (*stackObjectRecord)(add(rp, i*rsz))
			if lo, hi, ok := zgcSyscallObjRange(u.frame.varp, u.frame.argp, r); !ok {
				a.ok = false
				return false
			} else if addr >= lo && addr < hi {
				hit = true
				logosWalkStackObject(&u.frame, r, zgcSyscallVisit)
				if !a.ok {
					return false
				}
			}
		}
		u.next()
	}
	return hit && a.ok
}

// zgcSyscallObjRange maps one stack-object record to its address
// extent. False reports corrupt metadata; the admission fails closed.
func zgcSyscallObjRange(varp, argp uintptr, r *stackObjectRecord) (uintptr, uintptr, bool) {
	o := int64(r.off)
	var b int64
	if o >= 0 {
		b = int64(argp) + o
	} else {
		b = int64(varp) + o
	}
	if b < 0 {
		return 0, 0, false
	}
	sz := int64(r.size)
	if sz <= 0 {
		return 0, 0, sz == 0
	}
	if uint64(sz) > uint64(^uintptr(0))-uint64(b) {
		return 0, 0, false
	}
	return uintptr(b), uintptr(b) + uintptr(sz), true
}

// zgcSyscallTyped walks one referent value of known layout, visiting
// each pointer cell. Used for globals (no per-symbol bounds exist in
// the runtime) and as fallback for stack referents in no recorded
// object. The mask is prebuilt before the stop, so the nosplit read
// below only observes it; an unbuilt mask throws fail-loud instead of
// building inside the stop.
func zgcSyscallTyped(addr uintptr, typ unsafe.Pointer) {
	a := &zgcSyscallSTWArgs
	t := (*_type)(typ)
	if t.Size_ == 0 {
		return
	}
	if t.Size_ > ^uintptr(0)-addr {
		a.ok = false
		return
	}
	limit := addr + t.Size_
	tp := zgcForeignTypeCellsNoSplit(t, addr)
	for a.ok {
		var cell uintptr
		tp, cell = tp.next(limit)
		if cell == 0 {
			return
		}
		zgcSyscallVisit((*uintptr)(unsafe.Pointer(cell)))
	}
}

// zgcSyscallScanModule visits every mask-proven pointer word of one
// module's data and bss. Discovered stack targets queue referent
// expansion and discovered global targets queue their own modules, so
// the reachable global subgraph closes transitively.
func zgcSyscallScanModule(datap *moduledata) {
	a := &zgcSyscallSTWArgs
	zgcSyscallWalkMask(datap.data, datap.edata, datap.gcdatamask.bytedata)
	if a.ok {
		zgcSyscallWalkMask(datap.bss, datap.ebss, datap.gcbssmask.bytedata)
	}
}

// walkMask visits every set pointer word in [start, end) using the same
// bit convention the collector's block walker uses, with early exit on
// exhaustion.
func zgcSyscallWalkMask(start, end uintptr, mask *byte) {
	a := &zgcSyscallSTWArgs
	ptrSize := unsafe.Sizeof(uintptr(0))
	n := end - start
	for i := uintptr(0); i < n && a.ok; {
		bits := uint32(*addb(mask, i/(ptrSize*8)))
		if bits == 0 {
			i += ptrSize * 8
			continue
		}
		for j := 0; j < 8 && i < n && a.ok; j++ {
			if bits&1 != 0 {
				zgcSyscallVisit((*uintptr)(unsafe.Pointer(start + i)))
			}
			bits >>= 1
			i += ptrSize
		}
	}
}
