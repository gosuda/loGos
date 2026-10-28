// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

const (
	zgcPageShift = 21
	zgcPageSize  = 1 << zgcPageShift

	// zgcMaxPageBytes bounds the page a forwarding entry can index: the
	// 21-bit source index counts 8-byte granules.
	zgcMaxPageBytes = 1 << (21 + zgcGranuleShift)
	zgcGranuleShift = 3

	// zgcEvacPercent is the fragmentation a page must exceed to be evacuated.
	zgcEvacPercent = 35

	zgcFwdPopulated  = uint64(1) << 63
	zgcFwdIndexShift = zgcAddrBits
	zgcFwdIndexMask  = uint64(1)<<21 - 1
	zgcFwdToMask     = uint64(zgcAddrMask)
	zgcFwdMinCap     = 8
	zgcFwdHashMul    = 0x9E3779B97F4A7C15
)

// zgcPage is a page metadata header. It and its forwarding table share one
// off-heap mapping that is separate from the page, so the page can return
// to the OS while stale pointers still resolve through the table.
//
// A forwarding entry is one word: bit 63 populated, bits 62..42 the source
// offset in 8-byte granules, bits 41..0 the relocated address. Publishing
// an entry is one compare-and-swap, so racing relocators agree on the
// winner.
type zgcPage struct {
	_ sys.NotInHeap

	start, size, live uintptr
	fwd               unsafe.Pointer
	fwdMask           uintptr
	fwdShift          uint8
	mapBytes          uintptr
	released          atomic.Uint32

	// published counts the live bytes whose forwarding entries this
	// page's relocators won. The source page may return to the OS only
	// once it equals live: every live object then has a copy reachable
	// through the table, which outlives the page.
	published atomic.Uintptr
	// releaseEpoch is zgcRemapEpoch when the page was released. The
	// metadata, and with it the address range, may be reused only after
	// a later remap pass healed every slot that could still hold a
	// source address (zgcRetirePage).
	releaseEpoch uint32
}

// zgcRemapEpoch counts completed remap passes: collector passes that
// loaded every reachable pointer slot through the barrier, so no slot
// still holds an address on a page released before the pass began.
var zgcRemapEpoch atomic.Uint32

// zgcRemapDone records that a remap pass started after every page
// released so far has finished.
func zgcRemapDone() {
	zgcRemapEpoch.Add(1)
}

type zgcDirectory [1 << (zgcAddrBits - zgcPageShift)]atomic.Uintptr

var (
	zgcDirLock mutex
	zgcDirAddr uintptr
)

// zgcFragmentationExceeds reports whether garbage bytes of a size-byte page
// are more than zgcEvacPercent percent of it.
func zgcFragmentationExceeds(garbage, size uintptr) bool {
	return garbage*100 > size*zgcEvacPercent
}

// evacuable reports whether the page is over the evacuation threshold.
func (pg *zgcPage) evacuable() bool {
	if pg.live > pg.size {
		return false
	}
	return zgcFragmentationExceeds(pg.size-pg.live, pg.size)
}

func zgcTableCap(nObjects uintptr) uintptr {
	c := uintptr(zgcFwdMinCap)
	for c < 2*nObjects {
		c <<= 1
	}
	return c
}

// zgcNewDetachedPage returns an unregistered header of any size with a
// minimal forwarding table.
func zgcNewDetachedPage(size, live uintptr) *zgcPage {
	return zgcAllocPage(0, size, live, 0)
}

func zgcAllocPage(start, size, live, nObjects uintptr) *zgcPage {
	if size > zgcMaxPageBytes {
		throw("zgc: page larger than the forwarding index range")
	}
	slots := zgcTableCap(nObjects)
	hdr := alignUp(unsafe.Sizeof(zgcPage{}), 8)
	n := alignUp(hdr+slots*8, physPageSize)
	raw := sysAlloc(n, &memstats.other_sys, "logos zgc page metadata")
	if raw == nil {
		throw("zgc: out of memory allocating page metadata")
	}
	pg := (*zgcPage)(raw)
	pg.start, pg.size, pg.live = start, size, live
	pg.fwd = add(raw, hdr)
	pg.fwdMask = slots - 1
	pg.fwdShift = 64 - uint8(sys.TrailingZeros64(uint64(slots)))
	pg.mapBytes = n
	return pg
}

// zgcNewPage returns a registered header for the one-granule page at start,
// with a forwarding table sized for nObjects relocated objects.
func zgcNewPage(start, size, live, nObjects uintptr) *zgcPage {
	if start&(zgcPageSize-1) != 0 || size != zgcPageSize || start > zgcAddrMask-(size-1) {
		throw("zgc: page must be one aligned granule inside the 4 TB range")
	}
	pg := zgcAllocPage(start, size, live, nObjects)
	if !zgcDirectoryEnsure()[start>>zgcPageShift].CompareAndSwap(0, uintptr(unsafe.Pointer(pg))) {
		// A stale word into the old page must keep resolving through
		// its table until zgcRetirePage; a new page here would answer
		// for it instead.
		throw("zgc: page registered over a page that is not retired")
	}
	lock(&zgcPagesMu)
	zgcAllPages = append(zgcAllPages, pg)
	unlock(&zgcPagesMu)
	return pg
}

