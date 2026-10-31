// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import (
	stdjson "encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type single[T any] struct{ A T }

type quoted[T any] struct {
	A T `json:",string"`
}

type (
	myBool    bool
	myInt8    int8
	myInt     int
	myUint8   uint8
	myUintptr uintptr
	myFloat32 float32
	myFloat64 float64
	myString  string
)

func decodeSingle[T any](data string, read func(*Scanner, *T)) (single[T], error) {
	return decodeSingleBytes([]byte(data), read)
}

func decodeSingleBytes[T any](data []byte, read func(*Scanner, *T)) (single[T], error) {
	var out single[T]
	s := NewScanner(data, (*single[T])(nil))
	if !s.Object() {
		return out, s.End()
	}
	for s.Next() {
		if s.Key() == "A" {
			read(&s, &out.A)
		} else {
			s.Skip()
		}
	}
	return out, s.End()
}

func sameError(got, want error) bool {
	if (got == nil) != (want == nil) {
		return false
	}
	if got == nil {
		return true
	}
	switch w := want.(type) {
	case *stdjson.UnmarshalTypeError:
		g, ok := got.(*stdjson.UnmarshalTypeError)
		return ok && g.Value == w.Value && g.Type == w.Type && g.Offset == w.Offset && g.Struct == w.Struct && g.Field == w.Field
	case *stdjson.SyntaxError:
		g, ok := got.(*stdjson.SyntaxError)
		return ok && g.Error() == w.Error() && g.Offset == w.Offset
	}
	return got.Error() == want.Error()
}

func dump(v any) string {
	return fmt.Sprintf("%#v", v)
}

func checkSingle[T any](t *testing.T, inputs []string, read func(*Scanner, *T)) {
	t.Helper()
	for _, in := range inputs {
		for _, data := range []string{`{"A":` + in + `}`, `{ "A" : ` + in + ` , "B" : 1 }`, " \t\r\n{\"B\":[],\"A\":" + in + "}\n "} {
			got, gotErr := decodeSingle(data, read)
			var want single[T]
			wantErr := stdjson.Unmarshal([]byte(data), &want)
			if !sameError(gotErr, wantErr) {
				t.Errorf("%T %q: error = %#v (%v), want %#v (%v)", want.A, data, gotErr, gotErr, wantErr, wantErr)
				continue
			}
			if _, syntax := wantErr.(*stdjson.SyntaxError); syntax {
				continue
			}
			if dump(got) != dump(want) {
				t.Errorf("%T %q: value = %s, want %s", want.A, data, dump(got), dump(want))
			}
		}
	}
}

var intInputs = []string{
	`0`, `-0`, `5`, `-5`, `127`, `128`, `-128`, `-129`, `255`, `256`, `300`, `32767`, `32768`, `-32768`, `-32769`,
	`65535`, `65536`, `2147483647`, `2147483648`, `-2147483648`, `-2147483649`, `4294967295`, `4294967296`,
	`9223372036854775807`, `9223372036854775808`, `-9223372036854775808`, `-9223372036854775809`,
	`18446744073709551615`, `18446744073709551616`, `99999999999999999999999`, `1.5`, `-1.5`, `1e2`, `1E2`,
	`1e+2`, `1.0`, `10e-1`, `-1.5e-3`, `0.0`, `null`, `"x"`, `""`, `"5"`, `true`, `false`, `[]`, `[1]`, `[1,[2]]`,
	`{}`, `{"b":1}`, `{"b":{"c":[1]}}`,
}

var floatInputs = []string{
	`0`, `-0`, `0.0`, `1`, `-1`, `1.5`, `-1.5`, `0.1`, `1e2`, `1E2`, `1e+2`, `1e-2`, `1e999`, `-1e999`, `1e-999`,
	`3.4028235e38`, `3.4028236e38`, `3.5e38`, `1e39`, `-3.5e38`, `1.7976931348623157e308`, `1.7976931348623159e308`,
	`1.8e308`, `5e-324`, `1e-400`, `1.4e-45`, `1e-46`, `123456789012345678901234567890`, `0.000001`, `100000000000000000000000`,
	`null`, `"x"`, `"1.5"`, `true`, `[]`, `[1.5]`, `{}`, `{"b":2}`,
}

var boolInputs = []string{
	`true`, `false`, `null`, `0`, `1`, `-1.5`, `"true"`, `"x"`, `""`, `[]`, `[true]`, `{}`, `{"b":true}`,
}

