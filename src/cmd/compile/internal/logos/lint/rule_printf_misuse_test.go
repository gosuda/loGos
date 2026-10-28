// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"strings"
	"testing"
)

const printfMisuseLgoSrc = `package p

import (
	"fmt"
	"os"
)

func badArgCount() {
	fmt.Printf("user %s count %d\n", "alice") // want
	fmt.Sprintf("user %s count %d\n")         // want
	fmt.Fprintf(os.Stdout, "%s %s\n", "one")  // want
	fmt.Printf("count %d\n", 1, 2)            // want
}

func badType() {
	fmt.Printf("%d\n", "not an int") // want
	fmt.Printf("%s\n", 42)           // want
	fmt.Printf("%t\n", 123)          // want
}

func badIndex() {
	fmt.Printf("%[0]d\n", 1) // want
}

func cleanForms(dynamic string) {
	fmt.Printf("user %s count %d\n", "alice", 42)
	fmt.Sprintf("val: %v, str: %s, int: %d\n", true, "ok", 10)
	fmt.Fprintf(os.Stdout, "name: %s\n", "bob")
	fmt.Printf("%v %+v %#v %T\n", "s", 1, true, struct{}{})
	fmt.Printf(dynamic, 1, 2, 3)
}
`

const printfMisuseGoSrc = `package p

import (
	"fmt"
	"os"
)

func badArgCount() {
	fmt.Printf("user %s count %d\n", "alice") // want
	fmt.Sprintf("user %s count %d\n")         // want
	fmt.Fprintf(os.Stdout, "%s %s\n", "one")  // want
	fmt.Printf("count %d\n", 1, 2)            // want
}

func badType() {
	fmt.Printf("%d\n", "not an int") // want
	fmt.Printf("%s\n", 42)           // want
	fmt.Printf("%t\n", 123)          // want
}

func badIndex() {
	fmt.Printf("%[0]d\n", 1) // want
}

func cleanForms(dynamic string) {
	fmt.Printf("user %s count %d\n", "alice", 42)
	fmt.Sprintf("val: %v, str: %s, int: %d\n", true, "ok", 10)
	fmt.Fprintf(os.Stdout, "name: %s\n", "bob")
	fmt.Printf("%v %+v %#v %T\n", "s", 1, true, struct{}{})
	fmt.Printf(dynamic, 1, 2, 3)
}
`

func TestPrintfMisuse(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", printfMisuseLgoSrc}, finding{"correctness::printf_misuse", Forbid})
	expect(t, KindGo, nil, testSource{"x.go", printfMisuseGoSrc}, finding{"correctness::printf_misuse", Deny})
}

func TestPrintfMisuseAllowInGo(t *testing.T) {
	const goSrc = `package p

import "fmt"

//logos:allow(correctness::printf_misuse)
func allowed() {
	fmt.Printf("count %d\n", 1, 2)
}
`
	if got := summarize(Run(check(t, KindGo, "x.go", goSrc))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	got := summarize(Run(check(t, KindLgo, "x.lgo", goSrc)))
	want := []string{
		"5 correctness::allow_forbidden_rule forbid",
		"7 correctness::printf_misuse forbid",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("loGo/s package results %q, want %q", got, want)
	}
}
