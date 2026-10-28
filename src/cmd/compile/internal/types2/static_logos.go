// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types2

import (
	"cmd/compile/internal/syntax"
	"strconv"
	"strings"
	"unicode"
)

// LoGo/s M11 static serializer type rules (SPEC Sec 12). The lowering
// generates the methods of logosStaticMethods for each eligible struct,
// but user code in the same package calls those methods before any
// lowering runs, so the checker declares them up front.
// LogosStaticStruct is the one eligibility predicate; the checker hook and
// the lowering both call it, so the two sides cannot disagree. The
// upstream file calls in through one hook line: resolver.go
// (packageObjects, after the type declarations).

// LogosJSONField is the encoding/json view of one struct field: its key,
// whether the key came from the tag, and the tag options the static
// serializer models.
type LogosJSONField struct {
	Name      string
	Tagged    bool
	OmitEmpty bool
	OmitZero  bool
	StringOpt bool
}

// logosMethodShape is the signature of one synthesized method.
type logosMethodShape int

const (
	logosShapeMarshal   logosMethodShape = iota // () ([]byte, error)
	logosShapeUnmarshal                         // (data []byte) error
	logosShapeAppend                            // (dst []byte) ([]byte, error)
)

// logosStaticMethod is one method the hook declares on every eligible
// struct and the lowering generates the body of.
type logosStaticMethod struct {
	name    string
	pointer bool
	shape   logosMethodShape
}

// logosStaticMethods lists every synthesized method. A field named like
// one of them clashes, because Go forbids a field and a method of one
// name. The json family gates LogosStaticStruct; the binary family is
// enforced by the binary generator's own eligibility, so a
// binary-method-named field removes binary generation without touching
// json eligibility.
var logosStaticMethods = []logosStaticMethod{
	{"MarshalJSON", false, logosShapeMarshal},
	{"UnmarshalJSON", true, logosShapeUnmarshal},
	{"MarshalBinary", false, logosShapeMarshal},
	{"UnmarshalBinary", true, logosShapeUnmarshal},
	{"AppendBinary", false, logosShapeAppend},
}

// logosBinaryMethods names the synthesized binary family.
var logosBinaryMethods = []string{"MarshalBinary", "UnmarshalBinary", "AppendBinary"}

// LogosBinaryMethodNames exposes the synthesized binary family names so
// the binary generator can enforce its own field-name clash rule.
func LogosBinaryMethodNames() []string {
	return logosBinaryMethods
}

// logosTagKey splits one `name:"value"` pair off the front of a struct
// tag: the name, and the text starting at the opening quote of the value.
func logosTagKey(tag string) (name, rest string, ok bool) {
	tag = strings.TrimLeft(tag, " ")
	i := 0
	for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
		i++
	}
	if i == 0 || i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
		return "", "", false
	}
	return tag[:i], tag[i+1:], true
}

// logosTagQuoted splits the quoted string at the start of s from the rest.
func logosTagQuoted(s string) (quoted, rest string, ok bool) {
	i := 1
	for i < len(s) && s[i] != '"' {
		if s[i] == '\\' {
			i++
		}
		i++
	}
	if i >= len(s) {
		return "", "", false
	}
	return s[:i+1], s[i+1:], true
}

// logosTagLookup returns the value of key in a struct tag, with the
// conventions of reflect.StructTag.Lookup.
func logosTagLookup(tag, key string) (string, bool) {
	for {
		name, rest, ok := logosTagKey(tag)
		if !ok {
			return "", false
		}
		quoted, rest, ok := logosTagQuoted(rest)
		if !ok {
			return "", false
		}
		if name == key {
			value, err := strconv.Unquote(quoted)
			return value, err == nil
		}
		tag = rest
	}
}

// logosValidTagName reports whether s is a usable encoding/json key: the
// rule of encoding/json isValidTag.
func logosValidTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// logosTagOption reports whether the comma-separated options contain name.
func logosTagOption(opts, name string) bool {
	for opts != "" {
		var cur string
		cur, opts, _ = strings.Cut(opts, ",")
		if cur == name {
			return true
		}
	}
	return false
}

