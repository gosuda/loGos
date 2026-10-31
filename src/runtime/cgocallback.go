// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// These functions are called from C code via cgo/callbacks.go.
// cgocallbackg forwards one opaque frame to generated code; nested
// C-visible result words are healed there, next to _cgoCheckResult and
// via cgoForeignResults, not by walking an untyped frame here.

// Panic.

func _cgo_panic_internal(p *byte) {
	panic(gostringnocopy(p))
}
