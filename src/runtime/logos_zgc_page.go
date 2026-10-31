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

	// zgcMaxPages bounds every page list, and it is the total header
	// budget: one entry per 2 MB directory granule, the most
	// registrations the directory CAS can admit. Every distinct live
	// header holds exactly one granule slot in exactly one of two forms —
	// its own pointer while registered, the retiring tombstone while it
	// waits in the retire queue unpublished — and the CAS refuses any
	// claim while either stands, so distinct registered and retiring
	// headers together can never exceed this bound. Lists capped here
	// never grow, so their appends under pause locks never allocate.
	zgcMaxPages = 1 << (zgcAddrBits - zgcPageShift)

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

	// zgcMaxSourceRanges bounds the exact source ranges of one page: in-use
	// spans are disjoint and each takes at least one runtime page of the
	// 2 MB region, so the region holds at most this many of them.
	zgcMaxSourceRanges = zgcPageSize / pageSize
)

// zgcSourceRange is one maximal run of adjacent quarantined source spans:
// the bytes [lo, hi) belong to the page and to nothing else.
type zgcSourceRange struct {
	lo, hi uintptr
}

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

	// stateIdx is the one-based index of this page's span state in
	// zgcPageStates, or zero when it has none. It is an index and not a
	// pointer because this metadata is NotInHeap, so the collector
	// cannot trace it; the slice keeps the state alive.
	stateIdx uintptr

	start, size, live uintptr
	fwd               unsafe.Pointer
	fwdMask           uintptr
	fwdShift          uint8
	mapBytes          uintptr
	released          atomic.Uint32

	// abandoned marks a page intentionally discarded before release:
	// its to-space reservation failed or the selection vet refused it.
	// zgcAbandonPage sets it once, before the page is unpublished or
	// queued; reclamation reads it at a stop-the-world gate and never
	// frees an unreleased page that lacks it.
	abandoned bool
	// retiring records that this header installed the directory tombstone.
	// It is written and read under zgcPagesMu. Reclaim clears only that
	// marker, so freeing an unregistered detached test header cannot clear
	// another descriptor's marker at the same address.
	retiring bool

	// refs counts live leases on this page header. A relocation worker
	// gains one under zgcPagesMu before the header escapes
	// zgcNextUnfinishedPage; a preemptible pinner, attach or self-heal
	// caller gains one through zgcPageAcquire before any suspension and
	// drops it on every exit. Reclamation samples refs==0 (after the
	// acquisition domain drained) before any destructive step: span
	// metadata free, state pool return, header sysFree.
	refs atomic.Uintptr

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

	// Source ownership. A selected heap page owns only its quarantined
	// source spans, never the 2 MB region around them: the region may
	// also hold free pages, goroutine stacks and other manual spans, and
	// objects the mutator allocates after selection. srcN sorted,
	// disjoint, non-adjacent ranges live at src, in this page's own
	// metadata mapping. prepareSources writes them once, in the
	// selection pause before any to-space is reserved, and they never
	// change until zgcFreePage, so every reader (barrier, pin,
	// relocation commit under the foreign gate, release, identity
	// forget) reads them without a lock and without allocating. A page
	// starts with its whole region as the one range (a synthetic
	// registration names the memory it was registered over); srcExact
	// turns true when prepareSources replaces that with the exact
	// spans, and release refuses a not-exact page whose region holds a
	// heap arena (zgcReleasePage).
	src      unsafe.Pointer
	srcN     uintptr
	srcExact bool
}

// zgcRemapEpoch counts completed remap passes: collector passes that
// loaded every reachable pointer slot through the barrier, so no slot
// still holds an address on a page released before the pass began.
var zgcRemapEpoch atomic.Uint32

// zgcPageAcqDomain counts open transient read domains. A reader enters
// before its first raw pointer-deriving load (directory slot, header or
// forwarding probe) and leaves after the last; reclamation samples the
// counter zero after unpublishing and frees nothing while a domain is
// open, so every pointer derived inside a domain stays valid until the
// matching leave. Default sequentially consistent Add/Load only
// (internal/runtime/atomic doc 13). Signal-stack and no-P readers hold
// domains only: no locks, no allocation, no per-page refs there.
var zgcPageAcqDomain atomic.Uintptr

