// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/gc"
)

// logosEvacReserve records the destination capacity the previous evacuation
// asked for. gcStart allocates spans for the classes whose evacuation ran out
// of slots and publishes them where the sweeper will find them, which is what
// makes the grant legal: nextSpanForSweep walks the class centrals only, so a
// span that no list names is never swept and never returns to the allocator.
// The span is pushed on the unswept set for the coming sweep generation and
// keeps the current one in its own sweepgen, exactly what sweepLocked expects
// of a span it pops.
var logosEvacReserve struct {
	short [gc.NumSizeClasses << 1]uintptr
}

// logosEvacReserveLimit bounds the spans one class may reserve per cycle.
const logosEvacReserveLimit = 4

// logosEvacReserveDestination runs at gcStart after finishsweep_m, where the
// world is stopped and no marking has begun, so growing the heap cannot sweep
// an object the marked heap still references.
func logosEvacReserveDestination() {
	if !goexperiment.LogosGCMoving {
		return
	}
	sg := mheap_.sweepgen
	for c := range logosEvacReserve.short {
		sc := spanClass(c)
		need := logosEvacReserve.short[sc]
		logosEvacReserve.short[sc] = 0
		if need == 0 || sc.sizeclass() < 3 {
			continue
		}
		got := uintptr(0)
		for range logosEvacReserveLimit {
			ns := mheap_.alloc(logosImmixGrowNPages(sc), sc)
			if ns == nil {
				break
			}
			ns.initHeapBits()
			mheap_.central[sc].mcentral.fullUnswept(sg + 2).push(ns)
			got += logosSpareSlots(ns)
			if got >= need {
				break
			}
		}
	}
}

// logosEvacNoteShortfall records the marked objects an evacuation left in place
// because its class ran out of destination slots. The count, not the number of
// misses, decides how much the next cycle reserves, because one miss can leave
// a whole span behind.
func logosEvacNoteShortfall(s *mspan, from uintptr) {
	if !goexperiment.LogosGCMoving {
		return
	}
	n := uintptr(0)
	for j := from; j < uintptr(s.nelems); j++ {
		if s.markBitsForIndex(j).isMarked() {
			n++
		}
	}
	logosEvacReserve.short[s.spanclass] += n
}
