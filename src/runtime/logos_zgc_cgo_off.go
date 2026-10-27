// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import "unsafe"

// Inert twins of the moving collector's private cgo emission ABI
// (logos_zgc_cgo.go) for every runtime shape without it. cmd/cgo emits
// calls to these symbols only when its gate (the moving experiment on
// amd64 or arm64) holds, the exact complement of this file's constraint,
// so no ordinary build links a reference to them. They exist so the
// symbol set, signatures, and token type are identical on both shapes,
// and a generated package can never fail to link against a runtime that
// lacks the collector: nothing relocates here, no ownership is taken,
// and the invalid token pairs with nothing.

// cgoForeignBegin takes no ownership: nothing relocates on this build.
//
//go:linkname cgoForeignBegin
func cgoForeignBegin(rows unsafe.Pointer, n uintptr) logosForeignToken {
	return logosForeignInvalidToken
}

// cgoForeignEnd pairs with the invalid token and does nothing.
//
//go:linkname cgoForeignEnd
func cgoForeignEnd(t logosForeignToken) {}

// cgoForeignEnter opens no callback scope.
//
//go:linkname cgoForeignEnter
func cgoForeignEnter() logosForeignToken {
	return logosForeignInvalidToken
}

// cgoForeignResults leaves result words as the callback produced them:
// they are already canonical when nothing moves.
//
//go:linkname cgoForeignResults
func cgoForeignResults(rows unsafe.Pointer, n uintptr) {}
