// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"runtime/debug"
	"slices"
	"testing"
	"unsafe"
)

// Pin-stability attacks for the immix collector (SPEC Sec 8.4, redteam
// #159): address-hashed blocks MUST set the pin bit and pinned blocks MUST
// NOT evacuate, while pointer-keyed runtime maps keep working across
// relocation because their keys pin their blocks in place.
//
// Both tests reuse the production harness layout (immixProdFillBlocks /
// immixProdNewGroup over 1152-byte blobs, 28 slots per 32 KiB block) and
// the production pin seam (runtime.Pinner, the same seam
// immixProdExclusions uses), and both observe production state only
// through the exported-for-test seams (LogosImmixSpanOf,
// LogosImmixPlanes / LogosImmixPlanesAt, LogosEvacTotals). No test forces
// the cached mode and none seeds a flag, bitmap, or line.
//
// They need a test binary built with GOEXPERIMENT=logosgcmoving and linked
// with -ldflags=-X=runtime.logosGCMode=immix, the way the linker selects
// the mode, with GOMAXPROCS(1) and GC percent -1 set by the tests
// themselves so no background cycle starts. A binary without the mode
// skips them, unless LOGOS_IMMIX_REQUIRE is set.
//
// No new seam was needed: every inspector used here already exists in
// export_logos_immix_test.go / export_logos_move_test.go.

// TestLogosImmixPinnedBlocksNeverMove allocates eight block spans, pins a
// subset of the sparse blocks through runtime.Pinner, forces an evacuation
// cycle, and asserts the pinned block addresses are byte-identical before
// and after while the unpinned sparse control blocks moved (so the
// stillness of the pinned blocks is owed to the pin, not to an evacuation
// that never ran). It then releases the pin, collects again, and asserts
// the same blocks became evacuable: their survivors move and the block
// pin bit clears.
func TestLogosImmixPinnedBlocksNeverMove(t *testing.T) {
	immixProdRequireMode(t)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()

	const nBlocks, perBlock = 8, 28
	alloc := func() unsafe.Pointer { return unsafe.Pointer(new(immixProdBlob)) }
	blocks := immixProdFillBlocks(t, alloc, 0, nBlocks, perBlock)
	sparse := func(k int) bool { return k%10 == 0 }
	dense := func(k int) bool { return k%2 == 0 || k == perBlock-1 }

	host0 := immixProdNewGroup("host0", blocks[0], 100, dense)
	host1 := immixProdNewGroup("host1", blocks[1], 200, dense)
	pinned0 := immixProdNewGroup("pinned0", blocks[2], 300, sparse)
	pinned1 := immixProdNewGroup("pinned1", blocks[3], 400, sparse)
	quiet0 := immixProdNewGroup("quiet0", blocks[4], 500, sparse)
	quiet1 := immixProdNewGroup("quiet1", blocks[5], 600, sparse)
	// Blocks 6 and 7 are left entirely dead to supply destination slots.
	for _, b := range blocks {
		clear(b)
	}
	blocks = nil

	pinned := []*immixProdGroup{pinned0, pinned1}
	all := []*immixProdGroup{host0, host1, pinned0, pinned1, quiet0, quiet1}
	for _, g := range all {
		g.record()
	}

	var pin runtime.Pinner
	pin.Pin((*immixProdBlob)(pinned0.ptrs[1]))
	pin.Pin((*immixProdBlob)(pinned1.ptrs[1]))

	baseBefore := make(map[string]uintptr, len(pinned))
	for _, g := range pinned {
		baseBefore[g.name] = immixProdSpan(t, g.ptrs[1]).BlockBase
	}

	ev0, mv0 := runtime.LogosEvacTotals()
	runtime.GC()

	for _, g := range all {
		if !g.intact() {
			t.Fatalf("pinned collection: group %s lost its contents", g.name)
		}
	}
	for _, g := range pinned {
		st := immixProdSpan(t, g.ptrs[1])
		_, _, _, blockPinned, ok := runtime.LogosImmixPlanes(g.ptrs[1])
		for i, p := range g.ptrs {
			t.Logf("pinned %s[%d]: before=%#x after=%#x blockBaseBefore=%#x blockBaseAfter=%#x", g.name, i, g.before[i], uintptr(p), baseBefore[g.name], st.BlockBase)
		}
		if st.BlockBase != baseBefore[g.name] {
			t.Fatalf("pinned group %s block base changed from %#x to %#x", g.name, baseBefore[g.name], st.BlockBase)
		}
		if m := g.moved(); m != 0 {
			t.Fatalf("pinned group %s moved %d of %d survivors: %+v", g.name, m, len(g.ptrs), st)
		}
		if !ok || !st.Pinned || !blockPinned {
			t.Fatalf("pinned group %s: spanPinned=%v blockPinned=%v ok=%v, want both after a collection: %+v", g.name, st.Pinned, blockPinned, ok, st)
		}
	}
	for _, g := range []*immixProdGroup{host0, host1} {
		if m := g.moved(); m != 0 {
			t.Fatalf("dense group %s moved %d of %d survivors: %+v", g.name, m, len(g.ptrs), immixProdSpan(t, g.ptrs[1]))
		}
	}
	moved := quiet0.moved() + quiet1.moved()
	ev1, mv1 := runtime.LogosEvacTotals()
	t.Logf("control moved %d of %d survivors; evacuations %d->%d objects %d->%d", moved, len(quiet0.ptrs)+len(quiet1.ptrs), ev0, ev1, mv0, mv1)
	if moved*2 < len(quiet0.ptrs)+len(quiet1.ptrs) {
		t.Fatalf("the control moved %d of %d survivors of unexcluded sparse blocks: the evacuation did not run", moved, len(quiet0.ptrs)+len(quiet1.ptrs))
	}

	// Release the pin: the same blocks must become evacuable again.
	pin.Unpin()
	pinOld := uintptr(pinned0.ptrs[1])
	for _, g := range pinned {
		g.record()
	}
	runtime.GC()
	for _, g := range all {
		if !g.intact() {
			t.Fatalf("after unpin: group %s lost its contents", g.name)
		}
	}
	for _, g := range pinned {
		m := g.moved()
		t.Logf("released %s moved %d of %d survivors", g.name, m, len(g.ptrs))
		if m*2 < len(g.ptrs) {
			t.Fatalf("unpinned group %s moved %d of %d survivors: the pin of its block was not released", g.name, m, len(g.ptrs))
		}
	}
	if _, _, _, blockPinned, ok := runtime.LogosImmixPlanesAt(pinOld); !ok || blockPinned {
		t.Fatalf("the block of the unpinned object keeps its pin bit (metadata=%v pinned=%v)", ok, blockPinned)
	}
	runtime.KeepAlive(pin)
	runtime.KeepAlive(all)
}

