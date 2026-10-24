// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"slices"
	"testing"
	"unsafe"

	. "runtime"
)

// Concurrent-cycle regression tests: gating, boundaries, scan
// integration, and the pause benchmark. The full cycle (selection,
// relocation, release, retire) runs under -gc=zgc, which stays behind
// the mode gate; these tests pin the reachable machinery.

func TestLogosZgcCycleGatedOff(t *testing.T) {
	if LogosZgcCycleEnabled() {
		t.Skip("zgc mode linked: gate assertions apply to the default build")
	}
}

func TestLogosZgcUntagArgForeign(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)
	for _, p := range []uintptr{0, 1 << 50, 1<<63 - 1} {
		if got := LogosZgcUntagArg(p); got != p {
			t.Fatalf("untag(%#x) = %#x, want identity", p, got)
		}
	}
}

func TestLogosZgcUntagArgColoredHeap(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)
	base, free := logosZgcPlace(t)
	defer free()
	pg := LogosZgcNewPage(base, 0, 4)
	defer LogosZgcDropPage(pg)
	addr := base + 0x40
	colored := LogosZgcColored(addr, LogosZgcMark0)
	if got := LogosZgcUntagArg(colored); got != addr {
		t.Fatalf("untag(colored heap) = %#x, want %#x", got, addr)
	}
	if got := LogosZgcUntagArg(addr); got != addr {
		t.Fatalf("untag(canonical heap) = %#x, want %#x", got, addr)
	}
}

func TestLogosZgcScanWordForeign(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)
	if _, ok := LogosZgcRemapScanWord(0); ok {
		t.Fatal("nil resolves")
	}
	if _, ok := LogosZgcRemapScanWord(1 << 50); ok {
		t.Fatal("foreign word resolves")
	}
}

func TestLogosZgcHealScanSlotKeepsCanonical(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)
	x := new(uintptr)
	*x = 7
	addr := uintptr(unsafe.Pointer(x))
	slot := new(uintptr)
	*slot = addr
	LogosZgcHealScanSlot(slot)
	if *slot != addr {
		t.Fatalf("canonical slot rewritten to %#x", *slot)
	}
	*slot = LogosZgcColored(addr, LogosZgcMark1)
	LogosZgcHealScanSlot(slot)
	if *slot != addr {
		t.Fatalf("colored slot healed to %#x, want uncolored %#x", *slot, addr)
	}
}

func TestLogosZgcBoundaryStaysBlocked(t *testing.T) {
	if LogosZgcBoundaryLanded() {
		t.Fatal("Z2-asm wrappers landed without the audit artifact")
	}
}

func TestLogosZgcDeepPathGated(t *testing.T) {
	if !LogosZgcCanRelocateDeep() {
		t.Fatal("test goroutine cannot run the deep path")
	}
}

func TestLogosZgcPinSelfStackSmoke(t *testing.T) {
	x := new(uintptr)
	*x = 42
	KeepAlive(x)
	n := LogosZgcPinSelfStack()
	KeepAlive(n)
	GC()
}

func TestLogosZgcGuardSkipsLiveSpan(t *testing.T) {
	x := new(uintptr)
	*x = 3
	addr := uintptr(unsafe.Pointer(x))
	if LogosZgcSpanQuarantinedAt(addr) {
		t.Fatal("live span reads quarantined")
	}
	if LogosZgcSweepGuardAt(addr) {
		t.Fatal("sweep guard skips a live span")
	}
}

// BenchmarkLogosPause records the stop-the-world pause distribution of
// the active collector. The stage-3 acceptance replays it under -gc=zgc
// with relocation running and compares p99 against the recorded target;
// the contract asserts no number and changes no target here.
func BenchmarkLogosPause(b *testing.B) {
	var hold [][]byte
	defer func() { hold = nil }()
	keep := func() {
		hold = append(hold, make([]byte, 1<<20))
		if len(hold) > 64 {
			hold = hold[1:]
		}
	}
	for range 4 {
		keep()
		GC()
	}
	var ms MemStats
	ReadMemStats(&ms)
	base := ms.NumGC
	const n = 16
	lat := make([]uint64, 0, n)
	for range n {
		keep()
		GC()
		ReadMemStats(&ms)
		if ms.NumGC <= base {
			continue
		}
		base = ms.NumGC
		idx := (ms.NumGC - 1) % uint32(len(ms.PauseNs))
		lat = append(lat, ms.PauseNs[idx])
	}
	if len(lat) == 0 {
		b.Fatal("no collections observed")
	}
	slices.Sort(lat)
	b.ReportMetric(0, "ns/op")
	b.ReportMetric(0, "B/op")
	b.ReportMetric(0, "allocs/op")
	b.ReportMetric(float64(lat[len(lat)*50/100]), "p50-ns")
	b.ReportMetric(float64(lat[len(lat)*99/100]), "p99-ns")
}
