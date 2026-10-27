// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Colored-pointer layout (SPEC 19.2). A pointer word holds a 42-bit
// canonical address in bits 41..0, which spans 4 TB. Bits 63..48 are
// the region the MMU ignores under ARM64 TBI and x86 LAM or UAI.
//
// Software encoding: the four color bits sit inside the address word,
// so a load must test them and unmask with one AND.
//
//	bit 47 reserved zero, 46 final, 45 remapped, 44 mark1,
//	bit 43 reserved zero, 42 mark0
//
// Hardware encoding: the same four colors sit in the ignored byte, in
// bits 59..56 in the order mark0, mark1, remapped, final. Bits 63..60
// and 55..48 stay zero.
const (
	zgcAddrBits      = 42
	zgcAddrMask      = uintptr(1)<<zgcAddrBits - 1
	zgcTagRegionMask = uintptr(0xFFFF) << 48
	zgcColorShift    = 56
	zgcColorMask     = uintptr(0xF) << zgcColorShift
	zgcMark0Bit      = 42
	zgcMark1Bit      = 44
	zgcRemappedBit   = 45
	zgcFinalBit      = 46
)

// zgcColor is the four-bit color state: one bit per color, mark0 lowest.
type zgcColor uint8

const (
	zgcColorMark0 zgcColor = 1 << iota
	zgcColorMark1
	zgcColorRemapped
	zgcColorFinal

	zgcColorNibble = zgcColorMark0 | zgcColorMark1 | zgcColorRemapped | zgcColorFinal
)

// zgcEncoding selects where the color bits of a pointer word live.
type zgcEncoding uint8

const (
	zgcEncodingSoftware zgcEncoding = iota
	zgcEncodingHardware
)

var zgcSoftShift = [4]uint{zgcMark0Bit, zgcMark1Bit, zgcRemappedBit, zgcFinalBit}

var (
	zgcHardwareTagging bool
	zgcGoodMask        uintptr
	zgcColorMaskBits   uintptr
	zgcHeals           atomic.Uint64

	// zgcUnmaskBits is the complement of zgcColorMaskBits: code compiled
	// with -lb unmasks a good word with one AND of it. Before zgc is
	// configured it is all ones, so that AND keeps every word unchanged.
	zgcUnmaskBits = ^uintptr(0)
)

// zgcSoftBits returns color c in the software encoding.
func zgcSoftBits(c zgcColor) (bits uintptr) {
	for i, shift := range zgcSoftShift {
		bits |= uintptr(c>>i&1) << shift
	}
	return bits
}

// zgcTagBits returns color c in the hardware encoding.
func zgcTagBits(c zgcColor) uintptr {
	return uintptr(c&zgcColorNibble) << zgcColorShift
}

// zgcColorBits returns color c in the active encoding.
func zgcColorBits(c zgcColor) uintptr {
	if zgcHardwareTagging {
		return zgcTagBits(c)
	}
	return zgcSoftBits(c)
}

// zgcAddr unmasks v to its canonical address. One AND strips every color
// and tag bit in both encodings.
func zgcAddr(v uintptr) uintptr {
	return v & zgcAddrMask
}

// zgcGoodBits returns the encoded good color.
func zgcGoodBits() uintptr {
	return atomic.Loaduintptr(&zgcGoodMask)
}

// zgcConfigure selects the encoding and makes good the good color. It runs
// before any barrier executes.
func zgcConfigure(enc zgcEncoding, good zgcColor) {
	zgcHardwareTagging = enc == zgcEncodingHardware
	zgcFlip(good)
}

// zgcFlip makes good the good color. A word is good only when its color
// bits equal the good color exactly, so a stale color, an extra color bit,
// and no color at all are all bad. Every word of the previous good color
// turns bad at once. Flips run with the world stopped.
func zgcFlip(good zgcColor) {
	switch good {
	case zgcColorMark0, zgcColorMark1, zgcColorRemapped:
	default:
		throw("zgc: good color must be mark0, mark1 or remapped")
	}
	atomic.Storeuintptr(&zgcColorMaskBits, zgcColorBits(zgcColorNibble))
	atomic.Storeuintptr(&zgcUnmaskBits, ^zgcColorBits(zgcColorNibble))
	atomic.Storeuintptr(&zgcGoodMask, zgcColorBits(good))
}