var stringInputs = []string{
	`""`, `"abc"`, `"a b"`, `"\n\t\r\b\f\"\\\/"`, `"\u0041"`, `"\u00e9"`, `"\u00E9"`, `"\u2028"`, `"\u0000"`, `"\ud83d\ude00"`,
	`"\ud800"`, `"\udc00"`, `"\ud800\u0041"`, `"\ud800\ud800"`, `"\udc00\ud800"`, `"\ud83d"`, `"x\ud83d"`, `"\ud83d\n"`,
	`"日本語"`, `"é"`, `"😀"`, `"a` + "\xff" + `b"`, `"` + "\xc3" + `"`, `"` + "\xe2\x82" + `"`, `"` + "\xf0\x9f\x98" + `"`,
	`"` + "\xed\xa0\x80" + `"`, `"` + "\xc0\x80" + `"`, `"` + "\xff\xfe" + `\u0041"`, `"` + strings.Repeat("abcdefgh", 100) + `"`,
	`"` + strings.Repeat("é\n", 300) + `"`, `null`, `5`, `-1.5`, `true`, `false`, `[]`, `["a"]`, `{}`, `{"b":"c"}`,
}

func TestBoolReader(t *testing.T) {
	checkSingle(t, boolInputs, Bool[bool])
	checkSingle(t, boolInputs, Bool[myBool])
}

func TestIntReader(t *testing.T) {
	checkSingle(t, intInputs, Int[int])
	checkSingle(t, intInputs, Int[int8])
	checkSingle(t, intInputs, Int[int16])
	checkSingle(t, intInputs, Int[int32])
	checkSingle(t, intInputs, Int[int64])
	checkSingle(t, intInputs, Int[myInt8])
	checkSingle(t, intInputs, Int[myInt])
}

func TestUintReader(t *testing.T) {
	checkSingle(t, intInputs, Uint[uint])
	checkSingle(t, intInputs, Uint[uint8])
	checkSingle(t, intInputs, Uint[uint16])
	checkSingle(t, intInputs, Uint[uint32])
	checkSingle(t, intInputs, Uint[uint64])
	checkSingle(t, intInputs, Uint[uintptr])
	checkSingle(t, intInputs, Uint[myUint8])
	checkSingle(t, intInputs, Uint[myUintptr])
}

func TestFloatReader(t *testing.T) {
	checkSingle(t, floatInputs, Float[float64])
	checkSingle(t, floatInputs, Float[float32])
	checkSingle(t, floatInputs, Float[myFloat32])
	checkSingle(t, floatInputs, Float[myFloat64])
	checkSingle(t, intInputs, Float[float64])
	checkSingle(t, intInputs, Float[float32])
}

func TestStringReader(t *testing.T) {
	checkSingle(t, stringInputs, String[string])
	checkSingle(t, stringInputs, String[myString])
}

func TestNullLeavesValueUnchanged(t *testing.T) {
	s := NewScanner([]byte(`{"A":null,"B":null,"C":null,"D":null,"E":null}`), (*struct{})(nil))
	a, b, c, d, e := 7, uint8(8), 1.5, "keep", true
	if !s.Object() {
		t.Fatal("Object() = false")
	}
	for s.Next() {
		switch s.Key() {
		case "A":
			Int(&s, &a)
		case "B":
			Uint(&s, &b)
		case "C":
			Float(&s, &c)
		case "D":
			String(&s, &d)
		case "E":
			Bool(&s, &e)
		}
	}
	if err := s.End(); err != nil {
		t.Fatal(err)
	}
	if a != 7 || b != 8 || c != 1.5 || d != "keep" || !e {
		t.Fatalf("null changed values: %v %v %v %v %v", a, b, c, d, e)
	}
}

func TestMismatchContinuesDecoding(t *testing.T) {
	type pair struct {
		A int
		B string
		C bool
	}
	var p pair
	s := NewScanner([]byte(`{"A":"nope","B":"ok","C":[1],"A":3}`), (*pair)(nil))
	if !s.Object() {
		t.Fatal("Object() = false")
	}
	for s.Next() {
		switch s.Key() {
		case "A":
			Int(&s, &p.A)
		case "B":
			String(&s, &p.B)
		case "C":
			Bool(&s, &p.C)
		}
	}
	if s.Err() != nil {
		t.Fatalf("Err() = %v", s.Err())
	}
	if p.A != 3 || p.B != "ok" || p.C {
		t.Fatalf("decoded %+v", p)
	}
	ute, ok := s.Saved().(*stdjson.UnmarshalTypeError)
	if !ok {
		t.Fatalf("Saved() = %#v", s.Saved())
	}
	if ute.Value != "string" || ute.Type != reflect.TypeFor[int]() || ute.Offset != 11 || ute.Struct != "pair" || ute.Field != "A" {
		t.Fatalf("first saved error = %#v", ute)
	}
}

