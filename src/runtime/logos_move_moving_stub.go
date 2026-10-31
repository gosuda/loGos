// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && !(amd64 || arm64)

package runtime

import (
	"internal/abi"
	"unsafe"
)

//go:linkname maps_logosPinMapKey internal/runtime/maps.logosPinMapKey
func maps_logosPinMapKey(key *abi.Type, p unsafe.Pointer) {
}

//go:linkname sync_logosPinMapKey internal/sync.logosPinMapKey
func sync_logosPinMapKey(key *abi.Type, p unsafe.Pointer) {
}
