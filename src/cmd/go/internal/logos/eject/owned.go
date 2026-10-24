// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package eject

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"cmd/go/internal/base"
	"cmd/go/internal/cfg"
	"cmd/go/internal/load"
	"cmd/internal/logosowned"
)

// emitOwnedHelpers merges every logos_owned.json sidecar staged by the
// compiler into the copied owner packages. It runs after package copy
// and emission, so generated files land only in the output tree, and
// before go.mod rewriting, so a dependency module pulled in only by an
// owned helper still gets its require and replace lines. It reports
// whether any helper uses the support module.
//
// Three passes:
//  1. resolve each owner to its destination (copying a dependency
//     module if the helper is the first thing that needs it),
//  2. per package: compute each request's effective condition
//     (consumer condition AND its declaration conditions), OR
//     identical-body variants, check declaration collisions against
//     the copied source, and finalize each helper (format, output
//     hash, output name),
//  3. write the helper files.
//
// One owner keeps one package: each request is emitted in its defining
// package and the consumer hosts cross-owner tables, so no import edge
// between unrelated owners is added here.
//
// Consistency: dedupe is idempotent and deterministic (snapshot of the
// staged sidecars sorted before decode and again by owner, package, ID,
// and provenance). There is no exactly-once delivery across compiler
// actions; identical requests deduplicate and same-source distinct
// provenance survives until condition composition merges it.
func emitOwnedHelpers(plan *ejectPlan, staging, out string, ejected map[string]string) (bool, error) {
	reqs, err := gatherOwnedRequests(staging)
	if err != nil {
		return false, err
	}
	if len(reqs) == 0 {
		return false, nil
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return false, fmt.Errorf("resolve output: %v", err)
	}

	// Pass 1 — resolve owners to destinations.
	dests := make([]*ownedDest, 0, len(reqs))
	for i := range reqs {
		req := &reqs[i]
		owner := findOwnedOwner(plan, req.Owner)
		if owner == nil {
			return false, fmt.Errorf("owned request for %q has no ejected destination (standard library packages are not writable)", req.Owner)
		}
		if owner.Standard || owner.Goroot || gorootContains(owner.Dir) {
			return false, fmt.Errorf("owned request for %q has no ejected destination (standard library packages are not writable)", req.Owner)
		}
		if owner.Module == nil {
			return false, fmt.Errorf("owned request for %q has no module", req.Owner)
		}
		if owner.Name == "" || req.Package != owner.Name {
			return false, fmt.Errorf("owned request package %q does not match owner package %q (%s)", req.Package, owner.Name, req.Owner)
		}
		dstRoot, modRoot := ownedOutputRoots(plan, owner, out, abs, ejected)
		rel, err := filepath.Rel(modRoot, owner.Dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false, fmt.Errorf("owned request for %q outside its module", req.Owner)
		}
		dstDir := filepath.Join(dstRoot, rel)
		if err := ownedCheckDir(dstRoot, dstDir); err != nil {
			return false, fmt.Errorf("owned request for %q: %v", req.Owner, err)
		}
		dests = append(dests, &ownedDest{req: req, owner: owner, dstRoot: dstRoot, dstDir: dstDir})
	}

	// Pass 2 — per package: conditions, collisions, final names. One
	// writer per body group comes back (same-body variants absorbed).
	dests, err = applyOwnedConditionsAndCollisions(plan, dests)
	if err != nil {
		return false, err
	}

	// Pass 3 — write final bytes (never the input source: the output
	// hash names the final file independently of the input ID).
	usedRT := false
	for _, d := range dests {
		if _, err := os.Lstat(d.dst); err == nil {
			return false, fmt.Errorf("owned output %q already exists (conflicting generated declarations)", d.dst)
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat owned output %q: %v", d.dst, err)
		}
		if d.usedRT {
			usedRT = true
		}
		if err := os.MkdirAll(d.dstDir, 0777); err != nil {
			return false, fmt.Errorf("make owned directory: %v", err)
		}
		if fi, err := os.Lstat(d.dstDir); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("owned directory %q is not a directory", d.dstDir)
		}
		if err := os.WriteFile(d.dst, d.fixed, 0666); err != nil {
			return false, fmt.Errorf("write owned helper: %v", err)
		}
	}
	return usedRT, nil
}

