// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"strings"
	"testing"
)

func TestBinaryMethodsGenerated(t *testing.T) {
	text := lowerStaticText(t, "package p\ntype Node struct {\n N int64 `json:\"n\"`\n Next *Node\n Name string\n Data []byte\n Values []uint16\n Lookup map[string]float32\n Maybe int?\n}\n")
	for _, want := range []string{
		"func (_logos_v Node) MarshalBinary() ([]byte, error)",
		"func (_logos_v *Node) UnmarshalBinary(src []byte) error",
		"func (_logos_v Node) AppendBinary(dst []byte) ([]byte, error)",
		"(1<<20)/size",
		"binary field %s at offset %d: truncated input",
		"bad presence byte",
		"wrong version %d",
		"$trailing at offset %d: trailing bytes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in generated text", want)
		}
	}
}

// Bare pointer options must not use the wrapper fields that scalar options
// need. Exercise both representations in one generated struct.
func TestBinaryOptionRepresentations(t *testing.T) {
	text := lowerStaticText(t, "package p\ntype S struct { P *string? `json:\"p\"`; N int? `json:\"n\"` }\n")
	start := strings.Index(text, "func _logos_bin_S_")
	if start < 0 {
		t.Fatal("missing binary helpers")
	}
	text = text[start:]
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "v *string)") && (strings.Contains(line, "v.Present") || strings.Contains(line, "v.Value")) {
			t.Errorf("pointer option uses wrapper accessor: %s", line)
		}
	}
	if !strings.Contains(text, "v.Present") || !strings.Contains(text, "v.Value") {
		t.Error("scalar option no longer uses its presence wrapper")
	}
	if !strings.Contains(text, "{ return _logos_bin_S_") {
		t.Error("bare pointer option does not delegate to its payload codec")
	}
}

func TestBinaryUnsupportedShapeSkipped(t *testing.T) {
	text := lowerStaticText(t, "package p\ntype Bad struct { C chan int `json:\"c\"` }\n")
	if strings.Contains(text, "func (_logos_v Bad) MarshalBinary()") {
		t.Fatalf("unsupported binary shape generated:\n%s", text)
	}
}

func TestBinaryAllUserMethodsSuppressHelpers(t *testing.T) {
	src := `package p
type S struct {
	A int64  ` + "`json:\"a\"`" + `
	B string ` + "`json:\"b\"`" + `
}
func (s S) MarshalBinary() ([]byte, error) { return nil, nil }
func (s S) AppendBinary(b []byte) ([]byte, error) { return b, nil }
func (s *S) UnmarshalBinary(data []byte) error { return nil }
`
	text := lowerStaticText(t, src)
	if strings.Contains(text, "_logos_bin_S") {
		t.Errorf("binary helpers generated when user declares all binary methods:\n%s", text)
	}
	if !strings.Contains(text, "func (_logos_v S) MarshalJSON() ([]byte, error)") {
		t.Errorf("expected MarshalJSON to be generated for S:\n%s", text)
	}
}

func TestBinaryPartialUserMethodsPreserveCodecs(t *testing.T) {
	src := `package p
type S struct {
	A int64 ` + "`json:\"a\"`" + `
}
func (s S) MarshalBinary() ([]byte, error) { return nil, nil }
`
	text := lowerStaticText(t, src)
	if !strings.Contains(text, "func (_logos_v S) AppendBinary") {
		t.Errorf("missing AppendBinary in partial binary declaration:\n%s", text)
	}
	if !strings.Contains(text, "func (_logos_v *S) UnmarshalBinary") {
		t.Errorf("missing UnmarshalBinary in partial binary declaration:\n%s", text)
	}
	if !strings.Contains(text, "_logos_bin_S_") {
		t.Errorf("missing binary helpers in partial binary declaration:\n%s", text)
	}
}

// TestBinaryZeroWireBudget covers finding (3) per SPEC Sec 12.3: nonzero-wire
// counts pre-allocate at most min(len(rest)/2, 1MB/elem) with no total-budget
// rejection so lawful >1MiB payloads with all bytes present decode; zero-wire
// counts (no lawful input bound) are capped at 1<<20 with count-out-of-range,
// encoder symmetric, constant-time without per-element iteration.
func TestBinaryZeroWireBudget(t *testing.T) {
	text := lowerStaticText(t, "package p\ntype Empty struct{}\ntype S struct {\n\tV []Empty `json:\"v\"`\n}\n")
	if !strings.Contains(text, "if n > (1 << 20)") {
		t.Fatalf("zero-wire path lacks 1<<20 count cap:\n%s", text)
	}
	if !strings.Contains(text, "binary count %d out of range") {
		t.Errorf("zero-wire encoder lacks symmetric count-out-of-range rejection:\n%s", text)
	}
	if strings.Contains(text, "storage budget exceeded") || strings.Contains(text, "_logos_budget") {
		t.Errorf("zero-wire path must not enforce a shared storage budget:\n%s", text)
	}
	if !strings.Contains(text, "*v = make(") {
		t.Errorf("zero-wire path must allocate directly without per-element iteration:\n%s", text)
	}
	big := lowerStaticText(t, "package p\ntype B struct {\n\tV []uint64 `json:\"v\"`\n}\n")
	if strings.Contains(big, "storage budget exceeded") || strings.Contains(big, "_logos_budget") {
		t.Errorf("nonzero-wire path must not reject on count header alone (charge nothing on total):\n%s", big)
	}
	if !strings.Contains(big, "cap64 := min(n, uint64(len(src)-*off)/2, (1<<20)/size)") {
		t.Errorf("nonzero-wire path must pre-allocate at most min(len(rest)/2, 1MB/elem):\n%s", big)
	}
	if !strings.Contains(big, "for i := uint64(0); i < n; i++ {") {
		t.Errorf("nonzero-wire path must grow by append as bytes arrive:\n%s", big)
	}
}
