// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime/debug"
	"testing"

	. "runtime"
)

// Start-address versus source-address page resolution on a narrowed page.
//
// Crash this pins (zgc-verify28.log): the interior-pointer fixture
// resolved its page through the region start
// (LogosZgcPageFor of the granule base) and inserted the object address.
// When a narrowed (gappy, srcExact) page owns the granule, the region
// base can sit in one source range while the object sits in a gap: the
// insert then dies in forwardPublish (logos_zgc_page.go:490,
// "forwarding source outside its page's source ranges") even though the
// lookup that produced the page succeeded. Source-address resolution
// (LogosZgcPageFor of the object itself) is exact: it returns the page
// only when the object is a relocation source and nil otherwise. The
// fixture no longer resolves by granule base at all: it registers over a
// granule whose slot is free and passes the source address itself.
//
// The test below rebuilds that shape deterministically (heap, manual,
// heap) and asserts the contract without tripping the fatal: range
// edges resolve, the gap does not, and a legitimate in-range insert
// still publishes. It never inserts a gap address: that call fatals by
// design.
func TestLogosZgcForwardRangeStartVsSource(t *testing.T) {
	// The page goes over a real heap granule: only the zgc low heap
	// window keeps it registrable.
	logosZgcRequireHeapWindow(t)
	// GC stays off while the page is registered over a heap granule, and
	// the restore is registered first so it runs last: every page and
	// span cleanup below therefore completes before a collection can
	// start again.
	oldGCPercent := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(oldGCPercent) })
	GC()
	GC()

	const s0, gap, s1 = 0, 1, 2

	// This test needs real heap spans: prepareSources reads span state, so
	// a mapped region cannot play the part. The page therefore goes over a
	// heap granule, and it must be one whose directory slot is empty: a
	// collector page already there cannot be registered over, and a
	// narrowed one resolves to nil for these spans while still owning the
	// slot, which is exactly the collision that threw before.
	//
	// A rejected run keeps its spans allocated until the test ends: the
	// allocator hands out the lowest free pages first, so freeing a
	// rejected run would put the next trial straight back on its occupied
	// granule. The trials are bounded, and every one of them is freed
	// below, so nothing outlives the test. GC stays off from here on, so
	// no cycle can claim the accepted granule between the check and the
	// registration.
	//
	// A run is three spans of one runtime page, so consecutive trials on
	// their own would advance a few tens of KB — nowhere near the 2 MB
	// granule they share, which means a trial that lands on an owned
	// granule would be repeated by every later trial. Each rejection is
	// therefore followed by a held 2 MB filler, which consumes a whole
	// granule's worth of the page heap and pushes the next trial into a
	// later one.
	var run *LogosZgcSpanRun
	var region uintptr
	var rejected []*LogosZgcSpanRun
	var fillers [][]byte
	t.Cleanup(func() {
		for _, r := range rejected {
			for i := r.Len() - 1; i >= 0; i-- {
				r.Free(i)
			}
		}
		fillers = nil
	})
	for range 8 {
		cand := LogosZgcAllocRun([]int{LogosZgcRunHeap, LogosZgcRunManual, LogosZgcRunHeap})
		if cand == nil {
			break
		}
		base := cand.Base(s0) &^ (LogosZgcPageSize - 1)
		if LogosZgcPageSlotFree(base) {
			run, region = cand, base
			break
		}
		rejected = append(rejected, cand)
		// Held only, never read: its job is to take a whole granule out of
		// the page allocator so the next trial must start in a later one.
		fillers = append(fillers, make([]byte, LogosZgcPageSize))
	}
	if run == nil {
		t.Skip("no tight span run on a granule no page owns")
	}
	for i := 2; i >= 0; i-- {
		t.Cleanup(func() { run.Free(i) })
	}
	elem := run.Elem(s0)
	if elem == 0 || run.Elem(s1) != elem {
		t.Fatal("source spans have no common object size")
	}

	pg := LogosZgcNewPage(region, 2*elem, 2)
	t.Cleanup(func() {
		LogosZgcForgetPage(pg)
		LogosZgcDropPage(pg)
	})

	// Selection order is not address order; record reversed so the
	// prepare sort is exercised.
	LogosZgcRecordSources(pg, run, []int{s1, s0})
	var eligible bool
	LogosZgcWithWorldStopped(func() { eligible = LogosZgcEligible(pg) })
	if !eligible {
		t.Fatal("heap/manual/heap page refused: gaps are eligible ranges")
	}
	got, exact := LogosZgcPageSources(pg)
	if !exact || len(got) != 2 ||
		got[0] != [2]uintptr{run.Base(s0), run.Base(s0) + run.Bytes(s0)} ||
		got[1] != [2]uintptr{run.Base(s1), run.Base(s1) + run.Bytes(s1)} {
		t.Fatalf("source ranges = %v, exact %v; want the two heap extents", got, exact)
	}

	gapMid := (got[0][1] + got[1][0]) / 2
	t.Logf("page start %#x srcN %d ranges %#x-%#x %#x-%#x gap probe %#x",
		region, len(got), got[0][0], got[0][1], got[1][0], got[1][1], gapMid)

	// Range edges resolve to the page.
	for i, r := range got {
		for _, a := range []uintptr{r[0], r[1] - 1} {
			if LogosZgcPageFor(a) != pg {
				t.Fatalf("source edge %d %#x does not resolve to the page", i, a)
			}
		}
	}
	// The gap resolves to nothing and holds no entry.
	if LogosZgcPageFor(gapMid) != nil {
		t.Fatalf("gap address %#x resolves to the page", gapMid)
	}
	if to, ok := LogosZgcForward(pg, gapMid); ok {
		t.Fatalf("gap address %#x holds entry %#x", gapMid, to)
	}

	// A legitimate multi-range source still publishes: the throw's
	// validation accepts every address inside a registered range.
	if won := LogosZgcForwardInsert(pg, got[0][0], got[0][0]); won != got[0][0] {
		t.Fatalf("in-range insert returned %#x, want %#x", won, got[0][0])
	}

	// The crash mechanism: resolution through an address that names a
	// different range (or the gap) than the source can hand back a page
	// the source does not belong to. Only source-address resolution is
	// exact; inserting gapMid through any non-nil page returned for a
	// coarser address fatals at logos_zgc_page.go:490.
	startResolve := LogosZgcPageFor(region)
	sourceResolve := LogosZgcPageFor(gapMid)
	t.Logf("region-base resolve %v gap-source resolve %v (want nil)",
		startResolve == pg, sourceResolve)
	if sourceResolve != nil {
		t.Fatalf("gap source %#x resolved to a page", gapMid)
	}
	// Deterministic form of the same divergence: resolving by any
	// address other than the source can hand back a page the source
	// does not belong to. got[0][0] is in-range (hits); gapMid is not
	// (misses). The helper's region-base key is one such coarser
	// address; when the allocator places the region base in a range,
	// the crash layout is present in full.
	if coarse := LogosZgcPageFor(got[0][0]); coarse != pg {
		t.Fatalf("in-range address %#x did not resolve to the page", got[0][0])
	}
	if startResolve == pg {
		t.Logf("crash layout present: region base %#x resolves to pg while gap %#x does not",
			region, gapMid)
	}
}
