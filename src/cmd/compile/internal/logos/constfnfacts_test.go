// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"go/constant"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// checkConstFnFixture parses and checks one Logos source for constfn
// emission, failing on any parse or check diagnostic.
func checkConstFnFixture(t *testing.T, src string) (*types2.Package, *types2.Info, *syntax.File) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "constfn.lgo")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := syntax.ParseFile(path, func(err error) { t.Fatalf("parse: %v", err) }, nil, syntax.LogosMode)
	if err != nil {
		t.Fatal(err)
	}
	imp := newTestImporter(t)
	var checkErrs []error
	conf := types2.Config{
		Importer: imp,
		Error:    func(err error) { checkErrs = append(checkErrs, err) },
		Logos:    true,
	}
	info := &types2.Info{
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
	}
	pkg, _ := conf.Check("constfntest", []*syntax.File{file}, info)
	for _, err := range checkErrs {
		t.Errorf("check: %v", err)
	}
	if len(checkErrs) > 0 {
		t.FailNow()
	}
	return pkg, info, file
}

// parseConstFnPayload asserts one emitted source parses as real Logos
// syntax, returning its text.
func parseConstFnPayload(t *testing.T, rec facts.ConstFuncFact) string {
	t.Helper()
	text := string(rec.Source)
	var errs []string
	_, first := syntax.Parse(syntax.NewFileBase("constfn.lgo"), strings.NewReader(text), func(err error) {
		errs = append(errs, err.Error())
	}, nil, syntax.CheckBranches|syntax.LogosMode)
	if first != nil || len(errs) > 0 {
		t.Fatalf("constfn payload for %s does not parse: %v %q\n%s", rec.Object, first, errs, text)
	}
	return text
}

// wantScopeConst requires one scope name to be a constant of the expected
// type and integer value.
func wantScopeConst(t *testing.T, scope *types2.Scope, rec, name, wantType string, want int64) {
	t.Helper()
	obj := scope.Lookup(name)
	c, ok := obj.(*types2.Const)
	if !ok {
		t.Fatalf("constfn payload for %s: %s = %T, want a constant", rec, name, obj)
	}
	if got := c.Type().String(); got != wantType {
		t.Errorf("constfn payload for %s: %s has type %s, want %s", rec, name, got, wantType)
	}
	v, ok := constant.Int64Val(c.Val())
	if !ok || v != want {
		t.Errorf("constfn payload for %s: %s = %v, want %d", rec, name, c.Val(), want)
	}
}

// wantScopeStruct requires one scope name to have a struct type with the
// expected field types.
func wantScopeStruct(t *testing.T, scope *types2.Scope, rec, name string, want []string) {
	t.Helper()
	obj := scope.Lookup(name)
	if obj == nil {
		t.Fatalf("constfn payload for %s: missing %s", rec, name)
	}
	st, ok := obj.Type().Underlying().(*types2.Struct)
	if !ok {
		t.Fatalf("constfn payload for %s: %s has type %v, want a struct", rec, name, obj.Type())
	}
	if got := st.NumFields(); got != len(want) {
		t.Fatalf("constfn payload for %s: %s has %d fields, want %d", rec, name, got, len(want))
	}
	for i, wt := range want {
		if got := st.Field(i).Type().String(); got != wt {
			t.Errorf("constfn payload for %s: %s field %d has type %s, want %s", rec, name, i, got, wt)
		}
	}
}

// wantScopeConstFloat requires one scope name to be a constant with the
// expected float64 value. A typed-int source constant cannot combine with
// a fractional literal, so a clean recheck plus this value proves the
// payload preserved the constant's untypedness.
func wantScopeConstFloat(t *testing.T, scope *types2.Scope, rec, name string, want float64) {
	t.Helper()
	obj := scope.Lookup(name)
	c, ok := obj.(*types2.Const)
	if !ok {
		t.Fatalf("constfn payload for %s: %s = %T, want a constant", rec, name, obj)
	}
	v, ok := constant.Float64Val(c.Val())
	if !ok || v != want {
		t.Errorf("constfn payload for %s: %s = %v, want %v", rec, name, c.Val(), want)
	}
}

// wantScopeConstUint requires one scope name to be a constant with the
// expected uint64 value. A typed-int source constant cannot shift past
// the platform int width, so a clean recheck plus this value proves the
// payload preserved the constant's untypedness.
func wantScopeConstUint(t *testing.T, scope *types2.Scope, rec, name string, want uint64) {
	t.Helper()
	obj := scope.Lookup(name)
	c, ok := obj.(*types2.Const)
	if !ok {
		t.Fatalf("constfn payload for %s: %s = %T, want a constant", rec, name, obj)
	}
	v, ok := constant.Uint64Val(c.Val())
	if !ok || v != want {
		t.Errorf("constfn payload for %s: %s = %v, want %d", rec, name, c.Val(), want)
	}
}

