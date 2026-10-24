// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/runtime/gc"

// Shared evacuation state. Integ adaptation of the lane's
// logos_evac.go: the struct is the union of what both moving
// collectors need, because integ has no separate immix copy and the
// immix driver (logos_evac_immix.go) and the zgc pin core
// (logos_evac_pin.go) share one type. Every field below is used by at
// least one driver.
//
// granule is the exclusion granule in bytes: blocks for immix, pages
// for zgc. Each driver sets granule (and pinned.shift, which the
// address set hashes with) before it pins anything.
//
// No field may embed sys.NotInHeap (such as logosImmixBlock): that
// would make the whole struct not-in-heap and forbid the stack-resident
// pin states both collectors build (var ev logosEvac in zgcNewPinEv).
// The immix driver keeps its block scratch in a package-level variable
// instead.
type logosEvac struct {
	pinned     logosAddrSet
	cand       logosWordBuf
	pool       logosWordBuf
	cursors    [gc.NumSizeClasses << 1]logosDestCursor
	fwd        uintptr
	fwdBytes   uintptr
	blocks     uintptr
	moved      uintptr
	movedBytes uintptr
	seen       uintptr
	pinnedBlks uintptr
	fixedBlks  uintptr
	denseBlks  uintptr

	// granule is the exclusion granule in bytes: blocks for immix,
	// pages for zgc.
	granule uintptr
}
