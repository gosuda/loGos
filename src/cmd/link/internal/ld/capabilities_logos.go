// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"cmd/internal/goobj"
	"cmd/internal/objabi"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
)

// flagLogosCap names the JSON capabilities policy file cmd/go writes for
// logos builds. Empty (plain go, or go tool link by hand) disables the
// audit, so plain go keeps its upstream link behavior.
var flagLogosCap = flag.String("logos.cap", "", "read logos capabilities policy from `file` (logos builds only)")

// CapabilityKind classifies a protected sink.
type CapabilityKind uint8

// Sink capability kinds.
const (
	CapReflect CapabilityKind = iota + 1
	CapExec
	CapNet
	CapCgo
)

// capabilitySinks maps sink symbol names to their capability kind. The
// PLAN name runtime.logosFastCgoCall names no symbol in this tree; the
// tailored sink is runtime.logosCgocallFast, the M20 fast path.
// Process creation sinks syscall.StartProcess (src/syscall/exec_unix.go:272)
// beside os.StartProcess/os.startProcess; net sinks syscall.Connect
// (src/syscall/syscall_unix.go:295) beside syscall.Socket.
var capabilitySinks = map[string]CapabilityKind{
	"reflect.ValueOf":          CapReflect,
	"reflect.TypeOf":           CapReflect,
	"reflect.New":              CapReflect,
	"syscall.Exec":             CapExec,
	"syscall.ForkExec":         CapExec,
	"syscall.StartProcess":     CapExec,
	"os.StartProcess":          CapExec,
	"os.startProcess":          CapExec,
	"net.sysSocket":            CapNet,
	"syscall.Socket":           CapNet,
	"syscall.Connect":          CapNet,
	"runtime.cgocall":          CapCgo,
	"runtime.logosCgocallFast": CapCgo,
}

// Logos severity words shared with the cmd/go policy. The linker mirrors
// the policy JSON strings; it cannot import the cmd/go module.
const (
	logosDeny  = "deny"
	logosWarn  = "warn"
	logosAllow = "allow"
)

// logosCapPolicy mirrors the cmd/go LinkPolicy JSON (M12-S2): the two
// structs must carry the same keys. Unsafe sinks need no linker entry;
// M12-S1 covers them over the package graph.
type logosCapPolicy struct {
	TransitiveReflect       string   `json:"transitive_reflect"`
	TransitiveExec          string   `json:"transitive_exec"`
	TransitiveNet           string   `json:"transitive_net"`
	ReflectAllowlist        []string `json:"reflect_allowlist"`
	TransitiveExecAllowlist []string `json:"transitive_exec_allowlist"`
	TransitiveNetAllowlist  []string `json:"transitive_net_allowlist"`
	MainModules             []string `json:"main_modules"`
	CgoAllowed              []string `json:"cgo_allowed"`
}

// logosCapImpl is one callable method implementation: its name, its
// function type symbol, and the text symbol holding its code.
type logosCapImpl struct {
	name string
	typ  loader.Sym
	fn   loader.Sym
}

// logosCapHit is one reached sink with its kind and display name.
type logosCapHit struct {
	sink loader.Sym
	kind CapabilityKind
	name string
}

// capIsCgoTrampoline reports whether name is a cgo C-function wrapper:
// package-qualified _Cfunc_ symbols the cgo tool generates per C call
// site. External cgo calls leave the Go loader graph at the host object
// boundary (the wrapper points at an external C trampoline with no
// outgoing loader edges), so the wrapper, not runtime.cgocall, is the
// observable cgo-use point. _Ctype_ and _Cvar_ symbols carry no calls.
func capIsCgoTrampoline(name string) bool {
	_, after, ok := strings.Cut(name, "._Cfunc_")
	return ok && after != ""
}

// logosCapAuditor is a BFS over the live symbol graph from the link roots,
// recording the parent and every reverse edge of each visit for chain
// backtraces. It runs after the deadcode fixpoint, so AttrReachable already
// excludes dead code: a dependency that imports a sink package without
// calling it contributes no path.
type logosCapAuditor struct {
	ctxt   *Link
	ldr    *loader.Loader
	policy *logosCapPolicy
	std    map[string]bool
	graph  *capGraph
	roles  map[loader.Sym]capRole
	queue  []loader.Sym
	impls  []logosCapImpl
	hits   []logosCapHit
	fanned bool
	deps   []string
}

