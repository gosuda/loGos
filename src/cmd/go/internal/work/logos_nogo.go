// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"cmd/go/internal/base"
	"cmd/go/internal/cache"
	"cmd/go/internal/cfg"
	"cmd/go/internal/fsys"
	"cmd/go/internal/load"
	"cmd/go/internal/logos/config"
	"cmd/go/internal/modload"
	"cmd/go/internal/str"
)

// This file holds the logos nogo integration (cmd/go side of lib/logos-nogo,
// module logos/nogo). The helper lives at $GOROOT/lib/logos-nogo (full
// absolute path via cfg.GOROOT, never a W shorthand); cmd/go builds it once
// for the host, then runs it once per linted package with export data,
// archives, and dependency facts.
//
// Wiring: logos lint (src/cmd/go/internal/logos/lintcmd/lint.go runLint)
// adds a NogoAction sibling beside each LintAction for every LintsPackage.
// LintAction keeps the native compiler lints; NogoAction runs the helper's
// pinned per-package selection (logosPinnedAnalyzers plus Requires closure;
// binary analyzers are skipped when cmd/go passes no -archive) over the
// package's sources (ejected Go for loGo/s, original Go for Go) and
// normalizes raw helper findings on the cmd/go side: rule IDs as
// nogo::<lowercase-analyzer> (digits kept, e.g. nogo::sa1019), severity from
// the [lints] nogo group (default deny, Go forbid lowers to deny),
// //logos:allow(nogo::...) suppression with the existing scope semantics,
// and text/JSON output through the existing logos diagnostic shape
// (warning prefix in text, schema-1.0 JSON under --json-diagnostics).
// No helper policy flags are passed; the helper runs raw and cmd/go
// resolves policy, avoiding redundant flag plumbing.
//
// Position mapping for loGo/s is exact by construction: the compiler
// writes <ejected>.go.edits (cmd/compile/internal/logos.EditMap, version 2)
// beside each ejected file, and its segments partition the ejected text
// into verbatim copies of original byte ranges and compiler-generated
// runs. A finding whose physical offset lands in a copy segment maps to
// the exact original offset; one inside a generated run is a compiler-only
// finding and is dropped. Suppression runs in original coordinates:
// //logos:allow directives are collected from the original loGo/s source
// (also those inside replaced spans, which never survive ejection), and
// findings are matched after mapping. The helper reports both the
// adjusted position (display, //line applied) and a physical position
// (file and byte identity); mapping and file keying use the physical
// side, so a user //line directive cannot misroute a finding. A finding
// with no position metadata at all (package-level analyzer errors) is
// kept as reported: no invented origin, no silent drop. Unreadable or
// mismatched maps are hard errors, never silent mis-positioning.
//
// Policy paths (owned by the policy owner, not invented here):
// src/cmd/go/internal/logos/config/lints.go:19-59 (ten groups incl nogo,
// native PolicyJSON filters nogo), src/cmd/compile/internal/logos/lint/policy.go:50-112
// (native ParsePolicy rejects unknown groups), and
// src/cmd/compile/internal/base/logos_diag.go:75-153 (JSON schema needs
// resolved severity+span). This file reads the configured nogo severity via
// config.Load (same parser the load phase uses) and defaults to deny.

// nogoModuleDir returns the helper module directory as a full absolute path.
func nogoModuleDir() string {
	return filepath.Join(logosNogoWPrefixFull(), "lib", "logos-nogo")
}

func nogoGoBinary() string {
	return filepath.Join(logosNogoWPrefixFull(), "bin", "go"+cfg.ToolExeSuffix())
}

// nogoSourcesHash hashes the full helper source tree plus go.mod/go.sum.
// nogoSourcesHash mirrors lib/logos-nogo/cmd/logos-nogo/sourcehash.go SourceHash
// exactly (slash-normalized paths, _go.mod mapping, duplicate rejection).
// Runtime prefers the helper -source-hash API; this fallback covers cold
// source checkouts with no helper binary yet and -n dry runs.
func nogoSourcesHash() (string, error) {
	root := nogoModuleDir()
	if info, err := os.Stat(root); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", fmt.Errorf("logos nogo: source root %q is not a directory", root)
	}
	type nogoEntry struct {
		norm string
		full string
	}
	var entries []nogoEntry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		norm := filepath.ToSlash(rel)
		if norm == "_go.mod" {
			norm = "go.mod"
		} else if dir, ok := strings.CutSuffix(norm, "/_go.mod"); ok {
			norm = dir + "/go.mod"
		}
		entries = append(entries, nogoEntry{norm: norm, full: path})
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.SortFunc(entries, func(a, b nogoEntry) int {
		return strings.Compare(a.norm, b.norm)
	})
	seen := make(map[string]string, len(entries))
	h := sha256.New()
	for _, e := range entries {
		if prev, dup := seen[e.norm]; dup {
			return "", fmt.Errorf("logos nogo: duplicate normalized path %q (%q and %q)", e.norm, prev, e.full)
		}
		seen[e.norm] = e.full
		data, err := os.ReadFile(e.full)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "file %s %x\n", e.norm, sha256.Sum256(data))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// nogoHelperPath returns the persistent host helper binary path for hash.
func nogoHelperPath(hash string) (string, error) {
	dir, _, err := cache.DefaultDir()
	if err != nil {
		return "", err
	}
	if dir == "off" {
		return "", fmt.Errorf("logos nogo: build cache disabled (GOCACHE=off)")
	}
	return filepath.Join(dir, "logos-nogo", hash, "logos-nogo"+cfg.ToolExeSuffix()), nil
}

func nogoPrebuiltPath() string {
	return filepath.Join(cfg.GOROOTpkg, "tool", runtime.GOOS+"_"+runtime.GOARCH, "logos-nogo"+cfg.ToolExeSuffix())
}

