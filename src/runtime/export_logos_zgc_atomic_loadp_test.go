// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && arm64

package runtime

import "internal/runtime/atomic"

// Test-only seam for the arm64 out-of-line atomic Loadp hook (W1d).
//
// Need: package runtime_test cannot name the unexported color state or
// call the linknamed hook, and the hook's test must save and restore the
// exact pre-test color state. The wrappers below are the minimal opaque
// projection of that state: a snapshot save/restore pair and a one-line
// pass-through to zgcAtomicLoadpSlow. They mint no pages, publish no
// forwarding entries, and color nothing. This file imports no testing.

type LogosZgcColorState struct {
	Tag    bool
	Mask   uintptr
	Good   uintptr
	Unmask uintptr
}

// LogosZgcColorSave captures the active color encoding and masks.
func LogosZgcColorSave() LogosZgcColorState {
	return LogosZgcColorState{
		Tag:    zgcHardwareTagging,
		Mask:   atomic.Loaduintptr(&zgcColorMaskBits),
		Good:   atomic.Loaduintptr(&zgcGoodMask),
		Unmask: atomic.Loaduintptr(&zgcUnmaskBits),
	}
}

// LogosZgcColorRestore reinstalls a state LogosZgcColorSave captured.
func LogosZgcColorRestore(s LogosZgcColorState) {
	zgcHardwareTagging = s.Tag
	atomic.Storeuintptr(&zgcColorMaskBits, s.Mask)
	atomic.Storeuintptr(&zgcGoodMask, s.Good)
	atomic.Storeuintptr(&zgcUnmaskBits, s.Unmask)
}

// LogosZgcAtomicLoadpSlow runs the out-of-line arm64 atomic Loadp hook on
// the word v loaded from slot.
func LogosZgcAtomicLoadpSlow(slot *uintptr, v uintptr) uintptr {
	return zgcAtomicLoadpSlow(slot, v)
}
