// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ctfe

// This file exposes narrow read-only accessors over evaluated values
// for the native materialization of imported const-function results
// (M5-S2 cross-module support). Lowering pairs a recorded Value with
// the checker's nominal types2 type and spells the value without
// re-executing the call; these accessors supply exactly the shape the
// spelling needs (nilness and slice headers) while evaluation,
// operators, and literal rendering stay where they are. No policy
// lives here: callers decide what is spellable.

// IsNilValue reports whether v is a typed nil.
func (v Value) IsNilValue() bool { return v.isNil }

// SliceHeader exposes the backing store and window of a slice value:
// backing is the full shared store, and the visible elements are
// backing[lo:lo+length] with capacity cap_-lo. It reports false for
// non-slice values or a detached backing.
func (v Value) SliceHeader() (backing []Value, lo, length, cap_ int, ok bool) {
	if v.slice == nil || v.slice.backing == nil {
		return nil, 0, 0, 0, false
	}
	return *v.slice.backing, v.slice.lo, v.slice.length, v.slice.cap_, true
}

// SliceNeedsBacking reports whether a slice value cannot be spelled
// as a plain literal: a nonzero lower bound or spare capacity beyond
// the length. Such values spell as a full backing literal plus a
// slice expression, preserving length, capacity, and tail values.
func (v Value) SliceNeedsBacking() bool {
	if v.slice == nil {
		return false
	}
	return v.slice.lo != 0 || v.slice.cap_ != v.slice.length
}

// SliceAccessible returns the accessible backing window
// backing[lo:cap_], or nil when the window is out of range. The
// capacity endpoint is an absolute backing offset, so the window
// length is cap_-lo.
func (v Value) SliceAccessible() []Value {
	if v.slice == nil || v.slice.backing == nil {
		return nil
	}
	b := *v.slice.backing
	lo, hi := v.slice.lo, v.slice.cap_
	if lo < 0 || lo > hi || hi > len(b) {
		return nil
	}
	return b[lo:hi]
}

// SliceVisible returns the visible elements backing[lo:lo+length],
// or nil when the window is out of range.
func (v Value) SliceVisible() []Value {
	if v.slice == nil || v.slice.backing == nil {
		return nil
	}
	b := *v.slice.backing
	lo, hi := v.slice.lo, v.slice.lo+v.slice.length
	if lo < 0 || hi > len(b) || lo > hi {
		return nil
	}
	return b[lo:hi]
}
