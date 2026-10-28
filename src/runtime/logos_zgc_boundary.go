// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Go-side syscall, cgo, and string boundaries (W2).
//
// Every stub taking a pointer holds the memory by reference while the
// thread traps or blocks, so the kernel and C must never see a colored
// word. Untagging happens at the Go caller, never in asm: the untag is
// pure arithmetic, nosplit-safe, and allocation-free. P4 pins the
// by-reference blocks for the call duration; the pin pass covers the
// conservative frames. Until the Z2-asm wrappers land, the remaining
// assembly stub rows stay an audited residual.

//go:nosplit
//go:nowritebarrierrec
func zgcUntagFast(p uintptr) uintptr {
	if atomic.Loaduintptr(&zgcColorMaskBits) == 0 {
		return p
	}
	a := p & zgcAddrMask
	if a == p || zgcPageFor(a) == nil {
		return p
	}
	return a
}

// logosZgcUntagArg clears every color and tag bit of a pointer argument
// crossing into a syscall or cgo call. With colors inert it is the
// identity, so unconfigured C, mmap, and stack words keep their high
// address bits.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcUntagArg(p unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(zgcUntagFast(uintptr(p)))
}

// logosZgcCanonicalStoreValue clears every color and tag bit of a heap
// value being stored into a pointer slot. It must stay a direct function
// of its unsafe.Pointer argument, never the uintptr round trip of
// logosZgcUntagArg: upstream noescape hides escapes exactly through that
// uintptr conversion (stubs.go noescape/noEscapePtr), so a stored value
// routed through logosZgcUntagArg loses the escape edge that makes
// modulesinit heap-allocate the modules slice, and the global then names
// a dead stack frame (GDB 6114: modulesSlice == 0x7fffffffd5e0, the
// caller's C stack). With colors inert this returns p unchanged; the
// escape edge is structural, not arithmetic.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcCanonicalStoreValue(p unsafe.Pointer) unsafe.Pointer {
	q := unsafe.Pointer(zgcUntagFast(uintptr(p)))
	if q == p {
		return p
	}
	return q
}

// zgcCanonicalStringData returns the canonical data pointer of the string
// at sp. Map string fast paths compare through it so assembly memhash
// reads canonical bytes.
func zgcCanonicalStringData(sp *string) unsafe.Pointer {
	return unsafe.Pointer(zgcUntagFast(*(*uintptr)(unsafe.Pointer(sp))))
}

// maps_logosZgcCanonicalString is the maps package hook for canonical
// string data pointers (W2/W3 caller-canonicalized pattern).
//
//go:linkname maps_logosZgcCanonicalString internal/runtime/maps.logosZgcCanonicalString
func maps_logosZgcCanonicalString(sp *string) unsafe.Pointer {
	return zgcCanonicalStringData(sp)
}

// Canonical raw pointer atomics (W1b).
//
// A barriered load heals a heap slot to the good color, so a slot can
// hold a colored word when a raw atomic CAS or store touches it. The
// raw primitives below never heal: they canonicalize both operands,
// compare canonical identities against the raw word, and write only
// canonical replacement words, so a CAS succeeds across a color change
// and fails only on a true identity mismatch. With colors inert the
// untag is the identity and the CAS reduces to load, compare, swap.
//
// casPointerNoWB and storePointerNoWB are pulled by
// internal/runtime/atomic under the experiment (types.go
// CompareAndSwapNoWB and StoreNoWB); the same two runtime names serve
// the unconditional raw callers in stack.go and type.go, so the
// inverted-tag fallback file mirrors them with raw primitives.

// zgcAtomicStorepNoWB stores the canonical form of new at *ptr without
// a write barrier. Only canonical words are ever written; healing
// stays with the barriered-load slow path.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname zgcAtomicStorepNoWB internal/runtime/atomic.storePointerNoWB
func zgcAtomicStorepNoWB(ptr *unsafe.Pointer, new unsafe.Pointer) {
	atomic.StorepNoWB(noescape(unsafe.Pointer(ptr)), logosZgcCanonicalStoreValue(new))
}

// zgcAtomicCasp1 compares the canonical identity of old with the
// canonical identity of the raw word at *ptr. On identity match it
// retries until the raw word is unchanged, then swaps in the canonical
// form of new; a changed raw word (recolor, heal, relocation) retries
// rather than failing, a different canonical identity fails with the
// slot untouched. Never heals, never allocates, never locks.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname zgcAtomicCasp1 internal/runtime/atomic.casPointerNoWB
func zgcAtomicCasp1(ptr *unsafe.Pointer, old, new unsafe.Pointer) bool {
	want := zgcUntagFast(uintptr(old))
	repl := uintptr(logosZgcUntagArg(new))
	slot := (*uintptr)(unsafe.Pointer(ptr))
	for {
		raw := atomic.Loaduintptr(slot)
		if zgcUntagFast(raw) != want {
			return false
		}
		if atomic.Casuintptr(slot, raw, repl) {
			return true
		}
	}
}

// zgcBoundaryWrappersLanded reports whether the Z2-asm wrapper set (W1
// atomics, W6 debug/signal) landed. The Go-side untag above ships with
// this lane; the assembly rows stay blocked on the audit owner.
func zgcBoundaryWrappersLanded() bool {
	return false
}

// zgcWalkAllLiveForCheck verifies no stale reference remains after the
// gcStart rewrite when checkmark debugging is on.
func zgcWalkAllLiveForCheck() {
	logosWalkAllWith(zgcVerifySlot, logosWalkPolicy{skipSpan: zgcSpanQuarantined, useAllocBits: true})
}
