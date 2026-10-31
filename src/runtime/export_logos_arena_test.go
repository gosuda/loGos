// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosarena

package runtime

import "unsafe"

// Test seam for M18-S1: opaque wrappers over the arena allocator, the
// watermark pair, and the chunk bookkeeping for package runtime_test, which
// cannot name g, p, or sideStackChunk.

// LogosArenaMark is a saved arena watermark.
type LogosArenaMark = logosArenaMark

const (
	// LogosArenaChunkBytes is the chunk size.
	LogosArenaChunkBytes = logosArenaChunkBytes
	// LogosArenaCapacity is the noscan capacity of a fresh chunk.
	LogosArenaCapacity = logosArenaChunkBytes - logosArenaHeaderBytes
	// LogosArenaUnit is the scan-region size and alignment unit.
	LogosArenaUnit = logosArenaUnit
	// LogosArenaScanHeaderBytes is the size of the header before a scan object.
	LogosArenaScanHeaderBytes = logosArenaScanHeaderBytes
)

// LogosArenaInfo describes the calling goroutine's newest chunk.
type LogosArenaInfo struct {
	Chunks     int
	NoscanUsed uintptr
	ScanUsed   uintptr
	Free       uintptr
}

func logosEfaceType(elem any) *_type {
	return efaceOf(&elem)._type
}

// LogosArenaAlloc calls logosArenaAlloc with the dynamic type of elem.
func LogosArenaAlloc(elem any, size uintptr) unsafe.Pointer {
	return logosArenaAlloc(logosEfaceType(elem), size)
}

// LogosArenaMakeSlice calls logosArenaMakeSlice with the dynamic type of elem.
func LogosArenaMakeSlice(elem any, len64, cap64 int64) unsafe.Pointer {
	return logosArenaMakeSlice(logosEfaceType(elem), len64, cap64)
}

// LogosArenaMaxAlloc is maxAlloc, the largest size makeslice accepts.
const LogosArenaMaxAlloc uintptr = maxAlloc

// LogosArenaAllocScan calls logosArenaAllocScan with the dynamic type of elem.
func LogosArenaAllocScan(elem any, size uintptr) unsafe.Pointer {
	return logosArenaAllocScan(logosEfaceType(elem), size)
}

// LogosArenaSave saves the calling goroutine's watermark.
func LogosArenaSave() LogosArenaMark {
	return logosArenaSave()
}

// LogosArenaRestore pops the calling goroutine's arena back to m.
func LogosArenaRestore(m LogosArenaMark) {
	logosArenaRestore(m)
}

// LogosArenaMarkSave saves the calling goroutine's watermark as the three
// words the compiler's inline bump spills: chunk, noscan, scan.
func LogosArenaMarkSave() (chunk, noscan, scan uintptr) {
	return logosArenaMarkSave()
}

// LogosArenaMarkRestore pops the calling goroutine's arena back to the
// watermark the three words describe.
func LogosArenaMarkRestore(chunk, noscan, scan uintptr) {
	logosArenaMarkRestore(chunk, noscan, scan)
}

// LogosArenaRetSet stores the consent token for the next candidate callee.
func LogosArenaRetSet(ok bool) {
	logosArenaRetSet(ok)
}

// LogosArenaRetTake returns and clears the consent token.
func LogosArenaRetTake() bool {
	return logosArenaRetTake()
}

// LogosArenaFieldOffset is unsafe.Offsetof(g{}.logosArena), exported so
// package runtime_test can pin the offset the compiler's inline bump
// hard-codes (TestLogosArenaFieldOffset).
var LogosArenaFieldOffset = unsafe.Offsetof(g{}.logosArena)

// LogosArenaStat reports the calling goroutine's chunk chain.
func LogosArenaStat() LogosArenaInfo {
	var info LogosArenaInfo
	c := getg().logosArena
	for n := c; n != nil; n = n.next {
		info.Chunks++
	}
	if c != nil {
		info.NoscanUsed = c.noscanCur - c.noscanBase
		info.ScanUsed = c.scanBase - c.scanCur
		info.Free = c.scanCur - c.noscanCur
	}
	return info
}

// LogosArenaChunk describes one chunk of a goroutine's chain.
type LogosArenaChunk struct {
	Addr       uintptr
	NoscanUsed uintptr
	ScanUsed   uintptr
}

// LogosArenaGoid is the calling goroutine's id.
func LogosArenaGoid() uint64 {
	return getg().goid
}

