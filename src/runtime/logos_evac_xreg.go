// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64 || loong64)

package runtime

import "unsafe"

// pinXRegs pins the targets of the extended register state saved for an
// asynchronously preempted goroutine. Shared with the immix lane; the
// ported pin pass calls it through the shared logosEvac receiver.
func (ev *logosEvac) pinXRegs(gp *g) {
	if gp.xRegs.state == nil {
		return
	}
	b := uintptr(unsafe.Pointer(&gp.xRegs.state.regs))
	ev.pinRange(b, b+unsafe.Sizeof(gp.xRegs.state.regs))
}
