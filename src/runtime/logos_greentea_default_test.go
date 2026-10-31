// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm || !logosgc.nongreentea

package runtime_test

import (
	"internal/goexperiment"
	"math"
	"runtime"
	"testing"
)

// TestLogosGreenteaDefaultContract pins the shipped GreenTea defaults
// (SPEC Sec 19, T-301.2).
func TestLogosGreenteaDefaultContract(t *testing.T) {
	if !goexperiment.GreenTeaGC {
		t.Errorf("goexperiment.GreenTeaGC = false, want true (greentea is the default collector with no opt-in)")
	}
	if runtime.GOARCH != "wasm" {
		if !goexperiment.LogosSpanDeck {
			t.Errorf("goexperiment.LogosSpanDeck = false on GOARCH=%s, want true (span deck is the default on non-wasm; wasm uses the stub)", runtime.GOARCH)
		}
	}
	if goexperiment.LogosGen {
		t.Errorf("goexperiment.LogosGen = true, want false (generational cards stay opt-in behind GOEXPERIMENT=logosgen; a clean card-enabled make.bash crashes in internal/pkgbits.DataIdx, so no default flip is claimed)")
	}

	c := runtime.NewGCController(100, math.MaxInt64)
	c.LogosStartCycleAt("greentea", 0.9, 8)
	if !c.LogosElasticActive() {
		t.Fatalf("elastic pacing is not active under greentea; the greentea default requires it with no opt-in")
	}

	d := runtime.NewGCController(100, math.MaxInt64)
	d.LogosStartCycleAt("", 0.9, 8)
	if d.LogosElasticActive() {
		t.Fatalf("elastic pacing is active under an empty test-controller mode, want upstream pacing")
	}
}
