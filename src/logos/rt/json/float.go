// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	stdjson "encoding/json"
	"math"
	"reflect"
	"strconv"
)

// MaxFloatLen64 is the largest number of bytes AppendFloat writes for a
// finite value with bits 64, sign included. The longest output is a
// negative number in [1e-6, 1e-5) with 17 shortest digits in the plain
// form: sign, "0.", five zeros and 17 digits.
const MaxFloatLen64 = 25

// MaxFloatLen32 is the largest number of bytes AppendFloat writes for a
// finite value with bits 32, sign included. The longest output is a
// negative number in [1e20, 1e21) in the plain form: sign and 21 digits.
const MaxFloatLen32 = 22

func floatUsesExponent(abs float64, bits int) bool {
	if abs == 0 {
		return false
	}
	if bits == 32 {
		return float32(abs) < 1e-6 || float32(abs) >= 1e21
	}
	return abs < 1e-6 || abs >= 1e21
}

func trimExponentZero(b []byte) []byte {
	n := len(b)
	if n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		b[n-2] = b[n-1]
		b = b[:n-1]
	}
	return b
}

// AppendFloat appends f to dst as encoding/json formats a JSON number,
// following the ES6 number to string conversion: the shortest digits that
// round-trip for bits 32 or 64, an exponent form below 1e-6 and from 1e21,
// no zero padding of the exponent and "-0" for negative zero. The caller
// guarantees that f is finite; see FloatErr.
func AppendFloat(dst []byte, f float64, bits int) []byte {
	if bits == 32 {
		f = float64(float32(f))
	}
	if !floatUsesExponent(math.Abs(f), bits) {
		return strconv.AppendFloat(dst, f, 'f', -1, bits)
	}
	return trimExponentZero(strconv.AppendFloat(dst, f, 'e', -1, bits))
}

// FloatErr returns nil when f is finite and otherwise the
// *encoding/json.UnsupportedValueError encoding/json reports for it. v is
// the original field value, of any float type.
func FloatErr(v any, f float64, bits int) error {
	if !math.IsInf(f, 0) && !math.IsNaN(f) {
		return nil
	}
	return &stdjson.UnsupportedValueError{
		Value: reflect.ValueOf(v),
		Str:   strconv.FormatFloat(f, 'g', -1, bits),
	}
}
