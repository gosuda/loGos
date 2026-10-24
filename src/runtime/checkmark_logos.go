// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// logosCheckmarkStackRoots backs the stack-root preparation that
// allGsSnapshotSortedForGC performs for gcPrepareMarkRoots.
//
// When useCheckmark is set, the checkmark pass at mark termination is
// re-running gcPrepareMarkRoots with the world stopped and gcphase already
// _GCmarktermination, so the sorted snapshot must not allocate: gcmarknewobject
// throws on any allocation made while checkmarking (mgcmark.go), and mallocgc
// throws on _GCmarktermination. Return the plain allgs reslice instead: it is
// allocation-free, and unlike the sorted partition it does not reset _Gleaked
// statuses, which the goroutine leak profile still reads after mark
// termination (goroutineLeakProfileWithLabelsConcurrent identifies leaks by
// status).
//
// Taking every stack here is exactly what the checkmark pass verifies. When
// findGoroutineLeaks finishes a leak-detection cycle, it queues the leaked
// Gs' stack jobs and restarts the world to drain them before termination
// (gcMarkDone), so everything reachable from those stacks is already marked;
// the checkmark re-derives the same result with the world stopped. A leaked
// stack can legitimately surface pointers to objects the leak cycle never
// intended to keep, so if this pass observes an unmarked object, that is a
// true checkmark finding, not an artifact of the partition.
//
// The boolean reports whether this call handled preparation; the caller keeps
// its upstream body when it is false. No storage is allocated, no status is
// reset, and the non-checkmark path is untouched.
func logosCheckmarkStackRoots() ([]*g, int, bool) {
	if !useCheckmark {
		return nil, 0, false
	}
	return allGsSnapshot(), len(allgs), true
}
