// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	. "runtime"
	"runtime/debug"
	"testing"
)

// TestLogosZgcQuarantineStampHeldReleasedPageAcrossCycles pins the
// quarantine generation contract at its readers: a real quarantined
// source span of a held released page must carry the pre-bump sweepgen
// stamp — and stay reported as quarantined — across whole GC cycles,
// not only inside the pauses. The event order mirrors production:
// quarantine in the selection pause, release to the OS, then full
// runtime GC cycles whose mark-termination pauses refresh, drain,
// publish and bump mheap_.sweepgen, with gcSweep re-stamping right
// after its bump.
//
// Each post-GC probe reads the exact predicates the gcStart walk
// (skipSpan), the sweeper (bypass), the scan admission and findObject
// share: the span must still report quarantined and must not read as
// swept at the current generation while it awaits retire. Before the
// gcSweep-side refresh, the bump left the stamp equal to the current
// sweepgen, so the membership gate failed and the span read as swept —
// the walker admitted the MADV'ed source and the sweeper treated it as
// ordinary. The retained span is the scanned 1024-byte class (scanned,
// above MinSizeForMallocHeader), the class whose slots the walker
// admits through the malloc-header path.
func TestLogosZgcQuarantineStampHeldReleasedPageAcrossCycles(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run := LogosZgcAllocRun([]int{LogosZgcRunHeapScan})
	if run == nil {
		t.Fatal("no tight scanned span run in one 2 MB region after 256 trials")
	}
	src := run.Base(0)
	region := src &^ (LogosZgcPageSize - 1)
	pg := LogosZgcNewPage(region, 0, 1)
	LogosZgcRecordSources(pg, run, []int{0})
	var eligible bool
	LogosZgcWithWorldStopped(func() { eligible = LogosZgcEligible(pg) })
	if !eligible {
		t.Fatal("single-source scanned page refused by the selection vet")
	}
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
		run.Free(0)
	})

	LogosZgcWithWorldStopped(func() { LogosZgcQuarantineSource(pg, run, 0) })
	if !LogosZgcSpanQuarantinedAt(src) {
		t.Fatal("freshly quarantined span is not reported as quarantined")
	}
	if LogosZgcSpanSwept(src) {
		t.Fatal("quarantined span reads as swept before any sweepgen bump")
	}

	if !LogosZgcRelease(pg) {
		t.Fatal("release refused a page with no unpublished live bytes")
	}
	LogosZgcRemapDone()

	// Retention reader: hold a page lease across the real mark
	// termination pauses, then drop it validly before the retire.
	leased := LogosZgcPageAcquire(src)
	if leased == nil {
		t.Fatal("acquire missed the quarantined page's own source address")
	}
	defer func() {
		if leased != nil {
			LogosZgcPageRelease(leased)
		}
	}()

	for cycle := 1; cycle <= 2; cycle++ {
		GC()
		if !LogosZgcSpanQuarantinedAt(src) {
			t.Fatalf("after GC cycle %d the held released quarantined span lost its membership stamp", cycle)
		}
		if LogosZgcSpanSwept(src) {
			t.Fatalf("after GC cycle %d the quarantined span reads as swept at the current generation", cycle)
		}
	}

	LogosZgcPageRelease(leased)
	leased = nil

	var retiredOK bool
	LogosZgcWithWorldStopped(func() { retiredOK = LogosZgcRetire(pg) })
	if !retiredOK {
		t.Fatal("retire did not reclaim the released page after the lease dropped")
	}
	if LogosZgcSpanQuarantinedAt(src) {
		t.Fatal("retired span still reports as quarantined")
	}
	if LogosZgcPageFor(src) != nil || !LogosZgcPageSlotFree(region) {
		t.Fatal("retired page still resolves through the directory")
	}
	run.MarkFreed(0)
	retired = true
}
