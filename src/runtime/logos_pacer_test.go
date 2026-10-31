// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm || !logosgc.nongreentea

package runtime_test

import (
	"math"
	"math/rand/v2"
	"runtime"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// M19-S2 attacks (SPEC Sec 19, T-301.2, #161). Under the greentea mode the
// background utilization goal follows the PLAN curve of live heap over
// heap goal, mark assist stays as backstop, endCycle credits the cycle
// with the goal it ran with, and an empty test-controller mode keeps the
// upstream pacing.

const (
	logosPlanFloor   = 0.25
	logosPlanCeiling = 0.75
	logosPlanRise    = 0.80
	logosPlanTop     = 0.95
	logosPlanSlack   = 1e-6
)

var logosPlanPoints = []struct{ u, util float64 }{
	{0.79, 0.25},
	{0.80, 0.25},
	{0.875, 0.50},
	{0.95, 0.75},
	{0.96, 0.75},
}

var logosPlanProcs = []int{1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 64}

func logosPlan(u float64) float64 {
	switch {
	case u < logosPlanRise:
		return logosPlanFloor
	case u >= logosPlanTop:
		return logosPlanCeiling
	}
	return logosPlanFloor + (logosPlanCeiling-logosPlanFloor)*(u-logosPlanRise)/(logosPlanTop-logosPlanRise)
}

func logosNearly(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

func newLogosController() *runtime.GCController {
	return runtime.NewGCController(100, math.MaxInt64)
}

const logosGreentea = "greentea"

func TestLogosElasticCurve(t *testing.T) {
	for _, p := range logosPlanPoints {
		if got := runtime.LogosElasticUtilization(p.u); !logosNearly(got, p.util, 1e-9) {
			t.Errorf("elastic(%v) = %v, want %v", p.u, got, p.util)
		}
	}
	prev := runtime.LogosElasticUtilization(0)
	for i := 1; i <= 12000; i++ {
		u := float64(i) * 1e-4
		got := runtime.LogosElasticUtilization(u)
		if want := logosPlan(u); !logosNearly(got, want, 1e-12) {
			t.Fatalf("elastic(%v) = %v, want %v", u, got, want)
		}
		if got < logosPlanFloor || got > logosPlanCeiling {
			t.Fatalf("elastic(%v) = %v outside [%v, %v]", u, got, logosPlanFloor, logosPlanCeiling)
		}
		if got < prev {
			t.Fatalf("elastic is not monotone at u=%v: %v after %v", u, got, prev)
		}
		if got-prev > 1e-4*(logosPlanCeiling-logosPlanFloor)/(logosPlanTop-logosPlanRise)+1e-12 {
			t.Fatalf("elastic jumps at u=%v: %v after %v", u, got, prev)
		}
		prev = got
	}
}

func TestLogosElasticGoalAtCycleStart(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	for _, procs := range logosPlanProcs {
		for _, p := range logosPlanPoints {
			c := newLogosController()
			c.LogosStartCycleAt(logosGreentea, p.u, procs)
			if !c.LogosElasticActive() {
				t.Fatalf("procs=%d u=%v: elastic pacing is not active under greentea", procs, p.u)
			}
			got := c.LogosBackgroundUtilization(procs, 0)
			if want := logosPlan(c.LogosLiveOverGoal()); !logosNearly(got, want, 1e-9) {
				t.Errorf("procs=%d u=%v: utilization %v, want curve value %v", procs, c.LogosLiveOverGoal(), got, want)
			}
			if !logosNearly(got, p.util, logosPlanSlack) {
				t.Errorf("procs=%d u=%v: utilization %v, want %v", procs, p.u, got, p.util)
			}
		}
	}
}

func TestLogosElasticGoalFollowsReviseInCycle(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	for _, procs := range logosPlanProcs {
		for _, p := range logosPlanPoints {
			c := newLogosController()
			c.LogosStartCycleAt(logosGreentea, 0.5, procs)
			if got := c.LogosBackgroundUtilization(procs, 0); !logosNearly(got, logosPlanFloor, 1e-9) {
				t.Fatalf("procs=%d: utilization at u=0.5 is %v, want %v", procs, got, logosPlanFloor)
			}
			c.LogosReviseAt(p.u)
			got := c.LogosBackgroundUtilization(procs, 0)
			if want := logosPlan(c.LogosLiveOverGoal()); !logosNearly(got, want, 1e-9) {
				t.Errorf("procs=%d u=%v: utilization %v, want curve value %v", procs, c.LogosLiveOverGoal(), got, want)
			}
			if !logosNearly(got, p.util, logosPlanSlack) {
				t.Errorf("procs=%d u=%v: utilization %v, want %v", procs, p.u, got, p.util)
			}
		}
	}
}

func TestLogosElasticNeverLowersInCycle(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	const procs = 8
	c := newLogosController()
	c.LogosStartCycleAt(logosGreentea, 0.5, procs)
	c.LogosReviseAt(0.96)
	peak := c.LogosBackgroundUtilization(procs, 0)
	if !logosNearly(peak, logosPlanCeiling, 1e-9) {
		t.Fatalf("utilization at u=0.96 is %v, want %v", peak, logosPlanCeiling)
	}
	for _, u := range []float64{0.9, 0.5, 0.0} {
		c.LogosReviseAt(u)
		if got := c.LogosBackgroundUtilization(procs, 0); got < peak-1e-9 {
			t.Errorf("utilization fell to %v at u=%v after reaching %v", got, u, peak)
		}
	}
	c.LogosStartCycleAt(logosGreentea, 0.5, procs)
	if got := c.LogosBackgroundUtilization(procs, 0); !logosNearly(got, logosPlanFloor, 1e-9) {
		t.Errorf("a new cycle starts at utilization %v, want %v", got, logosPlanFloor)
	}
}

func TestLogosElasticWorkerAccounting(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	rng := rand.New(rand.NewPCG(19, 2))
	for _, procs := range []int{2, 3, 4, 8, 12, 16} {
		c := newLogosController()
		c.LogosStartCycleAt(logosGreentea, 0.4, procs)
		var running int64
		var peakU, lastTarget = 0.0, int64(0)
		limit := int64(float64(procs)*logosPlanCeiling + 1e-6)
		for step := range 4000 {
			switch rng.IntN(3) {
			case 0:
				u := rng.Float64() * 1.2
				c.LogosReviseAt(u)
				peakU = math.Max(peakU, c.LogosLiveOverGoal())
			case 1:
				if c.LogosStartDedicated() {
					running++
				}
			case 2:
				if running > 0 {
					c.LogosStopDedicated()
					running--
				}
			}
			target := c.LogosDedicatedTarget()
			if needed := c.LogosDedicatedNeeded(); needed < 0 || needed+running != target {
				t.Fatalf("procs=%d step=%d: needed %d + running %d != target %d", procs, step, needed, running, target)
			}
			if target < lastTarget || target > limit {
				t.Fatalf("procs=%d step=%d: target %d after %d, limit %d", procs, step, target, lastTarget, limit)
			}
			lastTarget = target
			if f, bound := c.LogosFractionalGoal(), 1/float64(int64(procs)-target); f < 0 || f >= bound {
				t.Fatalf("procs=%d step=%d: fractional goal %v outside [0, %v)", procs, step, f, bound)
			}
		}
		if got, want := c.LogosBackgroundUtilization(procs, running), logosPlan(math.Max(peakU, 0.4)); !logosNearly(got, want, 1e-8) {
			t.Errorf("procs=%d: final utilization %v, want curve at peak u %v", procs, got, want)
		}
	}
}

// TestLogosElasticConcurrentRaisesAddEachWorkerOnce raises the goal from many
// goroutines while others take and return dedicated workers and a reader
// loads the fractional goal the way the scheduler does. The goal is shared
// without a lock, so every raise must add its dedicated workers exactly
// once, the final goal must be the curve at the highest ratio any goroutine
// used, and no load may see a value outside what a goal can hold.
// This test is a functional check, not race evidence: the race detector does
// not instrument package runtime. The race argument is that every shared
// write goes through an atomic, and every plain read of the fractional goal
// (mgc.go pollFractionalWorkerExit, mgcpacer.go assignWaitingGCWorker and
// the startCycle trace) loads it atomically.
func TestLogosElasticConcurrentRaisesAddEachWorkerOnce(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	const procs, goroutines, steps, rounds = 64, 16, 4000, 50
	const span = logosPlanTop + 0.02 - logosPlanRise
	// With 64 Ps the curve holds at most 48 dedicated workers, so a
	// fractional goal per remaining P stays below 1/16.
	const fractionalBound = 1.0 / (procs - procs*logosPlanCeiling)
	for round := range rounds {
		c := newLogosController()
		c.LogosStartCycleAt(logosGreentea, 0.5, procs)
		ratios := make([]float64, goroutines)
		var mu sync.Mutex
		var running int64
		adjust := func(g int) {
			mu.Lock()
			defer mu.Unlock()
			if g%2 == 0 {
				if c.LogosStartDedicated() {
					running++
				}
			} else if running > 0 {
				c.LogosStopDedicated()
				running--
			}
		}
		var done, torn atomic.Bool
		var readers sync.WaitGroup
		readers.Go(func() {
			for !done.Load() {
				if f := c.LogosFractionalGoal(); !(f >= 0 && f < fractionalBound) {
					torn.Store(true)
					return
				}
			}
		})
		release := make(chan struct{})
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Go(func() {
				<-release
				for k := range steps {
					u := logosPlanRise + span*float64(k*goroutines+g)/float64(goroutines*steps)
					ratios[g] = max(ratios[g], c.LogosElasticStepAt(u))
					if k%4 == 0 {
						adjust(g + k)
					}
				}
			})
		}
		close(release)
		wg.Wait()
		done.Store(true)
		readers.Wait()
		if torn.Load() {
			t.Fatalf("round %d: a load of the fractional goal saw a value outside [0, %v)", round, fractionalBound)
		}
		peak := slices.Max(ratios)
		wantTarget := int64(float64(procs)*logosPlan(peak) + 1e-9)
		needed, target := c.LogosDedicatedNeeded(), c.LogosDedicatedTarget()
		if target != wantTarget {
			t.Fatalf("round %d: target %d, want %d at peak u %v", round, target, wantTarget, peak)
		}
		if needed+running != target {
			t.Fatalf("round %d: needed %d + running %d != target %d", round, needed, running, target)
		}
		if got, want := c.LogosBackgroundUtilization(procs, running), logosPlan(peak); !logosNearly(got, want, 1e-8) {
			t.Fatalf("round %d: utilization %v, want %v at peak u %v", round, got, want, peak)
		}
	}
}

// TestLogosElasticKeepsAssistBackstop: assist work per byte is the upstream
// computation at every ratio, so assist stays the backstop at and above the
// saturation ratio, where the background goal has reached its ceiling.
func TestLogosElasticKeepsAssistBackstop(t *testing.T) {
	assistAt := func(mode string, u float64) float64 {
		c := newLogosController()
		c.LogosStartCycleAt(mode, 0.5, 8)
		c.LogosReviseAt(u)
		return c.AssistWorkPerByte()
	}
	prev := 0.0
	for _, u := range []float64{0.5, 0.8, 0.9, 0.95, 0.96, 1.0, 1.2} {
		elastic, upstream := assistAt(logosGreentea, u), assistAt("", u)
		if elastic != upstream {
			t.Errorf("u=%v: assist work per byte %v under greentea, %v upstream", u, elastic, upstream)
		}
		if u >= logosPlanTop && !(elastic > 0) {
			t.Errorf("u=%v: assist work per byte %v, want assists to keep running", u, elastic)
		}
		if elastic < prev {
			t.Errorf("u=%v: assist work per byte %v fell below %v at a lower u", u, elastic, prev)
		}
		prev = elastic
	}
}

func logosUpstreamSplit(procs int) (int64, float64) {
	total := float64(procs) * runtime.GCBackgroundUtilization
	dedicated := int64(total + 0.5)
	if e := float64(dedicated)/total - 1; e < -0.3 || e > 0.3 {
		if float64(dedicated) > total {
			dedicated--
		}
		return dedicated, (total - float64(dedicated)) / float64(procs)
	}
	return dedicated, 0
}

// TestLogosEmptyModeKeepsUpstreamPacing: an empty mode keeps the upstream
// pacing in the test controller; the built-program test covers the
// production path.
func TestLogosEmptyModeKeepsUpstreamPacing(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	for _, procs := range logosPlanProcs {
		wantD, wantF := logosUpstreamSplit(procs)
		c := newLogosController()
		c.LogosStartCycleAt("", 0.5, procs)
		if c.LogosElasticActive() {
			t.Fatalf("procs=%d: elastic pacing is active with an empty test-controller mode", procs)
		}
		for _, u := range []float64{0.5, 0.8, 0.9, 0.95, 0.99, 1.1} {
			c.LogosReviseAt(u)
			if got := c.LogosDedicatedNeeded(); got != wantD {
				t.Errorf("procs=%d u=%v: %d dedicated workers, upstream has %d", procs, u, got, wantD)
			}
			if got := c.LogosFractionalGoal(); got != wantF {
				t.Errorf("procs=%d u=%v: fractional goal %v, upstream has %v", procs, u, got, wantF)
			}
		}
	}
}

func TestLogosOtherModesKeepUpstreamPacing(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	for _, mode := range []string{"zgc", "immix", "wasm", "batch", "epsilon"} {
		t.Run(mode, func(t *testing.T) {
			c := newLogosController()
			c.LogosStartCycleAt(mode, 0.9, 8)
			c.LogosReviseAt(0.99)
			wantD, wantF := logosUpstreamSplit(8)
			if c.LogosElasticActive() || c.LogosDedicatedNeeded() != wantD || c.LogosFractionalGoal() != wantF {
				t.Errorf("mode %q changes the upstream pacing", mode)
			}
			if got := c.LogosCycleUtilization(1e9, 8); got != runtime.GCBackgroundUtilization {
				t.Errorf("mode %q ends a cycle at utilization %v, upstream assumes %v", mode, got, runtime.GCBackgroundUtilization)
			}
		})
	}
}

// TestLogosTestControllerIgnoresProcessMode: a controller that a test builds
// takes its mode from the test, so the upstream pacer tests keep their
// numbers when the test binary is linked with greentea, the logos default.
func TestLogosTestControllerIgnoresProcessMode(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	old := runtime.LogosPacerSetMode(logosGreentea)
	defer runtime.LogosPacerSetMode(old)
	c := newLogosController()
	c.LogosStartCycleAt("", 0.97, 8)
	wantD, wantF := logosUpstreamSplit(8)
	if c.LogosElasticActive() || c.LogosDedicatedNeeded() != wantD || c.LogosFractionalGoal() != wantF {
		t.Errorf("a test controller paces elastically because the process mode is greentea")
	}
}

// logosEndProcs and logosEndElapsed describe the cycles of the endCycle
// attacks: eight Ps, one second of mark.
const (
	logosEndProcs   = 8
	logosEndElapsed = int64(1e9)
)

// TestLogosElasticEndCycleUsesMeanGoal: endCycle credits the cycle with the
// time-weighted mean of the background goals it ran with, where upstream
// assumes the fixed quarter.
func TestLogosElasticEndCycleUsesMeanGoal(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	type raise struct {
		u  float64
		at int64
	}
	cases := []struct {
		name   string
		procs  int
		u0     float64
		raises []raise
		want   float64
	}{
		{"floor all cycle", 8, 0.5, nil, 0.25},
		{"ceiling all cycle", 8, 0.97, nil, 0.75},
		{"one raise at half time", 8, 0.5, []raise{{0.96, 5e8}}, 0.50},
		{"two raises", 8, 0.5, []raise{{0.875, 2e8}, {0.96, 6e8}}, 0.55},
		{"a lower ratio later changes nothing", 8, 0.5, []raise{{0.96, 5e8}, {0.6, 7e8}}, 0.50},
		{"a raise at the very start", 8, 0.5, []raise{{0.96, 0}}, 0.75},
		{"three Ps", 3, 0.5, []raise{{0.96, 5e8}}, 0.50},
		{"sixty-four Ps", 64, 0.5, []raise{{0.875, 1e8}, {0.96, 9e8}}, 0.25*0.1 + 0.5*0.8 + 0.75*0.1},
		{"a raise after the clock was read", 8, 0.5, []raise{{0.96, 15e8}}, 0.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newLogosController()
			c.LogosStartCycleAt(logosGreentea, tc.u0, tc.procs)
			for _, r := range tc.raises {
				c.LogosElasticStepAtTime(r.u, r.at)
			}
			got := c.LogosCycleUtilization(logosEndElapsed, tc.procs)
			if !logosNearly(got, tc.want, 1e-9) {
				t.Errorf("cycle utilization %v, want %v", got, tc.want)
			}
		})
	}
}

