// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"strings"
	"testing"
)

const structtagLgoSrc = "package p\n\n" +
	"type BadTags struct {\n" +
	"\tName string `json:name,omitempty` // want\n" +
	"\tAge int32 `json:\"age\" json:\"years\"` // want\n" +
	"\tsecret string `json:\"secret\"` // want\n" +
	"}\n\n" +
	"type CleanTags struct {\n" +
	"\tName string `json:\"name,omitempty\" xml:\"name\"`\n" +
	"\tignored string `json:\"-\"`\n" +
	"}\n"

const structtagGoSrc = "package p\n\n" +
	"type BadTags struct {\n" +
	"\tName string `json:name,omitempty` // want\n" +
	"\tAge int32 `json:\"age\" json:\"years\"` // want\n" +
	"\tsecret string `json:\"secret\"` // want\n" +
	"}\n\n" +
	"type CleanTags struct {\n" +
	"\tName string `json:\"name,omitempty\" xml:\"name\"`\n" +
	"\tignored string `json:\"-\"`\n" +
	"}\n"

func TestStructTagMalformed(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", structtagLgoSrc}, finding{"correctness::structtag_malformed", Forbid})
	expect(t, KindGo, nil, testSource{"x.go", structtagGoSrc}, finding{"correctness::structtag_malformed", Deny})
}

func TestStructTagMalformedAllowInGo(t *testing.T) {
	const src = "package p\n\n" +
		"//logos:allow(correctness::structtag_malformed)\n" +
		"type S struct {\n" +
		"\tField string `json:bad`\n" +
		"}\n"

	if got := summarize(Run(check(t, KindGo, "x.go", src))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	got := summarize(Run(check(t, KindLgo, "x.lgo", src)))
	want := []string{
		"3 correctness::allow_forbidden_rule forbid",
		"5 correctness::structtag_malformed forbid",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("loGo/s package results %q, want %q", got, want)
	}
}
