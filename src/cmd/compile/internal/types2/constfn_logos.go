// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

// This file implements imported const-function resolution for M5-S2
// (SPEC Sec 2.1, T-201): a per-Checker cache owns parsed constfn
// records and their defining-package context, keyed by the record
// source position base. Imported bodies execute as EntConstFunc in the
// caller's SAME ctfe.Eval (shared steps, frames, depth and value
// limit); no nested evaluator is ever created for function calls.
// Record names resolve in the defining package/closure, never in
// caller scope; evaluator locals keep normal lexical precedence
// because the interpreter consults frames before the host.
//
// One predicate governs host, purity and const-map admission:
// logosResolveImportedConstFunc (with the thin boolean wrapper
// logosAdmitImportedConstFunc). A nil unit with a nil error means an
// ordinary import (no constfn record): call sites keep their existing
// behavior. A non-nil error means a missing, stale, malformed or
// otherwise unusable record, or a nested impurity the defining build
// can no longer vouch for; call sites fail closed with the detail.
//
// Private closure names (helpers, types, consts, vars) resolve from the
// record source, never from the imported scope: export data carries
// only exported objects. The checked signature of the exported root
// still comes from the imported scope. Mutable package variables stay
// impure; their initializers never execute. Methods and generic calls
// stay rejected by the existing evaluator and purity rules.

import (
	"bytes"
	"cmd/compile/internal/logos/ctfe"
	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/syntax"
	"fmt"
	"go/constant"
	"math/big"
	"strconv"
)

// logosConstFnUnit is one validated constfn record with its
// defining-package context. file is the parsed record source; base is
// the file base every node of that source shares, and the key the host
// uses to recognize defining-context nodes.
type logosConstFnUnit struct {
	path      string // defining import path
	defPkg    *Package
	file      *syntax.File
	base      *syntax.PosBase
	root      *syntax.FuncDecl // validated root body
	rootObj   *Func            // defining-package checked object (signature owner)
	funcs     map[string]*syntax.FuncDecl
	isConst   map[string]bool
	types     map[string]syntax.Expr // type underlying expressions by name
	aliases   map[string]bool        // type names declared as aliases
	vars      map[string]bool        // package variable names
	evalIota  int64                  // active iota ordinal during one constant evaluation
	inIota    bool                   // whether evalIota is active
	specs     map[string]*logosUnitConst
	imports   map[string]*Package // qualifier name -> imported package
	dotImport bool
	values    map[string]ctfe.Value // evaluated scope-miss constants
	busyConst map[string]bool       // in-progress constant evaluation
	err       error                 // sticky admission failure
}

// logosUnitConst is one file-scope constant specification with group
// inheritance applied: init and typ are the effective expressions, and
// iota is the spec ordinal within its group (0 outside one), supplied
// to on-demand evaluation.
type logosUnitConst struct {
	name     string
	init     syntax.Expr
	typ      syntax.Expr // may be nil
	iota     int
	usesIota bool
}

// logosConstFnKey namespaces the unit cache by defining path and object.
func logosConstFnKey(path, object string) string { return path + "\x00" + object }

// logosConstFnMaps lazily initializes the per-Checker constfn cache.
func (check *Checker) logosConstFnMaps() {
	if check.logosConstFnUnits == nil {
		check.logosConstFnUnits = map[string]*logosConstFnUnit{}
		check.logosConstFnBases = map[*syntax.PosBase]*logosConstFnUnit{}
		check.logosConstFnBusy = map[string]bool{}
	}
}

// logosUnitForNode reports the defining unit owning n, or nil for
// caller-package and universe nodes. The record file base identifies
// the context; bases are stable per cached unit.
func (check *Checker) logosUnitForNode(n syntax.Node) *logosConstFnUnit {
	if n == nil || check.logosConstFnBases == nil {
		return nil
	}
	base := syntax.StartPos(n).FileBase()
	if base == nil {
		return nil
	}
	return check.logosConstFnBases[base]
}

// logosAdmitImportedConstFunc is the single boolean predicate governing
// host, purity and const-map admission. It reports whether path.object
// names an imported const function with a validated, transitively pure
// body. detail is empty for ordinary imports (no record) and carries
// the failure reason otherwise.
func (check *Checker) logosAdmitImportedConstFunc(path, object string) (admitted bool, detail string) {
	u, _, _, err := check.logosResolveImportedConstFunc(path, object)
	if err != nil {
		return false, err.Error()
	}
	return u != nil, ""
}

