// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgen

package runtime

import "unsafe"

// Test seams for M19-S4: opaque wrappers over the generational collector
// for package runtime_test, which cannot name spans or the card table.

// LogosGenActive reports whether the process runs the generational collector.
func LogosGenActive() bool { return logosGen.active }

// LogosGenCardBytes returns the bytes of heap one card covers.
func LogosGenCardBytes() uintptr { return logosGenCardBytes }

// LogosGenCardTableBytes returns the card table size of one heap arena.
func LogosGenCardTableBytes() uintptr { return unsafe.Sizeof(logosGenCardTable{}) }

// LogosGenArenaBytes returns the heap bytes one arena maps.
func LogosGenArenaBytes() uintptr { return heapArenaBytes }

// LogosGenMatureAge returns the cycles a span must survive to be mature.
func LogosGenMatureAge() int { return logosGenMatureAge }

// LogosGenFullEvery returns the minor cycles that precede a full cycle.
func LogosGenFullEvery() int { return logosGenFullEvery }

// LogosGenCardAddr returns the address of the card byte of the heap address
// addr, or 0 when addr is outside the heap.
func LogosGenCardAddr(addr uintptr) uintptr {
	return uintptr(unsafe.Pointer(logosGenCardFor(addr)))
}

// LogosGenHeapTrigger returns the heap size that starts a cycle.
func LogosGenHeapTrigger(pacer, marked, minimum uint64) uint64 {
	return logosGenTriggerFor(pacer, marked, minimum)
}

// LogosGenMinorCycle runs one collection cycle that is forced to be minor.
func LogosGenMinorCycle() {
	logosGen.test = logosGenTestMinor
	GC()
	logosGen.test = logosGenTestNone
}

// LogosGenAutoCycle runs one collection cycle whose kind follows the
// minor-cycle counter, as a heap-triggered cycle's does.
func LogosGenAutoCycle() {
	logosGen.test = logosGenTestAuto
	GC()
	logosGen.test = logosGenTestNone
}

// LogosGenSpanAge returns the survived-cycle count of the span holding p,
// or -1 when p is not in a heap span.
func LogosGenSpanAge(p unsafe.Pointer) int {
	s := spanOf(uintptr(p))
	if s == nil || s.state.get() != mSpanInUse {
		return -1
	}
	return int(s.logosAge)
}

// LogosGenStoreFlush stores v into *slot, flushes this P's write barrier
// buffer, and reports whether the card of slot is dirty.
func LogosGenStoreFlush(slot *unsafe.Pointer, v unsafe.Pointer) bool {
	mp := acquirem()
	*slot = v
	systemstack(func() { wbBufFlush1(mp.p.ptr()) })
	dirty := logosGenCardDirty(uintptr(unsafe.Pointer(slot)))
	releasem(mp)
	return dirty
}

// LogosGenHistory returns the kinds of the cycles since the last reset,
// oldest first: 'm' for a minor cycle and 'F' for a full cycle.
func LogosGenHistory() []byte { return logosGenHistory() }

// LogosGenCycles returns the number of cycles recorded since the last reset.
func LogosGenCycles() int { return int(logosGen.histN) }

// LogosGenResetHistory forgets the recorded cycle kinds.
func LogosGenResetHistory() { logosGen.resetHistory() }

// LogosGenPromoteObjectForTest clears obj's card, runs the generational
// promote scan for the object at obj, and reports whether obj's card is
// dirty afterwards. It exercises logosGenPromoteObject's young-pointer
// predicate (logosAge < logosGenMatureAge): an age-1 pointee must dirty
// the card, which fails under the old age==0 predicate.
func LogosGenPromoteObjectForTest(obj unsafe.Pointer) bool {
	addr := uintptr(obj)
	if c := logosGenCardFor(addr); c != nil {
		*c = 0
	}
	s := spanOf(addr)
	if s == nil {
		return false
	}
	logosGenPromoteObject(s, addr)
	return logosGenCardDirty(addr)
}

// LogosGenDrainScratchForTest writes the tagged slot address obj into the
// current M's no-P scratch buffer, drains it through the production
// logosGenDrainMScratch path, and reports whether obj's card is dirty and
// every scratch word is zero afterwards.
func LogosGenDrainScratchForTest(obj unsafe.Pointer) bool {
	mp := acquirem()
	mp.logosWbScratch[0] = uintptr(obj) | logosGenSlotTag
	logosGenDrainMScratch(mp)
	dirty := logosGenCardDirty(uintptr(obj))
	clean := true
	for _, w := range mp.logosWbScratch {
		if w != 0 {
			clean = false
			break
		}
	}
	releasem(mp)
	return dirty && clean
}
