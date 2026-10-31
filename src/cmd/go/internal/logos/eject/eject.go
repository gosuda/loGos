// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package eject implements logos eject: it writes a complete pure-Go
// copy of the main module for upstream Go toolchains. Every .lgo file
// becomes a .go file compiled from the same lowered nodes the native
// build uses, every ejected loGo/s dependency module lands under
// DIR/_logos_deps, and the support sources become the shared module
// github.com/gosuda/loGos/rt.
package eject

import (
	"bytes"
	"context"
	"go/ast"
	"go/build/constraint"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"cmd/go/internal/base"
	"cmd/go/internal/cfg"
	"cmd/go/internal/load"
	"cmd/go/internal/modload"
	"cmd/go/internal/work"
)

// rtModulePath is the shared support module for ejected trees. Every
// ejected loGo/s dependency keeps one type identity for Option and
// friends through it.
const rtModulePath = "github.com/gosuda/loGos/rt"

// rtRequireVersion pins the support module in ejected go.mod files.
// The replace line always points at the ejected copy, so the version
// never resolves over the network.
const rtRequireVersion = "v0.0.0"

// CmdEject implements the logos eject command.
var CmdEject = &base.Command{
	Run:       runEject,
	UsageLine: "logos eject [-o DIR]",
	Short:     "write a pure-Go copy of the main module",
	Long: `
Eject writes a complete pure-Go copy of the main module to DIR:
every .lgo file becomes a .go file with comments kept, every
ejected loGo/s dependency module lands under DIR/_logos_deps, and
the support sources become the shared module
github.com/gosuda/loGos/rt under DIR/_logos_deps/rt.

DIR must not exist or must be empty. The ejected tree builds with an
upstream Go 1.27 toolchain. Eject never writes into the source tree.
	`,
}

// ejectOut holds the required -o DIR output directory.
var ejectOut string

// EmitGo holds the logos build --emit-go directory. When set, logos
// build is an alias of logos eject into that directory.
var EmitGo string

func init() {
	CmdEject.Flag.StringVar(&ejectOut, "o", "", "")
	base.AddChdirFlag(&CmdEject.Flag)
}

// Register adds the eject subcommand and the logos-only --emit-go build
// flag to the logos root command.
func Register(root *base.Command) {
	root.Commands = append(root.Commands, CmdEject)
	build := root.Lookup("build")
	if build == nil {
		return
	}
	if build.Flag.Lookup("emit-go") == nil {
		build.Flag.StringVar(&EmitGo, "emit-go", "", "")
	}
	orig := build.Run
	build.Run = func(ctx context.Context, cmd *base.Command, args []string) {
		if EmitGo == "" {
			orig(ctx, cmd, args)
			return
		}
		ejectToDir(ctx, EmitGo)
	}
	const help = "\n\t-emit-go=DIR\n\t\twrite a pure-Go copy of the main module to DIR instead of building (logos builds only).\n"
	if !strings.Contains(build.Long, "-emit-go") {
		before, after, found := strings.Cut(build.Long, "\nSee also:")
		if found {
			build.Long = before + help + "\nSee also:" + after
			return
		}
		build.Long += help
	}
}

// runEject implements the logos eject command. Eject covers the whole
// main module, so package arguments are rejected.
func runEject(ctx context.Context, cmd *base.Command, args []string) {
	if len(args) > 0 {
		base.Fatalf("logos eject: unexpected package arguments")
	}
	ejectToDir(ctx, ejectOut)
}

// ejectModule groups the ejected packages of one module.
type ejectModule struct {
	path    string
	version string
	dir     string
	pkgs    []*load.Package
}

// ejectPlan splits the reachable packages into the main module, the
// ejected loGo/s dependency modules, and the emission targets.
// provenance retains every loaded package by import path for read-only
// declaration provenance, including standard packages (never writable)
// and test variants. Emission still uses main/deps/logos only.
type ejectPlan struct {
	main       *ejectModule
	deps       []*ejectModule
	depByPath  map[string]*ejectModule
	logos      []*load.Package
	known      map[*load.Package]bool
	testBuilt  map[*load.Package]bool
	provenance map[string][]*load.Package
}

// testBuild holds the test variant of one package for emission of its
// test files.
type testBuild struct {
	main   *load.Package
	ptest  *load.Package
	pxtest *load.Package
}

// ejectToDir writes the ejected tree to out. It loads the main module,
// compiles every loGo/s package with -logos.eject into staging, then
// assembles out from the staging output plus copied sources. The two
// builds run with ModeBuild only, so link outputs land under the build
// work directory and nothing is written into the source tree.
func ejectToDir(ctx context.Context, out string) {
	checkEjectOut(out)
	if isEjectVariant() {
		if extra := os.Getenv("LOGOSEJECT_TAGS"); extra != "" {
			for s := range strings.SplitSeq(extra, ",") {
				if s != "" && !slices.Contains(cfg.BuildContext.BuildTags, s) {
					cfg.BuildContext.BuildTags = append(cfg.BuildContext.BuildTags, s)
				}
			}
		}
	}
	moduleLoader := modload.NewLoader()
	moduleLoader.InitWorkfile()
	work.BuildInit(moduleLoader)
	staging, err := os.MkdirTemp("", "logoseject")
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(staging); err != nil {
			base.Errorf("logos eject: %v", err)
		}
	}()
	load.ArmLogosEject(staging)
	defer load.DisarmLogosEject()
	opts := load.PackageOpts{AutoVCS: true}
	pkgs := load.PackagesAndErrors(moduleLoader, ctx, opts, []string{"all"})
	load.CheckPackageErrors(pkgs)
	plan := arrangeEjectPlan(pkgs)
	builds := collectTestBuilds(moduleLoader, ctx, opts, plan)
	makeEjectStagingDirs(plan, staging, builds)
	buildEjectRoots(moduleLoader, ctx, phaseOneRoots(plan))
	if len(builds) > 0 {
		stripEjectCopies(builds)
		buildEjectRoots(moduleLoader, ctx, testBuildMains(builds))
	}
	var variantOuts []string
	if !isEjectVariant() {
		variantOuts = stageInactiveVariantsIntoStaging(ctx, plan, staging)
	}
	writeEjectedTree(plan, staging, out)
	mergeVariantExtraDeps(out, variantOuts, plan)
	for _, dir := range variantOuts {
		_ = os.RemoveAll(dir)
	}
}

