// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// LogosEvacReserveResult records the outcome of one
// logosEvacReserveDestination pass for the runtime_test selector
// TestLogosEvacReserveDestination.
type LogosEvacReserveResult struct {
	Eligible      bool
	Granted       int
	TotalSpare    uintptr
	RemainingNeed uintptr
	StaleSetSpan  bool
	BadSweepgen   bool
	BadState      bool
	BadSpareCount bool
}

// are restored.
func LogosEvacReserveDestinationCheck(class uint8, need uintptr) (r LogosEvacReserveResult) {
	sc := makeSpanClass(class, false)

	prevImmix := logosImmixOn
	logosImmixOn = 1
	r.Eligible = logosImmixSpanClass(sc)
	if !r.Eligible {
		logosImmixOn = prevImmix
		return r
	}
	savedShort := logosEvacReserve.short
	defer func() {
		logosImmixOn = prevImmix
		logosEvacReserve.short = savedShort
	}()

	stw := stopTheWorld(stwForTestForeignRetain)
	defer startTheWorld(stw)

	sg := mheap_.sweepgen
	targetSet := mheap_.central[sc].mcentral.fullUnswept(sg + 2)

	var drained []*mspan
	for s := targetSet.pop(); s != nil; s = targetSet.pop() {
		drained = append(drained, s)
	}
	var granted []*mspan
	defer func() {
		for _, s := range drained {
			targetSet.push(s)
		}
		for _, s := range granted {
			targetSet.push(s)
		}
	}()

	logosEvacReserve.short[sc] = need
	logosEvacReserveDestination()

	for s := targetSet.pop(); s != nil; s = targetSet.pop() {
		granted = append(granted, s)
	}

	drainedSet := make(map[*mspan]bool, len(drained))
	for _, s := range drained {
		drainedSet[s] = true
	}
	for _, s := range granted {
		if drainedSet[s] {
			r.StaleSetSpan = true
		}
		if s.sweepgen != sg {
			r.BadSweepgen = true
		}
		if s.state.get() != mSpanInUse {
			r.BadState = true
		}
		spare := logosSpareSlots(s)
		if spare != uintptr(s.nelems)-1 {
			r.BadSpareCount = true
		}
		r.TotalSpare += spare
	}
	r.Granted = len(granted)
	r.RemainingNeed = logosEvacReserve.short[sc]
	return r
}