func nogoDigestViaHelper(sh *Shell, helperBin, root string) (string, error) {
	out, err := sh.runOut(root, nil, helperBin, "-source-hash", root)
	if err != nil {
		return "", err
	}
	d := strings.TrimSpace(string(out))
	if len(d) != 64 {
		return "", fmt.Errorf("logos nogo: bad -source-hash output %q", d)
	}
	return d, nil
}

func nogoStageModule(root, objdir string) (string, error) {
	stage := filepath.Join(objdir, "stage")
	if err := os.MkdirAll(stage, 0777); err != nil {
		return "", err
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(stage, rel), 0777)
		}
		destRel := rel
		if filepath.Base(rel) == "_go.mod" {
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), "go.mod")); err == nil {
				return nil
			}
			if d := filepath.Dir(rel); d == "." {
				destRel = "go.mod"
			} else {
				destRel = filepath.Join(d, "go.mod")
			}
		}
		return nogoCopyFile(filepath.Join(stage, destRel), path, info.Mode())
	})
	if err != nil {
		return "", err
	}
	return stage, nil
}

func nogoCopyFile(dst, src string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0777); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	err = out.Close()
	if cerr != nil {
		return cerr
	}
	return err
}

// nogoBootstrapAction builds the host helper once, cached by full source +
// module lock (go.mod/go.sum are part of the source hash) + host tool ID.
// The binary is published atomically (temp file + rename) and reused across
// builds. It obeys -n/-x via sh.runOut, never touches the network at build
// time (GOPROXY=off, GOSUMDB=off, GOWORK=off, GOTOOLCHAIN=local,
// -mod=readonly), and never falls back silently: missing modules are an
// explicit error naming the one-time official fetch.
func (b *Builder) nogoBootstrapAction() *Action {
	return b.cacheAction("logos nogo bootstrap", nil, func() *Action {
		return &Action{
			Mode:   "logos nogo bootstrap",
			Actor:  nogoBootstrapActor{},
			Objdir: b.NewObjdir(),
		}
	})
}

type nogoBootstrapActor struct{}

func (nogoBootstrapActor) Act(b *Builder, ctx context.Context, a *Action) error {
	sh := b.Shell(a)
	if err := sh.Mkdir(a.Objdir); err != nil {
		return err
	}
	srcHash, err := nogoSourcesHash()
	if err != nil {
		return err
	}
	prebuilt := nogoPrebuiltPath()
	if _, err := os.Stat(prebuilt); err == nil && !cfg.BuildN {
		if d, err := nogoDigestViaHelper(sh, prebuilt, nogoModuleDir()); err == nil {
			if d != srcHash {
				if cfg.BuildX {
					sh.ShowCmd("", "echo logos-nogo prebuilt stale, rebuilding from source")
				}
			} else if data, err := os.ReadFile(prebuilt + ".digest"); err == nil && strings.TrimSpace(string(data)) == srcHash {
				a.built = prebuilt
				if cfg.BuildX {
					sh.ShowCmd("", "echo logos-nogo %s prebuilt", prebuilt)
				}
				return nil
			}
		} else if cfg.BuildX {
			sh.ShowCmd("", "echo logos-nogo prebuilt unusable, rebuilding from source")
		}
	}
	if cfg.BuildN {
		if _, err := os.Stat(prebuilt); err == nil {
			if data, err := os.ReadFile(prebuilt + ".digest"); err == nil && strings.TrimSpace(string(data)) == srcHash {
				a.built = prebuilt
				sh.ShowCmd("", "echo logos-nogo %s prebuilt", prebuilt)
				return nil
			}
		}
	}
	toolID := b.toolID("compile")
	h := sha256.New()
	fmt.Fprintf(h, "sources %s\n", srcHash)
	fmt.Fprintf(h, "host %s %s\n", runtime.Version(), toolID)
	hash := hex.EncodeToString(h.Sum(nil))
	bin, err := nogoHelperPath(hash)
	if err != nil {
		return err
	}
	a.built = bin
	if _, err := os.Stat(bin); err == nil {
		if cfg.BuildN || cfg.BuildX {
			sh.ShowCmd("", "echo logos-nogo %s cached", bin)
		}
		return nil
	}
	if cfg.BuildN {
		sh.ShowCmd(nogoModuleDir(), "%s build -mod=readonly -o %s ./cmd/logos-nogo", nogoGoBinary(), bin)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(bin), "logos-nogo-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Close()
	os.Remove(tmpName)
	stage, err := nogoStageModule(nogoModuleDir(), a.Objdir)
	if err != nil {
		return err
	}
	goBin := nogoGoBinary()
	env := []string{
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GOOS=" + runtime.GOOS,
		"GOARCH=" + runtime.GOARCH,
	}
	out, err := sh.runOut(stage, env, goBin, "build", "-mod=readonly", "-o", tmpName, "./cmd/logos-nogo")
	if err != nil {
		os.Remove(tmpName)
		if isNogoMissingModules(out) {
			return fmt.Errorf("logos nogo bootstrap: missing modules (offline build): %s; one-time fetch with network: cd %s && GOFLAGS=-mod=mod GOSUMDB=sum.golang.org go mod tidy && go mod download all (official proxy only)",
				strings.TrimSpace(string(out)), nogoModuleDir())
		}
		if cfg.LogosJSON {
			// --json-diagnostics promises one JSON object per stderr line;
			// the `# logos nogo bootstrap` header would break that contract
			// for every JSON consumer. Report the failure plainly instead.
			// The failure and the nonzero exit are preserved: nothing about
			// the bootstrap outcome is masked, only the decoration is gated.
			if len(strings.TrimSpace(string(out))) != 0 {
				return fmt.Errorf("logos nogo bootstrap: %s", strings.TrimSpace(string(out)))
			}
			return err
		}
		return sh.reportCmd("logos nogo bootstrap", stage, out, err)
	}
	if cfg.BuildX {
		sh.ShowCmd(stage, "%s build -mod=readonly -o %s ./cmd/logos-nogo", goBin, bin)
	}
	if err := os.Chmod(tmpName, 0755); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, bin); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func isNogoMissingModules(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "missing go.sum entry") || strings.Contains(s, "no required module provides") ||
		strings.Contains(s, "cannot find module") || strings.Contains(s, "module lookup disabled")
}