// logosEndCycle starts a cycle at u0 under mode, runs setup, adds 64 KiB of
// allocation and 1 MiB of scan work, ends the cycle after logosEndElapsed
// with assist CPU time assist, and returns the cons/mark estimate with the
// allocation and scan work that produced it.
func logosEndCycle(mode string, u0 float64, setup func(*runtime.GCController), assist int64) (consMark float64, alloc, scan uint64) {
	c := newLogosController()
	c.LogosStartCycleAt(mode, u0, logosEndProcs)
	if setup != nil {
		setup(c)
	}
	scan = 1 << 20
	c.LogosAddWork(1<<16, int64(scan))
	alloc = c.HeapLive() - c.Triggered()
	c.EndCycle(c.HeapLive()/2, assist, logosEndElapsed, logosEndProcs)
	return c.LogosConsMark(), alloc, scan
}

// logosConsMark is the cons/mark ratio of one cycle at CPU utilization
// util with no idle marking.
func logosConsMark(alloc, scan uint64, util float64) float64 {
	return float64(alloc) * util / (float64(scan) * (1 - util))
}

func TestLogosElasticConsMarkFollowsGoal(t *testing.T) {
	if runtime.LogosGCModeIsSTW() {
		t.Skipf("stop-the-world GC mode (batch or epsilon): elastic pacing is off and every collection stops the world, so there is no concurrent pacing to assert")
	}
	check := func(t *testing.T, got float64, alloc, scan uint64, util float64) {
		t.Helper()
		if want := logosConsMark(alloc, scan, util); !(want > 0) || !logosNearly(got/want, 1, 1e-9) {
			t.Errorf("cons/mark %v, want %v at utilization %v", got, want, util)
		}
	}
	t.Run("upstream", func(t *testing.T) {
		got, alloc, scan := logosEndCycle("", 0.97, nil, 0)
		check(t, got, alloc, scan, logosPlanFloor)
	})
	t.Run("ceiling all cycle", func(t *testing.T) {
		got, alloc, scan := logosEndCycle(logosGreentea, 0.97, nil, 0)
		check(t, got, alloc, scan, logosPlanCeiling)
	})
	t.Run("raised at a quarter of the cycle", func(t *testing.T) {
		raise := func(c *runtime.GCController) { c.LogosElasticStepAtTime(0.96, 25e7) }
		got, alloc, scan := logosEndCycle(logosGreentea, 0.5, raise, 0)
		check(t, got, alloc, scan, logosPlanFloor*0.25+logosPlanCeiling*0.75)
	})
	t.Run("assist share adds to the goal", func(t *testing.T) {
		assist := logosEndElapsed * logosEndProcs / 10
		got, alloc, scan := logosEndCycle(logosGreentea, 0.5, nil, assist)
		check(t, got, alloc, scan, logosPlanFloor+0.1)
	})
	t.Run("upstream assist share", func(t *testing.T) {
		assist := logosEndElapsed * logosEndProcs / 10
		got, alloc, scan := logosEndCycle("", 0.5, nil, assist)
		check(t, got, alloc, scan, logosPlanFloor+0.1)
	})
	// Background 0.75 plus assist 0.5 would leave the mutator no CPU and
	// make the estimate negative; the cycle is capped at 0.95.
	t.Run("capped below one", func(t *testing.T) {
		assist := logosEndElapsed * logosEndProcs / 2
		got, alloc, scan := logosEndCycle(logosGreentea, 0.97, nil, assist)
		check(t, got, alloc, scan, 0.95)
	})
	t.Run("upstream is not capped", func(t *testing.T) {
		assist := logosEndElapsed * logosEndProcs / 2
		got, alloc, scan := logosEndCycle("", 0.97, nil, assist)
		check(t, got, alloc, scan, logosPlanFloor+0.5)
	})
}

