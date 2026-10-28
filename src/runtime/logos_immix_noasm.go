// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !amd64

package runtime

// logosHoleScanUsable reports that no AVX2 hole scan exists on this
// architecture.
//
//go:nosplit
func logosHoleScanUsable() bool {
	return false
}

// logosHoleScanAVX2 exists so that the hole scan dispatch compiles; the
// dispatch never reaches it because logosHoleScanUsable is false.
func logosHoleScanAVX2(lines *logosLineMark) (free logosHoles) {
	panic("logosHoleScanAVX2: unavailable on this architecture")
}