// checkEjectOut rejects an output directory that exists and is not
// empty. It runs before any loading or building, so a rejected eject
// costs nothing and changes nothing.
func checkEjectOut(out string) {
	if out == "" {
		base.Fatalf("logos eject: -o DIR is required")
	}
	st, err := os.Stat(out)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		base.Fatalf("logos eject: %s: %v", out, err)
	}
	if !st.IsDir() {
		base.Fatalf("logos eject: %s: not a directory", out)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		base.Fatalf("logos eject: %s: %v", out, err)
	}
	if len(entries) > 0 {
		base.Fatalf("logos eject: %s: directory is not empty", out)
	}
}

// arrangeEjectPlan groups the packages reachable from the main module
// into the main module entry, the loGo/s dependency modules, and the
// emission targets.
func arrangeEjectPlan(pkgs []*load.Package) *ejectPlan {
	gomod := modload.FindGoMod(base.Cwd())
	if gomod == "" {
		base.Fatalf("logos eject: no main module (run inside a module)")
	}
	plan := &ejectPlan{
		main:       &ejectModule{dir: filepath.Dir(gomod)},
		depByPath:  map[string]*ejectModule{},
		known:      map[*load.Package]bool{},
		testBuilt:  map[*load.Package]bool{},
		provenance: map[string][]*load.Package{},
	}
	for _, p := range load.PackageList(pkgs) {
		plan.provenance[p.ImportPath] = append(plan.provenance[p.ImportPath], p)
		plan.addPackage(p)
	}
	resortEjectPlan(plan)
	return plan
}

// addPackage files one reachable package into the plan. Test artifacts
// stay out: test mains and test variants build in the second phase,
// never as plan roots.
func (plan *ejectPlan) addPackage(p *load.Package) {
	if plan.known[p] || p.Standard || p.Module == nil || isTestArtifact(p) {
		return
	}
	plan.known[p] = true
	if p.Module.Main {
		if p.Module.Dir != "" && p.Module.Dir != plan.main.dir {
			base.Fatalf("logos eject: workspaces with several main modules are not supported")
		}
		plan.main.pkgs = append(plan.main.pkgs, p)
	} else {
		m := plan.depByPath[p.Module.Path]
		if m == nil {
			if p.Module.Path == "" || p.Module.Version == "" {
				base.Fatalf("logos eject: dependency module has no path or version")
			}
			if p.Module.Dir == "" {
				base.Fatalf("logos eject: module %s has no directory", p.Module.Path)
			}
			m = &ejectModule{path: p.Module.Path, version: p.Module.Version, dir: p.Module.Dir}
			plan.depByPath[p.Module.Path] = m
		}
		if m.version != p.Module.Version || m.dir != p.Module.Dir {
			base.Fatalf("logos eject: module %s has several versions", p.Module.Path)
		}
		m.pkgs = append(m.pkgs, p)
	}
	if ejectOwnsLogos(p) {
		plan.logos = append(plan.logos, p)
	}
}

// resortEjectPlan rebuilds the sorted dependency and target lists after
// test-only dependencies joined the plan.
func resortEjectPlan(plan *ejectPlan) {
	plan.deps = nil
	for _, m := range plan.depByPath {
		if moduleHasLogos(m) {
			plan.deps = append(plan.deps, m)
		}
	}
	slices.SortFunc(plan.deps, func(a, b *ejectModule) int { return strings.Compare(a.path, b.path) })
	slices.SortFunc(plan.logos, func(a, b *load.Package) int { return strings.Compare(a.ImportPath, b.ImportPath) })
	slices.SortFunc(plan.main.pkgs, func(a, b *load.Package) int { return strings.Compare(a.ImportPath, b.ImportPath) })
}

// ejectOwnsLogos reports whether p owns any loGo/s source, including
// host-inactive files. Unlike load.Package.IsLogos, which classifies by
// the first active file, eject must also lower alternate-platform files
// listed under IgnoredGoFiles: they never reach the host build, so
// without this the variant staging would never see them and the ejected
// tree would silently omit them instead of emitting their lowered Go.
func ejectOwnsLogos(p *load.Package) bool {
	if p.IsLogos() {
		return true
	}
	for _, name := range p.IgnoredGoFiles {
		if cfg.IsLogosFile(name) {
			return true
		}
	}
	return false
}

// moduleHasLogos reports whether any package of m is a loGo/s package.
func moduleHasLogos(m *ejectModule) bool {
	for _, p := range m.pkgs {
		if ejectOwnsLogos(p) {
			return true
		}
	}
	return false
}

// isTestArtifact reports whether p is a synthesized test main or a test
// variant produced for one. Such packages build in the second phase.
func isTestArtifact(p *load.Package) bool {
	return p.ForTest != "" || p.Internal.TestmainGo != nil
}

// hasLogosTestFiles reports whether p holds loGo/s test files needing
// test-variant compilation for emission.
func hasLogosTestFiles(p *load.Package) bool {
	for _, name := range p.TestGoFiles {
		if cfg.IsLogosFile(name) {
			return true
		}
	}
	for _, name := range p.XTestGoFiles {
		if cfg.IsLogosFile(name) {
			return true
		}
	}
	return false
}

// collectTestBuilds prepares test-variant builds for every loGo/s
// package holding loGo/s test files, and absorbs the test-only
// dependencies their loads discovered into the plan.
func collectTestBuilds(moduleLoader *modload.Loader, ctx context.Context, opts load.PackageOpts, plan *ejectPlan) []testBuild {
	var builds []testBuild
	var bad []*load.Package
	for _, p := range plan.logos {
		if !hasLogosTestFiles(p) {
			continue
		}
		pmain, ptest, pxtest, perr := load.TestPackagesFor(moduleLoader, ctx, opts, p, nil)
		if perr != nil {
			bad = append(bad, perr)
			continue
		}
		if pmain == nil {
			base.Fatalf("logos eject: no test main for %s", p.ImportPath)
		}
		builds = append(builds, testBuild{main: pmain, ptest: ptest, pxtest: pxtest})
		plan.testBuilt[p] = true
	}
	load.CheckPackageErrors(bad)
	var mains []*load.Package
	for _, t := range builds {
		mains = append(mains, t.main)
	}
	for _, p := range load.PackageList(mains) {
		found := false
		for _, q := range plan.provenance[p.ImportPath] {
			if q == p {
				found = true
				break
			}
		}
		if !found {
			plan.provenance[p.ImportPath] = append(plan.provenance[p.ImportPath], p)
		}
		plan.addPackage(p)
	}
	resortEjectPlan(plan)
	return builds
}

// makeEjectStagingDirs creates one staging directory per emission
// target, including test variants, so the compiler always finds its
// -logos.eject directory.
func makeEjectStagingDirs(plan *ejectPlan, staging string, builds []testBuild) {
	mkdir := func(importPath string) {
		if err := os.MkdirAll(filepath.Join(staging, filepath.FromSlash(importPath)), 0777); err != nil {
			base.Fatalf("logos eject: %v", err)
		}
	}
	for _, p := range plan.logos {
		mkdir(p.ImportPath)
	}
	for _, t := range builds {
		for _, p := range []*load.Package{t.ptest, t.pxtest} {
			if p != nil {
				mkdir(p.ImportPath)
			}
		}
	}
}