// ownedDest gathers everything needed to emit one helper file. ID is
// the transport integrity of the input request; name and dst come from
// the hash of the FINAL output source, set in pass 2 along with the
// final bytes and the support-module flag.
type ownedDest struct {
	req     *logosowned.Request
	owner   *load.Package
	dstRoot string
	dstDir  string
	name    string // logos_owned_<outputHash>.go (or _test.go for test-only)
	dst     string
	fixed   []byte // final formatted output bytes (set in pass 2)
	usedRT  bool   // output uses the support module (set in pass 2)
}

// gatherOwnedRequests reads every sidecar under staging. Paths sort
// before decoding, and requests sort again at dedupe time, so the
// merged order is deterministic no matter how consumers compiled.
func gatherOwnedRequests(staging string) ([]logosowned.Request, error) {
	var sidecars []string
	err := filepath.WalkDir(staging, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == logosowned.FileName {
			sidecars = append(sidecars, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gather owned sidecars: %v", err)
	}
	slices.Sort(sidecars)
	var all []logosowned.Request
	for _, sidecar := range sidecars {
		data, err := os.ReadFile(sidecar)
		if err != nil {
			return nil, fmt.Errorf("read owned sidecar: %v", err)
		}
		reqs, err := logosowned.Decode(data)
		if err != nil {
			return nil, fmt.Errorf("decode owned sidecar %s: %v", sidecar, err)
		}
		all = append(all, reqs...)
	}
	return dedupeOwnedRequests(all)
}

// dedupeOwnedRequests drops byte-identical requests and sorts the rest
// by owner, package, content ID, and provenance. The key is owner plus
// content ID plus canonical declarations: the same Source with
// distinct provenance keeps the same ID by contract and must survive
// until condition composition merges same-body variants, so only the
// same owner with the same ID, same source, and same declarations
// deduplicates. The same owner with the same ID but different content
// is a loud conflict, never a silent overwrite.
func dedupeOwnedRequests(reqs []logosowned.Request) ([]logosowned.Request, error) {
	seen := map[string]logosowned.Request{}
	for _, r := range reqs {
		key := r.Owner + "\x00" + r.ID + "\x00" + ownedDeclarationsKey(r.Declarations)
		if prev, ok := seen[key]; ok {
			if !ownedRequestsEqual(prev, r) {
				return nil, fmt.Errorf("conflicting owned requests for owner %q id %s", r.Owner, r.ID)
			}
			continue
		}
		seen[key] = r
	}
	out := make([]logosowned.Request, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b logosowned.Request) int {
		if c := strings.Compare(a.Owner, b.Owner); c != 0 {
			return c
		}
		if c := strings.Compare(a.Package, b.Package); c != 0 {
			return c
		}
		if c := strings.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		return strings.Compare(ownedDeclarationsKey(a.Declarations), ownedDeclarationsKey(b.Declarations))
	})
	return out, nil
}

// ownedDeclarationsKey renders the canonical declaration list for
// dedupe keys and deterministic ordering.
func ownedDeclarationsKey(decls []logosowned.Declaration) string {
	var b strings.Builder
	for _, d := range decls {
		b.WriteString(d.Owner)
		b.WriteByte(0)
		b.WriteString(d.File)
		b.WriteByte(1)
	}
	return b.String()
}

// ownedRequestsEqual compares two requests with slice provenance.
func ownedRequestsEqual(a, b logosowned.Request) bool {
	if a.Owner != b.Owner || a.Package != b.Package || a.ID != b.ID || a.Source != b.Source {
		return false
	}
	if len(a.Declarations) != len(b.Declarations) {
		return false
	}
	for i := range a.Declarations {
		if a.Declarations[i] != b.Declarations[i] {
			return false
		}
	}
	return true
}

// findOwnedOwner resolves an owner import path to the real package
// plan, preferring the non-test package when a test variant shares
// the path. Standard library packages never reach the plan, so a nil
// result names an unsupported destination at the caller.
func findOwnedOwner(plan *ejectPlan, owner string) *load.Package {
	var fallback *load.Package
	consider := func(p *load.Package) *load.Package {
		if p.ImportPath != owner {
			return nil
		}
		if p.ForTest == "" && p.Internal.TestmainGo == nil {
			return p
		}
		if fallback == nil {
			fallback = p
		}
		return nil
	}
	for _, p := range plan.main.pkgs {
		if hit := consider(p); hit != nil {
			return hit
		}
	}
	for _, path := range slices.Sorted(maps.Keys(plan.depByPath)) {
		for _, p := range plan.depByPath[path].pkgs {
			if hit := consider(p); hit != nil {
				return hit
			}
		}
	}
	// Provenance-only packages (standard, test variants) never resolve
	// to a writable destination; they stay read-only for conditions.
	return fallback
}

// ownedFindDeclPackage resolves a declaration owner to its package
// inventory for read-only condition reads. It prefers the non-test
// variant when several share the path and includes standard packages
// from the retained provenance. Emission never writes through this
// path.
func ownedFindDeclPackage(plan *ejectPlan, owner string) *load.Package {
	if plan != nil && plan.provenance != nil {
		if pkgs, ok := plan.provenance[owner]; ok && len(pkgs) > 0 {
			var fallback *load.Package
			for _, p := range pkgs {
				if p.ImportPath != owner {
					continue
				}
				if p.ForTest == "" && p.Internal.TestmainGo == nil {
					return p
				}
				if fallback == nil {
					fallback = p
				}
			}
			if fallback != nil {
				return fallback
			}
		}
	}
	return findOwnedOwner(plan, owner)
}

// ownedOutputRoots returns the output and module roots for owner: the
// main tree for main-module owners, the ejected copy for dependency
// owners. A dependency module with no other ejected content is copied
// now, so the helper still lands in a complete module the go.mod
// rewriting below covers.
func ownedOutputRoots(plan *ejectPlan, owner *load.Package, out, abs string, ejected map[string]string) (dstRoot, modRoot string) {
	if owner.Module.Main {
		return out, plan.main.dir
	}
	m := plan.depByPath[owner.Module.Path]
	if m == nil {
		// Unreachable: findOwnedOwner only returns planned packages.
		base.Fatalf("logos eject: owned request for %q has no module", owner.ImportPath)
	}
	if dst, ok := ejected[m.path]; ok {
		return dst, m.dir
	}
	dst := filepath.Join(out, "_logos_deps", m.path+"@"+m.version)
	ejected[m.path] = dst
	copyModuleTree(m.dir, dst, ejectSkipDir(m.dir, abs, out))
	plan.deps = append(plan.deps, m)
	slices.SortFunc(plan.deps, func(a, b *ejectModule) int { return strings.Compare(a.path, b.path) })
	return dst, m.dir
}

// ownedCheckDir rejects a destination outside dstRoot or behind a
// symlink. Missing trailing components are fine: they are created on
// emission. Existing components must all be real directories.
func ownedCheckDir(dstRoot, dst string) error {
	rel, err := filepath.Rel(dstRoot, dst)
	if err != nil {
		return fmt.Errorf("resolve %q: %v", dst, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q escapes its package", dst)
	}
	if rel == "." {
		return ownedCheckSymlink(dstRoot)
	}
	cur := dstRoot
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := ownedCheckSymlink(cur); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
	}
	return nil
}

// ownedCheckSymlink rejects symlinks and non-directories, so a link
// can never pull outside bytes into the tree or redirect a write.
func ownedCheckSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink", path)
	}
	if !fi.IsDir() && filepath.Ext(path) == "" {
		return fmt.Errorf("%q is not a directory", path)
	}
	return nil
}

