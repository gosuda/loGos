// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"strings"
	"testing"
)

const httpResponseLgoSrc = `package p

import "net/http"

func badGet() {
	resp, err := http.Get("http://example.com")
	defer resp.Body.Close() // want
	if err != nil {
		return
	}
	_ = resp
}

func goodGet() {
	resp, err := http.Get("http://example.com")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_ = resp
}

func otherDefer() {
	resp, err := http.Get("http://example.com")
	defer cleanup()
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_ = resp
}

func cleanup() {}
`

const httpResponseGoSrc = `package p

import "net/http"

func badGet() {
	resp, err := http.Get("http://example.com")
	defer resp.Body.Close() // want
	if err != nil {
		return
	}
	_ = resp
}

func goodGet() {
	resp, err := http.Get("http://example.com")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_ = resp
}

func otherDefer() {
	resp, err := http.Get("http://example.com")
	defer cleanup()
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_ = resp
}

func cleanup() {}
`

func TestHTTPResponse(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", httpResponseLgoSrc}, finding{"correctness::httpresponse", Forbid})
	expect(t, KindGo, nil, testSource{"x.go", httpResponseGoSrc}, finding{"correctness::httpresponse", Deny})
}

func TestHTTPResponseAllowInGo(t *testing.T) {
	const src = `package p

import "net/http"

//logos:allow(correctness::httpresponse)
func allowed() {
	resp, err := http.Get("http://example.com")
	defer resp.Body.Close()
	if err != nil {
		return
	}
	_ = resp
}
`
	if got := summarize(Run(check(t, KindGo, "x.go", src))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	got := summarize(Run(check(t, KindLgo, "x.lgo", src)))
	want := []string{
		"5 correctness::allow_forbidden_rule forbid",
		"8 correctness::httpresponse forbid",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("loGo/s package results %q, want %q", got, want)
	}
}
