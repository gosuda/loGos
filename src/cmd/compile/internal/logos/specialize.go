// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"strconv"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// Clone specialization (M13-S3, SPEC T-203.4). A same-package function
// whose interface parameter p serves only as a method receiver, a passed-on
// argument, a result, or an assertion operand gains a clone with p retyped
// to the concrete argument type, so the method calls inside it are static.
// Calls whose argument has that concrete type go to the clone; the original
// function stays for every other caller, function value, and linkname.
//
// The phase is neutral: it runs after lowering and the recheck on plain Go
// syntax with full type information, for Go and loGo/s packages alike, and
// ends with one more recheck. Inputs are the checked package, its Info
// (Defs, Uses, Selections, Instances, Implicits; types come from the syntax
// nodes), and the -d flags. Outputs are new FuncDecls named
// <f>·logosspec<N> (N counts per package from 1), renamed callee names at
// the rewritten call sites, and import declarations for packages that a
// retyped parameter names. Invariants:
//
//   - A call site is rewritten only when every specialized argument has a
//     static, spellable, non-interface type that implements the parameter's
//     declared interface, so the call type-checks and converts exactly as
//     before.
//   - A clone never changes a value an observer can see: a parameter
//     compared with nil, stored, reassigned, addressed, or passed to a
//     generic callee blocks specialization; a value method reached through
//     a pointer (whose nil receiver panics with a different text) also
//     blocks the call.
//   - Bodies with closures, go and defer statements, range-over-func loops,
//     or local type declarations never clone: their symbols would carry the
//     suffix in the middle of the name, and a local type would split in two.
//   - Each clone name is unique in the package, so no symbol collides and no
//     dedup pass exists; clones never carry a pragma.
//
// Ejected output stays plain Go because the phase never runs under
// -logos.eject (the clone names are not Go identifiers).

const (
	// specMaxTypeDepth is the deepest concrete type that specializes (#92).
	specMaxTypeDepth = 4
	// specMaxCallSiteTypes is the number of distinct concrete types one
	// original syntax call may observe across clone environments (#15).
	specMaxCallSiteTypes = 4
	// specO2CostCap is the largest callee body, in syntax nodes, that the
	// capped tiers specialize.
	specO2CostCap = 200
	// specO3InlineBudget is the inline budget cmd/go sets at -O=3
	// (work/logos_opt.go); the compiler reads the tier from it, because the
	// command passes logosspecialize=1 at O3 only.
	specO3InlineBudget = 400
	// specSuffix is the terminal symbol suffix the linker strips from
	// names written to pclntab.
	specSuffix = "·logosspec"
)

// SpecializeEnabled reports whether this compile runs clone
// specialization: -d=logosspecialize is set in a normal code-generating
// compile. Eject output must stay plain Go, helper modes only report, and
// the runtime keeps its upstream code.
func SpecializeEnabled() bool {
	return base.Debug.LogosSpecialize != 0 &&
		base.LogosEject == "" &&
		base.LogosMode == "" &&
		!base.LogosLintOnly &&
		!base.Flag.CompilingRuntime
}

// SpecializeCostCap returns the largest callee body, in syntax nodes, that
// specializes: 200 below the O3 tier and unlimited (zero) at O3.
func SpecializeCostCap() int {
	if base.Debug.InlBudget >= specO3InlineBudget {
		return 0
	}
	return specO2CostCap
}

// Specialize clones eligible functions of the checked package and rewrites
// their calls. costCap is the largest callee body in syntax nodes (zero for
// no limit). It mutates files in place and reports whether anything
// changed; a true result needs a recheck.
func Specialize(pkg *types2.Package, info *types2.Info, files []*syntax.File, costCap int) bool {
	s := &specializer{
		pkg:     pkg,
		info:    info,
		files:   files,
		costCap: costCap,
		funcs:   make(map[*types2.Func]*specFunc),
		refs:    make(map[*syntax.File]map[*types2.Package]string),
	}
	s.collect()
	if len(s.order) == 0 {
		return false
	}
	s.discover()
	return s.apply()
}

