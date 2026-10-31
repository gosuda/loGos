// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.arenas && !goexperiment.logosarena

package arena

// DefaultArenaEnabled reports whether the default allocation path may use
// the arena bump path. Plain-Go builds never enable it, so callers observe
// no arena allocation and no behavior change.
//
// Stability: stable.
const DefaultArenaEnabled = false

// DefaultAlloc returns a zeroed []byte of size bytes via the plain heap.
// This build never bump-allocates: it is exactly make([]byte, size), so
// plain-Go callers see no arena allocation and no behavior change.
//
// Stability: stable.
func DefaultAlloc(size int) []byte {
	return make([]byte, size)
}

// DefaultBumpAllocs counts DefaultAlloc calls that took the bump path.
// It is always zero on plain-Go builds; the plain-Go control test pins
// this.
//
// Stability: stable.
func DefaultBumpAllocs() int64 {
	return 0
}