type logosBenchNode struct {
	left, right *logosBenchNode
	pad         [6]uint64
}

func logosBenchTree(depth int) *logosBenchNode {
	if depth == 0 {
		return nil
	}
	return &logosBenchNode{left: logosBenchTree(depth - 1), right: logosBenchTree(depth - 1)}
}

func logosReadMetrics(b *testing.B, names ...string) []metrics.Value {
	b.Helper()
	s := make([]metrics.Sample, len(names))
	for i, name := range names {
		s[i].Name = name
	}
	metrics.Read(s)
	values := make([]metrics.Value, len(s))
	for i := range s {
		if s[i].Value.Kind() == metrics.KindBad {
			b.Fatalf("metric %s is not supported", s[i].Name)
		}
		values[i] = s[i].Value
	}
	return values
}

func logosAssistAndTotalCPU(b *testing.B) (assist, total float64) {
	b.Helper()
	v := logosReadMetrics(b, "/cpu/classes/gc/mark/assist:cpu-seconds", "/cpu/classes/total:cpu-seconds")
	return v[0].Float64(), v[1].Float64()
}

func logosPacerCycles(b *testing.B) uint64 {
	b.Helper()
	return logosReadMetrics(b, "/gc/cycles/total:gc-cycles")[0].Uint64()
}

