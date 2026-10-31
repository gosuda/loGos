// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/gc"
	"unsafe"
)

var zgcTestNext atomic.Uintptr

// Test seam for M19-S7: opaque wrappers over the zgc colored-pointer
// helpers for package runtime_test, which cannot name unexported
// runtime symbols and must not dereference synthetic addresses.

const (
	LogosZgcAddrBits     = zgcAddrBits
	LogosZgcAddrMask     = uint64(zgcAddrMask)
	LogosZgcTagRegion    = uint64(zgcTagRegionMask)
	LogosZgcColorMask    = uint64(zgcColorMask)
	LogosZgcPageSize     = zgcPageSize
	LogosZgcEvacPercent  = zgcEvacPercent
	LogosZgcMark0        = uint8(zgcColorMark0)
	LogosZgcMark1        = uint8(zgcColorMark1)
	LogosZgcRemapped     = uint8(zgcColorRemapped)
	LogosZgcFinal        = uint8(zgcColorFinal)
	LogosZgcColorNibble  = uint8(zgcColorMark0 | zgcColorMark1 | zgcColorRemapped | zgcColorFinal)
	LogosZgcMaxPageBytes = zgcMaxPageBytes
	// LogosZgcRelocateDone is the zgcRelocateStatus of a committed copy.
	LogosZgcRelocateDone = int(zgcRelocateDone)
)

// LogosZgcPage is the page metadata header the forwarding table lives in.
type LogosZgcPage = zgcPage

// LogosZgcSoftBits returns the software encoding of color c.
func LogosZgcSoftBits(c uint8) uint64 { return uint64(zgcSoftBits(zgcColor(c))) }

// LogosZgcTagBits returns the hardware-tag encoding of color c.
func LogosZgcTagBits(c uint8) uint64 { return uint64(zgcTagBits(zgcColor(c))) }

// LogosZgcAddr unmasks v to its canonical address.
func LogosZgcAddr(v uintptr) uintptr { return zgcAddr(v) }

// LogosZgcSelectSoftware selects the software encoding and the good color.
func LogosZgcSelectSoftware(good uint8) { zgcConfigure(zgcEncodingSoftware, zgcColor(good)) }

// LogosZgcSelectHardware selects the hardware-tag encoding and the good color.
func LogosZgcSelectHardware(good uint8) { zgcConfigure(zgcEncodingHardware, zgcColor(good)) }

// LogosZgcFlip makes good the good color.
func LogosZgcFlip(good uint8) { zgcFlip(zgcColor(good)) }

// LogosZgcGoodBits returns the encoded good color.
func LogosZgcGoodBits() uintptr { return zgcGoodBits() }

// LogosZgcColorMaskBits returns the encoded mask of all color bits the load
// barrier compares against the good color.
func LogosZgcColorMaskBits() uintptr { return atomic.Loaduintptr(&zgcColorMaskBits) }

// LogosZgcColored returns addr carrying color c in the active encoding.
func LogosZgcColored(addr uintptr, c uint8) uintptr { return addr + zgcColorBits(zgcColor(c)) }

// LogosZgcHeals returns the number of self-heal slow paths taken.
func LogosZgcHeals() uint64 { return zgcHeals.Load() }

// LogosZgcBarrierCode returns the machine code of the load barrier,
// from its entry to the entry of the next function in the module.
func LogosZgcBarrierCode() []byte {
	pc := abi.FuncPCABIInternal(zgcLoadBarrier)
	fn := findfunc(pc)
	if !fn.valid() {
		return nil
	}
	next := ^uint32(0)
	for _, ft := range fn.datap.ftab {
		if ft.entryoff > fn._func.entryOff && ft.entryoff < next {
			next = ft.entryoff
		}
	}
	code := make([]byte, next-fn._func.entryOff)
	copy(code, unsafe.Slice((*byte)(add(nil, pc)), len(code)))
	return code
}

// LogosZgcLoad runs the load barrier on the word at slot.
func LogosZgcLoad(slot *uintptr) uintptr { return zgcLoadBarrier(slot) }

// LogosZgcLoadSlow runs the slow half of the compiled load barrier on the
// word v loaded from slot.
func LogosZgcLoadSlow(slot *uintptr, v uintptr) uintptr { return zgcLoadBarrierSlow(slot, v) }

// LogosZgcRemap returns the relocated address of canonical addr.
func LogosZgcRemap(addr uintptr) uintptr { return zgcRemap(addr) }

// LogosZgcUnmaskBits returns the word that compiled barriers AND a good
// word with.
func LogosZgcUnmaskBits() uintptr { return atomic.Loaduintptr(&zgcUnmaskBits) }

// LogosZgcReset returns the color state to the one before zgc is
// configured: no color bits, so every compiled barrier takes its fast
// path and keeps the word.
func LogosZgcReset() {
	zgcHardwareTagging = false
	atomic.Storeuintptr(&zgcColorMaskBits, 0)
	atomic.Storeuintptr(&zgcGoodMask, 0)
	atomic.Storeuintptr(&zgcUnmaskBits, ^uintptr(0))
}

// LogosZgcUntag clears every color and tag bit of p.
func LogosZgcUntag(p uintptr) uintptr { return uintptr(zgcUntag(add(nil, p))) }

// LogosZgcEnableHardwareTagging reports whether the OS accepts hardware
// pointer tagging for this process and the errno that refused it.
func LogosZgcEnableHardwareTagging() (ok bool, errno uintptr) { return zgcEnableHardwareTagging() }

// LogosZgcFragmentationExceeds reports whether garbage bytes of size
// bytes select a page for evacuation.
func LogosZgcFragmentationExceeds(garbage, size uintptr) bool {
	return zgcFragmentationExceeds(garbage, size)
}

// LogosZgcMapPagesRaw maps n zeroed pages aligned to LogosZgcPageSize
// inside the 4 TB range, which the kernel's default mmap area is not in.
// It returns the aligned base and the raw mapping that LogosZgcUnmapPages
// takes.
func LogosZgcMapPagesRaw(n int) (base, raw uintptr) {
	total := uintptr(n+1) * zgcPageSize
	zgcTestNext.CompareAndSwap(0, 1<<40)
	hint := zgcTestNext.Add(total+zgcPageSize) - (total + zgcPageSize)
	r := sysReserve(add(nil, hint), total, "logos zgc test")
	if r == nil || uintptr(r)+total > zgcAddrMask+1 {
		throw("zgc test: kernel placed the mapping outside the 4 TB range")
	}
	sysMap(r, total, &memstats.other_sys, "logos zgc test")
	sysUsed(r, total, total)
	return alignUp(uintptr(r), zgcPageSize), uintptr(r)
}

// LogosZgcUnmapPages releases a mapping made by LogosZgcMapPagesRaw.
func LogosZgcUnmapPages(raw uintptr, n int) {
	sysFree(add(nil, raw), uintptr(n+1)*zgcPageSize, &memstats.other_sys)
}

// LogosZgcStore writes v at addr.
func LogosZgcStore(addr, v uintptr) { *(*uintptr)(add(nil, addr)) = v }

// LogosZgcWord reads the word at addr.
func LogosZgcWord(addr uintptr) uintptr { return *(*uintptr)(add(nil, addr)) }

// LogosZgcSlotAddr returns the address of slot.
func LogosZgcSlotAddr(slot *uintptr) uintptr { return uintptr(unsafe.Pointer(slot)) }

// LogosZgcFill sets n bytes at addr to b.
func LogosZgcFill(addr, n uintptr, b byte) {
	s := unsafe.Slice((*byte)(add(nil, addr)), n)
	for i := range s {
		s[i] = b
	}
}

// LogosZgcAllZero reports whether the n bytes at addr are all zero.
func LogosZgcAllZero(addr, n uintptr) bool {
	for _, b := range unsafe.Slice((*byte)(add(nil, addr)), n) {
		if b != 0 {
			return false
		}
	}
	return true
}

