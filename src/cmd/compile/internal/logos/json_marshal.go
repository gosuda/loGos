// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"cmp"
	"fmt"
	"strings"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// This file spells the generated MarshalJSON (SPEC Sec 12.1). The method
// pins every modeled field into a local, runs the checks that can fail
// (floats, reflective fallbacks) in declaration order, computes the output
// size from fixed key literals plus the helper lengths, then fills one
// buffer made once.

// indentLines prefixes every non-empty line with a tab.
func indentLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = "\t" + l
	}
	return out
}

// blockLines spells head { body }.
func blockLines(head string, body []string) []string {
	out := []string{head + " {"}
	out = append(out, indentLines(body)...)
	return append(out, "}")
}

// ifLines spells if cond { body }.
func ifLines(cond string, body []string) []string {
	return blockLines("if "+cond, body)
}

// ifElseLines spells if cond { yes } else { no }.
func ifElseLines(cond string, yes, no []string) []string {
	out := []string{"if " + cond + " {"}
	out = append(out, indentLines(yes)...)
	out = append(out, "} else {")
	out = append(out, indentLines(no)...)
	return append(out, "}")
}

// valueParts is the size and emit code of one present value: konst fixed
// bytes, size statements for the variable bytes, and the emit statements.
type valueParts struct {
	konst int
	size  []string
	emit  []string
}

// inlineSize returns the size statements with the constant added first.
func (p valueParts) inlineSize() []string {
	if p.konst == 0 {
		return p.size
	}
	return append([]string{fmt.Sprintf("_logos_n += %d", p.konst)}, p.size...)
}

// fieldCode is the code one field contributes to each phase.
type fieldCode struct {
	pins  []string
	pre   []string
	size  []string
	emit  []string
	fixed int
}

// marshalGen builds one MarshalJSON.
type marshalGen struct {
	js         *jsonStruct
	firstFixed int
	runtimeSep bool
	// recv is the value expression fields read from: the method
	// receiver for methods, the helper parameter for helpers.
	recv string
	// edges and calls carry the traversal plan for walk-shaped
	// methods and helpers; nil selects the legacy flat method.
	edges map[*jsonField]jsonEdge
	calls map[*jsonField]string
}

// optional reports whether the field may be left out of the output.
func (f *jsonField) optional() bool {
	return f.omitEmpty || f.omitZero
}

// newMarshalGen analyzes the separator plan of js: the output separator is
// a runtime byte for every field up to the first always-present field when
// the first field may be absent, and a fixed literal prefix otherwise.
func newMarshalGen(js *jsonStruct) *marshalGen {
	g := &marshalGen{js: js, firstFixed: -1, recv: jsonRecv}
	for i, f := range js.fields {
		if !f.optional() {
			g.firstFixed = i
			break
		}
	}
	g.runtimeSep = len(js.fields) == 0 || js.fields[0].optional()
	return g
}

// sepRuntimeAt reports whether field p writes its separator at run time.
func (g *marshalGen) sepRuntimeAt(p int) bool {
	return g.runtimeSep && (g.firstFixed < 0 || p <= g.firstFixed)
}

// litStmts spells the statements writing `sep "key":` and open for f.
func (g *marshalGen) litStmts(f *jsonField, open string) []string {
	key := keyLiteral(f.key) + open
	if g.sepRuntimeAt(f.pos) {
		return []string{"_logos_buf = append(_logos_buf, _logos_sep)", "_logos_buf = append(_logos_buf, `" + key + "`...)", "_logos_sep = ','"}
	}
	sep := ","
	if f.pos == 0 {
		sep = "{"
	}
	return []string{"_logos_buf = append(_logos_buf, `" + sep + key + "`...)"}
}

// litLen is the byte length written by litStmts.
func (g *marshalGen) litLen(f *jsonField, open string) int {
	return 1 + len(keyLiteral(f.key)) + len(open)
}

// scalarParts spells the present value x of codec c.
func (g *marshalGen) scalarParts(f *jsonField, c scalarCodec, x string) valueParts {
	open := c.open()
	p := valueParts{konst: g.litLen(f, open) + len(c.closeText())}
	p.size = []string{c.sizeStmt(x)}
	p.emit = append(g.litStmts(f, open), c.emitStmt(x))
	if c.closeText() != "" {
		p.emit = append(p.emit, `_logos_buf = append(_logos_buf, '"')`)
	}
	return p
}

// nullParts spells the literal null after the key of f.
func (g *marshalGen) nullParts(f *jsonField) valueParts {
	return valueParts{
		konst: g.litLen(f, "") + 4,
		emit:  append(g.litStmts(f, ""), `_logos_buf = append(_logos_buf, "null"...)`),
	}
}

// local returns the pinned local of field f.
func (f *jsonField) local() string {
	return fmt.Sprintf("_logos_f%d", f.pos)
}

// presenceName returns the presence local of field f.
func (f *jsonField) presenceName() string {
	return fmt.Sprintf("_logos_p%d", f.pos)
}

// code builds the code of one field.
func (g *marshalGen) code(f *jsonField) fieldCode {
	if g.edges != nil {
		if e, ok := g.edges[f]; ok && e != edgeLegacy {
			return g.edgeCode(f, e)
		}
	}
	var c fieldCode
	switch f.shape.kind {
	case shapeFallback:
		if f.quoted {
			c = g.fallbackQuotedCode(f)
			// fallbackQuotedCode for plain scalars already applies presence;
			// Number-quoted and method-wins paths return fallback-style code
			// that carries its own optional handling.
			return c
		}
		c = g.fallbackCode(f)
	case shapeScalar:
		c = g.scalarCode(f)
	case shapePointer:
		c = g.pointerCode(f)
	case shapeUnion:
		c = g.unionCode(f)
	default:
		c = g.sliceCode(f)
	}
	if f.shape.kind != shapeFallback && f.shape.kind != shapeUnion && f.optional() {
		c = g.guardPresence(f, c)
	}
	return c
}

// guardPresence wraps the phases of an optional modeled field in its
// presence local and declares that local first.
func (g *marshalGen) guardPresence(f *jsonField, c fieldCode) fieldCode {
	p := f.presenceName()
	out := fieldCode{pins: c.pins}
	out.pre = []string{p + " := " + g.presence(f)}
	if len(c.pre) > 0 {
		out.pre = append(out.pre, ifLines(p, c.pre)...)
	}
	out.size = ifLines(p, append(constLine(c.fixed), c.size...))
	out.emit = ifLines(p, c.emit)
	return out
}

// constLine returns the statement adding n bytes, or nothing for zero.
func constLine(n int) []string {
	if n == 0 {
		return nil
	}
	return []string{fmt.Sprintf("_logos_n += %d", n)}
}

