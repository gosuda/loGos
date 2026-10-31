// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	"encoding"
	"encoding/base64"
	stdjson "encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// This file carries one root MarshalJSON call's traversal state and the
// reflective fallback walk (M11 R2/R4). Generated helpers encode the
// statically spelled edges of types the root package can name; every
// other stateful edge (slices, arrays, maps, interfaces, unspellable
// types) funnels through Walk.Marshal, which keeps the same seen set so
// cycles spanning static and dynamic edges terminate with the reflective
// cycle error instead of overflowing the stack.
//
// Value encoding reuses the package helpers (AppendEscaped,
// AppendQuoted, EscapedLen, QuotedLen, AppendInt, AppendUint,
// AppendFloat, FloatErr) so no escaping or number rule is duplicated
// here; json.Number, whose literal rules belong to the reflective
// encoder, is delegated to MarshalField on the Number itself. Only
// field selection (which fields a struct value exposes) is
// ported in walkfields.go, because no helper provides it: it follows the
// encoding/json DefaultOptionsV1 rules for method-free values. Values
// with JSON methods are never walked: exact dynamic types in the root's
// Gen table are walked reflectively with shared state, every other
// method-having type is honoured through MarshalField, which starts a
// fresh encoding exactly like the upstream honoured-method branch and
// preserves genuine user overrides.
//
// Cycle tracking mirrors the reflective modes: pointers, maps, and
// slices bump ptrLevel and join the seen set once ptrLevel passes 1000
// (startDetectingCyclesAfter in both encoding/json and
// encoding/json/v2). Struct, array, and interface frames bump nothing,
// matching the reflective encoders. Detection timing past the threshold
// is unobservable (the error carries only the repeated type and no
// partial output is kept), so one counter serves both modes; only the
// visit-key shape is mode-specific (see walk_v1.go and walk_v2.go).
// Object and array frames also track JSON nesting: v2 enforces the
// jsontext 10000-frame cap, while v1 imposes no nesting cap. Depth
// errors come from the public JSON parser and are rebased across
// private helper buffers using exact emitted prefixes.

// Walk is one root MarshalJSON call's traversal state. The zero value
// is ready: Gen is filled by generated code before any stateful edge,
// and the seen set is created lazily past the threshold so the flat
// scalar path never allocates.
type Walk struct {
	// Gen holds the exact generated struct types this root may meet
	// through interface or reflective edges, as built by generated
	// AddType calls. Membership decides dispatch, never output.
	Gen []reflect.Type
	// fields caches the per-type field listing inside one call.
	fields map[reflect.Type][]walkField
	// ptrLevel counts pointer, map, and slice frames, mirroring the
	// reflective encoders.
	ptrLevel uint
	// depth counts object and array frames, mirroring jsontext nesting
	// for the v2 max-depth cap. v1 sets no cap; checkDepth is mode-gated.
	depth    uint
	seen     map[any]struct{}
	depthErr *stdjson.SyntaxError
}

// walkVisit is the v2 visit key: exact type, pointer, and slice length,
// mirroring encoding/json/v2 typedPointer. v1 keys are built in
// walk_v1.go with the exact v1 shapes.
type walkVisit struct {
	typ reflect.Type
	ptr any // unsafe.Pointer boxed, so this file imports no unsafe
	len int
}

// AddType records t and its pointer/value counterpart for dispatch. A
// nil t is ignored so generated code needs no nil guard around
// TypeOf on interface values. Generated code emits AddType only for
// statically typed paths the compiler knows are generated-side (plus
// the root pair); interface-typed paths never contribute, so a user
// method type met only dynamically can never enter Gen.
func (s *Walk) AddType(t reflect.Type) {
	if t == nil {
		return
	}
	s.Gen = append(s.Gen, t)
	if t.Kind() == reflect.Pointer {
		s.Gen = append(s.Gen, t.Elem())
	} else {
		s.Gen = append(s.Gen, reflect.PointerTo(t))
	}
}

// inGen reports whether the exact dynamic type t is a generated-side
// type of this root.
func (s *Walk) inGen(t reflect.Type) bool {
	for _, g := range s.Gen {
		if g == t {
			return true
		}
	}
	return false
}

// cycleError spells the reflective cycle error for the repeated value.
func cycleError(v reflect.Value) error {
	return &stdjson.UnsupportedValueError{Value: v, Str: "encountered a cycle via " + v.Type().String()}
}

// EnterFrame records one object or array frame. offset is relative to
// the current encoding buffer; helper callers rebase a depth error as
// it unwinds through their exact emitted prefixes.
func (s *Walk) EnterFrame(offset int64) error {
	if err := s.checkDepth(offset); err != nil {
		return err
	}
	s.depth++
	return nil
}

