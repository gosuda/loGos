// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Scan integration for the zgc cycle.
//
// A quarantined span yields no object at three sites: greyobject entry,
// tryDeferToSpanScan entry, and findObject. Under colors, all three
// unmask before span lookup and resolve forwarding. Heal-on-scan rewrites
// scanned slots to uncolored canonical words; conservative scanners
// unmask but never heal and never write.

// zgcRemapScanWord resolves a scanned pointer word to its current
// canonical address. It reports false for nil, foreign words, words
// into quarantined source spans, and words into slots the allocator has
// not handed out.
//
// A scanned slot may hold a stack, global, or C address, so this path
// must not run a heap lookup on an unvalidated word. Only a word that a
// registered relocation page names is resolved and checked for
// quarantine; every other in-field word is handed back stripped, and the
// caller's own arena lookup decides what it is.
//
//go:nowritebarrierrec
func zgcRemapScanWord(p uintptr) (uintptr, bool) {
	if p == 0 {
		return 0, false
	}
	c := p
	if atomic.Loaduintptr(&zgcColorMaskBits) != 0 {
		if p&^(zgcAddrMask|zgcFlagMask|zgcColorMask) != 0 {
			return 0, false
		}
		c = p & zgcAddrMask
	}
	if !zgcPageRegisteredNoSplit(c) {
		// Not a relocation page: decline, so the caller keeps its own
		// word and takes the normal scanning path. Handing back a word
		// this path cannot classify is what corrupts the scan.
		return 0, false
	}
	// The arena map may not be live yet (colors armed before the first
	// reservation); without it there is nothing to resolve or quarantine.
	if zgcHeapArenaLive() {
		c = zgcRemap(c)
		if s := spanOfHeap(c); s != nil {
			if zgcSpanQuarantined(s) {
				return 0, false
			}
			// A stale word may name a slot a previous sweep freed and
			// nothing has reused yet; resolution alone cannot tell it
			// from a live object. Reject it here so tryDeferToSpanScan
			// never sets an inline mark for it — exactly as the
			// conservative scanner skips such slots. Callers that fall
			// through to greyobject are covered again by
			// zgcCheckedScanTarget. Slots allocated during the mark
			// are already black, so declining them here loses nothing.
			idx := s.objIndex(c)
			if s.isFreeOrNewlyAllocated(idx) {
				return 0, false
			}
		}
	}
	return c, true
}

// zgcResolveScanTarget resolves a scan target triple (object start, span,
// index) through colors and forwarding. It reports false when the target
// has no scannable object: a quarantined source span, a span that left
// the heap, or an out-of-bounds word.
//
//go:nowritebarrierrec
func zgcResolveScanTarget(obj uintptr, span *mspan, objIndex uintptr) (uintptr, *mspan, uintptr, bool) {
	if !zgcCycleEnabled() {
		return obj, span, objIndex, true
	}
	c := obj
	if atomic.Loaduintptr(&zgcColorMaskBits) != 0 {
		if obj&^(zgcAddrMask|zgcFlagMask|zgcColorMask) != 0 {
			return 0, nil, 0, false
		}
		c = obj & zgcAddrMask
	}
	if !zgcPageRegisteredNoSplit(c) {
		// Not a relocation source: the caller's triple stands and
		// marking proceeds normally. Declining here would leave
		// every object outside the registered pages white, so
		// selected pages would release (and sweep would free) live
		// objects with no forwarding entry for the gcStart rewrite.
		return zgcCheckedScanTarget(obj, span, objIndex)
	}
	c, ok := zgcRemapScanWord(obj)
	if !ok {
		return 0, nil, 0, false
	}
	s := spanOfHeap(c)
	if s == nil {
		return 0, nil, 0, false
	}
	idx := s.objIndex(c)
	return zgcCheckedScanTarget(s.base()+idx*s.elemsize, s, idx)
}

// zgcCheckedScanTarget reports the resolved scan triple, rejecting slots
// the allocator has not handed out. A stale word — a pointer into a slot
// a previous sweep freed and nothing has reused yet — resolves cleanly
// through colors and forwarding, so resolution alone cannot tell it from
// a live object; only the allocation bitmap can. Every grey path that
// reaches greyobject while a zgc cycle runs funnels through
// zgcResolveScanTarget (shade, precise scans, foreign roots, the load
// barrier), so rejecting here closes all of them exactly as the
// conservative scanner does with isFreeOrNewlyAllocated. Slots allocated
// during the mark are already black, so skipping them here loses nothing.
// Without this, a shaded stale word sets a mark with no allocation behind
// it, and the quarantine vet later throws on live marks that exceed
// allocations.
//
//go:nowritebarrierrec
func zgcCheckedScanTarget(obj uintptr, span *mspan, objIndex uintptr) (uintptr, *mspan, uintptr, bool) {
	if span != nil && span.isFreeOrNewlyAllocated(objIndex) {
		return 0, nil, 0, false
	}
	// Greyobject funnel: every precise shade that survives to
	// greyobject passes here, so an allow on an empty span names the
	// setter in mgcmark.go. Count only an imminent set: greyobject
	return obj, span, objIndex, true
}

// zgcHealSlotWritable reports whether the collector may rewrite the word
// at slot. A scanned slot belongs to a heap object, to the running
// goroutine's stack, or to module data. The first two are the collector's
// to rewrite; module data is runtime state that the gcStart rewrite owns,
// and healing there once replaced mheap_.arenas with an arena base and
// faulted every arena lookup.
//
//go:nowritebarrierrec
func zgcHealSlotWritable(slot *uintptr) bool {
	p := uintptr(unsafe.Pointer(slot))
	if gp := getg(); gp != nil && p >= gp.stack.lo && p < gp.stack.hi {
		return true
	}
	return zgcHeapArenaLive() && spanOfHeap(p) != nil
}

// zgcHealScanSlot rewrites a scanned slot to its uncolored canonical
// word. Collector-fixed slots include runtime-owned words that plain
// code dereferences, so the heal never colors: a colored word in them
// faults under software encoding. A scanned slot is runtime-produced,
// so the heal accepts any word whose stripped form lies in the address
// field, without the heap-span check that zgcCanonical keeps for the
// untag and load paths; it still forwards through a registered page
// when present. Words with any bit above the non-address field are
// foreign and pass through untouched. It writes only to a slot the
// collector owns; see zgcHealSlotWritable.
//
//go:nowritebarrierrec
func zgcHealScanSlot(slot *uintptr) {
	// Gate on colors, not on the cycle: the test configures colors without
	// linking zgc mode, and every colored slot must still heal to the pure
	// address-field value. With colors inert the untag is the identity.
	if atomic.Loaduintptr(&zgcColorMaskBits) == 0 {
		return
	}
	if !zgcHealSlotWritable(slot) {
		return
	}
	v := *slot
	if v&^(zgcAddrMask|zgcFlagMask|zgcColorMask) != 0 {
		// Foreign word: high bits outside the address, flag, and color
		// fields. Leave every one of them intact.
		return
	}
	// It strips the non-address field and resolves a word that a
	// registered relocation page names; any other word changes only by
	// that strip.
	addr := v & zgcAddrMask
	if zgcPageRegisteredNoSplit(addr) && zgcHeapArenaLive() {
		addr = zgcRemap(addr)
	}
	if addr != v {
		*slot = addr
	}
}