// TestLogosImmixAddressHashedKeysStable inserts pointer-keyed runtime maps
// whose keys land in immix-managed blocks, forces three relocation cycles,
// and asserts every lookup still finds its value and every key kept its
// identity: under immix the address hash pins the key block, so each key
// address is byte-identical across the cycles and the block pin bit stays
// set, while unpinned sparse control blocks moved (so the key stillness is
// owed to the address-hash pin, not to an evacuation that never ran).
func TestLogosImmixAddressHashedKeysStable(t *testing.T) {
	immixProdRequireMode(t)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	runtime.GC()
	runtime.GC()

	const nBlocks, perBlock = 8, 28
	alloc := func() unsafe.Pointer { return unsafe.Pointer(new(immixProdBlob)) }
	blocks := immixProdFillBlocks(t, alloc, 0, nBlocks, perBlock)
	sparse := func(k int) bool { return k%10 == 0 }
	dense := func(k int) bool { return k%2 == 0 || k == perBlock-1 }

	keyA := immixProdNewGroup("mapkeysA", blocks[0], 1000, sparse)
	keyB := immixProdNewGroup("mapkeysB", blocks[1], 2000, sparse)
	quiet0 := immixProdNewGroup("quiet0", blocks[2], 3000, sparse)
	quiet1 := immixProdNewGroup("quiet1", blocks[3], 4000, sparse)
	host0 := immixProdNewGroup("host0", blocks[4], 5000, dense)
	host1 := immixProdNewGroup("host1", blocks[5], 6000, dense)
	// Blocks 6 and 7 are left entirely dead to supply destination slots.
	keys := slices.Concat(keyA.ptrs, keyB.ptrs)
	keyIDs := slices.Concat(keyA.ids, keyB.ids)

	// The runtime map path: inserting hashes the key by address and pins
	// the objects the hash mixes in, covering pointer, interface, and
	// array-of-pointer keys.
	byPtr := make(map[*immixProdBlob]int, len(keys))
	byIface := make(map[any]int, len(keys))
	for i, kp := range keys {
		k := (*immixProdBlob)(kp)
		st := immixProdSpan(t, kp)
		if !st.Active || !st.HasBlock {
			t.Fatalf("key %d (%p) is not in an immix block span: %+v", i, k, st)
		}
		byPtr[k] = i
		byIface[k] = i
	}
	byArr := make(map[[2]*immixProdBlob]int, len(keys)/2)
	for i := 0; i+1 < len(keys); i += 2 {
		byArr[[2]*immixProdBlob{(*immixProdBlob)(keys[i]), (*immixProdBlob)(keys[i+1])}] = i
	}
	// The inserts hash by address, so every key span must already carry
	// the sticky pin before any collection runs.
	for i, kp := range keys {
		if st := immixProdSpan(t, kp); !st.Pinned {
			t.Fatalf("key %d (%p): span not pinned by map insert: %+v", i, kp, st)
		}
	}

	for _, b := range blocks {
		clear(b)
	}
	blocks = nil
	keyBefore := make([]uintptr, len(keys))
	for i, kp := range keys {
		keyBefore[i] = uintptr(kp)
	}
	quiet0.record()
	quiet1.record()
	ev0, mv0 := runtime.LogosEvacTotals()

	lookup := func(step string) {
		t.Helper()
		for i, kp := range keys {
			k := (*immixProdBlob)(kp)
			if v, ok := byPtr[k]; !ok || v != i {
				t.Fatalf("%s: byPtr lookup of key %d (%p) = (%d, %v), want (%d, true)", step, i, k, v, ok, i)
			}
			if v, ok := byIface[k]; !ok || v != i {
				t.Fatalf("%s: byIface lookup of key %d (%p) = (%d, %v), want (%d, true)", step, i, k, v, ok, i)
			}
			if uintptr(kp) != keyBefore[i] {
				t.Fatalf("%s: map key %d moved from %#x to %#x: the address hash did not pin its block", step, i, keyBefore[i], uintptr(kp))
			}
			if !immixProdBlobIntact(kp, keyIDs[i]) {
				t.Fatalf("%s: map key %d (%p) lost its contents", step, i, kp)
			}
		}
		for i := 0; i+1 < len(keys); i += 2 {
			ak := [2]*immixProdBlob{(*immixProdBlob)(keys[i]), (*immixProdBlob)(keys[i+1])}
			if v, ok := byArr[ak]; !ok || v != i {
				t.Fatalf("%s: byArr lookup of keys %d,%d = (%d, %v), want (%d, true)", step, i, i+1, v, ok, i)
			}
		}
		if len(byPtr) != len(keys) || len(byIface) != len(keys) || len(byArr) != len(keys)/2 {
			t.Fatalf("%s: map lengths %d/%d/%d, want %d/%d/%d: keys duplicated or lost", step, len(byPtr), len(byIface), len(byArr), len(keys), len(keys), len(keys)/2)
		}
	}

	for range 3 {
		runtime.GC()
		lookup("cycle")
	}
	ev1, mv1 := runtime.LogosEvacTotals()
	t.Logf("keys stable across 3 cycles; evacuations %d->%d objects %d->%d", ev0, ev1, mv0, mv1)
	for i, kp := range keys {
		_, _, _, blockPinned, ok := runtime.LogosImmixPlanes(kp)
		if !ok || !blockPinned {
			t.Fatalf("key %d (%p): block pin bit lost (metadata=%v pinned=%v)", i, kp, ok, blockPinned)
		}
		t.Logf("key %d: before=%#x after=%#x blockPinned=%v", i, keyBefore[i], uintptr(kp), blockPinned)
	}
	moved := quiet0.moved() + quiet1.moved()
	t.Logf("control moved %d of %d survivors", moved, len(quiet0.ptrs)+len(quiet1.ptrs))
	if moved*2 < len(quiet0.ptrs)+len(quiet1.ptrs) {
		t.Fatalf("the control moved %d of %d survivors of unexcluded sparse blocks: the evacuation did not run", moved, len(quiet0.ptrs)+len(quiet1.ptrs))
	}
	for _, g := range []*immixProdGroup{host0, host1} {
		if !g.intact() {
			t.Fatalf("dense group %s lost its contents", g.name)
		}
	}

	// Assigning through the current addresses after the moves must update
	// in place, never duplicate a key.
	for i, kp := range keys {
		byPtr[(*immixProdBlob)(kp)] = 100 + i
	}
	if len(byPtr) != len(keys) {
		t.Fatalf("reassign duplicated keys: len %d, want %d", len(byPtr), len(keys))
	}
	for i, kp := range keys {
		if v := byPtr[(*immixProdBlob)(kp)]; v != 100+i {
			t.Fatalf("reassign of key %d reads %d, want %d", i, v, 100+i)
		}
	}
	runtime.KeepAlive(byPtr)
	runtime.KeepAlive(byIface)
	runtime.KeepAlive(byArr)
	runtime.KeepAlive(keys)
	runtime.KeepAlive([]*immixProdGroup{quiet0, quiet1, host0, host1})
}
