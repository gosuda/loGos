// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import "reflect"

type arrayStatus uint8

const (
	arrayFailed arrayStatus = iota
	arrayNull
	arrayOther
	arrayOpen
)

func (s *Scanner) arrayStart() arrayStatus {
	if s.err != nil {
		return arrayFailed
	}
	i := skipSpace(s.data, s.pos)
	if i < len(s.data) && s.data[i] == '[' {
		if s.depth+1 > maxDepth {
			s.fail()
			return arrayFailed
		}
		s.pos = i + 1
		s.depth++
		return arrayOpen
	}
	switch s.scalar() {
	case 0:
		return arrayFailed
	case 'n':
		return arrayNull
	}
	return arrayOther
}

func (s *Scanner) arrayEmpty() bool {
	i := skipSpace(s.data, s.pos)
	if i < len(s.data) && s.data[i] == ']' {
		s.pos = i + 1
		s.depth--
		return true
	}
	return false
}

func (s *Scanner) arrayNext() bool {
	if s.err != nil {
		return false
	}
	i := skipSpace(s.data, s.pos)
	if i >= len(s.data) {
		s.fail()
		return false
	}
	switch s.data[i] {
	case ',':
		s.pos = i + 1
		return true
	case ']':
		s.pos = i + 1
		s.depth--
		return false
	}
	s.fail()
	return false
}

// Slice decodes the JSON array at the cursor into *dst. null sets *dst to
// nil and [] sets an empty non-nil slice. Any other array decodes element i
// into the existing element i of *dst while i is below its length, like
// encoding/json (an element null or of the wrong kind leaves it alone), and
// appends a fresh zero element beyond that, so a slot past the old length
// never shows stale data. Nothing is pre-allocated: the slice grows by
// append only, so a hostile array costs memory only as its elements arrive.
// A value that is not an array saves one *UnmarshalTypeError and is skipped.
func Slice[E any](s *Scanner, dst *[]E, elem func(*Scanner, *E)) {
	switch s.arrayStart() {
	case arrayFailed:
		return
	case arrayNull:
		*dst = nil
		return
	case arrayOther:
		s.rejectKind(reflect.TypeFor[[]E]())
		return
	}
	out := *dst
	if out == nil {
		out = []E{}
	}
	if s.arrayEmpty() {
		*dst = out[:0]
		return
	}
	var zero E
	n := 0
	for {
		if n >= len(out) {
			out = append(out, zero)
		}
		before := s.saved
		elem(s, &out[n])
		s.indexNewError(before, n)
		n++
		if !s.arrayNext() {
			break
		}
	}
	*dst = out[:n]
}