// logosCapAudit runs the capabilities reachability audit after deadcode.
// The hook line lives in deadcode.go; every other line lives here.
func logosCapAudit(ctxt *Link, d *deadcodePass) {
	if *flagLogosCap == "" {
		return
	}
	data, err := os.ReadFile(*flagLogosCap)
	if err != nil {
		Errorf("logos capabilities: read policy %s: %v", *flagLogosCap, err)
		return
	}
	var policy logosCapPolicy
	if err := json.Unmarshal(data, &policy); err != nil {
		Errorf("logos capabilities: parse policy %s: %v", *flagLogosCap, err)
		return
	}
	a := &logosCapAuditor{
		ctxt:   ctxt,
		ldr:    ctxt.loader,
		policy: &policy,
		std:    ctxt.loader.StdPackages(),
		graph:  newCapGraph(),
		roles:  make(map[loader.Sym]capRole),
	}
	a.deps = parseLogosModinfoDeps(strdata["runtime.modinfo"])
	a.collectImpls(d)
	a.seedRoots(d)
	a.flood(d)
	a.report()
}

// collectImpls indexes the callable implementations of the reachable
// types' methods. It scans the loader instead of reusing
// d.markableMethods: the deadcode fixpoint removes every matched method
// from that list, so only unmatched leftovers remain by audit time.
func (a *logosCapAuditor) collectImpls(d *deadcodePass) {
	for s := loader.Sym(1); int(s) < a.ldr.NDef(); s++ {
		if !a.ldr.AttrReachable(s) || !a.ldr.IsGoType(s) {
			continue
		}
		if len(a.ldr.Data(s)) == 0 || !decodetypeHasUncommon(a.ctxt.Arch, a.ldr.Data(s)) {
			continue
		}
		relocs := a.ldr.Relocs(s)
		var triples []int
		for i := 0; i < relocs.Count(); i++ {
			r := relocs.At(i)
			if r.Type() != objabi.R_METHODOFF {
				continue
			}
			if r.Weak() {
				convert := a.ctxt.linkShared && a.ldr.IsItab(s) ||
					a.ctxt.canUsePlugins && r.Type().IsDirectCall()
				if !convert {
					i += 2
					continue
				}
			}
			triples = append(triples, i)
			i += 2
		}
		sigs := d.decodetypeMethods(a.ldr, a.ctxt.Arch, s, &relocs)
		for j, r := range triples {
			if j >= len(sigs) {
				break
			}
			fn := relocs.At(r + 2).Sym()
			if fn == 0 || !a.ldr.AttrReachable(fn) {
				continue
			}
			a.impls = append(a.impls, logosCapImpl{name: sigs[j].name, typ: sigs[j].typ, fn: fn})
		}
	}
}

// seedRoots enqueues the link roots deadcode starts from. A shared library
// roots every definition it keeps, then the main inittasks. Any other
// build roots the entry symbol (main.main and its inittask under
// -linkshared), the plugin entry points and exports, dynamic exports, the
// main inittasks, and the package init functions deadcode recorded.
func (a *logosCapAuditor) seedRoots(d *deadcodePass) {
	if a.ctxt.BuildMode == BuildModeShared {
		for i, n := 1, a.ldr.NDef(); i < n; i++ {
			s := loader.Sym(i)
			if a.ldr.SymType(s).IsText() && a.ldr.SymSize(s) == 0 {
				continue
			}
			a.enqueue(s, 0)
		}
		a.enqueue(a.ctxt.mainInittasks, 0)
		return
	}
	var names []string
	if a.ctxt.linkShared && (a.ctxt.BuildMode == BuildModeExe || a.ctxt.BuildMode == BuildModePIE) {
		names = append(names, "main.main", "main..inittask")
	} else {
		names = append(names, *flagEntrySymbol)
	}
	if a.ctxt.BuildMode == BuildModePlugin {
		prefix := objabi.PathToPrefix(*flagPluginPath)
		names = append(names, prefix+"..inittask", prefix+".main", "go:plugin.tabs")
		if exports := a.ldr.Lookup("go:plugin.exports", 0); exports != 0 {
			relocs := a.ldr.Relocs(exports)
			for i := range relocs.Count() {
				a.enqueue(relocs.At(i).Sym(), 0)
			}
		}
	}
	for _, name := range names {
		a.enqueue(a.ldr.Lookup(name, 0), 0)
		if abiInternalVer != 0 {
			a.enqueue(a.ldr.Lookup(name, abiInternalVer), 0)
		}
	}
	for _, s := range a.ctxt.dynexp {
		a.enqueue(s, 0)
	}
	for _, s := range a.ldr.WasmExports {
		a.enqueue(s, 0)
	}
	if a.ctxt.mainInittasks != 0 {
		a.enqueue(a.ctxt.mainInittasks, 0)
	}
	for _, s := range d.pkginits {
		a.enqueue(s, 0)
	}
}