// logosResolveImportedConstFunc resolves an imported const function to
// its defining unit, checked object and validated body. A nil unit with
// a nil error is an ordinary import (no usable record): the caller
// keeps its existing behavior. A non-nil error fails closed.
func (check *Checker) logosResolveImportedConstFunc(path, object string) (*logosConstFnUnit, *Func, *syntax.FuncDecl, error) {
	if path == "" || object == "" || check.conf == nil || !check.conf.Logos {
		return nil, nil, nil, nil
	}
	check.logosConstFnMaps()
	key := logosConstFnKey(path, object)
	if u, ok := check.logosConstFnUnits[key]; ok {
		if u.err != nil {
			return nil, nil, nil, u.err
		}
		return u, u.rootObj, u.root, nil
	}
	f, ok := facts.Lookup(path)
	if !ok || f == nil {
		return nil, nil, nil, nil
	}
	var src []byte
	for _, c := range f.ConstFuncs {
		if c.Object == object {
			src = c.Source
			break
		}
	}
	if src == nil {
		return nil, nil, nil, nil
	}
	u, fn, _, err := check.logosBuildConstFnUnit(path, object, src)
	if err != nil {
		check.logosConstFnUnits[key] = &logosConstFnUnit{path: path, err: err}
		return nil, nil, nil, err
	}
	return u, fn, u.root, nil
}

// logosImportForConstFunc imports path for constfn resolution: a
// directly imported package first, then the checker's import cache,
// then the configured importer. It mirrors the comptime copy loader
// (logos cfLoadPackage), which likewise opens packages the checked
// files do not import.
func (check *Checker) logosImportForConstFunc(path string) (*Package, error) {
	for _, pn := range check.imports {
		if imp := pn.Imported(); imp != nil && imp.Path() == path {
			return imp, nil
		}
	}
	for _, imp := range check.impMap {
		if imp != nil && imp.Path() == path {
			return imp, nil
		}
	}
	imp := check.conf.Importer
	if imp == nil {
		return nil, fmt.Errorf("no importer installed")
	}
	return imp.Import(path)
}

// logosBuildConstFnUnit parses, validates and admits one constfn
// record. The unit is cached before transitive admission so call cycles
// resolve to the unit instead of recursing; a failed walk sticks err
// on the unit and fails closed.
func (check *Checker) logosBuildConstFnUnit(path, object string, src []byte) (*logosConstFnUnit, *Func, *syntax.FuncDecl, error) {
	defPkg, err := check.logosImportForConstFunc(path)
	if err != nil {
		return nil, nil, nil, logosConstFnErrorf("cannot import %s for const func %s.%s: %v", path, path, object, err)
	}
	_, root, u, err := logosParseConstFnUnit(path, object, src)
	if err != nil {
		return nil, nil, nil, err
	}
	u.defPkg = defPkg
	obj, ok := defPkg.Scope().Lookup(object).(*Func)
	if !ok {
		return nil, nil, nil, fmt.Errorf("const func %s.%s is unavailable: %s has no such function", path, object, path)
	}
	u.rootObj = obj
	check.logosConstFnMaps()
	check.logosConstFnUnits[logosConstFnKey(path, object)] = u
	check.logosConstFnBases[u.base] = u
	if err := check.logosAdmitUnitImports(u); err != nil {
		u.err = err
		return nil, nil, nil, err
	}
	if err := check.logosAdmitUnitBodies(u); err != nil {
		u.err = err
		return nil, nil, nil, err
	}
	return u, obj, root, nil
}

