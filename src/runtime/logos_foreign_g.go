// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving

package runtime

// logosForeignGBytes is the size logosForeignG adds to g under the
// moving experiment, read by the size-floor test.
const logosForeignGBytes = 24

// logosForeignG holds this goroutine's foreign-call frame state. It is
// embedded in g (ahead of a non-zero-sized field), so consumers keep the
// promoted gp.logosForeignTop spelling.
type logosForeignG struct {
	// logosForeignTop heads this goroutine's foreign-call frame stack
	// (*zgcForeignFrame address, off-heap, 0 when outside foreign
	// code). Token ownership lives on the g, so migration across Ms
	// after exitsyscall cannot detach it. Only the owning g mutates
	// this chain; the exposure gate serializes publication against
	// readers.
	logosForeignTop uintptr
	// logosForeignSpare caches one off-heap frame for restricted
	// (nosplit) nested admission without allocation. G-local.
	logosForeignSpare uintptr
	// logosForeignDepth counts active foreign frames on this g,
	// published admissions and nested markers alike.
	logosForeignDepth uint32
	// logosForeignTrans counts scheduler foreign transitions
	// (reentersyscall/exitsyscall nesting). Transport bookkeeping
	// only: it never creates or destroys frame ownership.
	logosForeignTrans uint32
}
