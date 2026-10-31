// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && arm64

package runtime_test

import (
	"internal/runtime/atomic"
	"testing"
	"unsafe"

	. "runtime"
)

// W1d hook unit test. Calls the zgc atomic Loadp slow hook directly, so
// it covers the shared Go logic on amd64 and arm64; the arm64 assembly
// frame that reaches it is pinned by TestZgcArm64LoadpOutOfLine and by
// objdump.
//
// This test lives in package runtime_test (not package runtime) because
// an internal runtime test file cannot import testing: testing imports
// runtime, so that import is an import cycle in the test binary. The
// private hook and color state it needs come through the test-only seam
// in export_logos_zgc_atomic_loadp_test.go, which imports no testing.

func TestZgcAtomicLoadpSlowHook(t *testing.T) {
	saved := LogosZgcColorSave()
	defer func() {
		LogosZgcColorRestore(saved)
	}()

	// Colors inert: the hook is the identity, high bits intact.
	for _, w := range []uintptr{0, 1, 0x5555_0000_1000, ^uintptr(0)} {
		slot := w
		if got := LogosZgcAtomicLoadpSlow(&slot, w); got != w || slot != w {
			t.Fatalf("inert word %#x: hook %#x slot %#x, want identity", w, got, slot)
		}
	}

	heap := new([8]uintptr)
	heapAddr := uintptr(unsafe.Pointer(heap))
	if uint64(heapAddr) > LogosZgcAddrMask {
		t.Skipf("heap address %#x above the 4 TB canonical field", heapAddr)
	}

	LogosZgcSelectSoftware(LogosZgcMark0)
	good, bad := uintptr(LogosZgcSoftBits(LogosZgcMark0)), uintptr(LogosZgcSoftBits(LogosZgcMark1))

	// Good-colored heap word: canonical, slot untouched, no heal.
	slot := heapAddr + good
	before := LogosZgcHeals()
	if got := LogosZgcAtomicLoadpSlow(&slot, heapAddr+good); got != heapAddr || slot != heapAddr+good || LogosZgcHeals() < before {
		t.Fatalf("good word: hook %#x slot %#x heals %d, want %#x untouched", got, slot, LogosZgcHeals()-before, heapAddr)
	}

	// Bad color on ordinary heap: a live span covers the address but
	// no registered zgc source does, so the hook strips to the
	// canonical address without touching the slot and without
	// healing. The word never escapes colored; the stale slot word
	// stays valid because ordinary heap never relocates, and every
	// resolution path re-strips it through the same strip rule.
	slot = heapAddr + bad
	before = LogosZgcHeals()
	if got := LogosZgcAtomicLoadpSlow(&slot, heapAddr+bad); got != heapAddr || slot != heapAddr+bad || LogosZgcHeals() != before {
		t.Fatalf("ordinary-heap word: hook %#x slot %#x heals %d, want %#x stripped, slot untouched", got, slot, LogosZgcHeals()-before, heapAddr)
	}

	// Foreign high word: address bits, never stripped.
	slot = 0x5555_0000_1000
	if got := LogosZgcAtomicLoadpSlow(&slot, slot); got != slot {
		t.Fatalf("foreign word: hook %#x, want unchanged", got)
	}

	// Bad color on a registered page: canonical return, slot healed
	// to the good color, exactly one heal; a second load fast-paths.
	base, raw := logosZgcLoadpTestMap(t)
	pg := LogosZgcNewPage(base, 0, 16)
	defer func() {
		LogosZgcDropPage(pg)
		LogosZgcUnmapPages(raw, 1)
	}()
	slot = base + bad
	before = LogosZgcHeals()
	if got := LogosZgcAtomicLoadpSlow(&slot, base+bad); got != base || slot != base+good || LogosZgcHeals() < before+1 {
		t.Fatalf("registered word: hook %#x slot %#x heals %d, want %#x healed to %#x", got, slot, LogosZgcHeals()-before, base, base+good)
	}
	before = LogosZgcHeals()
	if got := LogosZgcAtomicLoadpSlow(&slot, slot); got != base || slot != base+good || LogosZgcHeals() != before {
		t.Fatalf("healed word: hook %#x slot %#x heals %d, want fast path", got, slot, LogosZgcHeals()-before)
	}
	KeepAlive(heap)
}

// TestZgcAtomicLoadpAsmSlowHeal drives the full out-of-line path: the
// arm64 assembly stub (reached through a func value, which the compiler
// cannot intrinsify) marshals (slot, raw) through its $32-16 frame into
// the hook, and the slow path heals a bad-colored word on a registered
// page. This is the only test that proves the stub's slot handoff, which
// the fast path never consumes.
func TestZgcAtomicLoadpAsmSlowHeal(t *testing.T) {
	saved := LogosZgcColorSave()
	defer func() {
		LogosZgcColorRestore(saved)
	}()
	LogosZgcSelectSoftware(LogosZgcMark0)
	good, bad := uintptr(LogosZgcSoftBits(LogosZgcMark0)), uintptr(LogosZgcSoftBits(LogosZgcMark1))

	base, raw := logosZgcLoadpTestMap(t)
	pg := LogosZgcNewPage(base, 0, 16)
	defer func() {
		LogosZgcDropPage(pg)
		LogosZgcUnmapPages(raw, 1)
	}()

	loadp := atomic.Loadp
	slot := base + bad
	before := LogosZgcHeals()
	got := loadp(unsafe.Pointer(&slot))
	if uintptr(got) != base || slot != base+good || LogosZgcHeals() < before+1 {
		t.Fatalf("asm slow heal: load %#x slot %#x heals %d, want %#x healed to %#x",
			uintptr(got), slot, LogosZgcHeals()-before, base, base+good)
	}
}

// logosZgcLoadpTestMap maps two zgc-sized pages inside the 4 TB range for
// a registered-page test and returns the aligned base and the raw mapping.
// It mirrors the original fixture (two pages, aligned base) through the
// established MapPagesRaw/UnmapPages seam, which reserves, maps, and marks
// used the same two pages the inlined sysReserve/sysMap/sysUsed sequence
// did.
func logosZgcLoadpTestMap(t *testing.T) (base, raw uintptr) {
	t.Helper()
	// Mode-independent mapped fixture: ensure registry capacity for the
	// registration below. An immix-linked binary never runs zgcInit, so
	// without this the first registration throws "page registry
	// overflow" and aborts the binary.
	LogosZgcInit()
	base, raw = LogosZgcMapPagesRaw(1)
	if uint64(base)+2*uint64(LogosZgcPageSize) > LogosZgcAddrMask+1 {
		t.Fatalf("kernel placed the test mapping outside the 4 TB range")
	}
	return base, raw
}
