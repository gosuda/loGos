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

// logosZgcForeignArg is the off-experiment stub; arguments pass through unchanged.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcForeignArg(p unsafe.Pointer) unsafe.Pointer {
	return p
}

// logosZgcForeignHoldEnter is the off-experiment stub; returns -1.
//
//go:nosplit
func logosZgcForeignHoldEnter(gp *g, root uintptr) int {
	return -1
}

// logosZgcForeignHoldExit is the off-experiment stub; no-op.
//
//go:nosplit
func logosZgcForeignHoldExit(gp *g) {
}

// Off-experiment stubs for the shared foreign registry API (owner A).
// Nothing relocates off-experiment: canonical addresses are identities,
// so no ownership is ever taken and every query answers genuinely
// negative. The signatures mirror the moving implementation so shared
// emission compiles on both shapes; behavior is honestly no-op rather
// than fake-moving.
//
// Token protocol off-experiment: Begin always returns the invalid token
// (token-free: proceed without End). End tolerates the invalid token as
// a no-op and throws on any non-nil frame, which is impossible here and
// signals cross-build misuse. Extend requires the same valid-token
// contract as the moving build; any call off-experiment is a caller bug
// and throws. There is no transport bookkeeping off-experiment (g
// carries no foreign state); nothing pins.

// logosZgcForeignBegin is the off-experiment stub: identity is stable,
// so no admission is needed and the invalid token is returned. Shape is
// still validated (same fail-loud producer contract as the moving
// build); only the pinning behavior differs.
//
//go:nosplit
//go:linkname logosZgcForeignBegin
func logosZgcForeignBegin(roots *logosForeignRoot, n uintptr) logosForeignToken {
	logosForeignValidateRoots(roots, n)
	return logosForeignInvalidToken
}

// logosZgcForeignBeginNoSplit is the off-experiment stub: invalid token.
// Shape is validated like Begin.
//
//go:nosplit
func logosZgcForeignBeginNoSplit(roots *logosForeignRoot, n uintptr) logosForeignToken {
	logosForeignValidateRoots(roots, n)
	return logosForeignInvalidToken
}

// logosZgcForeignEnd is the off-experiment stub: invalid tokens pair
// with nothing; a non-nil frame is impossible and throws.
//
//go:nosplit
//go:linkname logosZgcForeignEnd
func logosZgcForeignEnd(t logosForeignToken) {
	if !logosForeignTokenValid(t) {
		return
	}
	throw("logos zgc: foreign End of impossible off-experiment frame")
}

// logosZgcForeignEndNoSplit is the off-experiment stub: same contract as End.
//
//go:nosplit
func logosZgcForeignEndNoSplit(t logosForeignToken) {
	if !logosForeignTokenValid(t) {
		return
	}
	throw("logos zgc: foreign End of impossible off-experiment frame")
}

// logosZgcForeignExtend is the off-experiment stub: invalid outer tokens
// are a no-op, mirroring End. Nothing relocates here, so there is no
// closure to extend and no lifetime to prolong, including for callbacks
// of token-free outer calls. A valid token is impossible off-experiment
// (Begin never returns one) and throws, as it does for End.
func logosZgcForeignExtend(outer logosForeignToken, roots *logosForeignRoot, n uintptr) {
	logosForeignValidateRoots(roots, n)
	if !logosForeignTokenValid(outer) {
		return
	}
	throw("logos zgc: foreign Extend of impossible off-experiment frame")
}

// zgcForeignTransportEnter is the off-experiment stub: g carries no
// foreign state off-experiment (logosForeignG is empty), and the
// transition counter has no reader where no frame can exist, so this is
// a no-op. Transport bookkeeping lives only in the on-experiment
// registry.
//
//go:nosplit
func zgcForeignTransportEnter(gp *g) {
}

// zgcForeignTransportExit pairs TransportEnter on every exitsyscall path.
// Off-experiment it is a no-op, matching Enter.
//
//go:nosplit
func zgcForeignTransportExit(gp *g) {
}

// zgcForeignPagePinned is the off-experiment stub: no page ever pins,
// genuinely, because nothing moves.
//
//go:nosplit
func zgcForeignPagePinned(addr uintptr) bool {
	return false
}

// zgcForeignPagePinnedLocked is the off-experiment stub: always false.
//
//go:nosplit
func zgcForeignPagePinnedLocked(addr uintptr) bool {
	return false
}

// zgcForeignPagePinnedSTW is the off-experiment stub: always false. It
// exists so pause code shared across build shapes can call the STW form
// unconditionally; nothing relocates here, so no page ever pins.
//
//go:nosplit
func zgcForeignPagePinnedSTW(addr uintptr) bool {
	return false
}

// zgcForeignObjectExposed is the off-experiment stub: always false.
//
//go:nosplit
func zgcForeignObjectExposed(addr uintptr) bool {
	return false
}

// zgcForeignObjectExposedLocked is the off-experiment stub: always false.
//
//go:nosplit
func zgcForeignObjectExposedLocked(addr uintptr) bool {
	return false
}

// zgcForeignMarkRoots is the off-experiment stub: nothing to shade.
func zgcForeignMarkRoots(gcw *gcWork) {
}
