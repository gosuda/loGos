// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving

package runtime

// zgcEnableHardwareTagging reports that ARM64 Top Byte Ignore is active:
// Linux sets TCR_EL1.TBI0 for every user address space, so bits 63..56 are
// ignored by load and store instructions.
func zgcEnableHardwareTagging() (ok bool, errno uintptr) {
	return true, 0
}
