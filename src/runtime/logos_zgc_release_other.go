// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && !linux && (amd64 || arm64)

package runtime

import "unsafe"

// zgcReleasePages returns the pages of [v, v+n) to the OS through the
// runtime's portable unused-memory call.
func zgcReleasePages(v unsafe.Pointer, n uintptr) {
	sysUnused(v, n)
}