// zgcRemapDone records that a remap pass started after every page
// released so far has finished.
func zgcRemapDone() {
	zgcRemapEpoch.Add(1)
}

type zgcDirectory [1 << (zgcAddrBits - zgcPageShift)]atomic.Uintptr

// zgcSlotRetiring marks a directory granule whose descriptor left the
// registered list but is not reclaimed yet: fresh lookups miss it, and
// no claim may take the granule until reclamation frees the descriptor's
// header and clears the marker. The value must never collide with a
// header pointer: headers come from sysAlloc, so every one is
// page-aligned.
const zgcSlotRetiring = 1

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
	srcOff := hdr + slots*8
	n := alignUp(srcOff+zgcMaxSourceRanges*unsafe.Sizeof(zgcSourceRange{}), physPageSize)
	raw := sysAlloc(n, &memstats.other_sys, "logos zgc page metadata")
	if raw == nil {
		throw("zgc: out of memory allocating page metadata")
	}
	pg := (*zgcPage)(raw)
	pg.start, pg.size, pg.live = start, size, live
	pg.fwd = add(raw, hdr)
	pg.fwdMask = slots - 1
	pg.fwdShift = 64 - uint8(sys.TrailingZeros64(uint64(slots)))
	pg.src = add(raw, srcOff)
	// Until prepareSources replaces it, a page resolves its whole
	// region as one range: a synthetic registration (tests) names the
	// memory it was registered over, and a heap page is never visible to
	// a mutator in this state (the selection pause prepares it first).
	*pg.sourceRangeAt(0) = zgcSourceRange{start, start + size}
	pg.srcN = 1
	pg.mapBytes = n
	return pg
}

// zgcClaimPage checks the directory slot for start and, if it is free,
// registers a fresh header for the one-granule page at start, with a
// forwarding table sized for nObjects relocated objects. It reports
// whether it claimed the slot.
//
// The occupancy check, the CAS and the registry append are one step under
// zgcPagesMu, so a free slot cannot change hands before the claim lands;
// every transition of the slot (the claim, the retiring tombstone and
// the clear in zgcFreePage) holds the same lock. Selection's slot probes
// remain lock-free fast paths, but the claim is authoritative: a false
// return defers the region to a later cycle, exactly like those skips.
// The caller must skip on false, never retry: a registered page or a
// retiring descriptor owns its granule until reclamation frees it. The
// metadata allocated for a lost claim is returned before reporting false,
// so a skip leaks nothing.
//
// Discipline: the locked section never allocates (the list is capped at
// zgcMaxPages, which registration cannot exceed), parks nothing and takes
// no nested lock: no channel ops, no syscalls. The holder cannot be
// async-preempted mid-section (holding a runtime mutex sets m.locks,
// which the preemption predicate requires to be zero). The directory is
// ensured before the section, so zgcDirLock never nests inside
// zgcPagesMu; sysAlloc and sysFree run outside the section, as before.
func zgcClaimPage(start, size, live, nObjects uintptr) (*zgcPage, bool) {
	if start&(zgcPageSize-1) != 0 || size != zgcPageSize || start > zgcAddrMask-(size-1) {
		throw("zgc: page must be one aligned granule inside the 4 TB range")
	}
	pg := zgcAllocPage(start, size, live, nObjects)
	dir := zgcDirectoryEnsure()
	lock(&zgcPagesMu)
	if !dir[start>>zgcPageShift].CompareAndSwap(0, uintptr(unsafe.Pointer(pg))) {
		unlock(&zgcPagesMu)
		sysFree(unsafe.Pointer(pg), pg.mapBytes, &memstats.other_sys)
		return nil, false
	}
	if len(zgcAllPages) == cap(zgcAllPages) {
		throw("zgc: page registry overflow")
	}
	zgcAllPages = append(zgcAllPages, pg)
	unlock(&zgcPagesMu)
	return pg, true
}

// zgcNewPage returns a registered header for the one-granule page at start,
// with a forwarding table sized for nObjects relocated objects. It is
// zgcClaimPage that must succeed: a stale word into the old page must
// keep resolving through its table until zgcRetirePage, so a new page
// registered over one that is not retired would answer for it instead.
func zgcNewPage(start, size, live, nObjects uintptr) *zgcPage {
	pg, ok := zgcClaimPage(start, size, live, nObjects)
	if !ok {
		throw("zgc: page registered over a page that is not retired")
	}
	return pg
}

