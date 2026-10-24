// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package atomic

import "unsafe"

// Canonical ZGC pointer atomics (W1a), outside the moving-collector
// experiment: no slot ever carries color bits, so both helpers are the
// upstream raw primitives. The annotations keep the nosplit chain of the
// calling methods (StoreNoWB, CompareAndSwapNoWB) intact, exactly as
// with the raw assembly primitives they replace.

//go:nosplit
//go:nowritebarrierrec
func casPointerNoWB(ptr *unsafe.Pointer, old, new unsafe.Pointer) bool {
	// Casp1 is written in Go on wasm, where "*ptr = new" takes a write
	// barrier; the word forms are the same operation without one.
	return Casuintptr((*uintptr)(unsafe.Pointer(ptr)), uintptr(old), uintptr(new))
}

//go:nosplit
//go:nowritebarrierrec
func storePointerNoWB(ptr *unsafe.Pointer, new unsafe.Pointer) {
	// Storeuintptr rather than StorepNoWB for the same reason as the
	// CAS above: on wasm the pointer form compiles to a write barrier.
	Storeuintptr((*uintptr)(unsafe.Pointer(ptr)), uintptr(new))
}