// testBuildMains returns the test main roots of the second phase.
func testBuildMains(builds []testBuild) []*load.Package {
	mains := make([]*load.Package, 0, len(builds))
	for _, t := range builds {
		mains = append(mains, t.main)
	}
	return mains
}

// phaseOneRoots returns the first-phase build roots: every main-module
// package plus every marked loGo/s package, sorted for determinism.
func phaseOneRoots(plan *ejectPlan) []*load.Package {
	set := map[*load.Package]bool{}
	var roots []*load.Package
	for _, p := range plan.main.pkgs {
		if p.Standard || set[p] {
			continue
		}
		set[p] = true
		roots = append(roots, p)
	}
	for _, p := range plan.logos {
		if set[p] {
			continue
		}
		set[p] = true
		roots = append(roots, p)
	}
	return roots
}

// buildEjectRoots compiles roots and their dependencies with ModeBuild.
// Link outputs land under the build work directory, never the source
// tree. A failed build exits before any output is written.
func buildEjectRoots(moduleLoader *modload.Loader, ctx context.Context, roots []*load.Package) {
	b := work.NewBuilder("", moduleLoader.VendorDirOrEmpty)
	defer func() {
		if err := b.Close(); err != nil {
			base.Fatalf("logos eject: %v", err)
		}
	}()
	root := &work.Action{Mode: "go build"}
	for _, p := range roots {
		root.Deps = append(root.Deps, b.AutoAction(moduleLoader, work.ModeBuild, work.ModeBuild, p))
	}
	b.Do(ctx, root)
	if root.Failed != nil {
		base.SetExitStatus(1)
	}
	if base.GetExitStatus() != 0 {
		base.Exit()
	}
}

// stripEjectCopies keeps emission on exactly one compile per package in
// the second phase: the test variants. Test copies of dependencies lose
// the flag, so no staging file gets two concurrent writers.
func stripEjectCopies(builds []testBuild) {
	keep := map[*load.Package]bool{}
	var mains []*load.Package
	for _, t := range builds {
		mains = append(mains, t.main)
		if t.ptest != nil {
			keep[t.ptest] = true
			t.ptest.Internal.LogosEject = true
		}
		if t.pxtest != nil {
			keep[t.pxtest] = true
			t.pxtest.Internal.LogosEject = true
		}
	}
	for _, p := range load.PackageList(mains) {
		if p.Internal.LogosEject && !keep[p] {
			p.Internal.LogosEject = false
			p.Internal.Gcflags = withoutEjectFlags(p.Internal.Gcflags)
		}
	}
}

// withoutEjectFlags returns flags without the -logos.eject entries in a
// fresh slice, leaving shared backing arrays untouched.
func withoutEjectFlags(flags []string) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if !strings.HasPrefix(f, "-logos.eject=") {
			out = append(out, f)
		}
	}
	return out
}

// writeEjectedTree assembles the output directory from copied sources,
// compiler-emitted files, the support module, and rewritten go.mod files.
func writeEjectedTree(plan *ejectPlan, staging, out string) {
	abs, err := filepath.Abs(out)
	if err != nil {
		base.Fatalf("logos eject: %s: %v", out, err)
	}
	// Support use comes from every Go source the tree writes: copied
	// plain Go files in the main module and the ejected dependency
	// modules, and compiler-emitted loGo/s files. Support emission and the
	// go.mod rewriting below read this one flag.
	emitOne := emitPackageGoFiles
	if isEjectVariant() {
		emitOne = emitPackageGoFilesVariant
	}
	usedRT := copyModuleTree(plan.main.dir, out, ejectSkipDir(plan.main.dir, abs, out))
	for _, p := range plan.main.pkgs {
		if emitOne(p, plan, staging, out, plan.main.dir) {
			usedRT = true
		}
	}
	ejected := map[string]string{}
	for _, m := range plan.deps {
		dst := filepath.Join(out, "_logos_deps", m.path+"@"+m.version)
		ejected[m.path] = dst
		if copyModuleTree(m.dir, dst, ejectSkipDir(m.dir, abs, out)) {
			usedRT = true
		}
		for _, p := range m.pkgs {
			if emitOne(p, plan, staging, dst, m.dir) {
				usedRT = true
			}
		}
	}
	// Owned helpers land after package copy, so generated files only ever
	// appear in the output tree, and before go.mod rewriting, so modules
	// pulled in only by an owned helper still get their require lines.
	if usesOwned, err := emitOwnedHelpers(plan, staging, out, ejected); err != nil {
		base.Fatalf("logos eject: %v", err)
	} else if usesOwned {
		usedRT = true
	}
	var rtDir string
	if usedRT {
		rtDir = filepath.Join(out, "_logos_deps", "rt")
		writeSupportModule(out, goVersionOf(parseGoModFile(filepath.Join(out, "go.mod"))))
		addSupportRequire(filepath.Join(out, "go.mod"), "./_logos_deps/rt")
	}
	rewriteMainGoMod(out, plan)
	for _, m := range plan.deps {
		rewriteDepGoMod(plan, m, ejected)
	}
	if usedRT {
		for _, m := range plan.deps {
			addSupportRequire(filepath.Join(ejected[m.path], "go.mod"), relEjectTarget(ejected[m.path], rtDir))
		}
	}
}

func ejectPackageNames(p *load.Package, plan *ejectPlan) ([]string, map[string]bool) {
	names := slices.Clone(p.GoFiles)
	names = append(names, p.CgoFiles...)
	for _, name := range p.IgnoredGoFiles {
		if strings.HasSuffix(logosStem(name), "_test") && !plan.testBuilt[p] {
			continue
		}
		names = append(names, name)
	}
	if plan.testBuilt[p] {
		names = append(names, p.TestGoFiles...)
		names = append(names, p.XTestGoFiles...)
	}
	ignoredSet := make(map[string]bool, len(p.IgnoredGoFiles))
	for _, name := range p.IgnoredGoFiles {
		ignoredSet[name] = true
	}
	return names, ignoredSet
}

func writeLoweredFile(dstRoot, rel, stem, name string, raw []byte) bool {
	fixed, uses := rewriteEjectedGo(raw, name)
	dst := filepath.Join(dstRoot, rel, stem+".go")
	if err := os.MkdirAll(filepath.Dir(dst), 0777); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	if err := os.WriteFile(dst, fixed, 0666); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	return uses
}

