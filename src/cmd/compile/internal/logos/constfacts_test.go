// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"cmd/compile/internal/logos/facts"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
	"cmd/internal/goobj"
)

// TestEmitConstFacts checks a small .lgo package, calls EmitConstFacts
// directly, and asserts the const lines plus the ReceiverMutated and
// ParamMutated flags, including propagation to a wrapper and merging
// with pre-existing facts.
func TestEmitConstFacts(t *testing.T) {
	const src = `package factstest

type Box struct {
	Items const []int
	Name  string
}

func Sum(xs const []int) int {
	total := 0
	for _, v := range xs {
		total += v
	}
	return total
}

func AppendIt(xs []int, v int) []int {
	return append(xs, v)
}

func Double(xs []int) []int {
	return AppendIt(AppendIt(xs, 1), 2)
}

func (b *Box) Rename(n string) {
	b.Name = n
}

func (b *Box) Count() int {
	return len(b.Items)
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "facts.lgo")
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
	pkg, _ := conf.Check("factstest", []*syntax.File{file}, info)
	if err := errors.Join(checkErrs...); err != nil {
		t.Fatalf("check: %v", err)
	}

	seed := &facts.Facts{Fns: []facts.FnFact{{Object: "AppendIt", ReturnsNonNil: true}}}
	got := EmitConstFacts(pkg, info, []*syntax.File{file}, seed)
	if got != seed {
		t.Fatal("EmitConstFacts did not return the passed facts")
	}

	consts := make(map[string]bool)
	for _, c := range got.Consts {
		consts[c.Object+" "+c.Path] = true
	}
	for _, want := range []string{"Sum P0", "Box FItems"} {
		if !consts[want] {
			t.Errorf("missing const line %q (have %v)", want, got.Consts)
		}
	}

	fns := make(map[string]facts.FnFact)
	for _, fn := range got.Fns {
		fns[fn.Object] = fn
	}
	checkFn := func(object, mask string, recv bool) {
		t.Helper()
		fn, ok := fns[object]
		if mask == "" && !recv {
			if ok {
				t.Errorf("%s unexpectedly has facts %+v", object, fn)
			}
			return
		}
		if !ok {
			t.Errorf("missing fn facts for %s", object)
			return
		}
		if fn.ParamMutated != mask {
			t.Errorf("%s ParamMutated = %q, want %q", object, fn.ParamMutated, mask)
		}
		if fn.ReceiverMutated != recv {
			t.Errorf("%s ReceiverMutated = %v, want %v", object, fn.ReceiverMutated, recv)
		}
	}
	checkFn("AppendIt", "1", false)
	checkFn("Double", "1", false)
	checkFn("(*Box).Rename", "", true)
	checkFn("Sum", "", false)
	checkFn("(*Box).Count", "", false)
	if fn := fns["AppendIt"]; !fn.ReturnsNonNil {
		t.Error("AppendIt lost its seeded ReturnsNonNil flag")
	}
}

func TestWideMutationMaskHelpers(t *testing.T) {
	var m []byte
	for _, bit := range []int{0, 5, 63, 64, 70} {
		m = setMaskBit(m, bit)
	}
	for _, bit := range []int{0, 5, 63, 64, 70} {
		if !maskBit(m, bit) {
			t.Errorf("maskBit(%d) = false, want set", bit)
		}
	}
	for _, bit := range []int{1, 62, 65, 71, 1000} {
		if maskBit(m, bit) {
			t.Errorf("maskBit(%d) = true, want clear", bit)
		}
	}
	// tri-state: "" never parses (unknown), "0" parses to exact-clean.
	if b, ok := parseHexMask(""); ok || b != nil {
		t.Errorf(`parseHexMask("") = %v, %v; want nil, false`, b, ok)
	}
	if b, ok := parseHexMask("0"); !ok || b == nil || len(b) != 0 {
		t.Errorf(`parseHexMask("0") = %v, %v; want non-nil empty, true`, b, ok)
	}
	if b, ok := parseHexMask("ab"); !ok || len(b) != 1 || b[0] != 0xab {
		t.Errorf(`parseHexMask("ab") = %v, %v`, b, ok)
	}
	// explicit zero survives an empty merge and gains bits from a real one.
	if got := orHexMask("0", nil); got != "0" {
		t.Errorf(`orHexMask("0", nil) = %q, want "0"`, got)
	}
	if got := orHexMask("0", setMaskBit(nil, 70)); got != "400000000000000000" {
		t.Errorf(`orHexMask("0", bit70) = %q, want 400000000000000000`, got)
	}
}

func TestCalleeFactsMutationTriState(t *testing.T) {
	fp := goobj.FingerprintType{1, 2, 3, 4, 5, 6, 7, 8}
	const path = "example.com/m/trie"
	reg := func(f *facts.Facts) {
		t.Helper()
		member := facts.Encode(fp, f)
		if _, err := facts.Import(path, "export-bytes"+string(fp[:]), member); err != nil {
			t.Fatalf("Import: %v", err)
		}
	}
	pkg := types2.NewPackage("local/pkg", "pkg")
	dep := types2.NewPackage(path, "m")
	fn := types2.NewFunc(syntax.Pos{}, dep, "F", nil)
	s := &constBodyScan{pkg: pkg}

	// Record without ParamMutated: unknown mutation, never zero.
	reg(&facts.Facts{Fns: []facts.FnFact{{Object: "F", Readonly: true}}})
	if _, mask := s.calleeFacts(fn); mask != nil {
		t.Fatalf("readonly-only record: mask = %v, want nil (unknown)", mask)
	}
	// Explicit proven zero.
	reg(&facts.Facts{Fns: []facts.FnFact{{Object: "F", HasAssert: true, AssertTP: "0", AssertRTP: "0", ParamMutated: "0"}}})
	_, mask := s.calleeFacts(fn)
	if mask == nil || len(mask) != 0 {
		t.Fatalf("ParamMutated=0: mask = %v, want exact empty", mask)
	}
	// Exact wide mask: bit 70 set, bit 0 clear.
	reg(&facts.Facts{Fns: []facts.FnFact{{Object: "F", ParamMutated: "400000000000000000"}}})
	_, mask = s.calleeFacts(fn)
	if mask == nil || !maskBit(mask, 70) || maskBit(mask, 0) {
		t.Fatalf("wide mask: mask = %v, want bit 70 only", mask)
	}
	// MayYield-only record: still unknown.
	reg(&facts.Facts{Fns: []facts.FnFact{{Object: "F", MayYield: true, HasMayYield: true}}})
	if _, mask = s.calleeFacts(fn); mask != nil {
		t.Fatalf("MayYield-only record: mask = %v, want nil (unknown)", mask)
	}
}

func TestEmitAssertEffects(t *testing.T) {
	const src = `package asserttest

type Qual interface {
	const Name() string
}

func Identity[T any](v T) T { return v }

func As[T any](x any) T { return x.(T) }

type Box[T any] struct{ V T }

func (b Box[T]) As(x any) T { return x.(T) }

type Store[T any] struct{ V T }

func (s Store[T]) Get() T { return s.V }

func (s Store[T]) Project[U any](x any) U { return x.(U) }

type Plain struct{}
func (Plain) Identity[U any](x U) U { return x }
func (Plain) As[U any](x any) U { return x.(U) }
`
	dir := t.TempDir()
	path := filepath.Join(dir, "assert.lgo")
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
		Instances:  make(map[*syntax.Name]types2.Instance),
	}
	pkg, _ := conf.Check("asserttest", []*syntax.File{file}, info)
	if err := errors.Join(checkErrs...); err != nil {
		t.Fatalf("check (definitions must stay valid): %v", err)
	}

	got := EmitAssertEffects(pkg, &facts.Facts{})
	if got == nil {
		t.Fatal("EmitAssertEffects returned nil")
	}
	fns := make(map[string]facts.FnFact)
	for _, fn := range got.Fns {
		fns[fn.Object] = fn
	}
	check := func(object, tp, rtp string) {
		t.Helper()
		fn, ok := fns[object]
		if !ok || !fn.HasAssert {
			t.Errorf("%s: missing Known assert record (have %+v)", object, fn)
			return
		}
		if fn.AssertTP != tp || fn.AssertRTP != rtp {
			t.Errorf("%s: AssertTP=%q AssertRTP=%q, want %q/%q", object, fn.AssertTP, fn.AssertRTP, tp, rtp)
		}
	}
	check("Identity", "0", "0") // proven absence keeps Identity[Qualified] legal
	check("As", "1", "0")
	check("Box.As", "0", "1")
	check("Store.Get", "0", "0")
	check("Store.Project", "1", "0")
	check("Plain.Identity", "0", "0")
	check("Plain.As", "1", "0")
}

func TestApplyFuncFactsReachablePrivateOrigin(t *testing.T) {
	pkg := types2.NewPackage("example/privatefacts", "privatefacts")
	pos := syntax.Pos{}
	any := types2.NewInterfaceType(nil, nil)
	tp := types2.NewTypeParam(types2.NewTypeName(pos, pkg, "T", nil), any)
	private := types2.NewNamed(types2.NewTypeName(pos, pkg, "private", nil), types2.NewStruct(nil, nil), nil)
	private.SetTypeParams([]*types2.TypeParam{tp})
	rp := types2.NewTypeParam(types2.NewTypeName(pos, pkg, "U", nil), any)
	recvType, err := types2.Instantiate(nil, private, []types2.Type{rp}, false)
	if err != nil {
		t.Fatal(err)
	}
	recv := types2.NewVar(pos, pkg, "self", recvType)
	method := types2.NewFunc(pos, pkg, "As", types2.NewSignatureType(recv, []*types2.TypeParam{rp}, nil, nil, nil, false))
	private.AddMethod(method)
	instance, err := types2.Instantiate(nil, private, []types2.Type{types2.Typ[types2.Int]}, false)
	if err != nil {
		t.Fatal(err)
	}
	factory := types2.NewFunc(pos, pkg, "Factory", types2.NewSignatureType(nil, nil, nil, nil,
		types2.NewTuple(types2.NewVar(pos, pkg, "", instance)), false))
	pkg.Scope().Insert(factory)
	if pkg.Scope().Lookup("private") != nil {
		t.Fatal("private type must be reachable only through Factory's result")
	}
	before := factory.Type()
	records := []facts.FnFact{{Object: "private.As", HasAssert: true, AssertTP: "0", AssertRTP: "1", Readonly: true}}
	if err := ApplyFuncFacts(pkg, records); err != nil {
		t.Fatal(err)
	}
	if pkg.Scope().Lookup("Factory") != factory || factory.Type() != before {
		t.Fatal("metadata stamping replaced canonical declaration or signature")
	}
	fp, fr, known := method.LogosAssertEffects()
	if !known || types2.LogosAssertMaskHex(fp) != "0" || types2.LogosAssertMaskHex(fr) != "1" || !method.LogosReadonly() {
		t.Fatal("reachable private method origin lost verified metadata")
	}
	cloned, err := CloneWithOverlaysAndReadonly(pkg, nil, nil, records, nil)
	if err != nil || cloned != pkg {
		t.Fatalf("declaration-only clone API replaced package: %p != %p (%v)", cloned, pkg, err)
	}
}

func TestApplyFuncFactsRejectsMaskOutsideArity(t *testing.T) {
	for _, tc := range []struct{ name, tp, rtp string }{
		{"extra function bit", "2", "0"},
		{"receiver bit on function", "0", "1"},
		{"partial pair", "0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pkg := types2.NewPackage("example/arityfacts", "arityfacts")
			pos := syntax.Pos{}
			any := types2.NewInterfaceType(nil, nil)
			tp := types2.NewTypeParam(types2.NewTypeName(pos, pkg, "T", nil), any)
			fn := types2.NewFunc(pos, pkg, "Identity", types2.NewSignatureType(nil, nil, []*types2.TypeParam{tp}, nil, nil, false))
			pkg.Scope().Insert(fn)
			err := ApplyFuncFacts(pkg, []facts.FnFact{{Object: "Identity", HasAssert: true, AssertTP: tc.tp, AssertRTP: tc.rtp}})
			if err == nil {
				t.Fatal("malformed summary accepted at import boundary")
			}
			if _, _, known := fn.LogosAssertEffects(); known {
				t.Fatal("failed validation published a trusted summary")
			}
		})
	}
}
