// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"reflect"
	"runtime"
	"runtime/debug"
	"testing"
	"time"
	"unsafe"
)

// Hole-scan latency over production spans: record only, never gate. Each
// tier sweeps a real span of the 2048-byte pointer-free class (16 slots of
// 16 lines, exact density mapping) at a fixed hole density and times the
// exported hole scan over its occupancy plane.
// The STW rebuild itself has no external-test seam, so the timed path is
// the scan collection runs through FindHole applied to production planes.
var logosImmixHoleSpanTiers = []struct {
	name string
	pct  int
	drop func(k int) bool
}{
	{"holes-00", 0, func(k int) bool { return false }},
	{"holes-25", 25, func(k int) bool { return k%4 == 3 }},
	{"holes-50", 50, func(k int) bool { return k%2 == 1 }},
	{"holes-75", 75, func(k int) bool { return k%4 != 3 }},
}

var logosImmixHoleSpanSinks []int

var logosImmixHoleSpanKeep []unsafe.Pointer

func logosImmixHoleSpanFree(occ *runtime.LogosLineMark) int {
	free := 0
	for _, v := range occ {
		if v == 0 {
			free++
		}
	}
	return free
}

func logosImmixHoleSpanClass(tb testing.TB) immixProdClass {
	tb.Helper()
	for _, c := range immixProdClasses() {
		if c.size == 2048 && !c.scan {
			if !c.wantActive() {
				tb.Skipf("%v is not block-served in this build", c)
			}
			return c
		}
	}
	tb.Fatal("no 2048-byte pointer-free class in the class table")
	panic("unreachable")
}

// logosImmixHoleSpanRequireMode skips unless this is the immix build.
func logosImmixHoleSpanRequireMode(tb testing.TB) {
	tb.Helper()
	if runtime.LogosImmixMovingExperiment() && runtime.LogosHeapObjectsCanMove() && runtime.LogosImmixOn() {
		return
	}
	tb.Skip("needs GOEXPERIMENT=logosgcmoving and -ldflags=-X=runtime.logosGCMode=immix")
}

// logosImmixHoleSpanOf copies the span state of p, failing on miss.
func logosImmixHoleSpanOf(tb testing.TB, p unsafe.Pointer) runtime.LogosImmixSpan {
	tb.Helper()
	st, ok := runtime.LogosImmixSpanOf(p)
	if !ok {
		tb.Fatalf("%p is not in an in-use span", p)
	}
	return st
}

func logosImmixHoleSpanFresh(tb testing.TB, c immixProdClass, typ reflect.Type) unsafe.Pointer {
	tb.Helper()
	hold := make([]unsafe.Pointer, 0, 8)
	for range 2*c.wantElems() + 8 {
		p := reflect.New(typ).UnsafePointer()
		st := logosImmixHoleSpanOf(tb, p)
		if st.Active && st.AllocCount == 1 && st.FreeIndex == 1 && c.slot(p) == st.Base {
			logosImmixHoleSpanKeep = append(logosImmixHoleSpanKeep, hold...)
			return p
		}
		hold = append(hold, p)
	}
	tb.Fatalf("%v: no fresh span", c)
	panic("unreachable")
}

// logosImmixHoleSpanOccupy builds one swept production span at pct percent
// holes and returns a copy of its occupancy plane. The block is pinned so
// sparse tiers cannot evacuate under the measurement; the pin matches the
// exclusion discipline of the production harness.
func logosImmixHoleSpanOccupy(tb testing.TB, c immixProdClass, typ reflect.Type, pct int, drop func(k int) bool) (runtime.LogosLineMark, unsafe.Pointer) {
	tb.Helper()
	n := c.wantElems()
	objs := make([]unsafe.Pointer, n)
	live := make([]bool, n)
	first := logosImmixHoleSpanFresh(tb, c, typ)
	st0 := logosImmixHoleSpanOf(tb, first)
	base, elem := st0.Base, st0.ElemSize
	if int(st0.NElems) != n {
		tb.Fatalf("%v: fresh span has %d slots, want %d", c, st0.NElems, n)
	}
	objs[0] = first
	for k := 1; k < n; k++ {
		p := reflect.New(typ).UnsafePointer()
		if c.slot(p) != base+uintptr(k)*elem {
			tb.Fatalf("%v: allocation %d at %#x, want slot %#x", c, k, c.slot(p), base+uintptr(k)*elem)
		}
		objs[k] = p
	}
	for k, p := range objs {
		immixProdFill(c, p, k)
		live[k] = true
	}
	if !runtime.LogosImmixPinAddr(c.slot(first)) {
		tb.Fatalf("%v: cannot pin block of span %#x", c, base)
	}
	nLive := n
	for k := range objs {
		if drop(k) {
			live[k] = false
			objs[k] = nil
			nLive--
		}
	}
	anchor := -1
	for k, l := range live {
		if l {
			anchor = k
			break
		}
	}
	if anchor < 0 {
		tb.Fatalf("%v: tier drops every slot, no anchor survives", c)
	}
	runtime.GC()
	for k, p := range objs {
		if p == nil {
			continue
		}
		if c.slot(p) != base+uintptr(k)*elem || !immixProdIntact(c, p, k) {
			tb.Fatalf("%v: survivor %d moved or changed across the sweep", c, k)
		}
	}
	st := logosImmixHoleSpanOf(tb, objs[anchor])
	if int(st.AllocCount) != nLive {
		tb.Fatalf("%v: swept span holds %d objects, want the %d survivors", c, st.AllocCount, nLive)
	}
	occ, _, _, _, ok := runtime.LogosImmixPlanes(objs[anchor])
	if !ok {
		tb.Fatalf("%v: no block metadata for span %#x", c, base)
	}
	if gotFree, wantFree := logosImmixHoleSpanFree(&occ), runtime.LogosImmixLines*pct/100; gotFree != wantFree {
		tb.Fatalf("%v: occupancy plane has %d free lines, want %d for %d%% holes", c, gotFree, wantFree, pct)
	}
	logosImmixHoleSpanKeep = append(logosImmixHoleSpanKeep, objs...)
	return occ, objs[anchor]
}

