// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"cmd/compile/internal/base"
	"cmd/compile/internal/types2"
)

// This file spells the generated UnmarshalJSON (SPEC Sec 12.2): defaults
// first, a seen mask over the required non-nil fields, key dispatch by a
// switch (up to jsonKeySwitchMax keys) or a perfect-hash table, unknown
// keys skipped, and a per-field reflective fallback for unmodeled kinds.

// jsonKeySwitchMax is the largest key count dispatched by a switch.
const jsonKeySwitchMax = constMapSwitchMax

// foldJSONKey returns the case-folded form of a key: the encoding/json
// foldName rule, where every rune maps to the smallest rune of its
// unicode.SimpleFold orbit and ASCII letters map to upper case.
func foldJSONKey(k string) string {
	var b strings.Builder
	for i := 0; i < len(k); {
		c := k[i]
		if c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			b.WriteByte(c)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(k[i:])
		b.WriteRune(foldJSONRune(r))
		i += n
	}
	return b.String()
}

// foldJSONRune returns the smallest rune of the fold orbit of r.
func foldJSONRune(r rune) rune {
	for {
		r2 := unicode.SimpleFold(r)
		if r2 <= r {
			return r2
		}
		r = r2
	}
}

// seenWords returns the number of seen-mask words for n required fields.
func seenWords(n int) int {
	return (n + 63) / 64
}

// unmarshalText writes UnmarshalJSON and its helper declarations to b.
func (js *jsonStruct) unmarshalText(b *strings.Builder) {
	var body []string
	body = append(body, fmt.Sprintf("_logos_s := %s.NewScanner(_logos_data, (*%s)(nil))", jsonAlias, js.name))
	body = append(body, ifLines("!_logos_s.Object()", []string{"return _logos_s.End()"})...)
	if len(js.required) > 0 {
		body = append(body, fmt.Sprintf("var _logos_seen [%d]uint64", seenWords(len(js.required))))
	}
	body = append(body, js.defaults...)
	body = append(body, js.loopLines()...)
	body = append(body, ifLines("_logos_e := _logos_s.Err(); _logos_e != nil", []string{"return _logos_e"})...)
	body = append(body, js.requiredCheck()...)
	body = append(body, "return _logos_s.Saved()")
	fmt.Fprintf(b, "\nfunc (%s *%s) UnmarshalJSON(_logos_data []byte) error {\n", jsonRecv, js.name)
	for _, l := range indentLines(body) {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	js.keyFuncs(b)
	js.missingFunc(b)
}

// loopLines spells the member loop: key lookup, then one case per field.
func (js *jsonStruct) loopLines() []string {
	body := []string{
		fmt.Sprintf("_logos_i := _logos_jsonkey_%s(_logos_s.Key())", js.name),
		"if _logos_i < 0 {",
		fmt.Sprintf("\t_logos_i = _logos_jsonfold_%s(_logos_s.FoldKey())", js.name),
		"}",
		"switch _logos_i {",
	}
	for _, f := range js.fields {
		body = append(body, fmt.Sprintf("case %d:", f.pos))
		body = append(body, indentLines(js.decodeLines(f))...)
	}
	body = append(body, "default:", "\t_logos_s.Skip()", "}")
	return blockLines("for _logos_s.Next()", body)
}

// decodeLines spells the case body of one field.
func (js *jsonStruct) decodeLines(f *jsonField) []string {
	var lines []string
	if f.required >= 0 {
		lines = append(lines, ifLines("_logos_s.Null()", []string{
			fmt.Sprintf("return %s.MissingFieldError{Type: %q, Field: %q}", rtAlias, js.name, f.goName),
		})...)
		lines = append(lines, fmt.Sprintf("_logos_seen[%d] |= 1 << %d", f.required/64, f.required%64))
	}
	return append(lines, js.valueLines(f)...)
}

// valueLines spells the statements decoding the current value into f.
func (js *jsonStruct) valueLines(f *jsonField) []string {
	field := jsonRecv + "." + f.goName
	switch f.shape.kind {
	case shapeScalar:
		return []string{fmt.Sprintf("%s(&_logos_s, &%s)", f.codec().decodeFn(), field)}
	case shapePointer:
		return f.pointerLines(field)
	case shapeSlice:
		return []string{fmt.Sprintf("%s.Slice(&_logos_s, &%s, %s)", jsonAlias, field, f.elemCodec().decodeFn())}
	case shapeUnion:
		return js.unionLines(f, field)
	case shapeFallback:
		if f.quoted {
			if isExactNumber(f.typ) || hasUnmarshalMethods(f.typ) {
				return []string{fmt.Sprintf("_logos_s.FallbackQ(&%s)", field)}
			}
			if kind, ok := fallbackScalarKind(f.typ); ok {
				return js.fallbackQuotedValueLines(f, field, kind)
			}
		}
		return []string{fmt.Sprintf("_logos_s.Fallback(&%s)", field)}
	default:
		base.Fatalf("logos: unhandled shape kind %v for %s", f.shape.kind, f.goName)
		return nil
	}
}

// fallbackQuotedValueLines spells the decoding of a fallback scalar carrying
// the `string` option when the type has no unmarshal-side JSON methods: the
// quoted scalar reader (IntQ, UintQ, FloatQ, BoolQ, StringQ) for its
// underlying kind. Method types keep the reflective Fallback, which the
// encoder already handles with method-wins precedence.
func (js *jsonStruct) fallbackQuotedValueLines(f *jsonField, field string, kind scalarKind) []string {
	u := types2.Unalias(f.typ)
	if cv, ok := u.(*types2.ConstView); ok {
		u = types2.Unalias(cv.Elem)
	}
	_, isPtr := u.(*types2.Pointer)
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
		tmp.shape = jsonShape{kind: shapePointer, scalar: kind, nonNil: f.shape.nonNil}
		return tmp.pointerLines(field)
	}
	tmp.shape = jsonShape{kind: shapeScalar, scalar: kind}
	return []string{fmt.Sprintf("%s(&_logos_s, &%s)", tmp.codec().decodeFn(), field)}
}

