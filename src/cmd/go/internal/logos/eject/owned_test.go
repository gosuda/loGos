// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package eject

import (
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"cmd/go/internal/load"
	"cmd/go/internal/modinfo"
	"cmd/internal/logosowned"
)

func ownedTestMainPackage(dir, importPath, name string) *load.Package {
	return &load.Package{
		PackagePublic: load.PackagePublic{
			ImportPath: importPath,
			Name:       name,
			Dir:        filepath.Join(dir, "sub"),
			Module:     &modinfo.ModulePublic{Path: "example.com/mod", Main: true, Dir: dir},
		},
	}
}

func ownedTestPlan(mainDir string, pkgs ...*load.Package) *ejectPlan {
	plan := &ejectPlan{
		main:       &ejectModule{dir: mainDir, pkgs: pkgs},
		depByPath:  map[string]*ejectModule{},
		known:      map[*load.Package]bool{},
		testBuilt:  map[*load.Package]bool{},
		provenance: map[string][]*load.Package{},
	}
	for _, p := range pkgs {
		plan.provenance[p.ImportPath] = append(plan.provenance[p.ImportPath], p)
	}
	return plan
}

func ownedTestRequest(t *testing.T, owner, pkg, source string, decls []logosowned.Declaration) logosowned.Request {
	t.Helper()
	if decls == nil {
		t.Fatalf("ownedTestRequest requires non-nil declarations (empty allowed)")
	}
	r, err := logosowned.NewRequest(owner, pkg, source, decls)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

func writeOwnedSidecar(t *testing.T, staging, dir string, reqs ...logosowned.Request) {
	t.Helper()
	data, err := logosowned.Encode(reqs)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(staging, dir), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, dir, logosowned.FileName), data, 0666); err != nil {
		t.Fatal(err)
	}
}

// ownedTestWriteBoth writes content to the original owner dir and the
// copied output dir so condition reads (original) and collision checks
// (copied) see genuinely selected files.
func ownedTestWriteBoth(t *testing.T, ownerDir, outDir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(ownerDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, name), []byte(content), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, name), []byte(content), 0666); err != nil {
		t.Fatal(err)
	}
}

// assertOwnedHelperCondTable requires the helper in dir declaring name
// to build exactly under want for every assignment of tags. Conditions
// are compared by semantics (Expr.Eval over the full truth table), never
// by spelling, so factored and distributive renderings both pass. A
// missing declaration fails the test; an unconditional helper holds
// under every assignment.
func assertOwnedHelperCondTable(t *testing.T, dir, name string, tags []string, want func(map[string]bool) bool) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "logos_owned_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	found := false
	for _, m := range matches {
		d, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(d), name) {
			data, found = d, true
			break
		}
	}
	if !found {
		t.Fatalf("helper declaring %s not found", name)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse helper: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatalf("header expr: %v", err)
	}
	holds := func(assign map[string]bool) bool {
		if expr == nil {
			return true
		}
		return expr.Eval(func(tag string) bool { return assign[tag] })
	}
	for bits := 0; bits < 1<<len(tags); bits++ {
		assign := map[string]bool{}
		for i, tag := range tags {
			assign[tag] = bits&(1<<i) != 0
		}
		if got := holds(assign); got != want(assign) {
			t.Fatalf("%s condition mismatch under %v: got %v", name, assign, got)
		}
	}
}

func TestDedupeOwnedDeterministic(t *testing.T) {
	a := ownedTestRequest(t, "example.com/mod/b", "b", "package b\nfunc A() {}\n", []logosowned.Declaration{})
	b := ownedTestRequest(t, "example.com/mod/a", "a", "package a\nfunc B() {}\n", []logosowned.Declaration{})
	got, err := dedupeOwnedRequests([]logosowned.Request{a, b, a})
	if err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if len(got) != 2 || got[0].Owner != "example.com/mod/a" || got[1].Owner != "example.com/mod/b" {
		t.Fatalf("not deduplicated and sorted: %#v", got)
	}
	clash := b
	clash.Source = "package a\nfunc C() {}\n"
	if _, err := dedupeOwnedRequests([]logosowned.Request{b, clash}); err == nil {
		t.Fatalf("dedupe accepted conflicting content for one id")
	}
	// Identical helpers owned by different packages are not a conflict:
	// each lands in its own owner directory, so only the same owner with
	// the same ID but different content is a loud conflict, never a
	// silent overwrite. Same owner with a different ID survives to pass
	// 2, where identical bodies differing only in build headers merge.
	foreign := a
	foreign.Owner = "example.com/mod/z"
	got, err = dedupeOwnedRequests([]logosowned.Request{a, foreign})
	if err != nil {
		t.Fatalf("dedupe rejected multi-owner helpers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("dedupe dropped a multi-owner helper: %#v", got)
	}
}

func TestDedupeKeepsDistinctProvenance(t *testing.T) {
	src := "package sub\n\nfunc Value() int { return 0 }\n"
	a := ownedTestRequest(t, "example.com/mod/sub", "sub", src, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "a.go"}})
	b := ownedTestRequest(t, "example.com/mod/sub", "sub", src, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "b.go"}})
	if a.ID != b.ID {
		t.Fatalf("same source must keep the same input hash, got %s vs %s", a.ID, b.ID)
	}
	got, err := dedupeOwnedRequests([]logosowned.Request{a, b})
	if err != nil {
		t.Fatalf("dedupe rejected distinct provenance: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("dedupe dropped distinct provenance: %#v", got)
	}
	dup := ownedTestRequest(t, "example.com/mod/sub", "sub", src, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "a.go"}})
	got, err = dedupeOwnedRequests([]logosowned.Request{a, dup})
	if err != nil {
		t.Fatalf("dedupe: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("identical provenance did not deduplicate: %#v", got)
	}
}

