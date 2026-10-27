// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"cmd/compile/internal/logos"
	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/types2"
)

// applyLogosReadonlyForGoImporter restores declaration-level facts
// (readonly capabilities, generic assertion effects) for a Go importer
// of an lgo package. The verified record f comes from the same decode
// that read this package's export data. All records stamp in place on the
// canonical objects: no clone, no packages replacement, so every importer
// shares one nominal identity through Go wrappers. Option and ConstView
// overlays stay lgo-only so Go keeps lowered semantics; only the readonly
// bit is published. Distinct anonymous objects keep distinct indexes by
// pointer-keyed export dedup, so in-place bits cannot leak across shapes.
func applyLogosReadonlyForGoImporter(path string, pkg *types2.Package, packages map[string]*types2.Package, f *facts.Facts) (*types2.Package, error) {
	if pkg == nil || f == nil {
		return pkg, nil
	}
	if err := logos.ApplyFuncFacts(pkg, f.Fns); err != nil {
		return nil, err
	}
	if err := logos.ApplyReadonlyFactsInPlace(pkg, f.Readonlys); err != nil {
		return nil, err
	}
	return pkg, nil
}

// applyLogosOverlayForLgoImporter rebuilds an imported loGo/s package
// with Option and ConstView types. The verified record f comes from the
// same decode. Declaration-only records are stamped in place (no clone,
// no packages replacement); structural records keep the clone path,
// which the dependency preload protects so both import orders share one
// Named/alias/instance origin. Go packages never clone and never gain
// Option/ConstView overlays: their verified assert/readonly effects stamp
// in place so both Go and loGo/s importers fail closed on forgery.
func applyLogosOverlayForLgoImporter(path string, pkg *types2.Package, packages map[string]*types2.Package, f *facts.Facts) (*types2.Package, error) {
	if pkg == nil || f == nil {
		return pkg, nil
	}
	if f.GoPackage {
		if err := logos.ApplyFuncFacts(pkg, f.Fns); err != nil {
			return nil, err
		}
		if err := logos.ApplyReadonlyFactsInPlace(pkg, f.Readonlys); err != nil {
			return nil, err
		}
		return pkg, nil
	}
	if !logosHasStructuralFacts(f) {
		if err := logos.ApplyFuncFacts(pkg, f.Fns); err != nil {
			return nil, err
		}
		if err := logos.ApplyReadonlyFactsInPlace(pkg, f.Readonlys); err != nil {
			return nil, err
		}
		return pkg, nil
	}
	cloned, err := logos.CloneWithOverlaysAndReadonly(pkg, f.Opts, f.Consts, f.Fns, f.Readonlys)
	if err != nil {
		return nil, err
	}
	if cloned != pkg && packages != nil {
		packages[path] = cloned
	}
	return cloned, nil
}
