// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package check

import "testing"

// Reference: tools/checkconst/checkconst.go. The constRegexp grammar is
// "// +checkconst <pkg> <obj>" with +checksize/+checkoffset/+checkalign
// aliases sharing buildExpected, so the fixture is a source-only producer
// package exercising real typed fact export through the production
// checkPackage driver. Expected values below are independent literals
// written in this test (42/43/7/8), never computed with the checker: 8 is
// the language-spec size of int64, not a checker derivation.
const parityConstProducerFixture = `package producer

const Answer = 42

const Other = 7

var Marker int64
`

func TestCheckconstParity(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		i := nogoParityImporter(t, "checkconst")
		nogoParityWrite(t, i, "producer", parityConstProducerFixture)
		const consumer = `package consumer

import "producer"

const Check = 43 // +checkconst producer Answer

var _ = producer.Answer
var _ = Check
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one checkconst diagnostic", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, consumer, "const Check")
		wantMsg := `got value "43", wanted "42"`
		if got.Category != "checkconst" || got.Position.Line != wantLine || got.Message != wantMsg {
			t.Fatalf("finding = %+v, want category checkconst line %d message %q", got, wantLine, wantMsg)
		}
	})
	t.Run("size_mismatch", func(t *testing.T) {
		i := nogoParityImporter(t, "checkconst")
		nogoParityWrite(t, i, "producer", parityConstProducerFixture)
		const consumer = `package consumer

import "producer"

const SizeCheck = 7 // +checksize producer Marker

var _ = producer.Marker
var _ = SizeCheck
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one checkconst diagnostic", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, consumer, "const SizeCheck")
		wantMsg := `got value "7", wanted "8"`
		if got.Category != "checkconst" || got.Position.Line != wantLine || got.Message != wantMsg {
			t.Fatalf("finding = %+v, want category checkconst line %d message %q", got, wantLine, wantMsg)
		}
	})
	t.Run("match", func(t *testing.T) {
		i := nogoParityImporter(t, "checkconst")
		nogoParityWrite(t, i, "producer", parityConstProducerFixture)
		const consumer = `package consumer

import "producer"

const Check = 42 // +checkconst producer Answer

var _ = producer.Answer
var _ = Check
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics for a matching constant", findings)
		}
	})
	t.Run("unannotated", func(t *testing.T) {
		i := nogoParityImporter(t, "checkconst")
		nogoParityWrite(t, i, "producer", parityConstProducerFixture)
		const consumer = `package consumer

import "producer"

const Plain = 43

var _ = producer.Answer
var _ = Plain
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics without an annotation", findings)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		i := nogoParityImporter(t, "checkconst")
		nogoParityWrite(t, i, "producer", parityConstProducerFixture)
		const consumer = `package consumer

import "producer"

const Check = 42 // +checkconst bogus

var _ = producer.Answer
var _ = Check
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one malformed-directive diagnostic", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, consumer, "const Check")
		wantMsg := "potentially malformed checkconst directives"
		if got.Category != "checkconst" || got.Position.Line != wantLine || got.Message != wantMsg {
			t.Fatalf("finding = %+v, want category checkconst line %d message %q", got, wantLine, wantMsg)
		}
	})
	t.Run("conflicting", func(t *testing.T) {
		i := nogoParityImporter(t, "checkconst")
		nogoParityWrite(t, i, "producer", parityConstProducerFixture)
		const consumer = `package consumer

import "producer"

// +checkconst producer Answer
// +checkconst producer Other
const Check = 42

var _ = producer.Answer
var _ = Check
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one conflicting-values diagnostic", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, consumer, "producer Other")
		wantMsg := "multiple conflicting values"
		if got.Category != "checkconst" || got.Position.Line != wantLine || got.Message != wantMsg {
			t.Fatalf("finding = %+v, want category checkconst line %d message %q", got, wantLine, wantMsg)
		}
	})
}
