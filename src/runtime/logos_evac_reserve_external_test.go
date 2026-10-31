// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime"
	"testing"
)

// TestLogosEvacReserveDestination verifies that
// logosEvacReserveDestination grants destination spans whose total
// logosSpareSlots meets or exceeds the recorded shortfall. An accumulation
// of ns.nelems would over-count by one slot per granted span because each
// span reserves one unmarked slot for mcentral partial invariants; the
// granted >= 3 assertion distinguishes the two counts.
func TestLogosEvacReserveDestination(t *testing.T) {
	const (
		class = 3
		need  = 2 * 1344
	)
	r := runtime.LogosEvacReserveDestinationCheck(class, need)
	if !r.Eligible {
		t.Skip("scan class 3 is not immix-managed in this GC configuration")
	}
	if r.Granted == 0 {
		t.Fatalf("logosEvacReserveDestination granted 0 spans, want > 0")
	}
	if r.StaleSetSpan {
		t.Error("a granted span was already present in the target set before the reservation")
	}
	if r.BadSweepgen {
		t.Error("a granted span does not carry the current sweepgen")
	}
	if r.BadState {
		t.Error("a granted span is not mSpanInUse")
	}
	if r.BadSpareCount {
		t.Error("a granted span's logosSpareSlots differs from nelems-1")
	}
	if r.TotalSpare < need {
		t.Errorf("sum of logosSpareSlots = %d, want >= %d (short by %d)",
			r.TotalSpare, need, need-r.TotalSpare)
	}
	if r.Granted < 3 {
		t.Errorf("granted %d spans, want >= 3 (over-counting by one slot per span grants 2)",
			r.Granted)
	}
	if r.RemainingNeed != 0 {
		t.Errorf("logosEvacReserve.short = %d after reservation, want 0", r.RemainingNeed)
	}
}
