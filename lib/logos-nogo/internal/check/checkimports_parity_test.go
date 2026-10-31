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

// nogoCheckimportsWrite materialises one in-memory package source under the
// given basename so the production driver sees a real filename for the
// _unsafe.go suffix rule.
func nogoCheckimportsWrite(t *testing.T, i *importer, pkgPath, basename, source string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), basename)
	if err := os.WriteFile(filename, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	i.sources[pkgPath] = append(i.sources[pkgPath], filename)
}

// TestCheckimportsParity exercises checkimports through the production
// checkPackage driver: real parsing, real type checking, real diagnostics.
// Reference implementation:
// lib/logos-nogo/internal/checkimports/check_imports.go.
//
// Unsafe rule: a file importing "unsafe" must end in _unsafe.go (or _test.go).
// The driver passes real on-disk basenames via pass.Fset, so a.go fires and
// a_unsafe.go is clean. Fixtures use unsafe.Sizeof so the import is used.
//
// Std imports type-check through the production ImportMap path: fmt,
// archive/zip and their transitive deps resolve to real export data from the
// actual fork toolchain (`go list -export` via flags.Go, GOTOOLCHAIN=local,
// GOPROXY=off, PATH fork first), so fmt_clean and zip_default_disabled reach
// the analyzer via importPackage->findBinary->gcexportdata instead of failing
// in findStdPkg. PreferredZipImport stays at its default empty (no CLI/config
// wiring), so the archive/zip case remains disabled and zip_default_disabled
// asserts zero findings after real type checking; the opt-in branch stays
// covered by the direct-var unit test.
func TestCheckimportsParity(t *testing.T) {
	// Real std export closure through the existing harness/flags pattern.
	// The production driver resolves fmt/archive/zip via flags.ImportMap;
	// without entries it falls back to findStdPkg and fails with a missing
	// archive harness error. Populate the closure (including transitive deps)
	// from the actual toolchain so controls type-check. Save/restore via
	// t.Cleanup; the test stays serial (no t.Parallel) so the full Parity
	// suite is race-safe.
	{
		cmd := exec.Command(flags.Go, "list", "-export", "-e", "--deps", "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", "fmt", "archive/zip")
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -export fmt archive/zip: %v", err)
		}
		type savedEntry struct {
			old string
			ok  bool
		}
		saved := make(map[string]savedEntry)
		t.Cleanup(func() {
			for path, s := range saved {
				if s.ok {
					flags.ImportMap[path] = s.old
				} else {
					delete(flags.ImportMap, path)
				}
			}
		})
		for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				continue
			}
			path, export := parts[0], parts[1]
			if _, seen := saved[path]; !seen {
				if old, ok := flags.ImportMap[path]; ok {
					saved[path] = savedEntry{old: old, ok: true}
				} else {
					saved[path] = savedEntry{ok: false}
				}
			}
			flags.ImportMap[path] = export
		}
		if _, ok := flags.ImportMap["fmt"]; !ok {
			t.Fatalf("go list -export returned no Export for fmt")
		}
		if _, ok := flags.ImportMap["archive/zip"]; !ok {
			t.Fatalf("go list -export returned no Export for archive/zip")
		}
	}
	cases := []struct {
		name         string
		basename     string
		source       string
		wantFindings int
		wantMsg      string
		wantBase     string
	}{
		{
			name:         "unsafe_plain_attack",
			basename:     "a.go",
			source:       "package consumer\n\nimport \"unsafe\"\n\nvar _ = unsafe.Sizeof(0)\n",
			wantFindings: 1,
			wantMsg:      "must end with _unsafe.go",
			wantBase:     "a.go",
		},
		{
			name:         "unsafe_suffix_clean",
			basename:     "a_unsafe.go",
			source:       "package consumer\n\nimport \"unsafe\"\n\nvar _ = unsafe.Sizeof(0)\n",
			wantFindings: 0,
		},
		{
			name:         "fmt_clean",
			basename:     "c.go",
			source:       "package consumer\n\nimport \"fmt\"\n\nvar _ = fmt.Sprintf(\"%d\", 1)\n",
			wantFindings: 0,
		},
		{
			name:         "zip_default_disabled",
			basename:     "z.go",
			source:       "package consumer\n\nimport \"archive/zip\"\n\nvar _ = zip.NewReader\n",
			wantFindings: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := nogoParityImporter(t, "checkimports")
			nogoCheckimportsWrite(t, i, "consumer", tc.basename, tc.source)
			_, findings, _, err := i.checkPackage("consumer", i.sources["consumer"])
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != tc.wantFindings {
				t.Fatalf("findings = %v, want %d diagnostic(s)", findings, tc.wantFindings)
			}
			if tc.wantFindings == 0 {
				return
			}
			got := findings[0]
			wantLine := nogoParityLine(t, tc.source, `import "unsafe"`)
			if got.Category != "checkimports" || got.Position.Line != wantLine ||
				!strings.Contains(got.Message, tc.wantMsg) ||
				!strings.Contains(got.Message, tc.wantBase) {
				t.Fatalf("finding = %+v, want category checkimports line %d message %q naming %q", got, wantLine, tc.wantMsg, tc.wantBase)
			}
		})
	}
}
