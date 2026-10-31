// asmcheck -goexperiment logosgcmoving

//go:build amd64 || arm64

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codegen

import (
	"syscall"
	"unsafe"
)

// Moving-GC syscall foreign-admission emission wiring verification
// (compiler half; THROWAWAY CLASS, not permanent sole proof).
//
// Under GOEXPERIMENT=logosgcmoving on amd64/arm64, every static call to a
// //go:uintptrkeepalive (or //go:uintptrescapes) declaration whose
// arguments contain unsafe.Pointer->uintptr conversions must be wrapped at
// order time: rows naming the SAME escape-temp cells the call reads,
// runtime.zgcSyscallAdmit before the call, runtime.zgcSyscallEnd after the
// captured results. Dynamic (func-value) sites with conversion-shaped
// arguments must spill their own temps and route through
// runtime.zgcSyscallAdmitIndirect; the runtime no-ops unflagged targets
// with zero mutation, so scalar hash/address consumers are untouched.
// Nosplit callers take the NoSplit leaf pair, never the splittable entry.
//
// RED before the compiler change: none of the CALL patterns below exist,
// so every positive assertion fails. GREEN after: the union build emits
// them. A link failure instead of an asmcheck failure means the runtime
// half has not landed yet; that is a landing-order signal, not emission
// proof. Honest classification: CALL-presence checks verify implementation
// wiring during bring-up only. Permanent proof is the behavioral storm in
// test/logos/compile/logos_zgc_syscall.lgo, which shows an old/fixed
// difference on consumer-visible kernel I/O.
//
// Bodyless declarations (rawSyscallNoError, rawVforkSyscall, gettimeofday)
// take the same static path via their implicit pragma; they are not
// callable from this package, so they are covered by pattern parity, not
// by a dedicated case here.

//go:noinline
func directSyscallPtr(fd uintptr, p unsafe.Pointer, n uintptr) (uintptr, syscall.Errno) {
	// amd64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
	// amd64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	r1, _, err := syscall.Syscall(syscall.SYS_READ, fd, uintptr(p), n)
	return r1, err
}

//go:noinline
func scalarSyscallOnly() uintptr {
	// amd64:-"zgcSyscallAdmit"
	// amd64:-"zgcSyscallEnd"
	// arm64:-"zgcSyscallAdmit"
	// arm64:-"zgcSyscallEnd"
	r1, _, _ := syscall.RawSyscall(syscall.SYS_GETPID, 0, 0, 0)
	return r1
}

//go:noinline
func indirectSyscallPtr(f func(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno), trap, a1 uintptr, p unsafe.Pointer) uintptr {
	// amd64:"CALL\\s+runtime\\.zgcSyscallAdmitIndirect\\(SB\\)"
	// amd64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallAdmitIndirect\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	r1, _, _ := f(trap, a1, uintptr(p), 0)
	return r1
}

//go:noinline
func deferSyscallPtr(fd uintptr, p unsafe.Pointer, n uintptr) {
	defer func() {
		// amd64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
		// amd64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
		// arm64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
		// arm64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
		_, _, _ = syscall.Syscall(syscall.SYS_READ, fd, uintptr(p), n)
	}()
}

//go:noinline
func goSyscallPtr(fd uintptr, p unsafe.Pointer, n uintptr, done chan struct{}) {
	go func() {
		defer close(done)
		// amd64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
		// amd64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
		// arm64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
		// arm64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
		_, _, _ = syscall.Syscall(syscall.SYS_READ, fd, uintptr(p), n)
	}()
}

func wrapperSyscallPtr(fd uintptr, p unsafe.Pointer, n uintptr) uintptr {
	// amd64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
	// amd64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	r1, _, _ := syscall.Syscall(syscall.SYS_READ, fd, uintptr(p), n)
	return r1
}

//go:noinline
func callerViaInlineWrapper(fd uintptr, p unsafe.Pointer, n uintptr) uintptr {
	// Inlined emission keeps the wrapper body's original source lines, so
	// no admission CALL is ever tagged with this return line; the wrapper
	// patterns above verify the emitted calls wherever compiled. This
	// negative pins the other half: the wrapper must actually inline here
	// (no direct CALL survives), so the inline-carry path is exercised.
	// amd64:-"wrapperSyscallPtr\\(SB\\)"
	// arm64:-"wrapperSyscallPtr\\(SB\\)"
	return wrapperSyscallPtr(fd, p, n)
}

//go:nosplit
//go:noinline
func nosplitSyscallPtr(fd uintptr, p unsafe.Pointer, n uintptr) uintptr {
	// amd64:"CALL\\s+runtime\\.zgcSyscallAdmitNoSplit\\(SB\\)"
	// amd64:"CALL\\s+runtime\\.zgcSyscallEndNoSplit\\(SB\\)"
	// amd64:-"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
	// amd64:-"CALL\\s+runtime\\.zgcSyscallAdmitIndirect\\(SB\\)"
	// amd64:-"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallAdmitNoSplit\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcSyscallEndNoSplit\\(SB\\)"
	// arm64:-"CALL\\s+runtime\\.zgcSyscallAdmit\\(SB\\)"
	// arm64:-"CALL\\s+runtime\\.zgcSyscallAdmitIndirect\\(SB\\)"
	// arm64:-"CALL\\s+runtime\\.zgcSyscallEnd\\(SB\\)"
	r1, _, _ := syscall.Syscall(syscall.SYS_READ, fd, uintptr(p), n)
	return r1
}