// emitPackageGoFiles writes the compiler-emitted Go files of one loGo/s
// package under dstRoot, rewritten to the ejected import paths and
// formatted. It reports whether any file uses the support module.
// Missing non-ignore files fail loudly: after variant staging every
// owned file is present, so a miss is a build failure, never a silent
// omission or raw fallback.
func emitPackageGoFiles(p *load.Package, plan *ejectPlan, staging, dstRoot, modRoot string) bool {
	if !ejectOwnsLogos(p) {
		return false
	}
	rel, err := filepath.Rel(modRoot, p.Dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		base.Fatalf("logos eject: package %s outside module", p.ImportPath)
	}
	names, ignoredSet := ejectPackageNames(p, plan)
	used := false
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] || !cfg.IsLogosFile(name) {
			continue
		}
		seen[name] = true
		stem := logosStem(name)
		raw, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(p.ImportPath), stem+".go"))
		if err != nil {
			if os.IsNotExist(err) && ignoredSet[name] {
				srcData, readErr := os.ReadFile(filepath.Join(p.Dir, name))
				if readErr != nil {
					base.Fatalf("logos eject: missing ejected output for %s: %v", filepath.Join(p.Dir, name), readErr)
				}
				if isBuildIgnored(srcData) {
					continue
				}
			}
			base.Fatalf("logos eject: missing ejected output for %s: %v", filepath.Join(p.Dir, name), err)
		}
		if writeLoweredFile(dstRoot, rel, stem, name, raw) {
			used = true
		}
	}
	return used
}

// emitPackageGoFilesVariant emits only the files its own GOOS/tags
// context selected. Other inactive variants are absent in this context
// by design, so it skips every missing file; the main eject merges
// every helper tree and fails loudly if anything is still missing.
func emitPackageGoFilesVariant(p *load.Package, plan *ejectPlan, staging, dstRoot, modRoot string) bool {
	if !ejectOwnsLogos(p) {
		return false
	}
	rel, err := filepath.Rel(modRoot, p.Dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		base.Fatalf("logos eject: package %s outside module", p.ImportPath)
	}
	names, _ := ejectPackageNames(p, plan)
	used := false
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] || !cfg.IsLogosFile(name) {
			continue
		}
		seen[name] = true
		stem := logosStem(name)
		raw, err := os.ReadFile(filepath.Join(staging, filepath.FromSlash(p.ImportPath), stem+".go"))
		if err != nil {
			continue
		}
		if writeLoweredFile(dstRoot, rel, stem, name, raw) {
			used = true
		}
	}
	return used
}

// isEjectVariant reports whether this eject runs as an inactive-variant
// helper. Helpers do plain host-context emission only, so they never
// spawn further variants and recursion terminates.
func isEjectVariant() bool {
	return os.Getenv("LOGOSEJECT_IN_VARIANT") != ""
}

// inactiveFile is one owned .lgo file excluded from the host build by
// platform or tag constraints that still needs compiler lowering.
type inactiveFile struct {
	pkg  *load.Package
	name string
}

// collectInactiveLogosFiles lists every owned .lgo file the host build
// excluded: IgnoredGoFiles entries that are loGo/s sources, not
// test-skipped, not //go:build ignore, and missing from host staging.
// Active files missing from staging are build failures, not variants,
// so only ignored entries qualify here.
func collectInactiveLogosFiles(plan *ejectPlan, staging string) []inactiveFile {
	var out []inactiveFile
	for _, p := range plan.logos {
		for _, name := range p.IgnoredGoFiles {
			if !cfg.IsLogosFile(name) {
				continue
			}
			if strings.HasSuffix(logosStem(name), "_test") && !plan.testBuilt[p] {
				continue
			}
			srcData, err := os.ReadFile(filepath.Join(p.Dir, name))
			if err != nil {
				base.Fatalf("logos eject: missing inactive source %s: %v", filepath.Join(p.Dir, name), err)
			}
			if isBuildIgnored(srcData) {
				continue
			}
			stem := logosStem(name)
			if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(p.ImportPath), stem+".go")); err == nil {
				continue
			}
			out = append(out, inactiveFile{pkg: p, name: name})
		}
	}
	return out
}

// collectConstraintTags appends every tag named by expr.
func collectConstraintTags(expr constraint.Expr, tags *[]string) {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		*tags = append(*tags, e.Tag)
	case *constraint.NotExpr:
		collectConstraintTags(e.X, tags)
	case *constraint.AndExpr:
		collectConstraintTags(e.X, tags)
		collectConstraintTags(e.Y, tags)
	case *constraint.OrExpr:
		collectConstraintTags(e.X, tags)
		collectConstraintTags(e.Y, tags)
	}
}

// findVariantContextForFile finds one host-close build context that
// selects the inactive file, using go/build MatchFile so filename
// suffixes and //go:build lines evaluate exactly as the go tool does.
// It varies GOOS, GOARCH, and tags from the file; CGO stays host
// to keep the helper count to real platform/tag variants.
func findVariantContextForFile(pkgDir, name, hostGOOS, hostGOARCH string, hostTags []string) (string, string, []string, bool) {
	data, err := os.ReadFile(filepath.Join(pkgDir, name))
	if err != nil {
		base.Fatalf("logos eject: read inactive file %s: %v", filepath.Join(pkgDir, name), err)
	}
	pf, err := parser.ParseFile(token.NewFileSet(), name, data, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		base.Fatalf("logos eject: parse build header of %s: %v", filepath.Join(pkgDir, name), err)
	}
	expr, err := ownedHeaderExpr(pf)
	if err != nil {
		base.Fatalf("logos eject: parse build constraint of %s: %v", filepath.Join(pkgDir, name), err)
	}
	var mentioned []string
	if expr != nil {
		collectConstraintTags(expr, &mentioned)
	}
	mentioned = append(mentioned, ownedFilenameTags(name)...)
	seenMention := map[string]bool{}
	var uniqMention []string
	for _, t := range mentioned {
		if !seenMention[t] {
			seenMention[t] = true
			uniqMention = append(uniqMention, t)
		}
	}
	var goosMentioned []string
	var goarchMentioned []string
	var customMentioned []string
	for _, t := range uniqMention {
		if t == "wasip1" || slices.Contains(ownedGoosTags, t) {
			goosMentioned = append(goosMentioned, t)
			continue
		}
		if t == "riscv" {
			continue
		}
		if slices.Contains(ownedGoarchTags, t) {
			goarchMentioned = append(goarchMentioned, t)
			continue
		}
		if t == "cgo" || t == "ignore" || t == "unix" || t == "boringcrypto" || strings.HasPrefix(t, "go1.") || strings.HasPrefix(t, "goexperiment.") {
			continue
		}
		customMentioned = append(customMentioned, t)
	}
	goosCands := []string{hostGOOS}
	for _, g := range goosMentioned {
		if g != hostGOOS {
			goosCands = append(goosCands, g)
		}
	}
	goarchCands := []string{hostGOARCH}
	for _, a := range goarchMentioned {
		if a != hostGOARCH {
			goarchCands = append(goarchCands, a)
		}
	}
	hostTagSet := map[string]bool{}
	for _, t := range hostTags {
		hostTagSet[t] = true
	}
	var customUniq []string
	seenCustom := map[string]bool{}
	for _, t := range customMentioned {
		if !seenCustom[t] && !hostTagSet[t] {
			seenCustom[t] = true
			customUniq = append(customUniq, t)
		}
	}
	slices.Sort(customUniq)
	if goos, goarch, tags, ok := searchVariantContext(pkgDir, name, goosCands, goarchCands, hostTags, customUniq); ok {
		return goos, goarch, tags, true
	}
	for _, g := range []string{"linux", "darwin", "windows", "freebsd"} {
		if !slices.Contains(goosCands, g) {
			goosCands = append(goosCands, g)
		}
	}
	for _, g := range ownedGoosTags {
		if !slices.Contains(goosCands, g) {
			goosCands = append(goosCands, g)
		}
	}
	if !slices.Contains(goosCands, "wasip1") {
		goosCands = append(goosCands, "wasip1")
	}
	for _, a := range ownedGoarchTags {
		if a == "riscv" {
			continue
		}
		if !slices.Contains(goarchCands, a) {
			goarchCands = append(goarchCands, a)
		}
	}
	return searchVariantContext(pkgDir, name, goosCands, goarchCands, hostTags, customUniq)
}