// zgcPageUnpublish replaces pg's directory entry with the retiring
// tombstone and removes pg from the registered list. Idempotent: a retire
// attempt whose drain sample failed leaves the tombstoned descriptor
// queued, and its re-entry finds the tombstone and no list entry. The
// tombstone stops new transient readers from resolving pg while retaining
// ownership of the granule until reclamation clears it; readers already
// inside a domain and lease holders are drained by the caller's samples
// afterwards, and nothing else is reclaimed here.
func zgcPageUnpublish(pg *zgcPage) {
	// Discipline: one non-preemptible, allocation-free section for the
	// tombstone and the removal (slot CAS plus swap-remove only). The
	// tombstone holds the same lock as the claim in zgcClaimPage, so a
	// slot observed retiring cannot be claimed from under it, and a slot
	// cleared by reclamation cannot be claimed before its list entry leaves
	// with it.
	lock(&zgcPagesMu)
	if p := atomic.Loaduintptr(&zgcDirAddr); p != 0 {
		if (*zgcDirectory)(add(nil, p))[pg.start>>zgcPageShift].CompareAndSwap(uintptr(unsafe.Pointer(pg)), zgcSlotRetiring) {
			pg.retiring = true
		}
	}
	for i, q := range zgcAllPages {
		if q == pg {
			zgcAllPages[i] = zgcAllPages[len(zgcAllPages)-1]
			zgcAllPages = zgcAllPages[:len(zgcAllPages)-1]
			break
		}
	}
	unlock(&zgcPagesMu)
}