// logosTagOptionsSupported reports whether every option is one the static
// serializer models, each at most once: omitempty, omitzero, or string. Any
// other option, an empty option, or a repeated one is read differently by
// the encoding/json modes, so such a struct keeps the reflective path.
func logosTagOptionsSupported(opts string) bool {
	seen := make(map[string]bool)
	for opts != "" {
		var cur string
		cur, opts, _ = strings.Cut(opts, ",")
		if (cur != "omitempty" && cur != "omitzero" && cur != "string") || seen[cur] {
			return false
		}
		seen[cur] = true
	}
	return true
}

// logosJSONTagSupported reports whether field i of st has a json tag that
// both encoding/json modes read the same way: no tag, `-`, or a name valid
// under the v1 rule with supported options. An unexported field may carry
// no json tag but `-`.
func logosJSONTagSupported(st *Struct, i int) bool {
	tag, has := logosTagLookup(st.Tag(i), "json")
	if !has || tag == "-" {
		return true
	}
	if !st.Field(i).Exported() || strings.HasSuffix(tag, ",") {
		return false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name != "" && !logosValidTagName(name) {
		return false
	}
	return logosTagOptionsSupported(opts)
}

// LogosJSONFieldOf returns the encoding/json view of field i of st, and
// false for a field encoding/json skips: an unexported field or a field
// whose json tag is exactly "-".
func LogosJSONFieldOf(st *Struct, i int) (LogosJSONField, bool) {
	fld := st.Field(i)
	if !fld.Exported() {
		return LogosJSONField{}, false
	}
	tag, _ := logosTagLookup(st.Tag(i), "json")
	if tag == "-" {
		return LogosJSONField{}, false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if !logosValidTagName(name) {
		name = ""
	}
	out := LogosJSONField{
		Name:      name,
		Tagged:    name != "",
		OmitEmpty: logosTagOption(opts, "omitempty"),
		OmitZero:  logosTagOption(opts, "omitzero"),
		StringOpt: logosTagOption(opts, "string"),
	}
	if out.Name == "" {
		out.Name = fld.Name()
	}
	return out, true
}

// logosStaticUnwrap strips options and const views from a field type: both
// lower to their element.
func logosStaticUnwrap(t Type) Type {
	for {
		if o := logosAsOption(t); o != nil {
			t = o.Elem
			continue
		}
		if cv, ok := t.Underlying().(*ConstView); ok {
			t = cv.Elem
			continue
		}
		return t
	}
}

// logosStaticKindOK reports whether a field of type t may appear in a
// statically serialized struct. SPEC Sec 12.1 skips generation for
// interfaces (except closed unions) and for maps with non-string keys.
func logosStaticKindOK(t Type) bool {
	u := logosStaticUnwrap(t)
	switch iface := u.Underlying().(type) {
	case *Interface:
		return logosClosedUnion(u) != nil && !logosContainsLock(u)
	case *Map:
		b, ok := iface.Key().Underlying().(*Basic)
		return ok && b.Info()&IsString != 0
	}
	return true
}

// logosIsNonNilPointer reports whether a field type is a bare pointer,
// possibly under a const view: a required non-nil field.
func logosIsNonNilPointer(t Type) bool {
	if cv, ok := t.Underlying().(*ConstView); ok {
		t = cv.Elem
	}
	_, ok := Unalias(t).(*Pointer)
	return ok
}

// logosStaticTrigger reports whether the struct has json tags, recorded
// defaults, or non-nil pointer fields.
func logosStaticTrigger(info *Info, st *Struct) bool {
	if info != nil && len(info.LogosFieldDefaults[st]) > 0 {
		return true
	}
	for i := range st.NumFields() {
		if _, ok := logosTagLookup(st.Tag(i), "json"); ok || logosIsNonNilPointer(st.Field(i).Type()) {
			return true
		}
	}
	return false
}

// logosStaticNameClash reports a method-named field or two fields that
// encoding/json would give the same key.
func logosStaticNameClash(st *Struct) bool {
	return logosFamilyNameClash(st, "JSON")
}

func logosFamilyNameClash(st *Struct, family string) bool {
	keys := make(map[string]bool)
	for i := range st.NumFields() {
		for _, m := range logosStaticMethods {
			if !strings.HasSuffix(m.name, family) {
				continue
			}
			if st.Field(i).Name() == m.name {
				return true
			}
		}
		f, ok := LogosJSONFieldOf(st, i)
		if !ok {
			continue
		}
		if keys[f.Name] {
			return true
		}
		keys[f.Name] = true
	}
	return false
}

// logosContainsLock reports whether t is or transitively contains a sync lock.
// Generated value-receiver methods must never copy a lock, so such structs keep
// reflective encoding.
func logosContainsLock(t Type) bool {
	return logosContainsLockSeen(t, make(map[Type]bool))
}

func logosContainsLockSeen(t Type, seen map[Type]bool) bool {
	if t == nil {
		return false
	}
	t = Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	if o := logosAsOption(t); o != nil {
		return logosContainsLockSeen(o.Elem, seen)
	}
	if named, ok := t.(*Named); ok {
		if obj := named.Obj(); obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == "sync" {
			switch obj.Name() {
			case "Mutex", "RWMutex", "Cond", "WaitGroup", "Once", "Map", "Pool", "noCopy":
				return true
			}
		}
	}
	switch u := t.Underlying().(type) {
	case *ConstView:
		return logosContainsLockSeen(u.Elem, seen)
	case *Array:
		return logosContainsLockSeen(u.Elem(), seen)
	case *Struct:
		for i := range u.NumFields() {
			if logosContainsLockSeen(u.Field(i).Type(), seen) {
				return true
			}
		}
	case *Interface:
		if tset := logosClosedUnion(t); tset != nil {
			for _, term := range tset.terms {
				if logosContainsLockSeen(term.typ, seen) {
					return true
				}
			}
		}
	}
	return false
}

// LogosStaticStruct reports whether named is eligible for the static
// serializer (SPEC Sec 12.1) and returns its fields in declaration order.
// A package-level, non-generic, non-alias type declared with a struct
// literal (`type T struct{...}`) qualifies when it has no embedded field,
// no interface field, no map field with a non-string key, no field named
// like a generated method, and no json tag the encoding/json modes read
// differently, and when it has json tags, recorded field defaults, or
// non-nil pointer fields. A copy declaration (`type M T`) has the
// underlying struct of T but no methods, which keeps it a reflective twin
// of T. rhs is the type expression of the declaration and info supplies
// the recorded defaults (it may be nil).
func LogosStaticStruct(info *Info, named *Named, rhs syntax.Expr) ([]*Var, bool) {
	if _, ok := syntax.Unparen(rhs).(*syntax.StructType); !ok {
		return nil, false
	}
	if named == nil || named.TypeParams().Len() != 0 || named.TypeArgs().Len() != 0 {
		return nil, false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
		return nil, false
	}
	st, ok := named.Underlying().(*Struct)
	if !ok {
		return nil, false
	}
	if logosContainsLock(named) {
		return nil, false
	}
	fields := make([]*Var, st.NumFields())
	for i := range fields {
		fld := st.Field(i)
		if fld.Embedded() || !logosStaticKindOK(fld.Type()) || !logosJSONTagSupported(st, i) {
			return nil, false
		}
		fields[i] = fld
	}
	if logosStaticNameClash(st) || !logosStaticTrigger(info, st) {
		return nil, false
	}
	return fields, true
}

// LogosBinaryStruct applies the existing structural, tag, and trigger rules,
// but reserves only binary-family method names. Keeping this entry separate
// preserves the established JSON eligibility predicate.
func LogosBinaryStruct(info *Info, named *Named, rhs syntax.Expr) ([]*Var, bool) {
	if _, ok := syntax.Unparen(rhs).(*syntax.StructType); !ok {
		return nil, false
	}
	if named == nil || named.TypeParams().Len() != 0 || named.TypeArgs().Len() != 0 {
		return nil, false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
		return nil, false
	}
	st, ok := named.Underlying().(*Struct)
	if !ok {
		return nil, false
	}
	if logosContainsLock(named) {
		return nil, false
	}
	fields := make([]*Var, st.NumFields())
	for i := range fields {
		fld := st.Field(i)
		if fld.Embedded() || !logosStaticKindOK(fld.Type()) || !logosJSONTagSupported(st, i) {
			return nil, false
		}
		fields[i] = fld
	}
	if logosFamilyNameClash(st, "Binary") || !logosStaticTrigger(info, st) || !LogosBinaryTypeOK(named) {
		return nil, false
	}
	return fields, true
}

// LogosBinaryTypeOK reports whether every serialized field has a binary
// representation. Visiting a type twice closes a recursive shape; another
// unsupported branch still rejects the enclosing shape.
func LogosBinaryTypeOK(t Type) bool {
	return logosBinaryTypeOK(t, make(map[Type]bool))
}

func logosBinaryTypeOK(t Type, seen map[Type]bool) bool {
	t = Unalias(t)
	if seen[t] {
		return true
	}
	seen[t] = true
	if o := logosAsOption(t); o != nil {
		return logosBinaryTypeOK(o.Elem, seen)
	}
	switch u := t.Underlying().(type) {
	case *ConstView:
		return logosBinaryTypeOK(u.Elem, seen)
	case *Basic:
		return u.Info()&(IsBoolean|IsInteger|IsFloat|IsString) != 0
	case *Pointer:
		return logosBinaryTypeOK(u.Elem(), seen)
	case *Slice:
		return logosBinaryTypeOK(u.Elem(), seen)
	case *Array:
		return logosBinaryTypeOK(u.Elem(), seen)
	case *Map:
		key, ok := u.Key().Underlying().(*Basic)
		return ok && key.Info()&IsString != 0 && logosBinaryTypeOK(u.Elem(), seen)
	case *Struct:
		return logosBinaryFieldsOK(u, seen)
	}
	return false
}

func logosBinaryFieldsOK(st *Struct, seen map[Type]bool) bool {
	for i := range st.NumFields() {
		if _, ok := LogosJSONFieldOf(st, i); !ok {
			continue
		}
		if !logosBinaryTypeOK(st.Field(i).Type(), seen) {
			return false
		}
	}
	return true
}

// logosSynthStaticMethods declares the generated serializer methods on
// every eligible struct of the package, except a method the user already
// declares. The synthesized objects carry no position, which tells the
// lowering that no source declaration exists for them.
func (check *Checker) logosSynthStaticMethods() {
	if !check.conf.Logos {
		return
	}
	for _, obj := range check.objList {
		tname, ok := obj.(*TypeName)
		if !ok || tname.typ == nil || check.objMap[tname].tdecl.Alias {
			continue
		}
		named, ok := tname.typ.(*Named)
		if !ok {
			continue
		}
		_, jsonOK := LogosStaticStruct(check.Info, named, check.objMap[tname].tdecl.Type)
		_, binaryOK := LogosBinaryStruct(check.Info, named, check.objMap[tname].tdecl.Type)
		if jsonOK {
			check.logosAddStaticFamilyMethods(named, "JSON")
		}
		if binaryOK {
			check.logosAddStaticFamilyMethods(named, "Binary")
		}
	}
}

// logosAddStaticFamilyMethods adds one eligible method family to named,
// except methods the user already declares.
func (check *Checker) logosAddStaticFamilyMethods(named *Named, family string) {
	for _, m := range logosStaticMethods {
		if !strings.HasSuffix(m.name, family) {
			continue
		}
		if !logosHasMethod(named, m.name) {
			named.AddMethod(check.logosStaticFunc(named, m))
		}
	}
}

// logosHasMethod reports whether named already has a method of that name.
func logosHasMethod(named *Named, name string) bool {
	for i := range named.NumMethods() {
		if named.Method(i).Name() == name {
			return true
		}
	}
	return false
}

// logosStaticFunc builds the method object of m on named.
func (check *Checker) logosStaticFunc(named *Named, m logosStaticMethod) *Func {
	var recvType Type = named
	if m.pointer {
		recvType = NewPointer(named)
	}
	bytes := NewSlice(universeByte)
	result := func(t Type) *Var { return newVar(ResultVar, nopos, check.pkg, "", t) }
	var params, results []*Var
	switch m.shape {
	case logosShapeMarshal:
		results = []*Var{result(bytes), result(universeError)}
	case logosShapeUnmarshal:
		params = []*Var{newVar(ParamVar, nopos, check.pkg, "data", bytes)}
		results = []*Var{result(universeError)}
	case logosShapeAppend:
		params = []*Var{newVar(ParamVar, nopos, check.pkg, "dst", bytes)}
		results = []*Var{result(bytes), result(universeError)}
	}
	recv := newVar(RecvVar, nopos, check.pkg, "v", recvType)
	sig := NewSignatureType(recv, nil, nil, NewTuple(params...), NewTuple(results...), false)
	return NewFunc(nopos, check.pkg, m.name, sig)
}
