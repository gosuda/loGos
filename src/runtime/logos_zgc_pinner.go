// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

//go:nosplit
func logosZgcPinnerCanonicalize(ptr unsafe.Pointer) unsafe.Pointer {
	if ptr == nil {
		return nil
	}
	addr := uintptr(ptr)
	colors := atomic.Loaduintptr(&zgcColorMaskBits)
	if colors != 0 {
		if spanOfHeap(addr) == nil {
			a := addr &^ colors
			if a == 0 || a > zgcAddrMask {
				return ptr
			}
			pg := logosZgcPinnerPageFor(a)
			if pg == nil && spanOfHeap(a) == nil {
				return ptr
			}
			addr = a
		}
	}
	pg := logosZgcPinnerPageFor(addr)
	if pg == nil {
		return unsafe.Pointer(addr)
	}
	var base uintptr
	var off uintptr
	if s := spanOfHeap(addr); s != nil {
		idx := s.objIndex(addr)
		base = s.startAddr + idx*s.elemsize
		off = addr - base
	} else {
		base = addr
		off = 0
	}
	if base < pg.start || base-pg.start >= pg.size {
		return unsafe.Pointer(addr)
	}
	if to, ok := logosZgcPinnerForward(pg, base); ok {
		return unsafe.Pointer(to + off)
	}
	return unsafe.Pointer(addr)
}

// logosZgcPinnerPageFor is the nosplit form of zgcPageFor: the registered
// page whose source ranges contain addr, or nil.
//
//go:nosplit
func logosZgcPinnerPageFor(addr uintptr) *zgcPage {
	if addr > zgcAddrMask {
		return nil
	}
	p := atomic.Loaduintptr(&zgcDirAddr)
	if p == 0 {
		return nil
	}
	slot := (*uintptr)(add(unsafe.Pointer(p), (addr>>zgcPageShift)*unsafe.Sizeof(uintptr(0))))
	v := atomic.Loaduintptr(slot)
	if v == 0 {
		return nil
	}
	pg := (*zgcPage)(unsafe.Pointer(v))
	if !pg.holdsSource(addr) {
		return nil
	}
	return pg
}

//go:nosplit
func logosZgcPinnerForward(pg *zgcPage, base uintptr) (uintptr, bool) {
	if base < pg.start || base-pg.start >= pg.size {
		return 0, false
	}
	idx := uint64((base - pg.start) >> zgcGranuleShift)
	i := uintptr(idx*zgcFwdHashMul>>pg.fwdShift) & pg.fwdMask
	for range pg.fwdMask + 1 {
		e := atomic.Load64((*uint64)(add(pg.fwd, i*8)))
		if e == 0 {
			return 0, false
		}
		if uint64(e>>zgcFwdIndexShift&zgcFwdIndexMask) == idx {
			return uintptr(uint64(e) & uint64(zgcFwdToMask)), true
		}
		i = (i + 1) & pg.fwdMask
	}
	return 0, false
}

