// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import "unsafe"

//go:nosplit
func logosZgcPinnerCanonicalize(ptr unsafe.Pointer) unsafe.Pointer {
	return ptr
}

func logosZgcPinnerPin(ptr unsafe.Pointer) (unsafe.Pointer, bool) {
	return nil, false
}

func logosZgcPinnerSetPinned(ptr unsafe.Pointer) bool {
	return false
}

// logosPinSignalObject is the identity without allocation or locks when
// no moving collector runs.
func logosPinSignalObject(p unsafe.Pointer) unsafe.Pointer {
	return p
}