// gorootContains reports whether dir sits inside the toolchain root.
// Owned helpers never write there: the coordinator rejects those
// owners before any output path is built.
func gorootContains(dir string) bool {
	goroot := cfg.GOROOT
	if goroot == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(goroot, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// applyOwnedConditionsAndCollisions resolves, for each destination
// package: the effective build condition of every helper, the
// declaration collisions against the copied source, and the final
// output names (hash of the formatted output source). It returns the
// reduced writer list: one destination per body group, same-body
// variants absorbed.
func applyOwnedConditionsAndCollisions(plan *ejectPlan, dests []*ownedDest) ([]*ownedDest, error) {
	groups := map[string][]*ownedDest{}
	var keys []string
	for _, d := range dests {
		if groups[d.dstDir] == nil {
			keys = append(keys, d.dstDir)
		}
		groups[d.dstDir] = append(groups[d.dstDir], d)
	}
	slices.Sort(keys)
	writers := make([]*ownedDest, 0, len(dests))
	for _, dir := range keys {
		groupWriters, err := ownedResolveGroup(plan, dir, groups[dir])
		if err != nil {
			return nil, fmt.Errorf("owned request for %q: %v", groups[dir][0].req.Owner, err)
		}
		writers = append(writers, groupWriters...)
	}
	return writers, nil
}

// ownedResolveGroup analyzes one destination package using explicit
// declaration provenance: no second type checker, no guessed
// identifiers, no all-files scan. Each request's effective condition is
// its consumer condition ANDed with its declaration conditions; then
// identical bodies OR their effective conditions. Collisions and final
// output names are computed here too. It returns one writer per body
// group.
func ownedResolveGroup(plan *ejectPlan, dir string, dests []*ownedDest) ([]*ownedDest, error) {
	pkgName := dests[0].owner.Name

	// Copied owner source for collisions: every .go file whose package
	// clause names the owner package. A file name starting with the
	// generated prefix is not ownership: copied plain-Go source may
	// carry such a name and its declarations must collide with helpers.
	srcDecls := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read owner directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read owner file %s: %v", e.Name(), err)
		}
		f, err := parser.ParseFile(fset, e.Name(), data, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse owner file %s: %v", e.Name(), err)
		}
		if f.Name.Name != pkgName {
			continue // another namespace (ex. sub_test): never collides
		}
		for _, name := range ownedFileDeclNames(f) {
			if name == "_" || name == "init" || strings.Contains(name, ".") {
				continue
			}
			if prev, ok := srcDecls[name]; ok {
				_ = prev
				continue
			}
			srcDecls[name] = e.Name()
		}
	}

	// Helpers of this group, parsed for per-file duplicates.
	for _, d := range dests {
		if _, err := ownedDeclNames(d.req.Owner, []byte(d.req.Source)); err != nil {
			return nil, err
		}
	}

	// Same-body variants share one writer; distinct bodies with the
	// same declaration conflict instead of deduplicating.
	variants, err := ownedGroupVariants(dests)
	if err != nil {
		return nil, err
	}
	merged := map[string]string{}
	for _, group := range variants {
		names, err := ownedDeclNames(group[0].req.Owner, []byte(group[0].req.Source))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if name == "_" || name == "init" || strings.Contains(name, ".") {
				continue
			}
			if prev, ok := merged[name]; ok && prev != group[0].req.ID {
				return nil, fmt.Errorf("owned helpers for %q both declare %q (%s and %s)", group[0].req.Owner, name, prev, group[0].req.ID)
			}
			merged[name] = group[0].req.ID
		}
	}

	// Collisions vs copied source.
	for _, group := range variants {
		names, err := ownedDeclNames(group[0].req.Owner, []byte(group[0].req.Source))
		if err != nil {
			return nil, err
		}
		for _, decl := range names {
			if file, ok := srcDecls[decl]; ok {
				return nil, fmt.Errorf("owned helper for %q declares %q, already declared in %s", group[0].req.Owner, decl, file)
			}
		}
	}

	// Effective condition per body group: per-request consumer AND
	// declarations, then OR across the group's variants. Production and
	// test-only availability stay separate: P is the OR of non-test
	// requests, T is the OR of test requests AND NOT P when production
	// exists. A mixed group keeps its production file instead of
	// collapsing to _test.go, and the test file excludes P so the same
	// body never builds twice when P holds. With unconditional P the
	// extra test file is unnecessary; with no production requests only
	// _test.go is emitted.
	var writers []*ownedDest
	for _, group := range variants {
		slices.SortFunc(group, func(a, b *ownedDest) int {
			if c := strings.Compare(a.req.ID, b.req.ID); c != 0 {
				return c
			}
			return strings.Compare(ownedDeclarationsKey(a.req.Declarations), ownedDeclarationsKey(b.req.Declarations))
		})
		var prodDests []*ownedDest
		var testDests []*ownedDest
		var prodEffs []constraint.Expr
		var testEffs []constraint.Expr
		for _, v := range group {
			eff, test, err := ownedEffectiveCondition(plan, v.req, v.owner)
			if err != nil {
				return nil, err
			}
			if test {
				testDests = append(testDests, v)
				testEffs = append(testEffs, eff)
			} else {
				prodDests = append(prodDests, v)
				prodEffs = append(prodEffs, eff)
			}
		}
		body, err := ownedStripHeader(group[0].req.Source)
		if err != nil {
			return nil, fmt.Errorf("helper %s: %v", group[0].req.ID, err)
		}
		emitOne := func(rep *ownedDest, cond constraint.Expr, test bool) error {
			header := ownedRenderHeader(cond)
			finalSource := header + body
			fixed, uses, ok := convertGoFile([]byte(finalSource))
			if !ok {
				return fmt.Errorf("helper %s does not parse after condition merge", rep.req.ID)
			}
			sum := sha256.Sum256(fixed)
			hash := hex.EncodeToString(sum[:])
			if test {
				rep.name = "logos_owned_" + hash + "_test.go"
			} else {
				rep.name = "logos_owned_" + hash + ".go"
			}
			rep.dst = filepath.Join(rep.dstDir, rep.name)
			rep.fixed = fixed
			rep.usedRT = uses
			writers = append(writers, rep)
			return nil
		}
		if len(prodDests) == 0 {
			final := ownedOrExprs(testEffs)
			if err := emitOne(group[0], final, true); err != nil {
				return nil, err
			}
			continue
		}
		prodCond := ownedOrExprs(prodEffs)
		if err := emitOne(prodDests[0], prodCond, false); err != nil {
			return nil, err
		}
		if len(testDests) == 0 {
			continue
		}
		if prodCond == nil {
			continue
		}
		testOr := ownedOrExprs(testEffs)
		if testOr != nil && prodCond != nil && testOr.String() == prodCond.String() {
			continue
		}
		notProd := &constraint.NotExpr{X: prodCond}
		testCond := ownedAndExprs([]constraint.Expr{testOr, notProd})
		if err := emitOne(testDests[0], testCond, true); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(writers, func(a, b *ownedDest) int {
		return strings.Compare(a.dst, b.dst)
	})
	return writers, nil
}

