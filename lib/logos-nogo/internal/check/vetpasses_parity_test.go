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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file grounds the vendored go/analysis passes gVisor's nogo config
// conventionally enables (see the registry in analyzers.go) in the
// production checkPackage driver, following the existing *_parity_test.go
// pattern: each test builds a source-only fixture package, runs exactly one
// registered analyzer (plus its Requires closure) through real parsing,
// real type checking, and real diagnostics, and pins the finding the
// analyzer's own reference documents. Where an analyzer keys on a standard
// library import path (fmt, errors, context, net/http, encoding/json,
// sync/atomic), the fixture provides a source-only package at that exact
// path, since the test importer prefers in-memory sources over GOROOT
// archives; the analyzer only ever observes the import path string and the
// fixture's shapes. Two names from the conventional list need no test:
// "arg" names no analyzer in the vendored tree (or upstream), and cgocall
// requires cgo, which the importer force-disables. The composite and
// copylock passes register under their real analyzer names "composites"
// and "copylocks".

func TestAssignParity(t *testing.T) {
	// Reference: go/analysis/passes/assign/assign.go. A self-assignment
	// with no effects on either side is reported as such.
	const consumer = `package consumer

func f() int {
	x := 1
	x = x
	return x
}
`
	i := nogoParityImporter(t, "assign")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one assign diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "x = x")
	if got.Category != "assign" || got.Position.Line != wantLine || got.Message != "self-assignment of x" {
		t.Fatalf("finding = %+v, want category assign line %d message %q", got, wantLine, "self-assignment of x")
	}
}

func TestAtomicParity(t *testing.T) {
	// Reference: go/analysis/passes/atomic/atomic.go. Assigning the result
	// of sync/atomic.Add* back to the same address is a direct assignment
	// to an atomic value. The analyzer keys on the "sync/atomic" import
	// path, satisfied here by a source-only fixture package.
	const atom = `package atomic

func AddInt64(addr *int64, delta int64) int64 {
	*addr += delta
	return *addr
}
`
	const consumer = `package consumer

import "sync/atomic"

var v int64

func f() {
	v = atomic.AddInt64(&v, 1)
}
`
	i := nogoParityImporter(t, "atomic")
	nogoParityWrite(t, i, "sync/atomic", atom)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one atomic diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "atomic.AddInt64")
	if got.Category != "atomic" || got.Position.Line != wantLine || got.Message != "direct assignment to atomic value" {
		t.Fatalf("finding = %+v, want category atomic line %d message %q", got, wantLine, "direct assignment to atomic value")
	}
}

func TestBoolsParity(t *testing.T) {
	// Reference: go/analysis/passes/bools/bools.go. The same operand on
	// both sides of || is a redundant or.
	const consumer = `package consumer

func f(b bool) bool {
	return b || b
}
`
	i := nogoParityImporter(t, "bools")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one bools diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "return b || b")
	if got.Category != "bools" || got.Position.Line != wantLine || got.Message != "redundant or: b || b" {
		t.Fatalf("finding = %+v, want category bools line %d message %q", got, wantLine, "redundant or: b || b")
	}
}

func TestBuildtagParity(t *testing.T) {
	// Reference: go/analysis/passes/buildtag/buildtag.go. A +build line
	// after the package clause is misplaced; go/build ignores it, so the
	// file still type-checks and the analyzer reports the position.
	const consumer = `package consumer

// +build linux

var X = 1
`
	i := nogoParityImporter(t, "buildtag")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one buildtag diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "+build linux")
	if got.Category != "buildtag" || got.Position.Line != wantLine || got.Message != "misplaced +build comment" {
		t.Fatalf("finding = %+v, want category buildtag line %d message %q", got, wantLine, "misplaced +build comment")
	}
}

func TestCompositeParity(t *testing.T) {
	// Reference: go/analysis/passes/composite/composite.go. The analyzer
	// registers under the name "composites" and reports unkeyed literals
	// only for struct types from another package, so Point lives in a
	// dependency while the literal under test stays in the consumer.
	const shapes = `package shapes

type Point struct{ X, Y int }
`
	const consumer = `package consumer

import "shapes"

var _ = []shapes.Point{{1, 2}}
`
	i := nogoParityImporter(t, "composites")
	nogoParityWrite(t, i, "shapes", shapes)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one composites diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "[]shapes.Point{{1, 2}}")
	if got.Category != "composites" || got.Position.Line != wantLine || got.Message != "shapes.Point struct literal uses unkeyed fields" {
		t.Fatalf("finding = %+v, want category composites line %d message %q", got, wantLine, "shapes.Point struct literal uses unkeyed fields")
	}
}