type quotedCase struct {
	in    string
	want  string
	saved string
	fatal string
}

func decodeQuoted[T any](data string, read func(*Scanner, *T)) (quoted[T], *Scanner) {
	var out quoted[T]
	s := NewScanner([]byte(data), (*quoted[T])(nil))
	if !s.Object() {
		return out, &s
	}
	for s.Next() {
		if s.Key() == "A" {
			read(&s, &out.A)
		} else {
			s.Skip()
		}
	}
	return out, &s
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func quotedStateProblem(c quotedCase, s *Scanner) string {
	switch {
	case c.fatal != "":
		return diffText("Err()", errorText(s.Err()), c.fatal)
	case s.Err() != nil:
		return "unexpected fatal " + errorText(s.Err())
	}
	return diffText("Saved()", errorText(s.Saved()), c.saved)
}

func diffText(what, got, want string) string {
	if got == want {
		return ""
	}
	return fmt.Sprintf("%s = %q, want %q", what, got, want)
}

func quotedStdProblem[T any](c quotedCase, data string, s *Scanner) string {
	var want quoted[T]
	wantErr := stdjson.Unmarshal([]byte(data), &want)
	if c.fatal != "" {
		return diffText("encoding/json error", errorText(wantErr), c.fatal)
	}
	if dump(want.A) != c.want {
		return fmt.Sprintf("encoding/json value = %s, expectation %s", dump(want.A), c.want)
	}
	if !sameError(s.End(), wantErr) {
		return fmt.Sprintf("End() = %v, encoding/json = %v", s.End(), wantErr)
	}
	return ""
}

func quotedLiveProblem[T any](data string, s *Scanner, got quoted[T]) string {
	var want quoted[T]
	wantErr := stdjson.Unmarshal([]byte(data), &want)
	if dump(got.A) != dump(want.A) {
		return fmt.Sprintf("value = %s, encoding/json = %s", dump(got.A), dump(want.A))
	}
	if !sameError(s.End(), wantErr) {
		return fmt.Sprintf("End() = %v, encoding/json = %v", s.End(), wantErr)
	}
	return ""
}

func checkQuoted[T any](t *testing.T, cases []quotedCase, read func(*Scanner, *T)) {
	t.Helper()
	for _, c := range cases {
		data := `{"A":` + c.in + `}`
		got, s := decodeQuoted(data, read)
		if stdIsV2 {
			if msg := quotedLiveProblem(data, s, got); msg != "" {
				t.Errorf("%T %s: %s", got.A, data, msg)
			}
			continue
		}
		if dump(got.A) != c.want {
			t.Errorf("%T %s: value = %s, want %s", got.A, data, dump(got.A), c.want)
		}
		if msg := quotedStateProblem(c, s); msg != "" {
			t.Errorf("%T %s: %s", got.A, data, msg)
		}
		if msg := quotedStdProblem[T](c, data, s); msg != "" {
			t.Errorf("%T %s: %s", got.A, data, msg)
		}
	}
}

func tagMsg(item, typ string) string {
	return fmt.Sprintf("json: invalid use of ,string struct tag, trying to unmarshal %q into %s", item, typ)
}

func unquotedMsg(typ string) string {
	return "json: invalid use of ,string struct tag, trying to unmarshal unquoted value into " + typ
}

func TestIntQ(t *testing.T) {
	cases := []quotedCase{
		{in: `"5"`, want: `5`},
		{in: `"-5"`, want: `-5`},
		{in: `"0"`, want: `0`},
		{in: `null`, want: `0`},
		{in: `"null"`, want: `0`},
		{in: `5`, want: `0`, saved: unquotedMsg("int")},
		{in: `true`, want: `0`, saved: unquotedMsg("int")},
		{in: `[1]`, want: `0`, saved: unquotedMsg("int")},
		{in: `{"x":1}`, want: `0`, saved: unquotedMsg("int")},
		{in: `""`, want: `0`, saved: tagMsg("", "int")},
		{in: `"nul"`, want: `0`, saved: tagMsg("nul", "int")},
		{in: `"true"`, want: `0`, saved: tagMsg("true", "int")},
		{in: `"false"`, want: `0`, saved: tagMsg("false", "int")},
		{in: `"x"`, want: `0`, fatal: tagMsg("x", "int")},
		{in: `" 5"`, want: `0`, fatal: tagMsg(" 5", "int")},
		{in: `"+5"`, want: `0`, fatal: tagMsg("+5", "int")},
		{in: `"1.5"`, want: `0`, saved: "json: cannot unmarshal number 1.5 into Go struct field quoted[int].A of type int"},
		{in: `"1e2"`, want: `0`, saved: "json: cannot unmarshal number 1e2 into Go struct field quoted[int].A of type int"},
		{in: `"99999999999999999999"`, want: `0`, saved: "json: cannot unmarshal number 99999999999999999999 into Go struct field quoted[int].A of type int"},
		{in: `"\"5\""`, want: `0`, saved: "json: cannot unmarshal string into Go struct field quoted[int].A of type int"},
	}
	checkQuoted(t, cases, IntQ[int])
}

func TestIntQNarrow(t *testing.T) {
	cases := []quotedCase{
		{in: `"127"`, want: `127`},
		{in: `"-128"`, want: `-128`},
		{in: `"128"`, want: `0`, saved: "json: cannot unmarshal number 128 into Go struct field quoted[int8].A of type int8"},
		{in: `"-129"`, want: `0`, saved: "json: cannot unmarshal number -129 into Go struct field quoted[int8].A of type int8"},
	}
	checkQuoted(t, cases, IntQ[int8])
}

func TestUintQ(t *testing.T) {
	cases := []quotedCase{
		{in: `"255"`, want: `0xff`},
		{in: `"0"`, want: `0x0`},
		{in: `"256"`, want: `0x0`, saved: "json: cannot unmarshal number 256 into Go struct field quoted[uint8].A of type uint8"},
		{in: `"-1"`, want: `0x0`, saved: "json: cannot unmarshal number -1 into Go struct field quoted[uint8].A of type uint8"},
		{in: `"1.5"`, want: `0x0`, saved: "json: cannot unmarshal number 1.5 into Go struct field quoted[uint8].A of type uint8"},
		{in: `"x"`, want: `0x0`, fatal: tagMsg("x", "uint8")},
		{in: `null`, want: `0x0`},
		{in: `7`, want: `0x0`, saved: unquotedMsg("uint8")},
	}
	checkQuoted(t, cases, UintQ[uint8])
}

func TestFloatQ(t *testing.T) {
	cases := []quotedCase{
		{in: `"1.5"`, want: `1.5`},
		{in: `"-1.5e3"`, want: `-1500`},
		{in: `"0"`, want: `0`},
		{in: `"1e999"`, want: `0`, saved: "json: cannot unmarshal number 1e999 into Go struct field quoted[float64].A of type float64"},
		{in: `""`, want: `0`, saved: tagMsg("", "float64")},
		{in: `"x"`, want: `0`, fatal: tagMsg("x", "float64")},
		{in: `"null"`, want: `0`},
		{in: `1.5`, want: `0`, saved: unquotedMsg("float64")},
	}
	checkQuoted(t, cases, FloatQ[float64])
}

func TestFloatQ32(t *testing.T) {
	cases := []quotedCase{
		{in: `"1.5"`, want: `1.5`},
		{in: `"3.5e38"`, want: `0`, saved: "json: cannot unmarshal number 3.5e38 into Go struct field quoted[float32].A of type float32"},
	}
	checkQuoted(t, cases, FloatQ[float32])
}

func TestBoolQ(t *testing.T) {
	cases := []quotedCase{
		{in: `"true"`, want: `true`},
		{in: `"false"`, want: `false`},
		{in: `null`, want: `false`},
		{in: `"null"`, want: `false`},
		{in: `""`, want: `false`, saved: tagMsg("", "bool")},
		{in: `"tru"`, want: `false`, saved: tagMsg("tru", "bool")},
		{in: `"nul"`, want: `false`, saved: tagMsg("nul", "bool")},
		{in: `"truex"`, want: `false`, saved: tagMsg("truex", "bool")},
		{in: `true`, want: `false`, saved: unquotedMsg("bool")},
		{in: `1`, want: `false`, saved: unquotedMsg("bool")},
		{in: `"1"`, want: `false`, fatal: tagMsg("1", "bool")},
		{in: `"x"`, want: `false`, fatal: tagMsg("x", "bool")},
		{in: `"\"true\""`, want: `false`, saved: "json: cannot unmarshal string into Go struct field quoted[bool].A of type bool"},
	}
	checkQuoted(t, cases, BoolQ[bool])
}

func TestStringQ(t *testing.T) {
	cases := []quotedCase{
		{in: `"\"abc\""`, want: `"abc"`},
		{in: `"\"\""`, want: `""`},
		{in: `"\"a\\nb\""`, want: `"a\nb"`},
		{in: `"\"\\u00e9\""`, want: `"é"`},
		{in: `"\"a\\\"b\""`, want: `"a\"b"`},
		{in: `"\"it\\'s\""`, want: `"it's"`},
		{in: `null`, want: `""`},
		{in: `"null"`, want: `""`},
		{in: `"abc"`, want: `""`, fatal: tagMsg("abc", "string")},
		{in: `""`, want: `""`, saved: tagMsg("", "string")},
		{in: `"nul"`, want: `""`, saved: tagMsg("nul", "string")},
		{in: `"true"`, want: `""`, saved: tagMsg("true", "string")},
		{in: `"\"abc"`, want: `""`, fatal: tagMsg(`"abc`, "string")},
		{in: `"\"a\"b\""`, want: `""`, fatal: tagMsg(`"a"b"`, "string")},
		{in: `"5"`, want: `""`, fatal: tagMsg("5", "string")},
		{in: `5`, want: `""`, saved: unquotedMsg("string")},
		{in: `["a"]`, want: `""`, saved: unquotedMsg("string")},
	}
	checkQuoted(t, cases, StringQ[string])
}

func TestStringQEncodingBoundary(t *testing.T) {
	prefix := `{"A":`
	complete := [][]byte{
		{'"', '\\', '"', 'a', 0xff, 'b', '\\', '"', '"'},
		{'"', '\\', '"', 'a', 0xc3, '\\', '"', '"'},
		{'"', '\\', '"', 'a', 0xc3, '\\', '\\', 'n', '\\', '"', '"'},
		{'"', '\\', '"', '\\', '\\', 'u', 'd', '8', '0', '0', '\\', '"', '"'},
	}
	for _, outer := range complete {
		data := prefix + string(outer) + `}`
		got, s := decodeQuoted(data, StringQ[string])
		if msg := quotedLiveProblem(data, s, got); msg != "" {
			t.Errorf("StringQ %q: %s", data, msg)
		}
	}
	truncated := [][]byte{
		{'"', 'a', 0xc3},
		{'"', '\\', '"', 'a', 'b'},
	}
	for _, outer := range truncated {
		data := prefix + string(outer)
		got, s := decodeQuoted(data, StringQ[string])
		if msg := quotedLiveProblem(data, s, got); msg != "" {
			t.Errorf("StringQ trunc %q: %s", data, msg)
		}
	}
}

func TestQuotedParityOnSuccess(t *testing.T) {
	type qs struct {
		A int     `json:",string"`
		B bool    `json:",string"`
		C float64 `json:",string"`
		D uint8   `json:",string"`
		E string  `json:",string"`
	}
	in := `{"A":"-12","B":"true","C":"2.5e1","D":"200","E":"\"h\\u00e9llo\\n\""}`
	var want qs
	if err := stdjson.Unmarshal([]byte(in), &want); err != nil {
		t.Fatal(err)
	}
	var got qs
	s := NewScanner([]byte(in), (*qs)(nil))
	if !s.Object() {
		t.Fatal("Object() = false")
	}
	for s.Next() {
		switch s.Key() {
		case "A":
			IntQ(&s, &got.A)
		case "B":
			BoolQ(&s, &got.B)
		case "C":
			FloatQ(&s, &got.C)
		case "D":
			UintQ(&s, &got.D)
		case "E":
			StringQ(&s, &got.E)
		}
	}
	if err := s.End(); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReaderSyntaxErrorsAreFatal(t *testing.T) {
	for _, in := range []string{`tru`, `nul`, `"abc`, `-`, `1.`, `1e`, `01`, `"\x"`, "\"a\x01\"", `[1,]`, `{"a":}`, `truee`, `1 2`} {
		data := `{"A":` + in + `}`
		_, gotErr := decodeSingle(data, Int[int])
		var raw stdjson.RawMessage
		wantErr := stdjson.Unmarshal([]byte(data), &raw)
		if wantErr == nil {
			t.Fatalf("%q unexpectedly valid", data)
		}
		if !sameError(gotErr, wantErr) {
			t.Errorf("%q: error = %#v, want %#v", data, gotErr, wantErr)
		}
	}
}