// LogosZgcOffHeap reports whether addr lies outside every heap span.
func LogosZgcOffHeap(addr uintptr) bool { return spanOf(addr) == nil }

// LogosZgcSpanStart returns the base of the in-use heap span holding addr,
// and whether there is one. zgcRemap resolves an interior pointer to its
// object start through exactly this extent, so a fixture that tests
// interior remapping must prove its sources share a span instead of
// assuming the allocator placed them in one. A page fixture also uses it to
// prove a mapped region is not heap memory.
func LogosZgcSpanStart(addr uintptr) (base uintptr, ok bool) {
	s := spanOfHeap(addr)
	if s == nil {
		return 0, false
	}
	return s.base(), true
}

// LogosZgcNewPage creates a registered page header with a forwarding
// table sized for nObjects.
func LogosZgcNewPage(start, live uintptr, nObjects int) *LogosZgcPage {
	pg := zgcNewPage(start, zgcPageSize, live, uintptr(nObjects))
	logosZgcTestPages = append(logosZgcTestPages, pg)
	return pg
}

// LogosZgcNewDetachedPage creates an unregistered header of arbitrary
// size for selection tests.
func LogosZgcNewDetachedPage(size, live uintptr) *LogosZgcPage {
	return zgcNewDetachedPage(size, live)
}

// LogosZgcFreePage discards pg through the drain discipline: unpublish,
// then free the metadata immediately when no transient domain or lease
// is live, or defer the free to the next drained retire pass while a
// reader is held. A deferred request needs the world stopped — the
// deferred queue is stop-the-world bookkeeping — so a world-running
// caller holding a transient domain fails loud instead of leaking.
func LogosZgcFreePage(pg *LogosZgcPage) {
	zgcAbandonPage(pg)
}

// LogosZgcPageEvacuable reports whether pg is over the evacuation
// threshold.
func LogosZgcPageEvacuable(pg *LogosZgcPage) bool { return pg.evacuable() }

// LogosZgcPageFor returns the registered page containing addr.
func LogosZgcPageFor(addr uintptr) *LogosZgcPage { return zgcPageFor(addr) }

// LogosZgcPageSlotFree reports whether the directory granule holding addr
// has no registered page or pending retiring descriptor. A pending
// descriptor keeps its retiring tombstone in the slot until reclamation,
// so the predicate stays false across the deferred interval.
//
// It is the exact predicate selection uses before it claims a granule
// (zgcPageSlotOccupied), inverted, so a test cannot drift from the claim
// it is trying to satisfy.
//
// zgcPageFor answers a narrower question: a page whose exact source ranges
// exclude addr, or whose address sits in a gap between them, resolves to
// nil while its slot stays occupied. A test that registers a page over a
// granule must ask about the slot instead, because registration is per
// granule.
func LogosZgcPageSlotFree(addr uintptr) bool {
	base := addr &^ (zgcPageSize - 1)
	if base > zgcAddrMask-(zgcPageSize-1) {
		// No granule wholly inside the address field ends here, so
		// registration refuses it whatever the directory holds; the
		// directory is not indexed, since base>>zgcPageShift would leave it.
		return false
	}
	return !zgcPageSlotOccupied(base)
}

// LogosZgcPageAddr returns the address of the header itself.
func LogosZgcPageAddr(pg *LogosZgcPage) uintptr { return uintptr(unsafe.Pointer(pg)) }

// LogosZgcTableAddr returns the address and byte length of the
// forwarding table.
func LogosZgcTableAddr(pg *LogosZgcPage) (addr, n uintptr) {
	return uintptr(pg.fwd), (pg.fwdMask + 1) * 8
}

// LogosZgcForwardInsert publishes from -> to and returns the winner.
func LogosZgcForwardInsert(pg *LogosZgcPage, from, to uintptr) uintptr {
	return pg.forwardInsert(from, to)
}

// LogosZgcForward looks up the relocated address of from.
func LogosZgcForward(pg *LogosZgcPage, from uintptr) (uintptr, bool) { return pg.forward(from) }

// LogosZgcCopyOneResult runs the production claim-and-commit copy path
// for the object at src in span i of run: the real claim, bitmap write,
// slot finish and zgcRelocateObject commit, unmodified. The status is
// LogosZgcRelocateDone or one of its failure siblings.
func LogosZgcCopyOneResult(pg *LogosZgcPage, r *LogosZgcSpanRun, i int, src, size uintptr) (uintptr, int) {
	won, st := zgcCopyOneResult(pg, r.spans[i], src, size)
	return won, int(st)
}

// LogosZgcSetSpanSmallBits stores bits into the pointer bitmap of the
// object slot at addr in span i of run, the fixture's way to give a
// source object its pointer map.
func LogosZgcSetSpanSmallBits(r *LogosZgcSpanRun, i int, addr, bits uintptr) {
	r.spans[i].zgcSetSmallBits(addr, bits)
}

// LogosZgcSpanSmallBits reads the pointer bitmap of the object slot at
// addr in span i of run: the exact word a scanner consults for that
// slot.
func LogosZgcSpanSmallBits(r *LogosZgcSpanRun, i int, addr uintptr) uintptr {
	return r.spans[i].heapBitsSmallForAddr(addr)
}

// LogosZgcToSpaceSmallBits reads the pointer bitmap of the object slot
// at addr in pg's i-th reserved to-space span: the exact word a scanner
// consults for that slot once the page publishes.
func LogosZgcToSpaceSmallBits(pg *LogosZgcPage, i int, addr uintptr) uintptr {
	st := zgcPageStateIfAny(pg)
	if st == nil || i < 0 || i >= len(st.toSpace) || st.toSpace[i] == nil {
		throw("zgc test: no reserved to-space span for the bitmap read")
	}
	return st.toSpace[i].heapBitsSmallForAddr(addr)
}

// LogosZgcRelocate copies size bytes from to dst and publishes the
// forwarding entry.
func LogosZgcRelocate(pg *LogosZgcPage, from, size, dst uintptr) uintptr {
	return zgcRelocateObject(pg, from, size, dst)
}

// LogosZgcRelease returns the source page to the OS and reports whether it
// did; it refuses while a live object has no published copy.
func LogosZgcRelease(pg *LogosZgcPage) bool { return zgcReleasePage(pg) }

// LogosZgcRetire unregisters and frees a released page once a remap pass
// completed after its release, and reports whether it did.
func LogosZgcRetire(pg *LogosZgcPage) bool { return zgcRetirePage(pg) }

// logosZgcTestPages holds the pages tests created with LogosZgcNewPage,
// so a reset frees exactly those and never a page a collection owns.
var logosZgcTestPages []*LogosZgcPage

// LogosZgcDropTestPages frees every page a test registered, so a granule
// an earlier test owns cannot collide with a new synthetic page. Pages
// registered by the collector are left alone: their spans are live.
func LogosZgcDropTestPages() {
	pages := logosZgcTestPages
	logosZgcTestPages = nil
	for _, pg := range pages {
		LogosZgcDropPage(pg)
	}
}

// LogosZgcDropPage frees a page a test registered over memory it still
// owns, whatever it published. The collector never does this: a page
// with published relocations leaves only through zgcRetirePage. The
// free goes through the same drain discipline as LogosZgcFreePage, so a
// held transient reader defers the metadata free instead of racing it.
func LogosZgcDropPage(pg *LogosZgcPage) {
	for i, p := range logosZgcTestPages {
		if p == pg {
			logosZgcTestPages = append(logosZgcTestPages[:i], logosZgcTestPages[i+1:]...)
			break
		}
	}
	pg.published.Store(0)
	zgcAbandonPage(pg)
}

// LogosZgcRemapDone records a completed remap pass.
func LogosZgcRemapDone() { zgcRemapDone() }

// LogosZgcKeyHash is the hash of the pointer word v as a map key under zgc.
func LogosZgcKeyHash(v, seed uintptr) uintptr { return zgcKeyHash(v, seed) }