// logosZgcPinPrepare resolves the stable home of the object containing p
// for a strong pin. It is the one deep preparation shared by
// logosZgcPinnerPin and logosPinSignalObject.
//
// The copy runs before the M is held, and only when the zgc cycle runs
// and the source is an unpublished object on a registered page: a
// published copy is adopted through the forwarding table with no
// allocation in any context, and only an unpublished copy needs the
// preemptible deep path. The typed Result primitive keeps exhaustion and
// lawful freezes distinguishable at observation: claim failure is only
// NoMemory observed at the allocator, commit 0 is only Refused observed
// under the gate, missing heap or page is NotHeap. Large objects never
// copy: the large relocation path returns the base on every path without
// allocating, so it bypasses the preemptibility gate exactly as the old
// ensure-copy path did. With the M held the final span is resolved and
// swept and the home is verified stable; a stability refusal witnesses a
// pause that began after the copy, so the exact M acquired is released and
// the loop copies again. Retries happen only for that real pause
// instability witnessed by the stability check: already-forwarded,
// unregistered, and non-heap objects settle on the first pass. No copy ever
// runs under the M, so the preemptibility guard is never bypassed, and the
// M stays held across the caller's speciallock-then-gate commit, preserving
// M -> speciallock -> gate order with the gate leaf-last.
//
// Throw discipline: only NoMemory throws out-of-memory (plus the pre-existing
// non-relocatable-context throw, which is a caller context, not a relocation
// outcome). NotHeap uses the source via the lifetime protocol (unregistered
// sources are never moved by workers, so pinning the source with the M held
// is stable). Refused is never retried until unexposure and never misthrown
// as OOM: retrying a foreign-exposed source until its scope ends would
// self-deadlock a public Pinner.Pin inside a callback on the current G's
// active C-exposed object, because the scope cannot end until the callback
// returns. Refused pins the source in place below with the M held for the
// caller's commit (pinner bits or the sticky flag, M -> speciallock -> gate
// preserved, no copy under M), so the lawful callback use proceeds with no
// deadlock and no OOM while its exposure is active. The gate-held pin-bit
// veto in zgcRelocateObject and the gate-held forward recheck in the
// caller's commit close the claim-before-pin race in both orders; no
// whitelist, no return-false-without-pin, no weak sticky, and no new
// runtime panic is added here. Interior offsets survive end to end.
//
// On success the stable home is returned as a typed pointer with the M
// still held; the caller commits its pin (pinner bits or the sticky span
// flag) and releases mp. A nil mp means the M is already released and
// there is nothing to commit: p names no heap object.
func logosZgcPinPrepare(p unsafe.Pointer) (home unsafe.Pointer, s *mspan, mp *m) {
	if p == nil {
		return nil, nil, nil
	}
	for {
		hint := logosZgcPinnerCanonicalize(p)
		hintAddr := uintptr(hint)
		hintBase := hintAddr
		var off uintptr
		large := false
		if sHint := spanOfHeap(hintAddr); sHint != nil {
			large = sHint.spanclass.sizeclass() == 0
			hintIdx := sHint.objIndex(hintAddr)
			hintBase = sHint.startAddr + hintIdx*sHint.elemsize
			off = hintAddr - hintBase
		}
		base := hintBase
		var refused bool
		var refusedBase uintptr
		if zgcCycleEnabled() {
			if pg := zgcPageFor(base); pg != nil {
				if to, ok := pg.forward(base); ok {
					base = to
				} else if large {
					// Large never copies; base stays for the M-held stable path.
				} else if !zgcCanRelocateDeep() {
					throw("logos zgc: cannot pin object in non-relocatable context")
				} else if to, st := zgcRelocateOneResult(pg, base); st == zgcRelocateDone {
					base = to
				} else if st == zgcRelocateNoMemory {
					throw("logos zgc: out of memory pinning object")
				} else if st == zgcRelocateNotHeap {
					// Nil page/span only (defensive; Prepare calls Result
					// with non-nil pg on a heap base). Unregistered,
					// out-of-range, and holds races map to Refused via
					// commit 0 and take the pin-in-place path below, not
					// this branch. Use the source via AttachStable.
				} else {
					// Refused: frozen (exposed) source. Never retry until
					// unexposure (a public Pinner.Pin inside a callback on
					// the current G's active C-exposed object would
					// self-deadlock because the scope cannot end until the
					// callback returns) and never misthrow as OOM. Pin the
					// source in place below with the M held; the caller
					// commits pinner bits or the sticky flag preserving
					// M -> speciallock -> gate order with no copy under M.
					refused = true
					refusedBase = base
				}
			}
		}
		homeAddr := base + off
		mp = acquirem()
		s = spanOfHeap(homeAddr)
		if s == nil {
			releasem(mp)
			return unsafe.Pointer(homeAddr), nil, nil
		}
		if atomic.Load(&s.sweepgen) != mheap_.sweepgen+2 {
			s.ensureSwept()
		}
		KeepAlive(p)
		if refused {
			// Pin-in-place: AttachStable would report false for a
			// registered source, and retrying the copy would deadlock an
			// active same-G exposure, so neither is used here. With the M
			// held no pause can unregister the page, so a fresh lookup is
			// stable for registration; world-running workers may still
			// publish, so recheck forwarding and adopt a winning copy.
			if q2 := zgcPageFor(refusedBase); q2 != nil {
				if _, ok := q2.forward(refusedBase); ok {
					releasem(mp)
					continue
				}
			}
			// No forward: return the source with the M held; the caller's
			// gate-held commit rechecks forwarding and pins atomically.
			return unsafe.Pointer(homeAddr), s, mp
		}
		if !zgcAttachStable(s, homeAddr) {
			releasem(mp)
			continue
		}
		return unsafe.Pointer(homeAddr), s, mp
	}
}