// nogoEjectAction compiles a loGo/s package with -logos.eject into its
// Objdir/eject (plus -logos.lintonly, no object). The eject dir path is
// a.built for NogoAction to consume.
func (b *Builder) nogoEjectAction(p *load.Package) *Action {
	return b.cacheAction("logos nogo eject", p, func() *Action {
		a := &Action{
			Mode:    "logos nogo eject",
			Package: p,
			Actor:   nogoEjectActor{},
			Objdir:  b.NewObjdir(),
		}
		if p.Error == nil || !p.Error.IsImportCycle {
			for _, p1 := range p.Internal.Imports {
				a.Deps = append(a.Deps, b.CompileAction(ModeBuild, ModeBuild, p1))
			}
		}
		return a
	})
}

type nogoEjectActor struct{}

func (nogoEjectActor) Act(b *Builder, ctx context.Context, a *Action) error {
	p := a.Package
	sh := b.Shell(a)
	if err := sh.Mkdir(a.Objdir); err != nil {
		return err
	}
	embed, err := lintEmbedConfig(p)
	if err != nil {
		return err
	}
	icfg := lintImportConfig(a)
	ejectDir := a.Objdir + "eject"
	if err := sh.Mkdir(ejectDir); err != nil {
		return err
	}
	_, _, gcErr := BuildToolchain.gc(b, a, "", icfg, embed, "", false, "", str.StringList(p.GoFiles))
	// The eject compile is a file producer for the helper, not a diagnostic
	// reporter: the sibling LintAction compiles the same package and already
	// reports its native diagnostics (text or --json-diagnostics). Printing
	// the eject output here would duplicate every finding (logos_lint_routed
	// counts each warning once; logos_lint_nil_deref counts each JSON record
	// once). Eject writes its sources during lowering (noder/irgen.go
	// writeLogosEject), before lint resolution stops the compile, so lint
	// failures still leave current files behind; only a missing eject
	// directory is a real infrastructure failure worth reporting.
	entries, readErr := os.ReadDir(ejectDir)
	if readErr != nil {
		if gcErr != nil {
			return sh.reportCmd("", "", nil, gcErr)
		}
		return readErr
	}
	hasGo := false
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".edits") {
			hasGo = true
			break
		}
	}
	if !hasGo {
		if gcErr != nil {
			return sh.reportCmd("", "", nil, gcErr)
		}
		return fmt.Errorf("logos nogo: eject produced no Go sources for %s", p.ImportPath)
	}
	a.built = ejectDir
	return nil
}

// NogoAction runs the helper over one linted package. Deps are the host
// helper, the compiles of direct imports (export data), the NogoActions of
// linted direct imports (facts), the compile of p itself (archive for the
// binary analyzer and export correlation), the compile of internal/abi
// (export data for the helper hard import, including zero-import leaves),
// and for loGo/s the eject action.
// The archive compile type-checks p as well, so under --json-diagnostics
// its native type errors duplicate the lint compile's; consumers of the
// merged stream collapse those rule-less exact duplicates. The dep stays:
// its failure keeps the helper off code that does not compile.
// It never sets IgnoreFail: a failed fact input (helper error, unreadable
// map/facts) must stop dependents rather than report false clean. Findings
// with facts still written are reported via sh.Errorf/sh.Printf with a nil
// return so dependents keep their facts while the overall lint still fails.
func (b *Builder) NogoAction(s *modload.Loader, p *load.Package) *Action {
	if p == nil || !LintsPackage(p) {
		return nil
	}
	return b.cacheAction("logos nogo", p, func() *Action {
		a := &Action{
			Mode:    "logos nogo",
			Package: p,
			Actor:   nogoActor{},
			Objdir:  b.NewObjdir(),
		}
		a.Deps = append(a.Deps, b.nogoBootstrapAction())
		if p.Error == nil || !p.Error.IsImportCycle {
			for _, p1 := range p.Internal.Imports {
				a.Deps = append(a.Deps, b.CompileAction(ModeBuild, ModeBuild, p1))
			}
			for _, p1 := range p.Internal.Imports {
				if p1 == p {
					continue
				}
				if na := b.NogoAction(s, p1); na != nil {
					a.Deps = append(a.Deps, na)
				}
			}
		}
		if nogoNeedsArchive(p) {
			a.Deps = append(a.Deps, b.CompileAction(ModeBuild, ModeBuild, p))
		}
		// Real compile dependency for the helper hard import
		// (check.internalPackages). Leaf packages with zero imports
		// have no transitive stdlib in own Deps, so resolve
		// internal/abi through the existing loader and add its
		// compile output to compileBuilt for -import.
		{
			var stk load.ImportStack
			abi := load.LoadPackageWithFlags(s, "internal/abi", base.Cwd(), &stk, nil, 0)
			if abi.Error != nil {
				base.Fatalf("load internal/abi: %v", abi.Error)
			}
			a.Deps = append(a.Deps, b.CompileAction(ModeBuild, ModeBuild, abi))
		}
		if p.IsLogos() {
			a.Deps = append(a.Deps, b.nogoEjectAction(p))
		}
		return a
	})
}