// unionLines spells the decoding of a closed union: trials in declaration order
// into scratch values with isolated scanners over Scanner.Value(), assigning
// target only on a single success, returning UnionTrialError on zero successes
// or multiple successes (SPEC Sec 12.2, T-Sec12, T-203.1).
func (js *jsonStruct) unionLines(f *jsonField, field string) []string {
	members, ok := unionTerms(f.typ)
	if !ok || len(members) == 0 {
		base.Fatalf("logos: union field %s has no union terms", f.goName)
	}
	var localPkg *types2.Package
	if named, ok := types2.Unalias(f.typ).(*types2.Named); ok && named.Obj() != nil {
		localPkg = named.Obj().Pkg()
	}
	qual := func(p *types2.Package) string {
		if p == nil || (localPkg != nil && p.Path() == localPkg.Path()) {
			return ""
		}
		return p.Name()
	}
	unionName := types2.TypeString(f.typ, qual)
	memberQuotes := make([]string, len(members))
	for i, m := range members {
		memberQuotes[i] = strconv.Quote(types2.TypeString(m, qual))
	}
	membersList := strings.Join(memberQuotes, ", ")
	// Collect the union keyspace: bare names of members with emitted key helpers.
	// A key in _logos_raw modeled by any member but not this member disqualifies
	// this member's trial; keys no member models are ignored (SPEC Sec 12.2).
	type memberKeys struct {
		bareName string
		usable   bool
	}
	memberList := make([]memberKeys, len(members))
	for i, m := range members {
		var bareName string
		samePkg := false
		var st *types2.Struct
		isStruct := false
		if ptr, ok := types2.Unalias(m).(*types2.Pointer); ok {
			st, isStruct = types2.Unalias(ptr.Elem()).Underlying().(*types2.Struct)
			if named, ok := types2.Unalias(ptr.Elem()).(*types2.Named); ok && named.Obj() != nil {
				bareName = named.Obj().Name()
				samePkg = localPkg != nil && named.Obj().Pkg() != nil && localPkg.Path() == named.Obj().Pkg().Path()
			}
		} else if named, ok := types2.Unalias(m).(*types2.Named); ok && named.Obj() != nil {
			st, isStruct = types2.Unalias(m).Underlying().(*types2.Struct)
			bareName = named.Obj().Name()
			samePkg = localPkg != nil && named.Obj().Pkg() != nil && localPkg.Path() == named.Obj().Pkg().Path()
		}
		memberList[i] = memberKeys{bareName: bareName, usable: isStruct && st.NumFields() > 0 && bareName != "" && samePkg}
	}

	var trials []string
	trials = append(trials, "_logos_raw := _logos_s.Value()")
	trials = append(trials, "var _logos_match int")
	trials = append(trials, fmt.Sprintf("var _logos_target %s", unionName))

	for i, m := range members {
		spell := types2.TypeString(m, qual)
		scratch := fmt.Sprintf("_logos_scratch%d", i)
		st, isStruct := types2.Unalias(m).Underlying().(*types2.Struct)
		var bareName string
		samePkg := false
		if ptr, ok := types2.Unalias(m).(*types2.Pointer); ok {
			st, isStruct = types2.Unalias(ptr.Elem()).Underlying().(*types2.Struct)
			if named, ok := types2.Unalias(ptr.Elem()).(*types2.Named); ok && named.Obj() != nil {
				bareName = named.Obj().Name()
				samePkg = localPkg != nil && named.Obj().Pkg() != nil && localPkg.Path() == named.Obj().Pkg().Path()
			}
		} else if named, ok := types2.Unalias(m).(*types2.Named); ok && named.Obj() != nil {
			bareName = named.Obj().Name()
			samePkg = localPkg != nil && named.Obj().Pkg() != nil && localPkg.Path() == named.Obj().Pkg().Path()
		}
		hasKeyHelper := isStruct && st.NumFields() > 0 && bareName != "" && samePkg

		trials = append(trials, "{")
		if ptr, ok := types2.Unalias(m).(*types2.Pointer); ok {
			elem := types2.TypeString(ptr.Elem(), qual)
			trials = append(trials, fmt.Sprintf("\t%s := new(%s)", scratch, elem))
		} else {
			trials = append(trials, fmt.Sprintf("\tvar %s %s", scratch, spell))
		}
		trials = append(trials, fmt.Sprintf("\tif %s.UnmarshalJSON(_logos_raw) == nil {", scratch))
		if hasKeyHelper {
			trials = append(trials, "\t\t_logos_matched := false")
			trials = append(trials, fmt.Sprintf("\t\t_logos_chk := %s.NewScanner(_logos_raw, (*%s)(nil))", jsonAlias, bareName))
			trials = append(trials, "\t\tif _logos_chk.Object() {")
			trials = append(trials, "\t\t\t_logos_matched = true")
			trials = append(trials, "\t\t\t_logos_keys := 0")
			trials = append(trials, "\t\t\tfor _logos_chk.Next() {")
			trials = append(trials, fmt.Sprintf("\t\t\t\tif _logos_jsonkey_%s(_logos_chk.Key()) >= 0 || _logos_jsonfold_%s(_logos_chk.FoldKey()) >= 0 {", bareName, bareName))
			trials = append(trials, "\t\t\t\t\t_logos_keys++")
			trials = append(trials, "\t\t\t\t} else {")
			// A key modeled by any sibling member but not this one disqualifies
			// this trial; a key no member models is ignored.
			for j, sib := range memberList {
				if j == i || !sib.usable {
					continue
				}
				trials = append(trials, fmt.Sprintf("\t\t\t\t\tif _logos_jsonkey_%s(_logos_chk.Key()) >= 0 || _logos_jsonfold_%s(_logos_chk.FoldKey()) >= 0 {", sib.bareName, sib.bareName))
				trials = append(trials, "\t\t\t\t\t\t_logos_matched = false")
				trials = append(trials, "\t\t\t\t\t}")
			}
			trials = append(trials, "\t\t\t\t}")
			trials = append(trials, "\t\t\t\t_logos_chk.Skip()")
			trials = append(trials, "\t\t\t}")
			trials = append(trials, "\t\t\tif _logos_keys == 0 {")
			trials = append(trials, "\t\t\t\t_logos_matched = false")
			trials = append(trials, "\t\t\t}")
			trials = append(trials, "\t\t}")
			trials = append(trials, "\t\tif _logos_matched {")
			trials = append(trials, "\t\t\t_logos_match++")
			trials = append(trials, fmt.Sprintf("\t\t\t_logos_target = %s", scratch))
			trials = append(trials, "\t\t}")
		} else {
			trials = append(trials, "\t\t_logos_match++")
			trials = append(trials, fmt.Sprintf("\t\t_logos_target = %s", scratch))
		}
		trials = append(trials, "\t}")
		trials = append(trials, "}")
	}

	trials = append(trials, "switch _logos_match {")
	trials = append(trials, "case 1:")
	trials = append(trials, fmt.Sprintf("\t%s = _logos_target", field))
	trials = append(trials, "case 0:")
	trials = append(trials, fmt.Sprintf("\treturn %s.UnionTrialError{Union: %q, Members: []string{%s}, Matched: 0, Total: %d}", rtAlias, unionName, membersList, len(members)))
	trials = append(trials, "default:")
	trials = append(trials, fmt.Sprintf("\treturn %s.UnionTrialError{Union: %q, Members: []string{%s}, Matched: _logos_match, Total: %d}", rtAlias, unionName, membersList, len(members)))
	trials = append(trials, "}")

	if f.required < 0 {
		return ifElseLines("!_logos_s.Null()", trials, []string{field + " = nil"})
	}
	out := []string{"{"}
	out = append(out, indentLines(trials)...)
	return append(out, "}")
}

