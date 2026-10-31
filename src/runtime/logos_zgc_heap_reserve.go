// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import "unsafe"

// ZGC low-window heap reservation.
//
// Every ordinary Go heap span must lie completely below 1<<42 when the
// linker selected -X runtime.logosGCMode=zgc on a 64-bit moving build.
// The 42-bit canonical field (logos_zgc.go: zgcAddrMask) is fixed; high
// addresses are genuine address bits, not color, and the barrier drops
// them (zgcLoadBarrierSlow returns them unchanged). Placement, not a
// wider mask, keeps the heap manageable.
//
// Funnel: (h *mheap) sysAlloc in malloc.go is the single 64-bit heap
// reservation point (pre-reserve, hint loop, nil-hint fallback). mallocinit
// builds the hint list before any heap use (including goargs/goenvs
// allocations before gcinit), so pruning there covers startup. Growth,
// large, and small objects all flow through sysAlloc. Stacks, persistent
// memory, user arenas, and arena-index metadata stay outside the color
// field by construction and are never gated here.
//
// Locking: sysAlloc holds h.lock. Helpers below allocate no Go heap,
// perform no write barriers, and acquire no locks. The scan path runs
// only after retained hints are exhausted, i.e. on the terminal
// low-exhaustion trajectory, so steady-state lock behavior is unchanged
// (low hints hit first, zero probes).
//
// Exhaustion is defined operationally and finitely: every
// heapArenaBytes-aligned slot in (0, 1<<42) was probed and none honored
// the exact address. Only then does sysAlloc throw. No arbitrary attempt
// cap redefines exhaustion while lawful low space remains.

// logosZgcHeapEnabled reports whether low-window placement applies:
// exact zgc mode on a moving 64-bit build. All other modes (including
// greentea/immix under the same experiment) take the original path
// byte-for-byte.
//
//go:nosplit
func logosZgcHeapEnabled() bool {
	return logosGCMode == "zgc"
}

// logosZgcHeapInRange reports whether [v, v+n) lies completely inside
// the color-representable window. Whole-interval, wrap-safe; n may span
// multiple arenas.
//
//go:nosplit
func logosZgcHeapInRange(v uintptr, n uintptr) bool {
	if v == 0 || n == 0 {
		return false
	}
	end := v + n
	if end < v {
		return false
	}
	if end-1 > uintptr(zgcAddrMask) {
		return false
	}
	return true
}

// logosZgcIndexRange records one published arena index array.
type logosZgcIndexRange struct {
	base, size uintptr
}

// logosZgcIndexRanges records, per L1 entry, the address ranges of the
// heap arena index arrays (mheap_.arenas L2) published by sysAlloc. The
// index arrays are kernel-choice mappings (sysAllocOS) that heap
// bookkeeping does not otherwise track, so the exact-hit search below
// must consult this record explicitly: kernel exactness alone does not
// consult runtime ownership. Set under h.lock when an L2 array is
// allocated; never cleared (arena maps are never freed).
var logosZgcIndexRanges [1 << arenaL1Bits]logosZgcIndexRange

// logosZgcRecordIndex publishes the arena index array range for the
// search. Called once from sysAlloc under h.lock when the L2 array is
// allocated.
func logosZgcRecordIndex(l1 uint, base, size uintptr) {
	if l1 < uint(len(logosZgcIndexRanges)) {
		logosZgcIndexRanges[l1] = logosZgcIndexRange{base, size}
	}
}

// logosZgcHeapRangeOwned reports whether [base, base+n) overlaps an
// address range the runtime already owns: a previously granted heap
// range tracked in pageAlloc.inUse, or the recorded arena index array.
// The caller (sysAlloc) holds h.lock, so inUse is stable here. Any
// overlap means the range must not be granted again.
func logosZgcHeapRangeOwned(base, n uintptr) bool {
	end := base + n
	if end < base {
		return true
	}
	// The heap's current reservation tail is Reserved but not yet grown
	// into pageAlloc.inUse: the search runs under h.lock before grow
	// commits it, so the inUse scan below cannot see it. Treat it as
	// owned so an exact-hit probe never re-grants an address the heap
	// already owns; a re-grant would reach the page allocator without
	// its pages being committed.
	if cb, ce := mheap_.curArena.base, mheap_.curArena.end; cb < ce && base < ce && cb < end {
		return true
	}
	for i := range logosZgcIndexRanges {
		if ir := &logosZgcIndexRanges[i]; ir.size != 0 && base < ir.base+ir.size && ir.base < end {
			return true
		}
	}
	for _, r := range mheap_.pages.inUse.ranges {
		if rb, rl := r.base.addr(), r.limit.addr(); base < rl && rb < end {
			return true
		}
	}
	return false
}

// logosZgcIndexClearOf reports whether the recorded index array for l1
// is disjoint from the reservation [v, v+size) being published and lies
// outside the low window the exact-hit search grants from. The
// reservation being published is not in pageAlloc.inUse yet, so the
// search-time check cannot see it; the fresh L2 grant must be verified
// against it explicitly.
func logosZgcIndexClearOf(l1 uint, v, size uintptr) bool {
	if l1 >= uint(len(logosZgcIndexRanges)) {
		return false
	}
	ir := &logosZgcIndexRanges[l1]
	if ir.size == 0 {
		return false
	}
	iend := ir.base + ir.size
	if iend < ir.base {
		return false
	}
	vend := v + size
	if vend < v {
		return false
	}
	if v < iend && ir.base < vend {
		return false
	}
	if logosZgcHeapInRange(ir.base, ir.size) {
		return false
	}
	return true
}