// zgcLoadBarrierSlow is the out-of-line half of the load barrier that the
// compiler emits under -lb (cmd/compile/internal/ssa/logos_loadbarrier.go),
// and of zgcLoadBarrier. It runs when the word v loaded from slot does not
// carry exactly the good color, and returns the canonical address.
//
// Only a word whose canonical address lies on a page of the zgc page
// directory is a zgc heap word and heals. Any other word (nil, a pointer
// to C or mmap memory, to read-only data, or to a goroutine stack) is
// returned unchanged and its slot is left alone, because its high bits
// are address bits, not colors.
// zgcOnSignalStack reports whether the current SP is on the M's signal
// stack. sigtrampgo runs on the signal stack while getg() still names the
// interrupted goroutine (setg(gp) until setg(gsignal) in signal_unix.go),
// so the gsignal-identity check in zgcCanRelocateDeep misses that window.
// An SP range check catches both the window and ordinary signal handling
// with no lock, no allocation, and no slot write.
//
//go:nosplit
func zgcOnSignalStack() bool {
	gp := getg()
	mp := gp.m
	if mp == nil {
		return false
	}
	sg := mp.gsignal
	if sg == nil {
		return false
	}
	var tmp uintptr
	sp := uintptr(unsafe.Pointer(&tmp))
	return sp >= sg.stack.lo && sp < sg.stack.hi
}

// zgcNoSplitWindow reports whether the caller runs where the stack must
// not split: throwsplit set (notably the syscall entry path), execution
// on g0 or the signal stack, locks held, or no P. Every read below is a
// plain load or an inlined intrinsic, so the check itself never grows the
// stack. It is the compile-time-agnostic form of the ssa NoSplit contract:
// any present or future caller in a no-split window takes the remap-only
// path, never the healing body.
//
//go:nosplit
func zgcNoSplitWindow() bool {
	gp := getg()
	mp := gp.m
	if mp == nil {
		return true
	}
	if gp.throwsplit {
		return true
	}
	if gp == mp.g0 || gp == mp.gsignal {
		return true
	}
	if mp.locks != 0 {
		return true
	}
	if uintptr(mp.p) == 0 {
		return true
	}
	return zgcOnSignalStack()
}

// zgcLoadBarrierSlow is the out-of-line half of the load barrier. The
// entry is genuinely nosplit: it runs no splitting call before deciding
// the path (only the nosplit window check and, on that path, the nosplit
// remap chain), so entering it can never need stack growth on the caller
// frame — the prologue that killed syscall entry (newstack at
// zgcLoadBarrierSlow+0xe5 via runtime.save in reentersyscall, "stack
// split at bad time") cannot fire here. A no-split window takes the
// remap-only path through the
// already-nosplit remap machinery (SPEC 8.4 "or remap" disjunct, matching
// the signal-stack precedent): no gate lock, no shade, no slot write, no
// allocation. Healing needs a P and gcw and stays on the preemptible path
// below, which is unchanged. Sources stay valid under quarantine and the
// relocation workers publish the copies, so a deferred heal converges on
// the next preemptible load; release stays gated on full publication, so
// an unremapped source can never dangle.
//
//go:nosplit
func zgcLoadBarrierSlow(slot *uintptr, v uintptr) uintptr {
	addr := v &^ atomic.Loaduintptr(&zgcColorMaskBits)
	if addr > zgcAddrMask {
		return v
	}
	if slot == (*uintptr)(unsafe.Pointer(&getg().m)) {
		// The M slot is scheduler state that asm and non-barrier
		// readers dereference unmasked. It never relocates, including
		// when a synthetic test page claims its whole granule, so the
		// barrier returns the canonical address without a table walk
		// or a heal in every window.
		return addr
	}
	if zgcNoSplitWindow() {
		return zgcRemapNoSplit(addr)
	}
	pg := zgcPageFor(addr)
	if pg == nil {
		return v
	}
	if zgcOnSignalStack() || !zgcCanRelocateDeep() {
		// Signal-stack (by SP, covering the sigtrampgo window where
		// getg() still names the interrupted goroutine), nosplit, g0,
		// P-less, or lock-holding loads (notably cpuprof.add under -lb)
		// must not heal. SPEC 8.4: "Under zgc, pointer loads MUST test
		// the color and heal or remap on mismatch." This branch takes
		// the "or remap" disjunct with no gate lock, no shade, and no
		// slot write (zgcRemap is atomic loads only) and returns the
		// forwarded canonical address. SPEC 19.2: "Without hardware
		// tagging, loads MUST test the color." "On mismatch they MUST
		// take the self-heal path." "They MUST unmask to the canonical
		// address." That self-heal path (zgcSelfHeal below) runs only
		// in preemptible contexts: the slot converges on the next
		// preemptible load, sources stay valid under quarantine, and
		// copies are published by the relocation workers
		// (see logos_zgc_workers.go).
		return zgcRemap(addr)
	}
	return zgcSelfHeal(slot, v)
}

