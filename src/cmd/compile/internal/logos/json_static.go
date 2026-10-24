// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"fmt"
	"strings"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// This file lowers M11-S1 and M11-S2 static JSON (SPEC Sec 12, T-Sec12).
// For each eligible struct (types2.LogosStaticStruct) the lowering appends
// a value-receiver MarshalJSON and a pointer-receiver UnmarshalJSON, both
// plain Go that calls logos/rt/json, plus the key lookup functions of the
// decoder. Field kinds without static support (nested structs, maps,
// arrays, []byte, option payloads, types with their own JSON methods) fall
// back to the reflective encoding/json call per field. The generated text
// is parsed into nodes for the native tree and spelled identically in the
// ejected source, like the const map lowering.

// jsonAlias is the reserved local name of the logos/rt/json import.
const jsonAlias = "_logos_json"

// jsonImportPath is the import path of the static JSON support package.
const jsonImportPath = "logos/rt/json"

// jsonRecv is the receiver name of generated methods; the reserved prefix
// keeps it clear of every type name.
const jsonRecv = "_logos_v"

// scalarKind is the JSON class of a modeled scalar.
type scalarKind int

const (
	scalarBool scalarKind = iota
	scalarInt
	scalarUint
	scalarFloat32
	scalarFloat64
	scalarString
)

// shapeKind is the way a field is serialized.
type shapeKind int

const (
	shapeFallback shapeKind = iota
	shapeScalar
	shapePointer
	shapeSlice
	shapeUnion
)

// jsonShape classifies one field type: its static kind, the scalar it
// carries (the value, the pointee, or the slice element), and whether the
// declared type is a non-nil pointer.
type jsonShape struct {
	kind   shapeKind
	scalar scalarKind
	nonNil bool
}

// jsonField is one JSON-visible field of an eligible struct.
type jsonField struct {
	goName    string
	key       string
	omitEmpty bool
	omitZero  bool
	quoted    bool
	shape     jsonShape
	pos       int
	required  int
	typ       types2.Type
}

// jsonStruct is the generation model of one eligible struct.
type jsonStruct struct {
	name     string
	fields   []*jsonField
	defaults []string
	required []string
}

// lowerStaticJSON generates the serializer methods of every eligible
// struct declared in this file.
func (fl *fileLower) lowerStaticJSON() {
	for _, decl := range fl.file.DeclList {
		if td, ok := decl.(*syntax.TypeDecl); ok {
			fl.lowerStaticJSONType(td)
		}
	}
}

// lowerStaticJSONType generates the methods of one type declaration.
func (fl *fileLower) lowerStaticJSONType(td *syntax.TypeDecl) {
	named := fl.declaredNamed(td)
	if named == nil {
		return
	}
	fields, jsonOK := types2.LogosStaticStruct(fl.pkg.info, named, td.Type)
	binaryFields, binaryOK := types2.LogosBinaryStruct(fl.pkg.info, named, td.Type)
	if !jsonOK && !binaryOK {
		return
	}
	if !jsonOK {
		fields = binaryFields
	}
	js := fl.newJSONStruct(td.Name.Value, named.Underlying().(*types2.Struct), fields)
	var b strings.Builder
	if jsonOK && !userDeclaresMethod(named, "MarshalJSON") {
		if plan := fl.planJSONWalk(js, named); plan.stateful {
			vname := ""
			if key := fl.ensureJSONHelper(plan, named); key != "" {
				vname = fl.pkg.jsonHelpers[key].vname
			}
			if vname != "" {
				fl.emitJSONHelpers(td.Pos(), plan)
				js.marshalTextWalk(&b, plan, vname)
			} else {
				js.marshalText(&b)
			}
		} else {
			js.marshalText(&b)
		}
	}
	if jsonOK && !userDeclaresMethod(named, "UnmarshalJSON") {
		js.unmarshalText(&b)
	}
	if binaryOK {
		fl.lowerStaticBinary(td, named, js, &b)
	}
	if b.Len() == 0 {
		return
	}
	text := b.String()
	fl.addGeneratedDecls(td.Pos(), text)
	fl.needJSON = fl.needJSON || strings.Contains(text, jsonAlias+".")
	fl.needRT = fl.needRT || strings.Contains(text, rtAlias+".")
	fl.needBinary = fl.needBinary || strings.Contains(text, "_logos_b")
}

// declaredNamed returns the named type a package-level type declaration
// defines, or nil for aliases and generic declarations.
func (fl *fileLower) declaredNamed(td *syntax.TypeDecl) *types2.Named {
	if td.Alias || len(td.TParamList) > 0 {
		return nil
	}
	tn, ok := fl.pkg.info.Defs[td.Name].(*types2.TypeName)
	if !ok {
		return nil
	}
	named, _ := tn.Type().(*types2.Named)
	return named
}

// userDeclaresMethod reports whether the source declares a method of that
// name on named; the checker hook gives synthesized methods no position.
func userDeclaresMethod(named *types2.Named, name string) bool {
	for i := range named.NumMethods() {
		m := named.Method(i)
		if m.Name() == name && m.Pos().IsKnown() {
			return true
		}
	}
	return false
}

