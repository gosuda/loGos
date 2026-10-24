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
// linker emits the ABI0 wrapper the assembly stub calls.
//
//go:linkname zgcAtomicLoadpSlow internal/runtime/atomic.zgcAtomicLoadpSlow
func zgcAtomicLoadpSlow(slot *uintptr, v uintptr) uintptr {
	if (v^atomic.Loaduintptr(&zgcGoodMask))&atomic.Loaduintptr(&zgcColorMaskBits) == 0 {
		return v & atomic.Loaduintptr(&zgcUnmaskBits)
	}
	return zgcLoadBarrierSlow(slot, v)
}
