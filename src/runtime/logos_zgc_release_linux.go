// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && linux && (amd64 || arm64)

package runtime

import "unsafe"

// zgcReleasePages returns the pages of [v, v+n) to the OS with
// madvise(MADV_DONTNEED). Later reads see zero pages.
func zgcReleasePages(v unsafe.Pointer, n uintptr) {
	if madvise(v, n, _MADV_DONTNEED) != 0 {
		throw("zgc: madvise(MADV_DONTNEED) failed")
	}
}
