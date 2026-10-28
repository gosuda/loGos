// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// Shared evacuation containers, ported from fix-M19ImmixComplete
// logos_evac_mem.go. Parameterization vs the immix original: the address
// set carries the hash shift, so one implementation serves the immix
// block granule and the zgc page granule. Callers set shift (and the
// pin granule on logosEvac) before use; the zero value hashes with shift
// 0, which no caller may use. Merge note: keep this file identical to
// the immix lane's copy except the shift plumbing.

// logosWordBuf is a growable array of words in off-heap memory. The
// evacuation runs with the world stopped inside mark termination, where the
// Go heap must not be allocated from and pointer stores must not run write
// barriers, so every address it holds is a uintptr.
type logosWordBuf struct {
	base uintptr
	n    uintptr
	cap  uintptr
}

func (b *logosWordBuf) push(v uintptr) {
	if b.n == b.cap {
		b.grow(2 * b.cap)
	}
	*(*uintptr)(unsafe.Pointer(b.base + b.n*goarch.PtrSize)) = v
	b.n++
}

func (b *logosWordBuf) get(i uintptr) uintptr {
	return *(*uintptr)(unsafe.Pointer(b.base + i*goarch.PtrSize))
}

func (b *logosWordBuf) set(i, v uintptr) {
	*(*uintptr)(unsafe.Pointer(b.base + i*goarch.PtrSize)) = v
}

func (b *logosWordBuf) grow(words uintptr) {
	if words < 512 {
		words = 512
	}
	nb := sysAlloc(words*goarch.PtrSize, &memstats.other_sys, "logos evac")
	if nb == nil {
		throw("logos evacuation: out of memory")
	}
	if b.base != 0 {
		memmove(nb, unsafe.Pointer(b.base), b.n*goarch.PtrSize)
		sysFree(unsafe.Pointer(b.base), b.cap*goarch.PtrSize, &memstats.other_sys)
	}
	b.base = uintptr(nb)
	b.cap = words
}

func (b *logosWordBuf) release() {
	if b.base != 0 {
		sysFree(unsafe.Pointer(b.base), b.cap*goarch.PtrSize, &memstats.other_sys)
	}
	*b = logosWordBuf{}
}

// logosAddrSet is an insert-only set of nonzero addresses in off-heap memory.
type logosAddrSet struct {
	slots uintptr
	mask  uintptr
	count uintptr
	shift uint
}

func logosAddrHash(k, shift uintptr) uintptr {
	h := (k >> shift) * 0x9E3779B1
	return h ^ h>>15
}

func (s *logosAddrSet) slot(i uintptr) *uintptr {
	return (*uintptr)(unsafe.Pointer(s.slots + i*goarch.PtrSize))
}

func (s *logosAddrSet) add(k uintptr) {
	if (s.count+1)*2 > s.mask+1 {
		s.rehash()
	}
	for i := logosAddrHash(k, uintptr(s.shift)) & s.mask; ; i = (i + 1) & s.mask {
		cur := s.slot(i)
		if *cur == k {
			return
		}
		if *cur == 0 {
			*cur = k
			s.count++
			return
		}
	}
}

func (s *logosAddrSet) has(k uintptr) bool {
	if s.slots == 0 {
		return false
	}
	for i := logosAddrHash(k, uintptr(s.shift)) & s.mask; ; i = (i + 1) & s.mask {
		cur := *s.slot(i)
		if cur == k {
			return true
		}
		if cur == 0 {
			return false
		}
	}
}

func (s *logosAddrSet) rehash() {
	old, oldMask := s.slots, s.mask
	size := uintptr(1024)
	if old != 0 {
		size = (oldMask + 1) * 2
	}
	nb := sysAlloc(size*goarch.PtrSize, &memstats.other_sys, "logos evac")
	if nb == nil {
		throw("logos evacuation: out of memory")
	}
	s.slots = uintptr(nb)
	s.mask = size - 1
	s.count = 0
	if old == 0 {
		return
	}
	for i := uintptr(0); i <= oldMask; i++ {
		if k := *(*uintptr)(unsafe.Pointer(old + i*goarch.PtrSize)); k != 0 {
			s.add(k)
		}
	}
	sysFree(unsafe.Pointer(old), (oldMask+1)*goarch.PtrSize, &memstats.other_sys)
}

func (s *logosAddrSet) release() {
	if s.slots != 0 {
		sysFree(unsafe.Pointer(s.slots), (s.mask+1)*goarch.PtrSize, &memstats.other_sys)
	}
	*s = logosAddrSet{}
}
