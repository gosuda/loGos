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

// LogosZgcNewPage creates a registered page header with a forwarding
// table sized for nObjects.
func LogosZgcNewPage(start, live uintptr, nObjects int) *LogosZgcPage {
	return zgcNewPage(start, zgcPageSize, live, uintptr(nObjects))
}

// LogosZgcNewDetachedPage creates an unregistered header of arbitrary
// size for selection tests.
func LogosZgcNewDetachedPage(size, live uintptr) *LogosZgcPage {
	return zgcNewDetachedPage(size, live)
}

// LogosZgcFreePage unregisters and frees pg.
func LogosZgcFreePage(pg *LogosZgcPage) { zgcFreePage(pg) }

// LogosZgcPageEvacuable reports whether pg is over the evacuation
// threshold.
func LogosZgcPageEvacuable(pg *LogosZgcPage) bool { return pg.evacuable() }

// LogosZgcPageFor returns the registered page containing addr.
func LogosZgcPageFor(addr uintptr) *LogosZgcPage { return zgcPageFor(addr) }

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

// LogosZgcDropPage frees a page a test registered over memory it still
// owns, whatever it published. The collector never does this: a page
// with published relocations leaves only through zgcRetirePage.
func LogosZgcDropPage(pg *LogosZgcPage) {
	pg.published.Store(0)
	zgcFreePage(pg)
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

// LogosZgcForeignRetain runs the transitive closure over live with h as the
// only foreign thread and returns the retained-prefix length. Negative values
// report an invalid fixture (-1) or a marked expectClear object (-2).
func LogosZgcForeignRetain(live []*LogosZgcPage, h *LogosZgcForeignG, mark, expectClear []uintptr) int {
	one := []*g{h.gp} // world running; no STW allocation below
	zgcInitForeignRoots()
	stw := stopTheWorld(stwForTestForeignRetain)
	lock(&allglock)
	savedGs := allgs
	allgs = one
	atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&one[0]))
	atomic.Storeuintptr(&allglen, 1)
	unlock(&allglock)
	restore := func() {
		lock(&allglock)
		allgs = savedGs
		if len(savedGs) > 0 && &savedGs[0] != allgptr {
			atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&savedGs[0]))
		}
		atomic.Storeuintptr(&allglen, uintptr(len(savedGs)))
		unlock(&allglock)
		startTheWorld(stw)
	}
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

func logosZgcRunClass() spanClass {
	for c, sz := range gc.SizeClassToSize {
		if sz == 1024 {
			return makeSpanClass(uint8(c), true)
		}
	}
	throw("zgc test: no 1024-byte size class")
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
	class := logosZgcRunClass()
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
		if run.kinds[i] == LogosZgcRunHeap {
			run.elem[i] = s.elemsize
			run.nelems[i] = uintptr(s.nelems)
		}
	}
	return run
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
