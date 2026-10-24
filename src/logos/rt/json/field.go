// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	stdjson "encoding/json"
	"reflect"
)

type isZeroer interface {
	IsZero() bool
}

var isZeroerType = reflect.TypeFor[isZeroer]()

// MarshalField is the per-field reflective fallback of generated MarshalJSON
// methods: it returns encoding/json.Marshal(v). Callers pass one field
// value and never the whole receiver, which would recurse into the
// generated method.
func MarshalField(v any) ([]byte, error) {
	return stdjson.Marshal(v)
}

// OmitEmpty reports whether encoding/json omits the field holding v under
// the omitempty option: an empty array, map, slice or string, and a false,
// zero, nil or zero-valued bool, number, pointer or interface. A nil
// interface is empty. A field whose static type is an interface must be
// compared with nil by the caller, because boxing a non-nil interface
// into v yields its dynamic value.
func OmitEmpty(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len() == 0
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Pointer:
		return rv.IsZero()
	}
	return false
}

func nilReference(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// OmitZero reports whether encoding/json omits the field holding v under
// the omitzero option. A nil pointer or interface is zero without calling
// any method. Otherwise, if v implements IsZero() bool, or a copy of v
// boxed into an addressable temporary does (a pointer-receiver method),
// that method decides; else the value is zero when reflect.Value.IsZero
// says so. A field whose static type is an interface without an IsZero
// method must be compared with nil by the caller, because boxing a
// non-nil interface into v yields its dynamic value.
func OmitZero(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	if nilReference(rv) {
		return true
	}
	if z, ok := v.(isZeroer); ok {
		return z.IsZero()
	}
	if !reflect.PointerTo(rv.Type()).Implements(isZeroerType) {
		return rv.IsZero()
	}
	box := reflect.New(rv.Type())
	box.Elem().Set(rv)
	return box.Interface().(isZeroer).IsZero()
}