// zgcFreePage unregisters pg and returns its metadata mapping. It runs
// only after reclamation fully drained: the page is unpublished and a
// zgcPageAcqDomain and refs sample both returned zero, so no transient
// reader can still hold a derived pointer and no lease can still name
// the header. A page with a published relocation may go only through
// zgcRetirePage: until a remap pass after its release, stale words
// still resolve through the table, and before release the source and
// the copy would both be live.
func zgcFreePage(pg *zgcPage) {
	if pg.published.Load() != 0 && (pg.released.Load() == 0 || zgcRemapEpoch.Load() == pg.releaseEpoch) {
		throw("zgc: freeing a relocated page before a remap pass retired it")
	}
	if pg.published.Load() != 0 && zgcForeignPagePinnedLocked(pg.start) {
		throw("zgc: freeing a pinned page with published relocation")
	}
	if pg.stateIdx != 0 {
		throw("zgc: freeing a page that still owns a span state")
	}
	zgcPageUnpublish(pg)
	lock(&zgcPagesMu)
	if pg.retiring {
		p := atomic.Loaduintptr(&zgcDirAddr)
		if p == 0 {
			throw("zgc: retiring page directory disappeared")
		}
		if !(*zgcDirectory)(add(nil, p))[pg.start>>zgcPageShift].CompareAndSwap(zgcSlotRetiring, 0) {
			throw("zgc: retiring page tombstone was not owned")
		}
		pg.retiring = false
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

// zgcPageSlotOccupied reports whether the directory already holds a
// registered page or a retiring descriptor for the 2 MB region at base.
// Selection consults the raw slot, not zgcPageFor: a base in a gap between
// the registered page's exact source ranges resolves to nil yet still
// collides on registration, since the directory admits one page or
// retiring descriptor per granule. Lock-free atomic load only, safe in
// the selection pause.
func zgcPageSlotOccupied(base uintptr) bool {
	p := atomic.Loaduintptr(&zgcDirAddr)
	if p == 0 {
		return false
	}
	return (*zgcDirectory)(add(nil, p))[base>>zgcPageShift].Load() != 0
}

// zgcPageFor returns the registered page whose relocation sources contain
// addr, or nil. The directory is keyed by 2 MB region, but a region may
// hold more than the page's quarantined source spans (free pages, stacks
// and other manual spans, objects allocated after selection, the page's
// own to-space reservation). Those bytes are not relocation sources: no
// forwarding entry will ever name them, nothing there may be copied,
// healed through the table, or released, and every caller that asks "is
// this word on a page that is relocating" must see nil for them. Every
// Protected-internal lookup for callers that already exclude
// reclamation: stop-the-world pauses, the foreign-gate commit section
// (no STW under the gate), and lock-held registry sections. A
// world-running reader without such exclusion must go through
// zgcPageAcquire (retained handle) or zgcPageTransientEnter plus
// zgcPageLookupTransient (bounded read), never this.
func zgcPageFor(addr uintptr) *zgcPage {
	p := atomic.Loaduintptr(&zgcDirAddr)
	if p == 0 || addr > zgcAddrMask {
		return nil
	}
	v := (*zgcDirectory)(add(nil, p))[addr>>zgcPageShift].Load()
	if v == 0 || v == zgcSlotRetiring {
		return nil
	}
	pg := (*zgcPage)(add(nil, v))
	if !pg.holdsSource(addr) {
		return nil
	}
	return pg
}

// zgcPageTransientEnter opens a transient read domain. Every raw
// pointer a reader derives inside the domain (directory slot, header,
// source ranges, forwarding entries) stays valid until the matching
// zgcPageTransientLeave: reclamation samples the counter zero after
// unpublishing and frees nothing while a domain is open. Balanced and
// nestable; nosplit-safe so signal-stack and no-P readers can hold a
// domain across their whole bounded read. Nothing derived inside the
// domain may escape it; a reader that must keep the page beyond the
// bounded read takes a lease with zgcPageAcquire instead.
//
//go:nosplit
func zgcPageTransientEnter() {
	zgcPageAcqDomain.Add(1)
}

// zgcPageTransientLeave closes one open transient read domain.
//
//go:nosplit
func zgcPageTransientLeave() {
	zgcPageAcqDomain.Add(^uintptr(0))
}

// zgcPageLookupTransient resolves addr to the registered page whose
// relocation sources contain it, or nil: the exact membership filter of
// zgcPageFor, inside a transient read domain. Legal only between
// zgcPageTransientEnter and zgcPageTransientLeave, and its result must
// not escape the domain: it carries no lease.
//
//go:nosplit
func zgcPageLookupTransient(addr uintptr) *zgcPage {
	p := atomic.Loaduintptr(&zgcDirAddr)
	if p == 0 || addr > zgcAddrMask {
		return nil
	}
	v := (*zgcDirectory)(add(nil, p))[addr>>zgcPageShift].Load()
	if v == 0 || v == zgcSlotRetiring {
		return nil
	}
	pg := (*zgcPage)(add(nil, v))
	if !pg.holdsSource(addr) {
		return nil
	}
	return pg
}

// zgcPageAcquire resolves addr like zgcPageLookupTransient and returns
// a leased handle: the per-page ref is gained while the acquisition is
// still announced, before the domain closes, so reclamation — which
// samples the domain first and the refs afterwards — cannot free the
// header between the load and the ref. nil means addr is not a
// relocation source of any registered page. On every non-nil result the
// caller owes exactly one zgcPageRelease, on every path including
// refusals, retries and lost races.
//
// Nosplit: the whole body is atomic counter and directory loads plus
// the nosplit membership filter, so a caller may gain a lease while
// holding the M or the foreign gate — the changed-slot revalidation
// that must reacquire before its protected window ends. It allocates
// nothing, takes no lock, and grows no stack.
//
//go:nosplit
func zgcPageAcquire(addr uintptr) *zgcPage {
	zgcPageTransientEnter()
	pg := zgcPageLookupTransient(addr)
	if pg != nil {
		pg.refs.Add(1)
	}
	zgcPageTransientLeave()
	return pg
}

// zgcPageRelease drops one lease gained by zgcPageAcquire, or by
// zgcNextUnfinishedPage under the registry lock. Strict by contract, so
// a released nil is a caller defect, not a tolerated no-op: release
// exactly the handle acquire returned.
//
//go:nosplit
func zgcPageRelease(pg *zgcPage) {
	pg.refs.Add(^uintptr(0))
}

// sourceRangeAt returns the i-th source range. i < srcN.
//
//go:nosplit
func (pg *zgcPage) sourceRangeAt(i uintptr) *zgcSourceRange {
	return (*zgcSourceRange)(unsafe.Pointer(uintptr(pg.src) + i*unsafe.Sizeof(zgcSourceRange{})))
}

// sourceEnd returns the end of the source range containing addr and
// reports whether there is one. It reads immutable metadata: no lock, no
// allocation, safe under the foreign gate and in nosplit code.
//
//go:nosplit
func (pg *zgcPage) sourceEnd(addr uintptr) (uintptr, bool) {
	lo, hi := uintptr(0), pg.srcN
	stride := unsafe.Sizeof(zgcSourceRange{})
	base := uintptr(pg.src)
	for lo < hi {
		m := (lo + hi) >> 1
		r := (*zgcSourceRange)(unsafe.Pointer(base + m*stride))
		if r.hi <= addr {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo == pg.srcN {
		return 0, false
	}
	r := (*zgcSourceRange)(unsafe.Pointer(base + lo*stride))
	if r.lo > addr {
		return 0, false
	}
	return r.hi, true
}

// holdsSource reports whether addr lies in a source range of pg.
// It searches the immutable source ranges directly as a leaf function
// without nested calls: nosplit callers on deep call stacks (e.g. untag
// during write barriers under -N -l) have bounded stack budgets that
// would overflow if holdsSource called through sourceEnd and sourceRangeAt.
//
//go:nosplit
func (pg *zgcPage) holdsSource(addr uintptr) bool {
	lo, hi := uintptr(0), pg.srcN
	stride := unsafe.Sizeof(zgcSourceRange{})
	base := uintptr(pg.src)
	for lo < hi {
		m := (lo + hi) >> 1
		r := (*zgcSourceRange)(unsafe.Pointer(base + m*stride))
		if r.hi <= addr {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo == pg.srcN {
		return false
	}
	r := (*zgcSourceRange)(unsafe.Pointer(base + lo*stride))
	return r.lo <= addr
}

// holdsSourceObject reports whether the size bytes from addr lie inside
// one source range of pg. A copy moves exactly size bytes, so a source that
// spills out of its range would read, and relocate, memory the page does
// not own.
//
//go:nosplit
func (pg *zgcPage) holdsSourceObject(addr, size uintptr) bool {
	end, ok := pg.sourceEnd(addr)
	return ok && size <= end-addr
}

// regionHoldsHeapArena reports whether a heap arena covers any byte of the
// page region [start, start+size). Heap arenas hold every span, goroutine
// stacks and other manual spans included, so only a region with no arena
// is wholly owned by whoever registered a page on it. The arena table never
// shrinks, so a false answer stays false.
func regionHoldsHeapArena(start, size uintptr) bool {
	for ai, last := arenaIndex(start), arenaIndex(start+size-1); ai <= last; ai++ {
		if arenaL1Bits == 0 {
			if ai.l2() < uint(len(mheap_.arenas[0])) && mheap_.arenas[0][ai.l2()] != nil {
				return true
			}
		} else if ai.l1() < uint(len(mheap_.arenas)) {
			if l2 := mheap_.arenas[ai.l1()]; l2 != nil && l2[ai.l2()] != nil {
				return true
			}
		}
	}
	return false
}

// prepareSources records the bytes of spans, the quarantined source spans
// of pg, as pg's exact source ownership, and reports whether pg may enter
// relocation. It runs once per page in the selection pause, before any
// to-space reservation or quarantine, so the lock-free readers never see
// the ranges change; it sorts and coalesces in the page's own fixed
// storage and allocates nothing. A span that is not in use, leaves the
// region, or overlaps another makes the page ineligible (or, for an
// overlap, which in-use spans can never do, fatal).
func (pg *zgcPage) prepareSources(spans []*mspan) bool {
	if pg.srcExact {
		return true
	}
	// Nothing resolves until the ranges below are complete: a page that
	// is rejected half-built owns no byte.
	pg.srcN = 0
	if len(spans) == 0 {
		return false
	}
	end := pg.start + pg.size
	rs := (*[zgcMaxSourceRanges]zgcSourceRange)(pg.src)
	var n uintptr
	for _, s := range spans {
		if s == nil || s.state.get() != mSpanInUse {
			return false
		}
		lo, hi := s.base(), s.base()+s.npages*pageSize
		if lo < pg.start || hi > end || lo >= hi {
			return false
		}
		if n == zgcMaxSourceRanges {
			throw("zgc: more source spans than runtime pages in the region")
		}
		i := n
		for i > 0 && rs[i-1].lo > lo {
			rs[i] = rs[i-1]
			i--
		}
		if i > 0 && rs[i-1].hi > lo || i < n && rs[i].lo < hi {
			throw("zgc: overlapping in-use source spans")
		}
		rs[i] = zgcSourceRange{lo, hi}
		n++
	}
	m := uintptr(0)
	for i := uintptr(0); i < n; i++ {
		if m > 0 && rs[m-1].hi == rs[i].lo {
			rs[m-1].hi = rs[i].hi
		} else {
			rs[m] = rs[i]
			m++
		}
	}
	pg.srcN = m
	pg.srcExact = true
	return true
}

// releaseSources returns exactly the page's source ranges to the OS. Each
// range is shrunk inward to whole system pages: a system page larger than
// a runtime page would otherwise drag the bytes beside the range, which
// may be a neighbour's, with it. Nothing outside the ranges is touched;
// the gaps between them keep whatever owns them.
func (pg *zgcPage) releaseSources() {
	for i := uintptr(0); i < pg.srcN; i++ {
		r := pg.sourceRangeAt(i)
		lo, hi := alignUp(r.lo, physPageSize), alignDown(r.hi, physPageSize)
		if lo < hi {
			zgcReleasePages(add(nil, lo), hi-lo)
		}
	}
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
	if !pg.holdsSource(from) {
		throw("zgc: forwarding source outside its page's source ranges")
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

// zgcRelocateObject is the single authoritative copy commit for the zgc
// cycle. Preparation (claim/reservation, heap bits) happens outside the
// gate in the caller and may allocate or safepoint; this commit
// revalidates under the shared admission gate and performs exactly one
// memmove. The gate-held section holds zgcForeignGate across the
// exposure recheck, the pin-bit/sticky recheck, the memmove,
// forwardPublish, identity moveLocked and published accounting, so
// exposure publication or a concurrent pin commit cannot interleave
// between the check and the publication. No mheap.alloc, STW, safepoint
// or P/gcw operation runs while the gate is held. Callers must revalidate
// pg/forward after any allocation/safepoint by reaching this commit: the
// gate recheck is authoritative, any outside pre-check is racy.
// Source membership: only bytes inside the page's prepared source ranges
// are relocation sources (zgcPageFor filters on them, and the commit
// requires [from, from+size) to lie inside one range). Anything else in
// the 2 MB region (a gap object, the page's own to-space) is never copied
// and never published: the commit returns 0 and nothing is moved.
// Held sources freeze: an object-exposed source is never moved, and a
// source carrying a committed pin (pinner bit or sticky span flag) is
// never moved. The exposure test is the exact per-object one, not the
// region query: an exposed object elsewhere in the region (a gap object,
// a to-space copy) does not hold its neighbours, and freezing every
// source of the page for it would make a lawful pin or foreign admission
// of a source object fail. The pin test reads the source span's sticky
// flag and pinner bits with atomic loads only, so no speciallock is taken
// under the gate and M -> speciallock -> gate order is preserved. The
// commit returns 0 so the caller unclaims its slot and treats
// the object as unfinished for this cycle. Lock order is identity -> gate
// (gate leaf-last, taken last, no lock acquired while it is held); the
// identity lock is acquired first and held across reservation, gate and
// commit, so no concurrent identity insert can consume the reservation.
// reserveMoveLocked (may sysAlloc/sysFree) runs under identity lock outside
// the gate; moveLockedNoGrow under both locks never resizes/sysAllocs.
// Gate-held graph: PageFor/forward/source ranges (atomics and immutable
// metadata) + ObjectExposedLocked (span/alloc-bit reads, set lookups) +
// sticky flag and pinner-bit loads (atomics only, no speciallock) +
// memmove + forwardPublish (CAS) + moveNoGrow (find/remove/place, no
// grow) + published.Add. No mheap.alloc, sysAlloc/sysFree, STW, safepoint
// or P/gcw under gate.
func zgcRelocateObject(pg *zgcPage, from, size, to uintptr) uintptr {
	if pg == nil {
		return 0
	}
	t := &zgcIdentity
	lock(&t.lock)
	t.reserveMoveLocked(from, to)
	zgcForeignGateLock()
	if zgcPageFor(from) != pg {
		if won, ok := pg.forward(from); ok {
			zgcForeignGateUnlock()
			unlock(&t.lock)
			return won
		}
		zgcForeignGateUnlock()
		unlock(&t.lock)
		return 0
	}
	if won, ok := pg.forward(from); ok {
		zgcForeignGateUnlock()
		unlock(&t.lock)
		return won
	}
	if !pg.holdsSourceObject(from, size) || zgcForeignObjectExposedLocked(from) {
		zgcForeignGateUnlock()
		unlock(&t.lock)
		return 0
	}
	// No span means no sticky flag and no pinner bits to protect, so a
	// spanless source (e.g. synthetic sysMap registration) stays
	// copyable; only a heap span can veto.
	if srcSpan := spanOfHeap(from); srcSpan != nil {
		if srcSpan.logosPinned.Load() {
			zgcForeignGateUnlock()
			unlock(&t.lock)
			return 0
		}
		if pb := srcSpan.getPinnerBits(); pb != nil {
			idx := srcSpan.objIndex(from)
			if idx >= uintptr(srcSpan.nelems) {
				zgcForeignGateUnlock()
				unlock(&t.lock)
				return 0
			}
			state := pb.ofObject(idx)
			if state.isPinned() {
				zgcForeignGateUnlock()
				unlock(&t.lock)
				return 0
			}
		}
	}
	memmove(add(nil, to), add(nil, from), size)
	won, mine := pg.forwardPublish(from, to)
	if mine {
		t.moveLockedNoGrow(from, to)
		pg.published.Add(size)
		// Direct commits bypass the claim path, so ensure the dest mark
		// here; free slots are never allocated by this call.
		zgcEnsureDestRegistered(to)
	}
	zgcForeignGateUnlock()
	unlock(&t.lock)
	return won
}

// zgcReleasePage returns the source page to the OS and reports whether it
// did. It refuses while any live byte of the page has no published copy:
// those bytes are the only copy of their object. After release, a stale
// word still resolves through the forwarding table, which lives outside
// the page.
// Exact ranges: release returns the prepared source ranges and nothing
// else (releaseSources). The 2 MB region may also hold free pages,
// goroutine stacks and other manual spans, and mutator objects allocated
// after selection; none of them belongs to the page, so none is touched.
// A page whose ranges were never prepared (not srcExact) may be released
// whole only when no heap arena covers its region: it is then a synthetic
// registration over memory nothing else can own. Over a heap arena it
// would zero memory the page cannot account for, which is runtime
// corruption, so it throws instead of releasing or quietly refusing.
// Source freeze: a pinned page stays unreleased while foreign code may
// retain raw addresses (a pin anywhere in the region only delays release;
// it never frees a byte). The ranges are immutable, so the gate-held
// section reads them without a lock and allocates nothing; the only call
// under the gate is the release syscall, which takes no runtime lock.
func zgcReleasePage(pg *zgcPage) bool {
	if pg.released.Load() != 0 {
		return true
	}
	if pg.published.Load() != pg.live {
		return false
	}
	if logosZgcCommitLedgerDebug {
		lock(&mheap_.lock)
		for i := range pg.srcN {
			r := pg.sourceRangeAt(i)
			lo, hi := alignUp(r.lo, physPageSize), alignDown(r.hi, physPageSize)
			if lo < hi {
				logosZgcCommitLedgerAssertGrant(lo, hi-lo, "release")
			}
		}
		unlock(&mheap_.lock)
	}
	if !pg.srcExact && regionHoldsHeapArena(pg.start, pg.size) {
		throw("zgc: releasing a heap page whose source spans were never prepared")
	}
	zgcForeignGateLock()
	if zgcForeignPagePinnedLocked(pg.start) {
		zgcForeignGateUnlock()
		return false
	}
	pg.releaseEpoch = zgcRemapEpoch.Load()
	pg.releaseSources()
	pg.released.Store(1)
	zgcForeignGateUnlock()
	if logosZgcCommitLedgerDebug {
		lock(&mheap_.lock)
		for i := range pg.srcN {
			r := pg.sourceRangeAt(i)
			lo, hi := alignUp(r.lo, physPageSize), alignDown(r.hi, physPageSize)
			if lo < hi {
				logosZgcCommitLedgerRemove(lo, hi-lo, "release")
			}
		}
		unlock(&mheap_.lock)
	}
	return true
}