func matchVariantTags(pkgDir, name, goos, goarch string, hostTags, customUniq []string) ([]string, bool) {
	for mask := range 1 << len(customUniq) {
		tags := slices.Clone(hostTags)
		for i, t := range customUniq {
			if mask&(1<<i) != 0 && !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
		ctxt := cfg.BuildContext
		ctxt.GOOS = goos
		ctxt.GOARCH = goarch
		ctxt.BuildTags = slices.Clone(tags)
		if !slices.Contains(ctxt.ToolTags, "logos") {
			ctxt.ToolTags = append(slices.Clone(ctxt.ToolTags), "logos")
		}
		match, err := ctxt.MatchFile(pkgDir, name)
		if err != nil {
			base.Fatalf("logos eject: match inactive file %s: %v", filepath.Join(pkgDir, name), err)
		}
		if match {
			slices.Sort(tags)
			return tags, true
		}
	}
	return nil, false
}

func validEjectPlatform(goos, goarch string) bool {
	return work.CheckGOOSARCHPair(goos, goarch) == nil
}

func searchVariantContext(pkgDir, name string, goosCands, goarchCands, hostTags, customUniq []string) (string, string, []string, bool) {
	for _, goos := range goosCands {
		for _, goarch := range goarchCands {
			if !validEjectPlatform(goos, goarch) {
				continue
			}
			if tags, ok := matchVariantTags(pkgDir, name, goos, goarch, hostTags, customUniq); ok {
				return goos, goarch, tags, true
			}
		}
	}
	return "", "", nil, false
}

// variantCtx is one helper-eject build context: the GOOS, GOARCH, and
// full -tags list that select a group of inactive files with correct
// dependencies.
type variantCtx struct {
	goos   string
	goarch string
	tags   []string
}

// variantContextsForFiles groups inactive files by one satisfying
// context each, deduplicated so one helper eject covers every file
// its context selects.
func variantContextsForFiles(files []inactiveFile) []variantCtx {
	hostGOOS := cfg.BuildContext.GOOS
	hostGOARCH := cfg.BuildContext.GOARCH
	hostTags := slices.Clone(cfg.BuildContext.BuildTags)
	byKey := map[string]variantCtx{}
	var keys []string
	for _, f := range files {
		goos, goarch, tags, ok := findVariantContextForFile(f.pkg.Dir, f.name, hostGOOS, hostGOARCH, hostTags)
		if !ok {
			base.Fatalf("logos eject: no build context selects inactive file %s", filepath.Join(f.pkg.Dir, f.name))
		}
		key := goos + "\x00" + goarch + "\x00" + strings.Join(tags, ",")
		if _, exists := byKey[key]; !exists {
			keys = append(keys, key)
		}
		byKey[key] = variantCtx{goos: goos, goarch: goarch, tags: tags}
	}
	slices.Sort(keys)
	out := make([]variantCtx, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

// spawnVariantEjects runs one plain helper eject per variant context.
// Each helper uses the real compiler in its own GOOS/GOARCH/tags context,
// so every inactive file gets type-checked lowering with correct
// imports and dependencies. Helpers set LOGOSEJECT_IN_VARIANT so they
// never recurse. Consistency needs idempotent merges: helpers may
// overlap host outputs, and the main tree merges each helper file
// exactly once by staging path, so repeated helper content is safe.
func spawnVariantEjects(ctx context.Context, ctxs []variantCtx) []string {
	exe, err := os.Executable()
	if err != nil {
		base.Fatalf("logos eject: find logos binary for variants: %v", err)
	}
	var dirs []string
	for _, vc := range ctxs {
		dir, err := os.MkdirTemp("", "logoseject-variant")
		if err != nil {
			base.Fatalf("logos eject: %v", err)
		}
		dirs = append(dirs, dir)
		cmd := exec.CommandContext(ctx, exe, "eject", "-o", dir)
		cmd.Args[0] = "logos"
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		kept := make([]string, 0, len(os.Environ()))
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "GOOS=") || strings.HasPrefix(kv, "GOARCH=") || strings.HasPrefix(kv, "LOGOSEJECT_IN_VARIANT=") || strings.HasPrefix(kv, "LOGOSEJECT_TAGS=") {
				continue
			}
			kept = append(kept, kv)
		}
		env := append(kept, "LOGOSEJECT_IN_VARIANT=1")
		env = append(env, "GOOS="+vc.goos)
		env = append(env, "GOARCH="+vc.goarch)
		env = append(env, "LOGOSEJECT_TAGS="+strings.Join(vc.tags, ","))
		cmd.Env = env
		if err := cmd.Run(); err != nil {
			base.Fatalf("logos eject: variant eject for GOOS=%s GOARCH=%s tags=%q failed: %v", vc.goos, vc.goarch, strings.Join(vc.tags, ","), err)
		}
	}
	return dirs
}

// variantOutFile maps one owned package file to its lowered output in
// a helper eject tree: main-module packages at the module-relative
// path, dependency packages under _logos_deps.
func variantOutFile(variantOut string, plan *ejectPlan, pkg *load.Package, stem string) string {
	if pkg.Module != nil && pkg.Module.Main {
		rel, err := filepath.Rel(plan.main.dir, pkg.Dir)
		if err != nil {
			return ""
		}
		return filepath.Join(variantOut, rel, stem+".go")
	}
	if pkg.Module != nil && pkg.Module.Path != "" {
		ver := pkg.Module.Version
		mdir := pkg.Module.Dir
		rel, err := filepath.Rel(mdir, pkg.Dir)
		if err != nil {
			return ""
		}
		return filepath.Join(variantOut, "_logos_deps", pkg.Module.Path+"@"+ver, rel, stem+".go")
	}
	return ""
}

// stageInactiveVariantsIntoStaging lowers every owned inactive
// platform/tag variant with the real compiler: it spawns one helper
// eject per satisfying GOOS/tags context, then copies each inactive
// file's helper-lowered output into host staging before emission.
// Host emission then finds every non-ignore file, preserves its build
// constraints, and accounts support use via the normal rewritten
// outputs. It returns the helper trees for extra-dependency merging.
func stageInactiveVariantsIntoStaging(ctx context.Context, plan *ejectPlan, staging string) []string {
	files := collectInactiveLogosFiles(plan, staging)
	ctxs := variantContextsForFiles(files)
	outs := spawnVariantEjects(ctx, ctxs)
	for _, f := range files {
		stem := logosStem(f.name)
		dst := filepath.Join(staging, filepath.FromSlash(f.pkg.ImportPath), stem+".go")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		copied := false
		for _, out := range outs {
			src := variantOutFile(out, plan, f.pkg, stem)
			if src == "" {
				continue
			}
			data, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0777); err != nil {
				base.Fatalf("logos eject: %v", err)
			}
			if err := os.WriteFile(dst, data, 0666); err != nil {
				base.Fatalf("logos eject: %v", err)
			}
			copied = true
			break
		}
		if !copied {
			base.Fatalf("logos eject: missing ejected output for %s", filepath.Join(f.pkg.Dir, f.name))
		}
	}
	return outs
}

