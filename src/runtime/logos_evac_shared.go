// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Declarations shared by the untagged evacuation state (logos_evac.go) and
// both moving-collector drivers. They stay untagged: the logosEvac struct
// and the untagged test seams name them in every build configuration, while
// the drivers that use them carry their own build tags.

// logosPagesPerBlock is the number of heap pages in one immix block.
const logosPagesPerBlock = logosImmixBlockBytes / pageSize

// logosDestCursor walks the destination spans of one span class: the span
// index in the pool and the next object index to examine in that span.
type logosDestCursor struct {
	cur    uintptr
	end    uintptr
	obj    uintptr
	budget uintptr
	open   bool
}

// logosEvacTotals counts evacuation work across cycles. It is written with
// the world stopped.
var logosEvacTotals struct {
	cycles  uint64
	blocks  uint64
	objects uint64
	bytes   uint64
}