// logosConstFnErrorf formats a constfn admission failure.
func logosConstFnErrorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// logosParseConstFnUnit parses one record source and indexes its
// file-scope closure: helpers (const and plain), types, vars, const
// specs with group inheritance, and import declarations. It validates
// the root: exactly one file-scope `const func <object>` with no
// receiver, no type parameters and a body.
func logosParseConstFnUnit(path, object string, src []byte) (*syntax.File, *syntax.FuncDecl, *logosConstFnUnit, error) {
	base := syntax.NewFileBase("constfn " + path + "." + object)
	file, err := syntax.Parse(base, bytes.NewReader(src), nil, nil, syntax.LogosMode)
	if err != nil {
		return nil, nil, nil, logosConstFnErrorf("cannot parse const record for %s.%s: %v", path, object, err)
	}
	u := &logosConstFnUnit{
		path:      path,
		file:      file,
		base:      base,
		funcs:     map[string]*syntax.FuncDecl{},
		isConst:   map[string]bool{},
		types:     map[string]syntax.Expr{},
		aliases:   map[string]bool{},
		vars:      map[string]bool{},
		specs:     map[string]*logosUnitConst{},
		imports:   map[string]*Package{},
		values:    map[string]ctfe.Value{},
		busyConst: map[string]bool{},
	}
	var root *syntax.FuncDecl
	var plainRoot *syntax.FuncDecl
	for _, decl := range file.DeclList {
		switch decl := decl.(type) {
		case *syntax.ConstFuncDecl:
			fd := decl.Func
			if fd == nil || fd.Name == nil {
				continue
			}
			name := fd.Name.Value
			if _, dup := u.funcs[name]; dup {
				return nil, nil, nil, logosConstFnErrorf("const record for %s.%s declares %s twice", path, object, name)
			}
			u.funcs[name] = fd
			u.isConst[name] = true
			if name == object {
				root = fd
			}
		case *syntax.FuncDecl:
			if decl.Name == nil {
				continue
			}
			name := decl.Name.Value
			if _, dup := u.funcs[name]; dup {
				return nil, nil, nil, logosConstFnErrorf("const record for %s.%s declares %s twice", path, object, name)
			}
			u.funcs[name] = decl
			if name == object {
				plainRoot = decl
			}
		case *syntax.TypeDecl:
			if decl.Name != nil {
				u.types[decl.Name.Value] = decl.Type
				u.aliases[decl.Name.Value] = decl.Alias
			}
		case *syntax.VarDecl:
			for _, name := range decl.NameList {
				u.vars[name.Value] = true
			}
		}
	}
	if err := logosIndexUnitConsts(u, file); err != nil {
		return nil, nil, nil, err
	}
	if root == nil {
		if plainRoot != nil {
			return nil, nil, nil, logosConstFnErrorf("const record for %s.%s declares plain function %s, not a const func", path, object, object)
		}
		return nil, nil, nil, logosConstFnErrorf("const record for %s.%s has no const func %s", path, object, object)
	}
	if root.Recv != nil {
		return nil, nil, nil, logosConstFnErrorf("const record for %s.%s declares method %s", path, object, object)
	}
	if len(root.TParamList) != 0 {
		return nil, nil, nil, logosConstFnErrorf("const record for %s.%s declares generic function %s", path, object, object)
	}
	if root.Body == nil {
		return nil, nil, nil, logosConstFnErrorf("const record for %s.%s has no body", path, object)
	}
	u.root = root
	return file, root, u, nil
}

// logosIndexUnitConsts indexes file-scope constant specifications,
// applying Go group inheritance: a spec without a type or values
// inherits the previous spec of its group.
func logosIndexUnitConsts(u *logosConstFnUnit, file *syntax.File) error {
	last := map[*syntax.Group]syntax.Expr{}
	lastType := map[*syntax.Group]syntax.Expr{}
	ordinal := map[*syntax.Group]int{}
	for _, decl := range file.DeclList {
		cd, ok := decl.(*syntax.ConstDecl)
		if !ok {
			continue
		}
		if cd.Group == nil {
			// A standalone declaration inherits nothing and counts
			// iota from zero; uninitialized names stay absent.
			if cd.Values == nil {
				continue
			}
			for i, name := range cd.NameList {
				var init syntax.Expr
				if vals := syntax.UnpackListExpr(cd.Values); len(vals) == len(cd.NameList) {
					init = vals[i]
				} else if len(cd.NameList) == 1 {
					init = cd.Values
				} else {
					return logosConstFnErrorf("const record for %s cannot use values for %s", u.path, name.Value)
				}
				if _, dup := u.specs[name.Value]; dup {
					return logosConstFnErrorf("const record for %s declares %s twice", u.path, name.Value)
				}
				u.specs[name.Value] = &logosUnitConst{name: name.Value, init: init, typ: cd.Type, iota: 0, usesIota: logosUsesIota(init)}
			}
			continue
		}
		// Mirror the checker's group rule exactly: a spec carrying its
		// own type or values stands alone (an omitted type stays
		// omitted); only a bare spec inherits the previous values
		// and type together. Inheriting a type alongside fresh
		// values would reject valid sources at conversion.
		ord := ordinal[cd.Group]
		ordinal[cd.Group] = ord + 1
		var effType, effVals syntax.Expr
		if cd.Type != nil || cd.Values != nil {
			effType, effVals = cd.Type, cd.Values
			last[cd.Group], lastType[cd.Group] = effVals, effType
		} else {
			effType, effVals = lastType[cd.Group], last[cd.Group]
		}
		if effVals == nil {
			continue
		}
		vals := syntax.UnpackListExpr(effVals)
		for i, name := range cd.NameList {
			var init syntax.Expr
			switch {
			case len(vals) == len(cd.NameList):
				init = vals[i]
			case len(cd.NameList) == 1:
				init = effVals
			default:
				return logosConstFnErrorf("const record for %s cannot inherit values for %s", u.path, name.Value)
			}
			if _, dup := u.specs[name.Value]; dup {
				return logosConstFnErrorf("const record for %s declares %s twice", u.path, name.Value)
			}
			u.specs[name.Value] = &logosUnitConst{name: name.Value, init: init, typ: effType, iota: ord, usesIota: logosUsesIota(init)}
		}
	}
	return nil
}

