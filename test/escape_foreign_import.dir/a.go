package a

import "unsafe"

//go:noinline
func ForeignPointer(p *byte) uintptr { // ERROR "p does not escape"
	return uintptr(unsafe.Pointer(p))
}

//go:noinline
func ForeignSlice(b []byte) uintptr { // ERROR "b does not escape"
	return uintptr(unsafe.Pointer(&b[0]))
}

//go:noinline
func DereferenceByte(p **byte) byte { // ERROR "p does not escape"
	_ = unsafe.Pointer(*p)
	return **p
}

//go:noinline
func ReturnAndForeign(p *byte) (int, int, int, int, *byte) { // ERROR "leaking param: p to result ~r4 level=0"
	_ = unsafe.Pointer(p)
	return 0, 0, 0, 0, p
}