// LogosZgcPtrEqual reports whether the pointer words x and y name the same
// location now.
func LogosZgcPtrEqual(x, y uintptr) bool {
	return zgcptrequal(noescape(unsafe.Pointer(&x)), noescape(unsafe.Pointer(&y)))
}

// LogosZgcTypeHash is runtime.typehash of the value of type T at p.
func LogosZgcTypeHash[T any](p *T, seed uintptr) uintptr {
	return typehash(abi.TypeFor[T](), unsafe.Pointer(p), seed)
}

// LogosZgcEfaceEqual is the runtime interface comparison of x and y.
func LogosZgcEfaceEqual(x, y any) bool {
	ex, ey := efaceOf(&x), efaceOf(&y)
	return ex._type == ey._type && efaceeq(ex._type, ex.data, ey.data)
}

// LogosZgcIdentityKeysOn makes typehash and interface equality treat
// pointer words by identity, as -X runtime.logosGCMode=zgc does. No map
// whose hashes depend on it may live across LogosZgcIdentityKeysOff.
func LogosZgcIdentityKeysOn() { zgcIdentityForced = true }

// LogosZgcIdentityKeysOff undoes LogosZgcIdentityKeysOn.
func LogosZgcIdentityKeysOff() { zgcIdentityForced = false }

// LogosZgcHasIdentity reports whether addr holds an identity entry.
func LogosZgcHasIdentity(addr uintptr) bool {
	t := &zgcIdentity
	lock(&t.lock)
	ok := t.find(addr) != nil
	unlock(&t.lock)
	return ok
}

// LogosZgcPageReleased reports whether pg was released.
func LogosZgcPageReleased(pg *LogosZgcPage) bool { return pg.released.Load() != 0 }

// LogosZgcIdentityHash returns the identity hash of addr.
func LogosZgcIdentityHash(addr uintptr) uint32 { return zgcIdentityHash(add(nil, addr)) }

// LogosZgcIdentityMove carries the identity hash from one address to another.
func LogosZgcIdentityMove(from, to uintptr) { zgcIdentityMove(add(nil, from), add(nil, to)) }

// LogosZgcIdentityForget drops the identity hash of addr.
func LogosZgcIdentityForget(addr uintptr) { zgcIdentityForget(add(nil, addr)) }

// LogosZgcIdentityLen returns the number of live identity entries.
func LogosZgcIdentityLen() int { return zgcIdentityLen() }

// LogosZgcPointerHash is the map hash a pointer key takes under zgc.
func LogosZgcPointerHash(addr, seed uintptr) uintptr { return zgcPointerHash(add(nil, addr), seed) }

// LogosZgcCycleEnabled reports whether the zgc concurrent cycle runs.
func LogosZgcCycleEnabled() bool { return zgcCycleEnabled() }

// LogosZgcInit probes hardware tagging and configures the color encoding.
func LogosZgcInit() { zgcInit() }

// LogosZgcUntagArg clears the color and tag bits of a boundary-crossing
// pointer argument. Foreign words pass through.
func LogosZgcUntagArg(p uintptr) uintptr { return uintptr(logosZgcUntagArg(add(nil, p))) }

// LogosZgcCanonicalStringData returns the canonical data pointer of *sp.
func LogosZgcCanonicalStringData(sp *string) unsafe.Pointer { return zgcCanonicalStringData(sp) }

// LogosZgcRemapScanWord resolves a scanned word to its canonical address.
func LogosZgcRemapScanWord(p uintptr) (uintptr, bool) { return zgcRemapScanWord(p) }

// LogosZgcHealScanSlot rewrites a scanned slot to its uncolored canonical
// word.
func LogosZgcHealScanSlot(slot *uintptr) { zgcHealScanSlot(slot) }

// LogosZgcBoundaryLanded reports whether the Z2-asm wrapper set landed.
func LogosZgcBoundaryLanded() bool { return zgcBoundaryWrappersLanded() }

// LogosZgcCanRelocateDeep reports whether the current context may run the
// relocation deep path.
func LogosZgcCanRelocateDeep() bool { return zgcCanRelocateDeep() }

// LogosZgcSpanQuarantinedAt reports whether the heap span holding addr is
// a quarantined source span.
func LogosZgcSpanQuarantinedAt(addr uintptr) bool {
	s := spanOfHeap(addr)
	return s != nil && zgcSpanQuarantined(s)
}

// LogosZgcSweepGuardAt runs the sweep entry guard for the heap span
// holding addr and reports whether the sweeper must skip it.
func LogosZgcSweepGuardAt(addr uintptr) bool {
	s := spanOfHeap(addr)
	return s != nil && zgcSweepGuard(s)
}

// LogosZgcRemapEpoch returns the completed-remap-pass count.
func LogosZgcRemapEpoch() uint32 { return zgcRemapEpoch.Load() }

// LogosZgcPinSelfStack conservatively pins the calling goroutine's own
// stack and reports how many pages hold a pinned word.
func LogosZgcAttachCanCopy() bool { return zgcCanRelocateDeep() }

func LogosZgcAttachEnsureCopy(p uintptr) uintptr {
	return uintptr(zgcAttachEnsureCopy(add(nil, p)))
}

func LogosZgcPinSelfStack() int {
	ev := zgcNewPinEv()
	defer ev.pinned.release()
	zgcPinSelfStack(&ev)
	return int(ev.pinned.count)
}

// W6 pin-test seam: opaque handles over the pin pass and its synthetic
// holders for package runtime_test, which cannot name unexported runtime
// symbols. Each wrapper is a one-line pass-through; every assertion stays
// in logos_zgc_diagnostic_test.go and runs through real behavior.

// LogosZgcW6Pin is a page-granular pin set over the shared pin core.
type LogosZgcW6Pin struct {
	ev logosEvac
}

// LogosZgcW6PinNew returns a fresh W6 pin set.
func LogosZgcW6PinNew() *LogosZgcW6Pin {
	ev := zgcNewPinEv()
	return &LogosZgcW6Pin{ev: ev}
}

// LogosZgcW6PinFree releases the off-heap set backing p.
func LogosZgcW6PinFree(p *LogosZgcW6Pin) { p.ev.pinned.release() }

// LogosZgcW6PinOne pins addr and reports whether the page of addr ended
// up pinned: one pinPtr through zgcCanonical, exactly as the cycle runs.
func LogosZgcW6PinOne(p *LogosZgcW6Pin, addr uintptr) bool {
	p.ev.pinPtr(addr)
	return p.ev.pinned.has(zgcCanonical(addr) &^ (p.ev.granule - 1))
}

// LogosZgcW6PinCount reports how many pages p holds pinned.
func LogosZgcW6PinCount(p *LogosZgcW6Pin) int { return int(p.ev.pinned.count) }

// LogosZgcW6PinHas reports whether the page of addr is pinned in p
// without pinning anything new.
func LogosZgcW6PinHas(p *LogosZgcW6Pin, addr uintptr) bool {
	return p.ev.pinned.has(zgcCanonical(addr) &^ (p.ev.granule - 1))
}

// LogosZgcW6G is a synthetic goroutine holder for the W6 state-refusal
// and param-transition assertions: a detached g-shaped record the tests
// park in _Gwaiting, as the debug-call parking leaves it.
type LogosZgcW6G struct {
	gp *g
}

// LogosZgcW6NewG returns a synthetic g parked in _Gwaiting. The handle
// keeps the record alive through its gp pointer.
func LogosZgcW6NewG() *LogosZgcW6G {
	gp := new(g)
	gp.atomicstatus.Store(uint32(_Gwaiting))
	return &LogosZgcW6G{gp: gp}
}

// LogosZgcW6GAddr returns the address of the synthetic g record.
func LogosZgcW6GAddr(h *LogosZgcW6G) uintptr { return uintptr(unsafe.Pointer(h.gp)) }

// LogosZgcW6SetRunning parks the synthetic g in _Grunning when running
// is true and back in _Gwaiting otherwise.
func LogosZgcW6SetRunning(h *LogosZgcW6G, running bool) {
	if running {
		h.gp.atomicstatus.Store(uint32(_Grunning))
		return
	}
	h.gp.atomicstatus.Store(uint32(_Gwaiting))
}