// enqueue records the edge p -> s and visits s on first sight, skipping
// dull and dead symbols. Roots carry parent 0.
func (a *logosCapAuditor) enqueue(s, p loader.Sym) {
	if s == 0 || s == p || !a.ldr.AttrReachable(s) {
		return
	}
	if a.graph.visit(s, p) {
		a.queue = append(a.queue, s)
	}
}

// flood walks relocations from the roots, recording parents. Marker
// relocations (R_USETYPE, R_USEIFACE, R_INITORDER) and method-descriptor
// triples (R_METHODOFF) carry no calls and are skipped like in deadcode;
// R_USEIFACEMETHOD and R_USENAMEDMETHOD grow synthetic edges to the
// matching implementations with the call site as parent.
func (a *logosCapAuditor) flood(d *deadcodePass) {
	for len(a.queue) > 0 {
		s := a.queue[0]
		a.queue = a.queue[1:]
		name := a.ldr.SymName(s)
		if kind, ok := capabilitySinks[capSinkName(name)]; ok {
			a.hits = append(a.hits, logosCapHit{sink: s, kind: kind, name: capSinkName(name)})
		} else if capIsCgoTrampoline(name) && a.ldr.SymType(s).IsText() {
			a.hits = append(a.hits, logosCapHit{sink: s, kind: CapCgo, name: "runtime.cgocall"})
		}
		if a.isReflectSite(s) {
			for _, impl := range a.impls {
				if capIsExportedMethod(impl.name) {
					a.enqueue(impl.fn, s)
				}
			}
		}
		a.followRelocs(d, s)
		a.followAux(s)
		a.followInlTree(s)
		if a.ldr.IsExternal(s) {
			a.enqueue(a.ldr.OuterSym(s), s)
			a.enqueue(a.ldr.SubSym(s), s)
		}
	}
}

// reflectLookupSyms names the method-lookup functions whose use makes a
// function a reflection site, mirroring the compiler's REFLECTMETHOD flag
// (which covers only lookups the compiler cannot resolve statically).
var reflectLookupSyms = []string{
	"reflect.Value.Method",
	"reflect.Value.MethodByName",
	"reflect.Type.Method",
	"reflect.Type.MethodByName",
}

// isReflectSite reports whether s is a function that may look methods up
// reflectively. Type descriptors never are: their method tables name
// reflect.Value.Method without calling it, and counting them would make
// every function that mentions reflect.Value reach every exported method.
func (a *logosCapAuditor) isReflectSite(s loader.Sym) bool {
	if !a.ldr.SymType(s).IsText() {
		return false
	}
	if a.ldr.IsReflectMethod(s) || a.callsReflectLookup(s) {
		return true
	}
	fi := a.ldr.FuncInfo(s)
	if fi.Valid() {
		fi.Preload()
		for i := range int(fi.NumInlTree()) {
			inlined := fi.InlTree(i).Func
			if inlined != 0 && (a.ldr.IsReflectMethod(inlined) || a.callsReflectLookup(inlined) || capIsReflectLookupName(a.ldr.SymName(inlined))) {
				return true
			}
		}
	}
	return false
}

