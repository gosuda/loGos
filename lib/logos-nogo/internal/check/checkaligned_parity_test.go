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
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// nogoParityImporter builds an importer that runs exactly one registered
// analyzer (plus its Requires closure, e.g. buildssa for checklocks) through
// the production checkPackage driver: real parsing, real type checking, real
// fact export/import, and real diagnostics.
func nogoParityImporter(t *testing.T, name string) *importer {
	t.Helper()
	analyzers := make(map[*analysis.Analyzer]analyzer)
	for a, runner := range allAnalyzers {
		if a.Name == name {
			register(analyzers, runner)
		}
	}
	if len(analyzers) == 0 {
		t.Fatalf("%s is not registered", name)
	}
	return &importer{
		fset:      token.NewFileSet(),
		sources:   make(map[string][]string),
		cache:     make(map[string]*importerEntry),
		imports:   make(map[string]*types.Package),
		analyzers: analyzers,
	}
}

// nogoParityWrite materialises one in-memory package source for the importer.
func nogoParityWrite(t *testing.T, i *importer, path, source string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "source.go")
	if err := os.WriteFile(filename, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	i.sources[path] = append(i.sources[path], filename)
}

// nogoParityLine returns the 1-based line of the first source line containing
// substr, so expectations stay anchored to the statement under test.
func nogoParityLine(t *testing.T, source, substr string) int {
	t.Helper()
	for idx, line := range strings.Split(source, "\n") {
		if strings.Contains(line, substr) {
			return idx + 1
		}
	}
	t.Fatalf("source does not contain %q", substr)
	return 0
}

// Reference: tools/checkaligned/checkaligned.go. The analyzer matches a
// selector whose base identifier is literally named "atomic", so the fixture
// is a source-only package named atomic exercising that matching contract
// with real typed selector resolution through the production checkPackage
// driver.
const parityAtomicFixture = `package atomic

// AddInt64 mirrors sync/atomic.AddInt64: a blocked operation.
func AddInt64(addr *int64, delta int64) int64 {
	*addr += delta
	return *addr
}

// Add mirrors an ordinary unblocked operation: it must never be flagged.
func Add(addr *int, delta int) int {
	*addr += delta
	return *addr
}
`

func TestCheckalignedParity(t *testing.T) {
	t.Run("blocked_call", func(t *testing.T) {
		i := nogoParityImporter(t, "checkaligned")
		nogoParityWrite(t, i, "atomic", parityAtomicFixture)
		const consumer = `package consumer

import "atomic"

var counter int64

func Bump() {
	atomic.AddInt64(&counter, 1)
}
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one checkaligned diagnostic", findings)
		}
		got := findings[0]
		wantLine := nogoParityLine(t, consumer, "atomic.AddInt64")
		wantMsg := "don't call atomic.AddInt64; use atomicbitops.Int64 instead"
		if got.Category != "checkaligned" || got.Position.Line != wantLine || got.Message != wantMsg {
			t.Fatalf("finding = %+v, want category checkaligned line %d message %q", got, wantLine, wantMsg)
		}
	})
	t.Run("unblocked_call", func(t *testing.T) {
		i := nogoParityImporter(t, "checkaligned")
		nogoParityWrite(t, i, "atomic", parityAtomicFixture)
		const consumer = `package consumer

import "atomic"

var counter int

func Bump() {
	atomic.Add(&counter, 1)
}
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics for an unblocked operation", findings)
		}
	})
	t.Run("escape_hatch", func(t *testing.T) {
		i := nogoParityImporter(t, "checkaligned")
		nogoParityWrite(t, i, "atomic", parityAtomicFixture)
		const consumer = `// +checkalignedignore
package consumer

import "atomic"

var counter int64

func Bump() {
	atomic.AddInt64(&counter, 1)
}
`
		nogoParityWrite(t, i, "consumer", consumer)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics under +checkalignedignore", findings)
		}
	})
}