// ownedEffectiveCondition computes one request's effective condition:
// its consumer condition ANDed with the conditions of its declaration
// files. It also reports whether any declaration is test-only, which
// selects a _test.go output filename. Caller test status never makes
// a dependency helper test-only.
func ownedEffectiveCondition(plan *ejectPlan, req *logosowned.Request, helperOwner *load.Package) (constraint.Expr, bool, error) {
	consumer, err := ownedHeaderExprOf(req.Source)
	if err != nil {
		return nil, false, fmt.Errorf("helper %s: %v", req.ID, err)
	}
	conds := []constraint.Expr{consumer}
	isTest := false
	for _, decl := range req.Declarations {
		cond, test, err := ownedDeclarationCondition(plan, decl.Owner, decl.File, helperOwner)
		if err != nil {
			return nil, false, fmt.Errorf("helper %s declaration %s:%s: %v", req.ID, decl.Owner, decl.File, err)
		}
		conds = append(conds, cond)
		if test {
			isTest = true
		}
	}
	return ownedAndExprs(conds), isTest, nil
}

// ownedDeclarationCondition resolves one provenance entry to the build
// condition of its genuinely selected file. The file must belong to
// the declaration owner's selected source set (GoFiles, CgoFiles, and
// TestGoFiles, with every Logos variant normalized to Go form only for
// copy lookup). Original source is read for conditions. Missing
// or unselected files, missing inventories, and generated cgo outputs
// without a source mapping are explicit errors, never silent omissions.
func ownedDeclarationCondition(plan *ejectPlan, declOwner, declFile string, helperOwner *load.Package) (constraint.Expr, bool, error) {
	pkg := ownedFindDeclPackage(plan, declOwner)
	if pkg == nil {
		return nil, false, fmt.Errorf("declaration owner %q not in package plan", declOwner)
	}
	original, isTest, err := ownedSelectedFile(pkg, plan, declFile)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Join(pkg.Dir, original))
	if err != nil {
		return nil, false, fmt.Errorf("read declaration file %s of %q: %v", original, declOwner, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, original, data, parser.ImportsOnly|parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false, fmt.Errorf("parse declaration file %s of %q: %v", original, declOwner, err)
	}
	if f.Name.Name != pkg.Name {
		return nil, false, fmt.Errorf("declaration file %s of %q has package %q, want %q", original, declOwner, f.Name.Name, pkg.Name)
	}
	cond, err := ownedFileCondition(original, f)
	if err != nil {
		return nil, false, fmt.Errorf("condition of %s: %v", original, err)
	}
	return cond, isTest, nil
}

