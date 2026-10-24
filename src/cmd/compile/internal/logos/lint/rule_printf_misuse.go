// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// printfMisuse is correctness::printf_misuse: validates format string verbs
// against argument count and types for formatted I/O calls (fmt.Printf,
// fmt.Sprintf, fmt.Fprintf, fmt.Errorf, log.Printf, etc.).
var printfMisuse = Rule{
	ID:    "correctness::printf_misuse",
	Group: Correctness,
	Go:    true,
	Check: checkPrintfMisuse,
}

func checkPrintfMisuse(pass *Pass) {
	for _, f := range pass.Files {
		syntax.Inspect(f, func(n syntax.Node) bool {
			call, ok := n.(*syntax.CallExpr)
			if !ok {
				return true
			}
			name, formatIdx, ok := printfNameAndFormatIdx(pass, call)
			if !ok {
				return true
			}
			if len(call.ArgList) <= formatIdx {
				return true
			}
			if call.HasDots {
				return true
			}
			format, ok := formatStringVal(call.ArgList[formatIdx])
			if !ok {
				return true
			}
			args := call.ArgList[formatIdx+1:]
			if d, found := diagnosePrintfCall(pass, f, call, name, format, args); found {
				pass.Report(d)
			}
			return true
		})
	}
}

func printfNameAndFormatIdx(pass *Pass, call *syntax.CallExpr) (string, int, bool) {
	if fn := calleeFunc(pass, call); fn != nil {
		fullName := fn.FullName()
		switch fullName {
		case "fmt.Printf", "fmt.Sprintf", "fmt.Errorf", "log.Printf", "log.Panicf", "log.Fatalf":
			return fullName, 0, true
		case "fmt.Fprintf", "fmt.Appendf":
			return fullName, 1, true
		}
		if fn.Pkg() != nil {
			pkg := fn.Pkg().Path()
			mname := fn.Name()
			if pkg == "log" && (mname == "Printf" || mname == "Panicf" || mname == "Fatalf") {
				return mname, 0, true
			}
			if pkg == "testing" && (mname == "Errorf" || mname == "Fatalf" || mname == "Logf" || mname == "Skipf") {
				return mname, 0, true
			}
		}
	}
	if sel, ok := call.Fun.(*syntax.SelectorExpr); ok {
		if pkgIdent, ok := sel.X.(*syntax.Name); ok {
			combined := pkgIdent.Value + "." + sel.Sel.Value
			switch combined {
			case "fmt.Printf", "fmt.Sprintf", "fmt.Errorf", "log.Printf", "log.Panicf", "log.Fatalf":
				return combined, 0, true
			case "fmt.Fprintf", "fmt.Appendf":
				return combined, 1, true
			}
		}
	}
	return "", 0, false
}

func formatStringVal(e syntax.Expr) (string, bool) {
	if s, ok := stringConstOf(e); ok {
		return s, true
	}
	if lit, ok := e.(*syntax.BasicLit); ok && lit.Kind == syntax.StringLit {
		s, err := strconv.Unquote(lit.Value)
		if err == nil {
			return s, true
		}
	}
	return "", false
}

type printfDirective struct {
	verb     rune
	argIndex int
	widthArg int
	precArg  int
}

