// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// Shared precise-rewrite walk, ported from fix-M19ImmixComplete
// logos_evac_fix.go. The traversal below is verbatim; the With-variants
// add a caller policy so one core serves the immix moved-object skip
// and the zgc quarantine skip plus post-sweep alloc-bit liveness.
// Merge note: keep every logosWalk* name and body identical to the
// immix lane; the policy struct and With-variants are additive.

// logosSlotVisitor receives the address of every word that holds a pointer
// the evacuation must rewrite, or check, after objects moved.
type logosSlotVisitor func(slot *uintptr)

// logosFwdSlot returns the forwarding entry of object i of an evacuated span.
func (s *mspan) logosFwdSlot(i uintptr) *uintptr {
	return (*uintptr)(unsafe.Pointer(s.logosFwd + i*goarch.PtrSize))
}

// logosForward returns the address in the to-space for p when p points into an
// object that the running evacuation moved. Interior pointers keep their
// offset.
func logosForward(p uintptr) (uintptr, bool) {
	if p == 0 {
		return 0, false
	}
	s := spanOf(p)
	if s == nil || s.logosFwd == 0 {
		return 0, false
	}
	// Invariant: span metadata must outlive every record that names it. A
	// forwarding record exists only while the world is stopped inside the
	// evacuation that owns it, so its span is live and manual spans never
	// carry one. A retired span naming a record is impossible: freeSpanLocked
	// clears the record at span death and finish clears every record before
	// freeing the table. Fail fast instead of reading retired span-class
	// metadata (base, limit, elemsize, nelems) or the freed table.
	if s.state.get() != mSpanInUse {
		throw("logos evacuation: forwarding record on retired span")
	}
	if p < s.base() || p >= s.limit {
		return 0, false
	}
	i := s.objIndex(p)
	if i >= uintptr(s.nelems) {
		throw("logos evacuation: forwarding index out of range")
	}
	f := *s.logosFwdSlot(i)
	if f == 0 {
		return 0, false
	}
	return f + (p - (s.base() + i*s.elemsize)), true
}

// logosFixSlot rewrites a reference to a moved object.
func logosFixSlot(slot *uintptr) {
	if np, ok := logosForward(*slot); ok {
		*slot = np
	}
}

// logosVerifySlot throws when a reference to a moved object remains.
func logosVerifySlot(slot *uintptr) {
	if _, ok := logosForward(*slot); ok {
		print("runtime: slot ", slot, " still holds evacuated pointer ", hex(*slot), "\n")
		throw("logos evacuation: stale pointer to evacuated object")
	}
}

// logosWalkPolicy selects which spans and objects a parameterized walk
// visits. The zero value reproduces the immix traversal exactly:
// in-use, scannable, non-arena spans; marked, unmoved objects.
type logosWalkPolicy struct {
	// skipSpan reports whether to skip the whole span.
	skipSpan func(*mspan) bool
	// useAllocBits reads liveness from the allocation bits instead of
	// the mark bits (post-sweep, when the mark bits are fresh).
	useAllocBits bool
}

// logosWalkAll visits every reference the collector knows: the heap, the
// global data, the specials, the finalizer and cleanup queues, and the stacks.
// Stacks run last because the walk reads goroutine links the heap pass fixed.
func logosWalkAll(v logosSlotVisitor) {
	logosWalkHeap(v)
	logosWalkGlobals(v)
	logosWalkSpecials(v)
	logosWalkQueues(v)
	logosWalkStacks(v)
}

// logosWalkAllWith is logosWalkAll with a caller policy.
func logosWalkAllWith(v logosSlotVisitor, pol logosWalkPolicy) {
	logosWalkHeapWith(v, pol)
	logosWalkGlobals(v)
	logosWalkSpecials(v)
	logosWalkQueues(v)
	logosWalkStacks(v)
}

// logosWalkBlock visits the pointer words of [b, b+n) selected by ptrmask.
func logosWalkBlock(b, n uintptr, ptrmask *uint8, v logosSlotVisitor) {
	for i := uintptr(0); i < n; {
		bits := uint32(*addb(ptrmask, i/(goarch.PtrSize*8)))
		if bits == 0 {
			i += goarch.PtrSize * 8
			continue
		}
		for j := 0; j < 8 && i < n; j++ {
			if bits&1 != 0 {
				v((*uintptr)(unsafe.Pointer(b + i)))
			}
			bits >>= 1
			i += goarch.PtrSize
		}
	}
}

