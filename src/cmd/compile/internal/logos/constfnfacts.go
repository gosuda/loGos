// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"cmd/compile/internal/base"
	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// EmitConstFnFacts adds one constfn record per exported receiverless
// non-generic const function of pkg to f (allocating it when nil) and
// returns f. Each record's Source is one parseable Logos file: a package
// clause, explicit imports, and the complete same-package declaration
// closure of the function — every same-package exported or private
// constant, type, and helper function its body names, transitively, with
// grouped const blocks kept whole so implicit values and iota ordinals
// survive. Calls into other packages are not inlined: the record keeps
// the import spelled with the canonical alias of the payload and splices
// every resolved PkgName use to that alias, and the consumer resolves
// those callees through their own package facts. Mutable package names
// stay out of the record: their initializers must never evaluate in the
// consumer, and a closure needing one is impure and fails at admission.
// A broken closure is a producer defect and reports through the ordinary
// error channel rather than publishing a partial record.
func EmitConstFnFacts(pkg *types2.Package, info *types2.Info, files []*syntax.File, src map[*syntax.File][]byte, f *facts.Facts) *facts.Facts {
	if pkg == nil || pkg.Scope() == nil || info == nil {
		return f
	}
	var roots []*syntax.ConstFuncDecl
	for _, file := range files {
		if _, ok := src[file]; !ok {
			continue
		}
		for _, decl := range file.DeclList {
			cfd, ok := decl.(*syntax.ConstFuncDecl)
			if !ok || cfd.Func == nil || cfd.Func.Name == nil {
				continue
			}
			obj, _ := info.Defs[cfd.Func.Name].(*types2.Func)
			if obj == nil || !obj.Exported() || obj.Pkg() != pkg {
				continue
			}
			sig, ok := obj.Type().(*types2.Signature)
			if !ok || sig.Recv() != nil {
				continue
			}
			if tp := sig.TypeParams(); tp != nil && tp.Len() > 0 {
				continue
			}
			roots = append(roots, cfd)
		}
	}
	if len(roots) == 0 {
		return f
	}
	for _, cfd := range roots {
		if f == nil {
			f = &facts.Facts{}
		}
		payload, err := constFnClosure(pkg, info, files, src, cfd)
		if err != nil {
			base.Errorf("%s: logos: cannot close const function %s: %v", cfd.Func.Pos().String(), cfd.Func.Name.Value, err)
			continue
		}
		f.ConstFuncs = append(f.ConstFuncs, facts.ConstFuncFact{Object: cfd.Func.Name.Value, Source: payload})
	}
	return f
}

func constFnClosure(pkg *types2.Package, info *types2.Info, files []*syntax.File, src map[*syntax.File][]byte, cfd *syntax.ConstFuncDecl) ([]byte, error) {
	rootFile := fileOf(files, cfd.Func.Pos())
	if rootFile == nil {
		return nil, fmt.Errorf("root declaration's file not found")
	}
	s := newConstFnSrc(pkg, info, files, src)
	s.indexAll()

	rootObj, ok := info.Defs[cfd.Func.Name].(*types2.Func)
	if !ok || rootObj == nil {
		return nil, fmt.Errorf("const function %s has no checked object", cfd.Func.Name.Value)
	}
	seen := map[types2.Object]bool{rootObj: true}
	if err := s.collectDecl(rootFile, syntax.Node(cfd.Func), seen); err != nil {
		return nil, err
	}
	s.finalizeImports()

	rootText, err := s.renderConstFunc(rootFile, cfd)
	if err != nil {
		return nil, err
	}
	funcs := []constFnPiece{{name: cfd.Func.Name.Value, text: rootText, pos: cfd.ConstPos}}

	for _, entry := range s.collectedFuncs {
		var text string
		var pos syntax.Pos
		switch d := entry.decl.(type) {
		case *syntax.ConstFuncDecl:
			t, err := s.renderConstFunc(entry.file, d)
			if err != nil {
				return nil, err
			}
			text, pos = t, d.ConstPos
		case *syntax.FuncDecl:
			t, err := s.renderPlainFunc(entry.file, d)
			if err != nil {
				return nil, err
			}
			text, pos = t, d.Pos()
		default:
			return nil, fmt.Errorf("unexpected function declaration %T", entry.decl)
		}
		funcs = append(funcs, constFnPiece{name: entry.name, text: text, pos: pos})
	}

	var typesList []constFnPiece
	for _, entry := range s.collectedTypes {
		td, ok := entry.decl.(*syntax.TypeDecl)
		if !ok || td == nil {
			return nil, fmt.Errorf("unexpected type declaration %T", entry.decl)
		}
		t, err := s.renderType(entry.file, td)
		if err != nil {
			return nil, err
		}
		typesList = append(typesList, constFnPiece{name: entry.name, text: t, pos: td.Pos()})
	}

	var constsList []constFnPiece
	for _, entry := range s.collectedConsts {
		cd, ok := entry.decl.(*syntax.ConstDecl)
		if !ok || cd == nil {
			return nil, fmt.Errorf("unexpected const declaration %T", entry.decl)
		}
		t, err := s.renderConst(entry.file, cd)
		if err != nil {
			return nil, err
		}
		constsList = append(constsList, constFnPiece{name: entry.name, text: t, pos: cd.Pos()})
	}

	var varsList []constFnPiece
	for _, entry := range s.collectedVars {
		vd, ok := entry.decl.(*syntax.VarDecl)
		if !ok || vd == nil {
			return nil, fmt.Errorf("unexpected var declaration %T", entry.decl)
		}
		t, err := s.renderVar(entry.file, vd, entry.name)
		if err != nil {
			return nil, err
		}
		varsList = append(varsList, constFnPiece{name: entry.name, text: t, pos: vd.Pos()})
	}

	sortPieces(funcs[1:])
	sortPieces(typesList)
	sortPieces(constsList)
	sortPieces(varsList)

	var b strings.Builder
	b.WriteString("package " + rootFile.PkgName.Value + "\n\n")

	imps := slices.Clone(s.imports)
	slices.SortFunc(imps, func(a, b cfImport) int { return strings.Compare(a.path, b.path) })
	for _, imp := range imps {
		b.WriteString("import " + imp.name + " " + strconv.Quote(imp.path) + "\n")
	}
	if len(imps) > 0 {
		b.WriteString("\n")
	}

	for _, fn := range funcs {
		b.WriteString(fn.text)
		b.WriteString("\n\n")
	}
	for _, td := range typesList {
		b.WriteString(td.text)
		b.WriteString("\n\n")
	}
	for _, cd := range constsList {
		b.WriteString(cd.text)
		b.WriteString("\n\n")
	}
	for _, vd := range varsList {
		b.WriteString(vd.text)
		b.WriteString("\n\n")
	}

	return []byte(b.String()), nil
}

