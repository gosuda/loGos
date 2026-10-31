// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm || !logosgc.nongreentea

package runtime

import "internal/runtime/atomic"

// logosFractionalGoal is the fractional utilization goal storage. The active
// pacer shares the atomic implementation with the upstream controller: raises
// publish it from any P while the scheduler loads it.
type logosFractionalGoal = atomic.Float64

// Elastic background marking (M19-S2, SPEC Sec 19, T-301.2). Under the
// greentea collector the background mark utilization follows the PLAN curve
// of live heap over heap goal instead of the fixed gcBackgroundUtilization.
// Mark assist, its debt accounting and the CPU limiter are the upstream ones:
// they stay the backstop when background marking falls behind. Every other
// mode, including an empty production-controller mode, keeps upstream pacing.

const (
	// logosElasticFloor is the background utilization below logosElasticRise.
	logosElasticFloor = 0.25

	// logosElasticCeiling is the background utilization from logosElasticSaturate on.
	logosElasticCeiling = 0.75

	// logosElasticRise is the live-over-goal ratio where the curve starts to rise.
	logosElasticRise = 0.80

	// logosElasticSaturate is the live-over-goal ratio where the curve reaches
	// logosElasticCeiling and mark assist is the only remaining backstop.
	logosElasticSaturate = 0.95

	// logosElasticSlack absorbs float rounding when the curve value times
	// GOMAXPROCS lands on a whole worker count.
	logosElasticSlack = 1e-9

	// logosElasticMaxUtilization bounds the utilization endCycle derives for a
	// cycle. The curve gives background marking up to logosElasticCeiling of
	// the Ps, so background plus assist can reach 1, where the mutator share
	// 1-utilization in the cons/mark estimate vanishes.
	logosElasticMaxUtilization = 0.95
)

// logosElasticState is the elastic background marking state of one
// controller.
//
// active, procs and initial are written only when a cycle starts, with the
// world stopped. peak holds the bits of the largest background utilization
// goal seen in the cycle, in units of Ps. A non-negative float orders like its
// bits, so peak only rises, and the dedicated and fractional goals are both
// derived from one value of it. rise holds the bits of the sum over all
// raises of the raise times the P-nanoseconds from the cycle start to the
// raise; with peak it gives the time-weighted mean goal that endCycle needs.
type logosElasticState struct {
	// testMode is the collector mode of a test copy of the controller
	// (gcControllerState.test), which never reads logosGCMode: a controller
	// that tests build must not change pacing when the test binary itself is
	// linked with a collector mode. It is empty, the upstream pacing, unless
	// a test sets it.
	testMode string

	active  bool
	procs   int32
	initial float64
	peak    atomic.Uint64
	rise    atomic.Uint64
}

// logosElasticUtilization is the PLAN background utilization for live heap
// over heap goal u: logosElasticFloor below logosElasticRise, a linear rise
// to logosElasticCeiling below logosElasticSaturate, and logosElasticCeiling
// from there on.
func logosElasticUtilization(u float64) float64 {
	switch {
	case u < logosElasticRise:
		return logosElasticFloor
	case u >= logosElasticSaturate:
		return logosElasticCeiling
	}
	span := (u - logosElasticRise) / (logosElasticSaturate - logosElasticRise)
	return logosElasticFloor + (logosElasticCeiling-logosElasticFloor)*span
}

// logosElasticRatio is live over goal; a zero goal reads as an empty heap.
func logosElasticRatio(live, goal uint64) float64 {
	if goal == 0 {
		return 0
	}
	return float64(live) / float64(goal)
}

// logosElasticThreadBudget is the OS threads the collector may still mint
// without crowding the program's own parallelism out of sched.maxmcount:
// the limit minus existing Ms minus one M per P.
func logosElasticThreadBudget() int64 {
	budget := int64(sched.maxmcount) - int64(mcount()) - int64(gomaxprocs)
	if budget < 0 {
		return 0
	}
	return budget
}