// pointerLines spells the decoding of a pointer to a scalar: null clears an
// optional pointer, anything else decodes into the existing or a new pointee.
func (f *jsonField) pointerLines(field string) []string {
	fill := fmt.Sprintf("%s(&_logos_s, %s.Alloc(&%s))", f.codec().decodeFn(), jsonAlias, field)
	if f.required >= 0 {
		return []string{fill}
	}
	return ifElseLines("_logos_s.Null()", []string{field + " = nil"}, []string{fill})
}

// requiredCheck spells the mask test at the closing brace.
func (js *jsonStruct) requiredCheck() []string {
	n := len(js.required)
	var lines, conds []string
	for w := range seenWords(n) {
		lines = append(lines, fmt.Sprintf("const _logos_required%d = 1<<%d - 1", w, min(64, n-64*w)))
		conds = append(conds, fmt.Sprintf("_logos_seen[%[1]d]&_logos_required%[1]d != _logos_required%[1]d", w))
	}
	if n == 0 {
		return nil
	}
	ret := fmt.Sprintf("return %s.MissingFieldError{Type: %q, Field: _logos_jsonmissing_%s(_logos_seen)}", rtAlias, js.name, js.name)
	return append(lines, ifLines(strings.Join(conds, " || "), []string{ret})...)
}