func TestCopylocksParity(t *testing.T) {
	// Reference: go/analysis/passes/copylock/copylock.go. The analyzer
	// registers under the name "copylocks". A type whose pointer
	// implements sync.Locker (Lock/Unlock) but whose value does not is a
	// lock; declaring a parameter of that type copies it. The Locker
	// interface is constructed by the analyzer itself, so no sync import
	// is needed.
	const consumer = `package consumer

type Mutex struct{ v int }

func (m *Mutex) Lock()   {}
func (m *Mutex) Unlock() {}

func take(m Mutex) {}
`
	i := nogoParityImporter(t, "copylocks")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one copylocks diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "func take(m Mutex)")
	if got.Category != "copylocks" || got.Position.Line != wantLine ||
		!strings.Contains(got.Message, "take passes lock by value") {
		t.Fatalf("finding = %+v, want category copylocks line %d mentioning %q", got, wantLine, "take passes lock by value")
	}
}

func TestCtrlflowParity(t *testing.T) {
	// Reference: go/analysis/passes/ctrlflow/ctrlflow.go. The analyzer
	// provides control-flow graphs (and the lostcancel dependency) and
	// reports no diagnostics by itself, so parity is a clean run through
	// the production driver, proving the registration and its Requires
	// closure (inspect) execute.
	const consumer = `package consumer

func f() {
	for {
	}
}
`
	i := nogoParityImporter(t, "ctrlflow")
	nogoParityWrite(t, i, "consumer", consumer)
	if _, findings, _, err := i.checkPackage("consumer", i.sources["consumer"]); err != nil {
		t.Fatal(err)
	} else if len(findings) != 0 {
		t.Fatalf("findings = %v, want no ctrlflow diagnostics", findings)
	}
}

func TestErrorsasParity(t *testing.T) {
	// Reference: go/analysis/passes/errorsas/errorsas.go. The second
	// argument to errors.As must be a non-nil pointer to a type
	// implementing error. The analyzer resolves the callee through the
	// type index by the "errors" import path, satisfied here by a
	// source-only fixture package.
	const errs = `package errors

func As(err error, target any) bool {
	return false
}
`
	const consumer = `package consumer

import "errors"

type MyError struct{ msg string }

func (e MyError) Error() string { return e.msg }

func f(err error) bool {
	var target MyError
	return errors.As(err, target)
}
`
	i := nogoParityImporter(t, "errorsas")
	nogoParityWrite(t, i, "errors", errs)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one errorsas diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "errors.As(err, target)")
	wantMsg := "second argument to errors.As must be a non-nil pointer to either a type that implements error, or to any interface type"
	if got.Category != "errorsas" || got.Position.Line != wantLine || got.Message != wantMsg {
		t.Fatalf("finding = %+v, want category errorsas line %d message %q", got, wantLine, wantMsg)
	}
}

func TestHttpresponseParity(t *testing.T) {
	// Reference: go/analysis/passes/httpresponse/httpresponse.go. Using
	// the response before checking the error is reported. The analyzer
	// keys on the "net/http" import path and the Response type name,
	// satisfied here by a source-only fixture package.
	const http = `package http

type Header map[string][]string

type Body struct{}

func (Body) Close() error { return nil }

type Response struct{ Body Body }

func Get(url string) (*Response, error) { return nil, nil }
`
	const consumer = `package consumer

import "net/http"

func f(url string) error {
	resp, err := http.Get(url)
	defer resp.Body.Close()
	if err != nil {
		return err
	}
	return nil
}
`
	i := nogoParityImporter(t, "httpresponse")
	nogoParityWrite(t, i, "net/http", http)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one httpresponse diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "defer resp.Body.Close()")
	if got.Category != "httpresponse" || got.Position.Line != wantLine || got.Message != "using resp before checking for errors" {
		t.Fatalf("finding = %+v, want category httpresponse line %d message %q", got, wantLine, "using resp before checking for errors")
	}
}