// logosElasticDedicated is the number of whole dedicated workers in total
// Ps of background utilization. The grant is bounded by the thread budget:
// a dedicated worker needs an M for the whole cycle, so a user
// SetMaxThreads cap shrinks the worker count instead of throwing thread
// exhaustion inside mark enlistment.
func logosElasticDedicated(total float64) int64 {
	dedicated := int64(total + logosElasticSlack)
	if budget := logosElasticThreadBudget(); dedicated > budget {
		return budget
	}
	return dedicated
}

// logosIdleMarkWorkerMax bounds the idle-worker max for a cycle by the same
// thread budget: every worker waiting on a P can demand an M when the world
// restarts. The upstream value procs-dedicated ignores the thread limit.
func logosIdleMarkWorkerMax(procs int, dedicated int64) int32 {
	if max := int64(procs) - dedicated; max <= 0 {
		return 0
	}
	if budget := logosElasticThreadBudget(); budget < int64(procs)-dedicated {
		return int32(budget)
	}
	return int32(int64(procs) - dedicated)
}

// logosElasticFractional is the fractional goal that makes up the part of
// total that dedicated workers do not cover. A fractional worker runs only on
// a P without a dedicated worker, so the goal is per such P and the part is
// shared by procs minus dedicated Ps. The curve never exceeds 0.75 of procs,
// which keeps at least one such P. The thread budget scales the whole goal:
// a claimed worker can demand an M at every world restart, so the plan may
// claim at most budget non-dedicated Ps.
func logosElasticFractional(total float64, procs int32) float64 {
	dedicated := logosElasticDedicated(total)
	rest := total - float64(dedicated)
	if rest <= logosElasticSlack {
		return 0
	}
	nonDedicated := int64(procs) - dedicated
	if nonDedicated <= 0 {
		return 0
	}
	goal := rest / float64(nonDedicated)
	if budget := logosElasticThreadBudget() - dedicated; budget < nonDedicated {
		if budget <= 0 {
			return 0
		}
		goal *= float64(budget) / float64(nonDedicated)
	}
	return goal
}

// logosElasticMode is the collector mode that paces c: the linker's for the
// runtime's own controller, the test's choice for a test copy.
func (c *gcControllerState) logosElasticMode() string {
	if c.test {
		return c.logosElastic.testMode
	}
	return logosGCMode
}

// logosElasticTotal is the background utilization goal, in Ps, of the cycle
// at live heap over heap goal.
func (c *gcControllerState) logosElasticTotal(live, goal uint64) float64 {
	return float64(c.logosElastic.procs) * logosElasticUtilization(logosElasticRatio(live, goal))
}

// logosElasticStart replaces the worker split of a starting cycle when the
// collector mode is greentea. dedicated is the upstream split. It returns the
// number of dedicated workers to start, and sets the fractional goal for the
// rest. Plain go keeps the upstream split: an empty production-controller
// mode, an empty test-controller mode, any other mode, and stop-the-world
// collection all take the same upstream path. The caller holds worldsema
// with the world stopped.
func (c *gcControllerState) logosElasticStart(procs int, dedicated int64) int64 {
	e := &c.logosElastic
	mode := c.logosElasticMode()
	e.active = mode == "greentea" && debug.gcstoptheworld == 0
	if !e.active {
		return dedicated
	}
	e.procs = int32(procs)
	u := logosElasticRatio(c.heapLive.Load(), c.heapGoal())
	total := float64(procs) * logosElasticUtilization(u)
	e.initial = total
	e.rise.Store(0)
	e.peak.Store(float64bits(total))
	dedicated = logosElasticDedicated(total)
	c.fractionalUtilizationGoal.Store(logosElasticFractional(total, e.procs))
	if debug.gcpacertrace > 0 {
		print("pacer: elastic u=", u, " util=", total/float64(procs), " workers=", dedicated, "+", c.fractionalUtilizationGoal.Load(), "\n")
	}
	return dedicated
}

// logosElasticRevise follows the PLAN curve during the cycle. It is called
// from revise with the live heap and the heap goal before revise extends it.
// The read of the clock waits until the goal actually rises.
func (c *gcControllerState) logosElasticRevise(live, goal uint64) {
	e := &c.logosElastic
	if !e.active {
		return
	}
	total := c.logosElasticTotal(live, goal)
	if total <= float64frombits(e.peak.Load()) {
		return
	}
	c.logosElasticRaise(total, nanotime())
}