// logosPinSignalObject strongly pins the object containing p and returns
// its stable home as a typed pointer: nil stays nil, and an address that
// names no heap object (or no moving heap) is returned as its canonical
// identity with nothing committed. Unlike the legacy weak flag, the
// object is first copied to its stable home when the zgc cycle runs, and
// the sticky span flag commits before the M is released, so the home
// cannot move under the caller. When the source is an unpublished
// object on a registered page the caller must be preemptible; see
// logosZgcPinPrepare.
func logosPinSignalObject(p unsafe.Pointer) unsafe.Pointer {
	if p == nil {
		return nil
	}
	if !logosMovingMode() {
		return p
	}
	if !zgcCycleEnabled() {
		// Immix (or an idle zgc): no registry and no copy, only the
		// sticky flag with the M held against a stop-the-world
		// evacuation racing the flag.
		mp := acquirem()
		if s := spanOfHeap(uintptr(p)); s != nil {
			s.logosPinned.Store(true)
		}
		releasem(mp)
		return p
	}
	home, s, mp := logosZgcPinPrepare(p)
	if mp == nil {
		return home
	}
	for {
		finalAddr := uintptr(home)
		var base uintptr
		var off uintptr
		if s.spanclass.sizeclass() == 0 {
			base = s.startAddr
			off = finalAddr - base
		} else {
			idx := s.objIndex(finalAddr)
			base = s.startAddr + idx*s.elemsize
			off = finalAddr - base
		}
		zgcForeignGateLock()
		if pg := zgcPageFor(base); pg != nil {
			if won, ok := pg.forward(base); ok {
				zgcForeignGateUnlock()
				releasem(mp)
				p = unsafe.Pointer(won + off)
				home, s, mp = logosZgcPinPrepare(p)
				if mp == nil {
					return home
				}
				continue
			}
		}
		s.logosPinned.Store(true)
		zgcForeignGateUnlock()
		releasem(mp)
		return home
	}
}

func logosZgcPinnerPin(ptr unsafe.Pointer) (unsafe.Pointer, bool) {
	if ptr == nil {
		return nil, false
	}
	for {
		home, sFinal, mp := logosZgcPinPrepare(ptr)
		if mp == nil {
			return nil, false
		}
		finalAddr := uintptr(home)
		lock(&sFinal.speciallock)
		pb := sFinal.getPinnerBits()
		if pb == nil {
			pb = sFinal.newPinnerBits()
			sFinal.setPinnerBits(pb)
		}
		finalIdx := sFinal.objIndex(finalAddr)
		if finalIdx >= uintptr(sFinal.nelems) {
			unlock(&sFinal.speciallock)
			releasem(mp)
			return nil, false
		}
		var base uintptr
		var off uintptr
		if sFinal.spanclass.sizeclass() == 0 {
			base = sFinal.startAddr
			off = finalAddr - base
		} else {
			base = sFinal.startAddr + finalIdx*sFinal.elemsize
			off = finalAddr - base
		}
		zgcForeignGateLock()
		if pg := zgcPageFor(base); pg != nil {
			if won, ok := pg.forward(base); ok {
				zgcForeignGateUnlock()
				unlock(&sFinal.speciallock)
				releasem(mp)
				ptr = unsafe.Pointer(won + off)
				continue
			}
		}
		recheck := pb.ofObject(finalIdx)
		isMulti := recheck.isPinned()
		if isMulti {
			recheck.setMultiPinned(true)
		} else {
			recheck.setPinned(true)
		}
		zgcForeignGateUnlock()
		if isMulti {
			coff := finalIdx * sFinal.elemsize
			systemstack(func() {
				sFinal.incPinCounter(coff)
			})
		}
		unlock(&sFinal.speciallock)
		releasem(mp)
		return unsafe.Pointer(finalAddr), true
	}
}

func logosZgcPinnerSetPinned(ptr unsafe.Pointer) bool {
	_, ok := logosZgcPinnerPin(ptr)
	return ok
}
