// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package facts_test

import (
	"strconv"
	"strings"
	"testing"

	"cmd/compile/internal/logos/facts"
	"cmd/internal/goobj"
)

// TestConstFnEncodeDecodeRoundTrip checks the constfn framing: header
// line, source bytes, trailing newline, sorted placement beside comptime
// records, and a lossless Decode round trip.
func TestConstFnEncodeDecodeRoundTrip(t *testing.T) {
	fp := goobj.FingerprintType{0, 1, 2, 3, 4, 5, 6, 7}
	src := "package calc\n\nconst func F(x int) int {\n\treturn x\n}\n"
	in := &facts.Facts{
		Fingerprint: fp,
		Comptimes: []facts.ComptimeFact{
			{Object: "Gen", Source: []byte("func Gen() {}\n")},
		},
		ConstFuncs: []facts.ConstFuncFact{
			{Object: "F", Source: []byte(src)},
		},
	}
	got := facts.Encode(fp, in)
	// Records sort byte-wise: comptime sorts before constfn.
	want := "logos-facts 1\n" +
		"fingerprint 0001020304050607\n" +
		"comptime Gen 14\n" +
		"func Gen() {}\n" +
		"\n" +
		"constfn F " + itoa(len(src)) + "\n" +
		src + "\n"
	if string(got) != want {
		t.Fatalf("Encode mismatch:\n got %q\nwant %q", got, want)
	}
	back, err := facts.Decode(got)
	if err != nil {
		t.Fatalf("Decode(Encode) failed: %v", err)
	}
	if back.Fingerprint != fp {
		t.Fatalf("round trip lost the fingerprint: %+v", back.Fingerprint)
	}
	if len(back.ConstFuncs) != 1 {
		t.Fatalf("got %d constfn records, want 1", len(back.ConstFuncs))
	}
	cf := back.ConstFuncs[0]
	if cf.Object != "F" || string(cf.Source) != src {
		t.Fatalf("round trip mismatch: object %q source %q", cf.Object, cf.Source)
	}
	if len(back.Comptimes) != 1 || back.Comptimes[0].Object != "Gen" {
		t.Fatalf("round trip lost comptime records: %+v", back.Comptimes)
	}
}

// TestConstFnDecodeRejects checks the framing and duplicate validation
// of constfn records: malformed lengths, truncated bodies, missing
// trailing newlines, and duplicate objects all fail with the record's
// line number.
func TestConstFnDecodeRejects(t *testing.T) {
	head := "logos-facts 1\nfingerprint 0001020304050607\n"
	body := "package p\n\nconst func F() int {\n\treturn 1\n}\n"
	cases := []struct{ name, input, line string }{
		{"bad length", head + "constfn F xyz\n", "line 3"},
		{"negative length", head + "constfn F -1\n", "line 3"},
		{"zero length", head + "constfn F 0\n", "line 3"},
		{"missing length", head + "constfn F\n", "line 3"},
		{"truncated body", head + "constfn F " + itoa(len(body)) + "\n" + body[:10], "line 3"},
		{"short body", head + "constfn F 100\n" + body, "line 3"},
		{"missing newline after body", head + "constfn F " + itoa(len(body)) + "\n" + body, "line 3"},
		{"bad object", head + "constfn 9lives 4\nbody\n", "line 3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := facts.Decode([]byte(c.input))
			if err == nil {
				t.Fatalf("Decode(%q) succeeded, want error naming %s", c.input, c.line)
			}
			if !strings.Contains(err.Error(), c.line) {
				t.Fatalf("Decode(%q) error %q does not name %s", c.input, err, c.line)
			}
		})
	}
	t.Run("duplicate object", func(t *testing.T) {
		bodyA := "package p\n\nconst func F() int {\n\treturn 1\n}\n"
		bodyB := "package p\n\nconst func F() int {\n\treturn 11\n}\n"
		if len(bodyB) != len(bodyA)+1 {
			t.Fatalf("test bodies must differ by one byte: %d vs %d", len(bodyA), len(bodyB))
		}
		in := head +
			"constfn F " + itoa(len(bodyA)) + "\n" + bodyA + "\n" +
			"constfn F " + itoa(len(bodyB)) + "\n" + bodyB + "\n"
		_, err := facts.Decode([]byte(in))
		if err == nil {
			t.Fatalf("Decode(duplicate constfn F) succeeded, want error")
		}
		if !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("Decode(duplicate constfn F) error %q does not name the duplicate", err)
		}
	})
}

// TestConstFnDecodeLineNumbersAfterBody checks that a later record's
// error names its own line, counting the constfn body's newlines.
func TestConstFnDecodeLineNumbersAfterBody(t *testing.T) {
	body := "package p\n\nconst func F() int {\n\treturn 1\n}\n"
	// Head lines 1-2, constfn line 3, body lines 4-8, the framing
	// newline ends line 9, the malformed opt on line 10.
	in := head() + "constfn F " + itoa(len(body)) + "\n" + body + "\nopt H Q1\n"
	_, err := facts.Decode([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "line 10") {
		t.Fatalf("Decode error = %v, want error naming line 10", err)
	}
}

// TestConstFnVacuousRecordSkipped checks Encode skips vacuous constfn
// records, keeping output round-trippable.
func TestConstFnVacuousRecordSkipped(t *testing.T) {
	fp := goobj.FingerprintType{1}
	in := &facts.Facts{
		Fingerprint: fp,
		ConstFuncs:  []facts.ConstFuncFact{{Object: "F"}, {Source: []byte("x")}},
	}
	got := facts.Encode(fp, in)
	if string(got) != "logos-facts 1\nfingerprint 0100000000000000\n" {
		t.Fatalf("vacuous constfn records not skipped: %q", got)
	}
	if _, err := facts.Decode(got); err != nil {
		t.Fatalf("Decode(skipped) failed: %v", err)
	}
}

// TestConstFnMixedRecordsRoundTrip checks one member carrying constfn,
// comptime, opt, and fn records sorts and decodes whole.
func TestConstFnMixedRecordsRoundTrip(t *testing.T) {
	fp := goobj.FingerprintType{9, 8, 7, 6, 5, 4, 3, 2}
	src := "package p\n\nconst func F() int {\n\treturn 0\n}\n"
	in := &facts.Facts{
		Fingerprint: fp,
		Opts:        []facts.PathFact{{Object: "H", Path: "P0"}},
		Fns:         []facts.FnFact{{Object: "F", ReturnsNonNil: true}},
		Comptimes:   []facts.ComptimeFact{{Object: "G", Source: []byte("func G() {}\n")}},
		ConstFuncs:  []facts.ConstFuncFact{{Object: "F", Source: []byte(src)}},
	}
	back, err := facts.Decode(facts.Encode(fp, in))
	if err != nil {
		t.Fatalf("Decode(Encode) failed: %v", err)
	}
	if len(back.ConstFuncs) != 1 || len(back.Comptimes) != 1 || len(back.Opts) != 1 || len(back.Fns) != 1 {
		t.Fatalf("round trip lost records: %+v", back)
	}
	if back.ConstFuncs[0].Object != "F" || string(back.ConstFuncs[0].Source) != src {
		t.Fatalf("round trip changed the constfn body: %+v", back.ConstFuncs[0])
	}
	out := string(facts.Encode(back.Fingerprint, back))
	if !strings.Contains(out, "constfn F "+itoa(len(src))+"\n") {
		t.Fatalf("re-Encode not canonical: %q", out)
	}
}

func head() string {
	return "logos-facts 1\nfingerprint 0001020304050607\n"
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
