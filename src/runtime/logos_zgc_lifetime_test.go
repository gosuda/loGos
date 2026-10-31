// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	. "runtime"
	"runtime/debug"
	"testing"
	"unsafe"
)

// Lifetime regression: leases and transient read domains against
// retire, publication and deferred header free, over the real page,
// claim and span machinery. Every scenario holds its reader across a
// stop-the-world window, proves the reclaim is deferred, then drains
// exactly once with the directory and the private destination list
// consistent. Fixtures use the production paths: LogosZgcAllocRun,
// LogosZgcRecordSources, LogosZgcEligible, LogosZgcRelocate,
// LogosZgcRelease, LogosZgcRetire.

// logosZgcLifetimePage builds one registered page over one real heap
// span and vets it. The granule is reserved by LogosZgcAllocGranule: one
// contiguous page-free 2 MB region tiled end to end by the fixture's
// heap spans, source span first. The caller owns the GC-percent defer,
// span 0's cleanup and the page cleanup; the helper owns and frees the
// reserved rest of the granule.
func logosZgcLifetimePage(t *testing.T, objects int) (*LogosZgcSpanRun, *LogosZgcPage, uintptr, uintptr) {
	t.Helper()
	run := LogosZgcAllocGranule()
	if run == nil {
		t.Fatal("no tight span run on a granule no page owns after 8 trials")
	}
	t.Cleanup(func() {
		for i := run.Len() - 1; i >= 1; i-- {
			run.Free(i)
		}
	})
	elem := run.Elem(0)
	if elem == 0 {
		t.Cleanup(func() { run.Free(0) })
		t.Fatal("source span has no objects")
	}
	region := run.Base(0) &^ (LogosZgcPageSize - 1)
	pg := LogosZgcNewPage(region, uintptr(objects)*elem, objects)
	LogosZgcRecordSources(pg, run, []int{0})
	var eligible bool
	LogosZgcWithWorldStopped(func() { eligible = LogosZgcEligible(pg) })
	if !eligible {
		t.Cleanup(func() {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
			run.Free(0)
		})
		t.Fatal("single-source page refused by the selection vet")
	}
	return run, pg, region, elem
}

// logosZgcLifetimeCopy relocates the span's first object, releases the
// page and completes a remap pass: the retire precondition. It returns
// the source address and the destination slice; the caller must
// KeepAlive the slice past its final assertion.
func logosZgcLifetimeCopy(t *testing.T, pg *LogosZgcPage, run *LogosZgcSpanRun, elem uintptr) (uintptr, []byte) {
	src := run.Base(0)
	dst := make([]byte, elem)
	if won := LogosZgcRelocate(pg, src, elem, uintptr(unsafe.Pointer(&dst[0]))); won != uintptr(unsafe.Pointer(&dst[0])) {
		t.Fatalf("relocate published %#x, want the destination %#x", won, uintptr(unsafe.Pointer(&dst[0])))
	}
	if !LogosZgcRelease(pg) {
		t.Fatal("release refused a fully published page")
	}
	LogosZgcRemapDone()
	return src, dst
}

// TestLogosZgcRetireDefersUnderLease proves a worker-style lease held
// across a stop-the-world window blocks the retire attempt before the
// page is unpublished, and that releasing it lets the next pass reclaim
// the whole page.
func TestLogosZgcRetireDefersUnderLease(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run, pg, region, elem := logosZgcLifetimePage(t, 1)
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
		run.Free(0)
	})
	src, dst := logosZgcLifetimeCopy(t, pg, run, elem)
	defer func() { KeepAlive(dst) }()

	var leased *LogosZgcPage
	defer func() {
		if leased != nil {
			LogosZgcPageRelease(leased)
		}
	}()
	leased = LogosZgcPageAcquire(src)
	if leased == nil {
		t.Fatal("acquire missed the page's own source address")
	}
	var early bool
	LogosZgcWithWorldStopped(func() { early = LogosZgcRetire(pg) })
	if early {
		t.Fatal("retire reclaimed a leased page")
	}
	if LogosZgcPageFor(src) != pg {
		t.Fatal("leased page was unpublished before its lease drained")
	}
	LogosZgcPageRelease(leased)
	leased = nil

	var late bool
	LogosZgcWithWorldStopped(func() { late = LogosZgcRetire(pg) })
	if !late {
		t.Fatal("retire did not reclaim after the lease drained")
	}
	run.MarkFreed(0)
	retired = true
	if LogosZgcPageFor(src) != nil || !LogosZgcPageSlotFree(region) {
		t.Fatal("reclaimed page still resolves through the directory")
	}
}

