// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"unsafe"
)

// LogosMovingExperiment reports goexperiment.LogosGCMoving.
func LogosMovingExperiment() bool {
	return goexperiment.LogosGCMoving
}

// Test seam for M19-S5 and M19-S8: opaque wrappers over the relocation
// exclusion flag for package runtime_test, which cannot name mspan.

// LogosSpanNoReloc reports the logosNoReloc flag of the span holding p.
func LogosSpanNoReloc(p unsafe.Pointer) bool {
	s := spanOf(uintptr(p))
	return s != nil && s.logosNoReloc
}

// LogosSpanInitClearsNoReloc sets the flag on a fresh mspan, runs
// mspan.init on it, and reports whether the flag survived.
func LogosSpanInitClearsNoReloc() (before, after bool) {
	var s *mspan
	systemstack(func() {
		lock(&mheap_.lock)
		s = (*mspan)(mheap_.spanalloc.alloc())
		s.init(0, 0)
		s.logosNoReloc = true
		before = s.logosNoReloc
		s.init(0, 0)
		after = s.logosNoReloc
		mheap_.spanalloc.free(unsafe.Pointer(s))
		unlock(&mheap_.lock)
	})
	return before, after
}

// LogosHeapObjectsCanMove reports heapObjectsCanMove.
func LogosHeapObjectsCanMove() bool { return heapObjectsCanMove() }

// LogosEvacTotals reports the evacuations run so far and the objects they
// moved. It reads state the collector keeps for gctrace-independent totals.
func LogosEvacTotals() (cycles, moved uint64) {
	return logosEvacTotals.cycles, logosEvacTotals.objects
}
