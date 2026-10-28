// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

// Alloc returns *p, first pointing it at a new zero value when it is nil.
// Generated decoders call it so a pointer field decodes into its existing
// pointee, like encoding/json, without the generated text spelling the
// pointee type.
func Alloc[T any](p **T) *T {
	if *p == nil {
		*p = new(T)
	}
	return *p
}
