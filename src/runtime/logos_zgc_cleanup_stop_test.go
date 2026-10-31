// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// logosZgcStopProbe is the Stop-witness argument type, upstream
// TestCleanupStop's shape: pointer-bearing and at least as wide as the
// tiny class, so the witness's own cleanup is never subject to the
// pointer-free tiny batching that can silently keep a [16]byte witness
// from ever being dequeued.
type logosZgcStopProbe struct {
	v int
	p unsafe.Pointer
}

// Stop after the object was published on a registered page must remove the
// special from the copy's span, and the cancelled cleanup must never run.
// Publication is explicit (a concurrent evacuator's entry); AddCleanup,
// Stop, the GCs, and the cleanup queue below it are real.
func TestLogosZgcCleanupStopAfterRelocation(t *testing.T) {
	if !runtime.LogosZgcCycleEnabled() {
		t.Skip("needs the zgc cycle; run under -gc=zgc")
	}
	// Freeze automatic cycles while a page is registered over live heap.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()

	obj := new([64]byte)
	up := uintptr(unsafe.Pointer(obj))
	base := up &^ (runtime.LogosZgcPageSize - 1)

	// The copy must live outside the registered range, or attach would
	// copy it again instead of hitting the published entry.
	var dst *[64]byte
	for range 1 << 20 {
		dst = new([64]byte)
		if dp := uintptr(unsafe.Pointer(dst)); dp < base || dp >= base+runtime.LogosZgcPageSize {
			break
		}
		dst = nil
	}
	if dst == nil {
		t.Fatal("no copy outside the page under test")
	}
	dstAddr := uintptr(unsafe.Pointer(dst))

	pg := runtime.LogosZgcNewPage(base, 0, 4)
	dropped := false
	defer func() {
		if !dropped {
			runtime.LogosZgcDropPage(pg)
		}
	}()
	if got := runtime.LogosZgcForwardInsert(pg, up, dstAddr); got != dstAddr {
		t.Fatalf("forward winner = %#x, want %#x", got, dstAddr)
	}

	var firedA atomic.Bool
	c := runtime.AddCleanup(obj, func(int) { firedA.Store(true) }, 1)
	runtime.KeepAlive(obj)

	// Unregister before stopping: the analog of Stop after retire, when
	// the forwarding entry is gone.
	runtime.LogosZgcDropPage(pg)
	dropped = true

	c.Stop()
	c.Stop()
	runtime.KeepAlive(obj)

	// Positive control: the same lifecycle without Stop must run.
	objB := new([64]byte)
	var firedB atomic.Bool
	runtime.AddCleanup(objB, func(int) { firedB.Store(true) }, 2)
	runtime.KeepAlive(objB)

	obj, dst, objB = nil, nil, nil
	deadline := time.Now().Add(30 * time.Second)
	extra := 0
	for {
		if firedA.Load() {
			t.Fatal("cleanup ran after Stop")
		}
		if firedB.Load() {
			extra++
			if extra > 5 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("control cleanup never ran")
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// A cleanup attached before its object is forwarded must still cancel via
// its home: the forwarder publishes source->copy while cancellation is in
// flight. Resolving only the handle's home removes the source's special;
// following forwarding would divert to the copy's span, miss, and leave
// the cleanup queued (double-run) with its argument and closure pinned
// (leak) while touching table records retire frees (use-after-release).
// Both Stops run while the entry is published; the drop below is the
// retire analog, after which the stale source resolves to itself.
func TestLogosZgcCleanupStopRacesForward(t *testing.T) {
	if !runtime.LogosZgcCycleEnabled() {
		t.Skip("needs the zgc cycle; run under -gc=zgc")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()

	obj := new([64]byte)
	argObj := new([64]byte)
	var argCollected atomic.Bool
	runtime.AddCleanup(argObj, func(*atomic.Bool) { argCollected.Store(true) }, &argCollected)

	var fired atomic.Bool
	c := runtime.AddCleanup(obj, func(*[64]byte) { fired.Store(true) }, argObj)
	runtime.KeepAlive(obj)
	runtime.KeepAlive(argObj)

	up := uintptr(unsafe.Pointer(obj))
	base := up &^ (runtime.LogosZgcPageSize - 1)

	var dst *[64]byte
	for range 1 << 20 {
		dst = new([64]byte)
		if dp := uintptr(unsafe.Pointer(dst)); dp < base || dp >= base+runtime.LogosZgcPageSize {
			break
		}
		dst = nil
	}
	if dst == nil {
		t.Fatal("no copy outside the page under test")
	}
	dstAddr := uintptr(unsafe.Pointer(dst))

	pg := runtime.LogosZgcNewPage(base, 0, 4)
	dropped := false
	defer func() {
		if !dropped {
			runtime.LogosZgcDropPage(pg)
		}
	}()
	if got := runtime.LogosZgcForwardInsert(pg, up, dstAddr); got != dstAddr {
		t.Fatalf("forward winner = %#x, want %#x", got, dstAddr)
	}
	runtime.KeepAlive(obj)
	runtime.KeepAlive(dst)

	// Cancel while forwarded: a forwarding follow would land on the
	// copy's span and miss the source's record.
	c.Stop()
	c.Stop()
	runtime.KeepAlive(obj)
	runtime.KeepAlive(dst)

	// Retire analog for lookup (same convention as StopAfterRelocation
	// above): DropPage unregisters the page so the entry is gone and
	// Remap returns the stale source. Only a home lookup can have
	// removed the record without touching table records that Retire
	// frees; the Remap check plus the live queue below (argCollected
	// must still fire) proves it. Source-release safety follows from
	// the same ownership: the home span carries the special, so
	// selection excludes it and Release cannot take it while live.
	runtime.LogosZgcDropPage(pg)
	dropped = true
	if got := runtime.LogosZgcRemap(up); got != up {
		t.Fatalf("post-drop remap(%#x) = %#x, want the stale source itself", up, got)
	}

	obj, dst, argObj = nil, nil, nil
	deadline := time.Now().Add(30 * time.Second)
	for {
		if fired.Load() {
			t.Fatal("cleanup ran after Stop despite home lookup")
		}
		if argCollected.Load() {
			// The argument became collectable only because Stop
			// unlinked the home record; a miss would pin it until
			// the sweep queued and ran the cleanup above.
			if fired.Load() {
				t.Fatal("cleanup ran after Stop")
			}
			break
		}
		if time.Now().After(deadline) {
			if !argCollected.Load() {
				t.Fatal("cleanup argument never collected: Stop leaked the home record")
			}
			t.Fatal("cleanup argument never collected")
		}
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// After release, a remap pass, and retire, the stale source no longer
// resolves to its copy, so a Stop-time canonicalization alone cannot find a
// moved special. Pins the expiry half; the test above pins the handle half.
func TestLogosZgcCleanupStaleSourceExpiresAtRetire(t *testing.T) {
	base, free := logosZgcPlace(t)
	defer free()
	const size = 4096
	pg := runtime.LogosZgcNewPage(base, size, 1)
	retired := false
	defer func() {
		if !retired {
			runtime.LogosZgcDropPage(pg)
		}
	}()

	src := base
	runtime.LogosZgcFill(src, size, 0xa5)
	dst := base + runtime.LogosZgcPageSize + 0x800
	if got := runtime.LogosZgcRelocate(pg, src, size, dst); got != dst {
		t.Fatalf("relocate = %#x, want %#x", got, dst)
	}
	if !runtime.LogosZgcRelease(pg) {
		t.Fatal("release refused a fully published page")
	}
	if got := runtime.LogosZgcRemap(src); got != dst {
		t.Fatalf("pre-retire remap(%#x) = %#x, want copy %#x", src, got, dst)
	}
	// Retire-pause probes under a real world stop: zgcRetirePage asserts
	// a stopped world under staticlockranking, and the collector only
	// retires inside zgcRetirePendingPages in the mark-termination pause.
	// Relocation and release above stay outside because evacuators copy
	// concurrently and the release seam takes no STW assertion.
	var retiredEarly, retiredLate bool
	runtime.LogosZgcWithWorldStopped(func() { retiredEarly = runtime.LogosZgcRetire(pg) })
	if retiredEarly {
		t.Fatal("page retired before a remap pass completed after release")
	}
	runtime.LogosZgcRemapDone()
	runtime.LogosZgcWithWorldStopped(func() { retiredLate = runtime.LogosZgcRetire(pg) })
	if !retiredLate {
		t.Fatal("page not retired after a remap pass")
	}
	retired = true
	if got := runtime.LogosZgcRemap(src); got != src {
		t.Fatalf("post-retire remap(%#x) = %#x, want the stale source itself", src, got)
	}

	// Reuse answers for the new occupant, never the retired copy.
	runtime.LogosZgcFill(src, size, 0x5a)
	reuse := runtime.LogosZgcNewPage(base, 0, 1)
	defer runtime.LogosZgcFreePage(reuse)
	if runtime.LogosZgcPageFor(src) != reuse {
		t.Fatal("reused range is not registered to the new page")
	}
	if got := runtime.LogosZgcRemap(src); got != src {
		t.Fatalf("fresh word into the reused range remaps to %#x, want %#x", got, src)
	}
}

// logosZgcPlaceOutside allocates fresh T values until one lands outside
// the page-under-test's granule [base, base+LogosZgcPageSize) and inside
// a granule no registered page covers, rejecting same-granule placement
// with a fatal. The attach fixtures own every object they copy or attach
// to: a synthetic page keeps its whole-region source range, so an attach
// resolving into any covered granule copies that object and consumes a
// forwarding slot from a table the fixture neither counts nor sizes.
func logosZgcPlaceOutside[T any](t *testing.T, base uintptr, what string) *T {
	t.Helper()
	for range 1 << 20 {
		p := new(T)
		if pp := uintptr(unsafe.Pointer(p)); (pp < base || pp >= base+runtime.LogosZgcPageSize) && runtime.LogosZgcPageSlotFree(pp) {
			return p
		}
	}
	t.Fatalf("no %s outside the page under test in an unregistered granule", what)
	return nil
}

// Concurrent production attachment versus a real copy commit on a live
// registered page. The attach side is AddCleanup itself: zgcAttachResolve,
// the preemptible zgcAttachEnsureCopy copy-or-adopt (which refills a
// to-space span and runs the production claim-and-commit pipeline when the
// object is still unpublished), and the M-held addspecialStable splice. The
// copy side is LogosZgcRelocate, the single authoritative zgcRelocateObject
// commit: gate-held veto recheck, one memmove, CAS forwardPublish, identity
// move. The raw ForwardInsert seam is deliberately absent, since it bypasses the
// pin veto and the CAS convergence this race exists to observe, so it can
// prove neither. Whichever side wins the publish, exactly one copy exists,
// forwarding and the attach agree on that one home, Stop splices the record
// out of that home, and the surviving control cleanup fires exactly once
// after the referent dies.
func TestLogosZgcCleanupAttachRacesCopyCommit(t *testing.T) {
	if !runtime.LogosZgcCycleEnabled() {
		t.Skip("needs the zgc cycle; run under -gc=zgc")
	}
	// Freeze automatic cycles while a page is registered over live heap.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()

	obj := new([64]byte)
	up := uintptr(unsafe.Pointer(obj))
	base := up &^ (runtime.LogosZgcPageSize - 1)

	// The commit's destination and the raced cleanup's argument must be
	// placed before registration, outside the registered granule in a
	// granule no registered page covers. A synthetic page keeps its
	// whole-region source range, so an attach resolving into any
	// covered granule copies that object and consumes a forwarding slot
	// from a table this fixture neither counts nor sizes: the
	// forwarding-table-full throw that ended bg 188 before the
	// concurrent attach. Registration is per granule, so slot freedom,
	// not just range, is the predicate.
	dst := logosZgcPlaceOutside[[64]byte](t, base, "copy destination")
	dstAddr := uintptr(unsafe.Pointer(dst))
	argObj := logosZgcPlaceOutside[logosZgcStopProbe](t, base, "cleanup argument probe")

	pg := runtime.LogosZgcNewPage(base, 0, 1)
	dropped := false
	defer func() {
		if !dropped {
			runtime.LogosZgcDropPage(pg)
		}
	}()

	// The raced cleanup's argument is a separate object whose own
	// cleanup signals when it is collectable. The record holds its
	// argument, so only a real home splice-out releases it: this is the
	// witness that Stop matched the record on the home, not a silent
	// no-op against a moved or missing special. The witness type is
	// upstream TestCleanupStop's pointer-bearing shape: a pointer-free
	// 16-byte object sits in the tiny allocator, where a cleanup on
	// batched storage may never be dequeued, so [16]byte was a broken
	// collectability oracle here, not evidence of a leaked record.
	// argObj itself was placed before registration above; its own
	// attach resolves an unregistered granule, the ordinary no-copy
	// path, and stays after registration exactly like the raced attach
	// below.
	argDone := make(chan struct{}, 1)
	runtime.AddCleanup(argObj, func(chan struct{}) { argDone <- struct{}{} }, argDone)
	runtime.KeepAlive(argObj)

	var firedA atomic.Int32
	start := make(chan struct{})
	attachDone := make(chan struct{})
	copyDone := make(chan struct{})
	var c runtime.Cleanup
	var won atomic.Uintptr

	go func() {
		<-start
		// Production attach: resolve, ensure-copy-or-adopt, splice.
		c = runtime.AddCleanup(obj, func(*logosZgcStopProbe) { firedA.Add(1) }, argObj)
		close(attachDone)
	}()
	go func() {
		<-start
		// Production copy commit. A loser adopts the winner through
		// the gate-held forward recheck before any memmove, so the
		// reported address is the one published copy either way.
		won.Store(runtime.LogosZgcRelocate(pg, up, unsafe.Sizeof(*obj), dstAddr))
		close(copyDone)
	}()
	close(start)
	<-attachDone
	<-copyDone

	home := runtime.LogosZgcRemap(up)
	if home == up {
		t.Fatal("no copy was published for an unpublished source of a registered page")
	}
	if w := won.Load(); w != home {
		t.Fatalf("copy commit reported %#x, forwarding resolves %#x", w, home)
	}
	if again := runtime.LogosZgcRemap(up); again != home {
		t.Fatalf("remap moved %#x -> %#x across calls", home, again)
	}
	if runtime.LogosZgcPageFor(home) == pg {
		t.Fatalf("home %#x resolves into the registered source page", home)
	}
	runtime.GC()
	runtime.KeepAlive(obj)
	if again := runtime.LogosZgcRemap(up); again != home {
		t.Fatalf("canonical home moved %#x -> %#x across a GC", home, again)
	}

	// Stop twice: the second call must walk the home list and find
	// nothing of this id.
	c.Stop()
	c.Stop()
	runtime.KeepAlive(obj)

	if home == dstAddr {
		// The commit won the publish and the home is an ordinary heap
		// object outside the page, so the full death lifecycle is
		// observable. A second attach adopts the published copy
		// through the production forward fast path, with no second copy,
		// and must fire exactly once from that home.
		var firedCtl atomic.Int32
		ctlDone := make(chan struct{}, 2)
		runtime.AddCleanup(obj, func(int) {
			firedCtl.Add(1)
			ctlDone <- struct{}{}
		}, 1)
		runtime.KeepAlive(obj)
		runtime.KeepAlive(dst)

		obj, dst, argObj = nil, nil, nil
		// SetGCPercent(-1) above froze automatic collection, so the
		// deaths must be forced: the home is dst's ordinary mcentral
		// span, and only an explicit cycle sweeps it, releases argObj
		// from the spliced-out record's last reference, and queues the
		// cleanups the waits below observe.
		runtime.GC()
		select {
		case <-argDone:
		case <-time.After(30 * time.Second):
			t.Fatal("Stop leaked the home record: its argument never became collectable")
		}
		if n := firedA.Load(); n != 0 {
			t.Fatalf("stopped cleanup ran %d times after Stop", n)
		}
		select {
		case <-ctlDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("cleanup on the adopted home never ran (fired %d)", firedCtl.Load())
		}
		runtime.GC()
		if n := firedCtl.Load(); n != 1 {
			t.Fatalf("adopted cleanup ran %d times, want exactly 1", n)
		}
		if n := firedA.Load(); n != 0 {
			t.Fatalf("stopped cleanup ran %d times after a later GC", n)
		}
	} else {
		// The attach won the publish and the home is the runtime's own
		// to-space copy inside the attach's claim. A to-space span is
		// not on the swept sets until publication, so a death-fire
		// from that home is not observable without publication seams;
		// the witnessed contract here is single-copy convergence plus
		// Stop's home splice-out, proven by the argument becoming
		// collectable. The record is unlinked before the page drop, so
		// no live special sits on a span the drain frees.
		runtime.KeepAlive(dst)
		obj, dst, argObj = nil, nil, nil
		// Same forced cycle: with automatic collection frozen, argObj
		// would otherwise never die and the splice-out witness below
		// could never fire, regardless of where the record lived.
		runtime.GC()
		select {
		case <-argDone:
		case <-time.After(30 * time.Second):
			t.Fatal("Stop leaked the home record: its argument never became collectable")
		}
		runtime.GC()
		if n := firedA.Load(); n != 0 {
			t.Fatalf("stopped cleanup ran %d times after a later GC", n)
		}
	}

	runtime.LogosZgcDropPage(pg)
	dropped = true
}

// The same race with SetFinalizer: the g0 attach loop (preemptible
// ensure-copy-or-adopt outside systemstack, M-held addfinalizer splice
// inside) against the real copy commit. Removal is removespecial, which
// canonicalizes the word and resolves only the home span, so
// SetFinalizer(obj, nil) must find the record on whichever span won the
// publish. The deterministic removal probe is the re-attach: a removal
// that missed the home leaves the record in place and the next
// SetFinalizer throws "finalizer already set".
func TestLogosZgcFinalizerAttachRacesCopyCommit(t *testing.T) {
	if !runtime.LogosZgcCycleEnabled() {
		t.Skip("needs the zgc cycle; run under -gc=zgc")
	}
	// Freeze automatic cycles while a page is registered over live heap.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()

	obj := new([64]byte)
	up := uintptr(unsafe.Pointer(obj))
	base := up &^ (runtime.LogosZgcPageSize - 1)

	// Same ownership discipline as the cleanup race above: the copy
	// destination is placed before registration, outside the registered
	// granule in a granule no registered page covers, so the only
	// in-granule object the test copies or forwards is obj itself and
	// the table is sized for exactly that one owned source.
	dst := logosZgcPlaceOutside[[64]byte](t, base, "copy destination")
	dstAddr := uintptr(unsafe.Pointer(dst))

	pg := runtime.LogosZgcNewPage(base, 0, 1)
	dropped := false
	defer func() {
		if !dropped {
			runtime.LogosZgcDropPage(pg)
		}
	}()

	var fired atomic.Int32
	var got atomic.Uintptr
	finDone := make(chan struct{}, 2)
	fin := func(o *[64]byte) {
		fired.Add(1)
		got.Store(uintptr(unsafe.Pointer(o)))
		finDone <- struct{}{}
	}

	start := make(chan struct{})
	attachDone := make(chan struct{})
	copyDone := make(chan struct{})
	var won atomic.Uintptr

	go func() {
		<-start
		runtime.SetFinalizer(obj, fin)
		close(attachDone)
	}()
	go func() {
		<-start
		won.Store(runtime.LogosZgcRelocate(pg, up, unsafe.Sizeof(*obj), dstAddr))
		close(copyDone)
	}()
	close(start)
	<-attachDone
	<-copyDone

	home := runtime.LogosZgcRemap(up)
	if home == up {
		t.Fatal("no copy was published for an unpublished source of a registered page")
	}
	if w := won.Load(); w != home {
		t.Fatalf("copy commit reported %#x, forwarding resolves %#x", w, home)
	}
	runtime.GC()
	runtime.KeepAlive(obj)
	if again := runtime.LogosZgcRemap(up); again != home {
		t.Fatalf("canonical home moved %#x -> %#x across a GC", home, again)
	}

	if home == dstAddr {
		// The commit won: the record sits on an ordinary heap span, so
		// the death path is fully observable. This is the
		// moved-special positive path: the attach adopted the copy,
		// the referent died, and the finalizer must run exactly once
		// with the home address the record was spliced at.
		obj, dst = nil, nil
		// Force the cycle: SetGCPercent(-1) above froze automatic
		// collection, and the record sits on dst's ordinary mcentral
		// span, so only an explicit GC sweeps that span and queues the
		// finalizer the wait below observes.
		runtime.GC()
		select {
		case <-finDone:
		case <-time.After(30 * time.Second):
			t.Fatal("finalizer on the adopted home never ran")
		}
		if n := fired.Load(); n != 1 {
			t.Fatalf("finalizer ran %d times, want exactly 1", n)
		}
		if a := got.Load(); a != home {
			t.Fatalf("finalizer received %#x, want the home %#x", a, home)
		}
		runtime.GC()
		if n := fired.Load(); n != 1 {
			t.Fatalf("finalizer ran %d times after a later GC, want 1", n)
		}
	} else {
		// The attach won and the record sits on the runtime's internal
		// to-space copy, which is not on the swept sets until
		// publication, so a death-fire is not observable here. The
		// witnessed contract is removal on the canonical home: the
		// re-attach after removal must not throw Exists, and the
		// second removal unlinks the re-attached record so the page
		// drop below frees no span that still carries a special.
		runtime.SetFinalizer(obj, nil)
		runtime.KeepAlive(obj)
		runtime.SetFinalizer(obj, fin)
		runtime.KeepAlive(obj)
		runtime.SetFinalizer(obj, nil)
		runtime.KeepAlive(obj)
		runtime.KeepAlive(dst)
		obj, dst = nil, nil
		runtime.GC()
		runtime.GC()
		if n := fired.Load(); n != 0 {
			t.Fatalf("finalizer ran %d times after removal from the home", n)
		}
	}

	runtime.LogosZgcDropPage(pg)
	dropped = true
}