// followInlTree scans the inlined call tree of s to record inlined call edges,
// register inlined sinks, and trigger reflective lookups without requiring
// inlined symbols to carry physical function bodies or deadcode reachability.
func (a *logosCapAuditor) followInlTree(s loader.Sym) {
	fi := a.ldr.FuncInfo(s)
	if !fi.Valid() {
		return
	}
	fi.Preload()
	ninl := int(fi.NumInlTree())
	for i := range ninl {
		node := fi.InlTree(i)
		inlined := node.Func
		if inlined == 0 {
			continue
		}
		var p loader.Sym
		if node.Parent >= 0 && int(node.Parent) < ninl {
			p = fi.InlTree(int(node.Parent)).Func
		}
		if p == 0 {
			p = s
		}
		if inlined == p {
			continue
		}
		a.graph.visit(inlined, p)
		name := a.ldr.SymName(inlined)
		if kind, ok := capabilitySinks[capSinkName(name)]; ok {
			a.hits = append(a.hits, logosCapHit{sink: inlined, kind: kind, name: capSinkName(name)})
		} else if capIsCgoTrampoline(name) {
			a.hits = append(a.hits, logosCapHit{sink: inlined, kind: CapCgo, name: "runtime.cgocall"})
		}
		if capIsReflectLookupName(name) || a.ldr.IsReflectMethod(inlined) || a.callsReflectLookup(inlined) {
			for _, impl := range a.impls {
				if capIsExportedMethod(impl.name) {
					a.enqueue(impl.fn, inlined)
				}
			}
		}
	}
}

// callsReflectLookup reports whether s references a reflective method
// lookup, catching constant-argument lookups the compiler resolves
// without flagging REFLECTMETHOD.
func (a *logosCapAuditor) callsReflectLookup(s loader.Sym) bool {
	relocs := a.ldr.Relocs(s)
	for i := range relocs.Count() {
		r := relocs.At(i)
		t := r.Sym()
		if t == 0 || r.Type() == objabi.R_METHODOFF {
			continue
		}
		if capIsReflectLookupName(a.ldr.SymName(t)) {
			return true
		}
	}
	return false
}

// followRelocs enqueues the call and data targets of s.
func (a *logosCapAuditor) followRelocs(d *deadcodePass, s loader.Sym) {
	relocs := a.ldr.Relocs(s)
	for i := 0; i < relocs.Count(); i++ {
		r := relocs.At(i)
		if r.Weak() {
			convert := false
			if a.ctxt.linkShared && a.ldr.IsItab(s) {
				convert = true
			}
			if a.ctxt.canUsePlugins && r.Type().IsDirectCall() {
				convert = true
			}
			if !convert {
				continue
			}
		}
		switch r.Type() {
		case objabi.R_METHODOFF:
			i += 2
			continue
		case objabi.R_USETYPE, objabi.R_INITORDER:
			continue
		case objabi.R_USEIFACE:
			if rs := r.Sym(); a.ldr.IsItab(rs) {
				a.enqueue(rs, s)
			}
			continue
		case objabi.R_USEIFACEMETHOD:
			rs := r.Sym()
			if a.ctxt.linkShared && (a.ldr.SymType(rs) == sym.SDYNIMPORT || a.ldr.SymType(rs) == sym.Sxxx) {
				a.followExportedImpls(s)
				continue
			}
			m := d.decodeIfaceMethod(a.ldr, a.ctxt.Arch, rs, r.Add())
			a.followIfaceMethod(s, m.name, m.typ)
			continue
		case objabi.R_USENAMEDMETHOD:
			a.followIfaceMethodName(s, d.decodeGenericIfaceMethod(a.ldr, r.Sym()))
			continue
		}
		a.enqueue(r.Sym(), s)
	}
}

// followExportedImpls adds synthetic edges from an interface use site
// whose interface type lives in a shared library to every exported method
// implementation. The type cannot be decoded here, as in deadcode, and a
// dynamic link retains every method, so the exported methods are the ones
// the site can name from outside. The first site reached is the shortest
// chain, so later sites add no reachability and record no edges.
func (a *logosCapAuditor) followExportedImpls(from loader.Sym) {
	if a.fanned {
		return
	}
	a.fanned = true
	for _, impl := range a.impls {
		if capIsExportedMethod(impl.name) {
			a.enqueue(impl.fn, from)
		}
	}
}

// followIfaceMethod adds synthetic edges from a precise interface use
// site to the implementations matching the used method name and type.
func (a *logosCapAuditor) followIfaceMethod(from loader.Sym, name string, typ loader.Sym) {
	a.followIfaceImpls(from, name, typ)
}