// logosUsesIota reports whether e refers to iota.
func logosUsesIota(e syntax.Expr) bool {
	found := false
	syntax.Inspect(e, func(n syntax.Node) bool {
		if found {
			return false
		}
		if id, ok := n.(*syntax.Name); ok && id.Value == "iota" {
			found = true
			return false
		}
		return true
	})
	return found
}

// logosAdmitUnitImports resolves the record's import declarations to
// packages: explicit local names as written, otherwise the imported
// package's own name. Blank imports are skipped; dot imports mark the
// unit so bare-name resolution fails closed instead of guessing.
func (check *Checker) logosAdmitUnitImports(u *logosConstFnUnit) error {
	for _, decl := range u.file.DeclList {
		id, ok := decl.(*syntax.ImportDecl)
		if !ok || id.Path == nil {
			continue
		}
		path, err := strconv.Unquote(id.Path.Value)
		if err != nil {
			return logosConstFnErrorf("const record for %s has bad import path %s", u.path, id.Path.Value)
		}
		local := ""
		if id.LocalPkgName != nil {
			local = id.LocalPkgName.Value
		}
		if local == "_" {
			continue
		}
		if local == "." {
			u.dotImport = true
			continue
		}
		imp, err := check.logosImportForConstFunc(path)
		if err != nil {
			return logosConstFnErrorf("cannot import %s for const func %s.%s: %v", path, u.path, u.root.Name.Value, err)
		}
		if local == "" {
			local = imp.Name()
			if local == "" || local == "_" || local == "." {
				return logosConstFnErrorf("const record for %s imports %s under unusable name %q", u.path, path, local)
			}
		}
		if prev, dup := u.imports[local]; dup && prev != imp {
			return logosConstFnErrorf("const record for %s imports %s twice as %s", u.path, path, local)
		}
		u.imports[local] = imp
	}
	return nil
}

// logosAdmitUnitBodies validates the transitive closure of the root:
// every imported selector call or use names an admitted const
// function, a constant, or a type; every reachable scope-miss
// constant pre-evaluates with the same configured limits. Same-package
// helpers are walked, not re-checked: the defining build already
// applied the purity rules to them. Call cycles resolve through the
// cache: the unit is cached before the walk, so a cycle admits
// tentatively and the shared step limit bounds evaluation.
func (check *Checker) logosAdmitUnitBodies(u *logosConstFnUnit) error {
	visited := map[*syntax.FuncDecl]bool{}
	stack := []*syntax.FuncDecl{u.root}
	for len(stack) > 0 {
		fd := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[fd] {
			continue
		}
		visited[fd] = true
		var werr error
		syntax.Inspect(fd.Body, func(n syntax.Node) bool {
			if werr != nil {
				return false
			}
			switch n := n.(type) {
			case *syntax.CallExpr:
				werr = check.logosAdmitUnitCall(u, n, &stack)
				return werr == nil
			case *syntax.SelectorExpr:
				werr = check.logosAdmitUnitSel(u, n)
				return false
			case *syntax.Name:
				werr = check.logosAdmitUnitName(u, n)
				return true
			}
			return true
		})
		if werr != nil {
			return werr
		}
	}
	return nil
}

// logosAdmitUnitCall admits one call inside a defining body: imported
// selector calls resolve through the dependency's own record (with the
// checker's cycle guard); same-package helper calls join the walk;
// everything else is left to evaluation, which rejects methods,
// generics and literals with the existing rules.
func (check *Checker) logosAdmitUnitCall(u *logosConstFnUnit, c *syntax.CallExpr, stack *[]*syntax.FuncDecl) error {
	switch fun := syntax.Unparen(c.Fun).(type) {
	case *syntax.SelectorExpr:
		x, ok := syntax.Unparen(fun.X).(*syntax.Name)
		if !ok {
			return nil
		}
		dep, ok := u.imports[x.Value]
		if !ok {
			return nil
		}
		return check.logosAdmitUnitDep(u, dep, fun.Sel.Value)
	case *syntax.Name:
		if fd, ok := u.funcs[fun.Value]; ok && fd.Body != nil {
			*stack = append(*stack, fd)
		}
	}
	return nil
}

// logosAdmitUnitSel admits one selector use inside a defining body: a
// qualifier naming an imported package must select an admitted const
// function, a constant, or a type. Package variables stay impure and
// missing names fail closed with the dependency named.
func (check *Checker) logosAdmitUnitSel(u *logosConstFnUnit, sel *syntax.SelectorExpr) error {
	x, ok := syntax.Unparen(sel.X).(*syntax.Name)
	if !ok {
		return nil
	}
	dep, ok := u.imports[x.Value]
	if !ok {
		return nil
	}
	obj := dep.Scope().Lookup(sel.Sel.Value)
	switch obj := obj.(type) {
	case *Func:
		return check.logosAdmitUnitDep(u, dep, sel.Sel.Value)
	case *Const:
		return nil
	case *TypeName:
		return nil
	case *Var:
		return logosConstFnErrorf("read of imported variable %s.%s in const func %s.%s", dep.Path(), obj.Name(), u.path, u.root.Name.Value)
	}
	return logosConstFnErrorf("cannot resolve %s.%s in const func %s.%s", dep.Path(), sel.Sel.Value, u.path, u.root.Name.Value)
}

