// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	"math"
	"strconv"
)

var powersOfTen = [20]uint64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9,
	1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19,
}

// UintLen returns the number of bytes AppendUint writes for v. It reads the
// binary exponent from the float64 conversion of v, which can only
// overestimate the bit length by one for a v just below a power of two, and
// turns it into a decimal digit estimate that one table comparison
// corrects; there is no division and no loop. Setting the low bit leaves the
// digit count unchanged and makes 0 count as one digit.
func UintLen(v uint64) int {
	x := v | 1
	bitLen := int(math.Float64bits(float64(x))>>52) - 1022
	n := (bitLen * 1233) >> 12
	if x >= powersOfTen[n] {
		n++
	}
	return n
}

// IntLen returns the number of bytes AppendInt writes for v, including a
// leading minus sign.
func IntLen(v int64) int {
	if v >= 0 {
		return UintLen(uint64(v))
	}
	return 1 + UintLen(uint64(-v))
}

// AppendInt appends the decimal form of v to dst.
func AppendInt(dst []byte, v int64) []byte {
	return strconv.AppendInt(dst, v, 10)
}

// AppendUint appends the decimal form of v to dst.
func AppendUint(dst []byte, v uint64) []byte {
	return strconv.AppendUint(dst, v, 10)
}
