// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"testing"
)

const stringIntconvLgoSrc = `package p

import "strconv"

func badConversions(port int, id int64) {
	_ = string(port) // want
	_ = string(1234) // want
	_ = string(id)   // want
}

func formatDecimal(x int) string {
	return strconv.Itoa(x)
}

func cleanConversions(port int, r rune, b byte, bs []byte) {
	_ = strconv.Itoa(port)
	_ = string(r)
	_ = string('A')
	_ = string(b)
	_ = string(rune(port))
	_ = string(bs)
	_ = formatDecimal(port)
}
`

const stringIntconvGoSrc = `package p

import "strconv"

func badConversions(port int, id int64) {
	_ = string(port) // want
	_ = string(1234) // want
	_ = string(id)   // want
}

func formatDecimal(x int) string {
	return strconv.Itoa(x)
}

func cleanConversions(port int, r rune, b byte, bs []byte) {
	_ = strconv.Itoa(port)
	_ = string(r)
	_ = string('A')
	_ = string(b)
	_ = string(rune(port))
	_ = string(bs)
	_ = formatDecimal(port)
}
`

func TestStringIntconv(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", stringIntconvLgoSrc}, finding{"suspicious::string_intconv", Warn})
	expect(t, KindGo, nil, testSource{"x.go", stringIntconvGoSrc}, finding{"suspicious::string_intconv", Warn})
}

func TestStringIntconvAllow(t *testing.T) {
	const src = `package p

//logos:allow(suspicious::string_intconv)
func allowed(port int) string {
	return string(port)
}
`
	if got := summarize(Run(check(t, KindGo, "x.go", src))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	if got := summarize(Run(check(t, KindLgo, "x.lgo", src))); len(got) != 0 {
		t.Errorf("loGo/s package results %q, want none", got)
	}
}