// followIfaceMethodName adds synthetic edges from a generic interface use
// site to every implementation carrying the used method name.
func (a *logosCapAuditor) followIfaceMethodName(from loader.Sym, name string) {
	a.followIfaceImpls(from, name, 0)
}

// followIfaceImpls adds synthetic edges from an interface use site to the
// matching implementations. The nil type symbol matches any type.
func (a *logosCapAuditor) followIfaceImpls(from loader.Sym, name string, typ loader.Sym) {
	for _, impl := range a.impls {
		if impl.name != name {
			continue
		}
		if typ != 0 && impl.typ != typ {
			continue
		}
		a.enqueue(impl.fn, from)
	}
}

// followAux enqueues the auxiliary symbols of s, except type descriptors.
func (a *logosCapAuditor) followAux(s loader.Sym) {
	for i, n := 0, a.ldr.NAux(s); i < n; i++ {
		if aux := a.ldr.Aux(s, i); aux.Type() != goobj.AuxGotype {
			a.enqueue(aux.Sym(), s)
		}
	}
}

// capFinding is one dependency chain to one reached sink.
type capFinding struct {
	kind  CapabilityKind
	sink  string
	dep   string
	chain []loader.Sym
}

// findings collects every blamed chain per reached sink: one shortest
// chain per dependency, folding the sink symbols that share a display name
// (ABI wrappers, per-call-site cgo trampolines) into the shortest. Order
// follows hit order, then dependency path, so output is stable.
func (a *logosCapAuditor) findings() []capFinding {
	var out []capFinding
	index := make(map[string]int)
	for _, hit := range a.hits {
		for _, b := range a.blamedChains(hit) {
			out = addFinding(out, index, capFinding{kind: hit.kind, sink: hit.name, dep: b.dep, chain: b.chain})
		}
	}
	return out
}

// blamedChains returns the dependency chains for one hit. Cgo is a
// property of a package's own C calls: the standard library's cgo (the net
// resolver, os/user) never blames the dependencies that call it, so a cgo
// sink blames only a dependency that owns it.
func (a *logosCapAuditor) blamedChains(hit logosCapHit) []capBlame {
	if hit.kind == CapCgo && a.role(hit.sink).dep == "" {
		return nil
	}
	return a.graph.blamedChains(hit.sink, a.role)
}

// addFinding appends f, or replaces the kept finding for the same sink
// name and dependency when f's chain is shorter. index maps that pair to
// its position in out.
func addFinding(out []capFinding, index map[string]int, f capFinding) []capFinding {
	key := f.sink + "\x00" + f.dep
	i, seen := index[key]
	switch {
	case !seen:
		index[key] = len(out)
		return append(out, f)
	case len(f.chain) < len(out[i].chain):
		out[i] = f
	}
	return out
}

// report checks every blamed chain against the policy and emits one
// diagnostic per violating chain.
func (a *logosCapAuditor) report() {
	for _, f := range a.findings() {
		a.judge(f)
	}
}

// judge applies the policy row for f's sink kind and emits the diagnostic
// when the dependency is not exempt.
func (a *logosCapAuditor) judge(f capFinding) {
	if f.kind == CapCgo {
		if !slices.Contains(a.policy.CgoAllowed, f.dep) {
			a.emit("ERROR", "unauthorized_cgo", f.dep, f.sink, f.chain,
				`Remove the dependency or allow cgo for its module with [build] cgo = "allow" in logos.toml.`)
		}
		return
	}
	rule := a.ruleFor(f.kind)
	if rule.sev == logosAllow || capAllowlisted(rule.allow, f.dep) {
		return
	}
	word := "ERROR"
	if rule.sev == logosWarn {
		word = "WARNING"
	}
	if word == "WARNING" && isLogosTestBinary() {
		return
	}
	a.emit(word, rule.id, f.dep, f.sink, f.chain,
		fmt.Sprintf("Remove the dependency or explicitly authorize %q under '%s' in logos.toml.", f.dep+"/**", rule.allowKey))
}

// capRule is the policy row for one sink kind.
type capRule struct {
	id       string
	sev      string
	allowKey string
	allow    []string
}

