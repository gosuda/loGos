// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import "unsafe"

// Test hooks for the syscall foreign-admission lane (owner H-runtime).
//
// The compiler/runtime wire shape is mkbuiltin-compatible: rows travel
// as unsafe.Pointer to 2*n words ([Slot, Type] pairs) and the existing
// two-word token travels as (frame, gen) halves. These hooks re-expose
// that wire shape to external tests; they add no policy.

// LogosZgcSyscallMode reports whether the moving cycle runs in this
// binary. Cycle-dependent attacks skip when it is false; the disabled
// contract (invalid token, zero mutation) is asserted unconditionally.
func LogosZgcSyscallMode() bool { return zgcCycleEnabled() }

// LogosZgcSyscallTypeOf returns the *_type of v's dynamic type as the
// authoritative referent layout word for syscall rows. Tests use it to
// mimic the compiler-provided static pointee type.
func LogosZgcSyscallTypeOf(v any) unsafe.Pointer {
	return unsafe.Pointer(efaceOf(&v)._type)
}

// LogosZgcSyscallAdmit admits n [Slot, Type] pairs starting at rows.
func LogosZgcSyscallAdmit(rows unsafe.Pointer, n uintptr) (unsafe.Pointer, uint64) {
	return zgcSyscallAdmit(rows, n)
}

// LogosZgcSyscallAdmitIndirect admits only when code carries the
// foreign FuncFlag; otherwise it returns the invalid token.
func LogosZgcSyscallAdmitIndirect(code unsafe.Pointer, rows unsafe.Pointer, n uintptr) (unsafe.Pointer, uint64) {
	return zgcSyscallAdmitIndirect(code, rows, n)
}

// LogosZgcSyscallEnd releases the invocation named by (frame, gen).
func LogosZgcSyscallEnd(frame unsafe.Pointer, gen uint64) {
	zgcSyscallEnd(frame, gen)
}

// LogosZgcSyscallAdmitNoSplit is the nosplit leaf: pure check,
// token-free on success.
func LogosZgcSyscallAdmitNoSplit(rows unsafe.Pointer, n uintptr) (unsafe.Pointer, uint64) {
	return zgcSyscallAdmitNoSplit(rows, n)
}

// LogosZgcSyscallEndNoSplit pairs the nosplit leaf. Invalid tokens are
// a no-op.
func LogosZgcSyscallEndNoSplit(frame unsafe.Pointer, gen uint64) {
	zgcSyscallEndNoSplit(frame, gen)
}

// LogosZgcSyscallTokenValid reports whether (frame, gen) names live
// admission ownership.
func LogosZgcSyscallTokenValid(frame unsafe.Pointer, gen uint64) bool {
	return frame != nil && gen != 0
}

// LogosZgcSyscallExposed is the locking exact object-exposure query,
// used to prove retention while a frame is live and release after End.
func LogosZgcSyscallExposed(addr uintptr) bool {
	return zgcForeignObjectExposed(addr)
}
