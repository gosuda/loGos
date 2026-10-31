// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/logos"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
)

// logosSpecializeHook runs clone specialization (M13-S3) on the checked
// package after lowering and reports whether the caller must recheck. The
// phase is neutral: it reads plain Go syntax and types, so Go and loGo/s
// packages take the same path, and it runs only for -d=logosspecialize.
func logosSpecializeHook(pkg *types2.Package, info *types2.Info, files []*syntax.File) bool {
	if !logos.SpecializeEnabled() || !logos.Specialize(pkg, info, files, logos.SpecializeCostCap()) {
		return false
	}
	for _, p := range types.PkgMap() {
		p.Direct = false
	}
	typecheck.Target.Imports = nil
	return true
}