// LogosZgcW6SetParam stores the debug-call args-block word behind
// gp.param, the protocol transition debugCallWrap1 consumes.
func LogosZgcW6SetParam(h *LogosZgcW6G, p unsafe.Pointer) { h.gp.param = p }

// LogosZgcW6GetParam loads the gp.param word.
func LogosZgcW6GetParam(h *LogosZgcW6G) unsafe.Pointer { return h.gp.param }

// LogosZgcW6PinG runs the pin pass over the synthetic g and reports
// whether the pass accepted its state.
func LogosZgcW6PinG(p *LogosZgcW6Pin, h *LogosZgcW6G) bool { return p.ev.pinGoroutine(h.gp) }

// LogosZgcW6PushDefer links one heap defer record calling fn onto the
// synthetic g, so PinG discovery can prove the defer chain is walked.
// The record keeps fn alive through the gp chain.
func LogosZgcW6PushDefer(h *LogosZgcW6G, fn func()) {
	d := new(_defer)
	d.fn = fn
	d.link = h.gp._defer
	h.gp._defer = d
}

// LogosZgcForeignInit preallocates the foreign-closure bitsets before the
// fixture exists, so their first-touch allocation never falls inside the
// fixture window. Idempotent (zgcInitForeignRoots, logos_zgc_roots.go:111-118).
func LogosZgcForeignInit() { zgcInitForeignRoots() }

// LogosZgcForeignG is a synthetic _Gsyscall goroutine for the foreign-closure
// test. The handle keeps the record and its fake stack alive.
type LogosZgcForeignG struct {
	gp    *g
	stack []uintptr
}

// LogosZgcForeignNewG returns a synthetic g parked in _Gsyscall whose stack
// bounds cover nwords of test-owned memory.
func LogosZgcForeignNewG(nwords int) *LogosZgcForeignG {
	h := &LogosZgcForeignG{stack: make([]uintptr, nwords)}
	h.gp = new(g)
	h.gp.atomicstatus.Store(uint32(_Gsyscall))
	h.gp.stack.lo = uintptr(unsafe.Pointer(&h.stack[0]))
	h.gp.stack.hi = h.gp.stack.lo + uintptr(nwords)*goarch.PtrSize
	return h
}

// LogosZgcForeignGSet stores one word in the synthetic foreign stack.
func LogosZgcForeignGSet(h *LogosZgcForeignG, i int, w uintptr) { h.stack[i] = w }

// LogosZgcForeignGAddr returns the address of the synthetic foreign g record.
func LogosZgcForeignGAddr(h *LogosZgcForeignG) uintptr { return uintptr(unsafe.Pointer(h.gp)) }

// LogosZgcForeignRetain runs the transitive closure over live with h as the
// only foreign thread and returns the retained-prefix length. Negative values
// report an invalid fixture (-1) or a marked expectClear object (-2).
func LogosZgcForeignRetain(live []*LogosZgcPage, h *LogosZgcForeignG, mark, expectClear []uintptr) int {
	return logosZgcForeignRetainList(live, h, mark, expectClear, []*g{h.gp}) // world running; no STW allocation below
}

// LogosZgcForeignRetainColored runs the same closure over a goroutine list
// whose single element cell carries the remapped color on the g word: the
// word shape an -lb self-heal can leave in an allgs backing-array entry,
// which the foreign closure plain-loads before the gcStart rewrite. The
// slice header is left canonical in this entry-only control case (see
// LogosZgcForeignRetainColoredHeader for colored slice header coverage).
// Mutable globals, including the heap-backed array of allgs, are eligible
// for self-heal coloring by -lb mutators; readers in NoLB contexts must
// canonicalize the slice header backing array pointer before indexing,
// and each entry pointer before dereference. The colored word resolves to
// live heap (the g record), so zgcCanonical strips it instead of passing
// it through. The colored cell is built before the STW, so the closure
// reads it deterministically before any actual GC; the swap saves allgs
// and restores it immediately.
func LogosZgcForeignRetainColored(live []*LogosZgcPage, h *LogosZgcForeignG, mark, expectClear []uintptr) int {
	one := []*g{h.gp} // world running; no STW allocation below
	*(*uintptr)(unsafe.Pointer(&one[0])) = LogosZgcColored(uintptr(unsafe.Pointer(h.gp)), LogosZgcRemapped)
	return logosZgcForeignRetainList(live, h, mark, expectClear, one)
}

// LogosZgcForeignRetainColoredHeader runs the foreign closure over a goroutine
// list whose slice header array pointer is colored with the remapped color
// (Level 1), while the element *g entry itself remains canonical (Level 2).
// This directly exercises the NoLB slice header array canonicalization
// before range indexing (PC 0x48652a fault reproduction).
func LogosZgcForeignRetainColoredHeader(live []*LogosZgcPage, h *LogosZgcForeignG, mark, expectClear []uintptr) int {
	one := []*g{h.gp} // world running; no STW allocation below
	return logosZgcForeignRetainColoredHeaderList(live, h, mark, expectClear, one)
}

// LogosAllGsSnapshotSortedForGCColored runs allGsSnapshotSortedForGC under STW
// with allgs temporarily swapped to a single colored *g entry derived from h.
func LogosAllGsSnapshotSortedForGCColored(h *LogosZgcForeignG) ([]unsafe.Pointer, int) {
	one := []*g{h.gp}
	*(*uintptr)(unsafe.Pointer(&one[0])) = LogosZgcColored(uintptr(unsafe.Pointer(h.gp)), LogosZgcRemapped)
	return logosAllGsSnapshotSortedForGCTestList(one)
}

// LogosAllGsSnapshotSortedForGCColoredHeader runs allGsSnapshotSortedForGC under STW
// with allgs temporarily swapped to a slice whose backing array pointer itself is colored
// (Level 1), while the element entry remains canonical.
func LogosAllGsSnapshotSortedForGCColoredHeader(h *LogosZgcForeignG) ([]unsafe.Pointer, int) {
	one := []*g{h.gp}
	return logosAllGsSnapshotSortedForGCColoredHeaderTestList(one)
}

// LogosAllGsSnapshotSortedForGC runs allGsSnapshotSortedForGC under STW
// with allgs temporarily swapped to a single uncolored *g entry derived from h.
func LogosAllGsSnapshotSortedForGC(h *LogosZgcForeignG) ([]unsafe.Pointer, int) {
	one := []*g{h.gp}
	return logosAllGsSnapshotSortedForGCTestList(one)
}

// LogosGCMarkrootStackG invokes logosGCMarkrootStackG over an uncolored heap-backed
// slice containing h.gp, returning the canonical g as unsafe.Pointer.
func LogosGCMarkrootStackG(h *LogosZgcForeignG) unsafe.Pointer {
	raw := new([1]*g)
	raw[0] = h.gp
	one := abi.Escape(raw)[:]
	if spanOfHeap(uintptr(unsafe.Pointer(&one[0]))) == nil {
		throw("LogosGCMarkrootStackG: backing array not on heap")
	}
	gp := logosGCMarkrootStackG(one, 0)
	return unsafe.Pointer(gp)
}

// LogosGCMarkrootStackGColoredHeader invokes logosGCMarkrootStackG over a heap-backed
// slice where the header array pointer is colored (Level 1) and the entry word is colored (Level 2),
// verifying that both levels are canonicalized without mutating the caller's backing entry.
func LogosGCMarkrootStackGColoredHeader(h *LogosZgcForeignG) (gpCanon unsafe.Pointer, backingEntryStillColored bool) {
	raw := new([1]*g)
	raw[0] = h.gp
	one := abi.Escape(raw)[:]
	if spanOfHeap(uintptr(unsafe.Pointer(&one[0]))) == nil {
		throw("LogosGCMarkrootStackGColoredHeader: backing array not on heap")
	}
	coloredWord := LogosZgcColored(uintptr(unsafe.Pointer(h.gp)), LogosZgcRemapped)
	*(*uintptr)(unsafe.Pointer(&one[0])) = coloredWord

	hdr := (*slice)(unsafe.Pointer(&one))
	*(*uintptr)(unsafe.Pointer(&hdr.array)) = LogosZgcColored(uintptr(hdr.array), LogosZgcRemapped)

	gp := logosGCMarkrootStackG(one, 0)
	stillColored := *(*uintptr)(unsafe.Pointer(&one[0])) == coloredWord
	return unsafe.Pointer(gp), stillColored
}

