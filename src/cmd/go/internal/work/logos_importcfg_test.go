// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"slices"
	"testing"

	"cmd/go/internal/cfg"
	"cmd/go/internal/load"
)

func ownerTestAction(path, file, archive string, deps ...*Action) *Action {
	pkg := new(load.Package)
	pkg.ImportPath = path
	pkg.GoFiles = []string{file}
	return &Action{Package: pkg, built: archive, Deps: deps}
}

func ownerTestPaths(actions []*Action) []string {
	paths := make([]string, len(actions))
	for i, a := range actions {
		paths[i] = a.Package.ImportPath
	}
	return paths
}

func TestLogosSynthImportsGoWrapperClosure(t *testing.T) {
	old := cfg.BuildN
	cfg.BuildN = false
	t.Cleanup(func() { cfg.BuildN = old })
	goLeaf := ownerTestAction("example/goleaf", "leaf.go", "leaf.a")
	privateOwner := ownerTestAction("example/private", "p.lgo", "p.a", goLeaf)
	owner := ownerTestAction("example/owner", "o.lgo", "o.a", privateOwner)
	direct := ownerTestAction("example/direct", "d.lgo", "d.a")
	unbuilt := ownerTestAction("example/unbuilt", "u.lgo", "")
	bridge := &Action{Deps: []*Action{owner, owner, direct, unbuilt}}
	wrapper := ownerTestAction("example/wrapper", "w.go", "w.a", bridge)
	root := ownerTestAction("example/main", "m.go", "m.a", wrapper, direct)
	bridge.Deps = append(bridge.Deps, root)
	got := ownerTestPaths(logosSynthImports(root))
	want := []string{"example/owner", "example/private"}
	if !slices.Equal(got, want) {
		t.Fatalf("transitive lgo owners = %v, want %v (deduplicated, sorted, direct/unbuilt/Go excluded)", got, want)
	}
}

func TestLogosSynthImportsLgoKeepsFullClosure(t *testing.T) {
	old := cfg.BuildN
	cfg.BuildN = false
	t.Cleanup(func() { cfg.BuildN = old })
	leaf := ownerTestAction("example/leaf", "leaf.go", "leaf.a")
	owner := ownerTestAction("example/owner", "owner.lgo", "owner.a", leaf)
	direct := ownerTestAction("example/direct", "direct.go", "direct.a", owner)
	root := ownerTestAction("example/main", "main.lgo", "main.a", direct)
	got := ownerTestPaths(logosSynthImports(root))
	want := []string{"example/leaf", "example/owner"}
	if !slices.Equal(got, want) {
		t.Fatalf("lgo synthesis closure = %v, want %v", got, want)
	}
}

func TestLogosSynthImportsOrdinaryGoAndDryRun(t *testing.T) {
	old := cfg.BuildN
	cfg.BuildN = false
	t.Cleanup(func() { cfg.BuildN = old })
	leaf := ownerTestAction("example/leaf", "leaf.go", "leaf.a")
	direct := ownerTestAction("example/direct", "direct.go", "direct.a", leaf)
	root := ownerTestAction("example/main", "main.go", "main.a", direct)
	if got := logosSynthImports(root); len(got) != 0 {
		t.Fatalf("ordinary Go compile changed: %v", ownerTestPaths(got))
	}
	if got := logosSynthImports(&Action{Deps: []*Action{root}}); len(got) != 0 {
		t.Fatal("non-package action acquired synthetic imports")
	}
	root.Package.GoFiles = []string{"main.lgo"}
	cfg.BuildN = true
	if got := logosSynthImports(root); len(got) != 0 {
		t.Fatal("dry run acquired unresolved archive paths")
	}
}