func diagnosePrintfCall(pass *Pass, file *syntax.File, call *syntax.CallExpr, name, format string, args []syntax.Expr) (Diagnostic, bool) {
	dirs, hasExplicitIndex, malformedTag, ok := parsePrintfDirectives(format)
	if !ok {
		return Diagnostic{
			Pos:     syntax.StartPos(call),
			End:     pass.CallEnd(file, call),
			Message: fmt.Sprintf("%s format has malformed index tag [%s]", name, malformedTag),
		}, true
	}

	numArgs := len(args)
	maxArgNeeded := 0
	for _, d := range dirs {
		if d.widthArg >= 0 && d.widthArg+1 > maxArgNeeded {
			maxArgNeeded = d.widthArg + 1
		}
		if d.precArg >= 0 && d.precArg+1 > maxArgNeeded {
			maxArgNeeded = d.precArg + 1
		}
		if d.argIndex >= 0 && d.argIndex+1 > maxArgNeeded {
			maxArgNeeded = d.argIndex + 1
		}
	}

	// Check too few arguments
	if numArgs < maxArgNeeded {
		return Diagnostic{
			Pos:     syntax.StartPos(call),
			End:     pass.CallEnd(file, call),
			Message: fmt.Sprintf("%s call specifies %d args, %d provided", name, maxArgNeeded, numArgs),
		}, true
	}

	// Check too many arguments (only when no explicit indexing is used)
	if !hasExplicitIndex && numArgs > maxArgNeeded {
		return Diagnostic{
			Pos:     syntax.StartPos(call),
			End:     pass.CallEnd(file, call),
			Message: fmt.Sprintf("%s call specifies %d args, %d provided", name, maxArgNeeded, numArgs),
		}, true
	}

	// Check operand types
	for _, d := range dirs {
		if d.widthArg >= 0 && d.widthArg < numArgs {
			if !isIntegerType(TypeOf(args[d.widthArg])) {
				return Diagnostic{
					Pos:     syntax.StartPos(call),
					End:     pass.CallEnd(file, call),
					Message: fmt.Sprintf("%s format * width expects integer, provided %s", name, argTypeName(args[d.widthArg])),
				}, true
			}
		}
		if d.precArg >= 0 && d.precArg < numArgs {
			if !isIntegerType(TypeOf(args[d.precArg])) {
				return Diagnostic{
					Pos:     syntax.StartPos(call),
					End:     pass.CallEnd(file, call),
					Message: fmt.Sprintf("%s format * precision expects integer, provided %s", name, argTypeName(args[d.precArg])),
				}, true
			}
		}
		if d.argIndex >= 0 && d.argIndex < numArgs {
			if expected, match := matchVerbType(pass, d.verb, args[d.argIndex]); !match {
				return Diagnostic{
					Pos:     syntax.StartPos(call),
					End:     pass.CallEnd(file, call),
					Message: fmt.Sprintf("%s format verb %%%c at index %d expects %s, provided %s", name, d.verb, d.argIndex, expected, argTypeName(args[d.argIndex])),
				}, true
			}
		}
	}

	return Diagnostic{}, false
}

func parsePrintfDirectives(format string) (dirs []printfDirective, hasExplicitIndex bool, malformedTag string, ok bool) {
	pos := 0
	argCursor := 0

	for pos < len(format) {
		idx := strings.IndexByte(format[pos:], '%')
		if idx < 0 {
			break
		}
		pos += idx + 1
		if pos >= len(format) {
			break
		}
		if format[pos] == '%' {
			pos++
			continue
		}

		// Flags: [#0+- ]
		for pos < len(format) && strings.IndexByte("#0+- ", format[pos]) >= 0 {
			pos++
		}

		// Optional [n] before width
		verbArg := -1
		if pos < len(format) && format[pos] == '[' {
			tag, nextPos, tagOK := readIndexTag(format, pos)
			if !tagOK {
				return nil, false, tag, false
			}
			pos = nextPos
			hasExplicitIndex = true
			n, err := strconv.Atoi(tag)
			if err != nil || n < 1 {
				return nil, false, tag, false
			}
			verbArg = n - 1
		}

		// Width
		widthArg := -1
		if pos < len(format) && format[pos] == '*' {
			pos++
			if verbArg >= 0 {
				widthArg = verbArg
				verbArg = -1
			} else {
				widthArg = argCursor
				argCursor++
			}
		} else {
			for pos < len(format) && format[pos] >= '0' && format[pos] <= '9' {
				pos++
			}
		}

		// Precision
		precArg := -1
		if pos < len(format) && format[pos] == '.' {
			pos++
			if pos < len(format) && format[pos] == '[' {
				tag, nextPos, tagOK := readIndexTag(format, pos)
				if !tagOK {
					return nil, false, tag, false
				}
				pos = nextPos
				hasExplicitIndex = true
				n, err := strconv.Atoi(tag)
				if err != nil || n < 1 {
					return nil, false, tag, false
				}
				verbArg = n - 1
			}
			if pos < len(format) && format[pos] == '*' {
				pos++
				if verbArg >= 0 {
					precArg = verbArg
					verbArg = -1
				} else {
					precArg = argCursor
					argCursor++
				}
			} else {
				for pos < len(format) && format[pos] >= '0' && format[pos] <= '9' {
					pos++
				}
			}
		}

		// Optional [n] before verb
		if pos < len(format) && format[pos] == '[' {
			tag, nextPos, tagOK := readIndexTag(format, pos)
			if !tagOK {
				return nil, false, tag, false
			}
			pos = nextPos
			hasExplicitIndex = true
			n, err := strconv.Atoi(tag)
			if err != nil || n < 1 {
				return nil, false, tag, false
			}
			verbArg = n - 1
		}

		if pos >= len(format) {
			break
		}

		r, size := utf8.DecodeRuneInString(format[pos:])
		pos += size

		if verbArg < 0 {
			verbArg = argCursor
			argCursor++
		} else {
			argCursor = verbArg + 1
		}

		dirs = append(dirs, printfDirective{
			verb:     r,
			argIndex: verbArg,
			widthArg: widthArg,
			precArg:  precArg,
		})
	}

	return dirs, hasExplicitIndex, "", true
}