// TestEmitConstFnFactsPayloadParses closes one const function over every
// repaired span boundary and requires each exported const function to
// have a record whose closed source reparses, rechecks, and evaluates
// probe consts to the source-defined values.
func TestEmitConstFnFactsPayloadParses(t *testing.T) {
	const src = `package constfntest

const func Total() int {
	return Double(Zero()) + K + Space + Flag + SumPair(NewPair())
}

const func Double(x int) int {
	return x * 2
}

const func Zero() int {
	return 0
}

const func Empty() int {
	return 1
}

const func NewPair() Pair {
	return Pair{A: 1, B: 2}
}

const func SumPair(p Pair) int {
	return p.A + p.B
}

type Pair struct {
	A int
	B int /* } */
}

const /*g*/ (
	K = 1
	Space = Empty( )
)

const /*c*/ Flag = 2
`
	pkg, info, file := checkConstFnFixture(t, src)
	srcBytes := []byte(src)
	var f *facts.Facts
	f = EmitConstFnFacts(pkg, info, []*syntax.File{file}, map[*syntax.File][]byte{file: srcBytes}, f)
	probe := map[string]string{
		"Total":   "const total = Total()\nconst frac = K + 0.5\nconst big = K << 63",
		"Double":  "const doubled = Double(4)",
		"Zero":    "const zero = Zero()",
		"Empty":   "const empty = Empty()",
		"NewPair": "const pair = NewPair()",
		"SumPair": "const sum = SumPair(Pair{A: 1, B: 2})",
	}
	seen := map[string]bool{}
	for _, rec := range f.ConstFuncs {
		p, ok := probe[rec.Object]
		if !ok {
			t.Fatalf("unexpected constfn record %q", rec.Object)
		}
		if seen[rec.Object] {
			t.Fatalf("duplicate constfn record %q", rec.Object)
		}
		seen[rec.Object] = true
		_, scope := checkConstFnPayload(t, rec, p)
		switch rec.Object {
		case "Total":
			wantScopeConst(t, scope, rec.Object, "total", "int", 7)
			wantScopeConstFloat(t, scope, rec.Object, "frac", 1.5)
			wantScopeConstUint(t, scope, rec.Object, "big", 1<<63)
			wantScopeConst(t, scope, rec.Object, "Space", "int", 1)
			wantScopeConst(t, scope, rec.Object, "Flag", "untyped int", 2)
		case "Double":
			wantScopeConst(t, scope, rec.Object, "doubled", "int", 8)
		case "Zero":
			wantScopeConst(t, scope, rec.Object, "zero", "int", 0)
		case "Empty":
			wantScopeConst(t, scope, rec.Object, "empty", "int", 1)
		case "NewPair":
			if tn, ok := scope.Lookup("Pair").(*types2.TypeName); !ok || tn.IsAlias() {
				t.Errorf("constfn payload for %s: Pair lost its declared struct identity", rec.Object)
			}
			wantScopeStruct(t, scope, rec.Object, "pair", []string{"int", "int"})
		case "SumPair":
			wantScopeConst(t, scope, rec.Object, "sum", "int", 3)
		}
	}
	for name := range probe {
		if !seen[name] {
			t.Errorf("missing constfn record %q", name)
		}
	}
}

// TestConstFnRenderVarStructType renders an explicit struct-typed var
// through its closing brace and requires the declaration to parse.
func TestConstFnRenderVarStructType(t *testing.T) {
	const src = `package constfntest

var V struct {
	X int
	Y int /* } */
}

const func Total() int {
	return 1
}
`
	pkg, info, file := checkConstFnFixture(t, src)
	s := newConstFnSrc(pkg, info, []*syntax.File{file}, map[*syntax.File][]byte{file: []byte(src)})
	s.indexAll()
	if !s.at(file) {
		t.Fatal("cannot load fixture file")
	}
	var vd *syntax.VarDecl
	for _, d := range file.DeclList {
		if v, ok := d.(*syntax.VarDecl); ok {
			vd = v
		}
	}
	if vd == nil {
		t.Fatal("no var declaration in fixture")
	}
	text, err := s.renderVar(file, vd, "V")
	if err != nil {
		t.Fatalf("renderVar: %v", err)
	}
	var errs []string
	_, first := syntax.Parse(syntax.NewFileBase("constfnvar.lgo"), strings.NewReader("package constfntest\n"+text+"\n"), func(err error) {
		errs = append(errs, err.Error())
	}, nil, syntax.CheckBranches|syntax.LogosMode)
	if first != nil || len(errs) > 0 {
		t.Fatalf("rendered var does not parse: %v %q\n%s", first, errs, text)
	}
}
