// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"bytes"
	"cmd/compile/internal/base"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/sys"
	"go/ast"
	"go/parser"
	"go/token"
	"internal/testenv"
	"os"
	"slices"
	"sync"
	"testing"
)

func TestBuiltin(t *testing.T) {
	testenv.MustHaveGoRun(t)
	t.Parallel()

	old, err := os.ReadFile("builtin.go")
	if err != nil {
		t.Fatal(err)
	}

	new, err := testenv.Command(t, testenv.GoToolPath(t), "run", "mkbuiltin.go", "-stdout").Output()
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(old, new) {
		t.Fatal("builtin.go out of date; run mkbuiltin.go")
	}
}

// initLogosArenaBuiltinUniverse initializes the types universe once, following
// the inlheur test pattern: runtimeTypes needs ByteType and the basic types,
// which exist only after InitUniverse.
var initLogosArenaBuiltinUniverse = sync.OnceFunc(func() {
	types.PtrSize = 8
	types.RegSize = 8
	types.MaxWidth = 1 << 50
	base.Ctxt = &obj.Link{Arch: &obj.LinkArch{Arch: &sys.Arch{Alignment: 1, CanMergeLoads: true}}}
	InitUniverse()
})

// TestLogosArenaBuiltinsDeclared pins shared contract C1, the runtime arena
// entry-point signature set. It scans runtimeDecls for the six names and
// checks each signature in runtimeTypes against the declaration text in
// _builtin/runtime.go. It must not resolve the names with LookupRuntime:
// ir.Pkgs.Runtime is populated only by a real compile, so the lookup would
// call base.Fatalf (syms.go).
func TestLogosArenaBuiltinsDeclared(t *testing.T) {
	initLogosArenaBuiltinUniverse()
	t.Parallel()

	type expectation struct {
		params  []types.Kind
		results []types.Kind
	}
	want := map[string]expectation{
		"logosArenaAlloc":       {[]types.Kind{types.TPTR, types.TUINTPTR}, []types.Kind{types.TUNSAFEPTR}},
		"logosArenaAllocScan":   {[]types.Kind{types.TPTR, types.TUINTPTR}, []types.Kind{types.TUNSAFEPTR}},
		"logosArenaMarkSave":    {nil, []types.Kind{types.TUINTPTR, types.TUINTPTR, types.TUINTPTR}},
		"logosArenaMarkRestore": {[]types.Kind{types.TUINTPTR, types.TUINTPTR, types.TUINTPTR}, nil},
		"logosArenaRetTake":     {nil, []types.Kind{types.TBOOL}},
		"logosArenaRetSet":      {[]types.Kind{types.TBOOL}, nil},
	}

	byType := make(map[string]int, len(runtimeDecls))
	for _, d := range runtimeDecls {
		if d.tag != funcTag {
			continue
		}
		byType[d.name] = d.typ
	}
	sigs := runtimeTypes()
	checkKind := func(t *testing.T, name string, what string, got *types.Type, kind types.Kind) {
		t.Helper()
		if got.Kind() != kind {
			t.Fatalf("%s %s has kind %v, want %v", name, what, got.Kind(), kind)
		}
		if kind == types.TPTR && got.Elem() != types.ByteType {
			t.Fatalf("%s %s points at %v, want byte", name, what, got.Elem())
		}
	}
	for name, exp := range want {
		idx, ok := byType[name]
		if !ok {
			t.Fatalf("runtime.%s missing from runtimeDecls", name)
		}
		if idx < 0 || idx >= len(sigs) {
			t.Fatalf("runtime.%s has type index %d out of range", name, idx)
		}
		sig := sigs[idx]
		if sig.NumParams() != len(exp.params) || sig.NumResults() != len(exp.results) {
			t.Fatalf("runtime.%s has %d params and %d results, declaration wants %d and %d",
				name, sig.NumParams(), sig.NumResults(), len(exp.params), len(exp.results))
		}
		for i, kind := range exp.params {
			checkKind(t, name, "param", sig.Param(i).Type, kind)
		}
		for i, kind := range exp.results {
			checkKind(t, name, "result", sig.Result(i).Type, kind)
		}
	}

	// Cross-check against the declaration text: the table above must match
	// what _builtin/runtime.go actually declares, so editing one side
	// without regenerating fails here as well as in TestBuiltin.
	src, err := os.ReadFile("_builtin/runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "_builtin/runtime.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	typeExpr := func(e ast.Expr) string {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.StarExpr:
			if id, ok := e.X.(*ast.Ident); ok {
				return "*" + id.Name
			}
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok {
				return id.Name + "." + e.Sel.Name
			}
		}
		return ""
	}
	flat := func(fl *ast.FieldList) []string {
		var out []string
		if fl == nil {
			return out
		}
		for _, f := range fl.List {
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			for range n {
				out = append(out, typeExpr(f.Type))
			}
		}
		return out
	}
	// Expected declaration spellings in _builtin/runtime.go: param types
	// first, result types second. Compared as strings so a uintptr-to-int
	// edit fails even though the arity is unchanged.
	text := map[string][2][]string{
		"logosArenaAlloc":       {{"*byte", "uintptr"}, {"unsafe.Pointer"}},
		"logosArenaAllocScan":   {{"*byte", "uintptr"}, {"unsafe.Pointer"}},
		"logosArenaMarkSave":    {nil, {"uintptr", "uintptr", "uintptr"}},
		"logosArenaMarkRestore": {{"uintptr", "uintptr", "uintptr"}, nil},
		"logosArenaRetTake":     {nil, {"bool"}},
		"logosArenaRetSet":      {{"bool"}, nil},
	}
	seen := make(map[string]bool, len(want))
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		exp, ok := text[fn.Name.Name]
		if !ok {
			continue
		}
		seen[fn.Name.Name] = true
		params, results := flat(fn.Type.Params), flat(fn.Type.Results)
		if !slices.Equal(params, exp[0]) || !slices.Equal(results, exp[1]) {
			t.Fatalf("runtime.%s declares (%v) (%v), want (%v) (%v)",
				fn.Name.Name, params, results, exp[0], exp[1])
		}
	}
	for name := range want {
		if !seen[name] {
			t.Fatalf("runtime.%s missing from _builtin/runtime.go", name)
		}
	}
}