// presence spells the condition that optional field f is written.
// A scalar with omitzero and a signature-checked IsZero consults the
// runtime OmitZero helper (which honors value- and pointer-receiver
// IsZero like encoding/json, including addressable copies) instead of
// the naive zero test; with omitempty also set the naive empty test
// still applies, so the field is written only when neither test omits
// it. Pointers consult OmitZero the same way once a pointee IsZero can
// fire; otherwise nil alone decides, matching encoding/json.
func (g *marshalGen) presence(f *jsonField) string {
	x := f.local()
	switch f.shape.kind {
	case shapeScalar:
		if f.omitZero && f.shape.needsIsZero {
			if f.omitEmpty {
				return "!(" + f.codec().isZeroCond(x) + ") && !" + jsonAlias + ".OmitZero(" + x + ")"
			}
			return "!" + jsonAlias + ".OmitZero(" + x + ")"
		}
		return "!(" + f.codec().isZeroCond(x) + ")"
	case shapePointer:
		if f.omitZero && f.shape.needsIsZero {
			return x + " != nil && !" + jsonAlias + ".OmitZero(" + x + ")"
		}
		return x + " != nil"
	case shapeUnion:
		if f.omitZero {
			return x + " != nil && !" + jsonAlias + ".OmitZero(" + x + ")"
		}
		return x + " != nil"
	}
	var conds []string
	if f.omitEmpty {
		conds = append(conds, "len("+x+") != 0")
	}
	if f.omitZero {
		conds = append(conds, x+" != nil")
	}
	return strings.Join(conds, " && ")
}

// scalarCode spells a scalar field.
func (g *marshalGen) scalarCode(f *jsonField) fieldCode {
	x := f.local()
	c := f.codec()
	p := g.scalarParts(f, c, x)
	out := fieldCode{pins: []string{x + " := " + g.recv + "." + f.goName}}
	if chk := c.checkStmt(x); chk != "" {
		out.pre = []string{chk}
	}
	out.size = p.size
	out.emit = p.emit
	out.fixed = p.konst
	return out
}

// pointerCode spells a field holding a pointer to a scalar.
func (g *marshalGen) pointerCode(f *jsonField) fieldCode {
	x := f.local()
	c := f.codec()
	p := g.scalarParts(f, c, "*"+x)
	out := fieldCode{pins: []string{x + " := " + g.recv + "." + f.goName}}
	if chk := c.checkStmt("*" + x); chk != "" {
		out.pre = ifLines(x+" != nil", []string{chk})
	}
	if f.optional() {
		out.size = p.inlineSize()
		out.emit = p.emit
		return out
	}
	n := g.nullParts(f)
	out.size = ifElseLines(x+" != nil", p.inlineSize(), n.inlineSize())
	out.emit = ifElseLines(x+" != nil", p.emit, n.emit)
	return out
}

// sliceCode spells a field holding a slice of scalars.
func (g *marshalGen) sliceCode(f *jsonField) fieldCode {
	x := f.local()
	c := f.elemCodec()
	out := fieldCode{pins: []string{x + " := " + g.recv + "." + f.goName}}
	if chk := c.checkStmt("_logos_x"); chk != "" {
		loop := append([]string{"for _, _logos_x := range " + x + " {"}, indentLines([]string{chk})...)
		out.pre = append(loop, "}")
	}
	n := g.nullParts(f)
	out.size = ifElseLines(x+" == nil", n.inlineSize(), g.sliceSize(f, c, x))
	out.emit = ifElseLines(x+" == nil", n.emit, g.sliceEmit(f, c, x))
	return out
}

// sliceSize spells the size statements of a non-nil slice x: the bracket
// bytes, then the elements. Float elements have a fixed bound, so they
// take no loop; every other element adds its own length and a comma.
func (g *marshalGen) sliceSize(f *jsonField, c scalarCodec, x string) []string {
	lines := []string{fmt.Sprintf("_logos_n += %d", g.litLen(f, "[")+1)}
	if c.isFloat() {
		return append(lines, fmt.Sprintf("_logos_n += len(%s) * (%s.MaxFloatLen%d + 1)", x, jsonAlias, c.floatBits()))
	}
	body := append([]string{"if _logos_i > 0 { _logos_n++ }"}, elemSize(c, "_logos_x")...)
	lines = append(lines, "for _logos_i, _logos_x := range "+x+" {")
	lines = append(lines, indentLines(body)...)
	return append(lines, "}")
}

// sliceEmit spells the emit statements of a non-nil slice x.
func (g *marshalGen) sliceEmit(f *jsonField, c scalarCodec, x string) []string {
	lines := append(g.litStmts(f, "["), "for _logos_i, _logos_x := range "+x+" {")
	body := append([]string{"if _logos_i > 0 { _logos_buf = append(_logos_buf, ',') }"}, elemEmit(c, "_logos_x")...)
	lines = append(lines, indentLines(body)...)
	return append(lines, "}", "_logos_buf = append(_logos_buf, ']')")
}

// elemSize spells the size statements of one slice element x.
func elemSize(c scalarCodec, x string) []string {
	lines := []string{c.sizeStmt(x)}
	if n := len(c.open()) + len(c.closeText()); n > 0 {
		lines = append(lines, fmt.Sprintf("_logos_n += %d", n))
	}
	return lines
}

// elemEmit spells the emit statements of one slice element x.
func elemEmit(c scalarCodec, x string) []string {
	var lines []string
	if c.open() != "" {
		lines = append(lines, `_logos_buf = append(_logos_buf, '"')`)
	}
	lines = append(lines, c.emitStmt(x))
	if c.closeText() != "" {
		lines = append(lines, `_logos_buf = append(_logos_buf, '"')`)
	}
	return lines
}

// fallbackCode spells a field the static path does not model: the
// reflective encoding/json call of that field only.
func (g *marshalGen) fallbackCode(f *jsonField) fieldCode {
	field := g.recv + "." + f.goName
	b := fmt.Sprintf("_logos_b%d", f.pos)
	e := fmt.Sprintf("_logos_e%d", f.pos)
	call := []string{
		fmt.Sprintf("%s, %s := %s.MarshalField(%s)", b, e, jsonAlias, field),
	}
	check := ifLines(e+" != nil", []string{"return nil, " + e})
	var c fieldCode
	if !f.optional() {
		c.pre = append(call, check...)
		c.size = []string{fmt.Sprintf("_logos_n += %d + len(%s)", g.litLen(f, ""), b)}
		c.emit = append(g.litStmts(f, ""), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b))
		return c
	}
	p := f.presenceName()
	c.pre = []string{p + " := " + omitCall(f, field), "var " + b + " []byte"}
	inner := []string{fmt.Sprintf("var %s error", e), fmt.Sprintf("%s, %s = %s.MarshalField(%s)", b, e, jsonAlias, field)}
	c.pre = append(c.pre, ifLines(p, append(inner, check...))...)
	c.size = ifLines(p, []string{fmt.Sprintf("_logos_n += %d + len(%s)", g.litLen(f, ""), b)})
	c.emit = ifLines(p, append(g.litStmts(f, ""), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b)))
	return c
}