// nogoNeedsArchive reports whether the helper needs p's own compiled
// archive: only checkescape (the sole binaryAnalyzer) reads -archive, and
// only packages carrying a +checkescape annotation can produce its
// findings. For unannotated loGo/s packages the archive compile's native
// diagnostics would duplicate the sibling LintAction's reports
// line-for-line (the archive dep lints at defaults while LintAction lints
// at policy), so the dep is skipped; the helper then sees no -archive flag
// and skips binary analysis silently (check.Package), which is exact here
// because an unannotated package has no checkescape findings to lose. Go
// packages always keep the dep: ordinary Go compiles run no lints, so it
// is silent there, and unannotated Go packages stay warning-free exactly
// as before. An unreadable source conservatively keeps the dep.
func nogoNeedsArchive(p *load.Package) bool {
	if !p.IsLogos() {
		return true
	}
	for _, f := range p.GoFiles {
		data, err := os.ReadFile(filepath.Join(p.Dir, f))
		if err != nil {
			return true
		}
		if strings.Contains(string(data), "+checkescape") {
			return true
		}
	}
	return false
}

type nogoActor struct{}

// nogoFinding mirrors lib/logos-nogo/internal/check.Finding JSON.
type nogoFinding struct {
	Category string         `json:"Category"`
	Position nogoGoposition `json:"Position"`
	// Physical is the same token without //line directives, so file and
	// byte keying stay exact when the source carries line directives.
	// It is zero for package-level diagnostics, which carry a filename
	// in Position alone.
	Physical nogoGoposition `json:"Physical"`
	Message  string         `json:"Message"`
	GOARCH   string         `json:"GOARCH"`
	GOOS     string         `json:"GOOS"`
}

type nogoGoposition struct {
	Filename string `json:"Filename"`
	Offset   int    `json:"Offset"`
	Line     int    `json:"Line"`
	Column   int    `json:"Column"`
}

