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

package checkimports

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"golang.org/x/tools/go/analysis"
)

// Reference provenance: table mirrors
// gvisor.dev/gvisor/tools/checkimports/check_imports_test.go; zip cases use
// an explicit example wrapper because logos/nogo must not recommend the
// nonexistent gvisor.dev/gvisor/pkg/zip by default.
func TestImports(t *testing.T) {
	// Default must stay disabled: no nonexistent recommendation.
	if PreferredZipImport != "" {
		t.Fatalf("PreferredZipImport default = %q, want empty", PreferredZipImport)
	}
	for _, tc := range []struct {
		name     string
		pkg      string
		filename string
		imports  string
		want     string
	}{
		{
			name:     "unsafe",
			filename: "example.go",
			imports:  `import "unsafe"`,
			want:     "package unsafe imported by example.go; must end with _unsafe.go",
		},
		{
			name:     "unsafe_file",
			filename: "example_unsafe.go",
			imports:  `import "unsafe"`,
		},
		{
			name:     "unsafe_test",
			filename: "example_test.go",
			imports:  `import "unsafe"`,
		},
		{
			name:    "zip_default_disabled",
			imports: `import "archive/zip"`,
		},
		{
			name:     "zip_default_disabled_alias",
			filename: "example_test.go",
			imports:  `import other "archive/zip"`,
		},
		{
			name:    "ordinary_imports",
			imports: "import (\n\"io\"\n\"example.com/project/pkg/zip\"\n)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.filename, "package example\n"+tc.imports, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			var findings []string
			pass := &analysis.Pass{
				Fset:  fset,
				Files: []*ast.File{f},
				Pkg:   types.NewPackage(tc.pkg, "example"),
				Report: func(d analysis.Diagnostic) {
					findings = append(findings, d.Message)
				},
			}
			if _, err := Analyzer.Run(pass); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("unexpected findings: %v", findings)
				}
			} else if len(findings) != 1 || findings[0] != tc.want {
				t.Fatalf("findings = %v, want [%q]", findings, tc.want)
			}
		})
	}
}

func TestZipPreferenceExplicit(t *testing.T) {
	const wrapper = "example.com/project/pkg/zip"
	old := PreferredZipImport
	PreferredZipImport = wrapper
	defer func() { PreferredZipImport = old }()
	for _, tc := range []struct {
		name     string
		pkg      string
		filename string
		imports  string
		want     string
	}{
		{
			name:    "zip_proxy",
			pkg:     wrapper,
			imports: `import "archive/zip"`,
		},
		{
			name:    "zip_consumer",
			imports: `import "archive/zip"`,
			want:    "import example.com/project/pkg/zip instead of archive/zip",
		},
		{
			name:     "zip_test_alias",
			filename: "example_test.go",
			imports:  `import other "archive/zip"`,
			want:     "import example.com/project/pkg/zip instead of archive/zip",
		},
		{
			name:    "zip_subpackage",
			pkg:     wrapper + "/subpackage",
			imports: `import "archive/zip"`,
			want:    "import example.com/project/pkg/zip instead of archive/zip",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.filename, "package example\n"+tc.imports, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			var findings []string
			pass := &analysis.Pass{
				Fset:  fset,
				Files: []*ast.File{f},
				Pkg:   types.NewPackage(tc.pkg, "example"),
				Report: func(d analysis.Diagnostic) {
					findings = append(findings, d.Message)
				},
			}
			if _, err := Analyzer.Run(pass); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("unexpected findings: %v", findings)
				}
			} else if len(findings) != 1 || findings[0] != tc.want {
				t.Fatalf("findings = %v, want [%q]", findings, tc.want)
			}
		})
	}
}
