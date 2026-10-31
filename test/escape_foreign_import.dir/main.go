package main

import (
	"./a"
	"os"
	"runtime"
)

//go:noinline
func foreignPointer(n int) uintptr {
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	return a.ForeignPointer(&b[0])
}

//go:noinline
func foreignSlice(n int) uintptr {
	b := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	return a.ForeignSlice(b)
}

//go:noinline
func preallocatedControl(b []byte) uintptr { // ERROR "b does not escape"
	return a.ForeignSlice(b)
}

//go:noinline
func foreignImportedConstantCapacity() byte {
	b := make([]byte, 16) // ERROR "make\(\[\]byte, 16\) does not escape"
	b[0] = 42
	p := &b[0]
	_ = a.ForeignSlice(b)
	return a.DereferenceByte(&p)
}

func main() {
	n := len(os.Args) + 257
	control := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	for i := range control {
		control[i] = byte(i)
	}
	cancel := make([]byte, n) // ERROR "make\(\[\]byte, n\) escapes to heap"
	cancel[0] = 42
	p := &cancel[0]
	q := &p
	if got := a.DereferenceByte(q); got != 42 {
		panic("imported dereference mismatch") // ERROR "escapes to heap"
	}
	if got := foreignPointer(n); got == 0 {
		panic("foreign pointer result was nil") // ERROR "escapes to heap"
	}
	if got := foreignSlice(n); got == 0 {
		panic("foreign slice result was nil") // ERROR "escapes to heap"
	}
	if got := preallocatedControl(control); got == 0 {
		panic("matched foreign control result was nil") // ERROR "escapes to heap"
	}
	if got := foreignImportedConstantCapacity(); got != 42 {
		panic("imported constant-capacity foreign value mismatch") // ERROR "escapes to heap"
	}
	_, _, _, _, returned := a.ReturnAndForeign(&control[0])
	if returned != &control[0] || *returned != control[0] {
		panic("imported result relation mismatch") // ERROR "escapes to heap"
	}
	runtime.KeepAlive(control) // ERROR "control does not escape"
}