func (nogoActor) Act(b *Builder, ctx context.Context, a *Action) error {
	p := a.Package
	sh := b.Shell(a)
	if err := sh.Mkdir(a.Objdir); err != nil {
		return err
	}
	var helperBin string
	var ejectDir string
	var compileBuilt = map[string]string{}
	var factsBuilt = map[string]string{}
	var archiveBuilt string
	for _, d := range a.Deps {
		switch d.Mode {
		case "logos nogo bootstrap":
			helperBin = d.built
		case "logos nogo eject":
			if d.Package == p {
				ejectDir = d.built
			}
		case "logos nogo":
			if d.Package != nil && d.built != "" {
				factsBuilt[d.Package.ImportPath] = d.built
			}
		case "build", "build-install":
			if d.Package != nil && d.built != "" {
				if _, ok := compileBuilt[d.Package.ImportPath]; !ok {
					compileBuilt[d.Package.ImportPath] = d.built
				}
				if d.Package == p {
					archiveBuilt = d.built
				}
			}
		}
		// Transitive export data: walk compile deps for internal/abi and
		// other stdlib archives the driver hard-imports.
		if d.Package != nil {
			collectNogoTransitiveArchives(d, compileBuilt)
		}
	}
	if helperBin == "" && !cfg.BuildN {
		return fmt.Errorf("logos nogo: helper binary missing (bootstrap failed)")
	}
	isGo := !p.IsLogos()
	var srcs []string
	var ejectMaps = map[string]nogoEditMap{}
	var ejectSrc = map[string][]byte{}
	var ejectOut = map[string][]byte{}
	if isGo {
		for _, f := range p.GoFiles {
			srcs = append(srcs, fsys.Actual(filepath.Join(p.Dir, f)))
		}
	} else {
		if ejectDir == "" && !cfg.BuildN {
			return fmt.Errorf("logos nogo: eject dir missing for %s", p.ImportPath)
		}
		if cfg.BuildN {
			for _, f := range p.GoFiles {
				base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) + ".go"
				srcs = append(srcs, filepath.Join(ejectDir, base))
			}
			slices.Sort(srcs)
		}
		if !cfg.BuildN {
			entries, err := os.ReadDir(ejectDir)
			if err != nil {
				return fmt.Errorf("logos nogo: reading eject dir: %w", err)
			}
			for _, e := range entries {
				name := e.Name()
				if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".edits") {
					continue
				}
				full := filepath.Join(ejectDir, name)
				srcs = append(srcs, full)
				mapPath := full + ".edits"
				raw, err := os.ReadFile(mapPath)
				if err != nil {
					return fmt.Errorf("logos nogo: reading edit map %s: %w", mapPath, err)
				}
				var m nogoEditMap
				if err := json.Unmarshal(raw, &m); err != nil {
					return fmt.Errorf("logos nogo: malformed edit map %s: %w", mapPath, err)
				}
				outBytes, err := os.ReadFile(full)
				if err != nil {
					return err
				}
				origPath := filepath.Join(p.Dir, m.Source)
				origBytes, err := os.ReadFile(origPath)
				if err != nil {
					return fmt.Errorf("logos nogo: reading original %s: %w", origPath, err)
				}
				if err := nogoCheckEditMap(origBytes, outBytes, m); err != nil {
					return fmt.Errorf("logos nogo: bad edit map %s: %w", mapPath, err)
				}
				ejectMaps[full] = m
				ejectSrc[full] = origBytes
				ejectOut[full] = outBytes
			}
			if len(srcs) == 0 {
				return fmt.Errorf("logos nogo: no ejected sources for %s", p.ImportPath)
			}
			slices.Sort(srcs)
		}
	}
	// Ensure internal/abi export is present for the driver hard import.
	if _, ok := compileBuilt["internal/abi"]; !ok && !cfg.BuildN {
		if abi := nogoFindTransitiveArchive(a, "internal/abi", compileBuilt); abi != "" {
			compileBuilt["internal/abi"] = abi
		} else {
			return fmt.Errorf("logos nogo: missing internal/abi export data (driver hard import)")
		}
	}
	findingsFile := a.Objdir + "nogo.findings.json"
	factsFile := a.Objdir + "nogo.facts"
	a.built = factsFile
	args := []string{helperBin, "-package", p.ImportPath, "-findings", findingsFile, "-facts-output", factsFile, "-json"}
	args = append(args, "-go", nogoGoBinary())
	args = append(args, "-GOOS", cfg.Goos, "-GOARCH", cfg.Goarch)
	args = append(args, "-GOVERSION", runtime.Version())
	if tags := strings.Join(cfg.BuildContext.BuildTags, ","); tags != "" {
		args = append(args, "-tags", tags)
	}
	if cfg.BuildRace {
		args = append(args, "-race")
	}
	if cfg.BuildMSan {
		args = append(args, "-msan")
	}
	for _, imp := range slices.Sorted(maps.Keys(compileBuilt)) {
		args = append(args, "-import", imp+"="+compileBuilt[imp])
	}
	if archiveBuilt != "" {
		args = append(args, "-archive", p.ImportPath+"="+archiveBuilt)
	}
	for _, imp := range slices.Sorted(maps.Keys(factsBuilt)) {
		args = append(args, "-facts", imp+"="+factsBuilt[imp])
	}
	args = append(args, srcs...)
	if cfg.BuildN {
		sh.ShowCmd(p.Dir, "%s", joinUnambiguously(args))
		return nil
	}
	out, err := sh.runOut(p.Dir, nil, args)
	if err != nil {
		return sh.reportCmd("logos nogo "+p.ImportPath, p.Dir, out, err)
	}
	raw, err := os.ReadFile(findingsFile)
	if err != nil {
		return fmt.Errorf("logos nogo: reading findings: %w", err)
	}
	var findings []nogoFinding
	if len(bytes.TrimSpace(raw)) != 0 {
		if err := json.Unmarshal(raw, &findings); err != nil {
			return fmt.Errorf("logos nogo: malformed findings JSON: %w", err)
		}
	}
	sev, err := nogoSeverityFor(p)
	if err != nil {
		return err
	}
	if sev == "allow" {
		for _, f := range findings {
			if nogoIsErrorFinding(f.Message) {
				return fmt.Errorf("logos nogo: %s: %s", f.Category, f.Message)
			}
		}
		return nil
	}
	if isGo && sev == "forbid" {
		sev = "deny"
	}
	var out2 []nogoNorm
	if isGo {
		parsed := nogoParseGoAllows(p, srcs)
		for _, f := range findings {
			rule := nogoRuleID(f.Category)
			if rule == "" {
				return fmt.Errorf("logos nogo: empty analyzer category")
			}
			line, col, off := f.Position.Line, f.Position.Column, f.Position.Offset
			if line <= 0 || col <= 0 {
				return fmt.Errorf("logos nogo: malformed position %+v", f.Position)
			}
			if nogoSuppressed(parsed, f.Position.Filename, line, rule) {
				if sev == "forbid" {
					// lgo forbid would report allow_forbidden_rule; Go
					// forbid is capped to deny so the directive silences.
					// This branch is Go, so silenced.
				}
				continue
			}
			out2 = append(out2, nogoNorm{rule: rule, file: f.Position.Filename, line: line, col: col, off: off, msg: f.Message})
		}
	} else {
		// Segment maps are built once per ejected file: every output
		// byte is either a verbatim copy of an original range or
		// compiler-generated, so the mapping is exact with no
		// refinement.
		segMaps := make(map[string]*nogoSegMap, len(ejectMaps))
		origPaths := make(map[string]string, len(ejectMaps))
		origData := make(map[string][]byte, len(ejectMaps))
		allowsByOrig := make(map[string][]nogoAllow, len(ejectMaps))
		for full, m := range ejectMaps {
			pm, err := nogoNewSegMap(ejectSrc[full], ejectOut[full], m)
			if err != nil {
				return err
			}
			segMaps[full] = pm
			origPath := filepath.Join(p.Dir, m.Source)
			origPaths[full] = origPath
			origData[origPath] = ejectSrc[full]
			if _, ok := allowsByOrig[origPath]; !ok {
				allowsByOrig[origPath] = nogoOrigAllows(origPath, ejectSrc[full])
			}
		}
		for _, f := range findings {
			rule := nogoRuleID(f.Category)
			if rule == "" {
				return fmt.Errorf("logos nogo: empty analyzer category")
			}
			// Key and map through the physical position: a user //line
			// directive changes the adjusted filename, never the
			// physical file or byte. A finding without a physical file
			// and without position metadata is a package-level
			// diagnostic and is kept exactly as the helper reported it.
			ejFile, ejOff := f.Physical.Filename, f.Physical.Offset
			if ejFile == "" {
				if f.Position.Line <= 0 || f.Position.Column <= 0 {
					out2 = append(out2, nogoNorm{rule: rule, file: f.Position.Filename, line: f.Position.Line, col: f.Position.Column, off: f.Position.Offset, msg: f.Message})
					continue
				}
				ejFile, ejOff = f.Position.Filename, f.Position.Offset
			}
			if _, ok := ejectMaps[ejFile]; !ok {
				return fmt.Errorf("logos nogo: finding outside eject dir: %s", ejFile)
			}
			if ejOff < 0 || ejOff >= len(ejectOut[ejFile]) {
				return fmt.Errorf("logos nogo: finding offset %d outside %s", ejOff, ejFile)
			}
			origOff, generated := segMaps[ejFile].origOffset(ejOff)
			if generated {
				// The finding sits in bytes the compiler generated:
				// there is no user token to attribute it to.
				continue
			}
			origPath := origPaths[ejFile]
			origLine, origCol := nogoOffsetToLineCol(origData[origPath], origOff)
			if sev != "forbid" && nogoSuppressed(allowsByOrig[origPath], origPath, origLine, rule) {
				continue
			}
			out2 = append(out2, nogoNorm{rule: rule, file: origPath, line: origLine, col: origCol, off: origOff, msg: f.Message})
		}
		if sev == "forbid" {
			for _, origPath := range slices.Sorted(maps.Keys(allowsByOrig)) {
				for _, al := range allowsByOrig[origPath] {
					n, ok, err := nogoForbidDirective(origPath, origData[origPath], al)
					if err != nil {
						return err
					}
					if !ok {
						continue
					}
					out2 = append(out2, n)
				}
			}
		}
	}
	if len(out2) == 0 {
		return nil
	}
	if cfg.LogosJSON {
		var buf bytes.Buffer
		for _, n := range out2 {
			rec := nogoJSONRecord(n, sev)
			data, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			buf.Write(data)
			buf.WriteByte('\n')
		}
		if sev == "warn" {
			_, _ = io.WriteString(os.Stderr, buf.String())
			return nil
		}
		_, _ = io.WriteString(os.Stderr, buf.String())
		base.SetExitStatus(1)
		return nil
	}
	var buf bytes.Buffer
	for _, n := range out2 {
		if sev == "warn" {
			fmt.Fprintf(&buf, "%s:%d:%d: warning: [%s] %s\n", n.file, n.line, n.col, n.rule, n.msg)
		} else {
			fmt.Fprintf(&buf, "%s:%d:%d: [%s] %s\n", n.file, n.line, n.col, n.rule, n.msg)
		}
	}
	if sev == "warn" {
		_, _ = io.WriteString(os.Stderr, buf.String())
		return nil
	}
	// Deny/forbid: report via sh.Errorf (sets exit 1) but return nil so
	// dependents keep their facts; the lint still fails overall.
	sh.Errorf("%s", strings.TrimSuffix(buf.String(), "\n"))
	return nil
}