// addGeneratedDecls appends generated declaration text: parsed nodes for
// the native tree, plain text for the ejected source. Read-only marker
// lines register their var for read-only placement.
func (fl *fileLower) addGeneratedDecls(pos syntax.Pos, text string) {
	decls := parseGeneratedDecls(text, pos)
	for _, d := range decls {
		if vd, ok := d.(*syntax.VarDecl); ok && strings.Contains(text, rodataMarker+"\nvar "+vd.NameList[0].Value) {
			fl.rodataVars = append(fl.rodataVars, rodataVar{vd: vd})
		}
	}
	fl.constDecls = append(fl.constDecls, decls...)
	fl.constText += "\n" + text
}

// parseGeneratedDecls parses declaration text into nodes positioned at pos.
// parseConstMapDecls moves the closing braces; the colon of a case clause
// is the one scope boundary left, and a scope position from the generated
// file would break the order the debug info requires inside one function.
func parseGeneratedDecls(text string, pos syntax.Pos) []syntax.Decl {
	decls := parseConstMapDecls(text, pos)
	for _, d := range decls {
		syntax.Inspect(d, func(n syntax.Node) bool {
			if cc, ok := n.(*syntax.CaseClause); ok {
				cc.Colon = pos
			}
			return true
		})
	}
	return decls
}

// newJSONStruct builds the generation model of one struct: its JSON
// fields in declaration order with their classification, the default
// assignments, and the names of the required fields.
func (fl *fileLower) newJSONStruct(name string, st *types2.Struct, fields []*types2.Var) *jsonStruct {
	return fl.buildJSONStruct(name, st, fields)
}

// buildJSONStruct builds the generation model for a struct with the
// given JSON-visible fields: the compiler-resolved fields of an own
// type, or every field of an imported struct (invisible ones are
// skipped by the field rule, exactly like the reflective encoder
// skips them).
func (fl *fileLower) buildJSONStruct(name string, st *types2.Struct, fields []*types2.Var) *jsonStruct {
	js := &jsonStruct{name: name}
	for _, fld := range fields {
		i := fieldIndexOf(st, fld)
		if i < 0 {
			continue
		}
		info, ok := types2.LogosJSONFieldOf(st, i)
		if !ok {
			continue
		}
		f := &jsonField{
			goName:    fld.Name(),
			key:       info.Name,
			omitEmpty: info.OmitEmpty,
			omitZero:  info.OmitZero,
			pos:       len(js.fields),
			required:  -1,
			typ:       fld.Type(),
		}
		f.shape = fl.classifyJSONField(fld.Type())
		f.quoted = info.StringOpt && f.shape.kind != shapeFallback && f.shape.kind != shapeSlice && f.shape.kind != shapeUnion
		if f.shape.nonNil {
			f.required = len(js.required)
			js.required = append(js.required, f.goName)
		}
		js.fields = append(js.fields, f)
	}
	for _, d := range fl.pkg.info.LogosFieldDefaults[st] {
		js.defaults = append(js.defaults, fmt.Sprintf("%s.%s = %s", jsonRecv, d.Field, renderExpr(d.Default)))
	}
	return js
}

// fieldIndexOf returns the struct position of fld, or -1.
func fieldIndexOf(st *types2.Struct, fld *types2.Var) int {
	for i := range st.NumFields() {
		if st.Field(i) == fld {
			return i
		}
	}
	return -1
}

// classifyJSONField classifies one field type.
func (fl *fileLower) classifyJSONField(t types2.Type) jsonShape {
	if cv, ok := types2.Unalias(t).(*types2.ConstView); ok {
		t = cv.Elem
	}
	_, bare := types2.Unalias(t).(*types2.Pointer)
	if o := fl.asOption(t); o != nil {
		t = o.Elem
		if _, ok := types2.Unalias(t).(*types2.Pointer); !ok {
			return jsonShape{kind: shapeFallback}
		}
	}
	shape := fl.classifyShape(t)
	shape.nonNil = bare
	return shape
}

// classifyShape classifies a type with options and const views removed.
func (fl *fileLower) classifyShape(t types2.Type) jsonShape {
	if _, ok := unionTerms(t); ok {
		return jsonShape{kind: shapeUnion}
	}
	switch c := types2.Unalias(t).(type) {
	case *types2.Pointer:
		return fl.scalarShape(c.Elem(), shapePointer)
	case *types2.Slice:
		return fl.scalarShape(c.Elem(), shapeSlice)
	}
	return fl.scalarShape(t, shapeScalar)
}

// scalarShape returns the shape of kind k over element type elem, or the
// fallback shape when elem is not a modeled scalar.
func (fl *fileLower) scalarShape(elem types2.Type, k shapeKind) jsonShape {
	kind, ok := fl.scalarOf(elem)
	if !ok || (k == shapeSlice && kind == scalarUint && isByteSized(elem)) {
		return jsonShape{kind: shapeFallback}
	}
	return jsonShape{kind: k, scalar: kind}
}