// TestLogosZgcRetireDefersUnderTransientDomain proves the transient
// reader leg: a page that loses its drain sample to an open read domain
// is unpublished while its header and forwarding table stay alive for
// the reader that resolved it before the unpublish; the next drained
// pass frees it exactly once.
func TestLogosZgcRetireDefersUnderTransientDomain(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run, pg, region, elem := logosZgcLifetimePage(t, 1)
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
		run.Free(0)
	})
	src, dst := logosZgcLifetimeCopy(t, pg, run, elem)
	defer func() { KeepAlive(dst) }()

	domainOpen := false
	defer func() {
		if domainOpen {
			LogosZgcTransientLeave()
		}
	}()
	LogosZgcTransientEnter()
	domainOpen = true
	reader := LogosZgcPageFor(src)
	if reader == nil {
		t.Fatal("transient lookup missed the registered page")
	}
	var early bool
	LogosZgcWithWorldStopped(func() { early = LogosZgcRetire(pg) })
	if early {
		t.Fatal("retire freed metadata while a transient domain was open")
	}
	if LogosZgcPageFor(src) != nil {
		t.Fatal("drain-failed page stayed published")
	}
	if to, ok := LogosZgcForward(reader, src); !ok {
		t.Fatalf("unpublished descriptor lost forwarding for its pre-unpublish reader: to=%#x", to)
	}
	LogosZgcTransientLeave()
	domainOpen = false

	var late bool
	LogosZgcWithWorldStopped(func() { late = LogosZgcRetire(pg) })
	if !late {
		t.Fatal("retire did not reclaim after the domain drained")
	}
	run.MarkFreed(0)
	retired = true
	if LogosZgcPageFor(src) != nil || !LogosZgcPageSlotFree(region) {
		t.Fatal("reclaimed page still resolves through the directory")
	}
}

// TestLogosZgcNewbornFreeDefersUnderTransientReader proves the
// abandoned-newborn leg: a registered page that never released and
// never published, freed while a transient reader is live, is not freed
// but queued with its header intact. Its retiring tombstone reserves the
// original granule while the descriptor waits; a reserved destination
// list does not block it — an abandoned descriptor drains that reservation
// as part of the reclaim; when the reader leaves, the next drained pass
// frees it exactly once.
func TestLogosZgcNewbornFreeDefersUnderTransientReader(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run, pg, region, _ := logosZgcLifetimePage(t, 1)
	t.Cleanup(func() { run.Free(0) })
	// queued marks the request handed to the drain path: cleanup must
	// not free the descriptor again; freed marks the drain done.
	queued, freed := false, false
	domainOpen := false
	t.Cleanup(func() {
		if freed {
			return
		}
		if queued {
			if domainOpen {
				LogosZgcTransientLeave()
				domainOpen = false
			}
			LogosZgcWithWorldStopped(func() { LogosZgcDrainRetirePending() })
			return
		}
		LogosZgcForgetPage(pg)
		LogosZgcDropPage(pg)
	})

	reserved := false
	LogosZgcWithWorldStopped(func() { reserved = LogosZgcReserveToSpace(pg) })
	if !reserved {
		t.Fatal("to-space reservation failed")
	}
	if n := LogosZgcPageStateToSpaceLen(pg); n != 1 {
		t.Fatalf("reservation lists %d destination spans, want 1", n)
	}

	defer func() {
		if domainOpen {
			LogosZgcTransientLeave()
		}
	}()
	LogosZgcTransientEnter()
	domainOpen = true
	base := LogosZgcRetirePendingLen()
	LogosZgcWithWorldStopped(func() { LogosZgcFreePage(pg) })
	queued = true
	if n := LogosZgcRetirePendingLen(); n != base+1 {
		t.Fatalf("deferred header queue holds %d entries, want baseline %d + 1", n, base)
	}
	if LogosZgcPageSlotFree(region) {
		t.Fatal("deferred newborn lost its retiring granule reservation")
	}
	if _, ok := LogosZgcForward(pg, region); ok {
		t.Fatal("newborn header carries forwarding entries")
	}
	LogosZgcTransientLeave()
	domainOpen = false

	LogosZgcWithWorldStopped(func() { LogosZgcDrainRetirePending() })
	if n := LogosZgcRetirePendingLen(); n > base {
		t.Fatalf("drain did not consume the deferred descriptor: %d queued, baseline %d", n, base)
	}
	LogosZgcWithWorldStopped(func() { LogosZgcDrainRetirePending() })
	if n := LogosZgcRetirePendingLen(); n > base {
		t.Fatalf("second drain resurrected descriptors: %d queued, baseline %d", n, base)
	}
	freed = true
}