func logosWalkGlobals(v logosSlotVisitor) {
	for datap := &firstmoduledata; datap != nil; datap = datap.next {
		logosWalkBlock(datap.data, datap.edata-datap.data, datap.gcdatamask.bytedata, v)
		logosWalkBlock(datap.bss, datap.ebss-datap.bss, datap.gcbssmask.bytedata, v)
	}
}

// logosMoved reports whether object i of s was evacuated.
func logosMoved(s *mspan, i uintptr) bool {
	return s.logosFwd != 0 && *s.logosFwdSlot(i) != 0
}

func logosWalkHeap(v logosSlotVisitor) {
	for _, s := range mheap_.allspans {
		if s.state.get() == mSpanInUse && !s.spanclass.noscan() && !s.isUserArenaChunk {
			logosWalkSpan(s, v)
		}
	}
}

// logosWalkHeapWith is logosWalkHeap with a caller policy.
func logosWalkHeapWith(v logosSlotVisitor, pol logosWalkPolicy) {
	for _, s := range mheap_.allspans {
		if s.state.get() != mSpanInUse || s.spanclass.noscan() || s.isUserArenaChunk {
			continue
		}
		if pol.skipSpan != nil && pol.skipSpan(s) {
			continue
		}
		logosWalkSpanWith(s, v, pol)
	}
}

func logosWalkSpan(s *mspan, v logosSlotVisitor) {
	if s.spanclass.sizeclass() == 0 {
		if s.markBitsForIndex(0).isMarked() {
			v((*uintptr)(unsafe.Pointer(&s.largeType)))
			logosWalkObject(s, s.base(), v)
		}
		return
	}
	for i := uintptr(0); i < uintptr(s.nelems); i++ {
		if s.markBitsForIndex(i).isMarked() && !logosMoved(s, i) {
			logosWalkObject(s, s.base()+i*s.elemsize, v)
		}
	}
}

// logosWalkSpanWith is logosWalkSpan with a caller policy. Unlike the
// immix walk it does not consult the evacuated-forwarding table: the
// policy decides object selection, so one core serves collectors that
// forward differently.
func logosWalkSpanWith(s *mspan, v logosSlotVisitor, pol logosWalkPolicy) {
	if s.spanclass.sizeclass() == 0 {
		live := s.markBitsForIndex(0).isMarked()
		if pol.useAllocBits {
			live = s.allocCount > 0
		}
		if live {
			v((*uintptr)(unsafe.Pointer(&s.largeType)))
			logosWalkObject(s, s.base(), v)
		}
		return
	}
	for i := uintptr(0); i < uintptr(s.nelems); i++ {
		live := s.markBitsForIndex(i).isMarked()
		if pol.useAllocBits {
			// The sweep's allocated view: a fast-path hand-out bumps
			// freeindex without writing allocBits, so a slot below
			// freeindex is allocated even with a clear bit (sweep
			// resets freeindex to 0, so holes cannot sit below it).
			live = i < uintptr(s.freeindex) || s.allocBitsForIndex(i).isMarked()
		}
		if live {
			logosWalkObject(s, s.base()+i*s.elemsize, v)
		}
	}
}

// logosWalkObject visits the pointer slots of the object slot at base, the
// malloc header type word included. The precise type information is the one
// the collector scans with.
func logosWalkObject(s *mspan, base uintptr, v logosSlotVisitor) {
	if s.spanclass.sizeclass() != 0 && !heapBitsInSpan(s.elemsize) {
		v((*uintptr)(unsafe.Pointer(base)))
	}
	tp := s.typePointersOfUnchecked(base)
	limit := base + s.elemsize
	for {
		var addr uintptr
		if tp, addr = tp.next(limit); addr == 0 {
			return
		}
		v((*uintptr)(unsafe.Pointer(addr)))
	}
}

