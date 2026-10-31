// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	. "runtime"
	"testing"
)

// TestLogosZgcSnapshotSortedColoredG verifies that allGsSnapshotSortedForGC
// canonicalizes colored *g entries (such as those installed by SelfHeal CAS
// during -lb code runs) before dereferencing atomicstatus, readgstatus, or
// internalBlocked.
func TestLogosZgcSnapshotSortedColoredG(t *testing.T) {
	if LogosZgcColorMaskBits() == 0 {
		t.Skip("colors are inert: cannot construct a colored entry word")
	}
	LogosZgcForeignInit()
	var owned []*LogosZgcPage
	logosZgcForeignPagesQuiesce(t, &owned)

	h := LogosZgcForeignNewG(8)
	gAddr := LogosZgcForeignGAddr(h)
	if uint64(gAddr) > LogosZgcAddrMask {
		t.Skip("synthetic g lies outside the ZGC address field")
	}

	// 1. Colored case: canonical sorting must strip the color bits and return
	// the canonical pointer without faulting (old plain load faults with SIGSEGV).
	sorted, cutoff := LogosAllGsSnapshotSortedForGCColored(h)
	if len(sorted) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(sorted))
	}
	if uintptr(sorted[0]) != gAddr {
		t.Fatalf("snapshot entry = %x, want canonical %x", uintptr(sorted[0]), gAddr)
	}
	if cutoff < 0 || cutoff > 1 {
		t.Fatalf("cutoff = %d, want in [0, 1]", cutoff)
	}

	// 2. Parity case: uncolored entry produces identical canonical result.
	sortedParity, cutoffParity := LogosAllGsSnapshotSortedForGC(h)
	if len(sortedParity) != 1 {
		t.Fatalf("parity snapshot length = %d, want 1", len(sortedParity))
	}
	if uintptr(sortedParity[0]) != gAddr {
		t.Fatalf("parity snapshot entry = %x, want canonical %x", uintptr(sortedParity[0]), gAddr)
	}
	if cutoffParity != cutoff {
		t.Fatalf("cutoff parity mismatch: %d vs %d", cutoffParity, cutoff)
	}

	KeepAlive(h)
}

// TestLogosZgcSnapshotSortedColoredHeader verifies that allGsSnapshotSortedForGC
// canonicalizes a colored allgs slice header array pointer (Level 1) before indexing
// and dereferencing atomicstatus, readgstatus, or internalBlocked.
func TestLogosZgcSnapshotSortedColoredHeader(t *testing.T) {
	if LogosZgcColorMaskBits() == 0 {
		t.Skip("colors are inert: cannot construct a colored entry word")
	}
	LogosZgcForeignInit()
	var owned []*LogosZgcPage
	logosZgcForeignPagesQuiesce(t, &owned)

	h := LogosZgcForeignNewG(8)
	gAddr := LogosZgcForeignGAddr(h)
	if uint64(gAddr) > LogosZgcAddrMask {
		t.Skip("synthetic g lies outside the ZGC address field")
	}

	// 1. Level 1 colored slice header (canonical entry):
	// Canonical sorting must canonicalize allgs.array before indexing and return
	// the canonical pointer without faulting (old range loop faults with SIGSEGV).
	sorted, cutoff := LogosAllGsSnapshotSortedForGCColoredHeader(h)
	if len(sorted) != 1 {
		t.Fatalf("snapshot length = %d, want 1", len(sorted))
	}
	if uintptr(sorted[0]) != gAddr {
		t.Fatalf("snapshot entry = %x, want canonical %x", uintptr(sorted[0]), gAddr)
	}
	if cutoff < 0 || cutoff > 1 {
		t.Fatalf("cutoff = %d, want in [0, 1]", cutoff)
	}
	// 2. Parity case: uncolored entry produces identical canonical result.
	sortedParity, cutoffParity := LogosAllGsSnapshotSortedForGC(h)
	if len(sortedParity) != 1 {
		t.Fatalf("parity snapshot length = %d, want 1", len(sortedParity))
	}
	if uintptr(sortedParity[0]) != gAddr {
		t.Fatalf("parity snapshot entry = %x, want canonical %x", uintptr(sortedParity[0]), gAddr)
	}
	if cutoffParity != cutoff {
		t.Fatalf("cutoff parity mismatch: %d vs %d", cutoffParity, cutoff)
	}

	KeepAlive(h)
}

// TestLogosZgcMarkrootStackG verifies that logosGCMarkrootStackG canonicalizes
// both the stackRoots slice header backing array pointer (Level 1) and the
// individual *g entry word (Level 2), without mutating the caller's slice or backing array.
func TestLogosZgcMarkrootStackG(t *testing.T) {
	if LogosZgcColorMaskBits() == 0 {
		t.Skip("colors are inert: cannot construct a colored entry word")
	}
	LogosZgcForeignInit()
	var owned []*LogosZgcPage
	logosZgcForeignPagesQuiesce(t, &owned)

	h := LogosZgcForeignNewG(8)
	gAddr := LogosZgcForeignGAddr(h)
	if uint64(gAddr) > LogosZgcAddrMask {
		t.Skip("synthetic g lies outside the ZGC address field")
	}

	// 1. Colored header + colored entry:
	// Invokes logosGCMarkrootStackG over a heap-backed slice with both Level 1 and Level 2 colors.
	// On old uncanonicalized consumer, indexing a colored base faults with SIGSEGV.
	// On fixed consumer, both levels are canonicalized on the local view and backing array is not mutated.
	gpCanon, stillColored := LogosGCMarkrootStackGColoredHeader(h)
	if uintptr(gpCanon) != gAddr {
		t.Fatalf("markroot gp = %#x, want canonical %#x", uintptr(gpCanon), gAddr)
	}
	if !stillColored {
		t.Fatal("backing entry was mutated by logosGCMarkrootStackG")
	}

	// 2. Control: uncolored heap-backed slice returns identical canonical g.
	gpPlain := LogosGCMarkrootStackG(h)
	if uintptr(gpPlain) != gAddr {
		t.Fatalf("plain markroot gp = %#x, want canonical %#x", uintptr(gpPlain), gAddr)
	}

	KeepAlive(h)
}
