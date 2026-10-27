// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import "unsafe"

// Off-experiment stubs for the syscall foreign-admission lane. Nothing
// relocates here, so no admission is ever needed: every entry point is
// honestly token-free and every End pairs with nothing. Signatures
// mirror the moving lane exactly so shared compiler emission compiles
// on both shapes.
func zgcSyscallAdmit(rows unsafe.Pointer, n uintptr) (frame unsafe.Pointer, gen uint64) {
	return nil, 0
}

func zgcSyscallAdmitIndirect(code unsafe.Pointer, rows unsafe.Pointer, n uintptr) (frame unsafe.Pointer, gen uint64) {
	return nil, 0
}

func zgcSyscallEnd(frame unsafe.Pointer, gen uint64) {
}

//go:nosplit
func zgcSyscallAdmitNoSplit(rows unsafe.Pointer, n uintptr) (frame unsafe.Pointer, gen uint64) {
	return nil, 0
}

//go:nosplit
func zgcSyscallEndNoSplit(frame unsafe.Pointer, gen uint64) {
}