// logosAdmitUnitDep admits one dependency edge u -> dep.name: the
// target must itself resolve to an admitted imported const function.
// A dependency already under admission admits tentatively; the shared
// evaluator step limit bounds cyclic calls.
func (check *Checker) logosAdmitUnitDep(u *logosConstFnUnit, dep *Package, name string) error {
	key := logosConstFnKey(dep.Path(), name)
	if check.logosConstFnBusy[key] {
		return nil
	}
	check.logosConstFnBusy[key] = true
	defer delete(check.logosConstFnBusy, key)
	du, _, _, err := check.logosResolveImportedConstFunc(dep.Path(), name)
	if err != nil {
		return logosConstFnErrorf("call to imported function %s.%s in const func %s.%s: %v", dep.Path(), name, u.path, u.root.Name.Value, err)
	}
	if du == nil {
		return logosConstFnErrorf("call to imported function %s.%s in const func %s.%s", dep.Path(), name, u.path, u.root.Name.Value)
	}
	return nil
}

// logosAdmitUnitName pre-evaluates scope-miss constants reachable from
// defining bodies, so evaluation never reports them as unresolvable
// names. Checked (in-scope) constants and universe names need nothing:
// the host resolves them directly.
func (check *Checker) logosAdmitUnitName(u *logosConstFnUnit, id *syntax.Name) error {
	if id.Value == "_" {
		return nil
	}
	if obj, ok := u.defPkg.Scope().Lookup(id.Value).(*Const); ok {
		if obj.Val() == nil || obj.Val().Kind() == constant.Unknown {
			if _, err := check.logosUnitConstValue(u, id.Value); err != nil {
				return err
			}
		}
		return nil
	}
	if _, ok := u.specs[id.Value]; ok {
		_, err := check.logosUnitConstValue(u, id.Value)
		return err
	}
	return nil
}

// logosUnitConstValue evaluates one scope-miss defining constant from
// its record declaration, with the same configured step and value
// limits as caller initializers (never a relaxed limit). Results are
// cached per unit; cycles fail closed.
func (check *Checker) logosUnitConstValue(u *logosConstFnUnit, name string) (ctfe.Value, error) {
	if v, ok := u.values[name]; ok {
		return v, nil
	}
	if u.busyConst[name] {
		return ctfe.Value{}, logosConstFnErrorf("const %s in %s is part of an initialization cycle", name, u.path)
	}
	spec, ok := u.specs[name]
	if !ok {
		return ctfe.Value{}, logosConstFnErrorf("const %s in %s has no value", name, u.path)
	}
	u.busyConst[name] = true
	defer delete(u.busyConst, name)
	savedIota, savedIn := u.evalIota, u.inIota
	u.evalIota, u.inIota = int64(spec.iota), spec.usesIota
	defer func() { u.evalIota, u.inIota = savedIota, savedIn }()
	host := &ctfeHost{check: check}
	cfg := ctfe.Config{MaxSteps: check.conf.LogosCTFEMaxSteps, WordSize: check.logosWordSize()}
	target := spec.init
	if spec.typ != nil {
		// Evaluate through the declared type exactly as a checked
		// declaration conversion does, preserving the explicit
		// named type and target width before caching. The
		// conversion node is synthetic; only its Fun and Arg carry
		// positions, which is all the host and evaluator consult
		// (error positions wrap in the const identity below).
		// One evaluation only: the conversion evaluates the init
		// in place, so typed constants never consume steps twice.
		target = &syntax.CallExpr{Fun: spec.typ, ArgList: []syntax.Expr{spec.init}}
	}
	v, _, err := ctfe.Eval(host, cfg, target)
	if err != nil {
		return ctfe.Value{}, logosConstFnErrorf("cannot evaluate const %s in %s: %v", name, u.path, err)
	}
	if u.values == nil {
		u.values = map[string]ctfe.Value{}
	}
	u.values[name] = v
	return v, nil
}