// logosZgcRelocateIndex frees an index array that overlaps the in-flight
// reservation or sits inside the low window, and regrants it with an
// explicit high placement outside the window that the search scans. The
// caller holds h.lock. Failure throws rather than publishing overlap.
func logosZgcRelocateIndex(l1 uint, v, size, l2size uintptr) *[1 << arenaL2Bits]*heapArena {
	if l1 < uint(len(logosZgcIndexRanges)) {
		if old := logosZgcIndexRanges[l1]; old.size != 0 {
			sysFreeOS(unsafe.Pointer(old.base), old.size)
			logosZgcIndexRanges[l1] = logosZgcIndexRange{}
		}
	}
	// 2^(heapAddrBits-1) is high, canonical, and outside the color
	// window on every 64-bit build.
	hint := uintptr(1) << (heapAddrBits - 1)
	r := sysReserve(unsafe.Pointer(hint), l2size, "heap index")
	if r == nil || uintptr(r) != hint {
		if r != nil {
			sysUnreserve(r, l2size)
		}
		throw("logos zgc: cannot place arena index outside low window")
	}
	sysMap(r, l2size, &memstats.other_sys, "heap index")
	nl2 := (*[1 << arenaL2Bits]*heapArena)(r)
	logosZgcRecordIndex(l1, uintptr(r), l2size)
	if !logosZgcIndexClearOf(l1, v, size) {
		throw("logos zgc: arena index overlaps heap reservation")
	}
	return nl2
}

// logosZgcPruneHeapHints remaps heap hint entries whose base is already
// outside the color window into distinct low slots, preserving list
// length and low-first order. User-arena hints are untouched (manual
// memory, never colored). Randomized high hint bases are restricted
// here, for zgc only; the global randomizeHeapBase path is unchanged
// for every other mode. Call once from mallocinit after the 64-bit hint
// loop, before any heap use.
//
// Remapping (not dropping) is load-bearing: the default 64-bit hint set
// sits almost entirely above the 42-bit field — 0x00c0<<32 alone is
// 3*2^40, so only the i=0 hint clears the window — and dropping would
// leave a single low hint. Every later growth would then miss the hint
// loop and fall into the full-window exact-hit search, so ordinary heap
// would keep landing high: colored heap words would stop resolving
// (correctly rejected as foreign) and every low-placement gate would
// stay red. Each rewritten hint keeps 1GB spacing from 1GB up, safely
// above the binary and clear of the surviving head hint, and hints stay
// preferences the kernel verifies by exact hit, so a colliding slot
// just falls through to the next.
func logosZgcPruneHeapHints() {
	next := uintptr(1 << 30)
	const step = uintptr(1 << 30)
	limit := uintptr(zgcAddrMask) + 1 - heapArenaBytes
	pp := &mheap_.arenaHints
	for *pp != nil {
		h := *pp
		if h.addr == 0 || h.addr > uintptr(zgcAddrMask) {
			addr := alignUp(next, heapArenaBytes)
			if addr == 0 || addr > limit {
				addr = uintptr(heapArenaBytes)
			}
			h.addr = addr
			h.down = false
			next += step
		}
		pp = &h.next
	}
}

// logosZgcHeapSearch reserves n bytes at a kernel-honored low address.
// It scans every heapArenaBytes-aligned slot in the color window from
// the first valid arena base up to the mask, accepting only an exact
// p == v hit with the whole interval in range. Every mismatch is
// sysUnreserve before advancing, so no high mapping is ever published.
// Returns nil, 0 after the full finite window fails; the caller throws
// the descriptive low-exhaustion diagnostic. The slot count is derived
// from the window itself (<= (1<<42)/heapArenaBytes probes), never a
// fixed budget, with wrap-safe termination on the aligned domain.
func logosZgcHeapSearch(n uintptr) (unsafe.Pointer, uintptr) {
	n = alignUp(n, heapArenaBytes)
	if n == 0 || n > uintptr(zgcAddrMask)+1 {
		return nil, 0
	}
	if isSbrkPlatform {
		v, size := sysReserveAligned(nil, n, heapArenaBytes, "heap reservation")
		if v != nil && logosZgcHeapInRange(uintptr(v), size) {
			if logosZgcCommitLedgerDebug {
				logosZgcCommitLedgerAdd(uintptr(v), size)
			}
			return v, size
		}
		if v != nil {
			sysUnreserve(v, size)
		}
		return nil, 0
	}
	limit := uintptr(zgcAddrMask) + 1 - n
	for base := uintptr(heapArenaBytes); ; base += heapArenaBytes {
		if base > limit {
			break
		}
		if logosZgcHeapRangeOwned(base, n) {
			// Already granted to another reservation: an address
			// granted to one reservation is never granted to
			// another.
			if base+heapArenaBytes < base {
				break
			}
			continue
		}
		v := sysReserve(unsafe.Pointer(base), n, "heap reservation")
		if v == nil {
			if base+heapArenaBytes < base {
				break
			}
			continue
		}
		if uintptr(v) == base && logosZgcHeapInRange(base, n) {
			if logosZgcCommitLedgerDebug {
				logosZgcCommitLedgerAdd(base, n)
			}
			return v, n
		}
		sysUnreserve(v, n)
		if base+heapArenaBytes < base {
			break
		}
	}
	return nil, 0
}