// fallbackQuotedCode spells a fallback field carrying the `string` option.
// Method types win over the option and ignore it, exactly like the
// reflective encoders, so a type with marshal-side methods keeps the
// unquoted fallback. Exact json.Number is the one exception: it quotes the
// number literal before the method check, matching the reflective walker.
// Every other fallback scalar without marshal methods encodes as a quoted
// scalar via its underlying kind.
func (g *marshalGen) fallbackQuotedCode(f *jsonField) fieldCode {
	if isExactNumber(f.typ) {
		return g.fallbackNumberQuotedCode(f)
	}
	if hasMarshalMethods(f.typ) {
		return g.fallbackCode(f)
	}
	if kind, ok := fallbackScalarKind(f.typ); ok {
		return g.fallbackScalarQuotedCode(f, kind)
	}
	return g.fallbackCode(f)
}

// fallbackScalarQuotedCode spells a fallback scalar without marshal methods
// as a quoted scalar of its underlying kind. Presence (omitempty/omitzero,
// including IsZero) follows the scalar path via guardPresence.
func (g *marshalGen) fallbackScalarQuotedCode(f *jsonField, kind scalarKind) fieldCode {
	u := types2.Unalias(f.typ)
	if cv, ok := u.(*types2.ConstView); ok {
		u = types2.Unalias(cv.Elem)
	}
	_, isPtr := u.(*types2.Pointer)
	var elem types2.Type
	if isPtr {
		p := u.(*types2.Pointer)
		elem = types2.Unalias(p.Elem())
		if cv, ok := elem.(*types2.ConstView); ok {
			elem = types2.Unalias(cv.Elem)
		}
	} else {
		elem = u
		if _, ok := elem.(*types2.Option); ok {
			return g.fallbackCode(f)
		}
	}
	tmp := &jsonField{
		goName:    f.goName,
		key:       f.key,
		omitEmpty: f.omitEmpty,
		omitZero:  f.omitZero,
		quoted:    true,
		pos:       f.pos,
		required:  f.required,
		typ:       f.typ,
	}
	if isPtr {
		tmp.shape = jsonShape{kind: shapePointer, scalar: kind, nonNil: f.shape.nonNil, needsIsZero: needsIsZero(elem)}
		c := g.pointerCode(tmp)
		if tmp.optional() {
			c = g.guardPresence(tmp, c)
		}
		return c
	}
	tmp.shape = jsonShape{kind: shapeScalar, scalar: kind, nonNil: false, needsIsZero: needsIsZero(elem)}
	c := g.scalarCode(tmp)
	if tmp.optional() {
		c = g.guardPresence(tmp, c)
	}
	return c
}

// fallbackNumberQuotedCode spells exact json.Number with `,string`: the
// number literal from MarshalField wrapped in quotes, matching
// appendQuotedNumber in the reflective walker. A nil pointer encodes null
// unquoted; optional fields keep the fallback presence test.
func (g *marshalGen) fallbackNumberQuotedCode(f *jsonField) fieldCode {
	field := g.recv + "." + f.goName
	b := fmt.Sprintf("_logos_b%d", f.pos)
	e := fmt.Sprintf("_logos_e%d", f.pos)
	u := types2.Unalias(f.typ)
	if cv, ok := u.(*types2.ConstView); ok {
		u = types2.Unalias(cv.Elem)
	}
	_, isPtr := u.(*types2.Pointer)
	call := []string{
		fmt.Sprintf("%s, %s := %s.MarshalField(%s)", b, e, jsonAlias, field),
	}
	check := ifLines(e+" != nil", []string{"return nil, " + e})
	quotedSize := fmt.Sprintf("_logos_n += %d + len(%s) + 1", g.litLen(f, `"`), b)
	quotedEmit := append(g.litStmts(f, `"`), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b), `_logos_buf = append(_logos_buf, '"')`)
	nullSize := fmt.Sprintf("_logos_n += %d + 4", g.litLen(f, ""))
	nullEmit := append(g.litStmts(f, ""), `_logos_buf = append(_logos_buf, "null"...)`)
	var c fieldCode
	if !f.optional() {
		if !isPtr {
			c.pre = append(call, check...)
			c.size = []string{quotedSize}
			c.emit = quotedEmit
			return c
		}
		c.pre = append(call, check...)
		c.size = ifElseLines(field+" == nil", []string{nullSize}, []string{quotedSize})
		c.emit = ifElseLines(field+" == nil", nullEmit, quotedEmit)
		return c
	}
	p := f.presenceName()
	c.pre = []string{p + " := " + omitCall(f, field), "var " + b + " []byte"}
	inner := []string{fmt.Sprintf("var %s error", e), fmt.Sprintf("%s, %s = %s.MarshalField(%s)", b, e, jsonAlias, field)}
	c.pre = append(c.pre, ifLines(p, append(inner, check...))...)
	if !isPtr {
		c.size = ifLines(p, []string{quotedSize})
		c.emit = ifLines(p, quotedEmit)
		return c
	}
	c.size = ifLines(p, ifElseLines(field+" == nil", []string{nullSize}, []string{quotedSize}))
	c.emit = ifLines(p, ifElseLines(field+" == nil", nullEmit, quotedEmit))
	return c
}

// omitCall spells the reflective presence test of an unmodeled optional
// field: written unless it is empty or zero by the options it carries.
func omitCall(f *jsonField, field string) string {
	var tests []string
	if f.omitEmpty {
		tests = append(tests, jsonAlias+".OmitEmpty("+field+")")
	}
	if f.omitZero {
		tests = append(tests, jsonAlias+".OmitZero("+field+")")
	}
	return "!(" + strings.Join(tests, " || ") + ")"
}

// flattenUnionTerms returns the leaf member types of t in declaration order.
func flattenUnionTerms(t types2.Type) []types2.Type {
	terms, ok := unionTerms(t)
	if !ok {
		return nil
	}
	var out []types2.Type
	for _, term := range terms {
		if sub, ok := unionTerms(term); ok && len(sub) > 0 {
			out = append(out, flattenUnionTerms(term)...)
		} else {
			out = append(out, term)
		}
	}
	return out
}

// unionPackage returns the package declaring the union named type, or
// of its first named member if the union itself is anonymous.
func unionPackage(t types2.Type) *types2.Package {
	switch u := types2.Unalias(t).(type) {
	case *types2.Named:
		if obj := u.Obj(); obj != nil {
			return obj.Pkg()
		}
	}
	if terms, ok := unionTerms(t); ok {
		for _, m := range terms {
			switch mu := types2.Unalias(m).(type) {
			case *types2.Named:
				if obj := mu.Obj(); obj != nil {
					return obj.Pkg()
				}
			case *types2.Pointer:
				if named, ok := types2.Unalias(mu.Elem()).(*types2.Named); ok && named.Obj() != nil {
					return named.Obj().Pkg()
				}
			}
		}
	}
	return nil
}

// unionMemberSpelling returns the source spelling of union member t.
// Types declared in pkg are spelled without package qualification.
func unionMemberSpelling(t types2.Type, pkg *types2.Package) string {
	return types2.TypeString(t, func(p *types2.Package) string {
		if pkg != nil && p == pkg {
			return ""
		}
		if p == nil {
			return ""
		}
		return p.Name()
	})
}

