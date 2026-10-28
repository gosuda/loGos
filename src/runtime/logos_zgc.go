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
func zgcLoadBarrierSlow(slot *uintptr, v uintptr) uintptr {
	addr := v &^ atomic.Loaduintptr(&zgcColorMaskBits)
	if addr > zgcAddrMask {
		return v
	}
	pg := zgcPageFor(addr)
	if pg == nil {
		return v
	}
	// Concurrent relocation: when the canonical address lies on a
	// registered page with no published entry yet, the loading thread
	// relocates the object itself before healing the slot.
	zgcDeepRelocate(pg, addr)
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
		addr := zgcRemap(zgcAddr(old))
		// Load-barrier marking: a deep-path load that resolves a word
		// also greys its target while marking runs.
		zgcBarrierMark(addr)
		if atomic.Casuintptr(slot, old, addr+zgcGoodBits()) {
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

// zgcUntag clears every color and tag bit of p. Syscall and cgo entries
// pass pointers through it because the kernel rejects colored addresses.
func zgcUntag(p unsafe.Pointer) unsafe.Pointer {
	return add(nil, zgcAddr(uintptr(p)))
}
