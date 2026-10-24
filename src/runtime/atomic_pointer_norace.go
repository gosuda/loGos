// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !race

package runtime

import _ "unsafe"

//go:linkname sync_atomic_LoadUintptr sync/atomic.LoadUintptr
func sync_atomic_LoadUintptr(ptr *uintptr) uintptr
