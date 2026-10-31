// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// Test-only span gate for the heap-reserve placement proof.
//
// Need: package runtime_test cannot name spanOfHeap, isUserArenaChunk, or
// span limits, and the existing LogosZgcOffHeap reports spanOf (any
// mapped span, including free/manual) rather than the live-heap gate the
// collector and the foreign registry use. The placement proof must record
// a REAL live heap span (spanOfHeap, InUse, bounds-checked) with
// containment, rejecting user-arena chunks the registry also rejects, and
// must never accept a spanless mmap page as proof. This wrapper is the
// minimal read-only projection of that gate; it adds no policy, mints no
// spans, publishes no forwarding entries, and colors nothing.
func LogosZgcHeapReserveSpan(addr uintptr) (base, size uintptr, ok bool) {
	s := spanOfHeap(addr)
	if s == nil || s.isUserArenaChunk {
		return 0, 0, false
	}
	base = s.base()
	if s.limit <= base || addr < base || addr >= s.limit {
		return 0, 0, false
	}
	return base, s.limit - base, true
}
