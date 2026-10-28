// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build race

package runtime

//go:nosplit
func sync_atomic_LoadUintptr(ptr *uintptr) uintptr {
	return abigen_sync_atomic_LoadUintptr(ptr)
}