// ruleFor maps a sink kind to its policy row. Unknown severities fail
// closed to deny.
func (a *logosCapAuditor) ruleFor(kind CapabilityKind) capRule {
	switch kind {
	case CapReflect:
		return capRule{"unauthorized_reflect", capSeverityDeny(a.policy.TransitiveReflect), "reflect_allowlist", a.policy.ReflectAllowlist}
	case CapNet:
		return capRule{"unauthorized_net", capSeverityDeny(a.policy.TransitiveNet), "transitive_net_allowlist", a.policy.TransitiveNetAllowlist}
	default:
		return capRule{"unauthorized_exec", capSeverityDeny(a.policy.TransitiveExec), "transitive_exec_allowlist", a.policy.TransitiveExecAllowlist}
	}
}

// emit renders one chain and prints it: deny aborts the link through
// Errorf, warn prints and continues.
func (a *logosCapAuditor) emit(word, ruleID, dep, sink string, chain []loader.Sym, help string) {
	var frames []capFrame
	for _, s := range chain {
		if a.ldr.SymType(s).IsText() {
			frames = append(frames, a.frameOf(s))
		}
	}
	report := renderCapReport(word, ruleID, dep, sink, frames, help)
	if word == "ERROR" {
		Errorf("%s", report)
		return
	}
	fmt.Fprintln(os.Stderr, report)
}

// role returns how the chain search treats symbol s. The result is
// memoized: the search asks about every symbol that can reach a sink.
func (a *logosCapAuditor) role(s loader.Sym) capRole {
	if r, ok := a.roles[s]; ok {
		return r
	}
	r := a.roleOf(s)
	a.roles[s] = r
	return r
}

// roleOf classifies s. Only functions carry blame; data symbols (type
// descriptors, itabs, funcvals) sit between the frames that call and are
// called. Functions of package runtime are walls. A function is blamed on
// the package that owns its code unless that package is first-party or
// standard library, and compiler glue that names no linked package is
// exempt.
func (a *logosCapAuditor) roleOf(s loader.Sym) capRole {
	if !a.ldr.SymType(s).IsText() {
		return capRole{}
	}
	pkg := a.textPkg(s)
	switch {
	case pkg == "runtime":
		return capRole{wall: true}
	case pkg == "" || a.exemptPkg(pkg):
		return capRole{}
	}
	return capRole{dep: pkg}
}

// textPkg returns the import path of the package that owns the code of
// text symbol s, or "" when the name carries no linked package. The
// object's own package wins when the name starts with it and the package
// is one the link actually loaded; otherwise the name decides.
func (a *logosCapAuditor) textPkg(s loader.Sym) string {
	name := a.ldr.SymName(s)
	if pkg := a.ldr.SymPkg(s); pkg != "" && a.ctxt.LibraryByPkg[pkg] != nil && strings.HasPrefix(name, objabi.PathToPrefix(pkg)+".") {
		return pkg
	}
	return a.namedPkg(name)
}

// namedPkg parses the package from a symbol name: the text up to the first
// dot after the last slash, outside any type argument list, with its path
// escapes undone. Only a linked package counts, so glue named like
// "go:(*struct { encoding" or "type:.eq" yields "".
func (a *logosCapAuditor) namedPkg(name string) string {
	base, _, _ := strings.Cut(name, "[")
	elem := base[strings.LastIndexByte(base, '/')+1:]
	dot := strings.IndexByte(elem, '.')
	if dot < 0 {
		return ""
	}
	pkg, err := objabi.PrefixToPath(base[:len(base)-len(elem)+dot])
	if err != nil || a.ctxt.LibraryByPkg[pkg] == nil {
		return ""
	}
	return pkg
}

// exemptPkg reports whether pkg never takes blame: the program's own
// packages, first-party packages owned by a main module, and the standard
// library as the compiler marked it. The module path is no evidence of
// the standard library: a replaced dependency may have a dotless path.
// Ownership follows the module graph's longest-prefix rule (see mainOwns),
// so a nested module (its own go.mod) consumed as a dependency stays
// blamed even though a main module path prefixes it.
func (a *logosCapAuditor) exemptPkg(pkg string) bool {
	if capIsLocalPkg(pkg) || a.std[pkg] {
		return true
	}
	return a.mainOwns(pkg)
}

