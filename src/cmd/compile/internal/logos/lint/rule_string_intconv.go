// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// stringIntconv is suspicious::string_intconv (LINT-RULES.md Sec 1.2):
// detects conversions of the form string(x) where x has an integer type
// other than rune or byte, which yields a string of one Unicode code point
// rather than decimal digits.
var stringIntconv = Rule{
	ID:    "suspicious::string_intconv",
	Group: Suspicious,
	Go:    true,
	Check: checkStringIntconv,
}

func checkStringIntconv(pass *Pass) {
	for _, f := range pass.Files {
		syntax.Inspect(f, func(n syntax.Node) bool {
			call, ok := n.(*syntax.CallExpr)
			if !ok {
				return true
			}
			target, ok := targetStringType(pass, call)
			if !ok {
				return true
			}
			source, ok := isNonRuneInteger(TypeOf(call.ArgList[0]))
			if !ok {
				return true
			}
			pass.Reportf(call, "conversion from %s to %s yields a string of one rune, not a string of digits", source, target)
			return true
		})
	}
}

func targetStringType(pass *Pass, call *syntax.CallExpr) (string, bool) {
	if len(call.ArgList) != 1 || call.HasDots {
		return "", false
	}
	fun := unparen(call.Fun)
	var name *syntax.Name
	switch f := fun.(type) {
	case *syntax.Name:
		name = f
	case *syntax.SelectorExpr:
		name = f.Sel
	default:
		return "", false
	}
	obj := pass.Info.Uses[name]
	if obj == nil {
		if name.Value == "string" {
			obj = types2.Universe.Lookup("string")
		}
	}
	tname, ok := obj.(*types2.TypeName)
	if !ok {
		return "", false
	}
	T := tname.Type()
	if T == nil {
		return "", false
	}
	basic, ok := T.Underlying().(*types2.Basic)
	if !ok || basic.Kind() != types2.String {
		return "", false
	}
	return tname.Name(), true
}

func isNonRuneInteger(t types2.Type) (string, bool) {
	if t == nil {
		return "", false
	}
	basic, ok := t.Underlying().(*types2.Basic)
	if !ok {
		return "", false
	}
	if basic.Info()&types2.IsInteger == 0 {
		return "", false
	}
	switch basic.Kind() {
	case types2.Byte, types2.Rune, types2.UntypedRune:
		return "", false
	}
	name := basic.Name()
	if named, ok := t.(*types2.Named); ok && named.Obj() != nil {
		name = named.Obj().Name()
	}
	return name, true
}
