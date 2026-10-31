// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/goexperiment"

// logosImmixMode reports whether the immix moving collector is selected: the
// experiment is on and the linker chose -gc=immix (M19-S5).
func logosImmixMode() bool {
	return goexperiment.LogosGCMoving && logosGCMode == "immix"
}

// logosHeapObjectsCanMove is heapObjectsCanMove for the moving modes: immix
// (M19-S5) and zgc, which relocates objects out of fragmented pages through
// an off-heap forwarding table (SPEC Sec 19.2) with an identity hash that
// survives relocation (SPEC Sec 8.4).
func logosHeapObjectsCanMove() bool {
	return logosImmixMode() || (goexperiment.LogosGCMoving && logosGCMode == "zgc")
}
