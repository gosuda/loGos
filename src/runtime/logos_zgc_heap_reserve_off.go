// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import "unsafe"

// Stubs for builds where low-window placement cannot apply (experiment
// off or non-moving arch). Enabled is always false so malloc.go takes
// the original path byte-for-byte; the remaining stubs are unreachable
// through that gate.

func logosZgcHeapEnabled() bool { return false }

func logosZgcHeapInRange(v uintptr, n uintptr) bool { return true }

func logosZgcPruneHeapHints() {}

// logosZgcHeapRangeOwned is unreachable through the Enabled gate (always
// false here), which short-circuits before any call. Report not-owned so
// that, if ever reached, the caller takes the original upstream path
// byte-for-byte.
func logosZgcHeapRangeOwned(base, n uintptr) bool { return false }

func logosZgcHeapSearch(n uintptr) (unsafe.Pointer, uintptr) { return nil, 0 }

func logosZgcRecordIndex(l1 uint, base, size uintptr) {}

func logosZgcIndexClearOf(l1 uint, v, size uintptr) bool { return true }

func logosZgcRelocateIndex(l1 uint, v, size, l2size uintptr) *[1 << arenaL2Bits]*heapArena {
	return nil
}

// Committed-range ledger stubs for builds where the ledger cannot apply.
// The debug constant is false so every gated call site compiles away;
// the stubs keep the generic call sites type-checked.
const logosZgcCommitLedgerDebug = false

func logosZgcCommitLedgerAdd(base, size uintptr) {}

func logosZgcCommitLedgerAssertGrant(base, size uintptr, site string) {}

func logosZgcCommitLedgerRemove(base, size uintptr, site string) {}
