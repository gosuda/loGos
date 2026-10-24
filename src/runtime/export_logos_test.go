// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// LogosEnterFastCallForTest records a fast-call entry on the current M and
// reports the observed slots, timestamp, and flag.
func LogosEnterFastCallForTest(arg unsafe.Pointer, now int64) (slots [4]uintptr, stamp int64, flag bool) {
	mp := getg().m
	logosRecordFastCall(mp, arg, now)
	return mp.fastCgoFrame, mp.fastCallEnterNano, mp.inFastCall
}

// LogosExitFastCallForTest clears a fast-call record on the current M and
// reports the observed slots, timestamp, and flag.
func LogosExitFastCallForTest() (slots [4]uintptr, stamp int64, flag bool) {
	mp := getg().m
	logosClearFastCall(mp)
	return mp.fastCgoFrame, mp.fastCallEnterNano, mp.inFastCall
}

// LogosGrowsliceBufForTest calls growsliceBuf for elements of the dynamic
// type of elem, using buf (bufLen elements, already holding newLen-num
// elements) as the backing store, and reports the resulting slice header.
func LogosGrowsliceBufForTest(elem any, buf unsafe.Pointer, bufLen, newLen, num int) (array unsafe.Pointer, length, capacity int) {
	s := growsliceBuf(buf, newLen, bufLen, num, efaceOf(&elem)._type, buf, bufLen)
	return s.array, s.len, s.cap
}