func logosAllocBurst(workers, perWorker int) {
	var wg sync.WaitGroup
	sinks := make([]*logosBenchNode, workers)
	for w := range workers {
		wg.Go(func() {
			var keep *logosBenchNode
			for range perWorker {
				keep = &logosBenchNode{left: keep}
				if keep.left != nil && keep.left.left != nil {
					keep.left.left = nil
				}
			}
			sinks[w] = keep
		})
	}
	wg.Wait()
	runtime.KeepAlive(sinks)
}

// BenchmarkLogosAssistShare reports the share of CPU time spent in mark
// assist, from /cpu/classes/gc/mark/assist:cpu-seconds over
// /cpu/classes/total:cpu-seconds, for the upstream (wasm) pacing and for greentea.
// The upstream arm uses wasm because wasm is an upstream-pacing mode.
// The target for greentea is a lower share; it is recorded, not asserted.
func BenchmarkLogosAssistShare(b *testing.B) {
	for _, mode := range []struct{ name, gc string }{{"upstream", "wasm"}, {"greentea", "greentea"}} {
		b.Run(mode.name, func(b *testing.B) {
			old := runtime.LogosPacerSetMode(mode.gc)
			b.Cleanup(func() { runtime.LogosPacerSetMode(old) })
			live := logosBenchTree(19)
			runtime.GC()
			assist0, total0 := logosAssistAndTotalCPU(b)
			cycles0 := logosPacerCycles(b)
			b.ResetTimer()
			for range b.N {
				logosAllocBurst(runtime.GOMAXPROCS(0), 1<<18)
			}
			b.StopTimer()
			runtime.GC()
			assist1, total1 := logosAssistAndTotalCPU(b)
			if total1 > total0 {
				b.ReportMetric((assist1-assist0)/(total1-total0), "assist-share")
			}
			b.ReportMetric(float64(logosPacerCycles(b)-cycles0)/float64(b.N), "gc-cycles/op")
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			b.ReportMetric(float64(ms.HeapSys)/(1<<20), "heap-sys-MB")
			runtime.KeepAlive(live)
		})
	}
}