// mainOwns reports whether pkg belongs to a main module: pkg sits under a
// main module path and no dependency module claims a longer prefix of pkg.
// Dependency paths come from the runtime.modinfo blob cmd/go ships in
// importcfg (stored unquoted in strdata by the modinfo case in ld.go),
// parsed once in logosCapAudit; absent (hand-driven links) they default to
// none and ownership falls back to the pure-prefix rule. A main module
// nested inside a dependency directory still owns its own packages,
// because only a strictly longer dependency prefix takes blame; ties (a
// path listed as both) stay with the main module.
func (a *logosCapAuditor) mainOwns(pkg string) bool {
	owner := ""
	for _, m := range a.policy.MainModules {
		if m != "" && (pkg == m || strings.HasPrefix(pkg, m+"/")) && len(m) > len(owner) {
			owner = m
		}
	}
	if owner == "" {
		return false
	}
	for _, d := range a.deps {
		if d != "" && (pkg == d || strings.HasPrefix(pkg, d+"/")) && len(d) > len(owner) {
			return false
		}
	}
	return true
}

// parseLogosModinfoDeps extracts dependency module paths from the
// runtime.modinfo blob: BuildInfo text (src/runtime/debug/mod.go) wrapped
// in binary markers. "mod\t<main>..." names the main module,
// "dep\t<path>\t<version>..." names each dependency, and "=>..."
// continues a replaced entry. Only dep paths are returned; absent or
// empty blobs yield none.
func parseLogosModinfoDeps(blob string) []string {
	var out []string
	for _, line := range strings.Split(blob, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] != "dep" || f[1] == "" {
			continue
		}
		out = append(out, f[1])
	}
	return out
}

// isLogosTestBinary reports whether the linked binary is a generated test
// binary: cmd/go adds -X testing.testBinary=1 to test links
// (src/cmd/go/internal/load/test.go:307) for the testing.testBinary string
// (src/testing/testing.go:758). Only WARNINGs consult this; ERRORs still
// fail under `logos test` per the scope script cases D, E deny, F deny,
// and H. Unlike a _testmain.go filename check this cannot be triggered by
// a normal `logos build` file and survives unknown/trimpath file info.
func isLogosTestBinary() bool {
	return strdata["testing.testBinary"] == "1"
}

// frameOf renders one symbol as a report frame, falling back to
// unknown:0 when the object carries no function info.
func (a *logosCapAuditor) frameOf(s loader.Sym) capFrame {
	name := a.ldr.SymName(s)
	file, line := "unknown", 0
	if fi := a.ldr.FuncInfo(s); fi.Valid() {
		fi.Preload()
		if f := a.frameFileOf(s, fi); f != "" {
			file = f
		}
		line = int(fi.StartLine())
	}
	return capFrame{name: name, file: file, line: line}
}

// frameFileOf returns the defining file of s: the single used file when
// unambiguous, else the file in effect at the function entry decoded from
// the pcfile table, else "". File Table index 0 is never used: the table
// order follows map iteration in the compiler and carries no meaning.
func (a *logosCapAuditor) frameFileOf(s loader.Sym, fi loader.FuncInfo) string {
	if fi.NumFile() == 1 {
		if cu := a.ldr.SymUnit(s); cu != nil && int(fi.File(0)) < len(cu.FileTable) {
			return expandFile(cu.FileTable[fi.File(0)])
		}
		return ""
	}
	_, pcfile, _, _, _ := a.ldr.PcdataAuxs(s, nil)
	if pcfile == 0 {
		return ""
	}
	idx, ok := capEntryFile(a.ldr.Data(pcfile))
	if !ok {
		return ""
	}
	if cu := a.ldr.SymUnit(s); cu != nil && int(idx) < len(cu.FileTable) {
		return expandFile(cu.FileTable[idx])
	}
	return ""
}

// capEntryFile decodes the file index in effect at a function's entry
// from its pcfile table. The table is a (value, pc) pair sequence where
// the first value applies from the entry, values are varint deltas from
// -1, and pcs are uvarint deltas. The first non-negative value wins;
// anything else means no position info.
func capEntryFile(data []byte) (int32, bool) {
	val := int32(-1)
	rest := data
	for len(rest) > 0 {
		d, n := binary.Varint(rest)
		if n <= 0 {
			return 0, false
		}
		rest = rest[n:]
		val += int32(d)
		if val >= 0 {
			return val, true
		}
		if _, n := binary.Uvarint(rest); n <= 0 {
			return 0, false
		} else {
			rest = rest[n:]
		}
	}
	return 0, false
}

