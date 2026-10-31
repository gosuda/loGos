// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime"
	"testing"
	"unsafe"

	. "runtime"
)

// TestLogosZgcFlagLeakStaleStripsFlag is the regression test for the
// remapped-flag leak. A stale remapped-flagged word into ordinary span
// heap takes the load-barrier slow path, which must strip the flag to
// the canonical address instead of handing the flagged word to the
// dereference the compiled barrier feeds. The good color is mark0 here,
// so the remapped bit is stale, and the word names ordinary heap on no
// zgc page, so no forwarding entry can exist for it. The read must
// return the canonical address with the flag gone, leave the slot
// alone, and count no heal.
func TestLogosZgcFlagLeakStaleStripsFlag(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	LogosZgcSelectSoftware(LogosZgcMark0)
	defer LogosZgcReset()

	// The slow path strips a stale flag only from a word with an in-use
	// heap span behind it and returns a stack-naming word unchanged, so
	// obj must land on ordinary span heap. Holding it in the typed sink
	// keeps a live reference the compiler cannot sink, the same anchor
	// the sibling barrier fixtures use; the premise is checked below
	// instead of assumed.
	obj := new([4]uintptr)
	savedSink := logosZgcSink
	logosZgcSink = []any{obj}
	defer func() { logosZgcSink = savedSink }()
	a := uintptr(unsafe.Pointer(obj))
	if _, ok := LogosZgcSpanStart(a); !ok {
		t.Fatalf("fixture premise lost: %#x has no in-use heap span behind it", a)
	}
	if pg := LogosZgcPageFor(a); pg != nil {
		t.Fatalf("fixture premise lost: %#x is covered by registered relocation page %v", a, pg)
	}
	const remappedBit = uintptr(1) << 45
	flagged := a + remappedBit

	var slot uintptr = flagged
	before := LogosZgcHeals()
	got := LogosZgcLoad(&slot)
	if got != a {
		t.Fatalf("barriered read of stale-flagged word %#x = %#x, want canonical %#x", flagged, got, a)
	}
	if got&remappedBit != 0 {
		t.Fatalf("remapped flag survived the barriered read: %#x", got)
	}
	if slot != flagged {
		t.Fatalf("barriered read rewrote the slot to %#x, want %#x untouched", slot, flagged)
	}
	if n := LogosZgcHeals() - before; n != 0 {
		t.Fatalf("barriered read counted %d heals, want 0", n)
	}
	runtime.KeepAlive(obj)
}
