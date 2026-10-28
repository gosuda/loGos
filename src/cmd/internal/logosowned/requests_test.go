// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logosowned

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// goodSource is a minimal helper file; tests assert behavior about it,
// never its exact formatted bytes.
const goodSource = "package owner\n\n// Value reports the owned zero.\nfunc Value() int { return 0 }\n"

func mustRequest(t *testing.T, owner, pkg, source string, decls []Declaration) Request {
	t.Helper()
	if decls == nil {
		t.Fatalf("mustRequest requires non-nil declarations (empty allowed)")
	}
	r, err := NewRequest(owner, pkg, source, decls)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

func TestRoundTrip(t *testing.T) {
	r := mustRequest(t, "example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: "a.go"}})
	data, err := Encode([]Request{r})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], r) {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestRoundTripEmptyDeclarations(t *testing.T) {
	r := mustRequest(t, "example.com/mod/owner", "owner", goodSource, []Declaration{})
	data, err := Encode([]Request{r})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], r) {
		t.Fatalf("empty declarations round trip mismatch: %#v", got)
	}
	if got[0].Declarations == nil {
		t.Fatalf("empty declarations decoded to null")
	}
}

func TestEncodeEmptyIsArray(t *testing.T) {
	data, err := Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reqs, ok := m["requests"].([]any)
	if m["version"] != 1.0 || !ok || len(reqs) != 0 {
		t.Fatalf("empty envelope = %s", data)
	}
}

func TestNewRequestFormatsBeforeHash(t *testing.T) {
	// Unformatted input must still hash the formatted bytes.
	r := mustRequest(t, "example.com/mod/owner", "owner", "package owner\nfunc Value() int{return 0}\n", []Declaration{})
	sum := sha256.Sum256([]byte(r.Source))
	if r.ID != hex.EncodeToString(sum[:]) {
		t.Fatalf("ID is not the content hash")
	}
	if strings.Contains(r.Source, "int{return") {
		t.Fatalf("source was not formatted before hashing")
	}
}