// logosPrescanImportedCalls admits every imported selector in a const
// initializer before evaluation, so malformed, stale or impure records
// (and nested ordinary calls) report their detail instead of running
// and failing as unrelated unresolved names. Ordinary imports (no
// record) need nothing and evaluate as before.
func (check *Checker) logosPrescanImportedCalls(obj *Const, init syntax.Expr) error {
	var perr error
	syntax.Inspect(init, func(n syntax.Node) bool {
		if perr != nil {
			return false
		}
		sel, ok := n.(*syntax.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := syntax.Unparen(sel.X).(*syntax.Name)
		if !ok {
			return true
		}
		var path string
		if pn, ok := check.Info.Uses[x].(*PkgName); ok && pn.Imported() != nil {
			path = pn.Imported().Path()
		} else if pn, ok := check.pkg.scope.Lookup(x.Value).(*PkgName); ok && pn.Imported() != nil {
			path = pn.Imported().Path()
		} else {
			return true
		}
		if _, _, _, err := check.logosResolveImportedConstFunc(path, sel.Sel.Value); err != nil {
			perr = err
			return false
		}
		return false
	})
	return perr
}

// resolveUnitName resolves a bare identifier in defining context:
// qualifiers first (so qualified calls route to ResolveSel), then the
// checked scope (exported declarations), then the record closure for
// private helpers, types, constants and variables, then universe.
func (h *ctfeHost) resolveUnitName(u *logosConstFnUnit, id *syntax.Name) (ctfe.Entity, bool) {
	check := h.check
	if id.Value == "iota" {
		if u.inIota {
			return ctfe.Entity{Kind: ctfe.EntConst, Const: ctfe.IntValue(big.NewInt(u.evalIota), ctfe.UntypedInt())}, true
		}
		return ctfe.Entity{}, false
	}
	if _, ok := u.imports[id.Value]; ok {
		return ctfe.Entity{Kind: ctfe.EntPackage, Name: id.Value}, true
	}
	if obj := u.defPkg.Scope().Lookup(id.Value); obj != nil {
		if ent, ok := h.unitEntityOf(u, obj); ok {
			return ent, true
		}
		if _, isConst := obj.(*Const); isConst {
			if v, err := check.logosUnitConstValue(u, id.Value); err == nil {
				return ctfe.Entity{Kind: ctfe.EntConst, Const: v}, true
			}
		}
		return ctfe.Entity{}, false
	}
	if fd, ok := u.funcs[id.Value]; ok {
		if u.isConst[id.Value] {
			fn := h.buildUnitConstFunc(u, fd)
			if fn == nil {
				return ctfe.Entity{}, false
			}
			return ctfe.Entity{Kind: ctfe.EntConstFunc, Fn: fn}, true
		}
		fn := h.buildUnitPlainFunc(u, fd)
		if fn == nil {
			return ctfe.Entity{}, false
		}
		return ctfe.Entity{Kind: ctfe.EntPlainFunc, Fn: fn}, true
	}
	if spec, ok := u.specs[id.Value]; ok {
		if v, err := check.logosUnitConstValue(u, spec.name); err == nil {
			return ctfe.Entity{Kind: ctfe.EntConst, Const: v}, true
		}
		return ctfe.Entity{}, false
	}
	if u.vars[id.Value] {
		return ctfe.Entity{Kind: ctfe.EntPackageVar, Name: u.path + "." + id.Value}, true
	}
	if _, ok := u.types[id.Value]; ok {
		return ctfe.Entity{Kind: ctfe.EntType}, true
	}
	if u.dotImport {
		return ctfe.Entity{}, false
	}
	if obj := Universe.Lookup(id.Value); obj != nil {
		switch obj := obj.(type) {
		case *Builtin:
			return ctfe.Entity{Kind: ctfe.EntBuiltin, Builtin: obj.name}, true
		case *TypeName:
			return ctfe.Entity{Kind: ctfe.EntType}, true
		}
	}
	return ctfe.Entity{}, false
}

// unitEntityOf classifies one checked object of the defining package.
// Unlike entityOf it never touches the caller's declaration tracking:
// the defining package is fully checked, and its objects carry exact
// values. Unknown-valued constants fall back to record evaluation.
func (h *ctfeHost) unitEntityOf(u *logosConstFnUnit, obj Object) (ctfe.Entity, bool) {
	check := h.check
	switch obj := obj.(type) {
	case *Const:
		if obj.Val() != nil && obj.Val().Kind() != constant.Unknown {
			v, ok := h.constToValue(obj.Val(), h.fromType(obj.Type()))
			if !ok {
				return ctfe.Entity{}, false
			}
			return ctfe.Entity{Kind: ctfe.EntConst, Const: v}, true
		}
		if v, err := check.logosUnitConstValue(u, obj.Name()); err == nil {
			return ctfe.Entity{Kind: ctfe.EntConst, Const: v}, true
		}
		return ctfe.Entity{}, false
	case *Var:
		if _, isConst := u.specs[obj.Name()]; isConst {
			if v, err := check.logosUnitConstValue(u, obj.Name()); err == nil {
				return ctfe.Entity{Kind: ctfe.EntConst, Const: v}, true
			}
		}
		name := obj.Name()
		if obj.Pkg() != nil {
			name = obj.Pkg().Path() + "." + obj.Name()
		}
		return ctfe.Entity{Kind: ctfe.EntPackageVar, Name: name}, true
	case *Func:
		body, ok := u.funcs[obj.Name()]
		if !ok || body == nil {
			return ctfe.Entity{Kind: ctfe.EntImport, Name: u.path + "." + obj.Name()}, true
		}
		fn := h.buildFunc(obj, body)
		if fn == nil {
			return ctfe.Entity{}, false
		}
		if u.isConst[obj.Name()] {
			fn.Const = true
			return ctfe.Entity{Kind: ctfe.EntConstFunc, Fn: fn}, true
		}
		return ctfe.Entity{Kind: ctfe.EntPlainFunc, Fn: fn}, true
	case *TypeName:
		return ctfe.Entity{Kind: ctfe.EntType}, true
	}
	return ctfe.Entity{}, false
}

// resolveUnitSel resolves a selector in defining context: qualifiers
// map to the defining imports (never the caller's), so transitive
// calls resolve through their own package facts. Admitted const
// targets execute from the dependency's validated body with the
// imported checked signature; every other shape keeps its existing
// entity so evaluation reports the established detail.
func (h *ctfeHost) resolveUnitSel(u *logosConstFnUnit, sel *syntax.SelectorExpr) ctfe.Entity {
	check := h.check
	x, ok := syntax.Unparen(sel.X).(*syntax.Name)
	if !ok {
		return ctfe.Entity{}
	}
	dep, ok := u.imports[x.Value]
	if !ok {
		return ctfe.Entity{}
	}
	obj := dep.Scope().Lookup(sel.Sel.Value)
	switch obj := obj.(type) {
	case *Const:
		if obj.Val() == nil {
			return ctfe.Entity{}
		}
		v, ok := h.constToValue(obj.Val(), h.fromType(obj.Type()))
		if !ok {
			return ctfe.Entity{}
		}
		return ctfe.Entity{Kind: ctfe.EntConst, Const: v}
	case *Var:
		return ctfe.Entity{Kind: ctfe.EntPackageVar, Name: dep.Path() + "." + obj.Name()}
	case *Func:
		du, fn, body, err := check.logosResolveImportedConstFunc(dep.Path(), obj.Name())
		if err != nil || du == nil {
			return ctfe.Entity{Kind: ctfe.EntImport, Name: dep.Path() + "." + obj.Name()}
		}
		callee := h.buildFunc(fn, body)
		if callee == nil {
			return ctfe.Entity{}
		}
		callee.Const = true
		return ctfe.Entity{Kind: ctfe.EntConstFunc, Fn: callee}
	case *TypeName:
		return ctfe.Entity{Kind: ctfe.EntType}
	}
	return ctfe.Entity{}
}

// resolveUnitTypeExpr resolves a type expression in defining context:
// the checked defining scope first, the record type closure second,
// universe last. Private defined types keep their declaration name
// and defining path with Opaque set, so later spelling keeps failing
// closed instead of misspelling them in caller source.
func (h *ctfeHost) resolveUnitTypeExpr(u *logosConstFnUnit, e syntax.Expr) *ctfe.Type {
	return h.resolveUnitTypeExprIn(u, e, map[string]bool{})
}

func (h *ctfeHost) resolveUnitTypeExprIn(u *logosConstFnUnit, e syntax.Expr, busy map[string]bool) *ctfe.Type {
	check := h.check
	switch e := syntax.Unparen(e).(type) {
	case *syntax.Name:
		if t := ctfe.BasicType(e.Value, check.logosWordSize()); t != nil {
			return t
		}
		if e.Value == "byte" {
			return ctfe.BasicType("uint8", check.logosWordSize())
		}
		if e.Value == "rune" {
			return ctfe.BasicType("int32", check.logosWordSize())
		}
		if e.Value == "any" || e.Value == "error" {
			return nil
		}
		if obj, ok := u.defPkg.Scope().Lookup(e.Value).(*TypeName); ok {
			return h.fromType(obj.Type())
		}
		if under, ok := u.types[e.Value]; ok && under != nil {
			if busy[e.Value] {
				return nil
			}
			if u.aliases[e.Value] {
				busy[e.Value] = true
				t := h.resolveUnitTypeExprIn(u, under, busy)
				delete(busy, e.Value)
				return t
			}
			busy[e.Value] = true
			t := h.resolveUnitTypeExprIn(u, under, busy)
			delete(busy, e.Value)
			if t == nil || t.Impure != "" {
				return t
			}
			t.Named = e.Value
			t.Opaque = true
			t.PkgPath = u.path
			return t
		}
		if obj := Universe.Lookup(e.Value); obj != nil {
			if tn, ok := obj.(*TypeName); ok {
				return h.fromType(tn.Type())
			}
		}
	case *syntax.SelectorExpr:
		return h.resolveUnitQualified(u, e)
	case *syntax.SliceType:
		if el := h.resolveUnitTypeExprIn(u, e.Elem, busy); el != nil {
			return &ctfe.Type{Kind: ctfe.KSlice, Elem: el}
		}
	case *syntax.ArrayType:
		el := h.resolveUnitTypeExprIn(u, e.Elem, busy)
		if el == nil {
			return nil
		}
		if e.Len == nil {
			return &ctfe.Type{Kind: ctfe.KArray, Len: -1, Elem: el}
		}
		if n, ok := h.arrayLength(e.Len); ok {
			return &ctfe.Type{Kind: ctfe.KArray, Len: n, Elem: el}
		}
	case *syntax.MapType:
		k := h.resolveUnitTypeExprIn(u, e.Key, busy)
		v := h.resolveUnitTypeExprIn(u, e.Value, busy)
		if k != nil && v != nil {
			return &ctfe.Type{Kind: ctfe.KMap, Key: k, Elem: v}
		}
	case *syntax.StructType:
		fields := make([]ctfe.Field, 0, len(e.FieldList))
		for _, f := range e.FieldList {
			ft := h.resolveUnitTypeExprIn(u, f.Type, busy)
			if ft == nil {
				return nil
			}
			name := ""
			if f.Name != nil {
				name = f.Name.Value
			}
			fields = append(fields, ctfe.Field{Name: name, Type: ft})
		}
		return &ctfe.Type{Kind: ctfe.KStruct, Fields: fields}
	case *syntax.DotsType:
		return h.resolveUnitTypeExprIn(u, e.Elem, busy)
	case *syntax.ConstType:
		return h.resolveUnitTypeExprIn(u, e.Elem, busy)
	case *syntax.CallExpr:
		if _, ok := syntax.Unparen(e.Fun).(*syntax.Name); ok {
			return h.resolveUnitTypeExprIn(u, e.Fun, busy)
		}
		if sel, ok := syntax.Unparen(e.Fun).(*syntax.SelectorExpr); ok {
			return h.resolveUnitQualified(u, sel)
		}
	case *syntax.CompositeLit:
		return h.resolveUnitTypeExprIn(u, e.Type, busy)
	}
	return nil
}

// resolveUnitQualified resolves pkg.T through the defining imports.
func (h *ctfeHost) resolveUnitQualified(u *logosConstFnUnit, sel *syntax.SelectorExpr) *ctfe.Type {
	x, ok := syntax.Unparen(sel.X).(*syntax.Name)
	if !ok {
		return nil
	}
	dep, ok := u.imports[x.Value]
	if !ok {
		return nil
	}
	if obj, ok := dep.Scope().Lookup(sel.Sel.Value).(*TypeName); ok {
		t := h.fromType(obj.Type())
		if t == nil {
			return nil
		}
		t.Opaque = true
		return t
	}
	return nil
}

// buildUnitConstFunc assembles an interpreter function for a private
// const helper from its record body and structural types.
func (h *ctfeHost) buildUnitConstFunc(u *logosConstFnUnit, body *syntax.FuncDecl) *ctfe.Func {
	fn := h.buildUnitFuncImpl(u, body)
	if fn != nil {
		fn.Const = true
	}
	return fn
}

// buildUnitPlainFunc assembles an interpreter function for a private
// plain helper from its record body and structural types.
func (h *ctfeHost) buildUnitPlainFunc(u *logosConstFnUnit, body *syntax.FuncDecl) *ctfe.Func {
	return h.buildUnitFuncImpl(u, body)
}

// buildUnitFuncImpl assembles an interpreter function for a private
// helper from its record body and structural types. The root always
// uses the imported checked signature instead (unitEntityOf/buildFunc).
func (h *ctfeHost) buildUnitFuncImpl(u *logosConstFnUnit, body *syntax.FuncDecl) *ctfe.Func {
	if body == nil || body.Body == nil {
		return nil
	}
	fn := &ctfe.Func{Name: body.Name.Value, Body: body.Body}
	if len(body.TParamList) != 0 {
		fn.Generic = true
		return fn
	}
	for _, f := range body.Type.ParamList {
		pt := h.resolveUnitTypeExpr(u, f.Type)
		name := f.Name
		if name == nil {
			name = syntax.NewName(syntax.StartPos(f.Type), "")
		}
		fn.Params = append(fn.Params, ctfe.Param{Decl: name, Type: pt})
	}
	if n := len(body.Type.ParamList); n > 0 {
		if dots, ok := syntax.Unparen(body.Type.ParamList[n-1].Type).(*syntax.DotsType); ok && dots != nil {
			fn.Variadic = true
		}
	}
	for _, f := range body.Type.ResultList {
		fn.Results = append(fn.Results, h.resolveUnitTypeExpr(u, f.Type))
	}
	fn.ResultNames = resultNames(body)
	return fn
}