// TestLogosZgcRetiringRegionDefersSelection proves that an unpublished
// descriptor retains ownership of its original granule while a transient
// acquisition domain prevents reclaim. Each cycle runs the real pending
// drain, the authoritative selection claim, and the abandon path; the
// claim must skip the reserved region, so its queue cardinality stays
// one. Releasing the domain lets the same pending consumer reclaim it and
// release the slot.
func TestLogosZgcRetiringRegionDefersSelection(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run, pg, region, elem := logosZgcLifetimePage(t, 1)
	src := run.Base(0)
	nObjects := run.Nelems(0)
	domainOpen := false
	t.Cleanup(func() { run.Free(0) })

	queued, freed := false, false
	t.Cleanup(func() {
		if freed {
			return
		}
		if queued {
			if domainOpen {
				LogosZgcTransientLeave()
				domainOpen = false
			}
			LogosZgcWithWorldStopped(func() { LogosZgcDrainRetirePending() })
			return
		}
		LogosZgcForgetPage(pg)
		LogosZgcDropPage(pg)
	})
	defer func() {
		if domainOpen {
			LogosZgcTransientLeave()
		}
	}()
	LogosZgcTransientEnter()
	domainOpen = true
	if LogosZgcPageFor(src) != pg {
		t.Fatal("selection fixture did not resolve before abandon")
	}
	base := LogosZgcRetirePendingLen()
	LogosZgcWithWorldStopped(func() { LogosZgcFreePage(pg) })
	queued = true
	if n := LogosZgcRetirePendingRegionCount(region); n != 1 {
		t.Fatalf("queued %d descriptors for the initial region, want 1", n)
	}
	if LogosZgcPageSlotFree(region) {
		t.Fatal("unpublished descriptor did not reserve its original region")
	}
	if LogosZgcPageFor(src) != nil {
		t.Fatal("fresh lookup resolved an unpublished descriptor")
	}

	for i := range 16 {
		var candidate *LogosZgcPage
		LogosZgcWithWorldStopped(func() {
			LogosZgcDrainRetirePending()
			candidate = LogosZgcClaimGranule(region, elem, int(nObjects))
			if candidate != nil {
				LogosZgcFreePage(candidate)
			}
		})
		if n := LogosZgcRetirePendingRegionCount(region); n != 1 {
			t.Fatalf("cycle %d accumulated %d descriptors for the initial region, want 1", i, n)
		}
		if LogosZgcPageSlotFree(region) {
			t.Fatalf("cycle %d released the still-pending region", i)
		}
		if LogosZgcPageFor(src) != nil {
			t.Fatalf("cycle %d resolved a retiring descriptor to a fresh lookup", i)
		}
	}

	LogosZgcTransientLeave()
	domainOpen = false
	LogosZgcWithWorldStopped(func() { LogosZgcDrainRetirePending() })
	if n := LogosZgcRetirePendingRegionCount(region); n != 0 {
		t.Fatalf("drained queue retains %d descriptors for the initial region", n)
	}
	if LogosZgcPageFor(src) != nil || !LogosZgcPageSlotFree(region) {
		t.Fatal("reclaimed descriptor did not release its original region")
	}
	LogosZgcWithWorldStopped(func() { LogosZgcDrainRetirePending() })
	if n := LogosZgcRetirePendingRegionCount(region); n != 0 {
		t.Fatalf("second drain resurrected %d descriptors for the initial region", n)
	}
	if n := LogosZgcRetirePendingLen(); n > base {
		t.Fatalf("drain retained unrelated deferred descriptors: got %d, baseline %d", n, base)
	}
	var candidate *LogosZgcPage
	LogosZgcWithWorldStopped(func() {
		candidate = LogosZgcClaimGranule(region, elem, int(nObjects))
		if candidate != nil {
			LogosZgcFreePage(candidate)
		}
	})
	if candidate == nil {
		t.Fatal("reclaimed region did not accept a fresh authoritative claim")
	}
	if !LogosZgcPageSlotFree(region) {
		t.Fatal("fresh claim did not release its region after abandon")
	}
	freed = true
}

