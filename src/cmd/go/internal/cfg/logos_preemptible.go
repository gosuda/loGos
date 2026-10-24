// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

import "strings"

// logosPreemptibleLoopsExperiment is the GOEXPERIMENT name that enables
// the SSA insert-resched-checks pass (ssa/compile.go, gated by
// buildcfg.Experiment.PreemptibleLoops). M15-S3 makes it the default for
// logos wasm builds; plain go accepts it through GOEXPERIMENT but does
// not default it on until G3.
const logosPreemptibleLoopsExperiment = "preemptibleloops"

// RefreshLogosPreemptible applies the logos wasm default for the
// preemptibleloops experiment. Under logos with GOARCH=wasm it enables
// the experiment unless the startup GOEXPERIMENT mentions it:
// preemptibleloops, nopreemptibleloops, and none each count as an
// explicit user choice and are left alone, so an explicit user opt-out
// is a default, never a force. While the target wants the default the
// refresh only appends; once the target stops wanting it (any non-wasm
// retarget) it strips exactly the tokens it could have added, which is
// safe because the user never mentioned the experiment. That makes the
// refresh idempotent and per-target: a host retarget drops the wasm
// default but keeps an explicit user preemptibleloops.
//
// The mutation (RawGOEXPERIMENT plus the process and tool environments)
// mirrors RefreshLogosArena: the compiler the go command spawns reads
// GOEXPERIMENT from its environment, so the default must be published,
// not just computed. CleanGOEXPERIMENT and the experiment tool tags are
// recomputed with it, so the build cache keys the default like any
// other experiment setting.
//
// Callers: RefreshLogosArena chains it after the arena composition, and
// ForceHost calls it after retargeting so a host tool build never
// inherits the wasm default.
func RefreshLogosPreemptible() {
	if ExperimentErr != nil {
		return
	}
	mentioned, _ := logosPreemptibleUserState(logosInitGOEXPERIMENT)
	want := Logos && Goarch == "wasm" && !mentioned
	has := Experiment.PreemptibleLoops
	switch {
	case want && !has:
		logosApplyRawExperiment(appendExperiment(RawGOEXPERIMENT, logosPreemptibleLoopsExperiment))
	case !want && has && !mentioned:
		// Only our own earlier append can have enabled it: the user
		// never mentioned the experiment, so every token is ours.
		var kept []string
		for f := range strings.SplitSeq(RawGOEXPERIMENT, ",") {
			if f != logosPreemptibleLoopsExperiment {
				kept = append(kept, f)
			}
		}
		logosApplyRawExperiment(strings.Join(kept, ","))
	}
}

// logosPreemptibleUserState reports whether raw mentions the
// preemptibleloops experiment and the final state it selects, following
// GOEXPERIMENT order semantics: later tokens win, and none clears.
func logosPreemptibleUserState(raw string) (mentioned, enabled bool) {
	for f := range strings.SplitSeq(raw, ",") {
		switch f {
		case logosPreemptibleLoopsExperiment:
			mentioned, enabled = true, true
		case "no" + logosPreemptibleLoopsExperiment, "none":
			mentioned, enabled = true, false
		}
	}
	return mentioned, enabled
}
