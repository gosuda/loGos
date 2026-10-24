// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// LogosImmixStoreBarrier exposes the destination mapping independently of mode
// selection so the line-byte contract can also be tested in default builds.
func LogosImmixStoreBarrier(dst, size uintptr) {
	systemstack(func() {
		logosImmixMarkStoreRange(dst, size)
	})
}

// Test seam for M19-S8: opaque wrappers over the immix block and line
// layer for package runtime_test, which cannot name unexported members.

const (
	LogosImmixBlockBytes = logosImmixBlockBytes
	LogosImmixLineBytes  = logosImmixLineBytes
	LogosImmixLines      = logosImmixLines
)

type (
	LogosLineMark       = logosLineMark
	LogosImmixBlock     = logosImmixBlock
	LogosImmixAllocator = logosImmixAllocator
)

var logosImmixTestRegions [][]byte

// LogosImmixNewBlocks returns the base of n consecutive 32 KiB blocks of
// fresh heap memory that no block metadata has been created for yet. The
// memory stays reachable for the life of the process.
func LogosImmixNewBlocks(n int) uintptr {
	buf := make([]byte, (n+1)*logosImmixBlockBytes)
	logosImmixTestRegions = append(logosImmixTestRegions, buf)
	return alignUp(uintptr(unsafe.Pointer(&buf[0])), logosImmixBlockBytes)
}

// LogosImmixNewBlock creates the metadata of a fresh heap block.
func LogosImmixNewBlock() *LogosImmixBlock {
	return logosImmixBlockFor(LogosImmixNewBlocks(1))
}

func LogosImmixBlockFor(addr uintptr) *LogosImmixBlock { return logosImmixBlockFor(addr) }

func LogosImmixBlockLookup(addr uintptr) *LogosImmixBlock { return logosImmixBlockLookup(addr) }

func LogosImmixPinAddr(addr uintptr) bool { return logosImmixPinAddr(addr) }

// LogosImmixInHeap reports whether addr lies in a span known to the heap.
func LogosImmixInHeap(addr uintptr) bool { return spanOf(addr) != nil }

// LogosImmixHasAVX2 reports whether findHole takes the AVX2 path.
func LogosImmixHasAVX2() bool { return logosHoleScanUsable() }

func (b *logosImmixBlock) Base() uintptr { return b.base }

func (b *logosImmixBlock) Addr() uintptr { return uintptr(unsafe.Pointer(b)) }

func (b *logosImmixBlock) Pinned() bool { return b.blockPinned != 0 }

func (b *logosImmixBlock) Lines() *LogosLineMark { return &b.lines }

func (b *logosImmixBlock) MarkRange(lo, hi uintptr) { b.markRange(lo, hi) }

func (b *logosImmixBlock) ClearLines() { b.clearLines() }

func (b *logosImmixBlock) FreeLines() int { return b.freeLines() }

func (lm *logosLineMark) FindHole(from int) (start, end int, ok bool) { return lm.findHole(from) }

// FindHoleGo runs findHole through the portable Go scan.
func (lm *logosLineMark) FindHoleGo(from int) (start, end int, ok bool) {
	h := logosHolesGo(lm)
	return h.next(from)
}

// FindHoleAVX2 runs findHole through the AVX2 scan; callers must check
// LogosImmixHasAVX2 first.
func (lm *logosLineMark) FindHoleAVX2(from int) (start, end int, ok bool) {
	h := logosHoleScanAVX2(lm)
	return h.next(from)
}

func (a *logosImmixAllocator) Reset(b *LogosImmixBlock) { a.reset(b) }

func (a *logosImmixAllocator) Alloc(size uintptr) uintptr { return a.alloc(size) }

func (a *logosImmixAllocator) Cursor() uintptr { return a.cursor }

func (a *logosImmixAllocator) Limit() uintptr { return a.limit }

func (a *logosImmixAllocator) Next() int { return a.next }
