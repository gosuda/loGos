// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && arm64

package runtime

import (
	"internal/runtime/atomic"
	_ "unsafe"
)

// Out-of-line arm64 atomic pointer load (W1d).
//
// The Loadp intrinsic is suppressed under -race and
// -d=ssa/intrinsics/off, so those loads reach the assembly stub in
// internal/runtime/atomic, which cannot see the experiment (atomic
// assembly gets no GOEXPERIMENT macros) and therefore calls this hook
// unconditionally with the (slot, raw) pair. The hook mirrors the
// compiled barrier's fast path and delegates mismatches to
// zgcLoadBarrierSlow, so out-of-line loads observe the same canonical
// current object as barriered loads. Plain ABI, deliberately not
// nosplit: like the slow call the compiler emits for barriered loads,
// it may grow the stack. The off-experiment identity lives in
// logos_zgc_boundary_off.go; the linkname push here guarantees the
// linker emits the ABI0 wrapper the assembly stub calls. This hook is
// the W1d closed row named on zgcBoundaryWrappersLanded: the raw word
// never escapes unbarriered, so the flag's remaining residuals
// (reflectcall fn, defer records) are elsewhere.
//
//go:linkname zgcAtomicLoadpSlow internal/runtime/atomic.zgcAtomicLoadpSlow
func zgcAtomicLoadpSlow(slot *uintptr, v uintptr) uintptr {
	if (v^atomic.Loaduintptr(&zgcGoodMask))&atomic.Loaduintptr(&zgcColorMaskBits) == 0 {
		return v & atomic.Loaduintptr(&zgcUnmaskBits)
	}
	// Ordinary-heap stale color (e.g. the previous good color still on a
	// word into to-space, which is heap but never a registered source):
	// the slow path returns v unchanged when no source is registered for
	// addr, which would hand asm a colored word. Strip to the canonical
	// address here without touching the slot (the slow path heals
	// nothing on that branch either). Relocating pages still delegate
	// with the original v so the heal CAS sees the exact slot word;
	// foreign words (addr above the heap range) also still delegate.
	if colors := atomic.Loaduintptr(&zgcColorMaskBits); colors != 0 {
		addr := v &^ colors
		if addr != v && addr <= zgcAddrMask {
			zgcPageTransientEnter()
			pg := zgcPageLookupTransient(addr)
			zgcPageTransientLeave()
			if pg == nil && spanOfHeap(addr) != nil {
				return addr
			}
		}
	}
	return zgcLoadBarrierSlow(slot, v)
}