func logosAllGsSnapshotSortedForGCTestList(gs []*g) ([]unsafe.Pointer, int) {
	stw := stopTheWorld(stwForTestForeignRetain)
	lock(&allglock)
	savedGs := allgs
	allgs = gs
	atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&gs[0]))
	atomic.Storeuintptr(&allglen, uintptr(len(gs)))
	unlock(&allglock)
	defer func() {
		lock(&allglock)
		allgs = savedGs
		if len(savedGs) > 0 && &savedGs[0] != allgptr {
			atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&savedGs[0]))
		}
		atomic.Storeuintptr(&allglen, uintptr(len(savedGs)))
		unlock(&allglock)
		startTheWorld(stw)
	}()
	sorted, cutoff := allGsSnapshotSortedForGC()
	out := make([]unsafe.Pointer, len(sorted))
	for i, gp := range sorted {
		out[i] = unsafe.Pointer(gp)
	}
	return out, cutoff
}

func logosAllGsSnapshotSortedForGCColoredHeaderTestList(gs []*g) ([]unsafe.Pointer, int) {
	stw := stopTheWorld(stwForTestForeignRetain)
	lock(&allglock)
	savedGs := allgs
	allgs = gs
	atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&gs[0]))
	atomic.Storeuintptr(&allglen, uintptr(len(gs)))
	if len(allgs) > 0 {
		hdr := (*slice)(unsafe.Pointer(&allgs))
		*(*uintptr)(unsafe.Pointer(&hdr.array)) = LogosZgcColored(uintptr(hdr.array), LogosZgcRemapped)
	}
	unlock(&allglock)
	defer func() {
		lock(&allglock)
		allgs = savedGs
		if len(savedGs) > 0 && &savedGs[0] != allgptr {
			atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&savedGs[0]))
		}
		atomic.Storeuintptr(&allglen, uintptr(len(savedGs)))
		unlock(&allglock)
		startTheWorld(stw)
	}()
	sorted, cutoff := allGsSnapshotSortedForGC()
	out := make([]unsafe.Pointer, len(sorted))
	for i, gp := range sorted {
		out[i] = unsafe.Pointer(gp)
	}
	return out, cutoff
}

func logosZgcForeignRetainList(live []*LogosZgcPage, h *LogosZgcForeignG, mark, expectClear []uintptr, gs []*g) int {
	zgcInitForeignRoots()
	gp0 := &gs[0] // Bounds check before the world stops; every caller swaps in one element.
	stw := stopTheWorld(stwForTestForeignRetain)
	lock(&allglock)
	savedGs := allgs
	allgs = gs
	atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(gp0))
	atomic.Storeuintptr(&allglen, uintptr(len(gs)))
	unlock(&allglock)
	// One owner, one start: the manual restore calls below stay (a bare
	// deferred restore once ran after a manual one and started the world
	// twice), and the deferred call below is a no-op once a manual restore
	// ran. A panic between stop and manual restore still restarts the
	// world instead of leaving the next stop nested inside this one.
	stopped := true
	restore := func() {
		if !stopped {
			return
		}
		stopped = false
		lock(&allglock)
		allgs = savedGs
		if len(savedGs) > 0 && &savedGs[0] != allgptr {
			atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&savedGs[0]))
		}
		atomic.Storeuintptr(&allglen, uintptr(len(savedGs)))
		unlock(&allglock)
		startTheWorld(stw)
	}
	defer restore()
	for _, addr := range mark {
		s := spanOfHeap(addr)
		if s == nil {
			restore()
			return -1
		}
		idx := s.objIndex(addr)
		if idx >= uintptr(s.nelems) {
			restore()
			return -1
		}
		s.markBitsForIndex(idx).setMarked()
	}
	for _, addr := range expectClear {
		s := spanOfHeap(addr)
		if s == nil {
			restore()
			return -1
		}
		idx := s.objIndex(addr)
		if idx >= uintptr(s.nelems) {
			restore()
			return -1
		}
		if s.markBitsForIndex(idx).isMarked() {
			restore()
			return -2
		}
	}
	n := zgcForeignRetainLive(live)
	restore()
	return n
}

func logosZgcForeignRetainColoredHeaderList(live []*LogosZgcPage, h *LogosZgcForeignG, mark, expectClear []uintptr, gs []*g) int {
	zgcInitForeignRoots()
	gp0 := &gs[0] // Bounds check before the world stops; every caller swaps in one element.
	stw := stopTheWorld(stwForTestForeignRetain)
	lock(&allglock)
	savedGs := allgs
	allgs = gs
	atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(gp0))
	atomic.Storeuintptr(&allglen, uintptr(len(gs)))
	if len(allgs) > 0 {
		hdr := (*slice)(unsafe.Pointer(&allgs))
		*(*uintptr)(unsafe.Pointer(&hdr.array)) = LogosZgcColored(uintptr(hdr.array), LogosZgcRemapped)
	}
	unlock(&allglock)
	// One owner, one start: the manual restore calls below stay (a bare
	// deferred restore once ran after a manual one and started the world
	// twice), and the deferred call below is a no-op once a manual restore
	// ran. A panic between stop and manual restore still restarts the
	// world instead of leaving the next stop nested inside this one.
	stopped := true
	restore := func() {
		if !stopped {
			return
		}
		stopped = false
		lock(&allglock)
		allgs = savedGs
		if len(savedGs) > 0 && &savedGs[0] != allgptr {
			atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&savedGs[0]))
		}
		atomic.Storeuintptr(&allglen, uintptr(len(savedGs)))
		unlock(&allglock)
		startTheWorld(stw)
	}
	defer restore()
	for _, addr := range mark {
		s := spanOfHeap(addr)
		if s == nil {
			restore()
			return -1
		}
		idx := s.objIndex(addr)
		if idx >= uintptr(s.nelems) {
			restore()
			return -1
		}
		s.markBitsForIndex(idx).setMarked()
	}
	for _, addr := range expectClear {
		s := spanOfHeap(addr)
		if s == nil {
			restore()
			return -1
		}
		idx := s.objIndex(addr)
		if idx >= uintptr(s.nelems) {
			restore()
			return -1
		}
		if s.markBitsForIndex(idx).isMarked() {
			restore()
			return -2
		}
	}
	n := zgcForeignRetainLive(live)
	restore()
	return n
}

// Exact source-range release seam (logos_zgc_release_ranges_test.go).
//
// Everything under test is the runtime's own: zgcNewPage (LogosZgcNewPage),
// the page state selection fills and zgcPageEligible with its
// prepareSources (LogosZgcRecordSources, LogosZgcEligible),
// zgcRelocateObject (LogosZgcRelocate), zgcReleasePage and its madvise
// (LogosZgcRelease) and zgcRetirePage (LogosZgcRetire). The one thing the
// tests cannot reach from package runtime_test is an *mspan, so
// LogosZgcAllocRun hands out real spans from the real page allocator:
// in-use heap spans from allocSpan(spanAllocHeap) and manual spans from
// allocManual(spanAllocStack), each with the state the allocator gave it.
// No span field, pin, goroutine status or syscall result is written here.
//
// Observation limit: LogosZgcPageSources reads the private prepared range
// table, and LogosZgcGappyPages reads the private page registry; both are
// read-only. The public effect of the same behaviour (neighbour memory
// survives, stacks keep their frames) is asserted separately through the
// live heap in TestLogosZgcLiveHeapGappyReleaseKeepsNeighbours.