func TestGatherOwnedRequests(t *testing.T) {
	staging := t.TempDir()
	a := ownedTestRequest(t, "example.com/mod/a", "a", "package a\nfunc A() {}\n", []logosowned.Declaration{})
	b := ownedTestRequest(t, "example.com/mod/b", "b", "package b\nfunc B() {}\n", []logosowned.Declaration{})
	writeOwnedSidecar(t, staging, "example.com/mod/a", a)
	writeOwnedSidecar(t, staging, "example.com/mod/b", b)
	if err := os.MkdirAll(filepath.Join(staging, "empty"), 0777); err != nil {
		t.Fatal(err)
	}
	got, err := gatherOwnedRequests(staging)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("gathered %d requests", len(got))
	}
	if err := os.WriteFile(filepath.Join(staging, logosowned.FileName), []byte("{bad"), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := gatherOwnedRequests(staging); err == nil {
		t.Fatalf("gather accepted a malformed sidecar")
	}
}

func TestEmitOwnedHelperMainModule(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"a.go"}
	if err := os.MkdirAll(owner.Dir, 0777); err != nil {
		t.Fatal(err)
	}
	orig := "package sub\n\nfunc Existing() int { return 1 }\n"
	if err := os.WriteFile(filepath.Join(owner.Dir, "a.go"), []byte(orig), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	req := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nimport \"logos/rt\"\n\n// Generated is owned.\nfunc Generated() int { return rt.Zero() }\n", []logosowned.Declaration{})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
	usedRT, err := emitOwnedHelpers(plan, staging, out, map[string]string{})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !usedRT {
		t.Fatalf("emit did not propagate support-module use")
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 helper in out/sub, got %v (err: %v)", matches, err)
	}
	dst := matches[0]
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if !strings.Contains(string(data), rtModulePath) {
		t.Fatalf("helper lost the remapped support import")
	}
	if strings.Contains(dst, ".gen.go") {
		t.Fatalf("helper name must not use .gen.go")
	}
	// The input module is untouched: same bytes, no new files.
	if got, err := os.ReadFile(filepath.Join(owner.Dir, "a.go")); err != nil || string(got) != orig {
		t.Fatalf("input source mutated")
	}
	var extra []string
	_ = filepath.WalkDir(modDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(path) != "a.go" {
			extra = append(extra, path)
		}
		return nil
	})
	if len(extra) != 0 {
		t.Fatalf("input module gained files: %v", extra)
	}
}

func TestEmitOwnedCollisions(t *testing.T) {
	setup := func(t *testing.T, src string) (*ejectPlan, string, string, logosowned.Request) {
		modDir := t.TempDir()
		out := t.TempDir()
		owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
		owner.GoFiles = []string{"a.go"}
		if err := os.MkdirAll(owner.Dir, 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(owner.Dir, "a.go"), []byte(src), 0666); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(out, "sub"), 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "sub", "a.go"), []byte(src), 0666); err != nil {
			t.Fatal(err)
		}
		staging := t.TempDir()
		return ownedTestPlan(modDir, owner), staging, out, ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\nfunc Clashing() {}\n", []logosowned.Declaration{})
	}
	t.Run("source", func(t *testing.T) {
		plan, staging, out, req := setup(t, "package sub\nfunc Clashing() {}\n")
		writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
		if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err == nil {
			t.Fatalf("emit accepted a collision with copied source")
		}
	})
	t.Run("generated", func(t *testing.T) {
		plan, staging, out, req := setup(t, "package sub\nfunc Other() {}\n")
		// Same declared identifier with a genuinely different body, so
		// the IDs differ and the coordinator must report a conflict
		// instead of deduplicating.
		other := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\nfunc Clashing() int { return 1 }\n", []logosowned.Declaration{})
		writeOwnedSidecar(t, staging, "example.com/mod/sub", req, other)
		if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err == nil {
			t.Fatalf("emit accepted conflicting generated declarations")
		}
	})
	// Copied source may itself carry a generated-looking file name: the
	// prefix is not ownership. A declaration in it must collide with a
	// generated helper whose output file name differs.
	t.Run("source generated prefix", func(t *testing.T) {
		plan, staging, out, req := setup(t, "package sub\nfunc Clashing() {}\n")
		renamed := "logos_owned_user.go"
		data, err := os.ReadFile(filepath.Join(out, "sub", "a.go"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(out, "sub", "a.go")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "sub", renamed), data, 0666); err != nil {
			t.Fatal(err)
		}
		writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
		_, err = emitOwnedHelpers(plan, staging, out, map[string]string{})
		if err == nil {
			t.Fatalf("emit accepted a collision hidden by a generated-looking source file name")
		}
	})
	// External test files of the owner directory live in a different
	// package namespace and must not collide with package declarations.
	t.Run("external test namespace", func(t *testing.T) {
		plan, staging, out, req := setup(t, "package sub\nfunc Other() {}\n")
		if err := os.WriteFile(filepath.Join(out, "sub", "ext_test.go"), []byte("package sub_test\n\nfunc Clashing() {}\n"), 0666); err != nil {
			t.Fatal(err)
		}
		writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
		if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
			t.Fatalf("emit rejected a legal external-test declaration: %v", err)
		}
	})
}

