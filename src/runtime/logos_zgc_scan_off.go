// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

func zgcRemapScanWord(p uintptr) (uintptr, bool) {
	if p == 0 {
		return 0, false
	}
	return p, true
}

func zgcResolveScanTarget(obj uintptr, span *mspan, objIndex uintptr) (uintptr, *mspan, uintptr, bool) {
	return obj, span, objIndex, true
}

func zgcHealScanSlot(slot *uintptr) {}