func logosWalkSpecials(v logosSlotVisitor) {
	for _, s := range mheap_.allspans {
		if s.state.get() != mSpanInUse {
			continue
		}
		for sp := s.specials; sp != nil; sp = sp.next {
			logosWalkSpecial(s, sp, v)
		}
	}
}

// logosWalkSpecial visits the heap references of a special, which lives off
// the heap and is a root. The object of a finalizer special is walked too:
// the sweep resurrects it even when it is unmarked.
func logosWalkSpecial(s *mspan, sp *special, v logosSlotVisitor) {
	switch sp.kind {
	case _KindSpecialFinalizer:
		spf := (*specialfinalizer)(unsafe.Pointer(sp))
		v((*uintptr)(unsafe.Pointer(&spf.fn)))
		v((*uintptr)(unsafe.Pointer(&spf.fint)))
		v((*uintptr)(unsafe.Pointer(&spf.ot)))
		if !s.spanclass.noscan() {
			logosWalkObject(s, s.base()+sp.offset/s.elemsize*s.elemsize, v)
		}
	case _KindSpecialWeakHandle:
		v((*uintptr)(unsafe.Pointer(&(*specialWeakHandle)(unsafe.Pointer(sp)).handle)))
	case _KindSpecialCleanup:
		spc := (*specialCleanup)(unsafe.Pointer(sp))
		logosWalkBlock(uintptr(unsafe.Pointer(&spc.cleanup)), unsafe.Sizeof(cleanupFn{}), &cleanupFnPtrMask[0], v)
	}
}

// logosWalkQueues visits the queued finalizers and the queued cleanups with
// the masks the collector scans them with.
func logosWalkQueues(v logosSlotVisitor) {
	for fb := allfin; fb != nil; fb = fb.alllink {
		logosWalkBlock(uintptr(unsafe.Pointer(&fb.fin[0])), uintptr(fb.cnt)*unsafe.Sizeof(fb.fin[0]), &finptrmask[0], v)
	}
	for cb := (*cleanupBlock)(gcCleanups.all.Load()); cb != nil; cb = cb.alllink {
		logosWalkBlock(uintptr(unsafe.Pointer(&cb.cleanups[0])), uintptr(cb.n)*unsafe.Sizeof(cleanupFn{}), &cleanupBlockPtrMask[0], v)
	}
}

// logosWalkStacks visits the arena scan regions of every live non-running
// goroutine plus the stack roots of every goroutine whose stack the pin
// pass did not scan conservatively. Arena slots of each walked goroutine
// are always rewritten: unlike stack referents, nothing pins what an arena
// names. It does not walk a running goroutine, whose frames change under
// the walk.
//
// The zgc callers reach the skip: they run at gcStart, where the goroutine that
// stopped the world is still _Grunning, zgcGCStartHook pins what that stack
// names (zgcPinSelfStack), and a load barrier heals the rest.
//
// The immix walk never reaches it, and nothing heals a slot it would skip.
// logosEvacuate runs from gcMarkTermination after that function parked its
// caller in _Gwaiting (casGToWaitingForSuspendG), and stopTheWorldWithSema
// throws unless every P is _Pgcstop, so no other goroutine is _Grunning.
// collectPins, the gate of run and so of logosWalkAll, also returns false for
// _Grunning, _Gidle and _Gcopystack. A change that lets a goroutine run during
// the evacuation must pin its stack conservatively before it moves anything.
func logosWalkStacks(v logosSlotVisitor) {
	for _, gp := range allgs {
		switch readgstatus(gp) &^ _Gscan {
		case _Gdead, _Gdeadextra, _Grunning:
			continue
		}
		logosWalkArena(gp, v)
		if !logosStackConservative(gp) {
			logosWalkStack(gp, v)
		}
	}
}

