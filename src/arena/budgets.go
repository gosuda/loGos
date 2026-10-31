// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.arenas

package arena

// Default arena budgets shared by the logos bump path and the plain-Go
// control. Values are pinned by TestArenaBudgetsUnchanged and must not be
// retuned without a new measurement round.
//
//   - ChunkBytes (64 KiB) is the size of one default-arena chunk.
//   - MaxObjectBytes (64 KiB minus 1024 bytes) is the per-object
//     chunk-fit cap checked by ShouldBump; the bump gate below further
//     limits what the bump path ever places (at most 512 bytes).
//   - BumpGateBytes (512 bytes, rounded-payload gate) is the largest
//     16-byte-rounded payload that takes the inline bump path; larger
//     payloads fall back to the plain heap so the chunk is not
//     monopolized by a single object.
const (
	// ChunkBytes is the size in bytes of one default-arena chunk.
	//
	// Stability: stable. Pinned budget, 64 KiB.
	ChunkBytes = 64 << 10

	// MaxObjectBytes is the per-object chunk-fit cap enforced by
	// ShouldBump. It is necessary but not sufficient: the bump gate
	// (BumpGateBytes on the 16-byte-rounded size) further limits
	// bumped payloads to at most 512 bytes.
	//
	// Stability: stable. Pinned budget, 64 KiB minus 1024 bytes.
	MaxObjectBytes = ChunkBytes - 1024

	// BumpGateBytes bounds the inline bump path: only payloads whose
	// 16-byte-rounded size is at most this value take the bump path.
	//
	// Stability: stable. Pinned budget, 512-byte rounded-payload gate.
	BumpGateBytes = 512

	// bumpAlign is the rounding unit for the bump gate. It matches the
	// 16-byte scan-region unit used by the measured path.
	bumpAlign = 16
)

// ShouldBump reports whether a payload of size bytes takes the measured
// bump path: the 16-byte-rounded payload must fit the 512-byte bump gate
// and the unrounded size must fit the per-object budget.
//
// Stability: stable. Pure policy predicate over the pinned budgets.
func ShouldBump(size int) bool {
	if size < 0 {
		return false
	}
	if size > MaxObjectBytes {
		return false
	}
	rounded := (size + bumpAlign - 1) &^ (bumpAlign - 1)
	return rounded <= BumpGateBytes
}