func collectNogoTransitiveArchives(a *Action, into map[string]string) {
	seen := map[*Action]bool{}
	var walk func(x *Action)
	walk = func(x *Action) {
		if seen[x] {
			return
		}
		seen[x] = true
		if x.Package != nil && x.built != "" && x.Mode != "logos nogo" && x.Mode != "logos nogo eject" && x.Mode != "logos nogo bootstrap" {
			if _, ok := into[x.Package.ImportPath]; !ok {
				into[x.Package.ImportPath] = x.built
			}
		}
		for _, d := range x.Deps {
			walk(d)
		}
	}
	for _, d := range a.Deps {
		walk(d)
	}
}

func nogoFindTransitiveArchive(a *Action, imp string, have map[string]string) string {
	if f, ok := have[imp]; ok {
		return f
	}
	return ""
}

// nogoRuleID namespaces a helper analyzer category: nogo::<lowercase>.
func nogoRuleID(category string) string {
	c := strings.ToLower(category)
	if c == "" {
		return ""
	}
	return "nogo::" + c
}

func nogoIsErrorFinding(msg string) bool {
	return strings.HasPrefix(msg, "panic recovered:") || strings.HasPrefix(msg, "panic:")
}

// nogoSeverityFor resolves the [lints] nogo group for p via the same
// logos.toml parser the load phase uses. Absent file or absent key defaults
// to deny. Unknown levels are hard errors.
func nogoSeverityFor(p *load.Package) (string, error) {
	if p.Module == nil || p.Module.Dir == "" {
		return "deny", nil
	}
	cf, err := config.Load(p.Module.Dir)
	if err != nil {
		return "deny", nil
	}
	s, ok := cf.Lints.Groups["nogo"]
	if !ok || s == "" {
		return "deny", nil
	}
	switch s {
	case "forbid", "deny", "warn", "allow":
		return s, nil
	}
	return "", fmt.Errorf("logos nogo: invalid [lints] nogo severity %q", s)
}

var nogoAllowRe = regexp.MustCompile(`^//logos:allow\(([a-z][a-z0-9_]*::[a-z][a-z0-9_]*)\)(?:\s.*)?$`)

type nogoAllow struct {
	rule       string
	file       string
	directLine int
	directCol  int
	scopeEnd   int
}

func nogoParseGoAllows(p *load.Package, files []string) []nogoAllow {
	var out []nogoAllow
	for _, f := range files {
		out = append(out, nogoGoAllows(f)...)
	}
	return out
}

func nogoGoAllows(path string) []nogoAllow {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(src), "\n")
	// Map each code line to the longest node end starting there.
	starts := map[int]int{}
	var visit func(n ast.Node)
	_ = visit
	ast.Inspect(f, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncDecl, *ast.GenDecl, *ast.ExprStmt, *ast.AssignStmt, *ast.IfStmt,
			*ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt,
			*ast.SelectStmt, *ast.ReturnStmt, *ast.DeclStmt, *ast.LabeledStmt,
			*ast.BranchStmt, *ast.SendStmt, *ast.IncDecStmt, *ast.GoStmt, *ast.DeferStmt,
			*ast.EmptyStmt, *ast.BlockStmt:
			s := fset.Position(n.Pos())
			e := fset.Position(n.End())
			if cur, ok := starts[s.Line]; !ok || e.Line > cur {
				starts[s.Line] = e.Line
			}
		}
		return true
	})
	var out []nogoAllow
	for _, g := range f.Comments {
		for _, c := range g.List {
			sub := nogoAllowRe.FindStringSubmatch(c.Text)
			if sub == nil {
				continue
			}
			m := sub[1]
			pos := fset.Position(c.Slash)
			if !nogoOwnLine(lines, pos.Line, pos.Column) {
				continue
			}
			next := nogoNextCodeLine(lines, pos.Line)
			if next == 0 {
				continue
			}
			end, ok := starts[next]
			if !ok {
				continue
			}
			out = append(out, nogoAllow{rule: m, file: path, directLine: pos.Line, directCol: pos.Column, scopeEnd: end})
		}
	}
	return out
}

// nogoCodeTok is one non-comment token of an original-source scan.
type nogoCodeTok struct {
	off  int
	line int
	kind token.Token
}