func readIndexTag(format string, pos int) (tag string, nextPos int, ok bool) {
	closeIdx := strings.IndexByte(format[pos:], ']')
	if closeIdx < 0 {
		return format[pos+1:], len(format), false
	}
	tag = format[pos+1 : pos+closeIdx]
	return tag, pos + closeIdx + 1, true
}

func matchVerbType(pass *Pass, verb rune, arg syntax.Expr) (expected string, ok bool) {
	typ := TypeOf(arg)
	if typ == nil {
		return "", true
	}
	u := types2.Unalias(typ).Underlying()
	if _, isIface := u.(*types2.Interface); isIface {
		return "", true
	}
	if _, isParam := types2.Unalias(typ).(*types2.TypeParam); isParam {
		return "", true
	}

	switch verb {
	case 'v', 'T':
		return "", true

	case 's':
		if isStringOrBytes(typ) || isStringerOrError(typ) {
			return "", true
		}
		return "string", false

	case 'd', 'i':
		if isIntegerType(typ) || isPointerOrReferenceType(typ) {
			return "", true
		}
		return "integer", false

	case 'b':
		if isIntegerType(typ) || isFloatOrComplex(typ) || isPointerOrReferenceType(typ) {
			return "", true
		}
		return "integer or float", false

	case 'o', 'O':
		if isIntegerType(typ) || isPointerOrReferenceType(typ) {
			return "", true
		}
		return "integer", false

	case 'x', 'X':
		if isIntegerType(typ) || isFloatOrComplex(typ) || isStringOrBytes(typ) || isPointerOrReferenceType(typ) {
			return "", true
		}
		return "integer, float, or string", false

	case 'q':
		if isStringOrBytes(typ) || isIntegerType(typ) {
			return "", true
		}
		return "string or integer", false

	case 'p':
		if isPointerOrReferenceType(typ) {
			return "", true
		}
		return "pointer", false

	case 't':
		if isBooleanType(typ) {
			return "", true
		}
		return "bool", false

	case 'f', 'e', 'E', 'g', 'G':
		if isFloatOrComplex(typ) {
			return "", true
		}
		return "float", false

	case 'c', 'U':
		if isIntegerType(typ) {
			return "", true
		}
		return "rune or integer", false

	case 'w':
		if isErrorType(typ) || isStringerOrError(typ) {
			return "", true
		}
		return "error", false

	default:
		return "", true
	}
}

func isPointerOrReferenceType(t types2.Type) bool {
	if t == nil {
		return false
	}
	u := types2.Unalias(t).Underlying()
	switch u.(type) {
	case *types2.Pointer, *types2.Chan, *types2.Map, *types2.Signature, *types2.Slice:
		return true
	case *types2.Basic:
		return u.(*types2.Basic).Kind() == types2.UnsafePointer
	}
	return false
}

func isIntegerType(t types2.Type) bool {
	if t == nil {
		return false
	}
	b, ok := types2.Unalias(t).Underlying().(*types2.Basic)
	if !ok {
		return false
	}
	return b.Info()&types2.IsInteger != 0
}

func isBooleanType(t types2.Type) bool {
	if t == nil {
		return false
	}
	b, ok := types2.Unalias(t).Underlying().(*types2.Basic)
	if !ok {
		return false
	}
	return b.Info()&types2.IsBoolean != 0
}

func isFloatOrComplex(t types2.Type) bool {
	if t == nil {
		return false
	}
	b, ok := types2.Unalias(t).Underlying().(*types2.Basic)
	if !ok {
		return false
	}
	return b.Info()&(types2.IsFloat|types2.IsComplex) != 0
}

func isStringOrBytes(t types2.Type) bool {
	if t == nil {
		return false
	}
	u := types2.Unalias(t).Underlying()
	if b, ok := u.(*types2.Basic); ok {
		return b.Info()&types2.IsString != 0
	}
	if s, ok := u.(*types2.Slice); ok {
		if b, ok := types2.Unalias(s.Elem()).Underlying().(*types2.Basic); ok {
			return b.Kind() == types2.Byte || b.Kind() == types2.Rune
		}
	}
	return false
}

func isStringerOrError(t types2.Type) bool {
	if isErrorType(t) {
		return true
	}
	obj, _, _ := types2.LookupFieldOrMethod(t, true, nil, "String")
	if fn, ok := obj.(*types2.Func); ok {
		sig := fn.Type().(*types2.Signature)
		if sig.Params().Len() == 0 && sig.Results().Len() == 1 {
			if res, ok := types2.Unalias(sig.Results().At(0).Type()).Underlying().(*types2.Basic); ok && res.Kind() == types2.String {
				return true
			}
		}
	}
	return false
}

func argTypeName(arg syntax.Expr) string {
	t := TypeOf(arg)
	if t != nil {
		return t.String()
	}
	return "unknown"
}
