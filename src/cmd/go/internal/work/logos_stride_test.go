// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"internal/buildcfg"
	"testing"

	"cmd/go/internal/cfg"
)

func withStrideState(t *testing.T, logos bool, goarch string, exp *buildcfg.ExperimentFlags) {
	t.Helper()
	oldLogos, oldGoarch, oldExp := cfg.Logos, cfg.Goarch, cfg.Experiment
	oldForced := forcedGcflags
	t.Cleanup(func() {
		cfg.Logos, cfg.Goarch, cfg.Experiment = oldLogos, oldGoarch, oldExp
		forcedGcflags = oldForced
	})
	cfg.Logos, cfg.Goarch, cfg.Experiment = logos, goarch, exp
	forcedGcflags = nil
}

func strideExperiment(t *testing.T, goarch, goexp string) *buildcfg.ExperimentFlags {
	t.Helper()
	goos := "linux"
	if goarch == "wasm" {
		goos = "js"
	}
	exp, err := buildcfg.ParseGOEXPERIMENT(goos, goarch, goexp)
	if err != nil {
		t.Fatal(err)
	}
	return exp
}

func countStrideFlags() int {
	n := 0
	for _, f := range forcedGcflags {
		if f == logosLoopStrideFlag {
			n++
		}
	}
	return n
}

func TestLogosApplyLoopStride(t *testing.T) {
	withStrideState(t, true, "wasm", strideExperiment(t, "wasm", "preemptibleloops"))
	logosApplyLoopStride()
	if len(forcedGcflags) != 1 || forcedGcflags[0] != "-d=logosloopstride=4096" {
		t.Fatalf("logos wasm with experiment: forcedGcflags=%q, want exactly the stride default", forcedGcflags)
	}
}

func TestLogosApplyLoopStrideOff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		logos bool
		arch  string
		goexp string
	}{
		{"plain go wasm", false, "wasm", "preemptibleloops"},
		{"logos amd64", true, "amd64", "preemptibleloops"},
		{"logos wasm experiment off", true, "wasm", ""},
		{"logos wasm user opt-out", true, "wasm", "nopreemptibleloops"},
	} {
		withStrideState(t, tc.logos, tc.arch, strideExperiment(t, tc.arch, tc.goexp))
		logosApplyLoopStride()
		if n := countStrideFlags(); n != 0 {
			t.Errorf("%s: stride flag applied %d times, want none (forcedGcflags=%q)",
				tc.name, n, forcedGcflags)
		}
	}
}

func TestLogosApplyLoopStrideNilExperiment(t *testing.T) {
	withStrideState(t, true, "wasm", nil)
	logosApplyLoopStride()
	if len(forcedGcflags) != 0 {
		t.Fatalf("nil experiment: forcedGcflags=%q, want none", forcedGcflags)
	}
}