// nogoOrigAllows collects //logos:allow directives from the ORIGINAL
// loGo/s source, in original coordinates, so a directive inside a span
// the eject replaced still suppresses at the position the user wrote
// it. The loGo/s extensions are not parseable by go/parser, so the scan
// tolerates them: go/scanner reports the odd tokens as errors and keeps
// going, and only comments and bracket depth matter here. The scope of
// a directive is the construct on its next code line (nogoScopeEnd).
func nogoOrigAllows(path string, src []byte) []nogoAllow {
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, func(token.Position, string) {}, scanner.ScanComments)
	starts := nogoLineStarts(src)
	lineOf := func(off int) int {
		pos, found := slices.BinarySearch(starts, off)
		if found {
			return pos + 1
		}
		return pos
	}
	lines := strings.Split(string(src), "\n")
	type directive struct {
		rule      string
		line, col int
	}
	var toks []nogoCodeTok
	var dirs []directive
	for {
		pos, kind, lit := s.Scan()
		if kind == token.EOF {
			break
		}
		off := file.Offset(pos)
		if kind == token.COMMENT {
			sub := nogoAllowRe.FindStringSubmatch(lit)
			if sub == nil {
				continue
			}
			line := lineOf(off)
			col := off - starts[line-1] + 1
			if !nogoOwnLine(lines, line, col) {
				continue
			}
			dirs = append(dirs, directive{rule: sub[1], line: line, col: col})
			continue
		}
		if kind == token.SEMICOLON && lit == "\n" {
			continue // automatic semicolon: no byte of its own
		}
		toks = append(toks, nogoCodeTok{off: off, line: lineOf(off), kind: kind})
	}
	var out []nogoAllow
	for _, d := range dirs {
		next := nogoNextCodeLine(lines, d.line)
		if next == 0 {
			continue
		}
		out = append(out, nogoAllow{rule: d.rule, file: path, directLine: d.line, directCol: d.col, scopeEnd: nogoScopeEnd(toks, starts, next)})
	}
	return out
}

// nogoScopeEnd returns the last line of the construct that starts on
// nextLine: it walks the code tokens from that line's first token and
// stops after a token at bracket depth zero whose successor sits on a
// later line, which matches the parser-based node-end scopes for
// ordinary declarations and statements. The loGo/s extensions nest
// with Go brackets and string contents are single tokens, so the depth
// tracks the construct; an illegal token is a plain token here.
func nogoScopeEnd(toks []nogoCodeTok, starts []int, nextLine int) int {
	from, _ := slices.BinarySearchFunc(toks, starts[nextLine-1], func(t nogoCodeTok, off int) int {
		return cmp.Compare(t.off, off)
	})
	depth := 0
	end := nextLine
	for i := from; i < len(toks); i++ {
		t := toks[i]
		switch t.kind {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth--
		}
		end = t.line
		if depth <= 0 && (i+1 >= len(toks) || toks[i+1].line > t.line) {
			break
		}
	}
	return end
}

// nogoForbidDirective reports a //logos:allow that names a forbid rule.
// The directive already carries original coordinates, so the report
// needs no mapping.
func nogoForbidDirective(origPath string, origBytes []byte, al nogoAllow) (nogoNorm, bool, error) {
	if !strings.HasPrefix(al.rule, "nogo::") {
		return nogoNorm{}, false, nil
	}
	off := nogoEjectedOffset(origBytes, al.directLine, al.directCol)
	if off < 0 {
		return nogoNorm{}, false, fmt.Errorf("logos nogo: bad directive position %s:%d:%d", origPath, al.directLine, al.directCol)
	}
	return nogoNorm{rule: "correctness::allow_forbidden_rule", file: origPath, line: al.directLine, col: al.directCol, off: off, msg: fmt.Sprintf("//logos:allow(%s) forbids a forbid rule", al.rule)}, true, nil
}

func nogoSuppressed(allows []nogoAllow, file string, line int, rule string) bool {
	for _, al := range allows {
		if al.file != file || al.rule != rule {
			continue
		}
		if line > al.directLine && line <= al.scopeEnd {
			return true
		}
	}
	return false
}

func nogoOwnLine(lines []string, line, col int) bool {
	if line < 1 || line > len(lines) {
		return false
	}
	for i := 0; i < col-1 && i < len(lines[line-1]); i++ {
		if lines[line-1][i] != ' ' && lines[line-1][i] != '\t' {
			return false
		}
	}
	return true
}

func nogoNextCodeLine(lines []string, line int) int {
	for i := line; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			return 0
		}
		if strings.HasPrefix(t, "//") {
			continue
		}
		return i + 1
	}
	return 0
}

// nogoJSONRecord renders one schema-1.0 diagnostic line.
type nogoJSONSpan struct {
	StartLine  int `json:"start_line"`
	StartCol   int `json:"start_col"`
	EndLine    int `json:"end_line"`
	EndCol     int `json:"end_col"`
	ByteOffset int `json:"byte_offset"`
	ByteLen    int `json:"byte_len"`
}

type nogoJSONProvenance struct {
	OriginExpr   string `json:"origin_expr"`
	OriginLine   int    `json:"origin_line"`
	InferredType string `json:"inferred_type"`
}

type nogoJSONFix struct {
	Description      string `json:"description"`
	Replacement      string `json:"replacement"`
	InsertBeforeLine int    `json:"insert_before_line,omitempty"`
}

type nogoJSONRecordT struct {
	SchemaVersion     string             `json:"schema_version"`
	Severity          string             `json:"severity"`
	RuleID            string             `json:"rule_id"`
	File              string             `json:"file"`
	Span              nogoJSONSpan       `json:"span"`
	Message           string             `json:"message"`
	Provenance        nogoJSONProvenance `json:"provenance"`
	AutofixCandidates []nogoJSONFix      `json:"autofix_candidates"`
}

type nogoNorm struct {
	rule, file string
	line, col  int
	off        int
	msg        string
}

