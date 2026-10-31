// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

import (
	"encoding/json/jsontext"
	"reflect"
	"strconv"
)

// indexedFieldPaths is true when encoding/json reports the Field of an
// *UnmarshalTypeError inside an array with the element index in the path,
// as it does with goexperiment.jsonv2: "A.0" for the first element of A.
const indexedFieldPaths = true

// rejectQuotedBare saves the *UnmarshalTypeError the jsonv2 bridge reports
// for a bare value in a field with the ,string option: Value is the JSON
// kind word ("number", "bool", "array", or "object") and Offset is the end
// of the value. Decoding continues.
func (s *Scanner) rejectQuotedBare(typ reflect.Type) {
	s.rejectKind(typ)
}

// rejectQuotedContent saves the *UnmarshalTypeError the jsonv2 bridge
// reports for malformed quoted content in a field with the ,string option.
// res is one of qSavedTag, qFatalTag, or qString; every row is saved, never
// fatal, so later fields still decode like the oracle. The Value mirrors
// the bridge: "number" plus " " plus the unquoted inner content for numeric
// destinations (so empty content yields "number " with a trailing space,
// via rejectNumber, exactly like the bridge append in v2_inject.go),
// "string" plus the raw outer token for bool destinations, and the kind
// word for string destinations.
func (s *Scanner) rejectQuotedContent(res quotedResult, item string, typ reflect.Type) {
	switch typ.Kind() {
	case reflect.String:
		s.rejectKind(typ)
	case reflect.Bool:
		s.save(s.typeError("string "+s.literal(), typ))
	default:
		s.rejectNumber(item, typ)
	}
}

// quotedIntFallback replays the v2 oracle's own lexical policy for quoted
// content the shared pre-check rejects: strconv.ParseInt decides. Content
// it accepts (such as a leading +) is set like the oracle sets it;
// content it rejects falls through to the classified error mapping above.
func quotedIntFallback(item string, bits int) (int64, bool) {
	v, err := strconv.ParseInt(item, 10, bits)
	if err != nil {
		return 0, false
	}
	return v, true
}

// quotedUintFallback is quotedIntFallback for unsigned integers. It replays
// the oracle's strconv.ParseUint policy verbatim: forms the oracle accepts
// set exactly as the oracle sets them, and forms it rejects fall through
// to the classified error mapping, which the bridge reports the same way.
func quotedUintFallback(item string, bits int) (uint64, bool) {
	v, err := strconv.ParseUint(item, 10, bits)
	if err != nil {
		return 0, false
	}
	return v, true
}

// quotedFloatFallback is quotedIntFallback for floating-point values,
// covering the noncanonical forms the v2 policy accepts (leading +,
// NaN, Inf).
func quotedFloatFallback(item string, bits int) (float64, bool) {
	v, err := strconv.ParseFloat(item, bits)
	if err != nil {
		return 0, false
	}
	return v, true
}

// unquoteStringContent unquotes a quoted inner string literal for StringQ
// with the strict syntax the v2 bridge-side decoder enforces, via the same
// jsontext primitive the oracle uses — so acceptance and the resulting
// value agree with the oracle by construction. Go-only escapes such as \'
// fail here and flow to qFatalTag, which the v2 content policy saves as
// Value "string" with the value untouched.
//
// Ownership: with dst nil, AppendUnquote grows a fresh buffer and only ever
// appends into it (it never returns src itself), so v is private, aliases
// nothing, and is never reused after return; the in-package unsafeString
// helper transfers it into a string without a second copy, mirroring
// stringBody's established fresh-buffer handling (nil/len0-safe).
func unquoteStringContent(item string) (string, bool) {
	v, err := jsontext.AppendUnquote(nil, item)
	if err != nil {
		return "", false
	}
	return unsafeString(v), true
}
