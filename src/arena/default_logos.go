// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.arenas && goexperiment.logosarena

package arena

import (
	"sync"
	"sync/atomic"
)

// DefaultArenaEnabled reports whether the default allocation path may use
// the arena bump path. It is true only under goexperiment.logosarena.
//
// Stability: stable.
const DefaultArenaEnabled = true

var (
	defaultBumpMu    sync.Mutex
	defaultBumpChunk []byte
	defaultBumpOff   int
	defaultBumpCount atomic.Int64
)

// tryDefaultBumpCarve carves stride bytes for a size-byte payload from the
// current chunk, reporting false when no chunk has room. The lock is held
// only for the few carving statements; the 64 KiB refill allocation in
// DefaultAlloc happens outside the critical section.
func tryDefaultBumpCarve(stride, size int) ([]byte, bool) {
	defaultBumpMu.Lock()
	defer defaultBumpMu.Unlock()
	if defaultBumpChunk == nil || defaultBumpOff+stride > len(defaultBumpChunk) {
		return nil, false
	}
	off := defaultBumpOff
	defaultBumpOff += stride
	defaultBumpCount.Add(1)
	chunk := defaultBumpChunk
	// Full slice expression caps the result so append cannot bleed into
	// the next bump slot.
	return chunk[off : off+size : off+stride], true
}

// DefaultAlloc returns a zeroed []byte of size bytes. Payloads admitted by
// ShouldBump are bump-allocated out of 64 KiB chunks (the measured bump
// path); all other sizes fall back to the plain heap with identical
// contents. Callers observe valid zeroed memory either way; only the
// backing store differs.
//
// Unlike Arena, which must never be used concurrently by multiple
// goroutines, DefaultAlloc is safe for concurrent use: carving is
// mutex-guarded and the 64 KiB refill is allocated outside the lock.
//
// Bump slices share their chunk's backing array, so one live slice keeps
// its whole 64 KiB chunk alive until every slice from that chunk is dead.
// That is ordinary arena bulk-free semantics, not a leak: keep bumped
// payloads short-lived and bulk-oriented, and let long-lived or large
// payloads take the plain-heap fallback (the bump gate already routes
// rounded payloads over 512 bytes there).
//
// Stability: stable. The plain-Go build of this symbol
// (default_plain.go, !goexperiment.logosarena) is exactly
// make([]byte, size) with no arena activity.
func DefaultAlloc(size int) []byte {
	if !ShouldBump(size) {
		return make([]byte, size)
	}
	stride := (size + bumpAlign - 1) &^ (bumpAlign - 1)
	if b, ok := tryDefaultBumpCarve(stride, size); ok {
		return b
	}
	// Refill outside the critical section so the 64 KiB zeroing does not
	// serialize other callers; recheck under the lock in case another
	// caller refilled first.
	fresh := make([]byte, ChunkBytes)
	defaultBumpMu.Lock()
	defer defaultBumpMu.Unlock()
	if defaultBumpChunk != nil && defaultBumpOff+stride <= len(defaultBumpChunk) {
		off := defaultBumpOff
		defaultBumpOff += stride
		defaultBumpCount.Add(1)
		chunk := defaultBumpChunk
		return chunk[off : off+size : off+stride]
	}
	defaultBumpChunk = fresh
	defaultBumpOff = stride
	defaultBumpCount.Add(1)
	return fresh[0:size:stride]
}

// DefaultBumpAllocs counts DefaultAlloc calls that took the bump path in
// this process.
//
// Stability: stable.
func DefaultBumpAllocs() int64 {
	return defaultBumpCount.Load()
}