func TestEmitOwnedRejectsStdlib(t *testing.T) {
	plan := ownedTestPlan(t.TempDir())
	staging := t.TempDir()
	req := ownedTestRequest(t, "net/http", "http", "package http\nfunc Owned() {}\n", []logosowned.Declaration{})
	writeOwnedSidecar(t, staging, "net/http", req)
	_, err := emitOwnedHelpers(plan, staging, t.TempDir(), map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "net/http") {
		t.Fatalf("stdlib owner must fail naming the destination, got %v", err)
	}
}

func TestEmitOwnedOutsideModule(t *testing.T) {
	modDir := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.Dir = t.TempDir()
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	writeOwnedSidecar(t, staging, "example.com/mod/sub", ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\nfunc X() {}\n", []logosowned.Declaration{}))
	if _, err := emitOwnedHelpers(plan, staging, t.TempDir(), map[string]string{}); err == nil {
		t.Fatalf("emit accepted an owner outside its module")
	}
}

func TestOwnedDeclNamesDuplicates(t *testing.T) {
	if _, err := ownedDeclNames("example.com/mod/sub", []byte("package sub\nfunc Same() {}\nfunc Same() {}\n")); err == nil {
		t.Fatalf("collector accepted duplicate declarations in one helper")
	}
	// Repeated package-level init functions are legal Go.
	got, err := ownedDeclNames("example.com/mod/sub", []byte("package sub\nfunc init() {}\nfunc init() {}\nfunc Distinct() {}\n"))
	if err != nil {
		t.Fatalf("collector rejected legal init functions: %v", err)
	}
	if len(got) != 1 || got[0] != "Distinct" {
		t.Fatalf("collector tracked init: %#v", got)
	}
	// Repeated blank identifiers are legal Go.
	if _, err := ownedDeclNames("example.com/mod/sub", []byte("package sub\nvar _ = 1\nvar _ = 2\nfunc Distinct() {}\n")); err != nil {
		t.Fatalf("collector rejected legal blank identifiers: %v", err)
	}
}

func TestOwnedCheckDir(t *testing.T) {
	root := t.TempDir()
	if err := ownedCheckDir(root, filepath.Join(root, "sub")); err != nil {
		t.Fatalf("fresh dir rejected: %v", err)
	}
	if err := ownedCheckDir(root, filepath.Join(root, "..", "escape")); err == nil {
		t.Fatalf("escape accepted")
	}
	linkTarget := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatal(err)
	}
	if err := ownedCheckDir(root, filepath.Join(link, "sub")); err == nil {
		t.Fatalf("symlink accepted")
	}
}

func TestFindOwnedOwnerPrefersRealPackage(t *testing.T) {
	modDir := t.TempDir()
	real := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	variant := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	variant.ForTest = "sub"
	plan := ownedTestPlan(modDir, variant, real)
	if got := findOwnedOwner(plan, "example.com/mod/sub"); got != real {
		t.Fatalf("resolved to test variant")
	}
	if got := findOwnedOwner(plan, "example.com/mod/missing"); got != nil {
		t.Fatalf("resolved an unknown owner")
	}
}

func TestEmitOwnedIncludesDepModule(t *testing.T) {
	modDir := t.TempDir()
	depDir := t.TempDir()
	out := t.TempDir()
	owner := &load.Package{
		PackagePublic: load.PackagePublic{
			ImportPath: "example.com/dep/pkg",
			Name:       "pkg",
			Dir:        filepath.Join(depDir, "pkg"),
			Module:     &modinfo.ModulePublic{Path: "example.com/dep", Version: "v1.0.0", Dir: depDir},
		},
	}
	if err := os.MkdirAll(owner.Dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "go.mod"), []byte("module example.com/dep\n\ngo 1.27\n"), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir)
	plan.depByPath["example.com/dep"] = &ejectModule{path: "example.com/dep", version: "v1.0.0", dir: depDir, pkgs: []*load.Package{owner}}
	plan.provenance["example.com/dep/pkg"] = []*load.Package{owner}
	staging := t.TempDir()
	req := ownedTestRequest(t, "example.com/dep/pkg", "pkg", "package pkg\nfunc Owned() {}\n", []logosowned.Declaration{})
	writeOwnedSidecar(t, staging, "example.com/dep/pkg", req)
	ejected := map[string]string{}
	if _, err := emitOwnedHelpers(plan, staging, out, ejected); err != nil {
		t.Fatalf("emit: %v", err)
	}
	dst, ok := ejected["example.com/dep"]
	if !ok {
		t.Fatalf("dependency module was not registered")
	}
	matches, err := filepath.Glob(filepath.Join(dst, "pkg", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 helper in copied dep, got %v (err: %v)", matches, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "go.mod")); err != nil {
		t.Fatalf("copied module incomplete: %v", err)
	}
	found := false
	for _, m := range plan.deps {
		if m.path == "example.com/dep" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dependency module missing from plan for go.mod rewriting")
	}
}

