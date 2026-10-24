// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	stdjson "encoding/json"
	"testing"
)

type aliasTarget struct {
	N int `json:"abc"`
}

func TestSavedErrorDoesNotAliasInput(t *testing.T) {
	data := []byte(`{"abc":"x"}`)
	s := NewScanner(data, (*aliasTarget)(nil))
	var dst int
	if !s.Object() || !s.Next() {
		t.Fatal("no member")
	}
	Int(&s, &dst)
	copy(data, `{"ZZZ":"x"}`)
	ute, ok := s.Saved().(*stdjson.UnmarshalTypeError)
	if !ok {
		t.Fatalf("saved error = %v, want an UnmarshalTypeError", s.Saved())
	}
	if ute.Field != "abc" {
		t.Fatalf("Field = %q after the input buffer was reused, want abc", ute.Field)
	}
}

func TestSavedFallbackErrorDoesNotAliasInput(t *testing.T) {
	data := []byte(`{"abc":"x"}`)
	s := NewScanner(data, (*aliasTarget)(nil))
	var dst int
	if !s.Object() || !s.Next() {
		t.Fatal("no member")
	}
	s.Fallback(&dst)
	copy(data, `{"ZZZ":"x"}`)
	ute, ok := s.Saved().(*stdjson.UnmarshalTypeError)
	if !ok {
		t.Fatalf("saved error = %v, want an UnmarshalTypeError", s.Saved())
	}
	if ute.Field != "abc" {
		t.Fatalf("Field = %q after the input buffer was reused, want abc", ute.Field)
	}
}
