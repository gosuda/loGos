// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/cpu"

// logosHoleScanUsable reports whether the CPU runs logosHoleScanAVX2.
//
//go:nosplit
func logosHoleScanUsable() bool {
	return cpu.X86.HasAVX2
}

// logosHoleScanAVX2 compares the 256 line bytes of lines against zero with
// eight VPCMPEQB and returns the free-line bit set. The CPU must have AVX2.
//
//go:noescape
func logosHoleScanAVX2(lines *logosLineMark) (free logosHoles)
