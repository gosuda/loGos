// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"runtime"
	"testing"
	_ "unsafe"
)

//go:linkname heapObjectsCanMove runtime.heapObjectsCanMove
func heapObjectsCanMove() bool

func TestHeapObjectsCanMove(t *testing.T) {
	// SPEC Sec 19 (M19-S5 phase 1 for immix, phase 2 for zgc per Sec 19.2)
	// requires objects to move under -gc=immix and -gc=zgc, so this
	// upstream no-moving-gc check (which guards
	// go4.org/unsafe/assume-no-moving-gc) cannot hold in those modes. Plain
	// go keeps the check exactly as upstream wrote it.
	if runtime.LogosHeapObjectsCanMove() {
		t.Skip("SPEC Sec 19 requires moving objects under -gc=immix and -gc=zgc")
	}
	if heapObjectsCanMove() {
		// If this happens (or this test stops building),
		// it will break go4.org/unsafe/assume-no-moving-gc.
		t.Fatalf("heap objects can move!")
	}
}