// TestLogosZgcRetireLeavesUnreleasedPage proves the retire contract for
// an ordinary live descriptor: an unreleased page that was never
// abandoned is refused whole — not freed, not queued — and stays
// query-valid afterwards.
func TestLogosZgcRetireLeavesUnreleasedPage(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run, pg, _, _ := logosZgcLifetimePage(t, 1)
	t.Cleanup(func() {
		LogosZgcForgetPage(pg)
		LogosZgcDropPage(pg)
		run.Free(0)
	})
	src := run.Base(0)

	base := LogosZgcRetirePendingLen()
	var refused bool
	LogosZgcWithWorldStopped(func() { refused = !LogosZgcRetire(pg) })
	if !refused {
		t.Fatal("retire reclaimed an unreleased, non-abandoned page")
	}
	if n := LogosZgcRetirePendingLen(); n != base {
		t.Fatalf("retire queued the unreleased page: %d queued, baseline %d", n, base)
	}
	if LogosZgcPageFor(src) != pg {
		t.Fatal("refused page no longer resolves through the directory")
	}
}

// TestLogosZgcPublishDefersUnderLease proves publication against a held
// copy lease: a leased page is skipped whole with its destination list
// retained; after the lease drains, the next pause publishes the
// destination span, empties the list, resets the cursor, and a fresh
// reservation appends into a new private list with no seal in the way.
func TestLogosZgcPublishDefersUnderLease(t *testing.T) {
	if !LogosZgcCycleEnabled() {
		t.Skip("zgc cycle disabled: to-space publication is a cycle-mode pause step")
	}
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run, pg, _, _ := logosZgcLifetimePage(t, 1)
	t.Cleanup(func() {
		LogosZgcForgetPage(pg)
		LogosZgcDropPage(pg)
		run.Free(0)
	})
	src := run.Base(0)

	var leased *LogosZgcPage
	defer func() {
		if leased != nil {
			LogosZgcPageRelease(leased)
		}
	}()

	reserved := false
	LogosZgcWithWorldStopped(func() { reserved = LogosZgcReserveToSpace(pg) })
	if !reserved {
		t.Fatal("to-space reservation failed")
	}
	dest := LogosZgcToSpaceBase(pg, 0)
	if dest == 0 {
		t.Fatal("reservation lists no destination span")
	}
	if dest == src {
		t.Fatal("destination span is the source span")
	}
	if n := LogosZgcPageStateToSpaceLen(pg); n != 1 {
		t.Fatalf("reservation lists %d destination spans, want 1", n)
	}
	if LogosZgcSpanSwept(dest) {
		t.Fatal("reserved destination already carries the published sweepgen")
	}

	leased = LogosZgcPageAcquire(src)
	if leased == nil {
		t.Fatal("acquire missed the page's own source address")
	}
	LogosZgcWithWorldStopped(func() { LogosZgcPublishToSpaceSpans(pg) })
	if n := LogosZgcPageStateToSpaceLen(pg); n != 1 {
		t.Fatal("publication drained the destination list of a leased page")
	}
	if LogosZgcSpanSwept(dest) {
		t.Fatal("publication pushed a destination span of a leased page")
	}
	LogosZgcPageRelease(leased)
	leased = nil

	LogosZgcWithWorldStopped(func() { LogosZgcPublishToSpaceSpans(pg) })
	if n := LogosZgcPageStateToSpaceLen(pg); n != 0 {
		t.Fatalf("publication retained %d destination spans after the lease drained", n)
	}
	if !LogosZgcSpanSwept(dest) {
		t.Fatal("published destination span does not carry the current sweepgen")
	}
	if c := LogosZgcPageStateCursor(pg); c != 0 {
		t.Fatalf("cursor %d after publication, want 0", c)
	}

	refilled := false
	LogosZgcWithWorldStopped(func() { refilled = LogosZgcReserveToSpace(pg) })
	if !refilled {
		t.Fatal("fresh reservation after publication refused")
	}
	if n := LogosZgcPageStateToSpaceLen(pg); n != 1 {
		t.Fatalf("fresh private list holds %d spans, want 1", n)
	}
}