// ownedSelectedFile maps a declaration basename to its selected source
// entry. It returns the original inventory name and whether the entry
// is test-only.
func ownedSelectedFile(pkg *load.Package, plan *ejectPlan, declFile string) (string, bool, error) {
	if len(pkg.GoFiles) == 0 && len(pkg.CgoFiles) == 0 && len(pkg.TestGoFiles) == 0 && len(pkg.CompiledGoFiles) == 0 && len(pkg.IgnoredGoFiles) == 0 {
		return "", false, fmt.Errorf("owner %q has no source inventory (no all-files scan)", pkg.ImportPath)
	}
	for _, name := range pkg.CompiledGoFiles {
		if name == declFile {
			return "", false, fmt.Errorf("declaration file %q of %q is a generated cgo output with no source mapping (GoFiles=%q CgoFiles=%q CompiledGoFiles=%q)", declFile, pkg.ImportPath, pkg.GoFiles, pkg.CgoFiles, pkg.CompiledGoFiles)
		}
	}
	for _, name := range pkg.IgnoredGoFiles {
		if name == declFile {
			return "", false, fmt.Errorf("declaration file %q of %q is not selected (ignored)", declFile, pkg.ImportPath)
		}
	}
	for _, name := range pkg.GoFiles {
		if name == declFile {
			return name, false, nil
		}
	}
	for _, name := range pkg.CgoFiles {
		if name == declFile {
			return name, false, nil
		}
	}
	for _, name := range pkg.TestGoFiles {
		if name == declFile {
			return name, true, nil
		}
	}
	// A Logos source's copied output is .go while the inventory holds
	// .lgo: normalize only for this lookup, keeping the original for
	// condition reads.
	for _, name := range pkg.GoFiles {
		if cfg.IsLogosFile(name) && logosStem(name)+".go" == declFile {
			return name, false, nil
		}
	}
	for _, name := range pkg.TestGoFiles {
		if cfg.IsLogosFile(name) && logosStem(name)+".go" == declFile {
			return name, true, nil
		}
	}
	return "", false, fmt.Errorf("declaration file %q not in selected sources of %q", declFile, pkg.ImportPath)
}

