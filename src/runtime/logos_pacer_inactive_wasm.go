// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm && logosgc.nongreentea

package runtime

// Inactive elastic pacer for wasm builds whose selected collector mode is a
// known mode other than greentea. Only the three hooks that mgcpacer.go calls
// exist here, with the real inactive outcomes: start keeps the upstream
// split, revise has no effect, and end keeps the upstream utilization. No
// background goal is tracked, so the state is zero-sized. Stores happen only
// on the fractional goal while the world is stopped (cycle start), which is
// why plain memory is sound here; every active atomic stays in the active
// build. No new behavior is defined for greentea: greentea never selects
// this file.

// logosFractionalGoal is plain memory for the inactive build. The active
// build aliases the atomic implementation; both expose Load and Store so the
// shared controller field in mgcpacer.go compiles in either build.
type logosFractionalGoal float64

// Load returns the goal.
func (f *logosFractionalGoal) Load() float64 {
	return float64(*f)
}

// Store sets the goal. Callers hold worldsema with the world stopped.
func (f *logosFractionalGoal) Store(v float64) {
	*f = logosFractionalGoal(v)
}

// logosElasticState is empty in the inactive build: no peak, rise, or mode
// is tracked.
type logosElasticState struct{}

// logosElasticStart keeps the upstream worker split.
func (c *gcControllerState) logosElasticStart(procs int, dedicated int64) int64 {
	return dedicated
}

// logosElasticRevise has no effect once the upstream split is kept.
func (c *gcControllerState) logosElasticRevise(live, goal uint64) {
}

// logosElasticEndUtilization keeps the upstream utilization.
func (c *gcControllerState) logosElasticEndUtilization(utilization float64, now int64, procs int) float64 {
	return utilization
}

// logosIdleMarkWorkerMax keeps the upstream idle-worker bound in the
// inactive build: maxmcount is 10,000 by default and wasm runs on a
// single P, so the upstream procs-dedicated value already fits.
func logosIdleMarkWorkerMax(procs int, dedicated int64) int32 {
	return int32(int64(procs) - dedicated)
}
