// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/base"
)

// logosLoopStride returns the strip-mining interval for f, or zero when
// checks stay on every back edge. Positive only on wasm: the per-loop
// counter is a fix for targets without asynchronous preemption, not a
// change to native Go code generation.
func logosLoopStride(f *Func) int64 {
	if f.Config.arch != "wasm" || base.Debug.LogosLoopStride <= 0 || base.Ctxt.Pkgpath == "runtime" {
		return 0
	}
	return int64(base.Debug.LogosLoopStride)
}

// loopCounter carries one loop's strip-mining induction variable and the
// stride it counts against.
type loopCounter struct {
	phi    *Value // UInt64 phi at the loop header
	stride int64
}

// logosHeaderDominates reports whether the loop header dominates the
// back-edge source. The rewrite inserts blocks only along existing edges,
// so every path into an old block is unchanged and the snapshot dominator
// tree stays exact for this query. Strip mining reads the header counter
// from the new test block, which the header dominates only when it
// dominates the source; otherwise the edge keeps the per-iteration
// check.
func logosHeaderDominates(idom []*Block, h, bb *Block) bool {
	for b := bb; b != nil; b = idom[b.ID] {
		if b == h {
			return true
		}
	}
	return false
}

// logosLoopCounters creates one counter phi per loop header reached by a
// back edge, keyed by header. It returns nil when strip mining is off.
// A back edge into the entry block is skipped: the entry block has no
// predecessors, so there is no out-of-loop edge to initialize the
// counter on; those edges keep the per-iteration check.
func logosLoopCounters(f *Func, backEdges []edgeMem, stride int64) map[*Block]*loopCounter {
	if stride <= 0 {
		return nil
	}
	counters := make(map[*Block]*loopCounter, len(backEdges))
	for _, emc := range backEdges {
		h := emc.e.b
		if h == f.Entry || counters[h] != nil {
			continue
		}
		phi := h.NewValue0(h.Pos, OpPhi, f.Config.Types.UInt64)
		for _, p := range h.Preds {
			phi.AddArg(p.b.NewValue0I(p.b.Pos, OpConst64, f.Config.Types.UInt64, 0))
		}
		counters[h] = &loopCounter{phi: phi, stride: stride}
	}
	return counters
}

// logosLoopStrideGate installs the counted guard for one back edge:
//
//	test:
//	  if ctr == n-1 { goto sched }
//	  goto header
//	sched:
//	  resched(); ctr' = 0; goto header
//	header:
//	  ctr = phi(0 from entry, ctr+1 from test, 0 from sched)
//
// The counter value at the header is the number of completed iterations
// in the current window, so the compare is taken on iteration n and
// every n-th iteration after, and never before.
func logosLoopStrideGate(f *Func, lc *loopCounter, test, sched *Block, i int) {
	phi := lc.phi
	cfg := &f.Config.Types

	// The counter decides, not the stack limit: wasm has neither signal
	// preemption nor a sysmon thread, so the limit never moves and
	// sampling it only reads dead state.
	test.SetControl(test.NewValue2(test.Pos, OpEq64, cfg.Bool, phi,
		test.NewValue0I(test.Pos, OpConst64, cfg.UInt64, lc.stride-1)))

	// h dominates test (the header dominates every block on its back
	// edges), so the phi may be read here; the incremented value flows
	// only on the plain edge. The sched edge restarts the window at 0.
	inc := test.NewValue2(test.Pos, OpAdd64, cfg.UInt64, phi,
		test.NewValue0I(test.Pos, OpConst64, cfg.UInt64, 1))
	phi.SetArg(i, inc)
	phi.SetArg(len(phi.Args)-1, sched.NewValue0I(sched.Pos, OpConst64, cfg.UInt64, 0))
}
