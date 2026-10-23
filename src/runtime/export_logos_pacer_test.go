// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Test seam for M19-S2: elastic background marking, observed through the
// controller copy that export_test.go builds. A test copy takes its
// collector mode from LogosStartCycleAt, never from logosGCMode, so the
// seam changes no process-wide state.

// LogosElasticUtilization is the PLAN background utilization curve.
func LogosElasticUtilization(u float64) float64 {
	return logosElasticUtilization(u)
}

// LogosPacerSetMode sets the collector mode of the whole process under
// worldsema, so that no cycle start reads it half written, and returns the
// previous mode. Only the benchmark uses it, because it needs real
// collections under a mode.
func LogosPacerSetMode(mode string) (old string) {
	semacquire(&worldsema)
	old = logosGCMode
	logosGCMode = mode
	semrelease(&worldsema)
	return old
}

// LogosStartCycleAt starts a cycle under the collector mode with the live
// heap at u times the heap goal.
func (c *GCController) LogosStartCycleAt(mode string, u float64, gomaxprocs int) {
	c.logosElastic.testMode = mode
	c.setLiveAt(u)
	c.startCycle(0, gomaxprocs, gcTrigger{kind: gcTriggerHeap})
}

// LogosReviseAt moves the live heap to u times the heap goal and revises.
func (c *GCController) LogosReviseAt(u float64) {
	c.setLiveAt(u)
	c.revise()
}

func (c *GCController) setLiveAt(u float64) {
	c.heapLive.Store(uint64(u*float64(c.heapGoal()) + 0.5))
}

// LogosLiveOverGoal reports the live heap over the heap goal.
func (c *GCController) LogosLiveOverGoal() float64 {
	return float64(c.heapLive.Load()) / float64(c.heapGoal())
}

// LogosBackgroundUtilization is the background mark utilization the
// controller delivers: the dedicated workers, started or still needed, plus
// the fractional goal on each of the other Ps, over procs.
func (c *GCController) LogosBackgroundUtilization(gomaxprocs int, started int64) float64 {
	dedicated := float64(c.dedicatedMarkWorkersNeeded.Load() + started)
	return (dedicated + c.fractionalUtilizationGoal.Load()*(float64(gomaxprocs)-dedicated)) / float64(gomaxprocs)
}

// LogosElasticStepAt runs the revise hook with the live heap at u times the
// heap goal, without storing the live heap, and reports the ratio it used.
func (c *GCController) LogosElasticStepAt(u float64) float64 {
	goal := c.heapGoal()
	live := uint64(u*float64(goal) + 0.5)
	c.logosElasticRevise(live, goal)
	return float64(live) / float64(goal)
}

// LogosElasticStepAtTime is LogosElasticStepAt with the raise taking place
// at now nanoseconds after the cycle start, which is time 0 for a test copy.
func (c *GCController) LogosElasticStepAtTime(u float64, now int64) float64 {
	goal := c.heapGoal()
	live := uint64(u*float64(goal) + 0.5)
	c.logosElasticRaise(c.logosElasticTotal(live, goal), now)
	return float64(live) / float64(goal)
}

// LogosAddWork adds allocation and scan work to the running cycle without
// revising, so that a cycle's cons/mark inputs are exact and no assist
// estimate runs on a controller whose scan expectation is empty.
func (c *GCController) LogosAddWork(heapLive, scanWork int64) {
	c.heapLive.Add(heapLive)
	c.heapScanWork.Add(scanWork)
}

// LogosCycleUtilization is the utilization that endCycle derives for a
// cycle that ended at now when no assist ran.
func (c *GCController) LogosCycleUtilization(now int64, gomaxprocs int) float64 {
	return c.logosElasticEndUtilization(gcBackgroundUtilization, now, gomaxprocs)
}

// LogosConsMark is the cons/mark estimate of the last EndCycle.
func (c *GCController) LogosConsMark() float64 {
	return c.consMark
}

// LogosDedicatedTarget is the elastic dedicated worker target of the
// running cycle.
func (c *GCController) LogosDedicatedTarget() int64 {
	return logosElasticDedicated(float64frombits(c.logosElastic.peak.Load()))
}

// LogosElasticActive reports whether the running cycle paces elastically.
func (c *GCController) LogosElasticActive() bool {
	return c.logosElastic.active
}

// LogosDedicatedNeeded is the number of dedicated workers still to start.
func (c *GCController) LogosDedicatedNeeded() int64 {
	return c.dedicatedMarkWorkersNeeded.Load()
}

// LogosFractionalGoal is the fractional utilization goal.
func (c *GCController) LogosFractionalGoal() float64 {
	return c.fractionalUtilizationGoal.Load()
}

// LogosStartDedicated takes one dedicated worker the way
// assignWaitingGCWorker does.
func (c *GCController) LogosStartDedicated() bool {
	for {
		v := c.dedicatedMarkWorkersNeeded.Load()
		if v <= 0 {
			return false
		}
		if c.dedicatedMarkWorkersNeeded.CompareAndSwap(v, v-1) {
			return true
		}
	}
}

// LogosStopDedicated stops one dedicated worker the way markWorkerStop
// does.
func (c *GCController) LogosStopDedicated() {
	c.markWorkerStop(gcMarkWorkerDedicatedMode, 0)
}