// RebaseDepthError translates this walk's own depth error from a nested
// value buffer to its caller's buffer. User errors, including user
// SyntaxErrors, are returned untouched.
func (s *Walk) RebaseDepthError(err error, prefix int64) error {
	if s.depthErr != nil && err == s.depthErr {
		s.depthErr.Offset += prefix
	}
	return err
}

// LeaveFrame closes an enterFrame edge.
func (s *Walk) LeaveFrame() {
	s.depth--
}

// Enter records a pointer, map, or slice edge for the value held in p,
// reporting the reflective cycle error on a repeat visit past the
// threshold. Generated code calls Enter with the edge value itself;
// other kinds are ignored so sites need no kind guard.
func (s *Walk) Enter(p any) error {
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
	default:
		return nil
	}
	return s.enterValue(v)
}

// Leave closes an Enter edge, dropping the visit from the ancestor set.
func (s *Walk) Leave(p any) {
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
	default:
		return
	}
	s.leaveValue(v)
}

// enterValue is the reflect.Value core of Enter. A repeat visit returns
// the cycle error without recording and rolls back its own level, so
// the counters stay balanced even if the caller inspects the Walk after
// an error; every ancestor still runs its leaveValue on unwind.
func (s *Walk) enterValue(v reflect.Value) error {
	s.ptrLevel++
	if s.ptrLevel > 1000 {
		k := visitKey(v)
		if _, ok := s.seen[k]; ok {
			s.ptrLevel--
			return cycleError(v)
		}
		if s.seen == nil {
			s.seen = make(map[any]struct{})
		}
		s.seen[k] = struct{}{}
	}
	return nil
}

// leaveValue is the reflect.Value core of Leave.
func (s *Walk) leaveValue(v reflect.Value) {
	if s.ptrLevel > 1000 {
		delete(s.seen, visitKey(v))
	}
	s.ptrLevel--
}

