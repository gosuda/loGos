// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"unsafe"
)

// logosGCCanonicalG returns the canonical address of gp when ZGC colors are
// active, stripping color bits or remapping if evacuated. When ZGC is inactive
// or gp is nil, it returns gp unchanged.
//
//go:nosplit
func logosGCCanonicalG(gp *g) *g {
	if gp == nil || !zgcCycleEnabled() {
		return gp
	}
	return (*g)(unsafe.Pointer(zgcCanonical(uintptr(unsafe.Pointer(gp)))))
}

// logosGCCanonicalGs returns a snapshot header over gs whose backing
// address is canonicalized locally (never written back), so indexing the
// result is safe in NoLB code even when a self-heal CAS left a colored
// word in the header slot. Entry words are still canonicalized by the
// caller with logosGCCanonicalG. Allocation-free; gs is not mutated.
//
//go:nosplit
func logosGCCanonicalGs(gs []*g) []*g {
	hdr := *(*slice)(unsafe.Pointer(&gs))
	if hdr.array != nil {
		hdr.array = unsafe.Pointer(zgcCanonical(uintptr(hdr.array)))
	}
	return *(*[]*g)(unsafe.Pointer(&hdr))
}

// logosGCMarkrootStackG loads the idx'th stack root for markroot.
//
// Both stack-root producers that feed markroot preserve colored words
// verbatim: the regular path stores the allgs reslice as-is
// (gcPrepareMarkRoots) and the checkmark path returns the same reslice
// (logosCheckmarkStackRoots). The parameter pass is already an
// allocation-free local header snapshot; the shared header helper
// canonicalizes its array locally (never written back) to protect the
// indexed entry load, and logosGCCanonicalG on the loaded entry protects
// the status/field dereferences. Neither the passed slice nor the shared
// allgs backing array is mutated, so snapshot lifetimes are unchanged.
//
//go:nosplit
func logosGCMarkrootStackG(roots []*g, idx uint32) *g {
	s := logosGCCanonicalGs(roots)
	return logosGCCanonicalG(s[idx])
}
