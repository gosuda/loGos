// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package noder

import (
	"bytes"
	"io"
	"maps"
	"os"
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/logos"
	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/compile/internal/types2"
)

// This file holds the loGo/s lowering integration for the compiler
// frontend. Upstream files carry only hook lines; all logic lives here.
// Lowering runs once after the first successful types2 check, then the
// lowered tree is rechecked as ordinary Go.

// logosStore keeps the original bytes of logos files for lowering and
// eject, plus the compile arguments of stashed files for next reports,
// plus the pending eject edits of a changed package, plus the bytes of
// rewritten cgo user files (M20-S0), which lower but never eject.
var logosStore = struct {
	sync.Mutex
	src   map[*syntax.File][]byte
	cgo   map[*syntax.File][]byte
	names map[*syntax.File]string
	edits map[*syntax.File][]logos.Edit
}{src: make(map[*syntax.File][]byte), cgo: make(map[*syntax.File][]byte), names: make(map[*syntax.File]string)}

// logosStashesGo reports whether Go files keep their original bytes: while a
// next sidecar is armed, while logos lint reads their comments, and in hints
// mode, which converts positions from the bytes the compiler parsed.
func logosStashesGo() bool {
	return base.LogosNextOut != "" || base.LogosLintOnly || base.LogosMode == "hints"
}

// parseLogosFile parses one source file, remembering the original bytes
// of logos files for lowering and eject. Go files are remembered too
// when logosStashesGo reports it.
func parseLogosFile(fbase *syntax.PosBase, f *os.File, filename string, errh syntax.ErrorHandler, pragh syntax.PragmaHandler) *syntax.File {
	mode := syntax.CheckBranches | logosMode(filename)
	cgoUser := isLogosCgoUser(filename) // M20-S0: the rewritten user file of a loGo/s cgo input lowers too.
	if !isLogosFile(filename) && !logosStashesGo() && !cgoUser {
		file, _ := syntax.Parse(fbase, f, errh, pragh, mode)
		return file
	}
	data, err := io.ReadAll(f)
	if err != nil {
		errh(syntax.Error{Msg: err.Error()})
		return nil
	}
	if isLogosFile(filename) {
		data = rewriteLogosWasmDirectives(data, logosMode(filename))
	}
	file, _ := syntax.Parse(fbase, bytes.NewReader(data), errh, pragh, mode)
	if file != nil {
		logosStore.Lock()
		defer logosStore.Unlock()
		// src is ejectable input: logos files, and Go files while a stash
		// consumer is armed. cgo user bytes go to cgo only, so their text
		// never ejects and only lowering consumes them.
		if isLogosFile(filename) || logosStashesGo() {
			logosStore.src[file] = data
			logosStore.names[file] = filename
		}
		if cgoUser {
			logosStore.cgo[file] = data
		}
	}
	return file
}

// logosSources collects the remembered bytes of files, consuming them.
func logosSources(files []*syntax.File) map[*syntax.File][]byte {
	src := make(map[*syntax.File][]byte, len(files))
	logosStore.Lock()
	defer logosStore.Unlock()
	for _, file := range files {
		if data, ok := logosStore.src[file]; ok {
			src[file] = data
			delete(logosStore.src, file)
		}
	}
	return src
}

// logosCgoSources collects the remembered bytes of the rewritten cgo user
// files among files, consuming them.
func logosCgoSources(files []*syntax.File) map[*syntax.File][]byte {
	src := make(map[*syntax.File][]byte)
	logosStore.Lock()
	defer logosStore.Unlock()
	for _, file := range files {
		if data, ok := logosStore.cgo[file]; ok {
			src[file] = data
			delete(logosStore.cgo, file)
		}
	}
	return src
}

// logosProvenReadonlyMap builds THAT first check's sealed proofs for THIS
// invocation only. The caller (logosRecheckHook) stores the fresh map on
// the producer Config it received; irgen captures THAT map into its
// invocation-local carry for THAT immediate lowered recheck. No global,
// no stash/clear authority beyond this holder; on no-recheck the driver
// never carries, so later rechecks see nil and gain no fallback.
func logosProvenReadonlyMap(pkg *types2.Package) map[string]bool {
	handoff := make(map[string]bool)
	if pkg == nil || pkg.Scope() == nil {
		return handoff
	}
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		switch o := obj.(type) {
		case *types2.Func:
			if fn := o.Origin(); fn != nil && fn.LogosReadonly() {
				if k, ok := types2.LogosProvenKey(fn); ok {
					handoff[k] = true
				}
			}
		case *types2.TypeName:
			if n, ok := o.Type().(*types2.Named); ok {
				n = n.Origin()
				for i := range n.NumMethods() {
					if fn := n.Method(i).Origin(); fn != nil && fn.LogosReadonly() {
						if k, ok := types2.LogosProvenKey(fn); ok {
							handoff[k] = true
						}
					}
				}
			}
		}
	}
	return handoff
}

// logosSealProvenCarry takes the proof map the recheck hook sealed on the
// producer Config and returns it for the invocation-local carry in
// checkFiles. The carry is nil on the first check and holds THAT first
// package's map for the lowered recheck and any further rechecks of the
// same package in THIS checkFiles invocation (midway/specialize stamp the
// final package so the Writer keeps unqueried proofs). Local to that
// invocation only; other packages start nil. No global and no package var:
// keeping the carry a checkFiles local keeps one package's proofs out of
// the next package.
func logosSealProvenCarry(conf *types2.Config) map[string]bool {
	return conf.LogosProvenReadonly
}