type constFnPiece struct {
	name string
	text string
	pos  syntax.Pos
}

func sortPieces(list []constFnPiece) {
	slices.SortFunc(list, func(a, b constFnPiece) int {
		if c := cmp.Compare(a.pos.FileBase().Filename(), b.pos.FileBase().Filename()); c != 0 {
			return c
		}
		if c := cmp.Compare(a.pos.Line(), b.pos.Line()); c != 0 {
			return c
		}
		return cmp.Compare(a.pos.Col(), b.pos.Col())
	})
}

type constFnDecl struct {
	name string
	decl syntax.Decl
	file *syntax.File
}

type pkgUse struct {
	pos  syntax.Pos
	len  int
	path string
	file *syntax.File
}

type constFnSrc struct {
	pkg             *types2.Package
	info            *types2.Info
	files           []*syntax.File
	src             map[*syntax.File][]byte
	starts          map[*syntax.File][]int
	decls           map[string]*constFnDecl
	rawImports      map[string]string
	imports         []cfImport
	pkgUses         []pkgUse
	closedNames     map[string]bool
	collectedFuncs  []*constFnDecl
	collectedTypes  []*constFnDecl
	collectedConsts []*constFnDecl
	collectedVars   []*constFnDecl
	file            *syntax.File
	orig            []byte
	curStarts       []int
}

func newConstFnSrc(pkg *types2.Package, info *types2.Info, files []*syntax.File, src map[*syntax.File][]byte) *constFnSrc {
	starts := make(map[*syntax.File][]int, len(files))
	for _, f := range files {
		if data, ok := src[f]; ok && data != nil {
			starts[f] = lineStarts(data)
		}
	}
	return &constFnSrc{
		pkg:         pkg,
		info:        info,
		files:       files,
		src:         src,
		starts:      starts,
		decls:       make(map[string]*constFnDecl),
		rawImports:  make(map[string]string),
		closedNames: make(map[string]bool),
	}
}

func (s *constFnSrc) indexAll() {
	for _, file := range s.files {
		if _, ok := s.src[file]; !ok {
			continue
		}
		for _, decl := range file.DeclList {
			switch decl := decl.(type) {
			case *syntax.FuncDecl:
				// Methods never enter a closure: collection only follows
				// package-scope objects, so indexing one would only shadow
				// the type or function sharing its name.
				if decl.Name != nil && decl.Recv == nil {
					s.decls[decl.Name.Value] = &constFnDecl{name: decl.Name.Value, decl: decl, file: file}
				}
			case *syntax.ConstFuncDecl:
				if decl.Func != nil && decl.Func.Name != nil && decl.Func.Recv == nil {
					s.decls[decl.Func.Name.Value] = &constFnDecl{name: decl.Func.Name.Value, decl: decl, file: file}
				}
			case *syntax.TypeDecl:
				if decl.Name != nil {
					s.decls[decl.Name.Value] = &constFnDecl{name: decl.Name.Value, decl: decl, file: file}
				}
			case *syntax.ConstDecl:
				for _, n := range decl.NameList {
					if n != nil {
						s.decls[n.Value] = &constFnDecl{name: n.Value, decl: decl, file: file}
					}
				}
			case *syntax.VarDecl:
				for _, n := range decl.NameList {
					if n != nil {
						s.decls[n.Value] = &constFnDecl{name: n.Value, decl: decl, file: file}
					}
				}
			}
		}
	}
}