const (
	// LogosZgcRunHeap is an in-use heap span of 1024-byte noscan objects.
	LogosZgcRunHeap = 0
	// LogosZgcRunManual is a manual span, as a goroutine stack occupies.
	LogosZgcRunManual = 1
	// LogosZgcRunHeapScan is an in-use heap span of 1024-byte scanned
	// objects: pointer-bearing, above MinSizeForMallocHeader, the exact
	// span class the relocation walker admits and the malloc-header path
	// serves.
	LogosZgcRunHeapScan = 2
	// LogosZgcRunHeapScanSmall is an in-use heap span of a small scanned
	// class (64-byte objects): the pointer bitmap lives in the span heap
	// bits and adjacent slots share one bitmap word.
	LogosZgcRunHeapScanSmall = 3
)

// LogosZgcSpanRun is a run of adjacent spans in one 2 MB region, in the
// address order of the kinds that asked for them. The handle keeps the
// spans held until Free.
type LogosZgcSpanRun struct {
	spans  []*mspan
	kinds  []int
	freed  []bool
	base   []uintptr
	bytes  []uintptr
	elem   []uintptr
	nelems []uintptr
}

func logosZgcRun1024Class(noscan bool) spanClass {
	for c, sz := range gc.SizeClassToSize {
		if sz == 1024 {
			return makeSpanClass(uint8(c), noscan)
		}
	}
	throw("zgc test: no 1024-byte size class")
	return 0
}

// logosZgcRunClass is the fixture's original 1024-byte noscan heap class.
func logosZgcRunClass() spanClass { return logosZgcRun1024Class(true) }

// logosZgcRunClassScan is the 1024-byte scanned heap class.
func logosZgcRunClassScan() spanClass { return logosZgcRun1024Class(false) }

// logosZgcRunClassScanSmall is a small scanned heap class (64-byte
// objects): the pointer bitmap lives in the span heap bits and adjacent
// slots share one bitmap word.
func logosZgcRunClassScanSmall() spanClass {
	for c, sz := range gc.SizeClassToSize {
		if sz == 64 {
			return makeSpanClass(uint8(c), false)
		}
	}
	throw("zgc test: no 64-byte size class")
	return 0
}

// zgcTestRunTight reports whether spans are all present, end to end, and
// inside one registrable 2 MB region.
func zgcTestRunTight(spans []*mspan) bool {
	for _, s := range spans {
		if s == nil {
			return false
		}
	}
	region := spans[0].base() &^ (zgcPageSize - 1)
	if region > zgcAddrMask-(zgcPageSize-1) {
		return false
	}
	for i, s := range spans {
		end := s.base() + s.npages*pageSize
		if s.base() < region || end > region+zgcPageSize {
			return false
		}
		if i > 0 && s.base() != spans[i-1].base()+spans[i-1].npages*pageSize {
			return false
		}
	}
	return true
}

// zgcTestFreeSpan returns s to the heap. It runs on the system stack.
func zgcTestFreeSpan(s *mspan, kind int) {
	if kind == LogosZgcRunManual {
		mheap_.freeManual(s, spanAllocStack)
		return
	}
	atomic.Store(&s.sweepgen, mheap_.sweepgen)
	mheap_.freeSpan(s)
}

// LogosZgcAllocRun allocates one real span for each kind, each as large
// as a system page needs, and returns them only when they came out end to
// end inside a single 2 MB region. The page allocator hands out the
// lowest free pages first, so a trial that lands across a hole or a
// region edge is held (so the next trial cannot reuse it) and retried;
// held trials are freed before returning. It returns nil when no trial
// was tight. The caller keeps GC off while the run is held.
func LogosZgcAllocRun(kinds []int) *LogosZgcSpanRun {
	zgcInitPageRegistry()
	const trials = 256
	unitPages := max(pageSize, physPageSize) / pageSize
	n := len(kinds)
	run := &LogosZgcSpanRun{
		spans:  make([]*mspan, n),
		freed:  make([]bool, n),
		base:   make([]uintptr, n),
		bytes:  make([]uintptr, n),
		elem:   make([]uintptr, n),
		nelems: make([]uintptr, n),
	}
	// kinds is copied: slices is an import cycle in package runtime.
	run.kinds = make([]int, n)
	copy(run.kinds, kinds)
	cur := make([]*mspan, n)
	junk := make([]*mspan, 0, trials*n)
	junkKind := make([]int, 0, trials*n)
	found := false
	for range trials {
		systemstack(func() {
			for i, k := range kinds {
				if k == LogosZgcRunManual {
					cur[i] = mheap_.allocManual(unitPages, spanAllocStack)
				} else {
					class := logosZgcRunClass()
					if k == LogosZgcRunHeapScan {
						class = logosZgcRunClassScan()
					} else if k == LogosZgcRunHeapScanSmall {
						class = logosZgcRunClassScanSmall()
					}
					cur[i] = mheap_.allocSpan(unitPages, spanAllocHeap, class)
				}
			}
		})
		if zgcTestRunTight(cur) {
			copy(run.spans, cur)
			found = true
			break
		}
		for i, s := range cur {
			if s != nil {
				junk = append(junk, s)
				junkKind = append(junkKind, kinds[i])
			}
		}
	}
	systemstack(func() {
		for i, s := range junk {
			zgcTestFreeSpan(s, junkKind[i])
		}
	})
	if !found {
		return nil
	}
	for i, s := range run.spans {
		run.base[i] = s.base()
		run.bytes[i] = s.npages * pageSize
		if k := run.kinds[i]; k == LogosZgcRunHeap || k == LogosZgcRunHeapScan || k == LogosZgcRunHeapScanSmall {
			run.elem[i] = s.elemsize
			run.nelems[i] = uintptr(s.nelems)
		}
	}
	return run
}

// logosZgcGranuleRun wraps spans of the fixture's heap class as a run.
// Every span carries LogosZgcRunHeap, so Occupy, Free and MarkFreed treat
// them exactly like the spans LogosZgcAllocRun handed out.
func logosZgcGranuleRun(spans []*mspan) *LogosZgcSpanRun {
	run := &LogosZgcSpanRun{
		spans:  spans,
		freed:  make([]bool, len(spans)),
		base:   make([]uintptr, len(spans)),
		bytes:  make([]uintptr, len(spans)),
		elem:   make([]uintptr, len(spans)),
		nelems: make([]uintptr, len(spans)),
	}
	run.kinds = make([]int, len(spans))
	for i, s := range spans {
		run.kinds[i] = LogosZgcRunHeap
		run.base[i] = s.base()
		run.bytes[i] = s.npages * pageSize
		run.elem[i] = s.elemsize
		run.nelems[i] = uintptr(s.nelems)
	}
	return run
}