// logosStampProvenReadonly stamps the carried proofs onto the rechecked
// package. It is a no-op when the carry is empty (no recheck happened).
func logosStampProvenReadonly(pkg *types2.Package, proven map[string]bool) {
	if len(proven) > 0 {
		types2.LogosStampProvenReadonly(pkg, proven)
	}
}

// logosEmitAssertFacts records the package's generic assertion effects into
// the facts sidecar. It runs for Go and loGo/s packages alike at every -O
// level, after types2 checking completes: Lower has already merged its
// opt/const records into facts.Current, so EmitAssertEffects folds the
// assert masks into the same FnFact entries. A plain-Go package gains a
// __.LOGOS member only when assert records exist (see
// gc.writeLogosFacts), and that member carries `lang go` like the
// wasmyield backend marks Go packages, so importers never read it as
// loGo/s.
func logosEmitAssertFacts(pkg *types2.Package) {
	if pkg == nil {
		return
	}
	f := logos.EmitAssertEffects(pkg, facts.Current())
	if f == nil {
		return
	}
	if !base.Logos {
		f.GoPackage = true
	}
	facts.SetCurrent(f)
}

// logosRecheckHook runs the post-check loGo/s rewrite for the package that
// conf checked: lowering for a loGo/s package, MustNonNil wraps for a Go
// package that calls loGo/s code (M3-S7). It reports whether the caller
// must recheck.
func logosRecheckHook(conf *types2.Config, pkg *types2.Package, info *types2.Info, files []*syntax.File) bool {
	logosHintsBegin(pkg, info, files)
	if conf.Logos {
		types2.LogosSealExportedReadonly(pkg, info, files)
		conf.LogosProvenReadonly = logosProvenReadonlyMap(pkg)
		mustRecheck := lowerLogosHook(pkg, info, files)
		return mustRecheck
	}
	logosCgoSources(files) // M20-S0: only a loGo/s package lowers its cgo user files; drop their bytes.
	return logosGoCallerHook(pkg, info, files)
}

// lowerLogosHook lowers logos nodes after the first check and resets type
// checking for the recheck. It reports whether the caller must recheck.
// Sources are stashed for eject even when nothing changed, so unchanged
// logos files still eject as copies.
func lowerLogosHook(pkg *types2.Package, info *types2.Info, files []*syntax.File) bool {
	// M20-S0: the rewritten user file of a cgo input holds Go-syntax code
	// of the package, so its platform-pointer stores lower with the loGo/s
	// files: one source map carries both. Ejectable text stays in src;
	// cgo bytes enter lowerSrc only, so their edits never eject and their
	// bytes never re-enter the store. Check sites in generated files
	// already name the //line-mapped source (SPEC T-202); no post-pass
	// touches them.
	src := logosSources(files)
	lowerSrc := maps.Clone(src)
	maps.Copy(lowerSrc, logosCgoSources(files))
	if len(lowerSrc) == 0 {
		return false
	}
	changed, edits := logos.Lower(pkg, info, files, lowerSrc)
	if changed {
		logos.LogosRestoreErasedAnonReadonly()
	} else {
		logos.LogosClearErasedAnonReadonly()
	}
	for file := range edits {
		if _, ok := src[file]; !ok {
			delete(edits, file)
		}
	}
	logosHintsLowered(files, edits)
	// M2-S7: rewrite value-used unions to methods-only eject text. The
	// native tree is untouched, so this never forces a recheck by
	// itself; it only fails an eject on a dual-use union, with the
	// command prefix added once here.
	if base.LogosEject != "" {
		unionEdits, err := logos.EjectUnionEdits(info, files, src)
		if err != nil {
			base.Fatalf("logos eject: %v", err)
		}
		for file, list := range unionEdits {
			edits[file] = append(edits[file], list...)
		}
	} else {
		for file, list := range logos.NativeUnionEdits(info, files, src) {
			edits[file] = append(edits[file], list...)
		}
	}
	logosStore.Lock()
	defer logosStore.Unlock()
	for file, data := range src {
		logosStore.src[file] = data
	}
	logosStore.edits = edits
	if !changed {
		return false
	}
	for _, p := range types.PkgMap() {
		p.Direct = false
	}
	typecheck.Target.Imports = nil
	return true
}

// logosConfigureOwnedConstantsFirst prepares the first user check: plain
// Go checks and the first user-source check receive no generated-node
// registrations. Proof handoff rides the driver carry into the fresh
// Config, not this hook.
func logosConfigureOwnedConstantsFirst(conf *types2.Config) {
	logosConfigureOwnedConstantsImpl(conf, false)
}

// logosConfigureOwnedConstantsLowered grants generated nodes access to
// fresh private-type references during post-lowering checks.
func logosConfigureOwnedConstantsLowered(conf *types2.Config) {
	logosConfigureOwnedConstantsImpl(conf, true)
}

func logosConfigureOwnedConstantsImpl(conf *types2.Config, lowered bool) {
	if lowered {
		conf.LogosOwnedTypes = logos.OwnedTypeNodes()
	}
}

// writeLogosEject writes ejected Go sources when -logos.eject is set.
func writeLogosEject(files []*syntax.File) {
	dir := base.LogosEject
	if dir == "" {
		return
	}
	logosStore.Lock()
	src := logosStore.src
	edits := logosStore.edits
	logosStore.src = make(map[*syntax.File][]byte)
	logosStore.edits = nil
	logosStore.Unlock()
	if len(src) == 0 {
		return
	}
	if err := logos.Eject(dir, files, src, edits); err != nil {
		base.Fatalf("logos eject: %v", err)
	}
}