// ownedGroupVariants groups the destinations of one package by
// canonical body (source with its build header stripped). Groups of
// more than one are same-body variants that differ only in consumer
// headers or provenance: they emit ONE file, whose condition is the OR
// of the variants' effective conditions. Destinations keep their own
// req (input IDs stay intact for diagnostics); the representative is
// the first variant (sorted by input ID, so deterministic). A body
// that fails to strip is an explicit error, never a raw fallback.
func ownedGroupVariants(dests []*ownedDest) ([][]*ownedDest, error) {
	byBody := map[string][]*ownedDest{}
	var bodies []string
	for _, d := range dests {
		body, err := ownedStripHeader(d.req.Source)
		if err != nil {
			return nil, fmt.Errorf("helper %s: %v", d.req.ID, err)
		}
		if byBody[body] == nil {
			bodies = append(bodies, body)
		}
		byBody[body] = append(byBody[body], d)
	}
	slices.Sort(bodies)
	groups := make([][]*ownedDest, 0, len(bodies))
	for _, body := range bodies {
		groups = append(groups, byBody[body])
	}
	return groups, nil
}

// ownedStripHeader removes the build constraint lines before the
// package clause, keeping other leading comments.
func ownedStripHeader(source string) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", []byte(source), parser.PackageClauseOnly)
	if err != nil {
		return "", err
	}
	pkgOffset := fset.Position(f.Package).Offset
	var kept []string
	for _, ln := range strings.Split(source[:pkgOffset], "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || constraint.IsGoBuild(t) || constraint.IsPlusBuild(t) {
			continue
		}
		kept = append(kept, ln)
	}
	if len(kept) == 0 {
		return source[pkgOffset:], nil
	}
	return strings.Join(kept, "\n") + "\n" + source[pkgOffset:], nil
}