// zgcLoadBarrier loads the pointer word at slot. Plain stores and allocator
// results write words with no color, so the test is an exact color match,
// (v ^ good) & colorbits, not a test for bad bits: a word with no color
// bits must reach the slow path. Nil has no color and takes the slow path
// too, which returns it unchanged. The result is the canonical address.
//
// The fast path unmasks with zgcUnmaskBits exactly as the compiled barrier
// does. Before zgcConfigure the color mask is zero, every word matches,
// and zgcUnmaskBits is all ones, so the word comes back unchanged: a
// pointer to C, mmap, or stack memory above the 4 TB field keeps its
// high address bits.
//
//go:noinline
func zgcLoadBarrier(slot *uintptr) uintptr {
	v := atomic.Loaduintptr(slot)
	if (v^atomic.Loaduintptr(&zgcGoodMask))&atomic.Loaduintptr(&zgcColorMaskBits) == 0 {
		return v & atomic.Loaduintptr(&zgcUnmaskBits)
	}
	return zgcLoadBarrierSlow(slot, v)
}

// zgcSelfHeal replaces the bad word old in slot with the good-colored
// address of its relocated object and returns that address. Nil is
// returned as is and never counts as a heal. Concurrent healers and
// mutator stores are resolved by compare-and-swap: a lost race re-reads
// the slot and either accepts the winner's good word, accepts a stored
// nil, or heals the new bad word.
func zgcSelfHeal(slot *uintptr, old uintptr) uintptr {
	if old == 0 {
		return 0
	}
	zgcHeals.Add(1)
	for {
		// Grey outside the gate: needs a P/gcw and may safepoint, so it
		// never runs while the gate is held. Advisory only; the commit
		// below re-resolves authoritatively under the gate.
		zgcBarrierMark(zgcRemap(zgcAddr(old)))
		// Hold m.locks across the gate section with plain loads only
		// (this file is -lb-excluded, so no barrier fires here): a
		// barrier re-entered inside the gate (notably lock2's gp.m,
		// which fires pre-increment) then sees locks != 0 in the
		// no-split window check and remaps instead of re-entering this
		// heal and the gate recursively (proven cycle: lock2 ->
		// SelfHeal -> ForeignGateLock -> lock2). Unmask: the slot can
		// hold a colored word from an earlier preemptible heal (Ms are
		// heap objects), and a plain copy would fault on dereference;
		// this mirrors exactly what the barrier fast path would hand
		// out, with no slot write and no gate.
		gp := getg()
		mpw := uintptr(unsafe.Pointer(gp.m)) &^ atomic.Loaduintptr(&zgcColorMaskBits)
		if mpw > zgcAddrMask {
			mpw = uintptr(unsafe.Pointer(gp.m))
		}
		mp := (*m)(unsafe.Pointer(mpw))
		mp.locks++
		zgcForeignGateLock()
		cur := atomic.Loaduintptr(slot)
		if cur != old {
			zgcForeignGateUnlock()
			releasem(mp)
			old = cur
			if old == 0 || old&atomic.Loaduintptr(&zgcColorMaskBits) == atomic.Loaduintptr(&zgcGoodMask) {
				return zgcAddr(old)
			}
			continue
		}
		addr := zgcRemap(zgcAddr(old))
		if zgcForeignObjectExposedLocked(uintptr(unsafe.Pointer(slot))) {
			zgcForeignGateUnlock()
			releasem(mp)
			return addr
		}
		won := atomic.Casuintptr(slot, old, addr+zgcGoodBits())
		zgcForeignGateUnlock()
		releasem(mp)
		if won {
			return addr
		}
		old = atomic.Loaduintptr(slot)
		if old == 0 || old&atomic.Loaduintptr(&zgcColorMaskBits) == atomic.Loaduintptr(&zgcGoodMask) {
			return zgcAddr(old)
		}
	}
}

