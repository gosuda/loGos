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

// specConcrete reports whether t is a static type that a clone may adopt:
// typed, not an interface, and not a type parameter or a tuple.
func specConcrete(t types2.Type) bool {
	switch t := types2.Unalias(t).(type) {
	case *types2.Basic:
		return t.Info()&types2.IsUntyped == 0 && t.Kind() != types2.UnsafePointer && t.Kind() != types2.Invalid
	case *types2.TypeParam, *types2.Tuple:
		return false
	default:
		return !specIsIface(t)
	}
}

func specNilable(t types2.Type) bool {
	switch types2.Unalias(t).Underlying().(type) {
	case *types2.Pointer, *types2.Map, *types2.Slice, *types2.Chan, *types2.Signature:
		return true
	default:
		return false
	}
}

// specTypeDepth returns how deeply t nests: one for a basic or named type
// without arguments, and one more than its deepest component for a
// composite type or an instantiated generic type.
func specTypeDepth(t types2.Type) int {
	switch t := types2.Unalias(t).(type) {
	case *types2.Pointer:
		return 1 + specTypeDepth(t.Elem())
	case *types2.Slice:
		return 1 + specTypeDepth(t.Elem())
	case *types2.Array:
		return 1 + specTypeDepth(t.Elem())
	case *types2.Chan:
		return 1 + specTypeDepth(t.Elem())
	case *types2.Map:
		return 1 + max(specTypeDepth(t.Key()), specTypeDepth(t.Elem()))
	case *types2.Named:
		targs := t.TypeArgs()
		if targs == nil || targs.Len() == 0 {
			return 1
		}
		deepest := 0
		for i := range targs.Len() {
			deepest = max(deepest, specTypeDepth(targs.At(i)))
		}
		return 1 + deepest
	}
	return 1
}

// specValueMethodViaPointer reports whether a call through pointer type c
// would reach a value-receiver method declared directly on the pointee. The
// interface call panics with the wrapper's text on a nil pointer and the
// static call with the dereference's, so such a call keeps the interface.
// A method it cannot resolve blocks too. A method promoted through an
// embedded field never panics with the wrapper's text (the generated
// wrapper dereferences the pointer itself), so it does not block.
func specValueMethodViaPointer(c types2.Type, methods []*types2.Func) bool {
	if _, ok := types2.Unalias(c).(*types2.Pointer); !ok {
		return false
	}
	for _, m := range methods {
		obj, index, _ := types2.LookupFieldOrMethod(c, false, m.Pkg(), m.Name())
		fn, ok := obj.(*types2.Func)
		if !ok {
			return true
		}
		if len(index) != 1 {
			continue
		}
		recv := fn.Signature().Recv()
		if recv == nil {
			return true
		}
		if _, ptr := types2.Unalias(recv.Type()).(*types2.Pointer); !ptr {
			return true
		}
	}
	return false
}

// nilSafe reports whether a pointer of type c can never be nil in this
// package: the compile is a loGo/s package, where `*T` is non-nil by the
// checker's rules, and T is declared here. The nil-receiver panic text of a
// value method then cannot differ between the interface call and the static
// call, because no nil pointer reaches either.
func (s *specializer) nilSafe(c types2.Type) bool {
	if !base.Logos {
		return false
	}
	ptr, ok := types2.Unalias(c).(*types2.Pointer)
	if !ok {
		return false
	}
	named, ok := types2.Unalias(ptr.Elem()).(*types2.Named)
	return ok && named.Obj().Pkg() == s.pkg
}

// specSpeller writes types as source expressions at one position of one
// file. With commit set, a package the file does not import under a usable
// name gets an alias import; without it the speller only proves that the
// type can be written.
type specSpeller struct {
	s      *specializer
	file   *syntax.File
	pos    syntax.Pos
	commit bool
}

// canSpell reports whether type t can be written in file.
func (s *specializer) canSpell(file *syntax.File, t types2.Type, pos syntax.Pos) bool {
	_, ok := (&specSpeller{s: s, file: file, pos: pos}).typ(t)
	return ok
}

// spell builds the source form of type t for file, adding the alias imports
// it needs.
func (s *specializer) spell(file *syntax.File, t types2.Type, pos syntax.Pos) (syntax.Expr, bool) {
	return (&specSpeller{s: s, file: file, pos: pos, commit: true}).typ(t)
}