// isMarshalJSONSignature reports whether obj is a method with signature
// func() ([]byte, error).
func isMarshalJSONSignature(obj types2.Object) bool {
	fn, ok := obj.(*types2.Func)
	if !ok {
		return false
	}
	sig, ok := fn.Type().(*types2.Signature)
	if !ok {
		return false
	}
	if sig.Params().Len() != 0 || sig.Results().Len() != 2 {
		return false
	}
	res0 := sig.Results().At(0).Type()
	res1 := sig.Results().At(1).Type()
	sl, ok := types2.Unalias(res0).(*types2.Slice)
	if !ok {
		return false
	}
	b, ok := types2.Unalias(sl.Elem()).(*types2.Basic)
	if !ok || b.Kind() != types2.Uint8 {
		return false
	}
	named, ok := types2.Unalias(res1).(*types2.Named)
	if ok && named.Obj() != nil && named.Obj().Name() == "error" && named.Obj().Pkg() == nil {
		return true
	}
	return res1.String() == "error"
}

// unionScalarKind returns the scalar class of t when t is a plain or named
// scalar without overriding JSON methods.
func unionScalarKind(t types2.Type) (scalarKind, bool) {
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

// scalarStaticMarshal returns the inline statement emitting value val of
// class kind into buffer b.
func scalarStaticMarshal(kind scalarKind, val, b string) string {
	switch kind {
	case scalarBool:
		return fmt.Sprintf("if %s { %s = []byte(\"true\") } else { %s = []byte(\"false\") }", val, b, b)
	case scalarInt:
		return fmt.Sprintf("%s = %s.AppendInt(nil, int64(%s))", b, jsonAlias, val)
	case scalarUint:
		return fmt.Sprintf("%s = %s.AppendUint(nil, uint64(%s))", b, jsonAlias, val)
	case scalarFloat32:
		return fmt.Sprintf("if %s-%s != 0 { return nil, %s.FloatErr(%s, float64(%s), 32) }; %s = %s.AppendFloat(nil, float64(%s), 32)", val, val, jsonAlias, val, val, b, jsonAlias, val)
	case scalarFloat64:
		return fmt.Sprintf("if %s-%s != 0 { return nil, %s.FloatErr(%s, float64(%s), 64) }; %s = %s.AppendFloat(nil, float64(%s), 64)", val, val, jsonAlias, val, val, b, jsonAlias, val)
	case scalarString:
		return fmt.Sprintf("%s = append(%s.AppendEscaped(append([]byte(nil), '\"'), string(%s)), '\"')", b, jsonAlias, val)
	}
	return ""
}

// unionMemberStaticCall reports the statement that marshals _logos_m of
// member type m, writing into byte buffer b and error e.
func unionMemberStaticCall(m types2.Type, pkg *types2.Package, b, e string) string {
	if ptr, ok := types2.Unalias(m).(*types2.Pointer); ok {
		obj, _, indirect := types2.LookupFieldOrMethod(ptr, false, pkg, "MarshalJSON")
		if obj != nil && !indirect && isMarshalJSONSignature(obj) {
			return fmt.Sprintf("\tif _logos_m != nil { %s, %s = _logos_m.MarshalJSON() } else { %s = []byte(\"null\") }", b, e, b)
		}
		elemObj, _, elemInd := types2.LookupFieldOrMethod(ptr.Elem(), false, pkg, "MarshalJSON")
		if elemObj != nil && !elemInd && isMarshalJSONSignature(elemObj) {
			return fmt.Sprintf("\tif _logos_m != nil { %s, %s = _logos_m.MarshalJSON() } else { %s = []byte(\"null\") }", b, e, b)
		}
		if sk, ok := unionScalarKind(ptr.Elem()); ok {
			return fmt.Sprintf("\tif _logos_m == nil { %s = []byte(\"null\") } else { %s }", b, scalarStaticMarshal(sk, "*_logos_m", b))
		}
		return fmt.Sprintf("\t%s, %s = %s.MarshalField(_logos_m)", b, e, jsonAlias)
	}

	obj, _, indirect := types2.LookupFieldOrMethod(m, false, pkg, "MarshalJSON")
	if obj != nil && !indirect && isMarshalJSONSignature(obj) {
		return fmt.Sprintf("\t%s, %s = _logos_m.MarshalJSON()", b, e)
	}
	if obj != nil && indirect && isMarshalJSONSignature(obj) {
		return fmt.Sprintf("\t_logos_tmp := _logos_m; %s, %s = _logos_tmp.MarshalJSON()", b, e)
	}
	if sk, ok := unionScalarKind(m); ok {
		return "\t" + scalarStaticMarshal(sk, "_logos_m", b)
	}
	return fmt.Sprintf("\t%s, %s = %s.MarshalField(_logos_m)", b, e, jsonAlias)
}

// unionCode spells a field holding a closed-union value. SPEC Sec 12: a
// closed-union value marshals to its dynamic member's static bytes.
func (g *marshalGen) unionCode(f *jsonField) fieldCode {
	x := f.local()
	b := fmt.Sprintf("_logos_b%d", f.pos)
	e := fmt.Sprintf("_logos_e%d", f.pos)
	pins := []string{x + " := " + g.recv + "." + f.goName}

	terms := flattenUnionTerms(f.typ)
	pkg := unionPackage(f.typ)

	sw := []string{"switch _logos_m := " + x + ".(type) {"}
	for _, m := range terms {
		mname := unionMemberSpelling(m, pkg)
		sw = append(sw, "case "+mname+":", unionMemberStaticCall(m, pkg, b, e))
	}
	sw = append(sw, "case nil:")
	sw = append(sw, fmt.Sprintf("\t%s = []byte(\"null\")", b))
	sw = append(sw, "default:")
	sw = append(sw, fmt.Sprintf("\t%s, %s = %s.MarshalField(_logos_m)", b, e, jsonAlias))
	sw = append(sw, "}")

	check := ifLines(e+" != nil", []string{"return nil, " + e})

	var c fieldCode
	c.pins = pins
	if !f.optional() {
		c.pre = append([]string{"var " + b + " []byte", "var " + e + " error"}, sw...)
		c.pre = append(c.pre, check...)
		c.size = []string{fmt.Sprintf("_logos_n += %d + len(%s)", g.litLen(f, ""), b)}
		c.emit = append(g.litStmts(f, ""), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b))
		return c
	}

	p := f.presenceName()
	c.pre = []string{p + " := " + g.presence(f), "var " + b + " []byte"}
	inner := append([]string{fmt.Sprintf("var %s error", e)}, sw...)
	inner = append(inner, check...)
	c.pre = append(c.pre, ifLines(p, inner)...)
	c.size = ifLines(p, []string{fmt.Sprintf("_logos_n += %d + len(%s)", g.litLen(f, ""), b)})
	c.emit = ifLines(p, append(g.litStmts(f, ""), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b)))
	return c
}

