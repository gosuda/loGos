// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && arm64

package runtime

import (
	"internal/runtime/atomic"
	"testing"
	"unsafe"
)

// W1d hook unit test. Calls zgcAtomicLoadpSlow directly, so it covers
// the shared Go logic on amd64 and arm64; the arm64 assembly frame that
// reaches it is pinned by TestZgcArm64LoadpOutOfLine and by objdump.

var zgcLoadpTestNext atomic.Uintptr

func TestZgcAtomicLoadpSlowHook(t *testing.T) {
	oldTag := zgcHardwareTagging
	oldMask := atomic.Loaduintptr(&zgcColorMaskBits)
	oldGood := atomic.Loaduintptr(&zgcGoodMask)
	oldUnmask := atomic.Loaduintptr(&zgcUnmaskBits)
	defer func() {
		zgcHardwareTagging = oldTag
		atomic.Storeuintptr(&zgcColorMaskBits, oldMask)
		atomic.Storeuintptr(&zgcGoodMask, oldGood)
		atomic.Storeuintptr(&zgcUnmaskBits, oldUnmask)
	}()

	// Colors inert: the hook is the identity, high bits intact.
	for _, w := range []uintptr{0, 1, 0x5555_0000_1000, ^uintptr(0)} {
		slot := w
		if got := zgcAtomicLoadpSlow(&slot, w); got != w || slot != w {
			t.Fatalf("inert word %#x: hook %#x slot %#x, want identity", w, got, slot)
		}
	}

	heap := new([8]uintptr)
	heapAddr := uintptr(unsafe.Pointer(heap))
	if heapAddr > zgcAddrMask {
		t.Skipf("heap address %#x above the 4 TB canonical field", heapAddr)
	}

	zgcConfigure(zgcEncodingSoftware, zgcColorMark0)
	good, bad := zgcColorBits(zgcColorMark0), zgcColorBits(zgcColorMark1)

	// Good-colored heap word: canonical, slot untouched, no heal.
	slot := heapAddr + good
	before := zgcHeals.Load()
	if got := zgcAtomicLoadpSlow(&slot, heapAddr+good); got != heapAddr || slot != heapAddr+good || zgcHeals.Load() < before {
		t.Fatalf("good word: hook %#x slot %#x heals %d, want %#x untouched", got, slot, zgcHeals.Load()-before, heapAddr)
	}

	// Bad color on an unregistered heap address: no page covers it,
	// so it passes through unchanged and the slot is left alone.
	slot = heapAddr + bad
	before = zgcHeals.Load()
	if got := zgcAtomicLoadpSlow(&slot, heapAddr+bad); got != heapAddr+bad || slot != heapAddr+bad || zgcHeals.Load() != before {
		t.Fatalf("unregistered word: hook %#x slot %#x heals %d, want passthrough", got, slot, zgcHeals.Load()-before)
	}

	// Foreign high word: address bits, never stripped.
	slot = 0x5555_0000_1000
	if got := zgcAtomicLoadpSlow(&slot, slot); got != slot {
		t.Fatalf("foreign word: hook %#x, want unchanged", got)
	}

	// Bad color on a registered page: canonical return, slot healed
	// to the good color, exactly one heal; a second load fast-paths.
	base, raw := zgcLoadpTestMap(t)
	pg := zgcNewPage(base, zgcPageSize, 0, 16)
	defer func() {
		pg.published.Store(0)
		zgcFreePage(pg)
		sysFree(add(nil, raw), 2*zgcPageSize, &memstats.other_sys)
	}()
	slot = base + bad
	before = zgcHeals.Load()
	if got := zgcAtomicLoadpSlow(&slot, base+bad); got != base || slot != base+good || zgcHeals.Load() < before+1 {
		t.Fatalf("registered word: hook %#x slot %#x heals %d, want %#x healed to %#x", got, slot, zgcHeals.Load()-before, base, base+good)
	}
	before = zgcHeals.Load()
	if got := zgcAtomicLoadpSlow(&slot, slot); got != base || slot != base+good || zgcHeals.Load() != before {
		t.Fatalf("healed word: hook %#x slot %#x heals %d, want fast path", got, slot, zgcHeals.Load()-before)
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
	oldTag := zgcHardwareTagging
	oldMask := atomic.Loaduintptr(&zgcColorMaskBits)
	oldGood := atomic.Loaduintptr(&zgcGoodMask)
	oldUnmask := atomic.Loaduintptr(&zgcUnmaskBits)
	defer func() {
		zgcHardwareTagging = oldTag
		atomic.Storeuintptr(&zgcColorMaskBits, oldMask)
		atomic.Storeuintptr(&zgcGoodMask, oldGood)
		atomic.Storeuintptr(&zgcUnmaskBits, oldUnmask)
	}()
	zgcConfigure(zgcEncodingSoftware, zgcColorMark0)
	good, bad := zgcColorBits(zgcColorMark0), zgcColorBits(zgcColorMark1)

	base, raw := zgcLoadpTestMap(t)
	pg := zgcNewPage(base, zgcPageSize, 0, 16)
	defer func() {
		pg.published.Store(0)
		zgcFreePage(pg)
		sysFree(add(nil, raw), 2*zgcPageSize, &memstats.other_sys)
	}()

	loadp := atomic.Loadp
	slot := base + bad
	before := zgcHeals.Load()
	got := loadp(unsafe.Pointer(&slot))
	if uintptr(got) != base || slot != base+good || zgcHeals.Load() < before+1 {
		t.Fatalf("asm slow heal: load %#x slot %#x heals %d, want %#x healed to %#x",
			uintptr(got), slot, zgcHeals.Load()-before, base, base+good)
	}
}

// zgcLoadpTestMap maps two zgc-sized pages inside the 4 TB range for a
// registered-page test and returns the aligned base and the raw mapping.
func zgcLoadpTestMap(t *testing.T) (base, raw uintptr) {
	t.Helper()
	if zgcLoadpTestNext.Load() == 0 {
		zgcLoadpTestNext.Store(1 << 40)
	}
	total := uintptr(2) * zgcPageSize
	hint := zgcLoadpTestNext.Add(total+zgcPageSize) - (total + zgcPageSize)
	r := sysReserve(add(nil, hint), total, "logos zgc test")
	if r == nil || uintptr(r)+total > zgcAddrMask+1 {
		t.Fatalf("kernel placed the test mapping outside the 4 TB range")
	}
	sysMap(r, total, &memstats.other_sys, "logos zgc test")
	sysUsed(r, total, total)
	return alignUp(uintptr(r), zgcPageSize), uintptr(r)
}