func copyVariantDir(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		base.Fatalf("logos eject: read variant dir %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0777); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyVariantDir(s, d)
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			base.Fatalf("logos eject: read variant file %s: %v", s, err)
		}
		if err := os.WriteFile(d, data, 0666); err != nil {
			base.Fatalf("logos eject: write variant file %s: %v", d, err)
		}
	}
}

func variantDirUsesRT(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(data), rtModulePath) {
			found = true
		}
		return nil
	})
	return found
}

func mergeVariantExtraDeps(mainOut string, variantOuts []string, plan *ejectPlan) {
	hostDeps := map[string]bool{}
	for _, m := range plan.deps {
		hostDeps[m.path+"@"+m.version] = true
	}
	var extra []string
	for _, vout := range variantOuts {
		depsRoot := filepath.Join(vout, "_logos_deps")
		entries, err := os.ReadDir(depsRoot)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "rt" {
				continue
			}
			if hostDeps[e.Name()] {
				continue
			}
			dst := filepath.Join(mainOut, "_logos_deps", e.Name())
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			copyVariantDir(filepath.Join(depsRoot, e.Name()), dst)
			extra = append(extra, e.Name())
		}
	}
	slices.Sort(extra)
	mainMod := parseGoModFile(filepath.Join(mainOut, "go.mod"))
	changed := false
	for _, name := range extra {
		path, vers, ok := strings.Cut(name, "@")
		if !ok || path == "" || vers == "" {
			continue
		}
		needRequire := true
		for _, r := range mainMod.Require {
			if r.Mod.Path == path {
				needRequire = false
				break
			}
		}
		if needRequire {
			ensureGoRequire(mainMod, path, vers)
			changed = true
		}
		needReplace := true
		for _, r := range mainMod.Replace {
			if r.Old.Path == path && r.New.Path == "./_logos_deps/"+name {
				needReplace = false
				break
			}
		}
		if needReplace {
			setGoReplace(mainMod, path, vers, "./_logos_deps/"+name)
			changed = true
		}
	}
	if changed {
		writeGoModFile(filepath.Join(mainOut, "go.mod"), mainMod)
	}
	ensureVariantSupport(mainOut, variantOuts)
}

func ensureVariantSupport(mainOut string, variantOuts []string) {
	rtDst := filepath.Join(mainOut, "_logos_deps", "rt")
	if _, err := os.Stat(rtDst); err == nil {
		return
	}
	if !variantDirUsesRT(mainOut) {
		return
	}
	for _, vout := range variantOuts {
		rtSrc := filepath.Join(vout, "_logos_deps", "rt")
		if _, err := os.Stat(rtSrc); err != nil {
			continue
		}
		copyVariantDir(rtSrc, rtDst)
		addSupportRequire(filepath.Join(mainOut, "go.mod"), "./_logos_deps/rt")
		return
	}
	base.Fatalf("logos eject: missing support module for merged variants in %s", mainOut)
}

// isBuildIgnored reports whether source declares an ignore build constraint
// such as //go:build ignore or // +build ignore. It inspects only comments
// before the package clause, so syntax in the file body never interferes.
func isBuildIgnored(source []byte) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", source, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil {
		return false
	}
	expr, err := ownedHeaderExpr(f)
	if err != nil || expr == nil {
		return false
	}
	return hasIgnoreTag(expr)
}

// hasIgnoreTag reports whether the constraint expression requires the
// special "ignore" build tag.
func hasIgnoreTag(x constraint.Expr) bool {
	if x == nil {
		return false
	}
	switch e := x.(type) {
	case *constraint.TagExpr:
		return e.Tag == "ignore"
	case *constraint.NotExpr:
		return false
	case *constraint.AndExpr:
		return hasIgnoreTag(e.X) || hasIgnoreTag(e.Y)
	case *constraint.OrExpr:
		return hasIgnoreTag(e.X) && hasIgnoreTag(e.Y)
	}
	return false
}

