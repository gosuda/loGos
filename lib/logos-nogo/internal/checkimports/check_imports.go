// Copyright 2019 The gVisor Authors.
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

// Package checkimports enforces restrictions on package imports.
package checkimports

import (
	"path"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// PreferredZipImport names the project-local replacement for archive/zip.
//
// Reference gVisor enforces "import gvisor.dev/gvisor/pkg/zip instead of
// archive/zip". logos/nogo has no pkg/zip wrapper, so recommending that
// path would point at a nonexistent package. The rule is therefore
// explicit opt-in: when PreferredZipImport is empty (the default) the
// archive/zip case is disabled and only the unsafe rule applies. Set it
// to your project's wrapper (for example "example.com/project/pkg/zip")
// to restore gvisor-like enforcement; only the owning package itself is
// then exempt (subpackages still report, matching reference).
//
// Ported from gvisor.dev/gvisor/tools/checkimports; unsafe rule kept
// verbatim, zip preference made explicit per logos/nogo port contract.
var PreferredZipImport string

// Analyzer defines the entrypoint.
var Analyzer = &analysis.Analyzer{
	Name: "checkimports",
	Doc:  "enforces restrictions on package imports",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, f := range pass.Files {
		for _, imp := range f.Imports {
			pkg, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			switch pkg {
			case "unsafe":
				filename := pass.Fset.File(imp.Pos()).Name()
				if !strings.HasSuffix(filename, "_unsafe.go") && !strings.HasSuffix(filename, "_test.go") {
					pass.Reportf(imp.Pos(), "package unsafe imported by %s; must end with _unsafe.go", path.Base(filename))
				}
			case "archive/zip":
				if PreferredZipImport != "" && pass.Pkg.Path() != PreferredZipImport {
					pass.Reportf(imp.Pos(), "import %s instead of archive/zip", PreferredZipImport)
				}
			}
		}
	}
	return nil, nil
}
