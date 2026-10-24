// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"testing"
)

// logosLoopStrideSink accumulates the benchmark black hole so the
// optimizer keeps the spinning loop while adding no observable state.
var logosLoopStrideSink int

// BenchmarkLogosLoopStride reports the amortized cost of one strip-mined
// back-edge iteration against the 0.01% T-PERF budget. One window holds
// exactly 4096 iterations, so at most one resched call lands per 64
// windows and the steady state measures the header compare plus the
// counter add: that sum, minus the same loop with preemptibleloops off
// (no check at all), is the overhead. Run it three ways on wasm and
// divide each ns/op by the no-experiment figure: per-backedge SP checks
// (stride 0) set the ceiling, strip-mined checks (stride 4096) must stay
// under 1.0001x, and the comparison across strides shows the strip
// mining paid for itself. Benchmarks never assert: the numbers are the
// record, the T-PERF row's verdict lives in the commit body with the N
// from the slice gate.
func BenchmarkLogosLoopStride(b *testing.B) {
	outer := 64
	inner := 4096
	before := uint64(testing.AllocsPerRun(1, func() {
		for i := 0; i < 1; i++ {
			runLoopStrideWindow(inner)
		}
	}))
	if before != 0 {
		b.Fatalf("stride window of %d allocated %d times, want allocation-free back edges", inner, before)
	}
	b.SetBytes(int64(outer * inner))
	b.ResetTimer()
	for b.Loop() {
		for range outer {
			runLoopStrideWindow(inner)
		}
	}
}

// runLoopStrideWindow is one counted window whose trip count matches the
// 4096 stride. The body is one multiply-add on global state: no calls, no
// allocation, nothing for bounds-check elimination to hoist except the
// loop itself, so every iteration pays exactly the header-compare cost
// the gate measures.
func runLoopStrideWindow(n int) {
	acc := logosLoopStrideSink
	for i := range n {
		acc = acc*3 + i
	}
	logosLoopStrideSink = acc
}
