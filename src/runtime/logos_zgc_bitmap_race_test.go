// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime/debug"
	"sync"
	"testing"
	"unsafe"

	. "runtime"
)

// Concurrent claims of adjacent to-space slots share one heap-bitmap
// word. The contract under test: after two real claim-and-commit copies
// (the production zgcCopyOneResult path, unmodified), each destination
// slot's pointer map equals its source's pointer map over the exact
// object range — computed here independently of the production writer —
// and every copied pointer word names a real, still live referent. When
// the bitmap write ran outside the page mutex, the two plain
// read-modify-writes raced on the shared word and one slot's mask was
// lost: a published copy whose pointer map no longer matches its source,
// so a scan of the copy misses real referents. Runtime bitmap writes are
// not race-instrumented, so the check is value equality on the maps
// themselves, iterated to make the interleaving likely.
func TestLogosZgcClaimBitmapConcurrentMasks(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	if GOMAXPROCS(0) < 2 {
		t.Skip("concurrent claims need at least two Ps")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	const (
		ptrSize = 8
		rounds  = 512
	)
	for round := range rounds {
		// A fresh source run every round: the previous round ended with
		// its page unregistered and its source span freed, so the page
		// allocator hands the same granule back with clean alloc
		// accounting, and the round's two source slots start free the
		// way a real allocation would find them.
		run := LogosZgcAllocRun([]int{LogosZgcRunHeapScanSmall})
		if run == nil {
			t.Fatalf("round %d: no tight small scanned span after 256 trials", round)
		}
		elem := run.Elem(0)
		if elem == 0 || elem%ptrSize != 0 || elem > 512 {
			run.Free(0)
			t.Fatalf("round %d: small scanned class elem = %d, want a bitmap-object size <= 512", round, elem)
		}
		words := int(elem / ptrSize)
		srcA := run.Base(0)
		srcB := run.Base(0) + elem
		region := run.Base(0) &^ (LogosZgcPageSize - 1)
		if !LogosZgcPageSlotFree(region) {
			run.Free(0)
			t.Fatalf("round %d: granule %#x already occupied before the round (pending %d)", round, region, LogosZgcRetirePendingLen())
		}

		// One worker's fixture: fill the object words with its pattern, then
		// overwrite the pointer words with the addresses of a real referent
		// the test keeps alive, and give the slot its pointer map mask
		// through the span bitmap writer. Bit 0 of the mask stays clear so
		// word 0 keeps the pattern that identifies the copy.
		fill := func(src, pat, mask uintptr, ref []uintptr) (addrs []uintptr) {
			for k := range words {
				LogosZgcStore(src+uintptr(k)*ptrSize, pat+uintptr(k))
			}
			addrs = make([]uintptr, words)
			for k := range words {
				addrs[k] = uintptr(unsafe.Pointer(&ref[k]))
				if mask&(1<<uint(k)) != 0 {
					LogosZgcStore(src+uintptr(k)*ptrSize, addrs[k])
				}
			}
			return addrs
		}

		// Distinct per-round masks over the object's own words, so stale
		// bitmap words from a previous round or a recycled page can
		// never pass for the expected map.
		mA := uintptr(round%127)<<1 | 0x02
		mB := uintptr((round+64)%127)<<1 | 0x02
		if mA == mB || mA == 0 || mB == 0 {
			t.Fatalf("round %d: masks %#x/%#x", round, mA, mB)
		}
		patA := uintptr(round)<<8 | 0xAA
		patB := uintptr(round)<<8 | 0xBB

		refA := make([]uintptr, words)
		refB := make([]uintptr, words)
		for k := range refA {
			refA[k] = patA + uintptr(k)
			refB[k] = patB + uintptr(k)
		}
		addrA := fill(srcA, patA, mA, refA)
		addrB := fill(srcB, patB, mB, refB)
		LogosZgcSetSpanSmallBits(run, 0, srcA, mA)
		LogosZgcSetSpanSmallBits(run, 0, srcB, mB)
		if got := LogosZgcSpanSmallBits(run, 0, srcA); got != mA {
			t.Fatalf("round %d: source A map = %#x, want %#x", round, got, mA)
		}
		if got := LogosZgcSpanSmallBits(run, 0, srcB); got != mB {
			t.Fatalf("round %d: source B map = %#x, want %#x", round, got, mB)
		}
		// The two hand-built source objects are real objects of the run's
		// span: hand their slots out through the allocator's own occupancy
		// accounting each round, so the claim path's published-copy
		// decrement (zgcCopyOneResult) moves a real count of two to zero
		// instead of wrapping an uncounted span below zero.
		run.Occupy(0, 0)
		run.Occupy(0, 1)
		pg := LogosZgcNewPage(region, 2*elem, 2)
		LogosZgcRecordSources(pg, run, []int{0})
		var eligible, reserved bool
		LogosZgcWithWorldStopped(func() {
			eligible = LogosZgcEligible(pg)
			reserved = LogosZgcReserveToSpace(pg)
		})
		if !eligible || !reserved {
			LogosZgcDropPage(pg)
			run.Free(0)
			t.Fatalf("round %d: eligible=%v reserved=%v", round, eligible, reserved)
		}
		dstBase := LogosZgcToSpaceBase(pg, 0)

		type claimRes struct {
			won uintptr
			st  int
		}
		var ra, rb claimRes
		// endRound finishes the round's page through the lifecycle its
		// copies earned. With no committed copy the page has published
		// nothing and leaves through the abandon path, whose contract
		// this is exactly. Once a copy committed, the page owns a filled
		// to-space span carrying the copies' allocations, and abandon's
		// drain, which frees reserved spans with no occupancy vet, must
		// never see it: publish (the mark-termination pause's own
		// seam) hands the filled span to the allocator's swept sets and
		// drains the page's destination list, so the drop that follows
		// frees no span, and forget (the drop's own tail) clears the
		// source list without freeing the run's span, which this closure
		// then hands back to the allocator itself.
		endRound := func() {
			// The page is unregistered below, so the round's source span
			// is handed off and goes back to the allocator: the free vet
			// is allocCount, which the committed copies left at zero.
			defer run.Free(0)
			if ra.st != LogosZgcRelocateDone && rb.st != LogosZgcRelocateDone {
				LogosZgcDropPage(pg)
				return
			}
			LogosZgcWithWorldStopped(func() { LogosZgcPublishToSpaceSpans(pg) })
			if n := LogosZgcPageStateToSpaceLen(pg); n != 0 {
				t.Fatalf("round %d: publish left %d to-space spans: a span carrying committed copies cannot be abandoned", round, n)
			}
			LogosZgcDropPage(pg)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			ra.won, ra.st = LogosZgcCopyOneResult(pg, run, 0, srcA, elem)
		})
		wg.Go(func() {
			<-start
			rb.won, rb.st = LogosZgcCopyOneResult(pg, run, 0, srcB, elem)
		})
		close(start)
		wg.Wait()

		if ra.st != LogosZgcRelocateDone || rb.st != LogosZgcRelocateDone {
			endRound()
			t.Fatalf("round %d: copy statuses %d/%d, want %d/%d", round, ra.st, rb.st, LogosZgcRelocateDone, LogosZgcRelocateDone)
		}
		if ra.won == 0 || rb.won == 0 || ra.won == rb.won {
			endRound()
			t.Fatalf("round %d: winners %#x/%#x, want two distinct committed copies", round, ra.won, rb.won)
		}

		// Identify which reserved slot each copy landed in by its
		// content, then check the destination pointer map against the
		// independently known mask of the source that landed there.
		var dstA, dstB uintptr
		for di := range uintptr(2) {
			d := dstBase + di*elem
			switch LogosZgcWord(d) {
			case patA:
				dstA = d
			case patB:
				dstB = d
			default:
				endRound()
				t.Fatalf("round %d: slot %d word 0 = %#x, want copy A (%#x) or B (%#x)", round, di, LogosZgcWord(d), patA, patB)
			}
		}
		if dstA == 0 || dstB == 0 {
			endRound()
			t.Fatalf("round %d: copies not identified: A=%#x B=%#x", round, dstA, dstB)
		}
		if got := LogosZgcToSpaceSmallBits(pg, 0, dstA); got != mA {
			endRound()
			t.Fatalf("round %d: destination %#x pointer map = %#x, want %#x: concurrent adjacent-slot claim lost the bitmap update", round, dstA, got, mA)
		}
		if got := LogosZgcToSpaceSmallBits(pg, 0, dstB); got != mB {
			endRound()
			t.Fatalf("round %d: destination %#x pointer map = %#x, want %#x: concurrent adjacent-slot claim lost the bitmap update", round, dstB, got, mB)
		}
		// The copied pointer words name the real referents, and the
		// referents still hold their contents: the pointers the copy's
		// map protects are the ones keeping live objects reachable.
		for k := range words {
			if mA&(1<<uint(k)) != 0 {
				if got := LogosZgcWord(dstA + uintptr(k)*ptrSize); got != addrA[k] {
					endRound()
					t.Fatalf("round %d: copy A word %d = %#x, want referent %#x", round, k, got, addrA[k])
				}
				if got := LogosZgcWord(addrA[k]); got != refA[k] {
					endRound()
					t.Fatalf("round %d: referent A word %d = %#x, want %#x", round, k, got, refA[k])
				}
			}
			if mB&(1<<uint(k)) != 0 {
				if got := LogosZgcWord(dstB + uintptr(k)*ptrSize); got != addrB[k] {
					endRound()
					t.Fatalf("round %d: copy B word %d = %#x, want referent %#x", round, k, got, addrB[k])
				}
				if got := LogosZgcWord(addrB[k]); got != refB[k] {
					endRound()
					t.Fatalf("round %d: referent B word %d = %#x, want %#x", round, k, got, refB[k])
				}
			}
		}
		endRound()
	}
}