func TestLoopclosureParity(t *testing.T) {
	// Reference: go/analysis/passes/loopclosure/loopclosure.go. A loop
	// variable captured by a func literal that runs concurrently is
	// reported. The analyzer only traverses files below go1.22 (where
	// loop variables became per-iteration), hence the build marker.
	const consumer = `//go:build go1.21

package consumer

func f(vals []int, done chan int) {
	for _, v := range vals {
		go func() {
			done <- v
		}()
	}
}
`
	i := nogoParityImporter(t, "loopclosure")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one loopclosure diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "done <- v")
	if got.Category != "loopclosure" || got.Position.Line != wantLine || got.Message != "loop variable v captured by func literal" {
		t.Fatalf("finding = %+v, want category loopclosure line %d message %q", got, wantLine, "loop variable v captured by func literal")
	}
}

func TestLostcancelParity(t *testing.T) {
	// Reference: go/analysis/passes/lostcancel/lostcancel.go. Discarding
	// the cancel function returns the leak diagnostic. The analyzer
	// recognizes the call by the "context" qualifier name, satisfied here
	// by a source-only fixture package of that name.
	const ctx = `package context

type Context interface{}

type CancelFunc func()

func WithCancel(parent Context) (Context, CancelFunc) {
	return parent, func() {}
}
`
	const consumer = `package consumer

import "context"

func f(parent context.Context) {
	ctx, _ := context.WithCancel(parent)
	_ = ctx
}
`
	i := nogoParityImporter(t, "lostcancel")
	nogoParityWrite(t, i, "context", ctx)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one lostcancel diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "context.WithCancel")
	wantMsg := "the cancel function returned by context.WithCancel should be called, not discarded, to avoid a context leak"
	if got.Category != "lostcancel" || got.Position.Line != wantLine || got.Message != wantMsg {
		t.Fatalf("finding = %+v, want category lostcancel line %d message %q", got, wantLine, wantMsg)
	}
}

func TestNilfuncParity(t *testing.T) {
	// Reference: go/analysis/passes/nilfunc/nilfunc.go. A declared
	// function is never nil, so comparing it to nil is always false.
	const consumer = `package consumer

func f() {}

func g() bool {
	return f == nil
}
`
	i := nogoParityImporter(t, "nilfunc")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one nilfunc diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "return f == nil")
	if got.Category != "nilfunc" || got.Position.Line != wantLine || got.Message != "comparison of function f == nil is always false" {
		t.Fatalf("finding = %+v, want category nilfunc line %d message %q", got, wantLine, "comparison of function f == nil is always false")
	}
}

func TestNilnessParity(t *testing.T) {
	// Reference: go/analysis/passes/nilness/nilness.go. After the early
	// return on p == nil, the second comparison is impossible. The
	// analyzer works over SSA via its buildssa requirement, which the
	// driver registers through the Requires closure.
	const consumer = `package consumer

func f(p *int) int {
	if p == nil {
		return 0
	}
	if p == nil { // still nil
		return 1
	}
	return *p
}
`
	i := nogoParityImporter(t, "nilness")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one nilness diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "if p == nil { // still nil")
	if got.Category != "nilness" || got.Position.Line != wantLine || got.Message != "impossible condition: non-nil == nil" {
		t.Fatalf("finding = %+v, want category nilness line %d message %q", got, wantLine, "impossible condition: non-nil == nil")
	}
}

func TestPrintfParity(t *testing.T) {
	// Reference: go/analysis/passes/printf/printf.go. Sprintf is on the
	// analyzer's allowlist by its full "fmt.Sprintf" name, satisfied here
	// by a source-only fixture package at the "fmt" path; the position
	// is the whole call.
	const fmt = `package fmt

func Sprintf(format string, args ...any) string {
	return format
}
`
	const consumer = `package consumer

import "fmt"

var _ = fmt.Sprintf("%d %d", 1)
`
	i := nogoParityImporter(t, "printf")
	nogoParityWrite(t, i, "fmt", fmt)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one printf diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "fmt.Sprintf(")
	wantMsg := "fmt.Sprintf format %d reads arg #2, but call has 1 arg"
	if got.Category != "printf" || got.Position.Line != wantLine || got.Message != wantMsg {
		t.Fatalf("finding = %+v, want category printf line %d message %q", got, wantLine, wantMsg)
	}
}

