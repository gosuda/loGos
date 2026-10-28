// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"cmd/go/internal/cfg"
)

// logosLoopStrideFlag is the default wasm loop reschedule interval,
// passed to the compiler as a debug flag. The compiler owns the key
// (cmd/compile/internal/base/debug.go LogosLoopStride: 0 keeps
// per-backedge checks, N strip-mines every Nth iteration) and clamps it
// to 0 off wasm, so this default is inert outside logos wasm builds even
// if it ever reached another target's command line.
const logosLoopStrideFlag = "-d=logosloopstride=4096"

// logosApplyLoopStride appends the default loop-stride compiler flag for
// logos wasm builds with the preemptibleloops experiment enabled. It runs
// inside logosApplyOpt in BuildInit, so it lands in forcedGcflags ahead
// of any user -gcflags: an explicit user -d=logosloopstride (including 0
// for per-backedge checks) comes later on the compiler command line and
// wins by last-wins flag parsing, and both forcedGcflags and
// CleanGOEXPERIMENT are already part of the compile action ID, so the
// default and any override recompile instead of reusing cached output.
// The -O tier flags use disjoint -d keys, so the only coordination is
// sharing forcedGcflags order: forced first, user last.
func logosApplyLoopStride() {
	if !cfg.Logos || cfg.Goarch != "wasm" {
		return
	}
	if cfg.Experiment == nil || !cfg.Experiment.PreemptibleLoops {
		return
	}
	forcedGcflags = append(forcedGcflags, logosLoopStrideFlag)
}