// LogosZgcAllocGranule reserves one contiguous whole-granule heap span on
// a directory granule no page owns, by construction rather than by trial
// luck. LogosZgcAllocRun's unit-span trials cannot do this
// deterministically: the page cache hands out the lowest free bits first,
// so a trial's first unit span lands at the frontier's granule-relative
// page k and every later held trial repeats that offset — a misaligned
// frontier stays misaligned across the whole trial budget. This seam
// instead issues one allocSpan of exactly one granule's pages in the
// fixture's 1024-byte class: the request bypasses the page cache into
// the radix, and the span metadata is what allocSpan itself derives —
// elemsize 1024, nelems 2048, inline heap bits — never manufactured. A
// granule-aligned base is accepted when the directory slot is free;
// tight runs on occupied granules are held, not freed — freeing would
// lower searchAddr back onto the same granule — so the next trial walks
// to the following granule with the frontier still aligned. A misaligned
// base is freed and exactly the pages up to the next boundary are held
// in its place, so the next request starts on a boundary; the calling
// P's page cache is flushed first so neither step competes with
// cache-hidden bits. Held spans are released before returning, on
// success and on nil. It returns nil when eight granules were considered
// without an acceptable one or the realignment budget ran out; the span
// operations stay inside the old 8×256-trial envelope. The caller keeps
// GC off while the run is held, as with LogosZgcAllocRun.
func LogosZgcAllocGranule() *LogosZgcSpanRun {
	zgcInitPageRegistry()
	const granuleTrials = 8
	granulePages := uintptr(zgcPageSize / pageSize)
	class := logosZgcRunClass()

	var junk []*mspan
	var rejected []*LogosZgcSpanRun
	freeHeld := func() {
		systemstack(func() {
			for _, s := range junk {
				zgcTestFreeSpan(s, LogosZgcRunHeap)
			}
			for _, r := range rejected {
				for i := range r.spans {
					zgcTestFreeSpan(r.spans[i], LogosZgcRunHeap)
				}
			}
		})
	}
	// flushPcache returns the calling P's cached page bits to the radix,
	// so whole-granule requests and boundary advances see every free
	// page the allocator knows about.
	flushPcache := func() {
		systemstack(func() {
			lock(&mheap_.lock)
			if pp := getg().m.p.ptr(); pp != nil {
				pp.pcache.flush(&mheap_.pages)
			}
			unlock(&mheap_.lock)
		})
	}
	for trial, attempts := 0, 0; trial < granuleTrials && attempts < 4*granuleTrials; attempts++ {
		flushPcache()
		var cand *mspan
		systemstack(func() {
			cand = mheap_.allocSpan(granulePages, spanAllocHeap, class)
		})
		if cand == nil {
			break
		}
		base := cand.base()
		if base&^(zgcPageSize-1) == base && base <= zgcAddrMask-(zgcPageSize-1) {
			if !zgcPageSlotOccupied(base) {
				freeHeld()
				return logosZgcGranuleRun([]*mspan{cand})
			}
			// Held, not freed: freeing would lower searchAddr onto the
			// same granule and the next trial would consider it again.
			rejected = append(rejected, logosZgcGranuleRun([]*mspan{cand}))
			trial++
			continue
		}
		// Misaligned: free the candidate and hold exactly the pages up
		// to the next boundary, so the next request starts on one.
		advance := granulePages - base/pageSize%granulePages
		var hold *mspan
		systemstack(func() {
			zgcTestFreeSpan(cand, LogosZgcRunHeap)
			hold = mheap_.allocSpan(advance, spanAllocHeap, class)
		})
		if hold == nil {
			break
		}
		junk = append(junk, hold)
	}
	freeHeld()
	return nil
}

// Len returns the number of spans in the run.
func (r *LogosZgcSpanRun) Len() int { return len(r.spans) }

// Base returns the first address of span i.
func (r *LogosZgcSpanRun) Base(i int) uintptr { return r.base[i] }

// Bytes returns the size of span i.
func (r *LogosZgcSpanRun) Bytes(i int) uintptr { return r.bytes[i] }

// Elem returns the object size of heap span i.
func (r *LogosZgcSpanRun) Elem(i int) uintptr { return r.elem[i] }

// Nelems returns the object count of heap span i.
func (r *LogosZgcSpanRun) Nelems(i int) uintptr { return r.nelems[i] }

// Occupy hands out slot k of heap span i as a real allocation, with the
// allocator's own hand-out accounting (the alloc bit, allocCount, and the
// freeindex and alloc-cache advance) and nothing else: the pointer bitmap
// stays the caller's to write. A fixture object built by plain stores
// skips mallocgc, so its span does not count it; without the hand-out the
// claim path's "a published copy leaves the source count" decrement
// (zgcCopyOneResult) moves an uncounted span below zero, and the run's
// later free throws on a wrapped allocCount.
func (r *LogosZgcSpanRun) Occupy(i int, k uintptr) {
	s := r.spans[i]
	if r.kinds[i] == LogosZgcRunManual {
		throw("zgc test: occupy of a manual span")
	}
	if k >= uintptr(s.nelems) {
		throw("zgc test: occupy past the span end")
	}
	if s.allocBitsForIndex(k).isMarked() {
		throw("zgc test: occupy of an allocated slot")
	}
	s.allocBitsForIndex(k).setMarkedNonAtomic()
	s.allocCount++
	if k >= uintptr(s.freeindex) {
		s.freeindex = uint16(k) + 1
		if uintptr(s.freeindex) < uintptr(s.nelems) {
			s.refillAllocCache(s.freeindex / 8)
			s.allocCache >>= uint(s.freeindex) % 64
		}
	}
}

// OccupyFresh hands out slot k of heap span i through the GC-off malloc
// fast path (nextFreeFast): freeindex past the slot, the alloc-cache
// refill and shift, and allocCount, with the slot's alloc bit left
// clear — the fresh hand-out state before the next sweep swaps the mark
// bits into allocBits. The slot must be the span's next free slot, the
// way the fast path finds it; the heap-bit pointer map stays the
// caller's to write, as with Occupy.
func (r *LogosZgcSpanRun) OccupyFresh(i int, k uintptr) {
	s := r.spans[i]
	if r.kinds[i] == LogosZgcRunManual {
		throw("zgc test: fresh occupy of a manual span")
	}
	if k >= uintptr(s.nelems) {
		throw("zgc test: occupy past the span end")
	}
	if k != uintptr(s.freeindex) {
		throw("zgc test: fresh occupy off the fast-path slot")
	}
	if s.allocBitsForIndex(k).isMarked() {
		throw("zgc test: occupy of an allocated slot")
	}
	s.refillAllocCache(uint16(k) / 8)
	s.allocCache >>= uint(s.freeindex) % 64
	if v := nextFreeFast(s); v == 0 || uintptr(v) != s.base()+k*s.elemsize {
		throw("zgc test: fresh occupy did not hand out the slot")
	}
}

// MarkFreed records that span i was freed by the runtime itself (a
// retired page frees its source spans), so Free skips it.
func (r *LogosZgcSpanRun) MarkFreed(i int) { r.freed[i] = true }

// Free returns span i to the heap unless it is already freed.
func (r *LogosZgcSpanRun) Free(i int) {
	if r.freed[i] {
		return
	}
	r.freed[i] = true
	s, k := r.spans[i], r.kinds[i]
	systemstack(func() { zgcTestFreeSpan(s, k) })
}

// LogosZgcRecordSources records the spans of r at idx as the source spans
// of pg, in the order given, the way zgcSelectPages does for a page it
// selects: into the page's state list. The list is in no address order
// (selection walks allspans), and prepareSources sorts it.
func LogosZgcRecordSources(pg *LogosZgcPage, r *LogosZgcSpanRun, idx []int) {
	zgcInitPageRegistry()
	st := zgcPageState(pg)
	st.spans = st.spans[:0]
	for _, i := range idx {
		st.spans = append(st.spans, r.spans[i])
	}
}

// LogosZgcEligible runs zgcPageEligible: the selection pause's vet, which
// prepares the exact source ranges.
func LogosZgcEligible(pg *LogosZgcPage) bool { return zgcPageEligible(pg) }

// LogosZgcWithWorldStopped runs fn with the world stopped, mirroring the
// mark-termination/GC-start pauses that own zgcPageEligible (selection vet)
// and zgcRetirePage (retire). Under goexperiment.staticlockranking those
// seams assert the world is stopped; calling them with the world running
// throws even though the same bytes pass without the tag. Relocation
// (zgcRelocateObject) and release (zgcReleasePage) stay outside: concurrent
// evacuators copy with the world running, and the release seam takes no
// STW assertion, so wrapping copies would hide races the tests must keep.
//
// fn runs on the caller's goroutine stack, not on g0: the race build
// leaves g0 without a race context (mp.g0.racectx = 0), so an instrumented
// test function entering on the system stack faults in __tsan_func_enter.
// Every seam callback drives runtime paths that wrap their own systemstack
// sections where the heap lock demands g0 — zgcRetirePage,
// zgcAbandonDrain, mheap.alloc — so nothing here may call a bare
// //go:systemstack function, and fn must not allocate unboundedly: stack
// growth and runtime locks stay legal while the world is stopped, the
// leaf sections stay on g0 exactly like the pauses mirrored.
func LogosZgcWithWorldStopped(fn func()) {
	stw := stopTheWorld(stwUnknown)
	// Exactly one start on every path: a panic in fn still restarts the
	// world instead of leaving the next stop nested inside this one.
	defer startTheWorld(stw)
	fn()
}

