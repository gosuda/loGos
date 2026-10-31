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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"logos/nogo/internal/flags"
)

// Reference: tools/checkescape/checkescape.go. checkescape recognises
// heap allocations through the exact "+checkescape:heap" annotation grammar,
// matching SSA Alloc operations against calls in the real compiled archive
// (production checkPackage path: real parsing, type checking, SSA
// construction via buildssa, plus go tool objdump over a real compiled
// object). Fixture bodies are analysis-only and never execute.

// nogoParityArchive compiles source to a real archive with the local
// toolchain and points the production archive lookup at it, so the
// binaryAnalyzer path (go tool objdump over a real compiled archive)
// executes instead of failing with a missing archive. The on-disk file is
// named source.go so objdump's basename:line keys match the parsed package.
func nogoParityArchive(t *testing.T, i *importer, path, source string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "source.go")
	if err := os.WriteFile(src, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(dir, "pkg.a")
	cmd := exec.Command(flags.Go, "tool", "compile", "-pack", "-p", path, "-o", obj, "source.go")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile escape fixture: %v\n%s", err, out)
	}
	i.sources[path] = append(i.sources[path], src)
	old, ok := flags.ArchiveMap[path]
	flags.ArchiveMap[path] = obj
	t.Cleanup(func() {
		if ok {
			flags.ArchiveMap[path] = old
		} else {
			delete(flags.ArchiveMap, path)
		}
	})
}

// parityEscapeFixture leaks the heap in an annotated function (must report
// at the allocating statement) and in an unannotated function (must stay
// silent, proving the annotation gates the diagnostic).
const parityEscapeFixture = `package consumer

// Box is a plain value type.
type Box struct {
	v int
}

// Leaky allocates on the heap and must be diagnosed.
// +checkescape:heap
//
//go:noinline
//go:nosplit
func Leaky() *Box {
	return new(Box)
}

// Quiet also allocates on the heap but carries no annotation,
// so it must stay silent.
//
//go:noinline
//go:nosplit
func Quiet() *Box {
	return new(Box)
}
`

// parityEscapeStackFixture builds a composite literal that the compiler
// keeps on the stack: the archive proves no heap call, so the annotated
// function stays silent. Without a working archive the conservative
// fallback would report a heap escape here, so this control fails a dead
// objdump path. Shape mirrors the production test3 stackAllocation
// negative control.
const parityEscapeStackFixture = `package consumer

// StackStays keeps its composite literal on the stack and must stay silent.
// +checkescape:all
//
//go:noinline
//go:nosplit
func StackStays(a, b uint64) uint64 {
	values := &[2]uint64{a, b}
	return sum(values)
}

//go:noinline
//go:nosplit
func sum(values *[2]uint64) uint64 {
	return values[0] + values[1]
}
`

// parityEscapeMalformedFixture uses the malformed spelling from
// TestAnnotationSyntax ("// +checkescape all", space instead of colon),
// whose contract is a "malformed checkescape annotation" diagnostic.
const parityEscapeMalformedFixture = `package consumer

// Broken carries a malformed annotation (space, not colon).
// +checkescape all
func Broken() int {
	return 1
}
`

func TestCheckescapeParity(t *testing.T) {
	t.Run("annotated_heap_reports", func(t *testing.T) {
		i := nogoParityImporter(t, "checkescape")
		nogoParityArchive(t, i, "consumer", parityEscapeFixture)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		wantLine := nogoParityLine(t, parityEscapeFixture, "return new(Box)")
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one diagnostic for the annotated heap escape", findings)
		}
		got := findings[0]
		if got.Category != "checkescape" || got.Position.Line != wantLine || !strings.Contains(got.Message, "heap:") {
			t.Fatalf("finding = %+v, want category checkescape line %d naming the heap escape", got, wantLine)
		}
		// The detail must name the compiled allocator call (newobject or a
		// size-specialized mallocgc helper). On objdump failure the analyzer
		// falls back to "(possible, ...)" evidence and still reports, so a
		// heap:-only assertion would pass on a dead archive path.
		if !strings.Contains(got.Message, "runtime.newobject") && !strings.Contains(got.Message, "runtime.mallocgc") {
			t.Fatalf("finding = %+v, want compiled allocator evidence (runtime.newobject or runtime.mallocgc), not the objdump fallback", got)
		}
		if strings.Contains(got.Message, "(possible") {
			t.Fatalf("finding = %+v, want real archive evidence, not conservative fallback", got)
		}
	})
	t.Run("stack_allocated_clean", func(t *testing.T) {
		i := nogoParityImporter(t, "checkescape")
		nogoParityArchive(t, i, "consumer", parityEscapeStackFixture)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 0 {
			t.Fatalf("findings = %v, want no diagnostics for a stack-kept allocation under annotation", findings)
		}
	})
	t.Run("missing_archive", func(t *testing.T) {
		i := nogoParityImporter(t, "checkescape")
		nogoParityWrite(t, i, "consumer", parityEscapeFixture)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one diagnostic describing the missing archive", findings)
		}
		got := findings[0]
		if got.Category != "checkescape" || !strings.Contains(got.Message, `unable to find "consumer" archive`) {
			t.Fatalf("finding = %+v, want category checkescape naming the missing consumer archive", got)
		}
		if got.Position.Filename != i.sources["consumer"][0] {
			t.Fatalf("finding = %+v, want the missing-archive diagnostic anchored at the fixture source", got)
		}
	})
	t.Run("malformed_annotation", func(t *testing.T) {
		i := nogoParityImporter(t, "checkescape")
		nogoParityArchive(t, i, "consumer", parityEscapeMalformedFixture)
		_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
		if err != nil {
			t.Fatal(err)
		}
		wantLine := nogoParityLine(t, parityEscapeMalformedFixture, "func Broken()")
		if len(findings) != 1 {
			t.Fatalf("findings = %v, want exactly one diagnostic for the malformed annotation", findings)
		}
		got := findings[0]
		if got.Category != "checkescape" || got.Position.Line != wantLine || !strings.Contains(got.Message, "malformed checkescape annotation") {
			t.Fatalf("finding = %+v, want category checkescape line %d naming the malformed annotation", got, wantLine)
		}
	})
}
