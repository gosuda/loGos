// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

func provenanceCheck(t *testing.T, path string, files map[string]string) (*types2.Package, *types2.Info) {
	t.Helper()
	ejectTarget(t)
	var parsed []*syntax.File
	for name, src := range files {
		base := syntax.NewFileBase(name)
		var errs []error
		f, _ := syntax.Parse(base, bytes.NewReader([]byte(src)), func(err error) { errs = append(errs, err) }, nil, 0)
		for _, err := range errs {
			t.Fatalf("parse %s: %v", name, err)
		}
		if f == nil {
			t.Fatalf("parse %s: nil file", name)
		}
		parsed = append(parsed, f)
	}
	slices.SortFunc(parsed, func(a, b *syntax.File) int {
		return strings.Compare(a.Pos().FileBase().Filename(), b.Pos().FileBase().Filename())
	})
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Instances:  make(map[*syntax.Name]types2.Instance),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
	}
	var checkErrs []error
	conf := types2.Config{
		Importer: newTestImporter(t),
		Error:    func(err error) { checkErrs = append(checkErrs, err) },
	}
	pkg, _ := conf.Check(path, parsed, info)
	for _, err := range checkErrs {
		t.Fatalf("check %s: %v", path, err)
	}
	return pkg, info
}

func provenanceNamed(t *testing.T, info *types2.Info, name string) types2.Type {
	t.Helper()
	for _, obj := range info.Defs {
		tn, ok := obj.(*types2.TypeName)
		if !ok || tn.Name() != name {
			continue
		}
		return tn.Type()
	}
	t.Fatalf("type %s not defined", name)
	return nil
}

func TestOwnedDeclsPhysicalNotVirtual(t *testing.T) {
	_, info := provenanceCheck(t, "example.com/mod/phys", map[string]string{
		"phys.go": "package phys\n\n//line virtual.go:100\ntype Virt struct{ X int }\n",
	})
	vt := provenanceNamed(t, info, "Virt")
	decls, ok := ownedDeclsForTypes(vt)
	if !ok {
		t.Fatalf("decls failed for virtual-mapped type")
	}
	if len(decls) == 0 {
		t.Fatalf("empty decls for named type")
	}
	for _, d := range decls {
		if d.File == "virtual.go" {
			t.Fatalf("virtual filename leaked into provenance: %+v", decls)
		}
	}
	found := false
	for _, d := range decls {
		if d.Owner == "example.com/mod/phys" && d.File == "phys.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("physical provenance missing: %+v", decls)
	}
}

func TestOwnedDeclsBasenameStripsDirs(t *testing.T) {
	_, info := provenanceCheck(t, "example.com/mod/dir", map[string]string{
		"/tmp/dir/c.go": "package dir\ntype C struct{ X int }\n",
	})
	ct := provenanceNamed(t, info, "C")
	decls, ok := ownedDeclsForTypes(ct)
	if !ok || len(decls) == 0 {
		t.Fatalf("decls failed: ok=%v decls=%+v", ok, decls)
	}
	for _, d := range decls {
		if strings.ContainsAny(d.File, `/\:`) {
			t.Fatalf("directory leaked into provenance file %q", d.File)
		}
	}
	found := false
	for _, d := range decls {
		if d.Owner == "example.com/mod/dir" && d.File == "c.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("basename normalization failed: %+v", decls)
	}
}