// logosWalkArena visits the pointer slots of the scan objects in every
// chunk leased to gp. The scan region holds heap pointers the marking
// pass shades through logosArenaScanStack, so the evacuation must rewrite
// them too, or arena-held references stay stale after objects move. The
// walk is unconditional: a goroutine whose stack the pin pass scanned
// conservatively keeps its stack referents pinned, but nothing pins what
// its arena names, so its arena slots still need the rewrite. Chunks
// themselves are manual spans and never move; only the heap objects their
// scan regions point to do.
func logosWalkArena(gp *g, v logosSlotVisitor) {
	for c := gp.logosArena; c != nil; c = c.next {
		for hdrAddr := c.scanCur; hdrAddr < c.scanBase; {
			hdr := (*logosArenaScanHeader)(unsafe.Pointer(hdrAddr))
			obj := hdrAddr + logosArenaScanHeaderBytes
			hdrAddr = obj + alignUp(hdr.size, logosArenaUnit)
			if hdr.typ == nil || hdr.size == 0 {
				continue
			}
			logosWalkArenaObject(hdr.typ, obj, hdr.size, v)
		}
	}
}

// logosWalkArenaObject visits the pointer slots of one arena scan object:
// size/typ.Size_ consecutive elements of typ at obj. It mirrors
// logosArenaScanObject, except the caller rewrites slots instead of
// shading referents, so stack-range and span lookups are unnecessary:
// slots holding non-heap words simply have no forwarding entry.
func logosWalkArenaObject(typ *_type, obj, size uintptr, v logosSlotVisitor) {
	limit := obj + size
	// nextFast trusts the mask, and next clips to limit only when it
	// advances, so the first mask must be clipped here, exactly as the
	// marking scan does.
	tp := typePointers{elem: obj, addr: obj, mask: readUintptr(getGCMask(typ)), typ: typ}.fastForward(0, limit)
	for {
		var addr uintptr
		if tp, addr = tp.nextFast(); addr == 0 {
			if tp, addr = tp.next(limit); addr == 0 {
				return
			}
		}
		v((*uintptr)(unsafe.Pointer(addr)))
	}
}

// logosWalkStack visits the precise pointer slots of the frames of gp, its
// stack objects, and its defer records. A goroutine whose stack the pin pass
// scanned conservatively is not walked: its referents did not move.
func logosWalkStack(gp *g, v logosSlotVisitor) {
	if gp.sched.ctxt != nil {
		v((*uintptr)(unsafe.Pointer(&gp.sched.ctxt)))
	}
	var u unwinder
	for u.init(gp, 0); u.valid(); u.next() {
		logosWalkFrame(&u.frame, v)
	}
	if gp._defer != nil {
		v((*uintptr)(unsafe.Pointer(&gp._defer)))
	}
	for d := gp._defer; d != nil; d = d.link {
		v((*uintptr)(unsafe.Pointer(&d.fn)))
		v((*uintptr)(unsafe.Pointer(&d.link)))
		if d.head != nil {
			v((*uintptr)(unsafe.Pointer(&d.head)))
		}
	}
	for p := gp._panic; p != nil; p = p.link {
		// p.arg is an empty interface. Whenever it holds a value its data
		// word is a pointer: the value itself for a direct type (a type
		// stored in the word, abi.TFlagDirectIface, is pointer-shaped), the
		// address of a box for any other type. The box holds the value even
		// when the type has no pointers, and it can move, so Pointers() is
		// not the test. The type word is not a heap pointer slot.
		if e := efaceOf(&p.arg); e._type != nil {
			v((*uintptr)(unsafe.Pointer(&e.data)))
		}
	}
}

func logosWalkFrame(frame *stkframe, v logosSlotVisitor) {
	locals, args, objs := frame.getStackMap(false)
	if locals.n > 0 {
		size := uintptr(locals.n) * goarch.PtrSize
		logosWalkBlock(frame.varp-size, size, locals.bytedata, v)
	}
	if args.n > 0 {
		logosWalkBlock(frame.argp, uintptr(args.n)*goarch.PtrSize, args.bytedata, v)
	}
	if frame.varp == 0 {
		return
	}
	for i := range objs {
		logosWalkStackObject(frame, &objs[i], v)
	}
}

func logosWalkStackObject(frame *stkframe, obj *stackObjectRecord, v logosSlotVisitor) {
	base := frame.varp
	if obj.off >= 0 {
		base = frame.argp
	}
	ptr := base + uintptr(obj.off)
	if ptr < frame.sp {
		return
	}
	ptrBytes, gcData := obj.gcdata()
	logosWalkBlock(ptr, ptrBytes, gcData, v)
}
