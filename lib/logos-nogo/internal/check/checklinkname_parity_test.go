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
	"strings"
	"testing"
)

// Reference: tools/checklinkname/test/test_unsafe.go. The consumer links
// against a source-only dependency through the production fact flow: the dep
// package exports ResolvedSymbols/UnresolvedLinknames via checkPackage, and
// the consumer imports them with ImportPackageFact before resolving. The dep
// path is a single segment because splitSymbol cuts the remote symbol at the
// first dot; consumers use blank imports plus bodyless linkname declarations
// as in the reference test.
const parityLinkDep = `package dep

// target is the link target whose signature consumers must match.
func target() int {
	return 1
}

// Marker is referenced by consumers so the dep import is a real use;
// resolvePos locates diagnostics at a named import, which a blank import
// cannot provide.
var Marker = 1
`

func TestChecklinknameParity(t *testing.T) {
	t.Run("signature_mismatch", func(t *testing.T) {
		i := nogoParityImporter(t, "checklinkname")
		nogoParityWrite(t, i, "dep", parityLinkDep)
		const consumer = `package consumer

import "dep"

import _ "unsafe"

var _ = dep.Marker

//go:linkname local dep.target
func local() string
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one diagnostic for the mismatched linkname", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, consumer, `import "dep"`)
		if got.Category != "checklinkname" || got.Position.Line != wantLine || got.Physical.Line != wantLine ||
			!strings.Contains(got.Message, `symbol "target" has signature "func () (string)"`) ||
			!strings.Contains(got.Message, `expected signature "func () (int)"`) {
			t.Fatalf("finding = %+v, want category checklinkname reporting the signature mismatch", got)
		}
	})
	t.Run("signature_match", func(t *testing.T) {
		i := nogoParityImporter(t, "checklinkname")
		nogoParityWrite(t, i, "dep", parityLinkDep)
		const consumer = `package consumer

import "dep"

import _ "unsafe"

var _ = dep.Marker

//go:linkname local dep.target
func local() int
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics for a matching linkname", findings)
		}
	})
	t.Run("dangling_remote_in_main", func(t *testing.T) {
		i := nogoParityImporter(t, "checklinkname")
		nogoParityWrite(t, i, "dep", parityLinkDep)
		const mainPkg = `package main

import "dep"

import _ "unsafe"

var _ = dep.Marker

//go:linkname local dep.missing
func local() int
`
		nogoParityWrite(t, i, "main", mainPkg)
		_, findings, _, err := i.checkPackage("main", i.sources["main"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one diagnostic for the dangling remote", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, mainPkg, `import "dep"`)
		if got.Category != "checklinkname" || got.Position.Line != wantLine || got.Physical.Line != wantLine ||
			!strings.Contains(got.Message, `remote symbol "missing" not defined in package "dep"`) {
			t.Fatalf("finding = %+v, want category checklinkname reporting the undefined remote", got)
		}
	})
}