// logosElasticRaise raises the cycle's peak utilization goal to total, in Ps,
// when total is above it, at time now. It starts the dedicated workers the
// raise adds, records the raise for the cycle's mean goal, and publishes the
// fractional goal of the raised peak. The goal never falls inside a cycle,
// because running dedicated workers are not stopped.
//
// Every successful raise adds exactly the difference of the two dedicated
// counts it moves between to dedicatedMarkWorkersNeeded, so needed plus
// running dedicated workers always equals the count of the peak. All
// shared state is atomic and no lock is taken, as revise itself is lock
// free: raises that race each other are ordered by the compare-and-swap
// of peak.
func (c *gcControllerState) logosElasticRaise(total float64, now int64) {
	e := &c.logosElastic
	for {
		old := e.peak.Load()
		oldTotal := float64frombits(old)
		if oldTotal >= total {
			return
		}
		if !e.peak.CompareAndSwap(old, float64bits(total)) {
			continue
		}
		if add := logosElasticDedicated(total) - logosElasticDedicated(oldTotal); add != 0 {
			c.dedicatedMarkWorkersNeeded.Add(add)
			// The dedicated target grew, so idle mark workers must give
			// back the Ps the new dedicated workers need. Optimistic like
			// setMaxIdleMarkWorkers: running idle workers deschedule.
			c.setMaxIdleMarkWorkers(e.procs - int32(logosElasticDedicated(total)))
		}
		e.addRise((total - oldTotal) * float64(now-c.markStartTime))
		c.logosElasticPublish()
		return
	}
}

// addRise adds x to rise.
func (e *logosElasticState) addRise(x float64) {
	for {
		old := e.rise.Load()
		if e.rise.CompareAndSwap(old, float64bits(float64frombits(old)+x)) {
			return
		}
	}
}

// logosElasticPublish stores the fractional goal of the current peak, and
// stores again when a concurrent raise moved the peak meanwhile, so the last
// store always belongs to the final peak. Readers load the goal atomically; a
// reader that runs between a raise and its publication sees the goal of the
// peak before it, which is the goal the cycle ran with a moment earlier.
func (c *gcControllerState) logosElasticPublish() {
	e := &c.logosElastic
	for {
		peak := e.peak.Load()
		c.fractionalUtilizationGoal.Store(logosElasticFractional(float64frombits(peak), e.procs))
		if e.peak.Load() == peak {
			return
		}
	}
}

// mean is the time-weighted mean background utilization goal, in Ps, over the
// d nanoseconds since the cycle started. The goal is initial until the first
// raise and each raise of x at time t lifts it by x for the rest of the cycle,
// so the mean is peak minus rise over d. peak and rise are two atomics, but
// endCycle reads them with the world stopped, and a raise runs inside revise,
// whose callers (mcache.refill and the large-object paths of malloc) are not
// preemptible, so the stop waits for each raise to finish both updates. The
// result stays within [initial, peak] even when a raise lands after the
// caller read the clock, and so never leaves the range of goals the cycle ran
// with.
func (e *logosElasticState) mean(d int64) float64 {
	peak := float64frombits(e.peak.Load())
	if d <= 0 {
		return peak
	}
	return min(max(peak-float64frombits(e.rise.Load())/float64(d), e.initial), peak)
}

// logosElasticEndUtilization is endCycle's CPU utilization of the cycle that
// ended at now. utilization is upstream's: the fixed background utilization
// that endCycle assumes was met, plus the assist share. Under elastic pacing
// the background goal moved during the cycle, so the assumption is the
// time-weighted mean goal instead; without it the cons/mark estimate would
// credit the garbage collector with a quarter of the CPU while it ran with up
// to three quarters of it. The result stays below logosElasticMaxUtilization.
// Any other mode, including an empty test-controller mode, keeps upstream's value.
func (c *gcControllerState) logosElasticEndUtilization(utilization float64, now int64, procs int) float64 {
	e := &c.logosElastic
	if !e.active {
		return utilization
	}
	background := e.mean(now-c.markStartTime) / float64(procs)
	return min(utilization-gcBackgroundUtilization+background, logosElasticMaxUtilization)
}