// ownedHeaderExprOf parses the build constraints of one helper source.
func ownedHeaderExprOf(source string) (constraint.Expr, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "owned.go", []byte(source), parser.ParseComments)
	if err != nil {
		return nil, err
	}
	return ownedHeaderExpr(f)
}

// ownedGoosTags and ownedGoarchTags are the platform suffixes a file
// name may carry, sorted for deterministic iteration.
var ownedGoosTags = []string{"aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "windows", "zos"}

var ownedGoarchTags = []string{"386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le", "mipsle", "ppc64", "ppc64le", "riscv", "riscv64", "s390x", "sparc64", "wasm"}

// ownedFileCondition derives the build condition of one Go file: its
// explicit //go:build / // +build lines, the implicit GOOS/GOARCH tags
// of its file name, and the cgo tag if it imports "C". nil means the
// file builds in every configuration.
func ownedFileCondition(name string, f *ast.File) (constraint.Expr, error) {
	var parts []constraint.Expr
	h, err := ownedHeaderExpr(f)
	if err != nil {
		return nil, err
	}
	if h != nil {
		parts = append(parts, h)
	}
	for _, tag := range ownedFilenameTags(name) {
		parts = append(parts, &constraint.TagExpr{Tag: tag})
	}
	if ownedHasCgo(f) {
		parts = append(parts, &constraint.TagExpr{Tag: "cgo"})
	}
	return ownedAndExprs(parts), nil
}

// ownedHeaderExpr parses the build constraints declared before the
// package clause. A //go:build line wins over // +build lines; several
// legacy lines AND together.
func ownedHeaderExpr(f *ast.File) (constraint.Expr, error) {
	var modern, legacy constraint.Expr
	for _, cg := range f.Comments {
		if cg.Pos() > f.Pos() {
			break
		}
		for _, c := range cg.List {
			switch {
			case constraint.IsGoBuild(c.Text):
				expr, err := constraint.Parse(c.Text)
				if err != nil {
					return nil, err
				}
				modern = expr
			case constraint.IsPlusBuild(c.Text):
				expr, err := constraint.Parse(c.Text)
				if err != nil {
					return nil, err
				}
				if legacy == nil {
					legacy = expr
				} else {
					legacy = &constraint.AndExpr{X: legacy, Y: expr}
				}
			}
		}
	}
	if modern != nil {
		return modern, nil
	}
	return legacy, nil
}

// ownedFilenameTags returns the platform tags implied by a file name:
// name_GOOS_GOARCH.go, name_GOOS.go and name_GOARCH.go follow the go
// tool's file name rules. Every Logos variant owned by cfg.IsLogosFile
// normalizes through logosStem to Go form only for this suffix
// analysis and copy lookup; condition reads use the original source.
func ownedFilenameTags(name string) []string {
	var base string
	if cfg.IsLogosFile(name) {
		base = logosStem(name)
	} else {
		base = strings.TrimSuffix(name, ".go")
	}
	base = strings.TrimSuffix(base, "_test")
	for _, goarch := range ownedGoarchTags {
		if !strings.HasSuffix(base, "_"+goarch) {
			continue
		}
		for _, goos := range ownedGoosTags {
			if strings.HasSuffix(base, "_"+goos+"_"+goarch) {
				return []string{goos, goarch}
			}
		}
		return []string{goarch}
	}
	for _, goos := range ownedGoosTags {
		if strings.HasSuffix(base, "_"+goos) {
			return []string{goos}
		}
	}
	return nil
}

// ownedHasCgo reports whether the file imports "C".
func ownedHasCgo(f *ast.File) bool {
	for _, imp := range f.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "C" {
			return true
		}
	}
	return false
}