// LogosArenaChainOf stops the world and copies goroutine goid's chunk chain,
// newest first, into buf. It returns the chain length, which may exceed
// len(buf), and whether the goroutine is waiting. gopark runs the park hook
// before park_m sets the waiting status, so a waiting goroutine has already run
// the hook. An unknown goid yields 0 and false. It does not allocate.
func LogosArenaChainOf(goid uint64, buf []LogosArenaChunk) (n int, waiting bool) {
	stw := stopTheWorld(stwForTestCountPagesInUse)
	forEachG(func(gp *g) {
		if gp.goid != goid {
			return
		}
		waiting = readgstatus(gp)&^_Gscan == _Gwaiting
		for c := gp.logosArena; c != nil; c = c.next {
			if n < len(buf) {
				buf[n] = LogosArenaChunk{
					Addr:       uintptr(unsafe.Pointer(c)),
					NoscanUsed: c.noscanCur - c.noscanBase,
					ScanUsed:   c.scanBase - c.scanCur,
				}
			}
			n++
		}
	})
	startTheWorld(stw)
	return n, waiting
}

// LogosArenaEmptyChunk empties in place the chunk at addr of the calling
// goroutine's chain: its objects are discarded and its watermarks return to
// the empty state, while the chunks linked after it stay. Save, Restore, and
// allocation cannot reach this state for a chunk below the head: a restore
// pops the chunks above its mark, and a chunk is linked only above a head that
// holds an object. The seam builds it so the park rule for empty chunks behind
// a live chunk can be exercised.
func LogosArenaEmptyChunk(addr uintptr) bool {
	for c := getg().logosArena; c != nil; c = c.next {
		if uintptr(unsafe.Pointer(c)) == addr {
			c.popScan(c.scanBase)
			c.noscanCur = c.noscanBase
			return true
		}
	}
	return false
}

// LogosArenaCounts stops the world and reports the chunks that exist, the
// chunks leased to goroutines, and the chunks cached in Ps.
func LogosArenaCounts() (total, leased, pooled int) {
	stw := stopTheWorld(stwForTestCountPagesInUse)
	total = int(logosArenaChunkTotal.Load())
	forEachG(func(gp *g) {
		for c := gp.logosArena; c != nil; c = c.next {
			leased++
		}
	})
	for _, pp := range allp {
		pooled += int(pp.logosArenaPoolN)
	}
	startTheWorld(stw)
	return
}

// LogosArenaPoolCounts stops the world and reports the chunks cached in each
// P, indexed by P id.
func LogosArenaPoolCounts() []int {
	stw := stopTheWorld(stwForTestCountPagesInUse)
	counts := make([]int, len(allp))
	for i, pp := range allp {
		counts[i] = int(pp.logosArenaPoolN)
	}
	startTheWorld(stw)
	return counts
}

// LogosArenaSetLimit sets the chunk limit and returns the previous one.
func LogosArenaSetLimit(n int64) int64 {
	return logosArenaChunkLimit.Swap(n)
}

// LogosArenaInChunk reports whether p points into an arena chunk.
func LogosArenaInChunk(p unsafe.Pointer) bool {
	return inLogosArenaChunk(uintptr(p))
}

// LogosArenaSpanKind reports whether the span of p is a manual span and
// whether it carries the arena chunk flag.
func LogosArenaSpanKind(p unsafe.Pointer) (manual, flagged bool) {
	s := spanOf(uintptr(p))
	if s == nil {
		return false, false
	}
	return s.state.get() == mSpanManual, s.isLogosArenaChunk
}

// LogosArenaType returns the type word of the dynamic type of elem.
func LogosArenaType(elem any) unsafe.Pointer {
	return unsafe.Pointer(logosEfaceType(elem))
}

// LogosArenaScanHeader reads the header before the scan object at p.
func LogosArenaScanHeader(p unsafe.Pointer) (typ unsafe.Pointer, size uintptr) {
	h := (*logosArenaScanHeader)(unsafe.Pointer(uintptr(p) - logosArenaScanHeaderBytes))
	return unsafe.Pointer(h.typ), h.size
}

// LogosArenaIsGoPointer reports the cgo pointer check's view of p.
func LogosArenaIsGoPointer(p unsafe.Pointer) bool {
	return cgoIsGoPointer(p)
}

// LogosArenaCgoCheckPointer calls cgoCheckPointer.
func LogosArenaCgoCheckPointer(ptr, arg any) {
	cgoCheckPointer(ptr, arg)
}

// LogosArenaCgoCheckResult calls cgoCheckResult.
func LogosArenaCgoCheckResult(val any) {
	cgoCheckResult(val)
}

// LogosArenaCgoCheckPtrWrite calls cgoCheckPtrWrite.
func LogosArenaCgoCheckPtrWrite(dst *unsafe.Pointer, src unsafe.Pointer) {
	cgoCheckPtrWrite(dst, src)
}

// LogosArenaCgoCheckMemmove calls cgoCheckMemmove2 over the whole of the
// dynamic type of elem.
func LogosArenaCgoCheckMemmove(elem any, dst, src unsafe.Pointer) {
	typ := logosEfaceType(elem)
	cgoCheckMemmove2(typ, dst, src, 0, typ.Size_)
}
