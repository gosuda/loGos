// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving

package runtime

import (
	"internal/runtime/syscall/linux"
	"unsafe"
)

const (
	zgcSysArchPrctl         = 158
	zgcArchGetUntagMask     = 0x4001
	zgcArchEnableTaggedAddr = 0x4002

	// zgcLAMBits is the number of ignored bits the hardware encoding needs:
	// bits 62..56, so that color bits 59..56 never reach translation. Linux
	// exposes only LAM_U57 (6 bits, 62..57) and refuses a larger request
	// with EINVAL before enabling anything, so on such kernels the process
	// keeps its address space untouched and zgc uses the software encoding.
	zgcLAMBits = 7
)

// zgcEnableHardwareTagging asks the kernel for x86 Linear Address Masking
// that ignores every color bit 59..56 and reports whether the MMU now
// ignores all of them, with the errno that refused otherwise. A mode that
// ignores only part of the color nibble (LAM_U57 leaves bit 56 an address
// bit) is not support: a pointer colored in bit 56 would fault.
func zgcEnableHardwareTagging() (ok bool, errno uintptr) {
	if mask, enabled := zgcUntagMask(); enabled {
		if mask&zgcColorMask != 0 {
			return false, _EINVAL
		}
		return true, 0
	}
	_, _, errno = linux.Syscall6(zgcSysArchPrctl, zgcArchEnableTaggedAddr, zgcLAMBits, 0, 0, 0, 0)
	if errno != 0 {
		return false, errno
	}
	if mask, _ := zgcUntagMask(); mask&zgcColorMask != 0 {
		return false, _EINVAL
	}
	return true, 0
}

// zgcUntagMask returns the address bits the MMU translates and whether
// any tagging mode is active.
func zgcUntagMask() (mask uintptr, enabled bool) {
	_, _, errno := linux.Syscall6(zgcSysArchPrctl, zgcArchGetUntagMask, uintptr(unsafe.Pointer(&mask)), 0, 0, 0, 0)
	return mask, errno == 0 && mask != ^uintptr(0)
}