func TestShiftParity(t *testing.T) {
	// Reference: go/analysis/passes/shift/shift.go. Shifting a 32-bit
	// value by a constant 40 exceeds its width.
	const consumer = `package consumer

func f(x int32) int32 {
	return x << 40
}
`
	i := nogoParityImporter(t, "shift")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one shift diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "return x << 40")
	if got.Category != "shift" || got.Position.Line != wantLine || got.Message != "x (32 bits) too small for shift of 40" {
		t.Fatalf("finding = %+v, want category shift line %d message %q", got, wantLine, "x (32 bits) too small for shift of 40")
	}
}

func TestStdmethodsParity(t *testing.T) {
	// Reference: go/analysis/passes/stdmethods/stdmethods.go.
	// UnmarshalJSON must have signature UnmarshalJSON([]byte) error; the
	// missing error result is reported.
	const consumer = `package consumer

type T struct{}

func (T) UnmarshalJSON(data []byte) {}
`
	i := nogoParityImporter(t, "stdmethods")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one stdmethods diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "func (T) UnmarshalJSON(data []byte)")
	if got.Category != "stdmethods" || got.Position.Line != wantLine || got.Message != "method UnmarshalJSON(data []byte) should have signature UnmarshalJSON([]byte) error" {
		t.Fatalf("finding = %+v, want category stdmethods line %d message %q", got, wantLine, "method UnmarshalJSON(data []byte) should have signature UnmarshalJSON([]byte) error")
	}
}

func TestStringintconvParity(t *testing.T) {
	// Reference: go/analysis/passes/stringintconv/string.go. Converting
	// an int to a string yields a one-rune string, not digits.
	const consumer = `package consumer

func f(i int) string {
	return string(i)
}
`
	i := nogoParityImporter(t, "stringintconv")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one stringintconv diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "return string(i)")
	wantMsg := "conversion from int to string yields a string of one rune, not a string of digits"
	if got.Category != "stringintconv" || got.Position.Line != wantLine || got.Message != wantMsg {
		t.Fatalf("finding = %+v, want category stringintconv line %d message %q", got, wantLine, wantMsg)
	}
}

func TestStructtagParity(t *testing.T) {
	// Reference: go/analysis/passes/structtag/structtag.go. A tag that
	// is not key:"value" pairs is incompatible with StructTag.Get.
	const consumer = "package consumer\n\ntype T struct {\n\tX int `json:name`\n}\n"
	i := nogoParityImporter(t, "structtag")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one structtag diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "X int")
	wantMsg := "struct field tag `json:name` not compatible with reflect.StructTag.Get: bad syntax for struct tag value"
	if got.Category != "structtag" || got.Position.Line != wantLine || got.Message != wantMsg {
		t.Fatalf("finding = %+v, want category structtag line %d message %q", got, wantLine, wantMsg)
	}
}

func TestTestsParity(t *testing.T) {
	// Reference: go/analysis/passes/tests/tests.go. An example function
	// must refer to a known identifier. The analyzer only examines
	// _test.go files, so the fixture is written under that name directly;
	// no testing import is needed for this check.
	const consumer = `package consumer

func ExampleNope() {}
`
	i := nogoParityImporter(t, "tests")
	filename := filepath.Join(t.TempDir(), "example_test.go")
	if err := os.WriteFile(filename, []byte(consumer), 0600); err != nil {
		t.Fatal(err)
	}
	i.sources["consumer"] = append(i.sources["consumer"], filename)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one tests diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "func ExampleNope()")
	if got.Category != "tests" || got.Position.Line != wantLine || got.Message != "ExampleNope refers to unknown identifier: Nope" {
		t.Fatalf("finding = %+v, want category tests line %d message %q", got, wantLine, "ExampleNope refers to unknown identifier: Nope")
	}
}