func TestEmitOwnedExplicitProvenance(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"private_amd64.go", "tagged_linux.go", "plain.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "private_amd64.go", "package sub\n\ntype privateKey struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "tagged_linux.go", "//go:build linux\n\npackage sub\n\ntype linuxKey struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "plain.go", "package sub\n\nfunc Free() int { return 1 }\n")

	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()

	body := "package sub\n\nfunc UsePlatform() *privateKey { p := new(privateKey); return p }\n"
	// Same body, distinct owner file is impossible here (same owner) so
	// distinct consumer alternatives share the body: windows vs darwin
	// consumers both need the amd64 declaration.
	winSrc := "//go:build windows\n\n" + body
	darSrc := "//go:build darwin\n\n" + body
	winReq, err := logosowned.NewRequest("example.com/mod/sub", "sub", winSrc, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "private_amd64.go"}})
	if err != nil {
		t.Fatal(err)
	}
	darReq, err := logosowned.NewRequest("example.com/mod/sub", "sub", darSrc, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "private_amd64.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if winReq.ID == darReq.ID {
		t.Fatalf("different consumer headers must give different input IDs")
	}
	// Same input Source hash with different provenance survives dedupe:
	// winReq and darReq differ in Source (headers) so IDs differ; the
	// same-Source case is covered by TestDedupeKeepsDistinctProvenance.
	// Here the same body must merge to one output with OR consumers.
	linuxReq := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseLinux() *linuxKey { k := new(linuxKey); return k }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "tagged_linux.go"}})
	freeReq := ownedTestRequest(t, "example.com/mod/sub", "sub", "//go:build js\n\npackage sub\n\nfunc UseFree() int { return Free() }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "plain.go"}})

	writeOwnedSidecar(t, staging, "example.com/mod/sub", winReq, darReq, linuxReq, freeReq)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	headerOf := func(funcOrTypeName string) string {
		t.Helper()
		matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), funcOrTypeName) {
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
				if err != nil {
					t.Fatalf("parse helper: %v", err)
				}
				expr, err := ownedHeaderExpr(f)
				if err != nil {
					t.Fatalf("header expr: %v", err)
				}
				if expr == nil {
					return ""
				}
				return expr.String()
			}
		}
		t.Fatalf("helper declaring %s not found", funcOrTypeName)
		return ""
	}

	// Per-request consumer AND declaration, then OR across the body
	// group: (windows && amd64) || (darwin && amd64). The rendering may
	// factor amd64 out or leave it distributive; the truth table pins
	// the semantics instead: build exactly when amd64 holds with at
	// least one of windows/darwin.
	assertOwnedHelperCondTable(t, filepath.Join(out, "sub"), "UsePlatform",
		[]string{"windows", "darwin", "amd64"},
		func(assign map[string]bool) bool {
			return assign["amd64"] && (assign["windows"] || assign["darwin"])
		})
	if got := headerOf("UseLinux"); got != "linux" {
		t.Fatalf("UseLinux condition = %q, want linux", got)
	}
	// Unrelated owner files do not constrain: plain.go is
	// unconditional, so the js consumer stays js.
	if got := headerOf("UseFree"); got != "js" {
		t.Fatalf("UseFree condition = %q, want js", got)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 3 {
		t.Fatalf("expected 3 helpers (platform variants merged), got %d: %v", len(matches), matches)
	}
}

func TestEmitOwnedDistinctDeclarationAlternatives(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"a_amd64.go", "b_linux.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "a_amd64.go", "package sub\n\ntype aKey struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "b_linux.go", "package sub\n\ntype bKey struct{ v int }\n")
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	// Same body, same consumer, distinct provenance: effective
	// conditions are amd64 vs linux, ORed to one output.
	bodyA := "package sub\n\nfunc Alt() *aKey { return new(aKey) }\n"
	bodyB := "package sub\n\nfunc Alt() *bKey { return new(bKey) }\n"
	_ = bodyA
	_ = bodyB
	// Same stripped body with different declarations: use one body
	// shape that can reference either file via distinct helpers? For a
	// true same-body OR, both variants must share the body text, so use
	// a body that type-checks against either via the declaration list:
	// here both variants share the exact body and differ only in
	// provenance.
	shared := "package sub\n\nfunc Alt() int { return 1 }\n"
	reqA := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "a_amd64.go"}})
	reqB := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "b_linux.go"}})
	if reqA.ID != reqB.ID {
		t.Fatalf("shared body must share input hash, got %s vs %s", reqA.ID, reqB.ID)
	}
	writeOwnedSidecar(t, staging, "example.com/mod/sub", reqA, reqB)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 merged helper, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse helper: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatal(err)
	}
	if expr == nil {
		t.Fatalf("merged alternatives lost both conditions (got unconditional)")
	}
	got := expr.String()
	if got != "amd64 || linux" && got != "linux || amd64" {
		t.Fatalf("merged condition = %q, want amd64 || linux", got)
	}
}

func TestEmitOwnedPlatformFilenameTags(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"feat_linux.go", "arch_amd64.go", "plain.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "feat_linux.go", "package sub\n\ntype linuxFeat struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "arch_amd64.go", "package sub\n\ntype amd64Feat struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "plain.go", "package sub\n\nfunc Plain() int { return 1 }\n")
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	linReq := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseLinuxFeat() *linuxFeat { return new(linuxFeat) }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "feat_linux.go"}})
	amdReq := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseAmd64Feat() *amd64Feat { return new(amd64Feat) }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "arch_amd64.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", linReq, amdReq)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	find := func(name string) string {
		t.Helper()
		matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), name) {
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				expr, err := ownedHeaderExpr(f)
				if err != nil {
					t.Fatal(err)
				}
				if expr == nil {
					return ""
				}
				return expr.String()
			}
		}
		t.Fatalf("missing helper %s", name)
		return ""
	}
	if got := find("UseLinuxFeat"); got != "linux" {
		t.Fatalf("filename GOOS condition = %q, want linux", got)
	}
	if got := find("UseAmd64Feat"); got != "amd64" {
		t.Fatalf("filename GOARCH condition = %q, want amd64", got)
	}
}