func TestNewRequestCanonicalizesDeclarations(t *testing.T) {
	r := mustRequest(t, "example.com/mod/owner", "owner", goodSource, []Declaration{
		{Owner: "example.com/mod/b", File: "b.go"},
		{Owner: "example.com/mod/a", File: "a.go"},
		{Owner: "example.com/mod/a", File: "a.go"},
	})
	want := []Declaration{
		{Owner: "example.com/mod/a", File: "a.go"},
		{Owner: "example.com/mod/b", File: "b.go"},
	}
	if !reflect.DeepEqual(r.Declarations, want) {
		t.Fatalf("declarations not canonicalized: %#v", r.Declarations)
	}
	// Same source with distinct provenance keeps the same ID: the ID
	// hashes only the source, so both variants survive until the
	// coordinator merges same-body conditions.
	other := mustRequest(t, "example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: "other.go"}})
	if other.ID != r.ID {
		t.Fatalf("same source has different IDs: %s vs %s", other.ID, r.ID)
	}
	if reflect.DeepEqual(other, r) {
		t.Fatalf("distinct provenance compared equal")
	}
}

func TestDecodeRejects(t *testing.T) {
	r := mustRequest(t, "example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: "a.go"}})
	good, err := Encode([]Request{r})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	mutate := func(f func(m map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(good, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		f(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return out
	}
	reject := func(name string, data []byte) {
		t.Helper()
		if _, err := Decode(data); err == nil {
			t.Errorf("%s: Decode accepted malformed document", name)
		}
	}
	reject("unknown envelope field", mutate(func(m map[string]any) { m["extra"] = 1 }))
	reject("unknown request field", mutate(func(m map[string]any) {
		m["requests"].([]any)[0].(map[string]any)["extra"] = 1
	}))
	reject("wrong version", mutate(func(m map[string]any) { m["version"] = 2 }))
	reject("missing version", mutate(func(m map[string]any) { delete(m, "version") }))
	reject("missing requests", mutate(func(m map[string]any) { delete(m, "requests") }))
	reject("null requests", mutate(func(m map[string]any) { m["requests"] = nil }))
	reject("requests object", mutate(func(m map[string]any) { m["requests"] = map[string]any{} }))
	reject("trailing value", append(bytes.Clone(good), []byte(`{}`)...))
	reject("trailing null", append(bytes.Clone(good), []byte(`null`)...))
	reject("empty", []byte{})
	// JSON null for a request's source fails the empty-source check, so
	// a null cannot pose as the typed string.
	reject("null source", []byte(`{"version":1,"requests":[{"owner":"example.com/o","package":"o","id":"`+strings.Repeat("0", 64)+`","source":null,"declarations":[]}]}`))
	// Null and missing declaration arrays are invalid; empty is valid.
	reject("null declarations", []byte(`{"version":1,"requests":[{"owner":"example.com/o","package":"o","id":"`+strings.Repeat("0", 64)+`","source":"package o\n","declarations":null}]}`))
	reject("missing declarations", []byte(`{"version":1,"requests":[{"owner":"example.com/o","package":"o","id":"`+strings.Repeat("0", 64)+`","source":"package o\n"}]}`))
	reject("unknown declaration field", mutate(func(m map[string]any) {
		m["requests"].([]any)[0].(map[string]any)["declarations"].([]any)[0].(map[string]any)["extra"] = 1
	}))
	// Trailing whitespace carries no content and stays accepted, so a
	// producer that ends the file with a newline still decodes.
	if _, err := Decode(append(bytes.Clone(good), '\n')); err != nil {
		t.Errorf("trailing newline: Decode rejected content-free suffix: %v", err)
	}
}

func TestRequestValidation(t *testing.T) {
	r := mustRequest(t, "example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: "a.go"}})
	clone := func(f func(*Request)) Request {
		c := r
		c.Declarations = slices.Clone(r.Declarations)
		f(&c)
		return c
	}
	upper := strings.ToUpper(r.ID)
	check := func(name string, req Request) {
		t.Helper()
		if _, err := Encode([]Request{req}); err == nil {
			t.Errorf("%s: Encode accepted invalid request", name)
		}
	}
	check("bad owner", clone(func(c *Request) { c.Owner = "not an import path!" }))
	check("empty owner", clone(func(c *Request) { c.Owner = "" }))
	check("bad package", clone(func(c *Request) { c.Package = "has-dash" }))
	check("blank package", clone(func(c *Request) { c.Package = "_" }))
	check("keyword package", clone(func(c *Request) { c.Package = "func" }))
	check("package mismatch", clone(func(c *Request) { c.Package = "other" }))
	check("short id", clone(func(c *Request) { c.ID = "abcd" }))
	check("uppercase id", clone(func(c *Request) { c.ID = upper }))
	check("hash mismatch", clone(func(c *Request) { c.ID = strings.Repeat("0", 64) }))
	check("unparseable source", clone(func(c *Request) { c.Source = "package owner\nfunc (\n" }))
	check("wrong clause source", clone(func(c *Request) { c.Source = "package other\nfunc Value() int { return 0 }\n" }))
	check("null declarations", clone(func(c *Request) { c.Declarations = nil }))
	check("bad declaration owner", clone(func(c *Request) { c.Declarations = []Declaration{{Owner: "bad path!", File: "a.go"}} }))
	check("bad declaration file slash", clone(func(c *Request) { c.Declarations = []Declaration{{Owner: "example.com/mod/owner", File: "sub/a.go"}} }))
	check("bad declaration file colon", clone(func(c *Request) { c.Declarations = []Declaration{{Owner: "example.com/mod/owner", File: "a:b.go"}} }))
	check("bad declaration file empty", clone(func(c *Request) { c.Declarations = []Declaration{{Owner: "example.com/mod/owner", File: ""}} }))
	check("unsorted declarations", clone(func(c *Request) {
		c.Declarations = []Declaration{{Owner: "example.com/mod/b", File: "b.go"}, {Owner: "example.com/mod/a", File: "a.go"}}
	}))
	check("duplicate declarations", clone(func(c *Request) {
		c.Declarations = []Declaration{{Owner: "example.com/mod/a", File: "a.go"}, {Owner: "example.com/mod/a", File: "a.go"}}
	}))
}

func TestNewRequestAcceptsLogosAliases(t *testing.T) {
	// Every Logos variant is a valid basename at transport level;
	// plan membership decides selection later.
	for _, file := range []string{"feat.lgo", "feat.📜", "feat.⚡", "feat.ʕ◔ϖ◔ʔ"} {
		if _, err := NewRequest("example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: file}}); err != nil {
			t.Errorf("NewRequest rejected valid basename %q: %v", file, err)
		}
	}
	// A safe non-Go basename also passes transport; the coordinator
	// rejects it later as unselected.
	if _, err := NewRequest("example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: "feat.txt"}}); err != nil {
		t.Errorf("NewRequest rejected safe basename: %v", err)
	}
}

func TestNewRequestRejects(t *testing.T) {
	empty := []Declaration{}
	if _, err := NewRequest("example.com/mod/owner", "owner", "package other\n", empty); err == nil {
		t.Errorf("NewRequest accepted package clause mismatch")
	}
	if _, err := NewRequest("example.com/mod/owner", "not a pkg", goodSource, empty); err == nil {
		t.Errorf("NewRequest accepted bad package name")
	}
	if _, err := NewRequest("example.com/mod/owner", "_", goodSource, empty); err == nil {
		t.Errorf("NewRequest accepted blank package name")
	}
	if _, err := NewRequest("bad path!", "owner", goodSource, empty); err == nil {
		t.Errorf("NewRequest accepted bad owner")
	}
	if _, err := NewRequest("example.com/mod/owner", "owner", goodSource, nil); err == nil {
		t.Errorf("NewRequest accepted null declarations")
	}
	if _, err := NewRequest("example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "bad path!", File: "a.go"}}); err == nil {
		t.Errorf("NewRequest accepted bad declaration owner")
	}
	if _, err := NewRequest("example.com/mod/owner", "owner", goodSource, []Declaration{{Owner: "example.com/mod/owner", File: "../a.go"}}); err == nil {
		t.Errorf("NewRequest accepted non-basename declaration file")
	}
}