// specializer carries the state of one package.
type specializer struct {
	pkg     *types2.Package
	info    *types2.Info
	files   []*syntax.File
	costCap int

	funcs map[*types2.Func]*specFunc
	order []*specFunc // candidates in source order

	// Per-discovery state.
	clones  []*specClone
	byKey   map[string]*specClone
	seen    map[*syntax.CallExpr]map[string]bool
	blocked map[*syntax.CallExpr]bool
	sites   []specSite

	// Import state: refs caches the name a file uses for a package; added
	// holds the declarations apply inserts.
	refs      map[*syntax.File]map[*types2.Package]string
	added     map[*syntax.File][]syntax.Decl
	importSeq int
}

// specFunc is one function that may clone.
type specFunc struct {
	obj    *types2.Func
	decl   *syntax.FuncDecl
	file   *syntax.File
	params []specParam
	cost   int
	bad    bool // the body shape or the parameters block every clone
}

// specParam is one parameter of a specFunc, indexed like the signature.
type specParam struct {
	fn          *specFunc
	field       *syntax.Field
	v           *types2.Var
	iface       bool        // declared as a non-type-parameter interface
	slice       bool        // declared as an unnamed slice of such an interface
	elem        types2.Type // the element interface of a slice parameter
	root        *specParam  // for an element variable, the slice parameter it ranges over
	ok          bool        // every use is allowed
	benefit     bool        // a method call or a foldable assertion exists
	methods     []*types2.Func
	asserts     []specAssert
}

// specAssert is one assertion or type switch on a parameter.
type specAssert struct {
	node   syntax.Expr // *syntax.AssertExpr or *syntax.TypeSwitchGuard
	parent syntax.Node // parent of an AssertExpr, nil for a guard
	fold   bool        // a comma-ok form the clone folds to constants
}

// specClone is one planned clone: the callee and the concrete type of each
// specialized parameter (nil for the others).
type specClone struct {
	fn    *specFunc
	name  string
	types []types2.Type
	idx   int
}

// specSite is one call to rewrite. ctx is nil for a call in the original
// tree; otherwise call lies in the original body of ctx.fn and the rewrite
// lands in ctx's copy of it. unwrap lists the arguments that are
// rt.CovSlice conversions whose operand replaces them, and file is the file
// holding the call.
type specSite struct {
	ctx    *specClone
	file   *syntax.File
	call   *syntax.CallExpr
	target *specClone
	unwrap []int
}

// collect lists the functions that may clone: plain package-level
// functions with a body, no pragma, and an interface parameter.
func (s *specializer) collect() {
	for _, file := range s.files {
		for _, d := range file.DeclList {
			fd, ok := d.(*syntax.FuncDecl)
			if !ok || fd.Recv != nil || fd.TParamList != nil || fd.Body == nil || fd.Pragma != nil {
				continue
			}
			if fd.Name.Value == "_" || fd.Name.Value == "init" {
				continue
			}
			obj, _ := s.info.Defs[fd.Name].(*types2.Func)
			if obj == nil {
				continue
			}
			f := &specFunc{obj: obj, decl: fd, file: file}
			s.scanFunc(f)
			s.markPassOnBenefits(f)
			if f.bad {
				continue
			}
			s.funcs[obj] = f
			s.order = append(s.order, f)
		}
	}
}

// discover plans every clone and call-site rewrite. Planning reads the
// original tree only, so nothing mutates yet.
func (s *specializer) discover() {
	s.clones = nil
	s.byKey = make(map[string]*specClone)
	s.seen = make(map[*syntax.CallExpr]map[string]bool)
	s.blocked = make(map[*syntax.CallExpr]bool)
	s.sites = nil
	for _, file := range s.files {
		s.scan(file, file, nil, nil)
	}
	// Clone bodies are scanned while they grow: a call inside a clone
	// whose argument is a specialized parameter specializes the callee
	// for that parameter's concrete type.
	for i := 0; i < len(s.clones); i++ {
		c := s.clones[i]
		env := make(map[*types2.Var]types2.Type)
		for j, t := range c.types {
			if t != nil {
				env[c.fn.params[j].v] = t
			}
		}
		s.scan(c.fn.decl.Body, c.fn.file, c, env)
	}
	s.trimBlocked()
}