func TestEmitOwnedCrossOwnerRefs(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	ownerA := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	ownerA.Dir = filepath.Join(modDir, "sub")
	ownerA.GoFiles = []string{"a.go"}
	ownerB := &load.Package{
		PackagePublic: load.PackagePublic{
			ImportPath: "example.com/mod/other",
			Name:       "other",
			Dir:        filepath.Join(modDir, "other"),
			Module:     &modinfo.ModulePublic{Path: "example.com/mod", Main: true, Dir: modDir},
		},
	}
	ownerB.GoFiles = []string{"cond_linux.go"}
	if err := os.MkdirAll(ownerA.Dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ownerB.Dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerA.Dir, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	condOther := "//go:build linux\n\npackage other\n\ntype OtherKey struct{ V int }\n"
	if err := os.WriteFile(filepath.Join(ownerB.Dir, "cond_linux.go"), []byte(condOther), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir, ownerA, ownerB)
	outSub := filepath.Join(out, "sub")
	if err := os.MkdirAll(outSub, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	req := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseOther() int { return 1 }\n", []logosowned.Declaration{{Owner: "example.com/mod/other", File: "cond_linux.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 helper, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "example.com/mod/other") {
		t.Fatalf("cross-owner helper added a dependency edge")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatal(err)
	}
	if expr == nil || expr.String() != "linux" {
		t.Fatalf("cross-owner condition = %v, want linux", expr)
	}
}

func TestEmitOwnedTestOnly(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"a.go"}
	owner.TestGoFiles = []string{"helper_test.go"}
	ownerDir := owner.Dir
	if err := os.MkdirAll(ownerDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "helper_test.go"), []byte("package sub\n\ntype testKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	outSub := filepath.Join(out, "sub")
	if err := os.MkdirAll(outSub, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "helper_test.go"), []byte("package sub\n\ntype testKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir, owner)
	plan.testBuilt[owner] = true
	staging := t.TempDir()
	testReq := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseTest() *testKey { return new(testKey) }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "helper_test.go"}})
	plainReq := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UsePlain() int { return A() }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "a.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", testReq, plainReq)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 helpers, got %v", matches)
	}
	var testOut, plainOut string
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "UseTest") {
			testOut = m
		}
		if strings.Contains(string(data), "UsePlain") {
			plainOut = m
		}
	}
	if testOut == "" || plainOut == "" {
		t.Fatalf("missing test or plain helper in %v", matches)
	}
	if !strings.HasSuffix(testOut, "_test.go") {
		t.Fatalf("test-only declaration must select a _test.go output, got %s", testOut)
	}
	if strings.HasSuffix(plainOut, "_test.go") {
		t.Fatalf("caller-test independence: non-test declaration must not select _test.go, got %s", plainOut)
	}
}

func TestEmitOwnedLogosAliasExtension(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"feat_linux.⚡", "plain.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "plain.go", "package sub\n\nfunc Plain() int { return 1 }\n")
	if err := os.MkdirAll(ownerDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "feat_linux.⚡"), []byte("package sub\n\ntype aliasFeat struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	// Copied output normalizes the alias to Go form; the file itself
	// is skipped by the collision scan (not .go) which is correct.
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	req := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseAlias() *aliasFeat { return new(aliasFeat) }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "feat_linux.⚡"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 helper, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatal(err)
	}
	if expr == nil || expr.String() != "linux" {
		t.Fatalf("alias Logos filename must imply linux, got %v", expr)
	}
	// Selected-file rejection is preserved: an alias basename outside
	// the plan is still an explicit error, not a silent skip.
	bad := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseBad() int { return 1 }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "missing.⚡"}})
	badDir := t.TempDir()
	writeOwnedSidecar(t, badDir, "example.com/mod/sub", bad)
	if _, err := emitOwnedHelpers(plan, badDir, out, map[string]string{}); err == nil {
		t.Fatalf("unselected alias file accepted")
	}
}

func TestEmitOwnedMixedProdTestGroupKeepsProduction(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"a.go"}
	owner.TestGoFiles = []string{"helper_test.go"}
	ownerDir := owner.Dir
	if err := os.MkdirAll(ownerDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "helper_test.go"), []byte("package sub\n\ntype testKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	outSub := filepath.Join(out, "sub")
	if err := os.MkdirAll(outSub, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "helper_test.go"), []byte("package sub\n\ntype testKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir, owner)
	plan.testBuilt[owner] = true
	staging := t.TempDir()
	shared := "package sub\n\nfunc Mixed() int { return 1 }\n"
	prod := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "a.go"}})
	testOnly := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "helper_test.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", prod, testOnly)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("mixed group must keep one production writer, got %v", matches)
	}
	if strings.HasSuffix(matches[0], "_test.go") {
		t.Fatalf("mixed production/test-only group must not collapse to _test.go, got %s", matches[0])
	}
}

