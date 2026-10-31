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

// The GC-start rewrite walk (the logosWalkAllWith call of zgcGCStartHook,
// run here through LogosZgcWalkAllLiveFix) must treat every allocated slot
// as walkable. The runtime's own allocated rule — the one the sweep's
// zombie check (mgcsweep.go), the trace free path (traceallocfree.go) and
// the quarantine re-mark (logos_zgc_quarantine.go) all use — is
// (i < freeindex) || allocBits[i]: the malloc fast path (nextFreeFast)
// advances freeindex and the alloc cache without writing allocBits, so
// before the next sweep swaps the mark bits in, a live fresh object sits
// below freeindex with a clear alloc bit (sweep resets freeindex to 0, so
// no hole can sit below it). The walker's predicate read only the alloc
// bit, so a fresh object's pointer field never reached the rewrite: it
// kept naming the old source after the referent moved, and the source
// page's retire left it dangling.
//
// The fixture is the production heap graph in one instance: a real
// scanned span from the page allocator, slots handed out by the
// allocator's own paths (nextFreeFast for the fresh slot, the claim
// accounting for the rest), a referent moved by the production
// claim-and-commit copy (zgcCopyOneResult) with its forwarding entry
// published, and the GC-start walk itself. The oracle is pointer values,
// not wiring: the fresh slot's field must name the copy, the bit-marked
// control's field must still heal (the freeindex term must not break the
// bit path), and a true-free slot's stale word must survive untouched (no
// false positives above freeindex). uintptr locals are not pointer-typed,
// so the walk heals the fixture span but not this test's own copies of
// the addresses.
func TestLogosZgcFreshAllocationHealedByGCStartWalk(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	const ptrSize = 8
	run := LogosZgcAllocRun([]int{LogosZgcRunHeapScanSmall})
	if run == nil {
		t.Fatal("no small scanned span after 256 trials")
	}
	elem := run.Elem(0)
	if elem < 2*ptrSize || elem%ptrSize != 0 {
		run.Free(0)
		t.Fatalf("small scanned class elem = %d, want at least two pointer words", elem)
	}
	base := run.Base(0)
	region := base &^ (LogosZgcPageSize - 1)
	if !LogosZgcPageSlotFree(region) {
		run.Free(0)
		t.Fatalf("granule %#x already owned by a page before the fixture", region)
	}

	// Slot layout, handed out in the allocator's own order: 0 fresh (the
	// fast path: below freeindex, clear alloc bit), 1 bit-marked control,
	// 2 the referent, 3 the true-free control (above freeindex, clear
	// bit, stale bytes that must not be rewritten).
	patA := uintptr(0x1111111111111111)
	patB := uintptr(0x2222222222222222)
	patR := uintptr(0x3333333333333333)
	slotA := base + 0*elem
	slotB := base + 1*elem
	srcR := base + 2*elem
	slotFree := base + 3*elem

	LogosZgcStore(slotA, patA)
	LogosZgcStore(slotA+ptrSize, srcR)
	LogosZgcSetSpanSmallBits(run, 0, slotA, 0x02)
	LogosZgcStore(slotB, patB)
	LogosZgcStore(slotB+ptrSize, srcR)
	LogosZgcSetSpanSmallBits(run, 0, slotB, 0x02)
	LogosZgcStore(srcR, patR)
	LogosZgcSetSpanSmallBits(run, 0, srcR, 0)
	LogosZgcStore(slotFree+ptrSize, srcR)
	LogosZgcSetSpanSmallBits(run, 0, slotFree, 0x02)

	run.OccupyFresh(0, 0)
	run.Occupy(0, 1)
	run.Occupy(0, 2)

	pg := LogosZgcNewPage(region, 3*elem, 3)
	LogosZgcRecordSources(pg, run, []int{0})
	var eligible, reserved bool
	LogosZgcWithWorldStopped(func() {
		eligible = LogosZgcEligible(pg)
		reserved = LogosZgcReserveToSpace(pg)
	})
	// committed flips once a copy has: a page with a filled to-space span
	// must leave through publish, never abandon's drain.
	committed := false
	endRound := func() {
		// The three committed copies leave the source count at zero, so
		// the span hands back to the allocator through the same vet the
		// claim fixtures use.
		defer run.Free(0)
		if committed {
			LogosZgcWithWorldStopped(func() { LogosZgcPublishToSpaceSpans(pg) })
		}
		LogosZgcDropPage(pg)
	}
	if !eligible || !reserved {
		endRound()
		t.Fatalf("eligible=%v reserved=%v", eligible, reserved)
	}

	wonA, stA := LogosZgcCopyOneResult(pg, run, 0, slotA, elem)
	wonB, stB := LogosZgcCopyOneResult(pg, run, 0, slotB, elem)
	wonR, stR := LogosZgcCopyOneResult(pg, run, 0, srcR, elem)
	if stA != LogosZgcRelocateDone || stB != LogosZgcRelocateDone || stR != LogosZgcRelocateDone {
		endRound()
		t.Fatalf("copy statuses %d/%d/%d, want %d", stA, stB, stR, LogosZgcRelocateDone)
	}
	committed = true
	if wonA == 0 || wonB == 0 || wonR == 0 || wonA == wonB || wonA == wonR || wonB == wonR {
		endRound()
		t.Fatalf("winners %#x/%#x/%#x, want three distinct committed copies", wonA, wonB, wonR)
	}

	// The GC-start rewrite over the real heap graph.
	LogosZgcWithWorldStopped(LogosZgcWalkAllLiveFix)

	if got := LogosZgcWord(slotA + ptrSize); got != wonR {
		endRound()
		t.Fatalf("fresh slot below freeindex with a clear alloc bit kept %#x, want the copy %#x: the GC-start walk missed a fresh allocation", got, wonR)
	}
	if got := LogosZgcWord(slotB + ptrSize); got != wonR {
		endRound()
		t.Fatalf("bit-marked slot healed to %#x, want the copy %#x", got, wonR)
	}
	if got := LogosZgcWord(slotFree + ptrSize); got != srcR {
		endRound()
		t.Fatalf("free slot above freeindex rewritten to %#x, want the stale %#x untouched: the walk visited an unallocated slot", got, srcR)
	}
	if got := LogosZgcWord(wonR); got != patR {
		endRound()
		t.Fatalf("copy %#x word 0 = %#x, want the referent pattern %#x", wonR, got, patR)
	}
	endRound()
}