// missingFunc writes the function naming the first missing required field.
func (js *jsonStruct) missingFunc(b *strings.Builder) {
	if len(js.required) == 0 {
		return
	}
	quoted := make([]string, len(js.required))
	for i, name := range js.required {
		quoted[i] = strconv.Quote(name)
	}
	fmt.Fprintf(b, "\nfunc _logos_jsonmissing_%s(seen [%d]uint64) string {\n", js.name, seenWords(len(js.required)))
	fmt.Fprintf(b, "\tfor i, name := range [...]string{%s} {\n", strings.Join(quoted, ", "))
	b.WriteString("\t\tif seen[i/64]>>(i%64)&1 == 0 {\n\t\t\treturn name\n\t\t}\n\t}\n\treturn \"\"\n}\n")
}

// keyEntry is one key of a lookup function and the field index it yields.
type keyEntry struct {
	key string
	idx int
}

// keyFuncs writes the exact and the case-folded key lookup functions.
func (js *jsonStruct) keyFuncs(b *strings.Builder) {
	var exact, folded []keyEntry
	seen := make(map[string]bool)
	for _, f := range js.fields {
		exact = append(exact, keyEntry{f.key, f.pos})
		fk := foldJSONKey(f.key)
		if !seen[fk] {
			seen[fk] = true
			folded = append(folded, keyEntry{fk, f.pos})
		}
	}
	writeKeyFunc(b, js.name, "jsonkey", exact)
	writeKeyFunc(b, js.name, "jsonfold", folded)
}

// writeKeyFunc writes one lookup function _logos_<kind>_<name>(k string)
// int returning the field index of k or -1: a switch for few keys, a
// perfect-hash table (SPEC Sec 9.1) for more.
func writeKeyFunc(b *strings.Builder, name, kind string, keys []keyEntry) {
	if len(keys) <= jsonKeySwitchMax {
		fmt.Fprintf(b, "\nfunc _logos_%s_%s(k string) int {\n\tswitch k {\n", kind, name)
		for _, e := range keys {
			fmt.Fprintf(b, "\tcase %s:\n\t\treturn %d\n", strconv.Quote(e.key), e.idx)
		}
		b.WriteString("\t}\n\treturn -1\n}\n")
		return
	}
	writeKeyTable(b, name, kind, keys)
}

// writeKeyTable writes a read-only perfect-hash table over keys and the
// lookup function that probes it.
func writeKeyTable(b *strings.Builder, name, kind string, keys []keyEntry) {
	entries := make([]constMapKey, len(keys))
	for i, e := range keys {
		entries[i] = constMapKey{hash: constHashString(e.key), canon: e.key}
	}
	layout, err := buildConstMap(entries, defaultConstMapBudget)
	if err != nil {
		base.Fatalf("logos: no key table for %s: %v", name, err)
	}
	table := fmt.Sprintf("_logos_%ss_%s", kind, name)
	fmt.Fprintf(b, "\n%s\nvar %s = %s.ConstTable[string, int]{Salt: %d, Buckets: []%s.ConstBucket{", rodataMarker, table, rtAlias, layout.salt, rtAlias)
	for i, bk := range layout.buckets {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "{Off: %d, Size: %d, Seed: %d}", bk.off, bk.size, bk.seed)
	}
	fmt.Fprintf(b, "}, Slots: []%s.ConstSlot[string, int]{", rtAlias)
	for s, ki := range layout.slots {
		if s > 0 {
			b.WriteString(", ")
		}
		if ki < 0 {
			b.WriteString("{}")
			continue
		}
		fmt.Fprintf(b, "{Key: %s, Val: %d, Occ: true}", strconv.Quote(keys[ki].key), keys[ki].idx)
	}
	b.WriteString("}}\n")
	fmt.Fprintf(b, "func _logos_%s_%s(k string) int {\n\tif i, ok := %s.Get(%s.HashString(k), k); ok {\n\t\treturn i\n\t}\n\treturn -1\n}\n", kind, name, table, rtAlias)
}