func TestEmitOwnedMixedDistinctConditions(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"p.go"}
	owner.TestGoFiles = []string{"t_test.go"}
	ownerDir := owner.Dir
	if err := os.MkdirAll(ownerDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "p.go"), []byte("//go:build prodtag\n\npackage sub\n\ntype prodKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ownerDir, "t_test.go"), []byte("//go:build testtag\n\npackage sub\n\ntype testKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	outSub := filepath.Join(out, "sub")
	if err := os.MkdirAll(outSub, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "p.go"), []byte("//go:build prodtag\n\npackage sub\n\ntype prodKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outSub, "t_test.go"), []byte("//go:build testtag\n\npackage sub\n\ntype testKey struct{ v int }\n"), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir, owner)
	plan.testBuilt[owner] = true
	staging := t.TempDir()
	shared := "package sub\n\nfunc MixedPT() int { return 1 }\n"
	prod := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "p.go"}})
	testOnly := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "t_test.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", prod, testOnly)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 2 {
		t.Fatalf("distinct prod/test conditions must emit two files, got %v", matches)
	}
	var prodFile, testFile string
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			testFile = m
		} else {
			prodFile = m
		}
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "MixedPT") {
			t.Fatalf("split writer lost body in %s", m)
		}
	}
	if prodFile == "" || testFile == "" {
		t.Fatalf("need one .go and one _test.go, got %v", matches)
	}
	evalTags := func(path string, tags map[string]bool) bool {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		expr, err := ownedHeaderExpr(f)
		if err != nil {
			t.Fatal(err)
		}
		if expr == nil {
			return true
		}
		return expr.Eval(func(tag string) bool { return tags[tag] })
	}
	if !evalTags(prodFile, map[string]bool{"prodtag": true}) {
		t.Fatalf("production file must build when prodtag holds")
	}
	if evalTags(prodFile, map[string]bool{"prodtag": false}) {
		t.Fatalf("production file must not build without prodtag")
	}
	if evalTags(testFile, map[string]bool{"prodtag": true, "testtag": true}) {
		t.Fatalf("test file must not build when P holds (duplicate)")
	}
	if !evalTags(testFile, map[string]bool{"prodtag": false, "testtag": true}) {
		t.Fatalf("test file must build when P fails and T holds")
	}
	if evalTags(testFile, map[string]bool{"prodtag": false, "testtag": false}) {
		t.Fatalf("test file must not build when neither holds")
	}
}

func TestEmitOwnedNilOr(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"gated.go", "plain.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "gated.go", "//go:build linux\n\npackage sub\n\ntype gatedKey struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "plain.go", "package sub\n\nfunc Plain() int { return 1 }\n")
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	shared := "package sub\n\nfunc Alt() int { return 1 }\n"
	uncond := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{})
	gated := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "gated.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", uncond, gated)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 merged helper, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatal(err)
	}
	if expr != nil {
		t.Fatalf("unconditional OR must stay unconditional, got %q", expr.String())
	}
}