// Marshal encodes v with shared traversal state, returning fresh bytes
// like MarshalField. It is the funnel for every stateful edge the
// generated helpers cannot spell: slices, arrays, maps, interfaces, and
// unspellable or dynamic struct values.
func (s *Walk) Marshal(v any) ([]byte, error) {
	buf, err := s.appendValue(nil, reflect.ValueOf(v))
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// appendValue encodes rv, returning the extended buffer.
func (s *Walk) appendValue(buf []byte, rv reflect.Value) ([]byte, error) {
	if !rv.IsValid() {
		return append(buf, "null"...), nil
	}
	t := rv.Type()
	// Exact json.Number precedes both the method check and the string
	// branch: under goexperiment.jsonv2 Number implements MarshalJSONTo,
	// so the honour branch would catch it first. Routing it here keeps
	// numeric emission and validation in both modes.
	if t == numberType {
		return appendNumber(buf, rv)
	}
	if s.honorsJSONMethod(rv) {
		return appendHonored(buf, rv)
	}
	switch rv.Kind() {
	case reflect.Interface:
		if rv.IsNil() {
			return append(buf, "null"...), nil
		}
		return s.appendValue(buf, rv.Elem())
	case reflect.Pointer:
		if rv.IsNil() {
			return append(buf, "null"...), nil
		}
		if err := s.enterValue(rv); err != nil {
			return buf, err
		}
		buf, err := s.appendValue(buf, rv.Elem())
		s.leaveValue(rv)
		return buf, err
	case reflect.Bool:
		if rv.Bool() {
			return append(buf, "true"...), nil
		}
		return append(buf, "false"...), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return AppendInt(buf, rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return AppendUint(buf, rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		bits := int(t.Size()) * 8
		f := rv.Float()
		if f-f != 0 {
			return buf, FloatErr(rv.Interface(), f, bits)
		}
		return AppendFloat(buf, f, bits), nil
	case reflect.String:
		buf = append(buf, '"')
		buf = AppendEscaped(buf, rv.String())
		return append(buf, '"'), nil
	case reflect.Slice:
		if rv.IsNil() {
			return append(buf, "null"...), nil
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 && !sliceElemHasJSONMethod(rv.Type().Elem()) {
			buf = append(buf, '"')
			buf = append(buf, base64.StdEncoding.EncodeToString(rv.Bytes())...)
			return append(buf, '"'), nil
		}
		if err := s.EnterFrame(int64(len(buf))); err != nil {
			return buf, err
		}
		defer s.LeaveFrame()
		if err := s.enterValue(rv); err != nil {
			return buf, err
		}
		buf = append(buf, '[')
		for i := range rv.Len() {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			buf, err = s.appendValue(buf, rv.Index(i))
			if err != nil {
				s.leaveValue(rv)
				return buf, err
			}
		}
		buf = append(buf, ']')
		s.leaveValue(rv)
		return buf, nil
	case reflect.Array:
		if err := s.EnterFrame(int64(len(buf))); err != nil {
			return buf, err
		}
		defer s.LeaveFrame()
		buf = append(buf, '[')
		for i := range rv.Len() {
			if i > 0 {
				buf = append(buf, ',')
			}
			var err error
			buf, err = s.appendValue(buf, rv.Index(i))
			if err != nil {
				return buf, err
			}
		}
		return append(buf, ']'), nil
	case reflect.Map:
		if err := s.checkMapKeys(rv); err != nil {
			return buf, err
		}
		if rv.IsNil() {
			return append(buf, "null"...), nil
		}
		if err := s.EnterFrame(int64(len(buf))); err != nil {
			return buf, err
		}
		defer s.LeaveFrame()
		if err := s.enterValue(rv); err != nil {
			return buf, err
		}
		buf, err := s.appendMap(buf, rv)
		s.leaveValue(rv)
		return buf, err
	case reflect.Struct:
		return s.appendStruct(buf, rv)
	}
	return buf, &stdjson.UnsupportedTypeError{Type: t}
}

// numberType is the exact encoding/json.Number type. Like the
// reflective encoders, number handling is keyed on type identity, not
// on the string kind: an alias of Number is Number, while a defined
// type whose underlying type is Number is an ordinary string.
var numberType = reflect.TypeFor[stdjson.Number]()

// numberBody encodes rv, a value of exactly type json.Number, through
// MarshalField on the Number itself. The bytes and the error come from
// the real reflective encoder, so the empty-to-0 rule, the number
// grammar, and the error value and type of the active mode are never
// restated here.
func numberBody(rv reflect.Value) ([]byte, error) {
	return MarshalField(stdjson.Number(rv.String()))
}

// appendNumber encodes a json.Number as a bare JSON number.
func appendNumber(buf []byte, rv reflect.Value) ([]byte, error) {
	b, err := numberBody(rv)
	if err != nil {
		return buf, err
	}
	return append(buf, b...), nil
}

// appendQuotedNumber encodes a json.Number under the `string` option.
// A valid literal is a complete JSON number token, so the option quotes
// it by wrapping the bytes numberBody returned.
func appendQuotedNumber(buf []byte, rv reflect.Value) ([]byte, error) {
	b, err := numberBody(rv)
	if err != nil {
		return buf, err
	}
	buf = append(buf, '"')
	buf = append(buf, b...)
	return append(buf, '"'), nil
}

// appendStringOptionValue encodes a scalar field carrying the `string`
// struct-tag option: the value body inside JSON quotes. Only scalar
// kinds reach here, optionally behind one unnamed pointer; the field
// listing sets the option on those kinds alone, matching the
// reflective encoder. A honoured MarshalJSON or MarshalText method
// takes precedence over the option and ignores it, exactly as the
// reflective method encoders do, so it is dispatched before the kind
// switch and also covers a nil or non-nil pointer to a method type.
// The one exception, under the option only, is exact json.Number and
// non-nil *json.Number: they are quoted before the method check, because
// under goexperiment.jsonv2 Number implements MarshalJSONTo, which would
// emit the literal unquoted. Every other honoured method keeps precedence.
func (s *Walk) appendStringOptionValue(buf []byte, rv reflect.Value) ([]byte, error) {
	if rv.Type() == numberType {
		return appendQuotedNumber(buf, rv)
	}
	if rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Type().Elem() == numberType {
		return appendQuotedNumber(buf, rv.Elem())
	}
	if s.honorsJSONMethod(rv) {
		return appendHonored(buf, rv)
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return append(buf, "null"...), nil
		}
		return s.appendStringOptionValue(buf, rv.Elem())
	}
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			return append(buf, `"true"`...), nil
		}
		return append(buf, `"false"`...), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf = append(buf, '"')
		buf = AppendInt(buf, rv.Int())
		return append(buf, '"'), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf = append(buf, '"')
		buf = AppendUint(buf, rv.Uint())
		return append(buf, '"'), nil
	case reflect.Float32, reflect.Float64:
		bits := int(rv.Type().Size()) * 8
		f := rv.Float()
		if f-f != 0 {
			return buf, FloatErr(rv.Interface(), f, bits)
		}
		buf = append(buf, '"')
		buf = AppendFloat(buf, f, bits)
		return append(buf, '"'), nil
	case reflect.String:
		buf = append(buf, '"')
		buf = AppendQuoted(buf, rv.String())
		return append(buf, '"'), nil
	}
	return s.appendValue(buf, rv)
}

// honorsJSONMethod reports whether the walk hands rv to its JSON method
// instead of walking it: the reflective encoder would honour a method
// on rv's type (see hasJSONMethod), and the exact type is not
// generated-side. Types in Gen are walked reflectively with shared
// state even though their generated MarshalJSON would be honoured.
// Shared by appendValue and appendStringOptionValue so both contexts
// pick the same values.
func (s *Walk) honorsJSONMethod(rv reflect.Value) bool {
	return !s.inGen(rv.Type()) && hasJSONMethod(rv)
}

// appendHonored encodes rv, a value honorsJSONMethod accepted, through
// MarshalField, which starts a fresh encoding exactly like the upstream
// honoured-method branch: a nil pointer encodes null, a method error is
// returned as the encoder reports it, and the `string` option never
// applies.
func appendHonored(buf []byte, rv reflect.Value) ([]byte, error) {
	b, err := MarshalField(addrWalkInterface(rv))
	if err != nil {
		return buf, err
	}
	return append(buf, b...), nil
}

// addrWalkInterface boxes rv for the honour branch so MarshalField
// meets the method the reflective encoder would pick: an address-only
// method is reached through the address of an addressable rv, anything
// else through the value itself. A value-receiver method is in the
// method set of both T and *T, so checking the value method before the
// address-only one selects the same method as the mode-specific method
// precedence in walkMethodTypes.
func addrWalkInterface(rv reflect.Value) any {
	t := rv.Type()
	for _, iface := range walkMethodTypes {
		if t.Implements(iface) {
			return rv.Interface()
		}
		if t.Kind() != reflect.Pointer && rv.CanAddr() && reflect.PointerTo(t).Implements(iface) {
			return rv.Addr().Interface()
		}
	}
	return rv.Interface()
}

// hasJSONMethod reports whether the reflective encoder would honour a
// method on a value of rv's type, using the mode-specific method
// precedence in walkMethodTypes. Generated-side types never reach here;
// the caller filters Gen membership first.
func hasJSONMethod(rv reflect.Value) bool {
	t := rv.Type()
	for _, iface := range walkMethodTypes {
		if t.Implements(iface) {
			return true
		}
		if t.Kind() != reflect.Pointer && rv.CanAddr() && reflect.PointerTo(t).Implements(iface) {
			return true
		}
	}
	return false
}

// sliceElemHasJSONMethod reports whether a byte-slice fast path is
// barred because the element type carries a JSON method, matching
// newSliceEncoder.
func sliceElemHasJSONMethod(elem reflect.Type) bool {
	for _, iface := range walkMethodTypes {
		if elem.Implements(iface) {
			return true
		}
		if reflect.PointerTo(elem).Implements(iface) {
			return true
		}
	}
	return false
}

var (
	marshalerType     = reflect.TypeFor[stdjson.Marshaler]()
	textAppenderType  = reflect.TypeFor[encoding.TextAppender]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// walkMapKeySupported mirrors newMapEncoder: only string and integer
// key kinds, or keys implementing TextMarshaler, are encodable. Any
// other key type fails with UnsupportedTypeError for the map type
// before any element encodes.
func walkMapKeySupported(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return t.Implements(textMarshalerType)
}

// appendMap encodes a non-nil map with string-sorted keys, mirroring
// mapEncoder. Keys were validated by checkMapKeys before the nil fast
// path; a resolution failure is reported as an encoding error for the
// map type, exactly like the reflective encoder.
func (s *Walk) appendMap(buf []byte, rv reflect.Value) ([]byte, error) {
	type kv struct {
		ks string
		v  reflect.Value
	}
	kvs := make([]kv, 0, rv.Len())
	mi := rv.MapRange()
	for mi.Next() {
		ks, err := resolveWalkKey(mi.Key())
		if err != nil {
			return buf, fmt.Errorf("json: encoding error for type %q: %q", rv.Type().String(), err.Error())
		}
		kvs = append(kvs, kv{ks: ks, v: mi.Value()})
	}
	slices.SortFunc(kvs, func(a, b kv) int { return strings.Compare(a.ks, b.ks) })
	buf = append(buf, '{')
	for i := range kvs {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '"')
		buf = AppendEscaped(buf, kvs[i].ks)
		buf = append(buf, '"', ':')
		var err error
		buf, err = s.appendValue(buf, kvs[i].v)
		if err != nil {
			return buf, err
		}
	}
	return append(buf, '}'), nil
}

// resolveWalkKey mirrors resolveKeyName: strings pass through, integer
// kinds format in base 10, TextMarshaler keys are honoured. Any other
// key kind panics, exactly like the reflective encoder.
func resolveWalkKey(k reflect.Value) (string, error) {
	if k.Kind() == reflect.String {
		return k.String(), nil
	}
	if tm, ok := reflect.TypeAssert[encoding.TextMarshaler](k); ok {
		if k.Kind() == reflect.Pointer && k.IsNil() {
			return "", nil
		}
		text, err := tm.MarshalText()
		if err != nil {
			return "", err
		}
		return string(text), nil
	}
	switch k.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), nil
	}
	panic("unexpected map key type")
}