// logosStem returns name without its loGo/s extension, keeping a _test
// suffix when present.
func logosStem(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

// ejectSkipDir returns the path of out relative to one source module
// when out sits inside it, so the module copy skips the destination
// instead of feeding ejected files back into the tree.
func ejectSkipDir(modDir, outAbs, out string) string {
	rel, err := filepath.Rel(modDir, outAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if rel == "." {
		base.Fatalf("logos eject: %s: output is the module directory", out)
	}
	return rel
}

// copyModuleTree copies the non-loGo/s files of one module: every
// regular file except .git contents, logos.toml files, loGo/s sources
// (emitted separately), and the skip subtree (the destination when it
// sits inside the module). Symlinks and special files stay out, so a
// link can never pull outside bytes into the tree. It reports whether
// any copied Go file imports the loGo/s support packages, which the
// copy remapped to the shared support module.
func copyModuleTree(srcRoot, dstRoot, skip string) bool {
	uses := false
	err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dstRoot, 0777)
		}
		if skip != "" && (rel == skip || strings.HasPrefix(rel, skip+string(filepath.Separator))) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		for seg := range strings.SplitSeq(rel, string(filepath.Separator)) {
			if seg == ".git" {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dstRoot, rel), 0777)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := filepath.Base(rel)
		if name == "logos.toml" || cfg.IsLogosFile(name) {
			return nil
		}
		fileUses, err := copyDataFile(path, filepath.Join(dstRoot, rel))
		if fileUses {
			uses = true
		}
		return err
	})
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	return uses
}

// copyDataFile copies one non-loGo/s file. Go files that still parse
// are rewritten to the ejected import paths and formatted, so the tree
// stays gofmt-clean; anything else rides along byte for byte. It
// reports whether the written file imports the loGo/s support packages.
func copyDataFile(src, dst string) (bool, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	uses := false
	if strings.HasSuffix(src, ".go") {
		if fixed, fixedUses, ok := convertGoFile(data); ok {
			data = fixed
			uses = fixedUses
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0777); err != nil {
		return false, err
	}
	if err := os.WriteFile(dst, data, 0666); err != nil {
		return false, err
	}
	return uses, nil
}

// rewriteEjectedGo rewrites one compiler-emitted file to the ejected
// import paths and formats it. Compiler output must parse; anything
// else is a loud failure, never a silent copy.
func rewriteEjectedGo(data []byte, name string) ([]byte, bool) {
	fixed, uses, ok := convertGoFileWithSiteNS(data, siteNamespace(logosStem(name)))
	if !ok {
		base.Fatalf("logos eject: cannot parse ejected file for %s", name)
	}
	return fixed, uses
}

// sanitizeSiteStem maps a file stem to a valid identifier fragment:
// every run of non-alphanumeric/underscore becomes one underscore.
func sanitizeSiteStem(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	out := b.String()
	if out == "" {
		return "file"
	}
	return out
}

// siteNamespace namespaces per-compilation site symbols by file stem
// plus a hash of the raw stem: each helper eject restarts
// packageLower.siteSeq at zero, so two variant files merged into one
// package would otherwise both declare _logos_site0. Site declarations
// and uses are file-local, so _logos_siteN becomes
// _logos_site_<sanitized>_<hash>_N, distinct per file and stable per
// stem across helpers. The hash disambiguates stems that sanitize
// identically (for example my-file versus my_file).
func siteNamespace(stem string) string {
	sanitized := sanitizeSiteStem(stem)
	h := uint32(2166136261)
	for i := range len(stem) {
		h ^= uint32(stem[i])
		h *= 16777619
	}
	return sanitized + "_" + strconv.FormatUint(uint64(h), 16)
}

// renameSiteIdents namespaces file-local site identifiers via the AST
// so string literals and comments are never touched and identifier
// edges stay exact: only _logos_site plus trailing digits renames.
func renameSiteIdents(f *ast.File, ns string) {
	const prefix = "_logos_site"
	if ns == "" {
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if !strings.HasPrefix(id.Name, prefix) {
			return true
		}
		rest := id.Name[len(prefix):]
		if rest == "" {
			return true
		}
		for i := range len(rest) {
			if rest[i] < '0' || rest[i] > '9' {
				return true
			}
		}
		id.Name = prefix + "_" + ns + "_" + rest
		return true
	})
}

// convertGoFile parses one Go file, remaps loGo/s support imports,
// drops comptime imports, sorts the import blocks, and formats the
// result. It reports whether any support import was used.
func convertGoFile(data []byte) ([]byte, bool, bool) {
	return convertGoFileWithSiteNS(data, "")
}

// convertGoFileWithSiteNS parses one Go file as convertGoFile does. A
// non-empty siteNS additionally namespaces file-local _logos_siteN
// identifiers, keeping merged variant files distinct.
func convertGoFileWithSiteNS(data []byte, siteNS string) ([]byte, bool, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", data, parser.ParseComments)
	if err != nil {
		return nil, false, false
	}
	uses := remapLogosImports(f)
	renameSiteIdents(f, siteNS)
	ast.SortImports(fset, f)
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return nil, false, false
	}
	return buf.Bytes(), uses, true
}

// remapLogosImports rewrites logos/rt and logos/sync imports to the
// shared ejected module and drops comptime imports. Empty import
// declarations fall away.
func remapLogosImports(f *ast.File) bool {
	uses := false
	var decls []ast.Decl
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			decls = append(decls, decl)
			continue
		}
		var specs []ast.Spec
		for _, spec := range gen.Specs {
			imp, ok := spec.(*ast.ImportSpec)
			if !ok {
				specs = append(specs, spec)
				continue
			}
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				specs = append(specs, spec)
				continue
			}
			if path == "comptime" {
				continue
			}
			if rest, ok := strings.CutPrefix(path, "logos/"); ok {
				rest, _ = strings.CutPrefix(rest, "rt/")
				if rest == "rt" {
					imp.Path.Value = strconv.Quote(rtModulePath)
				} else {
					imp.Path.Value = strconv.Quote(rtModulePath + "/" + rest)
				}
				uses = true
			}
			specs = append(specs, spec)
		}
		if len(specs) == 0 {
			continue
		}
		gen.Specs = specs
		decls = append(decls, decl)
	}
	f.Decls = decls
	return uses
}

// writeSupportModule writes the shared support module: src/logos/rt at
// the root, src/logos/sync as sync/, without runtime and test files
// and without ejected build lines. The sources come from cfg.GOROOT, the
// root of the toolchain running this command.
func writeSupportModule(out, goVer string) {
	goroot := cfg.GOROOT
	rtDst := filepath.Join(out, "_logos_deps", "rt")
	copySupportDir(filepath.Join(goroot, "src", "logos", "rt"), rtDst)
	if st, err := os.Stat(filepath.Join(goroot, "src", "logos", "sync")); err == nil && st.IsDir() {
		copySupportDir(filepath.Join(goroot, "src", "logos", "sync"), filepath.Join(rtDst, "sync"))
	}
	if err := os.WriteFile(filepath.Join(rtDst, "go.mod"), []byte("module "+rtModulePath+"\n\ngo "+goVer+"\n"), 0666); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
}