// This section carries the per-root traversal plan (M11 R1/R4). Static
// edges to same-package generated-side named types encode through
// per-root helpers that share one stack-owned Walk; every other
// stateful edge funnels through Walk.Marshal. The Gen table lists the
// exact own-package generated types the root may meet dynamically,
// discovered from compile-time reachable named types. Interface-typed
// fields contribute no entries: a user method met only dynamically
// must honour through MarshalField, never walk. Imported types never
// enter the table; foreign methods stay honour.

// jsonEdge is how one field value encodes in a walk-shaped method.
type jsonEdge int

const (
	edgeLegacy    jsonEdge = iota // current scalar or leaf code, no Walk
	edgeHelper                    // value of generated-side named type
	edgePtrHelper                 // pointer to generated-side named type
	edgeWalk                      // Walk.Marshal with shared state
)

// jsonHelperDef is one traversal-helper pair for a spelled
// generated-side type: a value helper and a nil-tracking pointer
// helper sharing the caller's Walk.
type jsonHelperDef struct {
	key   string
	spell string
	vname string
	pname string
	js    *jsonStruct
	edges map[*jsonField]jsonEdge
	calls map[*jsonField]string
	made  bool
}

// jsonWalkPlan is one root's traversal plan: per-field edges and
// callees, shared helper pairs, and the Gen table prelude.
type jsonWalkPlan struct {
	stateful    bool
	edges       map[*jsonField]jsonEdge
	calls       map[*jsonField]string
	order       []string
	table       []string
	tableSeen   map[string]bool
	descendSeen map[*types2.Named]bool
}

// jsonEdgeOf classifies how the value of a field of type t encodes.
// Scalar and leaf shapes keep today's code; generated-side named types
// take helpers; every other composite funnels through the walk.
func (fl *fileLower) jsonEdgeOf(t types2.Type) jsonEdge {
	if fl.classifyJSONField(t).kind != shapeFallback {
		return edgeLegacy
	}
	core := jsonEdgeCore(fl, t)
	if p, ok := core.(*types2.Pointer); ok {
		if key, _, _ := fl.helperModelFor(p.Elem()); key != "" {
			return edgePtrHelper
		}
		if _, ok := types2.Unalias(p.Elem()).(*types2.Pointer); ok {
			return edgeWalk
		}
		if _, ok := scalarCore(p.Elem()); ok {
			return edgeLegacy
		}
		return edgeWalk
	}
	if key, _, _ := fl.helperModelFor(core); key != "" {
		return edgeHelper
	}
	switch types2.Unalias(core).Underlying().(type) {
	case *types2.Slice, *types2.Array, *types2.Map, *types2.Struct, *types2.Interface:
		return edgeWalk
	}
	return edgeLegacy
}

// jsonEdgeCore unwraps options and const views for edge analysis,
// mirroring classifyJSONField.
func jsonEdgeCore(fl *fileLower, t types2.Type) types2.Type {
	u := types2.Unalias(t)
	if o := fl.asOption(u); o != nil {
		u = o.Elem
	}
	if cv, ok := u.(*types2.ConstView); ok {
		u = cv.Elem
	}
	return u
}

// scalarCore reports whether t is a modeled scalar kind.
func scalarCore(t types2.Type) (scalarKind, bool) {
	if b, ok := types2.Unalias(t).(*types2.Basic); ok {
		return basicScalar(b)
	}
	return 0, false
}

// planJSONWalk builds the traversal plan for js rooted at rootNamed.
// Roots without a stateful edge keep today's flat method.
func (fl *fileLower) planJSONWalk(js *jsonStruct, rootNamed *types2.Named) *jsonWalkPlan {
	plan := &jsonWalkPlan{
		edges:       make(map[*jsonField]jsonEdge),
		calls:       make(map[*jsonField]string),
		tableSeen:   make(map[string]bool),
		descendSeen: make(map[*types2.Named]bool),
	}
	for _, f := range js.fields {
		e := fl.jsonEdgeOf(f.typ)
		if e == edgeLegacy {
			continue
		}
		plan.stateful = true
		plan.edges[f] = e
		plan.calls[f] = fl.planEdgeCall(plan, f, e)
	}
	if !plan.stateful {
		return plan
	}
	fl.visitJSONTable(plan, rootNamed)
	for _, f := range js.fields {
		fl.descendJSONTable(plan, f.typ)
	}
	return plan
}

// planEdgeCall returns the callee for a stateful field edge: a helper
// name, or "" for the shared walk. Registration happens once per type;
// repeated fields reuse the pair.
func (fl *fileLower) planEdgeCall(plan *jsonWalkPlan, f *jsonField, e jsonEdge) string {
	switch e {
	case edgeHelper, edgePtrHelper:
		key := fl.ensureJSONHelper(plan, f.typ)
		if key == "" {
			return ""
		}
		def := fl.pkg.jsonHelpers[key]
		if def.vname == "" {
			return ""
		}
		if e == edgePtrHelper {
			return def.pname
		}
		return def.vname
	}
	return ""
}

// ensureJSONHelper registers the helper pair for t, returning its
// registry key or "" when t is not helper-eligible. Models are built
// before nested registration so recursive types hit the registry.
func (fl *fileLower) ensureJSONHelper(plan *jsonWalkPlan, t types2.Type) string {
	key, spell, js := fl.helperModelFor(t)
	if key == "" {
		return ""
	}
	if fl.pkg.jsonHelpers == nil {
		fl.pkg.jsonHelpers = make(map[string]*jsonHelperDef)
	}
	if _, ok := fl.pkg.jsonHelpers[key]; ok {
		fl.visitJSONTable(plan, t)
		return key
	}
	def := &jsonHelperDef{key: key, spell: spell, js: js}
	fl.pkg.jsonHelpers[key] = def
	plan.order = append(plan.order, key)
	fl.pkg.jsonHelperSeq++
	def.vname = fmt.Sprintf("_logos_h%d", fl.pkg.jsonHelperSeq)
	fl.pkg.jsonHelperSeq++
	def.pname = fmt.Sprintf("_logos_h%d", fl.pkg.jsonHelperSeq)
	def.edges = make(map[*jsonField]jsonEdge)
	def.calls = make(map[*jsonField]string)
	for _, f := range js.fields {
		e := fl.jsonEdgeOf(f.typ)
		if e == edgeLegacy {
			continue
		}
		def.edges[f] = e
		def.calls[f] = fl.planEdgeCall(plan, f, e)
	}
	fl.visitJSONTable(plan, t)
	return key
}

// visitJSONTable records the Gen entry for a helper-eligible type and
// descends into its reachable generated types. Every helper-eligible
// type spells as TypeFor in the root's own package; types without a
// spelling never reach helpers, so the table holds exact types only.
func (fl *fileLower) visitJSONTable(plan *jsonWalkPlan, t types2.Type) {
	key, spell, js := fl.helperModelFor(t)
	if key == "" || spell == "" || plan.tableSeen[key] {
		return
	}
	plan.tableSeen[key] = true
	plan.table = append(plan.table, reflectAlias+".TypeFor["+spell+"]()")
	for _, f := range js.fields {
		fl.descendJSONTable(plan, f.typ)
	}
}