// logosImmixHoleSpanIsolate holds P count and GC percent steady while a
// span is built and swept, mirroring the child discipline of the
// production harness.
func logosImmixHoleSpanIsolate() func() {
	prevProcs := runtime.GOMAXPROCS(1)
	prevGC := debug.SetGCPercent(-1)
	return func() {
		runtime.GOMAXPROCS(prevProcs)
		debug.SetGCPercent(prevGC)
	}
}

// BenchmarkLogosImmixHoleScanSpans times the collection hole-scan path
// over one production occupancy plane per density tier. Recorded, never
// asserted.
func BenchmarkLogosImmixHoleScanSpans(b *testing.B) {
	logosImmixHoleSpanRequireMode(b)
	restore := logosImmixHoleSpanIsolate()
	defer restore()
	c := logosImmixHoleSpanClass(b)
	typ := c.typ()
	reflect.New(typ)
	runtime.GC()
	runtime.GC()
	occs := make([]runtime.LogosLineMark, len(logosImmixHoleSpanTiers))
	for i, tier := range logosImmixHoleSpanTiers {
		occ, _ := logosImmixHoleSpanOccupy(b, c, typ, tier.pct, tier.drop)
		occs[i] = occ
	}
	for i, tier := range logosImmixHoleSpanTiers {
		b.Run(tier.name, func(b *testing.B) {
			occ := occs[i]
			from, sum := 0, 0
			for b.Loop() {
				start, end, ok := occ.FindHole(from)
				if !ok {
					from = 0
					continue
				}
				from = end
				if from >= runtime.LogosImmixLines {
					from = 0
				}
				sum += start
			}
			logosImmixHoleSpanSinks = append(logosImmixHoleSpanSinks, sum)
		})
	}
}

// TestLogosImmixHoleScanLatencyRecordSpans builds one swept production
// span per density tier and t.Logs the ns-per-scan distribution of the
// hole scan over its occupancy plane: 100 trials of 200 scans with mean,
// p50, p95, p99 and max. No latency threshold.
func TestLogosImmixHoleScanLatencyRecordSpans(t *testing.T) {
	logosImmixHoleSpanRequireMode(t)
	t.Logf("hole-scan path: AVX2=%v", runtime.LogosImmixHasAVX2())
	restore := logosImmixHoleSpanIsolate()
	defer restore()
	c := logosImmixHoleSpanClass(t)
	t.Logf("span class: %v slots=%d", c, c.wantElems())
	typ := c.typ()
	reflect.New(typ)
	runtime.GC()
	runtime.GC()
	const trials, perTrial = 100, 200
	for _, tier := range logosImmixHoleSpanTiers {
		occ, anchor := logosImmixHoleSpanOccupy(t, c, typ, tier.pct, tier.drop)
		var trialNS [trials]float64
		from, sum := 0, 0
		for i := range trials {
			start := time.Now()
			for range perTrial {
				s, e, ok := occ.FindHole(from)
				if !ok {
					from = 0
					continue
				}
				from = e
				if from >= runtime.LogosImmixLines {
					from = 0
				}
				sum += s
			}
			trialNS[i] = float64(time.Since(start).Nanoseconds()) / float64(perTrial)
		}
		for i := 1; i < trials; i++ {
			for j := i; j > 0 && trialNS[j] < trialNS[j-1]; j-- {
				trialNS[j], trialNS[j-1] = trialNS[j-1], trialNS[j]
			}
		}
		mean := 0.0
		for _, v := range trialNS {
			mean += v
		}
		mean /= float64(trials)
		t.Logf("tier %s (%d%% holes): %d trials of %d scans: mean=%.2f p50=%.2f p95=%.2f p99=%.2f max=%.2f ns/scan",
			tier.name, tier.pct, trials, perTrial, mean,
			trialNS[trials*50/100], trialNS[trials*95/100], trialNS[trials*99/100], trialNS[trials-1])
		logosImmixHoleSpanSinks = append(logosImmixHoleSpanSinks, sum)
		occ2, _, _, _, ok := runtime.LogosImmixPlanes(anchor)
		if !ok {
			t.Fatalf("tier %s: lost block metadata after timing", tier.name)
		}
		if occ2 != occ {
			t.Fatalf("tier %s: occupancy plane changed under read-only timing", tier.name)
		}
	}
}