func (s *constFnSrc) at(file *syntax.File) bool {
	orig, ok := s.src[file]
	if !ok || orig == nil {
		return false
	}
	s.file = file
	s.orig = orig
	s.curStarts = s.starts[file]
	return true
}

func (s *constFnSrc) off(pos syntax.Pos) int {
	return s.curStarts[pos.Line()-1] + int(pos.Col()) - 1
}

func (s *constFnSrc) collectDecl(file *syntax.File, root syntax.Node, seen map[types2.Object]bool) error {
	var firstErr error
	syntax.Inspect(root, func(n syntax.Node) bool {
		if firstErr != nil {
			return false
		}
		name, ok := n.(*syntax.Name)
		if !ok {
			return true
		}
		s.closedNames[name.Value] = true
		obj := s.info.Uses[name]
		if obj == nil {
			return true
		}
		if pn, ok := obj.(*types2.PkgName); ok {
			s.pkgUses = append(s.pkgUses, pkgUse{
				pos:  name.Pos(),
				len:  len(name.Value),
				path: pn.Imported().Path(),
				file: file,
			})
			if err := s.addImport(pn); err != nil && firstErr == nil {
				firstErr = err
				return false
			}
			return true
		}
		if obj.Pkg() != s.pkg {
			return true
		}
		if s.pkg.Scope().Lookup(obj.Name()) != obj {
			return true
		}
		if seen[obj] {
			return true
		}
		if err := s.collectObject(obj, seen); err != nil && firstErr == nil {
			firstErr = err
			return false
		}
		return true
	})
	return firstErr
}

