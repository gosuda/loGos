// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

import (
	stdjson "encoding/json"
	"reflect"
)

// checkDepth reports no error: without goexperiment.jsonv2 the
// reflective encoder sets no nesting cap, so the walk never fails on
// depth.
func (s *Walk) checkDepth(offset int64) error {
	return nil
}

var walkMethodTypes = []reflect.Type{marshalerType, textMarshalerType}

// checkMapKeys rejects maps whose key type the reflective encoder
// cannot resolve, before the nil fast path: newMapEncoder selects
// unsupportedTypeEncoder from the key type without consulting nilness,
// so even a nil map with an unsupported key fails.
func (s *Walk) checkMapKeys(rv reflect.Value) error {
	if !walkMapKeySupported(rv.Type().Key()) {
		return &stdjson.UnsupportedTypeError{Type: rv.Type()}
	}
	return nil
}

// visitKey builds the ancestor-set key for a tracked value with the
// exact encoding/json shapes: interface-boxed pointers for pointer
// edges, unsafe pointer plus length for slices, and the unsafe pointer
// for maps. Types are not part of slice and map keys upstream; the same
// imprecision is kept so repeat detection matches the reflective
// encoder exactly.
func visitKey(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Pointer:
		return v.Interface()
	case reflect.Slice:
		return struct {
			ptr any
			len int
		}{v.UnsafePointer(), v.Len()}
	case reflect.Map:
		return v.UnsafePointer()
	}
	return nil
}