// capFrame is one rendered call-chain frame.
type capFrame struct {
	name string
	file string
	line int
}

// renderCapReport renders one violation in the PLAN Sec 13.2 format.
func renderCapReport(word, ruleID, dep, sink string, frames []capFrame, help string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [capabilities::%s]: Transitive dependency '%s' reaches '%s'\n", word, ruleID, dep, sink)
	b.WriteString("  --> Reachability Call Chain (cmd/link deadcode audit):\n")
	for i, f := range frames {
		marker := ""
		if i > 0 {
			marker = "  -> "
		}
		fmt.Fprintf(&b, "   %d. %s%-40s at %s:%d\n", i+1, marker, f.name+"()", f.file, f.line)
	}
	fmt.Fprintf(&b, " = Help: %s\n", help)
	return b.String()
}

// capSinkName normalizes a linker symbol name for sink matching by
// stripping clone suffixes to the base (SPEC T-203.4, T-205, red-team #9:
// a cloned sink such as syscall.Connect·logosspec1 or a comptime copy
// <f>_logosct1 is the same sink as its base) and ABI wrapper suffixes
// (cgo trampolines and other ABI0 callers reach runtime.cgocall through
// its .abi0 wrapper, not the bare symbol). Stripping loops to a fixpoint
// so chained suffixes (<f>_logosct<M>·logosspec<N>.abi0) collapse fully;
// the link symbol itself keeps the full suffix, so clones stay stable,
// unique, and resolvable in the symbol table while the audit sees one
// sink identity across shared, plugin, and internal links.
func capSinkName(name string) string {
	for {
		next := logosCloneBaseName(name)
		if base, ok := strings.CutSuffix(next, ".abi0"); ok {
			next = base
		} else if base, ok := strings.CutSuffix(next, ".abiinternal"); ok {
			next = base
		}
		if next == name {
			return name
		}
		name = next
	}
}

// capIsReflectLookupName reports whether name, after clone and ABI
// normalization, names a reflective method-lookup function. Cloned
// reflect helpers (reflect.Value.Method·logosspec1) are the same lookup
// point as their base; without normalization a clone would evade the
// reflective fan-out and capability metadata would not survive cloning.
func capIsReflectLookupName(name string) bool {
	return slices.Contains(reflectLookupSyms, capSinkName(name))
}

// capIsLocalPkg reports whether pkg is a package of the program itself that
// no other package imports: main, a command-line package, or a local
// directory outside any module ("_/..."). Those frames never blame.
func capIsLocalPkg(pkg string) bool {
	return pkg == "main" || pkg == "command-line-arguments" || strings.HasPrefix(pkg, "_/")
}

// capIsExportedMethod reports whether a method name is exported.
func capIsExportedMethod(name string) bool {
	for _, r := range name {
		return r >= 'A' && r <= 'Z'
	}
	return false
}

// capAllowlisted reports whether dep matches one of the allowlist globs.
// A trailing "/**" matches the prefix itself and everything under it; any
// other "**" segment spans zero or more segments; remaining segments match
// without crossing slashes.
func capAllowlisted(allowlist []string, dep string) bool {
	for _, pattern := range allowlist {
		if capMatchGlob(pattern, dep) {
			return true
		}
	}
	return false
}

// capMatchGlob matches one allowlist glob against one import path.
func capMatchGlob(pattern, dep string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
			return true
		}
	}
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, dep)
		return err == nil && ok
	}
	return capMatchSegs(strings.Split(pattern, "/"), strings.Split(dep, "/"))
}

// capMatchSegs matches glob segments against path segments, where a "**"
// segment spans zero or more segments and every other segment matches
// exactly one path segment.
func capMatchSegs(pats, segs []string) bool {
	if len(pats) == 0 {
		return len(segs) == 0
	}
	if pats[0] == "**" {
		for i := range len(segs) + 1 {
			if capMatchSegs(pats[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pats[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return capMatchSegs(pats[1:], segs[1:])
}

// capSeverityDeny normalizes a severity string, failing closed to deny.
func capSeverityDeny(sev string) string {
	if sev == logosAllow || sev == logosWarn {
		return sev
	}
	return logosDeny
}
