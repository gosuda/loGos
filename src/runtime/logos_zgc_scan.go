// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// Scan integration for the zgc cycle.
//
// A quarantined span yields no object at three sites: greyobject entry,
// tryDeferToSpanScan entry, and findObject. Under colors, all three
// unmask before span lookup and resolve forwarding. Heal-on-scan rewrites
// scanned slots to uncolored canonical words; conservative scanners
// unmask but never heal and never write.

// zgcRemapScanWord resolves a scanned pointer word to its current
// canonical address. It reports false for words with no object: nil,
// foreign words, and words into quarantined source spans.
//
//go:nowritebarrierrec
func zgcRemapScanWord(p uintptr) (uintptr, bool) {
	if p == 0 {
		return 0, false
	}
	c := zgcCanonical(p)
	s := spanOfHeap(c)
	if s == nil {
		return 0, false
	}
	if zgcSpanQuarantined(s) {
		return 0, false
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
	c, ok := zgcRemapScanWord(obj)
	if !ok {
		return 0, nil, 0, false
	}
	s := spanOfHeap(c)
	if s == nil {
		return 0, nil, 0, false
	}
	idx := s.objIndex(c)
	return s.base() + idx*s.elemsize, s, idx, true
}

// zgcHealScanSlot rewrites a scanned slot to its uncolored canonical
// word. Collector-fixed slots include runtime-owned words that plain
// code dereferences, so the heal never colors: a colored word in them
// faults under software encoding.
//
//go:nowritebarrierrec
func zgcHealScanSlot(slot *uintptr) {
	if !zgcCycleEnabled() {
		return
	}
	c := zgcCanonical(*slot)
	if c != *slot {
		*slot = c
	}
}