// isByteSized reports whether t is an 8-bit unsigned integer, the element
// kind encoding/json writes as base64.
func isByteSized(t types2.Type) bool {
	b, ok := t.Underlying().(*types2.Basic)
	return ok && b.Kind() == types2.Uint8
}

// jsonMethodNames lists the methods that change how encoding/json treats
// a scalar type; a type with any of them is not modeled.
var jsonMethodNames = []string{
	"MarshalJSON", "UnmarshalJSON", "MarshalJSONTo", "UnmarshalJSONFrom",
	"MarshalText", "UnmarshalText", "AppendText", "IsZero",
}

// scalarOf returns the scalar class of t when encoding/json treats it as a
// plain bool, integer, float, or string.
func (fl *fileLower) scalarOf(t types2.Type) (scalarKind, bool) {
	switch c := types2.Unalias(t).(type) {
	case *types2.Basic:
		return basicScalar(c)
	case *types2.Named:
		b, ok := c.Underlying().(*types2.Basic)
		if !ok || hasJSONMethods(c) {
			return 0, false
		}
		return basicScalar(b)
	}
	return 0, false
}

// hasJSONMethods reports whether named, or a pointer to it, has a method
// that changes JSON encoding, or is encoding/json.Number.
func hasJSONMethods(named *types2.Named) bool {
	obj := named.Obj()
	if obj.Pkg() != nil && obj.Pkg().Path() == "encoding/json" {
		return true
	}
	ptr := types2.NewPointer(named)
	for _, name := range jsonMethodNames {
		if m, _, _ := types2.LookupFieldOrMethod(ptr, false, obj.Pkg(), name); m != nil {
			return true
		}
	}
	return false
}

// basicScalar maps a basic type to its scalar class.
func basicScalar(b *types2.Basic) (scalarKind, bool) {
	switch b.Kind() {
	case types2.Bool:
		return scalarBool, true
	case types2.Int, types2.Int8, types2.Int16, types2.Int32, types2.Int64:
		return scalarInt, true
	case types2.Uint, types2.Uint8, types2.Uint16, types2.Uint32, types2.Uint64, types2.Uintptr:
		return scalarUint, true
	case types2.Float32:
		return scalarFloat32, true
	case types2.Float64:
		return scalarFloat64, true
	case types2.String:
		return scalarString, true
	}
	return 0, false
}

// jsonImport builds the implicit logos/rt/json import of lowered files.
func jsonImport(pos syntax.Pos) *syntax.ImportDecl {
	return at(&syntax.ImportDecl{
		LocalPkgName: nameOf(pos, jsonAlias),
		Path:         litOf(pos, fmt.Sprintf("%q", jsonImportPath), syntax.StringLit),
	}, pos)
}

// reflectAlias is the reserved local name of the reflect import that
// generated traversal tables use.
const reflectAlias = "_logos_reflect"

// reflectImport builds the implicit reflect import of lowered files
// whose generated serializers carry a traversal table.
func reflectImport(pos syntax.Pos) *syntax.ImportDecl {
	return at(&syntax.ImportDecl{
		LocalPkgName: nameOf(pos, reflectAlias),
		Path:         litOf(pos, `"reflect"`, syntax.StringLit),
	}, pos)
}

// helperEligibleType returns the generated-side named struct type t
// names when t is eligible for a static helper: a non-generic struct
// type of this package whose MarshalJSON is generated, not user
// declared. Generatedness is read off the method objects of this
// check: hook-synthesized methods carry no position while user
// declarations do. Promoted methods never qualify: the receiver base
// must be the type itself, so a downstream embedder keeps the honour
// branch. Imported types always funnel through the reflective walk,
// which honours their methods with a fresh encoding: foreign generated
// methods stay honour, with no cross-package sharing.
func (fl *fileLower) helperEligibleType(t types2.Type) *types2.Named {
	u := types2.Unalias(t)
	if o, isOpt := u.(*types2.Option); isOpt {
		u = o.Elem
	}
	if p, ok := u.(*types2.Pointer); ok {
		u = types2.Unalias(p.Elem())
	}
	named, ok := u.(*types2.Named)
	if !ok || named.TypeArgs().Len() != 0 {
		return nil
	}
	if _, ok := named.Underlying().(*types2.Struct); !ok {
		return nil
	}
	if named.Obj().Pkg() != fl.pkg.tpkg {
		return nil
	}
	for i := range named.NumMethods() {
		m := named.Method(i)
		if m.Name() != "MarshalJSON" {
			continue
		}
		recv := m.Signature().Recv().Type()
		if p, ok := recv.(*types2.Pointer); ok {
			recv = p.Elem()
		}
		if recv != named {
			continue
		}
		if m.Pos().IsKnown() {
			return nil
		}
		return named
	}
	return nil
}

// jsonTypeName returns the source spelling of helper-eligible named
// type t in this file. Helpers exist only for own-package types, which
// always spell by bare name.
func (fl *fileLower) jsonTypeName(named *types2.Named) (string, bool) {
	if named.Obj().Pkg() != fl.pkg.tpkg {
		return "", false
	}
	return named.Obj().Name(), true
}