// markPassOnBenefits gives a parameter a specialization benefit when its
// only observable use is passing it to another interface parameter. The
// callee may remain unspecialized (for example after its own site cap), but
// specializing the outer function still preserves the interface conversion.
func (s *specializer) markPassOnBenefits(f *specFunc) {
	tracked := make(map[*types2.Var]*specParam)
	for i := range f.params {
		p := &f.params[i]
		if p.v != nil {
			tracked[p.v] = p
		}
	}
	if len(tracked) == 0 {
		return
	}
	var stack []syntax.Node
	syntax.Inspect(f.decl.Body, func(n syntax.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		name, ok := n.(*syntax.Name)
		if !ok || len(stack) < 2 {
			return true
		}
		v, _ := s.info.Uses[name].(*types2.Var)
		p := tracked[v]
		if p == nil || !p.ok || p.benefit {
			return true
		}
		call, ok := stack[len(stack)-2].(*syntax.CallExpr)
		if ok && s.passOnUse(call, name) {
			p.benefit = true
		}
		return true
	})
}

// trimBlocked removes every site belonging to an original call that
// exceeded the per-original-call type-vector limit. Reachability is then
// computed from the surviving root sites and propagated through surviving
// clone-context sites, so an overflow in one nested call cannot discard
// independent sites or the outer clones that own them.
func (s *specializer) trimBlocked() {
	// Once a syntax call exceeds its cross-context cap, none of its
	// provisional rewrites may survive. Independent root sites remain.
	sites := make([]specSite, 0, len(s.sites))
	for _, st := range s.sites {
		if !s.blocked[st.call] {
			sites = append(sites, st)
		}
	}
	reachable := make(map[*specClone]bool)
	for _, st := range sites {
		if st.ctx == nil {
			reachable[st.target] = true
		}
	}
	for {
		grew := false
		for _, st := range sites {
			if st.ctx != nil && !reachable[st.ctx] {
				continue
			}
			if !reachable[st.target] {
				reachable[st.target] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	keptSites := make([]specSite, 0, len(sites))
	for _, st := range sites {
		if st.ctx == nil || reachable[st.ctx] {
			keptSites = append(keptSites, st)
		}
	}
	s.sites = keptSites
	keptClones := make([]*specClone, 0, len(s.clones))
	for _, c := range s.clones {
		if reachable[c] {
			c.idx = len(keptClones)
			keptClones = append(keptClones, c)
		}
	}
	s.clones = keptClones
}

// scan plans the calls under root, which lies in file. env maps the
// specialized parameters of the clone ctx to their concrete types.
func (s *specializer) scan(root syntax.Node, file *syntax.File, ctx *specClone, env map[*types2.Var]types2.Type) {
	syntax.Inspect(root, func(n syntax.Node) bool {
		call, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		name, ok := call.Fun.(*syntax.Name)
		if !ok {
			return true
		}
		obj, _ := s.info.Uses[name].(*types2.Func)
		f := s.funcs[obj]
		if f == nil {
			return true
		}
		if target := s.plan(f, call, s.callArgs(f, file, call, env)); target != nil {
			site := specSite{ctx: ctx, file: file, call: call, target: target}
			for i, t := range target.types {
				if t != nil && f.params[i].slice {
					site.unwrap = append(site.unwrap, i)
				}
			}
			s.sites = append(s.sites, site)
		}
		return true
	})
}

// callArgs returns the static type of each fixed argument of a call to f
// (nil for arguments that cannot specialize), or nil for a call shape that
// cannot: spread arguments, argument counts that differ from the
// signature, and multi-value forwarding. The entry of a slice parameter is
// the type of the operand of an rt.CovSlice conversion, the only argument
// that can name a concrete element type.
func (s *specializer) callArgs(f *specFunc, file *syntax.File, call *syntax.CallExpr, env map[*types2.Var]types2.Type) []types2.Type {
	np := len(f.params)
	sig := f.obj.Signature()
	fixed := np
	if sig.Variadic() {
		fixed--
	}
	if call.HasDots || len(call.ArgList) < fixed || (!sig.Variadic() && len(call.ArgList) != np) {
		return nil
	}
	args := make([]types2.Type, np)
	for i := range fixed {
		if !f.params[i].slice {
			args[i] = s.argType(call.ArgList[i], env)
		} else if inner, ok := s.covSliceInner(call.ArgList[i]); ok && s.covSliceArgUnaliased(file, call, i, inner) {
			args[i] = inner.GetTypeInfo().Type
		}
	}
	return args
}

// argType returns the static type of an argument. A specialized parameter
// of the enclosing clone has its concrete type.
func (s *specializer) argType(a syntax.Expr, env map[*types2.Var]types2.Type) types2.Type {
	if n, ok := a.(*syntax.Name); ok && env != nil {
		if v, _ := s.info.Uses[n].(*types2.Var); v != nil {
			if t, ok := env[v]; ok {
				return t
			}
		}
	}
	tv := a.GetTypeInfo()
	if tv.IsNil() || tv.IsType() || tv.IsBuiltin() || tv.IsVoid() {
		return nil
	}
	return tv.Type
}

// plan returns the clone for a call to f with the given argument types,
// creating it on first use, or nil when the call stays as it is.
func (s *specializer) plan(f *specFunc, call *syntax.CallExpr, args []types2.Type) *specClone {
	if args == nil {
		return nil
	}
	vec := s.vector(f, args)
	if vec == nil {
		return nil
	}
	key := specKey(vec)
	if s.blocked[call] {
		return nil
	}
	if s.seen[call] == nil {
		s.seen[call] = make(map[string]bool)
	}
	for _, t := range vec {
		if t != nil {
			s.seen[call][types2.TypeString(t, func(p *types2.Package) string { return p.Path() })] = true
		}
	}
	if len(s.seen[call]) > specMaxCallSiteTypes {
		s.blocked[call] = true
		return nil
	}
	if s.costCap > 0 && s.cost(f) > s.costCap {
		return nil
	}
	key = f.obj.Name() + key
	if c := s.byKey[key]; c != nil {
		return c
	}
	c := &specClone{fn: f, types: vec, idx: len(s.clones)}
	c.name = f.obj.Name() + specSuffix + strconv.Itoa(len(s.clones)+1)
	s.byKey[key] = c
	s.clones = append(s.clones, c)
	return c
}

// vector returns the concrete type per parameter that a call may
// specialize, or nil when no parameter qualifies.
func (s *specializer) vector(f *specFunc, args []types2.Type) []types2.Type {
	var vec []types2.Type
	for i := range f.params {
		p := &f.params[i]
		if i >= len(args) || args[i] == nil || !p.ok || !p.benefit {
			continue
		}
		var t types2.Type
		switch {
		case p.iface && s.concreteOK(f, p, args[i]):
			t = args[i]
		case p.slice:
			t = s.sliceOK(f, p, args[i])
		}
		if t == nil {
			continue
		}
		if vec == nil {
			vec = make([]types2.Type, len(f.params))
		}
		vec[i] = t
	}
	return vec
}

// specKey spells a vector as a map key: the fully qualified type of each
// specialized parameter, a dash for the others.
func specKey(vec []types2.Type) string {
	qf := func(p *types2.Package) string { return p.Path() }
	key := ""
	for _, t := range vec {
		if t == nil {
			key += "|-"
			continue
		}
		key += "|" + types2.TypeString(t, qf)
	}
	return key
}

// cost returns the size of f's body in syntax nodes.
func (s *specializer) cost(f *specFunc) int {
	if f.cost == 0 {
		syntax.Inspect(f.decl.Body, func(n syntax.Node) bool {
			if n != nil {
				f.cost++
			}
			return true
		})
	}
	return f.cost
}

// concreteOK reports whether type c may replace parameter p of f: a
// non-interface type that implements the declared interface, nests at most
// specMaxTypeDepth deep, spells in f's file, and keeps the nil-receiver
// behavior of every method the body calls.
func (s *specializer) concreteOK(f *specFunc, p *specParam, c types2.Type) bool {
	if !specConcrete(c) || specTypeDepth(c) > specMaxTypeDepth {
		return false
	}
	iface, ok := p.v.Type().Underlying().(*types2.Interface)
	if !ok || !types2.Implements(c, iface) {
		return false
	}
	if !s.nilSafe(c) && specValueMethodViaPointer(c, p.methods) {
		return false
	}
	return s.canSpell(f.file, c, f.decl.Pos())
}

// sliceOK returns the slice type a clone gives a slice parameter that
// receives a slice of a, or nil when it may not specialize: the element
// must be a concrete type that implements the parameter's element interface
// (checked here, whatever the conversion call claims), nest at most
// specMaxTypeDepth deep, spell in f's file, and keep the nil-receiver
// behavior of every method the body calls.
func (s *specializer) sliceOK(f *specFunc, p *specParam, a types2.Type) types2.Type {
	sl, ok := a.Underlying().(*types2.Slice)
	if !ok {
		return nil
	}
	elem := sl.Elem()
	if !specConcrete(elem) || specTypeDepth(elem) > specMaxTypeDepth {
		return nil
	}
	iface, ok := p.elem.Underlying().(*types2.Interface)
	if !ok || !types2.Implements(elem, iface) || (!s.nilSafe(elem) && specValueMethodViaPointer(elem, p.methods)) {
		return nil
	}
	t := types2.NewSlice(elem)
	if !s.canSpell(f.file, t, f.decl.Pos()) {
		return nil
	}
	return t
}

// apply builds every planned clone, rewrites the sites, and inserts the
// declarations and imports. It reports whether it changed anything.
func (s *specializer) apply() bool {
	if len(s.clones) == 0 {
		return false
	}
	s.added = make(map[*syntax.File][]syntax.Decl)
	copies := make([]*specCopier, len(s.clones))
	after := make(map[*syntax.FuncDecl][]syntax.Decl)
	origOf := make(map[syntax.Node]syntax.Node)
	for i, c := range s.clones {
		cp, decl := s.build(c)
		copies[i] = cp
		after[c.fn.decl] = append(after[c.fn.decl], decl)
		for orig, dup := range cp.memo {
			origOf[dup] = orig
		}
	}
	unwrapped := make(map[*syntax.File]bool)
	for _, st := range s.sites {
		call := st.call
		if st.ctx != nil {
			call = copies[st.ctx.idx].memo[call].(*syntax.CallExpr)
		}
		call.Fun.(*syntax.Name).Value = st.target.name
		for _, i := range st.unwrap {
			call.ArgList[i] = syntax.Unparen(call.ArgList[i]).(*syntax.CallExpr).ArgList[0]
			unwrapped[st.file] = true
		}
	}
	for _, file := range s.files {
		var list []syntax.Decl
		list = append(list, s.added[file]...)
		for _, d := range file.DeclList {
			list = append(list, d)
			if fd, ok := d.(*syntax.FuncDecl); ok {
				list = append(list, after[fd]...)
			}
		}
		file.DeclList = list
	}
	for file := range unwrapped {
		s.blankUnusedRT(file, origOf)
	}
	return true
}

// blankUnusedRT turns the logos/rt import of file into a blank import when
// the conversions apply unwrapped were its last uses, so the recheck does
// not reject an unused import. origOf maps the nodes of the clone bodies to
// the originals the checker resolved.
func (s *specializer) blankUnusedRT(file *syntax.File, origOf map[syntax.Node]syntax.Node) {
	for _, d := range file.DeclList {
		imp, ok := d.(*syntax.ImportDecl)
		if !ok || imp.Path == nil {
			continue
		}
		pn := s.pkgNameOf(imp)
		if pn == nil || pn.Imported().Path() != rtImportPath || pn.Name() == "_" {
			continue
		}
		used := false
		syntax.Inspect(file, func(n syntax.Node) bool {
			if name, ok := n.(*syntax.Name); ok {
				orig, isCopy := origOf[name]
				if !isCopy {
					orig = name
				}
				if on, ok := orig.(*syntax.Name); ok && s.info.Uses[on] == pn {
					used = true
				}
			}
			return !used
		})
		if used {
			continue
		}
		if imp.LocalPkgName == nil {
			imp.LocalPkgName = nameOf(imp.Path.Pos(), "_")
		} else {
			imp.LocalPkgName.Value = "_"
		}
	}
}