func TestEmitOwnedMissingUnselected(t *testing.T) {
	setup := func(t *testing.T) (*ejectPlan, *load.Package, string, string) {
		modDir := t.TempDir()
		out := t.TempDir()
		owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
		owner.GoFiles = []string{"a.go"}
		owner.IgnoredGoFiles = []string{"ignored.go"}
		owner.CompiledGoFiles = []string{"gen_cgo.go"}
		if err := os.MkdirAll(owner.Dir, 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(owner.Dir, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
			t.Fatal(err)
		}
		outSub := filepath.Join(out, "sub")
		if err := os.MkdirAll(outSub, 0777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outSub, "a.go"), []byte("package sub\n\nfunc A() int { return 1 }\n"), 0666); err != nil {
			t.Fatal(err)
		}
		return ownedTestPlan(modDir, owner), owner, t.TempDir(), out
	}
	emitOne := func(t *testing.T, plan *ejectPlan, staging, out string, decl logosowned.Declaration) error {
		req := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc Use() int { return 1 }\n", []logosowned.Declaration{decl})
		// Use a fresh staging dir per case so sidecars do not mix.
		dir := t.TempDir()
		writeOwnedSidecar(t, dir, "example.com/mod/sub", req)
		_, err := emitOwnedHelpers(plan, dir, out, map[string]string{})
		return err
	}
	t.Run("missing file", func(t *testing.T) {
		plan, _, staging, out := setup(t)
		if err := emitOne(t, plan, staging, out, logosowned.Declaration{Owner: "example.com/mod/sub", File: "missing.go"}); err == nil {
			t.Fatalf("missing file accepted")
		}
	})
	t.Run("unselected file", func(t *testing.T) {
		plan, _, staging, out := setup(t)
		if err := emitOne(t, plan, staging, out, logosowned.Declaration{Owner: "example.com/mod/sub", File: "other.go"}); err == nil {
			t.Fatalf("unselected file accepted")
		}
	})
	t.Run("ignored file", func(t *testing.T) {
		plan, _, staging, out := setup(t)
		if err := emitOne(t, plan, staging, out, logosowned.Declaration{Owner: "example.com/mod/sub", File: "ignored.go"}); err == nil {
			t.Fatalf("ignored file accepted")
		}
	})
	t.Run("generated cgo", func(t *testing.T) {
		plan, _, staging, out := setup(t)
		err := emitOne(t, plan, staging, out, logosowned.Declaration{Owner: "example.com/mod/sub", File: "gen_cgo.go"})
		if err == nil {
			t.Fatalf("generated cgo mapping gap silently excluded")
		}
		if !strings.Contains(err.Error(), "gen_cgo.go") || !strings.Contains(err.Error(), "CompiledGoFiles") {
			t.Fatalf("cgo gap must report exact fields, got %v", err)
		}
	})
	t.Run("unknown owner", func(t *testing.T) {
		plan, _, staging, out := setup(t)
		if err := emitOne(t, plan, staging, out, logosowned.Declaration{Owner: "example.com/mod/unknown", File: "a.go"}); err == nil {
			t.Fatalf("unknown owner accepted")
		}
	})
	t.Run("empty inventory", func(t *testing.T) {
		plan, _, staging, out := setup(t)
		empty := &load.Package{
			PackagePublic: load.PackagePublic{
				ImportPath: "example.com/empty/pkg",
				Name:       "pkg",
				Dir:        t.TempDir(),
				Module:     &modinfo.ModulePublic{Path: "example.com/empty", Main: false, Dir: t.TempDir(), Version: "v0.0.0"},
			},
		}
		plan.provenance["example.com/empty/pkg"] = []*load.Package{empty}
		if err := emitOne(t, plan, staging, out, logosowned.Declaration{Owner: "example.com/empty/pkg", File: "a.go"}); err == nil {
			t.Fatalf("empty inventory accepted (all-files scan forbidden)")
		}
	})
	t.Run("same hash different provenance merges", func(t *testing.T) {
		modDir := t.TempDir()
		out := t.TempDir()
		owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
		owner.GoFiles = []string{"x.go", "y.go"}
		ownerDir := owner.Dir
		outSub := filepath.Join(out, "sub")
		ownedTestWriteBoth(t, ownerDir, outSub, "x.go", "package sub\n\ntype xKey struct{ v int }\n")
		ownedTestWriteBoth(t, ownerDir, outSub, "y.go", "//go:build linux\n\npackage sub\n\ntype yKey struct{ v int }\n")
		plan := ownedTestPlan(modDir, owner)
		staging := t.TempDir()
		shared := "package sub\n\nfunc Same() int { return 1 }\n"
		rx := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "x.go"}})
		ry := ownedTestRequest(t, "example.com/mod/sub", "sub", shared, []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "y.go"}})
		if rx.ID != ry.ID {
			t.Fatalf("shared source must share input hash")
		}
		writeOwnedSidecar(t, staging, "example.com/mod/sub", rx, ry)
		if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
			t.Fatalf("emit: %v", err)
		}
		matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("same hash variants must merge to one output, got %v", matches)
		}
		data, err := os.ReadFile(matches[0])
		if err != nil {
			t.Fatal(err)
		}
		// The unconditional x.go declaration leaves one alternative
		// unconditional, so the OR stays unconditional.
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		expr, err := ownedHeaderExpr(f)
		if err != nil {
			t.Fatal(err)
		}
		if expr != nil {
			t.Fatalf("expected unconditional merge (x unconditional OR y linux), got %q", expr.String())
		}
		// Output filename must hash the actual final bytes. The merged,
		// formatted output can legitimately equal the input Source, so
		// the output hash may coincide with the input ID: verify the
		// name against the bytes on disk instead of requiring
		// inequality.
		sum := sha256.Sum256(data)
		if want := "logos_owned_" + hex.EncodeToString(sum[:]) + ".go"; filepath.Base(matches[0]) != want {
			t.Fatalf("output name %q must hash the final bytes, want %q", matches[0], want)
		}
	})
}

func TestEmitOwnedBuildCondition(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"private_amd64.go", "tagged_linux.go", "plain.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "private_amd64.go", "package sub\n\ntype privateKey struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "tagged_linux.go", "//go:build linux\n\npackage sub\n\ntype linuxKey struct{ v int }\n")
	ownedTestWriteBoth(t, ownerDir, outSub, "plain.go", "package sub\n\nfunc Free() int { return 1 }\n")

	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()

	// 1 & 2: Same body, different consumer headers -> OR'd consumer condition, ANDed with amd64.
	// Single output file emitted for the body group.
	reqPlatformWin, err := logosowned.NewRequest("example.com/mod/sub", "sub", "//go:build windows\n\npackage sub\n\nfunc UsePlatform() *privateKey { p := new(privateKey); return p }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "private_amd64.go"}})
	if err != nil {
		t.Fatal(err)
	}
	reqPlatformDar, err := logosowned.NewRequest("example.com/mod/sub", "sub", "//go:build darwin\n\npackage sub\n\nfunc UsePlatform() *privateKey { p := new(privateKey); return p }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "private_amd64.go"}})
	if err != nil {
		t.Fatal(err)
	}

	// 3: Untagged consumer references linuxKey in tagged_linux.go -> linux.
	reqLinux := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseLinux() *linuxKey { k := new(linuxKey); return k }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "tagged_linux.go"}})

	// 4: Tagged consumer references untagged Free() -> stays js (unrelated owner files don't restrict).
	reqFree := ownedTestRequest(t, "example.com/mod/sub", "sub", "//go:build js\n\npackage sub\n\nfunc UseFree() int { return Free() }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "plain.go"}})

	writeOwnedSidecar(t, staging, "example.com/mod/sub", reqPlatformWin, reqPlatformDar, reqLinux, reqFree)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	headerOf := func(funcOrTypeName string) string {
		t.Helper()
		matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), funcOrTypeName) {
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
				if err != nil {
					t.Fatalf("parse helper: %v", err)
				}
				expr, err := ownedHeaderExpr(f)
				if err != nil {
					t.Fatalf("header expr: %v", err)
				}
				if expr == nil {
					return ""
				}
				return expr.String()
			}
		}
		t.Fatalf("helper declaring %s not found in %v", funcOrTypeName, funcOrTypeName)
		return ""
	}

	// Request 1 and 2 merged: each consumer condition ANDed with
	// privateKey's amd64, ORed across the body group. The rendering may
	// factor amd64 out or leave it distributive; the truth table pins
	// the semantics instead.
	assertOwnedHelperCondTable(t, filepath.Join(out, "sub"), "UsePlatform",
		[]string{"windows", "darwin", "amd64"},
		func(assign map[string]bool) bool {
			return assign["amd64"] && (assign["windows"] || assign["darwin"])
		})

	// Request 3: linuxKey from tagged_linux.go.
	if got := headerOf("UseLinux"); got != "linux" {
		t.Fatalf("UseLinux condition = %q, want linux", got)
	}

	// Request 4: Free from plain.go (unconditional) -> stays js.
	if got := headerOf("UseFree"); got != "js" {
		t.Fatalf("UseFree condition = %q, want js (unrelated owner declarations must not restrict)", got)
	}

	// Verify only 3 files emitted (reqPlatformWin and reqPlatformDar merged into 1).
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 3 {
		t.Fatalf("expected exactly 3 helper files (variants merged), got %d: %v", len(matches), matches)
	}
}