func nogoJSONRecord(n nogoNorm, sev string) nogoJSONRecordT {
	off2, ok := nogoByteOffset(n.file, n.line, n.col)
	if !ok {
		off2 = n.off
	}
	return nogoJSONRecordT{
		SchemaVersion:     "1.0",
		Severity:          sev,
		RuleID:            n.rule,
		File:              n.file,
		Span:              nogoJSONSpan{StartLine: n.line, StartCol: n.col, EndLine: n.line, EndCol: n.col, ByteOffset: off2, ByteLen: 0},
		Message:           n.msg,
		Provenance:        nogoJSONProvenance{},
		AutofixCandidates: []nogoJSONFix{},
	}
}

func nogoEjectedOffset(data []byte, line, col int) int {
	starts := nogoLineStarts(data)
	if line < 1 || line > len(starts) || col < 1 {
		return -1
	}
	end := len(data)
	if line < len(starts) {
		end = starts[line] - 1
	}
	off := starts[line-1] + col - 1
	if off > end {
		return -1
	}
	return off
}

func nogoByteOffset(name string, line, col int) (int, bool) {
	data, err := os.ReadFile(name)
	if err != nil {
		return 0, false
	}
	starts := []int{0}
	for i, c := range data {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	if line < 1 || line > len(starts) || col < 1 {
		return 0, false
	}
	end := len(data)
	if line < len(starts) {
		end = starts[line] - 1
	}
	off := starts[line-1] + col - 1
	if off > end {
		return 0, false
	}
	return off, true
}

// --- Exact position mapping (compiler-written origin segments) ---

type nogoEditSpan struct {
	Start int `json:"start"`
	End   int `json:"end"`
	Len   int `json:"len"`
}

// nogoSegment is one run of the ejected text: the Len bytes at Out are
// either a verbatim copy of the Len bytes of the original at Orig or,
// with Orig negative, compiler-generated.
type nogoSegment struct {
	Out  int `json:"out"`
	Len  int `json:"len"`
	Orig int `json:"orig"`
}

type nogoEditMap struct {
	Version int            `json:"version"`
	Source  string         `json:"source"`
	Size    int            `json:"size"`
	Out     int            `json:"out"`
	Edits   []nogoEditSpan `json:"edits"`
	Segs    []nogoSegment  `json:"segments"`
}

func nogoOffsetToLineCol(data []byte, off int) (int, int) {
	if off < 0 {
		off = 0
	}
	if off > len(data) {
		off = len(data)
	}
	starts := nogoLineStarts(data)
	line := 1
	for i, s := range starts {
		if s > off {
			break
		}
		line = i + 1
	}
	col := off - starts[line-1] + 1
	return line, col
}

func nogoLineStarts(data []byte) []int {
	starts := []int{0}
	for i, c := range data {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func nogoCheckEditMap(src, out []byte, m nogoEditMap) error {
	if m.Version != 2 {
		return fmt.Errorf("edit map for %s has unsupported version %d", m.Source, m.Version)
	}
	if m.Size != len(src) || m.Out != len(out) {
		return fmt.Errorf("edit map for %s does not match its texts", m.Source)
	}
	prev, delta := 0, 0
	for _, s := range m.Edits {
		if !(s.Start >= prev && s.End >= s.Start && s.End <= len(src) && s.Len >= 0) {
			return fmt.Errorf("edit map for %s holds the invalid span %d:%d", m.Source, s.Start, s.End)
		}
		prev = s.End
		delta += s.Len - (s.End - s.Start)
	}
	if len(src)+delta != len(out) {
		return fmt.Errorf("edit map for %s does not add up to the lowered size", m.Source)
	}
	return nil
}

// nogoSegMap maps offsets of the ejected text to the original through
// the compiler-written origin segments: a copy segment maps exactly, a
// generated run has no original.
type nogoSegMap struct {
	orig []byte
	segs []nogoSegment
}

// nogoNewSegMap validates the segments of m against both texts and
// returns the map. The segments must partition the whole ejected text
// in order, and every copy segment must be a byte-exact copy of its
// original range; a map that fails any check is a hard error, never a
// silently wrong mapping.
func nogoNewSegMap(src, out []byte, m nogoEditMap) (*nogoSegMap, error) {
	if err := nogoCheckEditMap(src, out, m); err != nil {
		return nil, err
	}
	end := 0
	for _, sg := range m.Segs {
		if sg.Len <= 0 || sg.Out != end {
			return nil, fmt.Errorf("edit map for %s holds the invalid segment %d:%d:%d", m.Source, sg.Out, sg.Len, sg.Orig)
		}
		if sg.Orig >= 0 {
			if sg.Orig+sg.Len > len(src) || string(out[sg.Out:sg.Out+sg.Len]) != string(src[sg.Orig:sg.Orig+sg.Len]) {
				return nil, fmt.Errorf("edit map for %s holds a segment that is not a verbatim copy", m.Source)
			}
		}
		end = sg.Out + sg.Len
	}
	if end != len(out) {
		return nil, fmt.Errorf("edit map for %s does not cover the lowered text", m.Source)
	}
	return &nogoSegMap{orig: src, segs: m.Segs}, nil
}

// origOffset maps an offset of the ejected text to its original offset.
// generated reports that the offset sits in compiler-generated bytes,
// which have no user position to attribute a finding to. The offset
// must lie in the ejected text; the caller checks that.
func (m *nogoSegMap) origOffset(off int) (orig int, generated bool) {
	lo, hi := 0, len(m.segs)
	for lo < hi {
		mid := (lo + hi) / 2
		if m.segs[mid].Out+m.segs[mid].Len <= off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(m.segs) {
		return 0, true
	}
	sg := m.segs[lo]
	if sg.Orig < 0 {
		return 0, true
	}
	return sg.Orig + (off - sg.Out), false
}