// typ writes t, or reports false when it cannot be written there: local,
// unexported, or type-parameter types, composite shapes beyond pointers,
// slices, arrays, maps and channels, and types of packages no file imports.
func (sp *specSpeller) typ(t types2.Type) (syntax.Expr, bool) {
	pos := sp.pos
	switch t := types2.Unalias(t).(type) {
	case *types2.Basic:
		if !specConcrete(t) || sp.s.pkg.Scope().Lookup(t.Name()) != nil {
			return nil, false
		}
		return nameOf(pos, t.Name()), true
	case *types2.Pointer:
		elem, ok := sp.typ(t.Elem())
		if !ok {
			return nil, false
		}
		return at(&syntax.Operation{Op: syntax.Mul, X: elem}, pos), true
	case *types2.Slice:
		elem, ok := sp.typ(t.Elem())
		if !ok {
			return nil, false
		}
		return at(&syntax.SliceType{Elem: elem}, pos), true
	case *types2.Array:
		elem, ok := sp.typ(t.Elem())
		if !ok {
			return nil, false
		}
		return at(&syntax.ArrayType{Len: litOf(pos, strconv.FormatInt(t.Len(), 10), syntax.IntLit), Elem: elem}, pos), true
	case *types2.Map:
		key, ok := sp.typ(t.Key())
		if !ok {
			return nil, false
		}
		elem, ok := sp.typ(t.Elem())
		if !ok {
			return nil, false
		}
		return at(&syntax.MapType{Key: key, Value: elem}, pos), true
	case *types2.Chan:
		elem, ok := sp.typ(t.Elem())
		if !ok {
			return nil, false
		}
		var dir syntax.ChanDir
		switch t.Dir() {
		case types2.SendOnly:
			dir = syntax.SendOnly
		case types2.RecvOnly:
			dir = syntax.RecvOnly
		}
		return at(&syntax.ChanType{Dir: dir, Elem: elem}, pos), true
	case *types2.Named:
		return sp.named(t)
	}
	return nil, false
}

// named writes a package-level named type, with its type arguments when it
// is an instantiation.
func (sp *specSpeller) named(t *types2.Named) (syntax.Expr, bool) {
	pos := sp.pos
	obj := t.Obj()
	pkg := obj.Pkg()
	if pkg == nil || obj.Parent() != pkg.Scope() {
		return nil, false
	}
	var name syntax.Expr
	if pkg == sp.s.pkg {
		name = nameOf(pos, obj.Name())
	} else {
		if !obj.Exported() {
			return nil, false
		}
		qual, ok := sp.qualifier(pkg)
		if !ok {
			return nil, false
		}
		name = at(&syntax.SelectorExpr{X: nameOf(pos, qual), Sel: nameOf(pos, obj.Name())}, pos)
	}
	targs := t.TypeArgs()
	if targs == nil || targs.Len() == 0 {
		return name, true
	}
	list := make([]syntax.Expr, 0, targs.Len())
	for i := range targs.Len() {
		arg, ok := sp.typ(targs.At(i))
		if !ok {
			return nil, false
		}
		list = append(list, arg)
	}
	index := list[0]
	if len(list) > 1 {
		index = at(&syntax.ListExpr{ElemList: list}, pos)
	}
	return at(&syntax.IndexExpr{X: name, Index: index}, pos), true
}

// qualifier returns the name the file uses for pkg. A file without a usable
// import of it borrows the import path from another file of the package; a
// committing speller adds a reserved alias import for it, and the other
// returns a placeholder.
func (sp *specSpeller) qualifier(pkg *types2.Package) (string, bool) {
	s, file := sp.s, sp.file
	if name, ok := s.refs[file][pkg]; ok {
		return name, true
	}
	if name, ok := s.importName(file, pkg); ok {
		s.cacheRef(file, pkg, name)
		return name, true
	}
	path := s.importPath(pkg)
	if path == nil {
		return "", false
	}
	if !sp.commit {
		return "_", true
	}
	s.importSeq++
	alias := "_logosspec" + strconv.Itoa(s.importSeq)
	pos := file.Pos()
	decl := at(&syntax.ImportDecl{
		LocalPkgName: nameOf(pos, alias),
		Path:         litOf(pos, path.Value, syntax.StringLit),
	}, pos)
	s.added[file] = append(s.added[file], decl)
	s.cacheRef(file, pkg, alias)
	return alias, true
}

func (s *specializer) cacheRef(file *syntax.File, pkg *types2.Package, name string) {
	if s.refs[file] == nil {
		s.refs[file] = make(map[*types2.Package]string)
	}
	s.refs[file][pkg] = name
}

// importName returns the name file gives an ordinary (neither blank nor
// dot) import of pkg.
func (s *specializer) importName(file *syntax.File, pkg *types2.Package) (string, bool) {
	for _, d := range file.DeclList {
		imp, ok := d.(*syntax.ImportDecl)
		if !ok {
			continue
		}
		if pn := s.pkgNameOf(imp); pn != nil && pn.Imported() == pkg && pn.Name() != "_" && pn.Name() != "." {
			return pn.Name(), true
		}
	}
	return "", false
}

// importPath returns the path literal of the first import of pkg in any
// file of the package.
func (s *specializer) importPath(pkg *types2.Package) *syntax.BasicLit {
	for _, file := range s.files {
		for _, d := range file.DeclList {
			imp, ok := d.(*syntax.ImportDecl)
			if !ok || imp.Path == nil {
				continue
			}
			if pn := s.pkgNameOf(imp); pn != nil && pn.Imported() == pkg {
				return imp.Path
			}
		}
	}
	return nil
}

// pkgNameOf returns the package name object the checker recorded for an
// import declaration.
func (s *specializer) pkgNameOf(imp *syntax.ImportDecl) *types2.PkgName {
	var obj types2.Object
	if imp.LocalPkgName != nil {
		obj = s.info.Defs[imp.LocalPkgName]
	} else {
		obj = s.info.Implicits[imp]
	}
	pn, _ := obj.(*types2.PkgName)
	return pn
}
