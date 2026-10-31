// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	stdjson "encoding/json"
	jsonv2 "encoding/json/v2"
	"reflect"
	"strings"
)

// maxWalkDepth mirrors jsontext maxNestingDepth: the encoder push fails
// once the stack holds this many tokens.
const maxWalkDepth = 10000

var walkMethodTypes = []reflect.Type{
	reflect.TypeFor[jsonv2.MarshalerTo](),
	marshalerType,
	textAppenderType,
	textMarshalerType,
}

// newWalkDepthError obtains the standard library's depth error using
// only its public API. This runs only after encoding has already failed,
// and parses synthetic JSON rather than revisiting the caller's graph.
func newWalkDepthError(written int64) *stdjson.SyntaxError {
	var v any
	err := stdjson.Unmarshal([]byte(strings.Repeat("[", maxWalkDepth+1)), &v)
	e := *err.(*stdjson.SyntaxError)
	e.Offset = written + 1
	return &e
}

// checkDepth reports the reflective max-depth error when nesting reaches
// the cap, mirroring the encoder push that fails when the stack is
// full. The offset carries the bytes written before the failing frame.
func (s *Walk) checkDepth(offset int64) error {
	if s.depth >= maxWalkDepth {
		s.depthErr = newWalkDepthError(offset)
		return s.depthErr
	}
	return nil
}

// checkMapKeys mirrors the v2 map arshaler: a nil map encodes as null
// without consulting the key type, so key validation applies only to
// non-nil maps. (v1 validates first; see walk_v1.go.)
func (s *Walk) checkMapKeys(rv reflect.Value) error {
	if rv.IsNil() {
		return nil
	}
	if !walkMapKeySupported(rv.Type().Key()) {
		return &stdjson.UnsupportedTypeError{Type: rv.Type()}
	}
	return nil
}

// sliceWalkLen reports the slice length for a visit key, or zero for
// other kinds, mirroring encoding/json/v2 sliceLen.
func sliceWalkLen(v reflect.Value) int {
	if v.Kind() == reflect.Slice {
		return v.Len()
	}
	return 0
}

// visitKey builds the ancestor-set key for a tracked value with the
// exact encoding/json/v2 shape: exact type, unsafe pointer, and slice
// length, mirroring typedPointer.
func visitKey(v reflect.Value) any {
	return walkVisit{v.Type(), v.UnsafePointer(), sliceWalkLen(v)}
}