func (s *constFnSrc) collectObject(obj types2.Object, seen map[types2.Object]bool) error {
	if obj == nil || obj.Pkg() != s.pkg || seen[obj] {
		return nil
	}
	if s.pkg.Scope().Lookup(obj.Name()) != obj {
		return nil
	}
	switch obj := obj.(type) {
	case *types2.Func:
		seen[obj] = true
		entry, ok := s.decls[obj.Name()]
		if !ok || entry == nil {
			return fmt.Errorf("declaration of same-package function %s not found", obj.Name())
		}
		s.collectedFuncs = append(s.collectedFuncs, entry)
		var inner *syntax.FuncDecl
		switch d := entry.decl.(type) {
		case *syntax.ConstFuncDecl:
			inner = d.Func
		case *syntax.FuncDecl:
			inner = d
		}
		return s.collectDecl(entry.file, syntax.Node(inner), seen)

	case *types2.TypeName:
		seen[obj] = true
		entry, ok := s.decls[obj.Name()]
		if !ok || entry == nil {
			return fmt.Errorf("declaration of same-package type %s not found", obj.Name())
		}
		td, ok := entry.decl.(*syntax.TypeDecl)
		if !ok || td == nil {
			return fmt.Errorf("declaration of same-package type %s not found", obj.Name())
		}
		if td.Group != nil {
			for _, spec := range s.groupTypeSpecs(entry.file, td.Group) {
				if spec.Name != nil {
					if o := s.info.Defs[spec.Name]; o != nil {
						seen[o] = true
					}
				}
			}
		}
		s.collectedTypes = append(s.collectedTypes, entry)
		if td.Group != nil {
			for _, spec := range s.groupTypeSpecs(entry.file, td.Group) {
				if spec.Type != nil {
					if err := s.collectDecl(entry.file, syntax.Node(spec.Type), seen); err != nil {
						return err
					}
				}
			}
			return nil
		}
		return s.collectDecl(entry.file, syntax.Node(td.Type), seen)

	case *types2.Const:
		seen[obj] = true
		entry, ok := s.decls[obj.Name()]
		if !ok || entry == nil {
			return fmt.Errorf("declaration of same-package constant %s not found", obj.Name())
		}
		cd, ok := entry.decl.(*syntax.ConstDecl)
		if !ok || cd == nil {
			return fmt.Errorf("declaration of same-package constant %s not found", obj.Name())
		}
		for _, n := range cd.NameList {
			if n != nil {
				if o := s.info.Defs[n]; o != nil {
					seen[o] = true
				}
			}
		}
		if cd.Group != nil {
			for _, spec := range s.groupConstSpecs(entry.file, cd.Group) {
				for _, n := range spec.NameList {
					if n != nil {
						if o := s.info.Defs[n]; o != nil {
							seen[o] = true
						}
					}
				}
			}
		}
		s.collectedConsts = append(s.collectedConsts, entry)
		if cd.Group != nil {
			for _, spec := range s.groupConstSpecs(entry.file, cd.Group) {
				if spec.Values != nil {
					if err := s.collectDecl(entry.file, syntax.Node(spec.Values), seen); err != nil {
						return err
					}
				}
				if spec.Type != nil {
					if err := s.collectDecl(entry.file, syntax.Node(spec.Type), seen); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if cd.Values != nil {
			if err := s.collectDecl(entry.file, syntax.Node(cd.Values), seen); err != nil {
				return err
			}
		}
		if cd.Type != nil {
			if err := s.collectDecl(entry.file, syntax.Node(cd.Type), seen); err != nil {
				return err
			}
		}
		return nil

	case *types2.Var:
		seen[obj] = true
		entry, ok := s.decls[obj.Name()]
		if !ok || entry == nil {
			return fmt.Errorf("declaration of same-package variable %s not found", obj.Name())
		}
		vd, ok := entry.decl.(*syntax.VarDecl)
		if !ok || vd == nil {
			return fmt.Errorf("declaration of same-package variable %s not found", obj.Name())
		}
		s.collectedVars = append(s.collectedVars, entry)
		if vd.Type != nil {
			return s.collectDecl(entry.file, syntax.Node(vd.Type), seen)
		}
		return s.collectNamedTypes(obj.Type(), seen)
	}
	return nil
}

func (s *constFnSrc) collectNamedTypes(t types2.Type, seen map[types2.Object]bool) error {
	if t == nil {
		return nil
	}
	if a, ok := t.(*types2.Alias); ok {
		// Keep the alias itself: unaliasing first would drop Alias.Obj,
		// so an exported alias over a private target would vanish from
		// the closure even though the emitted source spells it.
		if a.Obj() != nil {
			if opkg := a.Obj().Pkg(); opkg != nil && opkg != s.pkg {
				if err := s.recordImport(opkg.Path(), opkg.Name()); err != nil {
					return err
				}
			} else if opkg == s.pkg {
				if err := s.collectObject(a.Obj(), seen); err != nil {
					return err
				}
			}
		}
		if ta := a.TypeArgs(); ta != nil {
			for i := range ta.Len() {
				if err := s.collectNamedTypes(ta.At(i), seen); err != nil {
					return err
				}
			}
		}
		return s.collectNamedTypes(a.Rhs(), seen)
	}
	switch t := types2.Unalias(t).(type) {
	case *types2.Named:
		if t.Obj() != nil {
			if opkg := t.Obj().Pkg(); opkg != nil && opkg != s.pkg {
				if err := s.recordImport(opkg.Path(), opkg.Name()); err != nil {
					return err
				}
			} else if opkg == s.pkg {
				if err := s.collectObject(t.Obj(), seen); err != nil {
					return err
				}
			}
		}
		if tp := t.TypeArgs(); tp != nil {
			for i := range tp.Len() {
				if err := s.collectNamedTypes(tp.At(i), seen); err != nil {
					return err
				}
			}
		}
	case *types2.Pointer:
		return s.collectNamedTypes(t.Elem(), seen)
	case *types2.Slice:
		return s.collectNamedTypes(t.Elem(), seen)
	case *types2.Array:
		return s.collectNamedTypes(t.Elem(), seen)
	case *types2.Map:
		if err := s.collectNamedTypes(t.Key(), seen); err != nil {
			return err
		}
		return s.collectNamedTypes(t.Elem(), seen)
	case *types2.Chan:
		return s.collectNamedTypes(t.Elem(), seen)
	case *types2.Struct:
		for i := range t.NumFields() {
			if err := s.collectNamedTypes(t.Field(i).Type(), seen); err != nil {
				return err
			}
		}
	case *types2.Tuple:
		for i := range t.Len() {
			if err := s.collectNamedTypes(t.At(i).Type(), seen); err != nil {
				return err
			}
		}
	case *types2.Signature:
		if t.Params() != nil {
			if err := s.collectNamedTypes(t.Params(), seen); err != nil {
				return err
			}
		}
		if t.Results() != nil {
			if err := s.collectNamedTypes(t.Results(), seen); err != nil {
				return err
			}
		}
	case *types2.Interface:
		for i := range t.NumExplicitMethods() {
			if err := s.collectNamedTypes(t.ExplicitMethod(i).Type(), seen); err != nil {
				return err
			}
		}
		for i := range t.NumEmbeddeds() {
			if err := s.collectNamedTypes(t.EmbeddedType(i), seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *constFnSrc) addImport(pn *types2.PkgName) error {
	if pn == nil || pn.Imported() == nil {
		return fmt.Errorf("import of unnamed package")
	}
	return s.recordImport(pn.Imported().Path(), pn.Imported().Name())
}

func (s *constFnSrc) recordImport(path, name string) error {
	if name == "" || name == "." || name == "_" {
		return fmt.Errorf("import of %s binds no usable package name", path)
	}
	if _, ok := s.rawImports[path]; !ok {
		s.rawImports[path] = name
	}
	return nil
}

func (s *constFnSrc) finalizeImports() {
	paths := slices.Sorted(maps.Keys(s.rawImports))
	for _, path := range paths {
		baseName := s.rawImports[path]
		name := baseName
		if !s.freshName(name) {
			for i := 1; ; i++ {
				cand := name + strconv.Itoa(i)
				if s.freshName(cand) {
					name = cand
					break
				}
			}
		}
		s.imports = append(s.imports, cfImport{name: name, path: path})
	}
}

func (s *constFnSrc) aliasFor(path string) string {
	for _, imp := range s.imports {
		if imp.path == path {
			return imp.name
		}
	}
	return ""
}

func (s *constFnSrc) freshName(name string) bool {
	for _, imp := range s.imports {
		if imp.name == name {
			return false
		}
	}
	if s.closedNames[name] {
		return false
	}
	for _, file := range s.files {
		if slices.ContainsFunc(file.DeclList, func(d syntax.Decl) bool {
			return declDeclaresName(d, name)
		}) {
			return false
		}
	}
	return true
}

func declDeclaresName(d syntax.Decl, name string) bool {
	switch d := d.(type) {
	case *syntax.FuncDecl:
		return d.Name != nil && d.Name.Value == name
	case *syntax.ConstFuncDecl:
		return d.Func != nil && d.Func.Name != nil && d.Func.Name.Value == name
	case *syntax.TypeDecl:
		return d.Name != nil && d.Name.Value == name
	case *syntax.ConstDecl:
		return slices.ContainsFunc(d.NameList, func(n *syntax.Name) bool { return n != nil && n.Value == name })
	case *syntax.VarDecl:
		return slices.ContainsFunc(d.NameList, func(n *syntax.Name) bool { return n != nil && n.Value == name })
	case *syntax.ImportDecl:
		if d.LocalPkgName != nil {
			return d.LocalPkgName.Value == name
		}
		if d.Path != nil {
			p := unquotePath(d.Path.Value)
			if _, last, ok := strings.CutLast(p, "/"); ok {
				return last == name
			}
			return p == name
		}
	}
	return false
}

func unquotePath(lit string) string {
	if len(lit) >= 2 && lit[0] == '"' && lit[len(lit)-1] == '"' {
		return lit[1 : len(lit)-1]
	}
	return lit
}

func (s *constFnSrc) renderConstFunc(file *syntax.File, cfd *syntax.ConstFuncDecl) (string, error) {
	if !s.at(file) {
		return "", fmt.Errorf("cannot load file for const function %s", cfd.Func.Name.Value)
	}
	start := s.off(cfd.ConstPos)
	end := s.funcEnd(cfd.Func)
	if end <= start || end > len(s.orig) {
		return "", fmt.Errorf("span out of bounds for const function %s", cfd.Func.Name.Value)
	}
	return s.spliceRange(file, start, end)
}

func (s *constFnSrc) renderPlainFunc(file *syntax.File, fd *syntax.FuncDecl) (string, error) {
	if !s.at(file) {
		return "", fmt.Errorf("cannot load file for function %s", fd.Name.Value)
	}
	start := cfFuncStart(s.orig, s.off(syntax.StartPos(fd)))
	end := s.funcEnd(fd)
	if end <= start || end > len(s.orig) {
		return "", fmt.Errorf("span out of bounds for function %s", fd.Name.Value)
	}
	return s.spliceRange(file, start, end)
}

func (s *constFnSrc) renderType(file *syntax.File, td *syntax.TypeDecl) (string, error) {
	if !s.at(file) {
		return "", fmt.Errorf("cannot load file for type %s", td.Name.Value)
	}
	start, ok := s.typeStart(td)
	if !ok {
		return "", fmt.Errorf("cannot find type keyword for %s", td.Name.Value)
	}
	end := s.typeEnd(td)
	if end <= start || end > len(s.orig) {
		return "", fmt.Errorf("span out of bounds for type %s", td.Name.Value)
	}
	return s.spliceRange(file, start, end)
}

func (s *constFnSrc) renderConst(file *syntax.File, cd *syntax.ConstDecl) (string, error) {
	if !s.at(file) {
		return "", fmt.Errorf("cannot load file for const %s", cd.NameList[0].Value)
	}
	start, ok := s.constStart(cd)
	if !ok {
		return "", fmt.Errorf("cannot find const keyword for %s", cd.NameList[0].Value)
	}
	end := s.constEnd(cd)
	if end <= start || end > len(s.orig) {
		return "", fmt.Errorf("span out of bounds for const %s", cd.NameList[0].Value)
	}
	return s.spliceRange(file, start, end)
}

func (s *constFnSrc) renderVar(file *syntax.File, vd *syntax.VarDecl, name string) (string, error) {
	s.at(file)
	if vd.Type != nil {
		start := s.off(syntax.StartPos(vd.Type))
		end := s.typeExprEnd(vd.Type)
		if end <= start || end > len(s.orig) {
			return "", fmt.Errorf("span out of bounds for var %s type", name)
		}
		typeText, err := s.spliceRange(file, start, end)
		if err != nil {
			return "", err
		}
		return "var " + name + " " + typeText, nil
	}
	obj := s.pkg.Scope().Lookup(name)
	if obj != nil {
		typStr := types2.TypeString(obj.Type(), func(p *types2.Package) string {
			if p == s.pkg {
				return ""
			}
			return s.aliasFor(p.Path())
		})
		return "var " + name + " " + typStr, nil
	}
	return "var " + name + " int", nil
}

func (s *constFnSrc) spliceRange(file *syntax.File, start, end int) (string, error) {
	raw := s.orig[start:end]
	var subs []splice
	for _, u := range s.pkgUses {
		if u.file != file {
			continue
		}
		uOff := s.starts[file][u.pos.Line()-1] + int(u.pos.Col()) - 1
		uEnd := uOff + u.len
		if uOff >= start && uEnd <= end {
			alias := s.aliasFor(u.path)
			if alias != "" && alias != string(s.orig[uOff:uEnd]) {
				subs = append(subs, splice{
					start: uOff - start,
					end:   uEnd - start,
					text:  alias,
				})
			}
		}
	}
	if len(subs) == 0 {
		return string(raw), nil
	}
	out, err := applySplices(raw, subs)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (s *constFnSrc) funcEnd(fd *syntax.FuncDecl) int {
	if fd.Body != nil {
		off := s.off(syntax.EndPos(fd))
		if off < len(s.orig) && s.orig[off] == '}' {
			return off + 1
		}
		return off
	}
	return s.off(syntax.EndPos(fd.Type))
}

func (s *constFnSrc) typeStart(td *syntax.TypeDecl) (int, bool) {
	if td.Group != nil {
		specs := s.groupTypeSpecs(s.file, td.Group)
		if len(specs) == 0 {
			return 0, false
		}
		firstOff := s.off(specs[0].Pos())
		return s.scanBackGroupKeyword(firstOff, "type")
	}
	return s.scanBackSingleKeyword(s.off(td.Pos()), "type")
}

func (s *constFnSrc) typeEnd(td *syntax.TypeDecl) int {
	if td.Group != nil {
		specs := s.groupTypeSpecs(s.file, td.Group)
		if len(specs) == 0 {
			return s.off(syntax.EndPos(td))
		}
		last := specs[len(specs)-1]
		return s.scanForwardCloseParen(s.specTypeEnd(last))
	}
	return s.specTypeEnd(td)
}

func (s *constFnSrc) specTypeEnd(td *syntax.TypeDecl) int {
	if e := s.typeExprEnd(td.Type); e > 0 {
		return e
	}
	return s.off(syntax.EndPos(td))
}

func (s *constFnSrc) scanBracedTypeEnd(start int) int {
	depth := 0
	inString := byte(0)
	for off := start; off < len(s.orig); off++ {
		c := s.orig[off]
		if inString != 0 {
			if c == inString && (inString == '`' || s.orig[off-1] != '\\') {
				inString = 0
			}
			continue
		}
		if c == '"' || c == '`' {
			inString = c
			continue
		}
		if c == '\'' {
			off++
			for off < len(s.orig) && s.orig[off] != '\'' {
				if s.orig[off] == '\\' {
					off++
				}
				off++
			}
			continue
		}
		if c == '/' && off+1 < len(s.orig) && s.orig[off+1] == '/' {
			for off < len(s.orig) && s.orig[off] != '\n' {
				off++
			}
			continue
		}
		if c == '/' && off+1 < len(s.orig) && s.orig[off+1] == '*' {
			off += 2
			for off+1 < len(s.orig) && (s.orig[off] != '*' || s.orig[off+1] != '/') {
				off++
			}
			off++
			continue
		}
		if c == '{' {
			depth++
			continue
		}
		if c == '}' {
			depth--
			if depth == 0 {
				return off + 1
			}
			continue
		}
	}
	return start
}

func (s *constFnSrc) constStart(cd *syntax.ConstDecl) (int, bool) {
	if cd.Group != nil {
		specs := s.groupConstSpecs(s.file, cd.Group)
		if len(specs) == 0 {
			return 0, false
		}
		firstOff := s.off(specs[0].Pos())
		return s.scanBackGroupKeyword(firstOff, "const")
	}
	return s.scanBackSingleKeyword(s.off(cd.Pos()), "const")
}

func (s *constFnSrc) constEnd(cd *syntax.ConstDecl) int {
	if cd.Group != nil {
		specs := s.groupConstSpecs(s.file, cd.Group)
		if len(specs) == 0 {
			return s.off(syntax.EndPos(cd))
		}
		last := specs[len(specs)-1]
		return s.scanForwardCloseParen(s.specConstEnd(last))
	}
	return s.specConstEnd(cd)
}

func (s *constFnSrc) specConstEnd(cd *syntax.ConstDecl) int {
	if cd.Values != nil {
		return s.exprEnd(cd.Values)
	}
	if cd.Type != nil {
		return s.off(syntax.EndPos(cd.Type))
	}
	return s.off(syntax.EndPos(cd))
}

func (s *constFnSrc) groupConstSpecs(file *syntax.File, g *syntax.Group) []*syntax.ConstDecl {
	var specs []*syntax.ConstDecl
	for _, d := range file.DeclList {
		if c, ok := d.(*syntax.ConstDecl); ok && c.Group == g {
			specs = append(specs, c)
		}
	}
	return specs
}

func (s *constFnSrc) groupTypeSpecs(file *syntax.File, g *syntax.Group) []*syntax.TypeDecl {
	var specs []*syntax.TypeDecl
	for _, d := range file.DeclList {
		if t, ok := d.(*syntax.TypeDecl); ok && t.Group == g {
			specs = append(specs, t)
		}
	}
	return specs
}

func (s *constFnSrc) scanBackSingleKeyword(nameOff int, kw string) (int, bool) {
	return s.matchKeyword(s.skipTriviaBack(nameOff), kw)
}

func (s *constFnSrc) scanBackGroupKeyword(firstOff int, kw string) (int, bool) {
	i := s.skipTriviaBack(firstOff)
	if i > 0 && s.orig[i-1] == '(' {
		i = s.skipTriviaBack(i - 1)
	}
	return s.matchKeyword(i, kw)
}

func (s *constFnSrc) matchKeyword(i int, kw string) (int, bool) {
	kLen := len(kw)
	if i >= kLen && string(s.orig[i-kLen:i]) == kw {
		if i == kLen || isWordBoundary(s.orig[i-kLen-1]) {
			return i - kLen, true
		}
	}
	return 0, false
}

func (s *constFnSrc) scanForwardCloseParen(afterLast int) int {
	if off := s.skipTriviaFwd(afterLast); off < len(s.orig) && s.orig[off] == ')' {
		return off + 1
	}
	return afterLast
}

func isWordBoundary(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == ';' || b == '(' || b == ')'
}

func (s *constFnSrc) declEndOfExpr(e syntax.Expr) int {
	if e == nil {
		return 0
	}
	return s.off(syntax.EndPos(e))
}

// typeExprEnd returns the end offset of one type expression, scanning a
// braced struct or interface past its field list to the true closing
// brace, whose EndPos stops at the last field, and a func type past its
// parameter and result lists, whose EndPos stops at the `func` keyword
// when both lists are empty. Map and channel types end at their element
// type, recursing for func-typed elements.
func (s *constFnSrc) typeExprEnd(e syntax.Expr) int {
	switch e := e.(type) {
	case *syntax.StructType, *syntax.InterfaceType:
		return s.scanBracedTypeEnd(s.off(syntax.StartPos(e)))
	case *syntax.FuncType:
		return s.scanFuncTypeEnd(e)
	case *syntax.MapType:
		return s.typeExprEnd(e.Value)
	case *syntax.ChanType:
		return s.typeExprEnd(e.Elem)
	}
	return s.declEndOfExpr(e)
}

// scanFuncTypeEnd returns the end of the func type ft: past
// parenthesized results, past a single result type, or past the
// parameter list when there are no results. A negative return fails the
// span bounds check that follows, failing the closure closed instead of
// truncating it.
func (s *constFnSrc) scanFuncTypeEnd(ft *syntax.FuncType) int {
	if ft == nil {
		return -1
	}
	off := s.skipTriviaFwd(s.off(syntax.StartPos(ft)) + len("func"))
	if off >= len(s.orig) || s.orig[off] != '(' {
		return -1
	}
	off = s.skipBalanced(off)
	if off < 0 {
		return -1
	}
	off = s.skipTriviaFwd(off)
	if off < len(s.orig) && s.orig[off] == '(' {
		return s.skipBalanced(off)
	}
	if len(ft.ResultList) == 0 {
		return off
	}
	last := ft.ResultList[len(ft.ResultList)-1]
	if last == nil || last.Type == nil {
		return -1
	}
	return s.typeExprEnd(last.Type)
}

// skipBalanced returns the offset just past the group closing the
// bracket opener at start (one of "([{")), or -1 when none closes it.
// Strings, rune literals, and comments cannot hold brackets, and valid
// source balances every opener, so counting the target pair is exact.
func (s *constFnSrc) skipBalanced(start int) int {
	if start >= len(s.orig) {
		return -1
	}
	var close byte
	switch s.orig[start] {
	case '(':
		close = ')'
	case '[':
		close = ']'
	case '{':
		close = '}'
	default:
		return -1
	}
	open := s.orig[start]
	depth := 0
	for off := start; off < len(s.orig); {
		if c := s.orig[off]; c == '"' || c == '`' || c == '\'' {
			off = s.skipString(off)
			continue
		}
		if j := s.skipTriviaFwd(off); j != off {
			off = j
			continue
		}
		if c := s.orig[off]; c == open {
			depth++
		} else if c == close {
			depth--
			if depth == 0 {
				return off + 1
			}
		}
		off++
	}
	return -1
}

// skipString returns the offset just past the string or rune literal
// opening at off.
func (s *constFnSrc) skipString(off int) int {
	q := s.orig[off]
	if q == '`' {
		for off++; off < len(s.orig) && s.orig[off] != '`'; off++ {
		}
		return off + 1
	}
	off++
	for off < len(s.orig) && s.orig[off] != q {
		if s.orig[off] == '\\' {
			off++
		}
		off++
	}
	return off + 1
}

func (s *constFnSrc) exprEnd(e syntax.Expr) int {
	off := s.off(syntax.EndPos(e))
	if _, ok := e.(*syntax.CallExpr); ok {
		// EndPos of a nullary call stops at its function, so consume
		// the empty parens: the sibling closer first, then the same
		// shape through trivia for comments between the parens.
		if close, ok := nullaryCallEnd(s.orig, off); ok {
			return close
		}
		if j := s.skipTriviaFwd(off); j < len(s.orig) && s.orig[j] == '(' {
			if k := s.skipTriviaFwd(j + 1); k < len(s.orig) && s.orig[k] == ')' {
				return k + 1
			}
		}
	}
	if j := s.skipTriviaFwd(off); j < len(s.orig) {
		switch s.orig[j] {
		case ')', ']', '}':
			return j + 1
		}
	}
	return off
}

// skipTriviaFwd advances past whitespace and comments. Callers index the
// result only after a bounds check, so an overrun at EOF just ends the
// skip.
func (s *constFnSrc) skipTriviaFwd(off int) int {
	for off < len(s.orig) {
		c := s.orig[off]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			off++
			continue
		}
		if c == '/' && off+1 < len(s.orig) {
			if s.orig[off+1] == '/' {
				for off < len(s.orig) && s.orig[off] != '\n' {
					off++
				}
				continue
			}
			if s.orig[off+1] == '*' {
				off += 2
				for off+1 < len(s.orig) && (s.orig[off] != '*' || s.orig[off+1] != '/') {
					off++
				}
				off += 2
				if off > len(s.orig) {
					off = len(s.orig)
				}
				continue
			}
		}
		break
	}
	return off
}

// skipTriviaBack moves past whitespace and comments.
func (s *constFnSrc) skipTriviaBack(i int) int {
	for i > 0 {
		c := s.orig[i-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i--
			continue
		}
		if c == '/' && i >= 2 && s.orig[i-2] == '/' {
			for i > 0 && s.orig[i-1] != '\n' {
				i--
			}
			continue
		}
		if c == '/' && i >= 2 && s.orig[i-2] == '*' {
			if start := s.blockCommentStart(i); start >= 0 {
				i = start
				continue
			}
		}
		break
	}
	return i
}

// blockCommentStart returns the start of the block comment ending at i
// (exclusive), or -1 when none opens it. Comments do not nest, so the
// nearest opener with no intervening closer is the match; anything else
// fails the keyword check that follows, exactly as before.
func (s *constFnSrc) blockCommentStart(i int) int {
	for j := i - 2; j >= 1; j-- {
		if s.orig[j-1] == '/' && s.orig[j] == '*' {
			return j - 1
		}
	}
	return -1
}

func fileOf(files []*syntax.File, pos syntax.Pos) *syntax.File {
	for _, f := range files {
		if f != nil && f.Pos().Base() == pos.Base() {
			return f
		}
	}
	return nil
}
