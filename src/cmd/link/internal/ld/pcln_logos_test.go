// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import "testing"

func TestLogosFuncDisplayName(t *testing.T) {
	checks := []struct {
		label string
		input string
		want  string
	}{
		// A decimal clone number is removed only when it is the final suffix.
		{label: "specializationOne", input: "main.Copy·logosspec1", want: "main.Copy"},
		{label: "specializationLongNumber", input: "main.Copy·logosspec4096", want: "main.Copy"},
		{label: "specializationZero", input: "main.Copy·logosspec0", want: "main.Copy"},
		{label: "comptimeOne", input: "main.Autowire·logosct7", want: "main.Autowire"},
		{label: "comptimePackagePath", input: "example.com/p.Autowire·logosct123", want: "example.com/p.Autowire"},

		// Name syntax preceding the suffix is retained byte-for-byte.
		{label: "pointerReceiver", input: "example.com/p.(*T).Method·logosspec2", want: "example.com/p.(*T).Method"},
		{label: "valueReceiver", input: "example.com/p.T.Method·logosct12", want: "example.com/p.T.Method"},
		{label: "genericSpecialization", input: "example.com/p.Foo[go.shape.int]·logosspec1", want: "example.com/p.Foo[go.shape.int]"},
		{label: "genericComptime", input: "example.com/p.Foo[go.shape.string]·logosct10", want: "example.com/p.Foo[go.shape.string]"},

		// Comptime copies keep the source-level ASCII spelling
		// <f>_logosct<N> (U+00B7 never survives spliced source text);
		// the linker strips it exactly like the middle-dot form.
		{label: "comptimeASCIISuffix", input: "example.com/p.Foo_logosct1", want: "example.com/p.Foo"},
		{label: "comptimeASCIIPackagePath", input: "example.com/p.Autowire_logosct123", want: "example.com/p.Autowire"},
		{label: "comptimeASCIIReceiver", input: "example.com/p.T.Method_logosct12", want: "example.com/p.T.Method"},
		{label: "comptimeASCIIGeneric", input: "example.com/p.Foo[go.shape.string]_logosct10", want: "example.com/p.Foo[go.shape.string]"},
		{label: "comptimeASCIINumberMissing", input: "example.com/p.Foo_logosct", want: "example.com/p.Foo_logosct"},
		{label: "comptimeASCIINoBase", input: "_logosct1", want: "_logosct1"},
		{label: "comptimeASCIILetterNumber", input: "example.com/p.Foo_logosctX", want: "example.com/p.Foo_logosctX"},
		{label: "comptimeASCIILetterAfterNumber", input: "example.com/p.Foo_logosct1x", want: "example.com/p.Foo_logosct1x"},
		{label: "comptimeASCIIDigitsAfterOtherText", input: "example.com/p.Foo_logosct2extra3", want: "example.com/p.Foo_logosct2extra3"},

		// Names without a complete middle-dot suffix are unchanged.
		{label: "ordinaryFunction", input: "main.main", want: "main.main"},
		{label: "ordinaryMethodName", input: "main.Copy", want: "main.Copy"},
		{label: "asciiPeriod", input: "example.com/p.Foo.logosspec1", want: "example.com/p.Foo.logosspec1"},
		{label: "numberMissing", input: "example.com/p.Foo·logosspec", want: "example.com/p.Foo·logosspec"},
		{label: "comptimeNumberMissing", input: "example.com/p.Foo·logosct", want: "example.com/p.Foo·logosct"},
		{label: "letterNumber", input: "example.com/p.Foo·logosspecX", want: "example.com/p.Foo·logosspecX"},
		{label: "hexLikeNumber", input: "example.com/p.Foo·logosspec0x1", want: "example.com/p.Foo·logosspec0x1"},
		{label: "signedNumber", input: "example.com/p.Foo·logosspec-1", want: "example.com/p.Foo·logosspec-1"},

		// The suffix must have a non-empty base and must be terminal.
		{label: "noBaseSpecialization", input: "·logosspec1", want: "·logosspec1"},
		{label: "noBaseComptime", input: "·logosct1", want: "·logosct1"},
		{label: "letterAfterNumber", input: "example.com/p.Foo·logosspec1x", want: "example.com/p.Foo·logosspec1x"},
		{label: "underscoreAfterNumber", input: "example.com/p.Foo·logosspec1_extra", want: "example.com/p.Foo·logosspec1_extra"},
		{label: "middleDotTextAfterNumber", input: "example.com/p.Foo·logosspec1·extra", want: "example.com/p.Foo·logosspec1·extra"},
		{label: "digitsAfterOtherText", input: "example.com/p.Foo·logosct2extra3", want: "example.com/p.Foo·logosct2extra3"},
		{label: "dotAfterNumber", input: "example.com/p.Foo·logosspec1.2", want: "example.com/p.Foo·logosspec1.2"},

		// Display names may have more than one compiler-generated suffix.
		{label: "specializationOfComptime", input: "example.com/p.Bar·logosct1·logosspec2", want: "example.com/p.Bar"},
		{label: "comptimeOfSpecialization", input: "example.com/p.Bar·logosspec12·logosct3", want: "example.com/p.Bar"},
		{label: "threeGeneratedSuffixes", input: "example.com/p.Bar·logosct1·logosspec2·logosct3", want: "example.com/p.Bar"},
		{label: "emptyBaseChain", input: "·logosspec1·logosct2", want: "·logosspec1·logosct2"},

		// Source-level comptime copies chain with specializations both ways.
		{label: "specializationOfASCIIComptime", input: "example.com/p.Bar_logosct1·logosspec2", want: "example.com/p.Bar"},
		{label: "asciiComptimeOfSpecialization", input: "example.com/p.Bar·logosspec12_logosct3", want: "example.com/p.Bar"},
		{label: "threeSuffixesASCIIFirst", input: "example.com/p.Bar_logosct1·logosspec2·logosct3", want: "example.com/p.Bar"},
		{label: "emptyBaseASCIIChain", input: "·logosspec1_logosct2", want: "·logosspec1_logosct2"},
	}

	for _, tc := range checks {
		t.Run(tc.label, func(t *testing.T) {
			if got := logosFuncDisplayName(tc.input); got != tc.want {
				t.Fatalf("logosFuncDisplayName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestLogosCloneBaseMatchesDisplay pins the one-concern rule: the
// capability base projection and the pclntab display strip are the same
// function over the same table, so sink identity and trace display agree
// for every clone spelling (SPEC T-203.4, T-205, red-team #9, #74).
func TestLogosCloneBaseMatchesDisplay(t *testing.T) {
	for _, name := range []string{
		"main.main",
		"main.Copy·logosspec1",
		"main.Autowire·logosct7",
		"example.com/p.Foo_logosct1",
		"example.com/p.Bar_logosct1·logosspec2",
		"example.com/p.Bar·logosspec12_logosct3",
		"runtime.cgocall.abi0",
	} {
		if got, want := logosCloneBaseName(name), logosFuncDisplayName(name); got != want {
			t.Errorf("logosCloneBaseName(%q) = %q, display = %q, want identical", name, got, want)
		}
	}
}