func TestUnmarshalParity(t *testing.T) {
	// Reference: go/analysis/passes/unmarshal/unmarshal.go. The second
	// argument to encoding/json.Unmarshal must be a pointer. The analyzer
	// keys on the callee's "encoding/json" package path, satisfied here
	// by a source-only fixture package.
	const js = `package json

func Unmarshal(data []byte, v any) error {
	return nil
}
`
	const consumer = `package consumer

import "encoding/json"

type V struct{ N int }

func f() error {
	var v V
	return json.Unmarshal([]byte("{}"), v)
}
`
	i := nogoParityImporter(t, "unmarshal")
	nogoParityWrite(t, i, "encoding/json", js)
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one unmarshal diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "json.Unmarshal(")
	if got.Category != "unmarshal" || got.Position.Line != wantLine || got.Message != "call of Unmarshal passes non-pointer as second argument" {
		t.Fatalf("finding = %+v, want category unmarshal line %d message %q", got, wantLine, "call of Unmarshal passes non-pointer as second argument")
	}
}

func TestUnreachableParity(t *testing.T) {
	// Reference: go/analysis/passes/unreachable/unreachable.go.
	// Statements after a return never execute.
	const consumer = `package consumer

func f() {
	return
	_ = 1
}
`
	i := nogoParityImporter(t, "unreachable")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one unreachable diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "_ = 1")
	if got.Category != "unreachable" || got.Position.Line != wantLine || got.Message != "unreachable code" {
		t.Fatalf("finding = %+v, want category unreachable line %d message %q", got, wantLine, "unreachable code")
	}
}

func TestUnsafeptrParity(t *testing.T) {
	// Reference: go/analysis/passes/unsafeptr/unsafeptr.go. Converting a
	// plain uintptr to unsafe.Pointer is flagged; unsafe itself is
	// synthesized by go/types, so no fixture package is needed.
	const consumer = `package consumer

import "unsafe"

func f(u uintptr) unsafe.Pointer {
	return unsafe.Pointer(u)
}
`
	i := nogoParityImporter(t, "unsafeptr")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one unsafeptr diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "unsafe.Pointer(u)")
	if got.Category != "unsafeptr" || got.Position.Line != wantLine || got.Message != "possible misuse of unsafe.Pointer" {
		t.Fatalf("finding = %+v, want category unsafeptr line %d message %q", got, wantLine, "possible misuse of unsafe.Pointer")
	}
}

func TestUnusedresultParity(t *testing.T) {
	// Reference: go/analysis/passes/unusedresult/unusedresult.go. The
	// analyzer flags discarded results of no-arg methods named Error or
	// String (among its allowlists), so a local Error method exercises
	// the check with no imports.
	const consumer = `package consumer

type E struct{ msg string }

func (e E) Error() string { return e.msg }

func f() {
	e := E{}
	e.Error()
}
`
	i := nogoParityImporter(t, "unusedresult")
	nogoParityWrite(t, i, "consumer", consumer)
	_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one unusedresult diagnostic", findings)
	}
	got := findings[0]
	wantLine := nogoParityLine(t, consumer, "e.Error()")
	if got.Category != "unusedresult" || got.Position.Line != wantLine || got.Message != "result of (consumer.E).Error call not used" {
		t.Fatalf("finding = %+v, want category unusedresult line %d message %q", got, wantLine, "result of (consumer.E).Error call not used")
	}
}

// TestVetPassRegistry pins the census of the vendored go/analysis passes
// gVisor's nogo config conventionally enables: every name below must
// resolve in the analyzer registry (driving both the helper run and the
// name-keyed config in internal/config). Composite and copylock register
// under their real analyzer names "composites" and "copylocks".
func TestVetPassRegistry(t *testing.T) {
	want := []string{
		"assign",
		"atomic",
		"bools",
		"buildtag",
		"cgocall",
		"composites",
		"copylocks",
		"ctrlflow",
		"errorsas",
		"httpresponse",
		"loopclosure",
		"lostcancel",
		"nilfunc",
		"nilness",
		"printf",
		"shift",
		"stdmethods",
		"stringintconv",
		"structtag",
		"tests",
		"unmarshal",
		"unreachable",
		"unsafeptr",
		"unusedresult",
	}
	names := AnalyzerNames()
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s is not registered", w)
		}
	}
}