// LogosZgcWalkAllLiveFix runs the GC-start rewrite over the real heap
// graph: the exact logosWalkAllWith call of zgcGCStartHook
// (logos_zgc_cycle.go), with the quarantine skip and post-sweep
// alloc-bit liveness. Slots are rewritten only when they name an object
// with a published copy. The world must be stopped, as in the pause
// that owns the call.
func LogosZgcWalkAllLiveFix() {
	logosWalkAllWith(zgcFixSlot, logosWalkPolicy{skipSpan: zgcSpanQuarantined, useAllocBits: true})
}

// LogosZgcPageSources returns the prepared source ranges of pg as sorted
// [lo, hi) pairs, and whether prepareSources produced them (a registered
// page that was never prepared reports its whole region).
func LogosZgcPageSources(pg *LogosZgcPage) (ranges [][2]uintptr, exact bool) {
	for i := uintptr(0); i < pg.srcN; i++ {
		r := pg.sourceRangeAt(i)
		ranges = append(ranges, [2]uintptr{r.lo, r.hi})
	}
	return ranges, pg.srcExact
}

// LogosZgcNewDetachedPageAt creates an unregistered header whose region is
// [start, start+size), for prepareSources vetting that must not claim the
// directory slot.
func LogosZgcNewDetachedPageAt(start, size uintptr) *LogosZgcPage {
	return zgcAllocPage(start, size, 0, 0)
}

// LogosZgcForgetPage drops the span lists selection recorded for pg.
func LogosZgcForgetPage(pg *LogosZgcPage) { zgcPageForget(pg) }

// LogosZgcFirstMismatch returns the offset of the first byte of
// [addr, addr+n) that is not b, or -1.
func LogosZgcFirstMismatch(addr, n uintptr, b byte) int {
	for i, c := range unsafe.Slice((*byte)(add(nil, addr)), n) {
		if c != b {
			return i
		}
	}
	return -1
}

// LogosZgcByteAt reads the byte at addr.
func LogosZgcByteAt(addr uintptr) byte { return *(*byte)(add(nil, addr)) }

// LogosZgcGappyPages counts the registered pages whose prepared source
// ranges do not cover their whole region.
func LogosZgcGappyPages() int {
	n := 0
	zgcPagesLock()
	for _, pg := range zgcAllPages {
		if !pg.srcExact {
			continue
		}
		var covered uintptr
		for i := uintptr(0); i < pg.srcN; i++ {
			r := pg.sourceRangeAt(i)
			covered += r.hi - r.lo
		}
		if covered < pg.size {
			n++
		}
	}
	zgcPagesUnlock()
	return n
}

// LogosZgcPageAcquire resolves addr to its relocation source page and
// returns a leased handle: nil when addr is not a source of any
// registered page. The caller owes exactly one LogosZgcPageRelease per
// non-nil result, on every path.
func LogosZgcPageAcquire(addr uintptr) *LogosZgcPage { return zgcPageAcquire(addr) }

// LogosZgcPageRelease drops one lease gained by LogosZgcPageAcquire.
func LogosZgcPageRelease(pg *LogosZgcPage) { zgcPageRelease(pg) }

// LogosZgcTransientEnter opens a transient read domain: every raw
// pointer derived inside it stays valid until the matching
// LogosZgcTransientLeave. Test-side stand-in for a signal-stack or no-P
// reader, including one held across a stop-the-world window.
func LogosZgcTransientEnter() { zgcPageTransientEnter() }

// LogosZgcTransientLeave closes one open transient read domain.
func LogosZgcTransientLeave() { zgcPageTransientLeave() }

// LogosZgcPublishToSpaceSpans publishes pg's filled to-space spans onto
// the swept span sets. Stop-the-world caller: it asserts a stopped
// world. A leased page is skipped whole, its destination list retained
// for a later pause.
func LogosZgcPublishToSpaceSpans(pg *LogosZgcPage) { zgcPublishToSpaceSpans(pg) }

// LogosZgcReserveToSpace pre-reserves one to-space span per source span
// of pg, the selection-pause step. Stop-the-world caller.
func LogosZgcReserveToSpace(pg *LogosZgcPage) bool { return zgcReservePageToSpace(pg) }

// LogosZgcQuarantineSource quarantines the i-th source span of pg exactly
// as the selection pause does: it drops the marks, takes the pre-bump
// sweepgen stamp and enters the quarantine set the walker, sweeper and
// scan admission all consult. Stop-the-world caller.
func LogosZgcQuarantineSource(pg *LogosZgcPage, r *LogosZgcSpanRun, i int) {
	zgcQuarantineSpan(r.spans[i], pg)
}

// LogosZgcPageStateToSpaceLen returns how many to-space spans pg's
// state lists: the deferred destination list a lease retains and
// publication drains.
func LogosZgcPageStateToSpaceLen(pg *LogosZgcPage) int {
	st := zgcPageStateIfAny(pg)
	if st == nil {
		return 0
	}
	return len(st.toSpace)
}

// LogosZgcPageStateCursor returns pg's claim cursor.
func LogosZgcPageStateCursor(pg *LogosZgcPage) uintptr {
	st := zgcPageStateIfAny(pg)
	if st == nil {
		return 0
	}
	return st.cursor
}

// LogosZgcToSpaceBase returns the base of the i-th reserved to-space
// span of pg, or 0 when pg lists no such span. It is the exact
// destination address publication scrubs and pushes; a source span
// base never proves the transfer.
func LogosZgcToSpaceBase(pg *LogosZgcPage, i int) uintptr {
	st := zgcPageStateIfAny(pg)
	if st == nil || i < 0 || i >= len(st.toSpace) || st.toSpace[i] == nil {
		return 0
	}
	return st.toSpace[i].base()
}

// LogosZgcSpanSwept reports whether the span at addr carries the current
// sweepgen, the mark publication leaves on a pushed to-space span.
func LogosZgcSpanSwept(addr uintptr) bool {
	s := spanOfHeap(addr)
	return s != nil && atomic.Load(&s.sweepgen) == mheap_.sweepgen
}

// LogosZgcRetirePendingLen returns how many descriptors — retired pages
// and deferred headers — wait in the retire queue.
func LogosZgcRetirePendingLen() int {
	lock(&zgcQuarantineLock)
	n := len(zgcRetirePending)
	unlock(&zgcQuarantineLock)
	return n
}

// LogosZgcRetirePendingRegionCount returns how many queued descriptors
// name the directory granule holding addr. It observes the real pending
// queue, rather than a test counter, to catch duplicate descriptors for a
// region while a transient acquisition domain keeps reclamation open.
func LogosZgcRetirePendingRegionCount(addr uintptr) int {
	base := addr &^ (zgcPageSize - 1)
	lock(&zgcQuarantineLock)
	n := 0
	for _, pg := range zgcRetirePending {
		if pg.start == base {
			n++
		}
	}
	unlock(&zgcQuarantineLock)
	return n
}

// LogosZgcClaimGranule is a test seam for the authoritative directory
// claim used by selection. It intentionally delegates directly to
// zgcClaimPage: a held registered or retiring slot returns nil, and the
// detached candidate allocated by that failed claim is freed by the
// production failure path. A successful result is a freshly registered
// page the caller must abandon or retire. The caller must hold the world
// stopped.
func LogosZgcClaimGranule(base, live uintptr, nObjects int) *LogosZgcPage {
	pg, ok := zgcClaimPage(base, zgcPageSize, live, uintptr(nObjects))
	if !ok {
		return nil
	}
	return pg
}

// LogosZgcDrainRetirePending runs the pending-queue consumer once.
// Stop-the-world caller: it asserts a stopped world.
func LogosZgcDrainRetirePending() { zgcRetirePendingDrain() }
