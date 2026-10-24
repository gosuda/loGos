// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// Page selection for the zgc cycle.
//
// Eligible 2 MB pages over the fixed-base heap, directory-registered at
// selection: garbage above 35 percent of the page, every overlapping span
// fully inside the page, every span movable, no pin exclusion overlap,
// capped by zgcMaxRelocateBytesPerCycle, most fragmented first. live == 0
// pages are never registered; a pinned or immovable span excludes the
// whole page.

// zgcMaxRelocateBytesPerCycle caps the bytes a cycle relocates.
var zgcMaxRelocateBytesPerCycle = 64 << 20

// zgcSpanMovable reports whether the objects of s may move. A span that
// holds a finalizer or cleanup special now or ever, a span with pinned
// objects, a span in a cache, and a span that extends beyond one page
// stays in place.
func zgcSpanMovable(s *mspan) bool {
	if s.spanclass.sizeclass() == 0 || s.isUserArenaChunk || s.sweepgen != mheap_.sweepgen {
		return false
	}
	if s.logosNoReloc || s.specials != nil || logosSpanPinned(s) {
		return false
	}
	return true
}

// logosSpanPinned is shared with the immix driver and defined in
// logos_evac_immix.go: both collectors exclude a span whose objects feed
// an address hash. This file only consumes it.

// zgcPageLive counts the marked bytes of the in-use spans overlapping the
// page at base. The world is stopped and marking is complete.
func zgcPageLive(base uintptr, spans []*mspan) (live uintptr, movable bool) {
	for _, s := range spans {
		if s == nil || s.state.get() != mSpanInUse {
			return 0, false
		}
		if s.base() < base || s.base()+s.npages*pageSize > base+zgcPageSize {
			return 0, false
		}
		if !zgcSpanMovable(s) {
			return 0, false
		}
		if s.spanclass.sizeclass() == 0 {
			if s.markBitsForIndex(0).isMarked() {
				live += s.elemsize
			}
			continue
		}
		for i := uintptr(0); i < uintptr(s.nelems); i++ {
			if s.markBitsForIndex(i).isMarked() {
				live += s.elemsize
			}
		}
	}
	return live, true
}

// zgcOverlappingSpans collects the in-use spans overlapping [base,
// base+zgcPageSize) into out.
func zgcOverlappingSpans(base uintptr, out *[]*mspan) {
	*out = (*out)[:0]
	for _, s := range mheap_.allspans {
		if s.state.get() != mSpanInUse {
			continue
		}
		if s.limit <= base || s.base() >= base+zgcPageSize {
			continue
		}
		*out = append(*out, s)
	}
}

// zgcSelectPages selects pages for relocation and registers them in the
// directory. It returns the number of pages registered. The world is
// stopped; marking is complete. Selection never registers a page with no
// live bytes, a page overlapping a pinned span, or more than budget bytes
// of live data.
// zgcSelectScratch bounds pause-time selection storage: the heap scan is
// allocation-free and only the most fragmented zgcSelectKeep candidates
// are retained. All selection scratch is stop-the-world exclusive,
// preallocated by zgcInitSelect while the world runs.
var zgcSelectScratchSpans []*mspan

type zgcCandidate struct {
	base uintptr
	live uintptr
}

var zgcSelectScratchCands []zgcCandidate

// zgcScratchPages stages page lists in pauses without allocating: every
// user holds it stop-the-world exclusively across its own statements.
var zgcScratchPages []*zgcPage

const zgcSelectKeep = 256

func zgcInitSelect() {
	if zgcSelectScratchSpans == nil {
		zgcSelectScratchSpans = make([]*mspan, 0, 1024)
	}
	if zgcSelectScratchCands == nil {
		zgcSelectScratchCands = make([]zgcCandidate, 0, 256)
	}
	if zgcScratchPages == nil {
		zgcScratchPages = make([]*zgcPage, 0, 128)
	}
}

func zgcSelectPages(budget uintptr) int {
	if !zgcCycleEnabled() {
		return 0
	}
	assertWorldStopped()
	ev := zgcNewPinEv()
	defer ev.pinned.release()
	if !ev.collectPins() {
		return 0
	}
	if budget == 0 || budget > uintptr(zgcMaxRelocateBytesPerCycle) {
		budget = uintptr(zgcMaxRelocateBytesPerCycle)
	}
	spans := zgcSelectScratchSpans[:0]
	cands := zgcSelectScratchCands[:0]
	for _, ai := range mheap_.heapArenas {
		base := arenaBase(ai)
		for off := uintptr(0); off < heapArenaBytes; off += zgcPageSize {
			bb := base + off
			if bb > zgcAddrMask-(zgcPageSize-1) {
				continue
			}
			if ev.pinned.has(bb) {
				continue
			}
			zgcOverlappingSpans(bb, &spans)
			if len(spans) == 0 {
				continue
			}
			live, ok := zgcPageLive(bb, spans)
			if !ok || live == 0 {
				continue
			}
			if !zgcFragmentationExceeds(zgcPageSize-live, zgcPageSize) {
				continue
			}
			cands = zgcSelectKeepCands(cands, zgcCandidate{base: bb, live: live})
		}
	}
	zgcSelectScratchSpans = spans[:0]
	// Most fragmented first: ascending live bytes.
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j].live < cands[j-1].live; j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
	n := 0
	var used uintptr
	for _, c := range cands {
		if used+c.live > budget {
			break
		}
		zgcOverlappingSpans(c.base, &spans)
		live, ok := zgcPageLive(c.base, spans)
		if !ok || live == 0 || live != c.live {
			continue
		}
		var nObjects uintptr
		for _, s := range spans {
			if s.spanclass.sizeclass() == 0 {
				nObjects++
			} else {
				nObjects += uintptr(s.nelems)
			}
		}
		pg := zgcNewPage(c.base, zgcPageSize, live, nObjects)
		st := zgcPageState(pg)
		st.spans = append(st.spans[:0], spans...)
		used += live
		n++
	}
	zgcSelectScratchSpans = spans[:0]
	zgcSelectScratchCands = cands[:0]
	return n
}

// zgcSelectKeepCands retains the most fragmented candidates in bounded
// storage: append while room lasts, otherwise replace the least
// fragmented entry when the newcomer is more fragmented.
func zgcSelectKeepCands(cands []zgcCandidate, c zgcCandidate) []zgcCandidate {
	if len(cands) < zgcSelectKeep {
		return append(cands, c)
	}
	worst := 0
	for i := range cands {
		if cands[i].live > cands[worst].live {
			worst = i
		}
	}
	if c.live < cands[worst].live {
		cands[worst] = c
	}
	return cands
}