// zgcFreePage unregisters pg and returns its metadata mapping.
// A page with a published relocation may go only through zgcRetirePage:
// until a remap pass after its release, stale words still resolve
// through its table, and before release the source and the copy would
// both be live.
func zgcFreePage(pg *zgcPage) {
	if pg.published.Load() != 0 && (pg.released.Load() == 0 || zgcRemapEpoch.Load() == pg.releaseEpoch) {
		throw("zgc: freeing a relocated page before a remap pass retired it")
	}
	if p := atomic.Loaduintptr(&zgcDirAddr); p != 0 {
		(*zgcDirectory)(add(nil, p))[pg.start>>zgcPageShift].CompareAndSwap(uintptr(unsafe.Pointer(pg)), 0)
	}
	lock(&zgcPagesMu)
	for i, q := range zgcAllPages {
		if q == pg {
			zgcAllPages[i] = zgcAllPages[len(zgcAllPages)-1]
			zgcAllPages = zgcAllPages[:len(zgcAllPages)-1]
			break
		}
	}
	unlock(&zgcPagesMu)
	sysFree(unsafe.Pointer(pg), pg.mapBytes, &memstats.other_sys)
}

func zgcDirectoryEnsure() *zgcDirectory {
	if p := atomic.Loaduintptr(&zgcDirAddr); p != 0 {
		return (*zgcDirectory)(add(nil, p))
	}
	lock(&zgcDirLock)
	p := zgcDirAddr
	if p == 0 {
		raw := sysAlloc(unsafe.Sizeof(zgcDirectory{}), &memstats.other_sys, "logos zgc page directory")
		if raw == nil {
			throw("zgc: out of memory allocating the page directory")
		}
		p = uintptr(raw)
		atomic.Storeuintptr(&zgcDirAddr, p)
	}
	unlock(&zgcDirLock)
	return (*zgcDirectory)(add(nil, p))
}

// zgcPageFor returns the registered page containing addr, or nil.
func zgcPageFor(addr uintptr) *zgcPage {
	p := atomic.Loaduintptr(&zgcDirAddr)
	if p == 0 || addr > zgcAddrMask {
		return nil
	}
	return (*zgcPage)(add(nil, (*zgcDirectory)(add(nil, p))[addr>>zgcPageShift].Load()))
}

func (pg *zgcPage) fwdSlot(i uintptr) *atomic.Uint64 {
	return (*atomic.Uint64)(add(pg.fwd, i*8))
}

func (pg *zgcPage) fwdHome(idx uint64) uintptr {
	return uintptr(idx * zgcFwdHashMul >> pg.fwdShift)
}

func zgcFwdIndexOf(e uint64) uint64 {
	return e >> zgcFwdIndexShift & zgcFwdIndexMask
}

// forwardInsert records that the object at from moved to to and returns the
// address every caller must use: to when this call published the entry, the
// earlier writer's address otherwise. from must be an object start.
func (pg *zgcPage) forwardInsert(from, to uintptr) uintptr {
	won, _ := pg.forwardPublish(from, to)
	return won
}

// forwardPublish is forwardInsert that also reports whether this call's
// compare-and-swap installed the entry. Only that call may account the
// object as published: an earlier writer may have chosen the same target.
func (pg *zgcPage) forwardPublish(from, to uintptr) (uintptr, bool) {
	if from < pg.start || from-pg.start >= pg.size {
		throw("zgc: forwarding source outside its page")
	}
	if to > zgcAddrMask {
		throw("zgc: relocation target above the 4 TB range")
	}
	idx := uint64((from - pg.start) >> zgcGranuleShift)
	want := zgcFwdPopulated | idx<<zgcFwdIndexShift | uint64(to)
	i := pg.fwdHome(idx)
	for range pg.fwdMask + 1 {
		slot := pg.fwdSlot(i)
		e := slot.Load()
		if e == 0 && slot.CompareAndSwap(0, want) {
			return to, true
		}
		if e == 0 {
			e = slot.Load()
		}
		if zgcFwdIndexOf(e) == idx {
			return uintptr(e & zgcFwdToMask), false
		}
		i = (i + 1) & pg.fwdMask
	}
	throw("zgc: forwarding table full")
	return 0, false
}

// forward returns the relocated address of the object that started at from.
func (pg *zgcPage) forward(from uintptr) (uintptr, bool) {
	if from < pg.start || from-pg.start >= pg.size {
		return 0, false
	}
	idx := uint64((from - pg.start) >> zgcGranuleShift)
	i := pg.fwdHome(idx)
	for range pg.fwdMask + 1 {
		e := pg.fwdSlot(i).Load()
		if e == 0 {
			return 0, false
		}
		if zgcFwdIndexOf(e) == idx {
			return uintptr(e & zgcFwdToMask), true
		}
		i = (i + 1) & pg.fwdMask
	}
	return 0, false
}

// zgcRelocateObject copies the size-byte object at from to to and publishes
// the forwarding entry. It returns the winning address; a caller that lost
// the race discards its copy.
// The copy completes before publication, and publication happens under
// the identity lock together with the identity transfer, so a hasher
// sees either the source with its identity or the copy with it.
func zgcRelocateObject(pg *zgcPage, from, size, to uintptr) uintptr {
	memmove(add(nil, to), add(nil, from), size)
	t := &zgcIdentity
	lock(&t.lock)
	won, mine := pg.forwardPublish(from, to)
	if mine {
		t.moveLocked(from, to)
		pg.published.Add(size)
	}
	unlock(&t.lock)
	return won
}

// zgcReleasePage returns the source page to the OS and reports whether it
// did. It refuses while any live byte of the page has no published copy:
// those bytes are the only copy of their object. After release, a stale
// word still resolves through the forwarding table, which lives outside
// the page.
func zgcReleasePage(pg *zgcPage) bool {
	if pg.released.Load() != 0 {
		return true
	}
	if pg.published.Load() != pg.live {
		return false
	}
	pg.releaseEpoch = zgcRemapEpoch.Load()
	zgcReleasePages(add(nil, pg.start), pg.size)
	pg.released.Store(1)
	return true
}
