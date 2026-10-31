// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.arenas

package arena_test

import (
	"arena"
	"testing"
)

// Compile-time pins: renaming or removing any exported symbol breaks this
// file. Each symbol's stability marker lives on its doc comment in the
// non-test source.
var (
	_ *arena.Arena                = (*arena.Arena)(nil)
	_ func() *arena.Arena         = arena.NewArena
	_ func(*arena.Arena)          = (*arena.Arena).Free
	_ func(*arena.Arena) *T1      = arena.New[T1]
	_ func(*arena.Arena, int, int) []T1 = arena.MakeSlice[T1]
	_ func(int) int               = arena.Clone[int]
	_ func(int) []byte            = arena.DefaultAlloc
)

func TestArenaAPIShape(t *testing.T) {
	// Value-level references so an uncalled generic or const still fails
	// the build if it disappears.
	if arena.ChunkBytes <= 0 {
		t.Fatalf("ChunkBytes = %d, want positive", arena.ChunkBytes)
	}
	if arena.MaxObjectBytes <= 0 {
		t.Fatalf("MaxObjectBytes = %d, want positive", arena.MaxObjectBytes)
	}
	if arena.BumpGateBytes <= 0 {
		t.Fatalf("BumpGateBytes = %d, want positive", arena.BumpGateBytes)
	}
	_ = arena.ShouldBump(1)
	_ = arena.DefaultBumpAllocs()
	_ = arena.DefaultArenaEnabled
	_ = arena.Clone[int]
}

func TestArenaBudgetsUnchanged(t *testing.T) {
	if arena.ChunkBytes != 65536 {
		t.Errorf("ChunkBytes = %d, want 65536 (64 KiB)", arena.ChunkBytes)
	}
	if arena.MaxObjectBytes != 64512 {
		t.Errorf("MaxObjectBytes = %d, want 64512 (64 KiB minus 1024)", arena.MaxObjectBytes)
	}
	if arena.BumpGateBytes != 512 {
		t.Errorf("BumpGateBytes = %d, want 512 (rounded-payload bump gate)", arena.BumpGateBytes)
	}
	if arena.MaxObjectBytes != arena.ChunkBytes-1024 {
		t.Errorf("MaxObjectBytes = %d, want ChunkBytes-1024 = %d", arena.MaxObjectBytes, arena.ChunkBytes-1024)
	}
	// Gate boundaries on 16-byte-rounded payloads.
	cases := map[int]bool{
		-1:   false,
		0:    true,
		1:    true,
		512:  true,
		513:  false, // rounds to 528
		1024: false,
	}
	for size, want := range cases {
		if got := arena.ShouldBump(size); got != want {
			t.Errorf("ShouldBump(%d) = %v, want %v", size, got, want)
		}
	}
	if arena.ShouldBump(arena.MaxObjectBytes+1) {
		t.Errorf("ShouldBump(MaxObjectBytes+1) = true, want false")
	}
}

func TestArenaDefaultPlainGoControl(t *testing.T) {
	if arena.DefaultArenaEnabled {
		// Logos build: small payloads take the measured bump path and
		// are counted; oversized payloads fall back without counting.
		before := arena.DefaultBumpAllocs()
		small := arena.DefaultAlloc(32)
		if len(small) != 32 {
			t.Fatalf("DefaultAlloc(32) len = %d, want 32", len(small))
		}
		for i, b := range small {
			if b != 0 {
				t.Fatalf("DefaultAlloc(32)[%d] = %d, want 0", i, b)
			}
		}
		if got := arena.DefaultBumpAllocs(); got != before+1 {
			t.Fatalf("DefaultBumpAllocs = %d, want %d after one bump", got, before+1)
		}
		large := arena.DefaultAlloc(arena.BumpGateBytes + 1)
		if len(large) != arena.BumpGateBytes+1 {
			t.Fatalf("DefaultAlloc(large) len = %d, want %d", len(large), arena.BumpGateBytes+1)
		}
		if got := arena.DefaultBumpAllocs(); got != before+1 {
			t.Fatalf("DefaultBumpAllocs = %d, want %d (large must not bump)", got, before+1)
		}
		return
	}
	// Plain-Go control: no arena allocation and no behavior change.
	// Every size, including ones that would bump under logos, must leave
	// the bump counter at zero and return plain zeroed memory.
	for _, size := range []int{0, 1, 32, 512, 513, 4096} {
		got := arena.DefaultAlloc(size)
		if len(got) != size {
			t.Fatalf("DefaultAlloc(%d) len = %d, want %d", size, len(got), size)
		}
		for i, b := range got {
			if b != 0 {
				t.Fatalf("DefaultAlloc(%d)[%d] = %d, want 0", size, i, b)
			}
		}
		if n := arena.DefaultBumpAllocs(); n != 0 {
			t.Fatalf("DefaultBumpAllocs = %d after DefaultAlloc(%d), want 0 on plain-Go builds", n, size)
		}
	}
}
