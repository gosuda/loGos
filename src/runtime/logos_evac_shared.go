// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Declarations shared by the untagged evacuation state (logos_evac.go) and
// both moving-collector drivers. They stay untagged: the logosEvac struct
// and the untagged test seams name them in every build configuration, while
// the drivers that use them carry their own build tags.

// logosPagesPerBlock is the number of heap pages in one immix block.
const logosPagesPerBlock = logosImmixBlockBytes / pageSize

// logosDestCursor walks the destination spans of one span class: the span
// index in the pool and the next object index to examine in that span.
type logosDestCursor struct {
	cur    uintptr
	end    uintptr
	obj    uintptr
	budget uintptr
	open   bool
}

// spanSlotSpecial reports whether a special record covers slot i of s.
func spanSlotSpecial(s *mspan, i uintptr) bool {
	if s.specials == nil {
		return false
	}
	lo := i * s.elemsize
	hi := lo + s.elemsize
	for sp := s.specials; sp != nil; sp = sp.next {
		if sp.offset >= lo && sp.offset < hi {
			return true
		}
	}
	return false
}

// logosSpareSlots returns how many unmarked slots of ds the evacuation may
// claim. It leaves one: a span the central lists hold as partial must still
// have a free slot after the next sweep.
func logosSpareSlots(ds *mspan) uintptr {
	free := uintptr(0)
	for i := uintptr(0); i < uintptr(ds.nelems); i++ {
		if !ds.markBitsForIndex(i).isMarked() && !spanSlotSpecial(ds, i) {
			free++
		}
	}
	if free == 0 {
		return 0
	}
	return free - 1
}

// logosEvacTotals counts evacuation work across cycles. It is written with
// the world stopped.
var logosEvacTotals struct {
	cycles  uint64
	blocks  uint64
	objects uint64
	bytes   uint64
}
