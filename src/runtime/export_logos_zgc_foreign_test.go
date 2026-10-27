// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import "unsafe"

// Exported helpers for tests and runtime wrappers.

const (
	LogosZgcGRunning = uint32(_Grunning)
	LogosZgcGSyscall = uint32(_Gsyscall)
)

func LogosZgcForeignGSetStatus(gp unsafe.Pointer, status uint32) {
	if gp != nil {
		(*g)(gp).atomicstatus.Store(status)
	}
}

func LogosZgcForeignHoldEnter(gp unsafe.Pointer, root uintptr) int {
	return logosZgcForeignHoldEnter((*g)(gp), root)
}

func LogosZgcForeignHoldExit(gp unsafe.Pointer) {
	logosZgcForeignHoldExit((*g)(gp))
}

func LogosZgcForeignHoldCount() int32 {
	return zgcForeignHoldCount.Load()
}

func LogosZgcForeignHoldDepth(gp unsafe.Pointer) uint32 {
	return logosZgcForeignHoldDepth((*g)(gp))
}

func LogosZgcPageHasActiveHold(pg unsafe.Pointer) bool {
	return zgcPageHasActiveHold((*zgcPage)(pg))
}

func LogosZgcForeignResetHolds() {
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		slot.active.Store(0)
		slot.root.Store(0)
		slot.epoch.Store(0)
		slot.depth.Store(0)
		slot.gp.Store(0)
	}
	zgcForeignHoldCount.Store(0)
}