func TestOwnedDeclsTransitiveAcrossFiles(t *testing.T) {
	_, info := provenanceCheck(t, "example.com/mod/pkg", map[string]string{
		"a.go": "package pkg\ntype Inner struct{ X int }\n",
		"b.go": "package pkg\ntype Outer struct{ F Inner }\ntype AliasOuter = Outer\n",
	})
	ot := provenanceNamed(t, info, "Outer")
	decls, ok := ownedDeclsForTypes(ot)
	if !ok {
		t.Fatalf("decls failed for outer")
	}
	owners := map[string]bool{}
	files := map[string]bool{}
	for _, d := range decls {
		owners[d.Owner] = true
		files[d.File] = true
	}
	if !files["a.go"] || !files["b.go"] {
		t.Fatalf("transitive files missing: %+v", decls)
	}
	if len(owners) != 1 || !owners["example.com/mod/pkg"] {
		t.Fatalf("unexpected owners: %+v", decls)
	}
	at := provenanceNamed(t, info, "AliasOuter")
	adecls, ok := ownedDeclsForTypes(at)
	if !ok {
		t.Fatalf("decls failed for alias")
	}
	afiles := map[string]bool{}
	for _, d := range adecls {
		afiles[d.File] = true
	}
	if !afiles["a.go"] || !afiles["b.go"] {
		t.Fatalf("alias transitive files missing: %+v", adecls)
	}
}

func TestOwnedDeclsDistinctOwners(t *testing.T) {
	_, infoA := provenanceCheck(t, "example.com/a", map[string]string{
		"a.go": "package a\ntype A struct{ X int }\n",
	})
	_, infoB := provenanceCheck(t, "example.com/b", map[string]string{
		"b.go": "package b\ntype B struct{ Y string }\n",
	})
	at := provenanceNamed(t, infoA, "A")
	bt := provenanceNamed(t, infoB, "B")
	decls, ok := ownedDeclsForTypes(at, bt)
	if !ok {
		t.Fatalf("decls failed for cross-owner roots")
	}
	hasA, hasB := false, false
	for _, d := range decls {
		if d.Owner == "example.com/a" && d.File == "a.go" {
			hasA = true
		}
		if d.Owner == "example.com/b" && d.File == "b.go" {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Fatalf("cross-owner provenance missing: %+v", decls)
	}
}

func TestOwnedDeclsBuiltinsNeedNone(t *testing.T) {
	ejectTarget(t)
	decls, ok := ownedDeclsForTypes(types2.Typ[types2.Int], types2.Typ[types2.String])
	if !ok {
		t.Fatalf("builtins decls failed")
	}
	if decls == nil {
		t.Fatalf("nil decls (must be non-nil empty for codec)")
	}
	if len(decls) != 0 {
		t.Fatalf("builtins need no declaration, got %+v", decls)
	}
}

func TestOwnedDeclsCyclicTerminates(t *testing.T) {
	_, info := provenanceCheck(t, "example.com/mod/cyc", map[string]string{
		"c.go": "package cyc\ntype T struct{ Next *T }\n",
	})
	tt := provenanceNamed(t, info, "T")
	decls, ok := ownedDeclsForTypes(tt)
	if !ok {
		t.Fatalf("cyclic decls failed")
	}
	if len(decls) == 0 {
		t.Fatalf("empty decls for cyclic type")
	}
	for _, d := range decls {
		if d.Owner != "example.com/mod/cyc" || d.File != "c.go" {
			t.Fatalf("unexpected cyclic provenance: %+v", decls)
		}
	}
}

func TestOwnedDeclsGenericOriginNotInstantiationSite(t *testing.T) {
	_, info := provenanceCheck(t, "example.com/mod/gen", map[string]string{
		"a.go": "package gen\ntype G[T any] struct{ V T }\n",
		"b.go": "package gen\nvar _ G[int]\n",
	})
	var inst types2.Type
	for _, in := range info.Instances {
		if n, ok := in.Type.(*types2.Named); ok && n.Origin().Obj().Name() == "G" {
			inst = in.Type
			break
		}
	}
	if inst == nil {
		t.Fatalf("generic instance G[int] not found")
	}
	decls, ok := ownedDeclsForTypes(inst)
	if !ok {
		t.Fatalf("generic decls failed")
	}
	foundOrigin := false
	for _, d := range decls {
		if d.Owner == "example.com/mod/gen" && d.File == "a.go" {
			foundOrigin = true
		}
		if d.File == "b.go" {
			t.Fatalf("instantiation site must not supply declaration: %+v", decls)
		}
	}
	if !foundOrigin {
		t.Fatalf("generic origin provenance missing: %+v", decls)
	}
}
