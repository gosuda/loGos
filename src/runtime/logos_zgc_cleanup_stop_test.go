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
	if runtime.LogosZgcRetire(pg) {
		t.Fatal("page retired before a remap pass completed after release")
	}
	runtime.LogosZgcRemapDone()
	if !runtime.LogosZgcRetire(pg) {
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