// copySupportDir copies the Go sources of one support directory,
// skipping runtime variants, test files, and non-Go files.
func copySupportDir(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if strings.HasPrefix(name, ".") || name == "testdata" {
				continue
			}
			copySupportDir(filepath.Join(src, name), filepath.Join(dst, name))
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_runtime.go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			base.Fatalf("logos eject: %v", err)
		}
		fixed, _, ok := convertGoFile(stripEjectedBuildTag(data))
		if !ok {
			base.Fatalf("logos eject: cannot parse support file %s", name)
		}
		if err := os.MkdirAll(dst, 0777); err != nil {
			base.Fatalf("logos eject: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), fixed, 0666); err != nil {
			base.Fatalf("logos eject: %v", err)
		}
	}
}

// stripEjectedBuildTag removes ejected build lines, so the support copy
// compiles under plain go with no tags set.
func stripEjectedBuildTag(data []byte) []byte {
	lines := bytes.Split(data, []byte("\n"))
	kept := lines[:0]
	for _, l := range lines {
		trimmed := bytes.TrimSpace(l)
		if bytes.Equal(trimmed, []byte("//go:build logosejected")) || bytes.Equal(trimmed, []byte("// +build logosejected")) {
			continue
		}
		kept = append(kept, l)
	}
	return bytes.Join(kept, []byte("\n"))
}

// parseGoModFile reads and parses one go.mod file.
func parseGoModFile(path string) *modfile.File {
	data, err := os.ReadFile(path)
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	return f
}

// writeGoModFile formats one go.mod file back to disk.
func writeGoModFile(path string, f *modfile.File) {
	if err := os.WriteFile(path, modfile.Format(f.Syntax), 0666); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
}

// goVersionOf returns the go directive of f for the support go.mod.
func goVersionOf(f *modfile.File) string {
	if f.Go != nil && f.Go.Version != "" {
		return f.Go.Version
	}
	return "1.27"
}

// rewriteMainGoMod points the ejected main go.mod at every ejected
// dependency module with require plus replace lines.
func rewriteMainGoMod(out string, plan *ejectPlan) {
	path := filepath.Join(out, "go.mod")
	f := parseGoModFile(path)
	for _, m := range plan.deps {
		ensureGoRequire(f, m.path, m.version)
		setGoReplace(f, m.path, m.version, "./_logos_deps/"+m.path+"@"+m.version)
	}
	rebaseLocalReplaces(f, plan.main.dir, out, ejectedPaths(out, plan))
	writeGoModFile(path, f)
}

func ejectedPaths(out string, plan *ejectPlan) map[string]string {
	m := map[string]string{}
	for _, dep := range plan.deps {
		m[dep.path] = filepath.Join(out, "_logos_deps", dep.path+"@"+dep.version)
	}
	return m
}

func rebaseLocalReplaces(f *modfile.File, origDir, dstDir string, ejected map[string]string) {
	absOrig, err := filepath.Abs(origDir)
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	absDst, err := filepath.Abs(dstDir)
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	type pending struct {
		oldPath string
		oldVers string
		newPath string
	}
	var work []pending
	for _, r := range slices.Clone(f.Replace) {
		if _, ok := ejected[r.Old.Path]; ok {
			continue
		}
		if r.New.Version != "" || r.New.Path == "" || filepath.IsAbs(r.New.Path) {
			continue
		}
		target := filepath.Join(absOrig, filepath.FromSlash(r.New.Path))
		if relOrig, err := filepath.Rel(absOrig, target); err == nil && relOrig != ".." && !strings.HasPrefix(relOrig, ".."+string(filepath.Separator)) {
			dstTarget := filepath.Join(absDst, relOrig)
			if fi, err := os.Stat(dstTarget); err == nil && fi.IsDir() {
				target = dstTarget
			}
		}
		rel, err := filepath.Rel(absDst, target)
		if err != nil {
			base.Fatalf("logos eject: %v", err)
		}
		work = append(work, pending{r.Old.Path, r.Old.Version, "./" + filepath.ToSlash(rel)})
	}
	for _, w := range work {
		setGoReplace(f, w.oldPath, w.oldVers, w.newPath)
	}
}

// addSupportRequire points one ejected go.mod at the support module
// with require plus replace lines.
func addSupportRequire(gomodPath, target string) {
	f := parseGoModFile(gomodPath)
	ensureGoRequire(f, rtModulePath, rtRequireVersion)
	setGoReplace(f, rtModulePath, rtRequireVersion, target)
	writeGoModFile(gomodPath, f)
}

// relEjectTarget returns the go.mod replace target from one ejected
// directory to another.
func relEjectTarget(from, to string) string {
	rel, err := filepath.Rel(from, to)
	if err != nil {
		base.Fatalf("logos eject: %v", err)
	}
	return "./" + filepath.ToSlash(rel)
}

// rewriteDepGoMod points one ejected dependency go.mod at every other
// ejected dependency module, and repoints its own replaces of ejected
// modules at the ejected copies.
func rewriteDepGoMod(plan *ejectPlan, m *ejectModule, ejected map[string]string) {
	dst := ejected[m.path]
	f := parseGoModFile(filepath.Join(dst, "go.mod"))
	for _, other := range plan.deps {
		if other.path == m.path {
			continue
		}
		target := relEjectTarget(dst, ejected[other.path])
		ensureGoRequire(f, other.path, other.version)
		setGoReplace(f, other.path, other.version, target)
	}
	var olds [][2]string
	for _, r := range f.Replace {
		if _, ok := ejected[r.Old.Path]; ok {
			olds = append(olds, [2]string{r.Old.Path, r.Old.Version})
		}
	}
	for _, o := range olds {
		setGoReplace(f, o[0], o[1], relEjectTarget(dst, ejected[o[0]]))
	}
	rebaseLocalReplaces(f, m.dir, dst, ejected)
	writeGoModFile(filepath.Join(dst, "go.mod"), f)
}

// ensureGoRequire adds a require line unless one already names path.
func ensureGoRequire(f *modfile.File, path, vers string) {
	for _, r := range f.Require {
		if r.Mod.Path == path {
			return
		}
	}
	if err := f.AddRequire(path, vers); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
}

// setGoReplace points every replace of oldPath at newPath, dropping
// stale targets first so no duplicate replaces remain.
func setGoReplace(f *modfile.File, oldPath, oldVers, newPath string) {
	for _, r := range slices.Clone(f.Replace) {
		if r.Old.Path == oldPath && (r.New.Path != newPath || r.New.Version != "") {
			if err := f.DropReplace(r.Old.Path, r.Old.Version); err != nil {
				base.Fatalf("logos eject: %v", err)
			}
		}
	}
	for _, r := range f.Replace {
		if r.Old.Path == oldPath && r.New.Path == newPath && r.New.Version == "" {
			return
		}
	}
	if err := f.AddReplace(oldPath, oldVers, newPath, ""); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
}