// descendJSONTable records Gen entries for generated types reachable
// through composite edges that the walk funnels dynamically: slices,
// arrays, maps, pointers, and plain structs. Interface interiors are
// unknown statically and contribute nothing, which keeps user methods
// met only dynamically on the honour branch.
func (fl *fileLower) descendJSONTable(plan *jsonWalkPlan, t types2.Type) {
	core := jsonEdgeCore(fl, t)
	if key, _, _ := fl.helperModelFor(core); key != "" {
		fl.visitJSONTable(plan, core)
		return
	}
	switch u := types2.Unalias(core).(type) {
	case *types2.Pointer:
		fl.descendJSONTable(plan, u.Elem())
	case *types2.Slice:
		fl.descendJSONTable(plan, u.Elem())
	case *types2.Array:
		fl.descendJSONTable(plan, u.Elem())
	case *types2.Map:
		fl.descendJSONTable(plan, u.Elem())
	case *types2.Named:
		fl.addPromotedJSONDispatchType(plan, u)
		fl.descendNamedJSONTable(plan, u)
	case *types2.Struct:
		fl.descendStructJSONTable(plan, u)
	}
}

// addPromotedJSONDispatchType records a local named struct whose promoted
// MarshalJSON method was synthesized on an embedded generated type. Such a
// type must enter Walk.Gen: otherwise the runtime honors the promoted method,
// bypasses the enclosing struct's promoted fields, and wraps cycle errors as
// a MarshalJSON method error instead of returning the walk's reflective
// cycle error directly.
func (fl *fileLower) addPromotedJSONDispatchType(plan *jsonWalkPlan, named *types2.Named) {
	if named == nil || named.Obj() == nil || named.Obj().Pkg() != fl.pkg.tpkg {
		return
	}
	method, index, _ := types2.LookupFieldOrMethod(named, true, fl.pkg.tpkg, "MarshalJSON")
	if len(index) <= 1 {
		return
	}
	fn, ok := method.(*types2.Func)
	if !ok || fn.Pkg() != fl.pkg.tpkg || fn.Pos().IsKnown() {
		return
	}
	recv := fn.Signature().Recv().Type()
	if p, ok := recv.(*types2.Pointer); ok {
		recv = p.Elem()
	}
	promoted, ok := types2.Unalias(recv).(*types2.Named)
	if !ok || promoted.Obj() == nil || promoted.Obj().Pkg() != fl.pkg.tpkg || promoted == named || fl.helperEligibleType(promoted) == nil {
		return
	}
	spell, ok := fl.jsonTypeName(named)
	if !ok || named.TypeArgs().Len() != 0 || named.TypeParams().Len() != 0 {
		return
	}
	key := named.Obj().Pkg().Path() + "." + named.Obj().Name()
	if !plan.tableSeen[key] {
		plan.tableSeen[key] = true
		plan.table = append(plan.table, reflectAlias+".TypeFor["+spell+"]()")
	}
}

// descendNamedJSONTable descends into a plain named struct's fields for
// table discovery. Generated-side named types divert to visitJSONTable
// before reaching here. The per-plan guard terminates recursive types.
func (fl *fileLower) descendNamedJSONTable(plan *jsonWalkPlan, named *types2.Named) {
	if plan.descendSeen[named] {
		return
	}
	plan.descendSeen[named] = true
	st, ok := named.Underlying().(*types2.Struct)
	if !ok {
		return
	}
	fl.descendStructJSONTable(plan, st)
}

// descendStructJSONTable descends into a struct's exported fields for
// table discovery, including exported embedded fields. Unexported
// fields are skipped; the walk resolves them dynamically. Promotion
// itself needs no table model: the walk inlines embedded fields exactly
// like the reflective encoder, and the table only dispatches bare
// dynamic values.
func (fl *fileLower) descendStructJSONTable(plan *jsonWalkPlan, st *types2.Struct) {
	for i := range st.NumFields() {
		sf := st.Field(i)
		if !sf.Exported() {
			continue
		}
		fl.descendJSONTable(plan, sf.Type())
	}
}

// helperModelFor returns the registry key, source spelling, and field
// model for a helper-eligible type, or a "" key when the walk must
// funnel the edge dynamically instead. Pointers share their element's
// model; the Gen table adds the counterpart. Named generated-side
// structs qualify directly; spellable method-free generic
// instantiations qualify through substitution; everything else funnels
// through Walk.Marshal, which honours methods at run time.
func (fl *fileLower) helperModelFor(t types2.Type) (string, string, *jsonStruct) {
	core := jsonEdgeCore(fl, t)
	if p, ok := core.(*types2.Pointer); ok {
		core = types2.Unalias(p.Elem())
	}
	if named := fl.helperEligibleType(core); named != nil {
		key := named.Obj().Pkg().Path() + "." + named.Obj().Name()
		spell, ok := fl.jsonTypeName(named)
		if !ok {
			spell = ""
		}
		return key, spell, fl.modelNamedStruct(named)
	}
	return fl.instHelperModel(core)
}

// modelNamedStruct builds the field model of a generated-side named
// struct from its declared fields.
func (fl *fileLower) modelNamedStruct(named *types2.Named) *jsonStruct {
	st := named.Underlying().(*types2.Struct)
	vars := make([]*types2.Var, st.NumFields())
	for i := range vars {
		vars[i] = st.Field(i)
	}
	return fl.buildJSONStruct(named.Obj().Name(), st, vars)
}

// jsonInstType is a spellable method-free generic instantiation.
type jsonInstType struct {
	origin *types2.Named
	args   []types2.Type
	spell  string
}

// instHelperModel returns the registry key, spelling, and substituted
// model for a spellable method-free generic instantiation, or a "" key.
// The origin must be an embedding-free struct, carry no JSON-relevant
// method, and take only nameable arguments; substitution must resolve
// every visible field. Anything else funnels through the walk.
func (fl *fileLower) instHelperModel(core types2.Type) (string, string, *jsonStruct) {
	inst, ok := fl.spellableInstType(core)
	if !ok {
		return "", "", nil
	}
	st, ok := inst.origin.Underlying().(*types2.Struct)
	if !ok {
		return "", "", nil
	}
	for i := range st.NumFields() {
		if st.Field(i).Embedded() {
			return "", "", nil
		}
	}
	if hasOriginJSONMethods(inst.origin) {
		return "", "", nil
	}
	params := inst.origin.TypeParams()
	js := &jsonStruct{name: inst.spell}
	for i := range st.NumFields() {
		sf := st.Field(i)
		// Unresolvable substitutions keep the origin type and funnel
		// through the walk: leaked parameters classify as dynamic
		// edges, never as helpers, so sharing is preserved.
		ft, ok := substJSONType(sf.Type(), params, inst.args)
		if !ok {
			ft = sf.Type()
		}
		info, ok := types2.LogosJSONFieldOf(st, i)
		if !ok {
			continue
		}
		f := &jsonField{
			goName:    sf.Name(),
			key:       info.Name,
			omitEmpty: info.OmitEmpty,
			omitZero:  info.OmitZero,
			pos:       len(js.fields),
			required:  -1,
			typ:       ft,
		}
		f.shape = fl.classifyJSONField(ft)
		f.quoted = info.StringOpt && isQuotableForString(ft, f.shape)
		if f.shape.nonNil {
			f.required = len(js.required)
			js.required = append(js.required, f.goName)
		}
		js.fields = append(js.fields, f)
	}
	return "inst:" + inst.spell, inst.spell, js
}

