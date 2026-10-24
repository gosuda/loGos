// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// Without zgc no object moves: pointer words hash and compare by address.

func zgcIdentityKeys(t *_type) bool { return false }

func zgcCanonical(v uintptr) uintptr { return v }

func zgcIdentityTracking() bool { return false }

func zgcIdentitySweepFree(x uintptr) {}

func zgcptrhash(p unsafe.Pointer, h uintptr) uintptr {
	return memhash(p, h, goarch.PtrSize)
}

func zgcptrequal(p, q unsafe.Pointer) bool {
	return *(*uintptr)(p) == *(*uintptr)(q)
}