// ownedAndExprs ANDs conditions, dropping duplicates and nils. Nil is
// the true condition, so AND drops it.
func ownedAndExprs(conds []constraint.Expr) constraint.Expr {
	return ownedCombineAnd(conds)
}

// ownedOrExprs ORs conditions, dropping duplicates. Nil is the true
// condition, so any nil OR makes the whole OR true (nil).
func ownedOrExprs(conds []constraint.Expr) constraint.Expr {
	for _, c := range conds {
		if c == nil {
			return nil
		}
	}
	seen := map[string]bool{}
	var uniq []constraint.Expr
	for _, c := range conds {
		if c == nil {
			continue
		}
		if s := c.String(); seen[s] {
			continue
		} else {
			seen[s] = true
		}
		uniq = append(uniq, c)
	}
	var out constraint.Expr
	for _, c := range uniq {
		if out == nil {
			out = c
			continue
		}
		out = &constraint.OrExpr{X: out, Y: c}
	}
	return out
}

// ownedCombineAnd folds conditions with AND, dropping duplicates and
// nils.
func ownedCombineAnd(conds []constraint.Expr) constraint.Expr {
	seen := map[string]bool{}
	var uniq []constraint.Expr
	for _, c := range conds {
		if c == nil {
			continue
		}
		if s := c.String(); seen[s] {
			continue
		} else {
			seen[s] = true
		}
		uniq = append(uniq, c)
	}
	var out constraint.Expr
	for _, c := range uniq {
		if out == nil {
			out = c
			continue
		}
		out = &constraint.AndExpr{X: out, Y: c}
	}
	return out
}

// ownedRenderHeader renders a normalized build header: the //go:build
// line plus its legacy // +build rendering. Empty condition means no
// header at all.
func ownedRenderHeader(expr constraint.Expr) string {
	if expr == nil {
		return ""
	}
	header := "//go:build " + expr.String() + "\n"
	if lines, err := constraint.PlusBuildLines(expr); err == nil {
		header += strings.Join(lines, "\n") + "\n"
	}
	return header + "\n"
}

// ownedDeclNames collects the top-level declared identifiers of a
// rewritten helper: named types, values, functions, and methods
// qualified by their receiver so overloads across types stay distinct.
// A duplicate name inside one file cannot compile, so it fails here
// with the owner context instead of after output. Repeated blank
// identifiers and package-level init functions stay legal Go and pass.
func ownedDeclNames(owner string, data []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "owned.go", data, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse generated helper: %v", err)
	}
	var names []string
	seen := map[string]bool{}
	bump := func(name string) error {
		// Repeated blank identifiers and package-level init functions
		// are legal Go: neither leaves the generated namespace, so
		// neither can collide with copied source or other helpers.
		if name == "_" || name == "init" {
			return nil
		}
		if seen[name] {
			return fmt.Errorf("owned helper for %q declares %q twice", owner, name)
		}
		seen[name] = true
		names = append(names, name)
		return nil
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				if err := bump(d.Name.Name); err != nil {
					return nil, err
				}
				continue
			}
			if err := bump(ownedReceiver(d.Recv.List) + "." + d.Name.Name); err != nil {
				return nil, err
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if err := bump(s.Name.Name); err != nil {
						return nil, err
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if err := bump(name.Name); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}
	return names, nil
}

// ownedFileDeclNames lists the top-level declared identifiers of one
// already-valid file (no duplicate check).
func ownedFileDeclNames(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				names = append(names, d.Name.Name)
				continue
			}
			names = append(names, ownedReceiver(d.Recv.List)+"."+d.Name.Name)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				case *ast.ValueSpec:
					for _, name := range s.Names {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	return names
}

// ownedReceiver names the base type of a method receiver.
func ownedReceiver(fields []*ast.Field) string {
	if len(fields) == 0 {
		return ""
	}
	t := fields[0].Type
	for {
		switch v := t.(type) {
		case *ast.StarExpr:
			t = v.X
		case *ast.IndexExpr:
			t = v.X
		case *ast.IndexListExpr:
			t = v.X
		default:
			return ownedTypeName(t)
		}
	}
}

// ownedTypeName renders the dotted path of a receiver base type.
func ownedTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return ownedTypeName(v.X) + "." + v.Sel.Name
	default:
		return ""
	}
}
