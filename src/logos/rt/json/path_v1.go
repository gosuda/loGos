// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

import "reflect"

// indexedFieldPaths is false when encoding/json reports the Field of an
// *UnmarshalTypeError inside an array without the element index, as it does
// without goexperiment.jsonv2: "A" for any element of A.
const indexedFieldPaths = false

// rejectQuotedBare saves the legacy encoding/json tag error for a bare
// (non-string) JSON value in a field with the ,string option. Decoding
// continues and the first saved error wins.
func (s *Scanner) rejectQuotedBare(typ reflect.Type) {
	s.save(tagUnquoted(typ))
}

// rejectQuotedContent preserves the legacy encoding/json policy for
// malformed quoted content in a field with the ,string option: res is one
// of qSavedTag, qFatalTag, or qString. Empty and n/t/f-led content saves a
// tagError, other malformed content is fatal, and valid inner string
// content for a non-string destination saves an *UnmarshalTypeError.
func (s *Scanner) rejectQuotedContent(res quotedResult, item string, typ reflect.Type) {
	switch res {
	case qSavedTag:
		s.save(tagMismatch(item, typ))
	case qFatalTag:
		s.fatal(tagMismatch(item, typ))
	case qString:
		s.rejectKind(typ)
	}
}

// quotedIntFallback offers a version-gated second chance for quoted
// content the shared lexical pre-check rejects. The legacy policy has no
// such content: everything encoding/json accepts here the pre-check
// already accepts, so this always declines and behavior is unchanged.
func quotedIntFallback(item string, bits int) (int64, bool) {
	return 0, false
}

// quotedUintFallback is quotedIntFallback for unsigned integers.
func quotedUintFallback(item string, bits int) (uint64, bool) {
	return 0, false
}

// quotedFloatFallback is quotedIntFallback for floating-point values.
func quotedFloatFallback(item string, bits int) (float64, bool) {
	return 0, false
}

// unquoteStringContent unquotes a quoted inner string literal for StringQ,
// preserving the legacy acceptance (including Go-only escapes such as \'),
// exactly as encoding/json without the jsonv2 experiment.
func unquoteStringContent(item string) (string, bool) {
	return unquoteLiteral(item)
}
