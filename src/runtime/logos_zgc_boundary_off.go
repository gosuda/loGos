// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Fallback raw pointer atomics (W1b, plus the W1d arm64 Loadp hook)
// outside the moving-collector experiment: no slot ever carries color
// bits, so the raw primitives are already canonical. The signatures
// mirror the tagged helpers, and the same three runtime names serve
// the unconditional raw callers in stack.go and type.go (CAS pair) and
// the unconditional arm64 Loadp stub (slow hook), so the W1a and W1d
// pull linknames resolve on every build shape.
//
// zgcAtomicStorepNoWB stores new at *ptr without a write barrier.
//
//go:nosplit
//go:nowritebarrierrec
func zgcAtomicStorepNoWB(ptr *unsafe.Pointer, new unsafe.Pointer) {
	atomic.StorepNoWB(noescape(unsafe.Pointer(ptr)), new)
}

// zgcAtomicLoadpSlow is the arm64 out-of-line Loadp hook (W1d). The
// assembly stub is unconditional and already did the acquire load, so
// the identity is exact off-experiment: no colors can exist, and there
// is no barrier call inside (no recursion). Plain ABI, deliberately
// not nosplit, matching the tagged real implementation in
// logos_zgc_atomic_loadp.go so both shapes expose the same symbol
// contract to the linker's ABI0 wrapper.
//
//go:linkname zgcAtomicLoadpSlow internal/runtime/atomic.zgcAtomicLoadpSlow
func zgcAtomicLoadpSlow(slot *uintptr, v uintptr) uintptr { return v }

// zgcAtomicCasp1 compares *ptr with old and swaps in new on match,
// exactly the raw primitive: without color bits the canonical identity
// is the raw word, so a plain compare preserves the semantics and the
// CAS is single-shot like Casp1.
//
//go:nosplit
//go:nowritebarrierrec
func zgcAtomicCasp1(ptr *unsafe.Pointer, old, new unsafe.Pointer) bool {
	return atomic.Casp1(ptr, old, new)
}
