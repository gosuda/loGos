// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"fmt"
	"internal/pkgbits"
	"os"

	"cmd/compile/internal/base"
	"cmd/compile/internal/importer"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
)

// logosDepVerifiedFacts returns dep's fingerprint-verified facts record, or
// nil when dep is unresolvable or carries no sidecar. It is the one loader
// for every preload predicate so probe and decode share verification:
// unavailable importcfg entries skip, errors in present archives propagate,
// a missing member means no facts, and a stale fingerprint fails. It reuses
// facts.Import, the same fingerprint-bound decode the importer records, so
// the two predicates below cannot diverge on stale archives. GoPackage
// never implies absence.
func logosDepVerifiedFacts(dep string) (*facts.Facts, error) {
	if !logosDepResolvable(dep) {
		return nil, nil
	}
	f, err := openPackage(dep)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := readExportData(f)
	if err != nil {
		return nil, err
	}
	member, err := facts.FindMember(f)
	if err != nil || member == nil {
		return nil, err
	}
	rec, err := facts.Import(dep, data, member)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// logosDepWillStructurallyClone distinguishes an unavailable owner from
// an available archive that fails verification. Only unavailable importcfg
// entries may be skipped; errors in present archives propagate.
func logosDepWillStructurallyClone(dep string) (bool, error) {
	rec, err := logosDepVerifiedFacts(dep)
	if err != nil || rec == nil {
		return false, err
	}
	return len(rec.Opts) != 0 || len(rec.Consts) != 0, nil
}

func logosDepResolvable(dep string) bool {
	if base.Flag.Cfg.PackageFile == nil {
		return true
	}
	return base.Flag.Cfg.PackageFile[dep] != ""
}

func logosHasStructuralFacts(f *facts.Facts) bool {
	return f != nil && (len(f.Opts) > 0 || len(f.Consts) > 0)
}

// logosFnsNeedPreload reports whether fns carry declaration-only facts that
// must be present before dependents decode: a readonly capability or a known
// assertion effect. Records without either need nothing.
func logosFnsNeedPreload(fns []facts.FnFact) bool {
	for _, fn := range fns {
		if fn.Readonly || fn.HasAssert {
			return true
		}
	}
	return false
}

// logosDepNeedsPreload reports whether dep must be imported before a
// dependent decodes, even when it will not structurally clone. Structural
// (Opts/Consts) deps clone for lgo views; readonly (Readonlys) and effect
// (Fns.Readonly/HasAssert) deps stamp in place without cloning or packages
// replacement, but the dependent must still decode against the annotated
// canonical owner. Verification mirrors logosDepWillStructurallyClone:
// unavailable entries skip, present-archive errors propagate, a missing
// member means no facts, and a stale fingerprint fails. GoPackage never
// implies absence: assert-only Go members still need preload.
func logosDepNeedsPreload(dep string) (bool, error) {
	rec, err := logosDepVerifiedFacts(dep)
	if err != nil || rec == nil {
		return false, err
	}
	return len(rec.Opts) != 0 || len(rec.Consts) != 0 || len(rec.Readonlys) != 0 || logosFnsNeedPreload(rec.Fns), nil
}

// readLogosFacts imports the package's verified facts record. The
// returned record is the one fingerprint-bound to THIS decode; callers
// pass it on so annotation never re-reads the global, overwritable
// registry entry.
func readLogosFacts(f *os.File, path, data string) (*facts.Facts, error) {
	member, err := facts.FindMember(f)
	if err != nil {
		return nil, err
	}
	if member == nil {
		return nil, nil
	}
	rec, err := facts.Import(path, data, member)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// logosImporterState records the importer configuration: isLogos preserves
// execution-language identity (keeping dual-use unions legal across recheck),
// while restoreOverlay controls whether first-pass Option/ConstView qualifiers
// are restored onto imported packages.
type logosImporterState struct {
	isLogos        bool
	restoreOverlay bool
}

// logosImporters records the importer state serving loGo/s packages. It is
// keyed by importer because gcimports is an upstream type that takes
// hook lines only. A missing entry means a Go importer: it rejects a
// dual-use union at the import (M2-S7), while a loGo/s importer keeps
// the full union.
var logosImporters = make(map[*gcimports]logosImporterState)

// logosMarkImporter records whether m imports for a loGo/s package and whether
// overlays should be restored. It sets the entry unconditionally so a recycled
// importer address never inherits a stale kind across typecheck rechecks.
func logosMarkImporter(m *gcimports, isLogos, restoreOverlay bool) {
	logosImporters[m] = logosImporterState{
		isLogos:        isLogos,
		restoreOverlay: restoreOverlay,
	}
}

// logosImportFrom imports path for m, selecting the package kind and overlay
// restoration from the importer mark: only the loGo/s kind keeps dual-use
// unions legal, while overlay restoration is disabled during lowered recheck.
func logosImportFrom(m *gcimports, path string) (*types2.Package, error) {
	st := logosImporters[m]
	_, pkg, err := readImportFileImpl(path, typecheck.Target, m.ctxt, m.packages, st.isLogos, st.restoreOverlay)
	return pkg, err
}

// preloadLogosOwners imports every package named by pr's SectionPkg table
// before pr itself decodes, so foreign nominals resolve to the canonical
// owner object. Without this, importing b before a captures a's types as
// lowered stubs from b's export; the later overlay of a replaces
// packages[a] with a clone, leaving b.Make's result (stale a.Box) distinct
// from the canonical a.Box. With the preload, b decodes against the already
// overlaid a, and both import orders share one Named/alias/instance origin.
// Generic instances also canonicalize because instantiation uses the
// preloaded origin.
func preloadLogosOwners(target *ir.Package, env *types2.Context, packages map[string]*types2.Package, pr *pkgbits.PkgDecoder, selfPath string, st logosImporterState) error {
	if packages == nil || pr == nil {
		return nil
	}
	n := pr.NumElems(pkgbits.SectionPkg)
	seen := make(map[string]struct{}, n)
	for i := range n {
		dep := pr.PeekPkgPath(pkgbits.RelElemIdx(i))
		if dep == "" || dep == "builtin" || dep == "unsafe" {
			continue
		}
		if dep == selfPath {
			continue
		}
		if _, ok := seen[dep]; ok {
			continue
		}
		seen[dep] = struct{}{}
		if pkg, ok := packages[dep]; ok && pkg.Complete() {
			continue
		}
		if !logosDepResolvable(dep) {
			continue
		}
		if !st.restoreOverlay {
			need, err := logosDepNeedsPreload(dep)
			if err != nil {
				return err
			}
			if !need {
				continue
			}
		}
		if _, _, err := readImportFileImpl(dep, target, env, packages, st.isLogos, st.restoreOverlay); err != nil {
			return err
		}
	}
	return nil
}

func readImportFileImpl(path string, target *ir.Package, env *types2.Context, packages map[string]*types2.Package, isLogos, restoreOverlay bool) (pkg1 *types.Pkg, pkg2 *types2.Package, err error) {
	path, err = resolveImportPath(path)
	if err != nil {
		return
	}

	if path == "unsafe" {
		pkg1, pkg2 = types.UnsafePkg, types2.Unsafe

		// TODO(mdempsky): Investigate if this actually matters. Why would
		// the linker or runtime care whether a package imported unsafe?
		if !pkg1.Direct {
			pkg1.Direct = true
			target.Imports = append(target.Imports, pkg1)
		}

		return
	}

	pkg1 = types.NewPkg(path, "")
	if packages != nil {
		pkg2 = packages[path]
		if !(pkg1.Direct == (pkg2 != nil && pkg2.Complete())) {

			base.Fatalf("pkg1.Direct == (pkg2 != nil && pkg2.Complete()), path=%s\npackages=%v", path, packages)
		}
	}

	if pkg1.Direct {
		return
	}
	pkg1.Direct = true
	target.Imports = append(target.Imports, pkg1)

	f, err := openPackage(path)
	if err != nil {
		return
	}
	defer f.Close()

	data, err := readExportData(f)
	if err != nil {
		return
	}
	var logosFacts *facts.Facts
	if logosFacts, err = readLogosFacts(f, path, data); err != nil {
		return
	}

	if base.Debug.Export != 0 {
		fmt.Printf("importing %s (%s)\n", path, f.Name())
	}

	pr := pkgbits.NewPkgDecoder(pkg1.Path, data)

	// Hydrate owners before the dependent decodes: the unified export
	// carries only the dependent's lowered view of foreign nominals, so
	// decoding B first would capture A as a stale *types2.Named. The
	// later overlay of A clones into a new package, orphaning B's
	// signatures (reverse order fails while forward works). Preloading
	// every SectionPkg dependency with the same importer kind makes the
	// canonical overlaid A the object B's lazy scopes resolve to, so
	// both orders share one Named/alias/generic identity. The first-pass
	// loGo/s view (restoreOverlay) preloads every dependency; Go
	// importers preload every dependency carrying facts
	// (see logosDepNeedsPreload): structural deps clone for lgo views
	// while declaration-only facts stamp metadata in place and never
	// replace the package, but the dependent must still decode against
	// the annotated canonical owner in both cases.
	if err = preloadLogosOwners(target, env, packages, &pr, path, logosImporterState{isLogos: isLogos, restoreOverlay: restoreOverlay}); err != nil {
		return
	}

	// The union-import check needs the package scope, but resolving a
	// foreign lazy scope tripped importer asserts. Gate it on the
	// export naming a loGo/s source file, which plain-Go archives
	// never do; the sidecar check additionally requires the transport
	// to be intact.
	checkLogosUnion := !isLogos && logosFacts != nil && logosExportHasLogosFile(&pr)

	// Read package descriptors for both types2 and compiler backend.
	readPackage(newPkgReader(pr), pkg1, false)
	pkg2 = importer.ReadPackage(env, packages, pr)
	if pkg2 != nil && restoreOverlay {
		if pkg2, err = applyLogosOverlayForLgoImporter(path, pkg2, packages, logosFacts); err != nil {
			return
		} // logos M3-S7 hook: .lgo importers see Option types.
	} else if pkg2 != nil && logosFacts != nil {
		if pkg2, err = applyLogosReadonlyForGoImporter(path, pkg2, packages, logosFacts); err != nil {
			return
		}
	}
	if pkg2 != nil && checkLogosUnion {
		checkLogosGoImport(pkg2)
	}

	err = addFingerprint(path, data)
	return
}
