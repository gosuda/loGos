// errorcheck -0 -m

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package escape

import "unsafe"

func foreignDirect(n int) { // ERROR "can inline foreignDirect"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	_ = unsafe.Pointer(&b[0])
}

//go:noinline
func foreignPointerHelper(p *byte) { // ERROR "p does not escape"
	_ = unsafe.Pointer(p)
}

func foreignPointerHelperCaller(n int) { // ERROR "can inline foreignPointerHelperCaller"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	foreignPointerHelper(&b[0])
}

//go:noinline
func foreignPointerDepth(pp **byte) { // ERROR "pp does not escape"
	_ = unsafe.Pointer(*pp)
}

func foreignPointerDepthCaller(n int) { // ERROR "can inline foreignPointerDepthCaller"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	p := &b[0]
	foreignPointerDepth(&p)
}

func foreignSliceData(n int) { // ERROR "can inline foreignSliceData"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	p := unsafe.SliceData(b)
	p = (*byte)(unsafe.Add(unsafe.Pointer(p), 1))
	s := unsafe.Slice(p, n)
	_ = unsafe.Pointer(unsafe.SliceData(s))
}

func foreignLocalClosure(n int) { // ERROR "can inline foreignLocalClosure"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	use := func() {      // ERROR "can inline foreignLocalClosure.func1"
		_ = unsafe.Pointer(&b[0])
	}
	use() // ERROR "inlining call to foreignLocalClosure.func1"
}

//go:noinline
func foreignReturnThroughHelper(p *byte) *byte { // ERROR "leaking param: p to result ~r0 level=0"
	return p
}

func foreignReturnBoundary(n int) { // ERROR "can inline foreignReturnBoundary"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	_ = unsafe.Pointer(foreignReturnThroughHelper(&b[0]))
}

func foreignHeaderAndElement(n int) { // ERROR "can inline foreignHeaderAndElement"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	_ = unsafe.Pointer(&b)
	_ = unsafe.Pointer(&b[0])
}

func foreignFiveResults(p *byte) (int, int, int, int, *byte) { // ERROR "can inline foreignFiveResults" "leaking param: p to result ~r4 level=0"
	_ = unsafe.Pointer(p)
	return 0, 0, 0, 0, p
}

func foreignSliceContents(n int) { // ERROR "can inline foreignSliceContents"
	local := byte(1)      // ERROR "moved to heap: local"
	b := make([]*byte, n) // ERROR "make\(\[\]\*byte, n\) escapes to heap"
	b[0] = &local
	_ = unsafe.Pointer(unsafe.SliceData(b))
}

func foreignScalarControl() { // ERROR "can inline foreignScalarControl"
	word := uintptr(1)
	_ = uintptr(unsafe.Pointer(&word))
	var fixed [8]byte
	_ = uintptr(unsafe.Pointer(&fixed[0]))
}

func foreignHeaderOnly(n int) { // ERROR "can inline foreignHeaderOnly"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	_ = unsafe.Pointer(&b)
}

func foreignNonBoundary(n int) { // ERROR "can inline foreignNonBoundary"
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) does not escape"
	_ = b
	var fixed [8]byte
	_ = fixed
	var word uintptr
	_ = word
}

//go:noinline
func foreignConstantCapacity() {
	b := make([]byte, 16) // ERROR "make\(\[\]byte, 16\) does not escape"
	_ = unsafe.Pointer(&b[0])
}

//go:noinline
func foreignConstantCapacityExplicit() {
	b := make([]byte, 16, 64) // ERROR "make\(\[\]byte, 16, 64\) does not escape"
	_ = unsafe.Pointer(&b[0])
}

//go:noinline
func foreignConstantCapacityRecovered() {
	size := 32
	b := make([]byte, size) // ERROR "make\(\[\]byte, 32\) does not escape"
	_ = unsafe.Pointer(&b[0])
}

//go:noinline
func foreignDynamicLengthFixedCapacity(n int) {
	b := make([]byte, n, 8) // ERROR "make\(\[\]byte, n, 8\) does not escape"
	_ = unsafe.Pointer(&b[0])
}

//go:noinline
func foreignFixedLengthDynamicCapacity(n int) {
	b := make([]byte, 8, n) // ERROR "make\(\[\]byte, 8, n\) escapes to heap"
	_ = unsafe.Pointer(&b[0])
}