func TestEmitOwnedCgoCondition(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.CgoFiles = []string{"cgo_linux.go"}
	owner.GoFiles = []string{"plain.go"}
	ownerDir := owner.Dir
	outSub := filepath.Join(out, "sub")
	ownedTestWriteBoth(t, ownerDir, outSub, "plain.go", "package sub\n\nfunc Plain() int { return 1 }\n")
	cgoSrc := "package sub\n\nimport \"C\"\n\ntype cgoKey struct{ v int }\n"
	ownedTestWriteBoth(t, ownerDir, outSub, "cgo_linux.go", "//go:build linux\n\n"+cgoSrc)
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	req := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc UseCgo() int { return 1 }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "cgo_linux.go"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 helper, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatal(err)
	}
	if expr == nil {
		t.Fatalf("cgo declaration lost its condition")
	}
	got := expr.String()
	if !strings.Contains(got, "linux") || !strings.Contains(got, "cgo") {
		t.Fatalf("cgo condition = %q, want linux and cgo", got)
	}
	_ = slices.Clone(owner.GoFiles)
}

func TestEmitOwnedLogosHeaderCondition(t *testing.T) {
	modDir := t.TempDir()
	out := t.TempDir()
	owner := ownedTestMainPackage(modDir, "example.com/mod/sub", "sub")
	owner.GoFiles = []string{"calc.lgo"}
	ownerDir := owner.Dir
	if err := os.MkdirAll(ownerDir, 0777); err != nil {
		t.Fatal(err)
	}
	// Real calc.lgo pattern: package clause, imports, build tags, and Logos const func bodies
	calcSource := `//go:build linux
package sub

import "example.com/constlib/transitive"

const privateBase = 10

type privateFactor int

const func privateHelper(x int, f privateFactor) int {
	return x*int(f) + privateBase
}

const func ComputeClosed(x int) int {
	return privateHelper(x, privateFactor(3))
}
`
	if err := os.WriteFile(filepath.Join(ownerDir, "calc.lgo"), []byte(calcSource), 0666); err != nil {
		t.Fatal(err)
	}
	plan := ownedTestPlan(modDir, owner)
	staging := t.TempDir()
	req := ownedTestRequest(t, "example.com/mod/sub", "sub", "package sub\n\nfunc ComputeValue() int { return 1 }\n", []logosowned.Declaration{{Owner: "example.com/mod/sub", File: "calc.lgo"}})
	writeOwnedSidecar(t, staging, "example.com/mod/sub", req)
	if _, err := emitOwnedHelpers(plan, staging, out, map[string]string{}); err != nil {
		t.Fatalf("emit with calc.lgo header failed: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "sub", "logos_owned_*.go"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 helper, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse emitted helper: %v", err)
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil {
		t.Fatal(err)
	}
	if expr == nil || expr.String() != "linux" {
		t.Fatalf("calc.lgo build tag linux not propagated, got %v", expr)
	}

	// Malformed header still fails: syntax error in package clause or comments
	badDir := t.TempDir()
	badOwner := ownedTestMainPackage(badDir, "example.com/mod/bad", "bad")
	badOwner.GoFiles = []string{"bad.lgo"}
	if err := os.MkdirAll(badOwner.Dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badOwner.Dir, "bad.lgo"), []byte("package\n\nconst x = 1\n"), 0666); err != nil {
		t.Fatal(err)
	}
	badPlan := ownedTestPlan(badDir, badOwner)
	badStaging := t.TempDir()
	badReq := ownedTestRequest(t, "example.com/mod/bad", "bad", "package bad\n\nfunc UnusedDecl() int { return 1 }\n", []logosowned.Declaration{{Owner: "example.com/mod/bad", File: "bad.lgo"}})
	writeOwnedSidecar(t, badStaging, "example.com/mod/bad", badReq)
	if _, err := emitOwnedHelpers(badPlan, badStaging, t.TempDir(), map[string]string{}); err == nil {
		t.Fatalf("malformed header accepted unexpectedly")
	}
}