// hasOriginJSONMethods reports whether the origin declares a
// JSON-relevant method. Any hit disqualifies static helpers: the walk
// honours methods at run time instead of bypassing them.
func hasOriginJSONMethods(origin *types2.Named) bool {
	for i := range origin.NumMethods() {
		m := origin.Method(i)
		for _, name := range jsonMethodNames {
			if m.Name() == name {
				return true
			}
		}
	}
	return false
}

// spellableInstType resolves core to a generic instantiation whose
// origin and arguments can be spelled in generated signatures.
func (fl *fileLower) spellableInstType(core types2.Type) (jsonInstType, bool) {
	named, ok := core.(*types2.Named)
	if !ok || named.TypeArgs().Len() == 0 {
		return jsonInstType{}, false
	}
	origin := named.Origin()
	ospell, ok := fl.jsonTypeName(origin)
	if !ok {
		return jsonInstType{}, false
	}
	targs := named.TypeArgs()
	args := make([]types2.Type, targs.Len())
	parts := make([]string, targs.Len())
	for i := range args {
		args[i] = targs.At(i)
		s, ok := fl.spellJSONType(args[i])
		if !ok {
			return jsonInstType{}, false
		}
		parts[i] = s
	}
	spell := ospell + "[" + strings.Join(parts, ", ") + "]"
	return jsonInstType{origin: origin, args: args, spell: spell}, true
}

// spellJSONType returns the source spelling of a helper-signature type:
// named types through the import registry, basics by keyword, and
// composites thereof. Anything else is unspellable and funnels through
// the walk.
func (fl *fileLower) spellJSONType(t types2.Type) (string, bool) {
	switch u := types2.Unalias(t).(type) {
	case *types2.Basic:
		return u.Name(), true
	case *types2.Named:
		if u.TypeArgs().Len() != 0 {
			inst, ok := fl.spellableInstType(u)
			if !ok {
				return "", false
			}
			return inst.spell, true
		}
		return fl.jsonTypeName(u)
	case *types2.Pointer:
		s, ok := fl.spellJSONType(u.Elem())
		if !ok {
			return "", false
		}
		return "*" + s, true
	case *types2.Slice:
		s, ok := fl.spellJSONType(u.Elem())
		if !ok {
			return "", false
		}
		return "[]" + s, true
	case *types2.Array:
		s, ok := fl.spellJSONType(u.Elem())
		if !ok {
			return "", false
		}
		return fmt.Sprintf("[%d]%s", u.Len(), s), true
	case *types2.Map:
		k, ok := fl.spellJSONType(u.Key())
		if !ok {
			return "", false
		}
		v, ok := fl.spellJSONType(u.Elem())
		if !ok {
			return "", false
		}
		return "map[" + k + "]" + v, true
	}
	return "", false
}

// substJSONType replaces origin type parameters with arguments. Only
// shapes that survive substitution spelling are resolved; anything
// else reports false and the edge funnels through the walk.
func substJSONType(t types2.Type, params *types2.TypeParamList, args []types2.Type) (types2.Type, bool) {
	switch u := t.(type) {
	case *types2.TypeParam:
		for i := range params.Len() {
			if params.At(i) == u {
				return args[i], true
			}
		}
		return nil, false
	case *types2.Pointer:
		e, ok := substJSONType(u.Elem(), params, args)
		if !ok {
			return nil, false
		}
		return types2.NewPointer(e), true
	case *types2.Slice:
		e, ok := substJSONType(u.Elem(), params, args)
		if !ok {
			return nil, false
		}
		return types2.NewSlice(e), true
	case *types2.Array:
		e, ok := substJSONType(u.Elem(), params, args)
		if !ok {
			return nil, false
		}
		return types2.NewArray(e, u.Len()), true
	case *types2.Map:
		k, ok := substJSONType(u.Key(), params, args)
		if !ok {
			return nil, false
		}
		v, ok := substJSONType(u.Elem(), params, args)
		if !ok {
			return nil, false
		}
		return types2.NewMap(k, v), true
	case *types2.Named:
		if u.TypeArgs().Len() == 0 {
			return t, true
		}
		return nil, false
	case *types2.Basic:
		return t, true
	case *types2.Interface:
		return t, true
	}
	return nil, false
}

// marshalText writes the MarshalJSON method of js to b.
func (js *jsonStruct) marshalText(b *strings.Builder) {
	g := newMarshalGen(js)
	var pins, pre, size, emit []string
	fixed := 1
	for _, f := range js.fields {
		c := g.code(f)
		pins = append(pins, c.pins...)
		pre = append(pre, c.pre...)
		size = append(size, c.size...)
		emit = append(emit, c.emit...)
		fixed += c.fixed
	}
	var body []string
	body = append(body, pins...)
	body = append(body, pre...)
	body = append(body, fmt.Sprintf("_logos_n := %d", fixed))
	body = append(body, size...)
	if g.firstFixed < 0 {
		body = append(body, ifLines("_logos_n == 1", []string{"_logos_n = 2"})...)
	}
	body = append(body, "_logos_buf := make([]byte, 0, _logos_n)")
	if g.runtimeSep {
		body = append(body, "_logos_sep := byte('{')")
	}
	body = append(body, emit...)
	if g.firstFixed < 0 {
		body = append(body, ifLines("_logos_sep == '{'", []string{"_logos_buf = append(_logos_buf, '{')"})...)
	}
	body = append(body, "_logos_buf = append(_logos_buf, '}')", "return _logos_buf, nil")
	fmt.Fprintf(b, "\nfunc (%s %s) MarshalJSON() ([]byte, error) {\n", jsonRecv, js.name)
	for _, l := range indentLines(body) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
}

// edgeCode spells a stateful field edge: a helper call for
// generated-side named types, or the shared walk for every other
// composite. The shape mirrors fallbackCode: the value bytes come from
// one call, the key from the fixed literal plan.
// helperCall spells a helper invocation, or "" when the edge funnels
// through the shared walk instead.
func helperCall(name, local string) string {
	if name == "" {
		return ""
	}
	return fmt.Sprintf("%s(%s, _logos_s)", name, local)
}