// zgcRemap returns the relocated address of canonical addr, or addr itself
// when its page is not evacuating or its object has not moved yet. The
// forwarding table is keyed by object start, so an interior pointer finds
// its object through the extents of the heap span holding it and keeps
// its offset into the copy. Span metadata lives off the page, so this
// works after the source page is released. Memory without a span (a page
// the tests map directly) has no extents, and only object starts remap.
func zgcRemap(addr uintptr) uintptr {
	pg := zgcPageFor(addr)
	if pg == nil {
		return addr
	}
	base := addr
	if s := spanOfHeap(addr); s != nil {
		base = s.base() + s.objIndex(addr)*s.elemsize
	}
	if to, ok := pg.forward(base); ok {
		return to + (addr - base)
	}
	return addr
}

// zgcRemapNoSplit is the no-split-window twin of zgcRemap for the
// load-barrier entry: it resolves addr to the canonical address without
// growing the stack and without calling any splitting function. Every
// step is arithmetic, a bounds check, or a nosplit-marked atomic load;
// the span-extents and forwarding-table walks it duplicates are inlined
// here (rather than shared) so no future edit to the shared paths can
// silently reintroduce a splitting call into this window. Any miss falls
// back to the unmasked source address: sources stay valid under
// quarantine while release waits on full publication, so a miss never
// dangles — it only defers release — and the next preemptible load of
// the slot heals and converges. Keep the granule, table, and magic
// constants below in sync with logos_zgc_page.go and mheap.go; on any
// doubt return the source, never a guess.

//go:nosplit
func zgcRemapNoSplit(addr uintptr) uintptr {
	p := atomic.Loaduintptr(&zgcDirAddr)
	if p == 0 || addr > zgcAddrMask {
		return addr
	}
	v := (*zgcDirectory)(add(nil, p))[addr>>zgcPageShift].Load()
	if v == 0 {
		return addr
	}
	pg := (*zgcPage)(add(nil, v))
	if !pg.holdsSource(addr) {
		return addr
	}
	// Object start through span extents, mirroring zgcRemap.
	ri := arenaIdx((addr - arenaBaseOffset) / heapArenaBytes)
	var l1, l2 uint
	if arenaL1Bits == 0 {
		l2 = uint(ri)
	} else {
		l1 = uint(ri) >> arenaL1Shift
		if l1 >= uint(len(mheap_.arenas)) {
			return addr
		}
		l2 = uint(ri) & (1<<arenaL2Bits - 1)
	}
	var ha *heapArena
	if arenaL1Bits == 0 {
		if l2 >= uint(len(mheap_.arenas[0])) {
			return addr
		}
		ha = mheap_.arenas[0][l2]
	} else {
		l2t := mheap_.arenas[l1]
		if l2t == nil {
			return addr
		}
		if l2 >= uint(len(l2t)) {
			return addr
		}
		ha = l2t[l2]
	}
	if ha == nil {
		return addr
	}
	s := ha.spans[(addr/pageSize)%pagesPerArena]
	base := addr
	if s != nil && s.state.s.Load() == uint8(mSpanInUse) && addr >= s.startAddr && addr < s.limit {
		n := addr - s.startAddr
		base = s.startAddr + uintptr((uint64(n)*uint64(s.divMul))>>32)*s.elemsize
	}
	// Forwarding-table walk mirroring (*zgcPage).forward.
	if base < pg.start || base-pg.start >= pg.size {
		return addr
	}
	idx := uint64((base - pg.start) >> zgcGranuleShift)
	i := uintptr(idx * zgcFwdHashMul >> pg.fwdShift)
	for range pg.fwdMask + 1 {
		e := (*atomic.Uint64)(add(pg.fwd, i*8)).Load()
		if e == 0 {
			return addr
		}
		if e>>zgcFwdIndexShift&zgcFwdIndexMask == idx {
			return uintptr(e&zgcFwdToMask) + (addr - base)
		}
		i = (i + 1) & pg.fwdMask
	}
	return addr
}

// zgcUntag clears every color and tag bit of p. Syscall and cgo entries
// pass pointers through it because the kernel rejects colored addresses.
func zgcUntag(p unsafe.Pointer) unsafe.Pointer {
	return add(nil, zgcAddr(uintptr(p)))
}