// walkCall spells the shared-walk invocation for one value.
func walkCall(local string) string {
	return fmt.Sprintf("_logos_s.Marshal(%s)", local)
}

func (g *marshalGen) edgeCode(f *jsonField, e jsonEdge) fieldCode {
	local := f.local()
	call := cmp.Or(helperCall(g.calls[f], local), walkCall(local))
	b := fmt.Sprintf("_logos_b%d", f.pos)
	ec := fmt.Sprintf("_logos_e%d", f.pos)
	stmt := []string{fmt.Sprintf("%s, %s := %s", b, ec, call)}
	check := ifLines(ec+" != nil", []string{
		fmt.Sprintf("return nil, _logos_s.RebaseDepthError(%s, int64(len(_logos_buf) + %d))", ec, g.litLen(f, "")),
	})
	var c fieldCode
	pins := []string{local + " := " + g.recv + "." + f.goName}
	guard, ok := g.edgePresence(f, e, local)
	if !ok {
		c.pins = pins
		c.pre = append(stmt, check...)
		c.size = []string{fmt.Sprintf("_logos_n += %d + len(%s)", g.litLen(f, ""), b)}
		c.emit = append(g.litStmts(f, ""), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b))
		return c
	}
	p := f.presenceName()
	c.pins = pins
	c.pre = []string{p + " := " + guard, "var " + b + " []byte"}
	inner := []string{fmt.Sprintf("var %s error", ec), fmt.Sprintf("%s, %s = %s", b, ec, call)}
	c.pre = append(c.pre, ifLines(p, append(inner, check...))...)
	c.size = ifLines(p, []string{fmt.Sprintf("_logos_n += %d + len(%s)", g.litLen(f, ""), b)})
	c.emit = ifLines(p, append(g.litStmts(f, ""), fmt.Sprintf("_logos_buf = append(_logos_buf, %s...)", b)))
	return c
}

// edgePresence returns the presence guard for an optional stateful
// field, or false for unconditional struct-value edges: a struct value
// is never empty, so omitempty alone writes the field, while omitzero
// still consults the shared OmitZero helper. Pointer helpers consult
// OmitZero too, matching the reflective omitzero rule that drops nil
// pointers and IsZero values alike.
func (g *marshalGen) edgePresence(f *jsonField, e jsonEdge, local string) (string, bool) {
	if !f.optional() {
		return "", false
	}
	if e == edgeHelper && !f.omitZero {
		return "", false
	}
	if e == edgePtrHelper {
		if f.omitZero {
			return local + " != nil && !" + jsonAlias + ".OmitZero(" + local + ")", true
		}
		return local + " != nil", true
	}
	return omitCall(f, g.recv+"."+f.goName), true
}

// marshalTextWalk writes a walk-shaped MarshalJSON method: the
// traversal table prelude, then a tail call into the value helper that
// shares the root's Walk. State lives in helpers and the walk; the
// method itself only owns the table. The receiver edge is entered
// before the helper call so cycle levels count the root the way the
// reflective encoder counts the value it was handed.
func (js *jsonStruct) marshalTextWalk(b *strings.Builder, plan *jsonWalkPlan, vname string) {
	var body []string
	body = append(body, "var _logos_s "+jsonAlias+".Walk")
	for _, t := range plan.table {
		body = append(body, "_logos_s.AddType("+t+")")
	}
	body = append(body,
		"if _logos_err := _logos_s.Enter(&"+jsonRecv+"); _logos_err != nil { return nil, _logos_err }",
		"_logos_b, _logos_e := "+vname+"("+jsonRecv+", &_logos_s)",
		"_logos_s.Leave(&"+jsonRecv+")",
		"return _logos_b, _logos_e")
	fmt.Fprintf(b, "\nfunc (%s %s) MarshalJSON() ([]byte, error) {\n", jsonRecv, js.name)
	for _, l := range indentLines(body) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
}

// emitJSONHelpers generates the helper pairs of plan into this file and
// marks the support imports. Pairs already emitted by another file of
// the package are referenced, never redefined.
func (fl *fileLower) emitJSONHelpers(pos syntax.Pos, plan *jsonWalkPlan) {
	for _, key := range plan.order {
		def := fl.pkg.jsonHelpers[key]
		if def.made || def.vname == "" {
			continue
		}
		def.made = true
		var b strings.Builder
		def.emitInto(&b)
		fl.addGeneratedDecls(pos, b.String())
	}
	fl.needJSON = true
	fl.needReflect = true
}

// emitInto writes the value and pointer helpers of one pair. The value
// helper runs the field pipeline with the plan's edges; the pointer
// helper folds nil, cycle tracking, and the dereference so call sites
// keep the fallback shape.
func (def *jsonHelperDef) emitInto(b *strings.Builder) {
	g := newMarshalGen(def.js)
	g.recv = "_logos_w"
	g.edges = def.edges
	g.calls = def.calls
	prelude := []string{
		"if _logos_err := _logos_s.EnterFrame(0); _logos_err != nil { return nil, _logos_err }",
		"defer _logos_s.LeaveFrame()",
	}
	body := sequentialJSONBody(g, prelude)
	fmt.Fprintf(b, "\nfunc %s(_logos_w %s, _logos_s *%s.Walk) ([]byte, error) {\n", def.vname, def.spell, jsonAlias)
	for _, l := range indentLines(body) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	fmt.Fprintf(b, "\nfunc %s(_logos_w *%s, _logos_s *%s.Walk) ([]byte, error) {\n", def.pname, def.spell, jsonAlias)
	inner := []string{
		"if _logos_w == nil { return []byte(\"null\"), nil }",
		"if _logos_err := _logos_s.Enter(_logos_w); _logos_err != nil { return nil, _logos_err }",
		"_logos_b, _logos_e := " + def.vname + "(*_logos_w, _logos_s)",
		"_logos_s.Leave(_logos_w)",
		"if _logos_e != nil { return nil, _logos_e }",
		"return _logos_b, nil",
	}
	for _, l := range indentLines(inner) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
}

// sequentialJSONBody retains the field codecs but encodes each field
// before starting the next. Thus a failing nested value can be rebased
// using the actual bytes already emitted, without sizing or replaying
// any user method. Only walk-shaped helpers use this path.
func sequentialJSONBody(g *marshalGen, prelude []string) []string {
	var pins, fields []string
	for _, f := range g.js.fields {
		c := g.code(f)
		pins = append(pins, c.pins...)
		fields = append(fields, c.pre...)
		fields = append(fields, c.emit...)
	}
	body := pins
	body = append(body, prelude...)
	body = append(body, "var _logos_buf []byte")
	if g.runtimeSep {
		body = append(body, "_logos_sep := byte('{')")
	}
	body = append(body, fields...)
	if g.firstFixed < 0 {
		body = append(body, ifLines("_logos_sep == '{'", []string{"_logos_buf = append(_logos_buf, '{')"})...)
	}
	return append(body, "_logos_buf = append(_logos_buf, '}')", "return _logos_buf, nil")
}
