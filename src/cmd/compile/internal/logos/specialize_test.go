// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"cmd/compile/internal/base"
	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
	"internal/testenv"
)

const specPrelude = `package p

type Reader interface{ Read(b []byte) int }
type Writer interface{ Write(b []byte) int }
type Flusher interface{ Flush() int }

type buf struct{ n int }

func (b *buf) Read(p []byte) int  { return len(p) }
func (b *buf) Write(p []byte) int { b.n += len(p); return len(p) }
func (b *buf) Flush() int         { return b.n }

type plain struct{ n int }

func (p plain) Read(b []byte) int  { return p.n }
func (p plain) Write(b []byte) int { return len(b) }

`

// specSources parses and checks the files of one package.
func specSources(t *testing.T, imp types2.Importer, srcs ...string) ([]*syntax.File, *types2.Package, *types2.Info) {
	t.Helper()
	var files []*syntax.File
	for i, src := range srcs {
		name := fmt.Sprintf("spec%d.go", i)
		f, err := syntax.Parse(syntax.NewFileBase(name), strings.NewReader(src), func(err error) { t.Fatalf("parse %s: %v", name, err) }, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	pkg, info := specCheck(t, imp, files)
	return files, pkg, info
}

// specCheck type-checks files the way the compiler front end does, with
// types stored in the syntax and no Info.Types map.
func specCheck(t *testing.T, imp types2.Importer, files []*syntax.File) (*types2.Package, *types2.Info) {
	t.Helper()
	info := &types2.Info{
		StoreTypesInSyntax: true,
		Defs:               make(map[*syntax.Name]types2.Object),
		Uses:               make(map[*syntax.Name]types2.Object),
		Selections:         make(map[*syntax.SelectorExpr]*types2.Selection),
		Implicits:          make(map[syntax.Node]types2.Object),
		Scopes:             make(map[syntax.Node]*types2.Scope),
		Instances:          make(map[*syntax.Name]types2.Instance),
	}
	var errs []error
	conf := types2.Config{Importer: imp, Error: func(err error) { errs = append(errs, err) }}
	pkg, _ := conf.Check("p", files, info)
	if len(errs) > 0 {
		t.Fatalf("check: %v", errs)
	}
	return pkg, info
}

// specResult is what one Specialize run left behind.
type specResult struct {
	changed bool
	funcs   []string // function names of the first file in declaration order
	text    string   // every file printed on one line
}

// clones returns the clone names, in order.
func (r specResult) clones() []string {
	var out []string
	for _, n := range r.funcs {
		if strings.Contains(n, specSuffix) {
			out = append(out, n)
		}
	}
	return out
}

// hasClone reports whether f has a clone.
func (r specResult) hasClone(f string) bool {
	for _, n := range r.clones() {
		if strings.HasPrefix(n, f+specSuffix) {
			return true
		}
	}
	return false
}

// specRun specializes one package and rechecks the result, which must type
// check as ordinary Go.
func specRun(t *testing.T, imp types2.Importer, costCap int, srcs ...string) specResult {
	t.Helper()
	files, pkg, info := specSources(t, imp, srcs...)
	var r specResult
	r.changed = Specialize(pkg, info, files, costCap)
	var buf bytes.Buffer
	for _, f := range files {
		for _, d := range f.DeclList {
			if fd, ok := d.(*syntax.FuncDecl); ok && f == files[0] {
				r.funcs = append(r.funcs, fd.Name.Value)
			}
		}
		if _, err := syntax.Fprint(&buf, f, syntax.LineForm); err != nil {
			t.Fatal(err)
		}
		buf.WriteByte('\n')
	}
	r.text = buf.String()
	if r.changed {
		specCheck(t, imp, files)
	}
	return r
}

func runSpecializedGo(t *testing.T, source string) string {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	source = strings.Replace(source, "package p", "package main", 1)
	source = strings.ReplaceAll(source, specSuffix, "Spec")
	source += "\nfunc main() { println(use()) }\n"
	if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(testenv.GoToolPath(t), "run", path).CombinedOutput()
	if err != nil {
		t.Fatalf("run specialized fixture: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSpecializeBasic(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
func Copy(w Writer, r Reader) int { return w.Write(nil) + r.Read(nil) }
func use() int { b := &buf{}; return Copy(b, b) }
`)
	if !r.changed || len(r.clones()) != 1 || r.clones()[0] != "Copy·logosspec1" {
		t.Fatalf("clones = %v\n%s", r.clones(), r.text)
	}
	for _, want := range []string{
		"func Copy·logosspec1(w *buf, r *buf) int",
		"return Copy·logosspec1(b, b)",
		"func Copy(w Writer, r Reader) int",
	} {
		if !strings.Contains(r.text, want) {
			t.Errorf("missing %q in\n%s", want, r.text)
		}
	}
}

func TestSpecializeBlocked(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"reassigned", `func F(w Writer) int { w = &buf{}; return w.Write(nil) }`},
		{"storedInField", `type holder struct{ w Writer }
func F(w Writer) *holder { h := &holder{w: w}; w.Write(nil); return h }`},
		{"addressTaken", `func F(w Writer) int { p := &w; return (*p).Write(nil) }`},
		{"copiedToLocal", `func F(w Writer) int { x := w; return x.Write(nil) }`},
		{"nilComparedAlias", `type WriterAlias = Writer
func F(w WriterAlias) int { if w == nil { return 0 }; return w.Write(nil) }`},
		{"capturedByClosure", `func F(w Writer) int { f := func() int { return w.Write(nil) }; return f() }`},
		{"deferred", `func F(w Writer) int { defer w.Write(nil); return 0 }`},
		{"spawned", `func F(w Writer) int { go w.Write(nil); return 0 }`},
		{"genericPassOn", `func g[T any](x T) int { return 0 }
func F(w Writer) int { return g(w) + w.Write(nil) }`},
		{"builtinPassOn", `func F(w Writer) int { println(w); return w.Write(nil) }`},
		{"methodValue", `func F(w Writer) int { f := w.Write; return f(nil) }`},
		{"localType", `func F(w Writer) int { type t struct{}; return w.Write(nil) }`},
		{"noInterfaceUse", `func F(w Writer) int { return 1 }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := specRun(t, nil, 0, specPrelude+c.body+`
func use() { _ = F(&buf{}) }
`)
			if len(r.clones()) != 0 {
				t.Fatalf("clones = %v\n%s", r.clones(), r.text)
			}
		})
	}
}

func TestSpecializeNilCompare(t *testing.T) {
	r := specRun(t, nil, 0, `package p

type Writer interface{ Write([]byte) int }
type node struct{}
func (*node) Write([]byte) int { return 5 }

func F(w Writer) int { if w == nil { return 1 }; return w.Write(nil) }
var p *node
func use() int {
	if F(nil) != 1 {
		return 1
	}
	if F(p) != 5 {
		return 2
	}
	if F(&node{}) != 5 {
		return 3
	}
	return 0
}
`)
	if len(r.clones()) != 0 {
		t.Fatalf("parameter compared with nil must block specialization: clones = %v\n%s", r.clones(), r.text)
	}
	if got := runSpecializedGo(t, r.text); got != "0" {
		t.Fatalf("nil comparison behavior changed: got %q, want 0\n%s", got, r.text)
	}
}

func TestSpecializeNilCompareParenthesized(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
func F(w Writer) int {
	if (w) == nil {
		return 0
	}
	return w.Write(nil)
}
func use() int { return F(plain{}) }
`)
	if len(r.clones()) != 0 {
		t.Fatalf("parenthesized nil comparison must block specialization: clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeValueMethodViaPointer(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
func F(w Writer) int { return w.Write(nil) }
func use() int { return F(&plain{}) }
`)
	if len(r.clones()) != 0 {
		t.Fatalf("a nil *plain panics differently through the interface than through the dereference: clones = %v\n%s", r.clones(), r.text)
	}
	r = specRun(t, nil, 0, specPrelude+`
func F(w Writer) int { return w.Write(nil) }
func use() int { return F(plain{}) }
`)
	if len(r.clones()) != 1 {
		t.Fatalf("a plain value reaches its value method directly: clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeInterfaceArgument(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
func F(w Writer) int { return w.Write(nil) }
func use() int { var w Writer = &buf{}; return F(w) + F(nil) }
`)
	if len(r.clones()) != 0 {
		t.Fatalf("clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeFunctionValue(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
func F(w Writer) int { return w.Write(nil) }
var fn func(Writer) int = F
func use() int { return F(&buf{}) + fn(&buf{}) }
`)
	if len(r.clones()) != 1 {
		t.Fatalf("clones = %v\n%s", r.clones(), r.text)
	}
	for _, want := range []string{"var fn func(Writer) int = F", "F·logosspec1(&buf{})", "fn(&buf{})"} {
		if !strings.Contains(r.text, want) {
			t.Errorf("missing %q in\n%s", want, r.text)
		}
	}
}

func TestSpecializeAssertions(t *testing.T) {
	src := `package p

type Writer interface{ Write([]byte) int }
type Flusher interface{ Flush() int }

type both struct{}
func (both) Write([]byte) int { return 2 }
func (both) Flush() int { return 3 }

type plain struct{}
func (plain) Write([]byte) int { return 5 }

func F(w Writer) int {
	false := true
	if !false {
		return 7
	}
	if f, ok := w.(Flusher); ok {
		return f.Flush()
	}
	return w.Write(nil)
}
func use() int { return F(both{})*10 + F(plain{}) }
`
	r := specRun(t, nil, 0, src)
	if !strings.Contains(r.text, "0 == 0") || !strings.Contains(r.text, "0 != 0") {
		t.Fatalf("comma-ok assertion fold lost shadow-safe constants:\n%s", r.text)
	}
	if got := runSpecializedGo(t, r.text); got != "35" {
		t.Fatalf("comma-ok assertion behavior changed: got %q, want 35\n%s", got, r.text)
	}

	r = specRun(t, nil, 0, `package p

type Reader interface{ Read([]byte) int }
type Writer interface{ Write([]byte) int }
type Flusher interface{ Flush() int }

type full struct{}
func (full) Read([]byte) int { return 2 }
func (full) Write([]byte) int { return 3 }
func (full) Flush() int { return 5 }

func G(w Writer) int {
	f := w.(Flusher)
	switch v := w.(type) {
	case Reader:
		return v.Read(nil)
	}
	return f.Flush() + w.Write(nil)
}
func use() int { return G(full{}) }
`)
	if got := runSpecializedGo(t, r.text); got != "2" {
		t.Fatalf("assertion/type-switch behavior changed: got %q, want 2\n%s", got, r.text)
	}
}

func TestSpecializeChainAndRecursion(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
func Outer(w Writer) int { return w.Write(nil) + Inner(w) }
func Inner(w Writer) int { return w.Write(nil) }
func Walk(w Writer, n int) int {
	if n == 0 {
		return w.Write(nil)
	}
	return Walk(w, n-1)
}
func use() int { return Outer(&buf{}) + Walk(&buf{}, 3) }
`)
	if len(r.clones()) != 3 {
		t.Fatalf("clones = %v\n%s", r.clones(), r.text)
	}
	for _, want := range []string{"Inner·logosspec3(w)", "return Walk·logosspec2(w, "} {
		if !strings.Contains(r.text, want) {
			t.Errorf("missing %q in\n%s", want, r.text)
		}
	}
}

func TestSpecializeIndependentCallSites(t *testing.T) {
	for _, n := range []int{3, 4, 5, 6} {
		var src strings.Builder
		src.WriteString(specPrelude + "func F(w Writer) int { return w.Write(nil) }\nfunc use() int { return 0")
		for i := range n {
			fmt.Fprintf(&src, " + F(&mono%d{})", i)
		}
		src.WriteString(" }\n")
		for i := range n {
			fmt.Fprintf(&src, "type mono%d struct{}\nfunc (*mono%d) Write(b []byte) int { return %d }\n", i, i, i)
		}
		r := specRun(t, nil, 0, src.String())
		if got := len(r.clones()); got != n {
			t.Errorf("%d independent call-site types: %d clones, want %d\n%s", n, got, n, r.text)
		}
	}
}

func TestSpecializeCallSiteCap(t *testing.T) {
	var src strings.Builder
	src.WriteString(specPrelude + "func F(w Writer) int { return w.Write(nil) }\nfunc Inner(w Writer) int { return F(w) }\nfunc use() int { return 0")
	for i := range 5 {
		fmt.Fprintf(&src, " + Inner(&mono%d{})", i)
	}
	src.WriteString(" }\n")
	for i := range 5 {
		fmt.Fprintf(&src, "type mono%d struct{}\nfunc (*mono%d) Write(b []byte) int { return %d }\n", i, i, i)
	}
	r := specRun(t, nil, 0, src.String())
	if !r.hasClone("Inner") || r.hasClone("F") {
		t.Fatalf("the capped nested site must not discard independent outer clones: clones = %v\n%s", r.clones(), r.text)
	}
	innerClones := 0
	for _, name := range r.clones() {
		if strings.HasPrefix(name, "Inner"+specSuffix) {
			innerClones++
		}
	}
	if innerClones != 5 {
		t.Fatalf("got %d Inner clones, want 5: %v\n%s", innerClones, r.clones(), r.text)
	}
	for i := 1; i <= 5; i++ {
		if !strings.Contains(r.text, fmt.Sprintf("func Inner·logosspec%d", i)) {
			t.Errorf("missing outer clone %d:\n%s", i, r.text)
		}
	}
}

func TestSpecializeFiveConcreteArguments(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
type m0 struct{}
type m1 struct{}
type m2 struct{}
type m3 struct{}
type m4 struct{}
func (m0) Write([]byte) int { return 0 }
func (m1) Write([]byte) int { return 1 }
func (m2) Write([]byte) int { return 2 }
func (m3) Write([]byte) int { return 3 }
func (m4) Write([]byte) int { return 4 }
func F(a, b, c, d, e Writer) int {
	return a.Write(nil) + b.Write(nil) + c.Write(nil) + d.Write(nil) + e.Write(nil)
}
func use() int { return F(m0{}, m1{}, m2{}, m3{}, m4{}) }
`)
	if r.hasClone("F") {
		t.Fatalf("a call with five concrete argument types must stay interface: clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeTypeDepth(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
type Named interface{ Name() string }
type Leaf struct{}
func (Leaf) Name() string { return "leaf" }
type Wrap[T Named] struct{ in T }
func (w Wrap[T]) Name() string { return "w" + w.in.Name() }
func Show2(n Named) string { return n.Name() }
func Show3(n Named) string { return n.Name() }
func Show4(n Named) string { return n.Name() }
func Show5(n Named) string { return n.Name() }
func use() string {
	return Show2(Wrap[Leaf]{}) + Show3(Wrap[Wrap[Leaf]]{}) + Show4(Wrap[Wrap[Wrap[Leaf]]]{}) + Show5(Wrap[Wrap[Wrap[Wrap[Leaf]]]]{})
}
`)
	for f, want := range map[string]bool{"Show2": true, "Show3": true, "Show4": true, "Show5": false} {
		if got := r.hasClone(f); got != want {
			t.Errorf("%s clone = %v, want %v\n%s", f, got, want, r.text)
		}
	}
	if !strings.Contains(r.text, "n Wrap[Wrap[Wrap[Leaf]]]") {
		t.Errorf("depth four clone is not typed with the instantiation\n%s", r.text)
	}
}

func TestSpecializeTypeDepthPointer(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
type Named interface{ Name() string }
type Leaf struct{}
func (*Leaf) Name() string { return "leaf" }
type Wrap[T Named] struct{ in T }
func (w *Wrap[T]) Name() string { return "w" + w.in.Name() }
func Show2(n Named) string { return n.Name() }
func Show4(n Named) string { return n.Name() }
func Show6(n Named) string { return n.Name() }
func use() string {
	return Show2(&Leaf{}) + Show4(&Wrap[*Leaf]{}) + Show6(&Wrap[*Wrap[*Leaf]]{})
}
`)
	for f, want := range map[string]bool{"Show2": true, "Show4": true, "Show6": false} {
		if got := r.hasClone(f); got != want {
			t.Errorf("%s clone = %v, want %v\n%s", f, got, want, r.text)
		}
	}
}

// specCostSource builds a callee whose body has exactly the requested node
// count, or "" when the shape cannot reach it. Each x++ adds two nodes and
// each x = 0 adds three.
func specCostSource(t *testing.T, want int) string {
	t.Helper()
	build := func(twos, threes int) (string, int) {
		src := specPrelude + "func F(w Writer) int {\n\tx := 0\n" +
			strings.Repeat("\tx++\n", twos) + strings.Repeat("\tx = 0\n", threes) +
			"\treturn w.Write(nil) + x\n}\nfunc use() int { return F(&buf{}) }\n"
		files, pkg, info := specSources(t, nil, src)
		s := &specializer{pkg: pkg, info: info, files: files, funcs: make(map[*types2.Func]*specFunc)}
		s.collect()
		if len(s.order) != 1 {
			t.Fatalf("collected %d candidates, want 1", len(s.order))
		}
		return src, s.cost(s.order[0])
	}
	_, floor := build(0, 0)
	extra := want - floor
	if extra < 0 {
		t.Fatalf("cost %d is below the %d nodes of the smallest callee", want, floor)
	}
	threes := extra % 2
	if extra < 3*threes {
		t.Fatalf("cost %d is not reachable from %d nodes", want, floor)
	}
	src, got := build((extra-3*threes)/2, threes)
	if got != want {
		t.Fatalf("callee has %d nodes, want %d", got, want)
	}
	return src
}

func TestSpecializeCostCap(t *testing.T) {
	for _, tc := range []struct {
		cost int
		want bool
	}{
		{specO2CostCap - 1, true},
		{specO2CostCap, true},
		{specO2CostCap + 1, false},
	} {
		src := specCostSource(t, tc.cost)
		if src == "" {
			t.Fatalf("no callee with %d nodes", tc.cost)
		}
		if got := len(specRun(t, nil, specO2CostCap, src).clones()) == 1; got != tc.want {
			t.Errorf("cost %d at the capped tier: clone = %v, want %v", tc.cost, got, tc.want)
		}
		if got := len(specRun(t, nil, 0, src).clones()) == 1; !got {
			t.Errorf("cost %d with no cap: no clone", tc.cost)
		}
	}
}

func TestSpecializeCrossFileImport(t *testing.T) {
	imp := newTestImporter(t)
	r := specRun(t, imp, 0, `package p

import "io"

func Fill(w io.Writer) { w.Write(nil) }
`, `package p

import "bytes"

func use() {
	var b bytes.Buffer
	Fill(&b)
}
`)
	if !r.hasClone("Fill") {
		t.Fatalf("no clone\n%s", r.text)
	}
	if !strings.Contains(r.text, `import _logosspec1 "bytes"`) || !strings.Contains(r.text, "w *_logosspec1.Buffer") {
		t.Errorf("clone is not typed through an alias import\n%s", r.text)
	}
}

// specRTSource stands in for logos/rt: the specializer recognizes the
// conversion call by package path and function name.
const specRTSource = `package rt

func CovSlice[I any, E any](s []E) []I {
	if s == nil {
		return nil
	}
	out := make([]I, len(s))
	for i, e := range s {
		out[i] = any(e).(I)
	}
	return out
}
`

// specRTImporter resolves logos/rt to the stand-in package.
type specRTImporter struct{ rt *types2.Package }

func (m specRTImporter) Import(path string) (*types2.Package, error) {
	if path != "logos/rt" {
		return nil, fmt.Errorf("unknown import %q", path)
	}
	return m.rt, nil
}

func newSpecRTImporter(t *testing.T) types2.Importer {
	t.Helper()
	f, err := syntax.Parse(syntax.NewFileBase("rt.go"), strings.NewReader(specRTSource), func(err error) { t.Fatal(err) }, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	conf := types2.Config{Error: func(err error) { t.Fatal(err) }}
	pkg, err := conf.Check("logos/rt", []*syntax.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return specRTImporter{rt: pkg}
}

const specCreatures = `package p

import _logos_rt "logos/rt"

type Creature interface{ Name() string }
type Monster struct{ n string }

func (m *Monster) Name() string { return m.n }

type Rock struct{}

func (Rock) Name() string { return "rock" }

`

func TestSpecializeConstSlice(t *testing.T) {
	imp := newSpecRTImporter(t)
	r := specRun(t, imp, 0, specCreatures+`
func Names(cs []Creature) string {
	s := ""
	for _, c := range cs {
		s += c.Name()
	}
	return s + cs[0].Name()
}
func use(ms []*Monster) string { return Names(_logos_rt.CovSlice[Creature](ms)) }
`)
	if !r.hasClone("Names") {
		t.Fatalf("no clone\n%s", r.text)
	}
	for _, want := range []string{
		"func Names·logosspec1(cs []*Monster) string",
		"return Names·logosspec1(ms)",
		`import _ "logos/rt"`,
		"func Names(cs []Creature) string",
	} {
		if !strings.Contains(r.text, want) {
			t.Errorf("missing %q in\n%s", want, r.text)
		}
	}
	if strings.Contains(r.text, "CovSlice") {
		t.Errorf("the conversion survived\n%s", r.text)
	}
}

func TestSpecializeConstSliceKeepsImportInUse(t *testing.T) {
	imp := newSpecRTImporter(t)
	r := specRun(t, imp, 0, specCreatures+`
func Names(cs []Creature) string {
	s := ""
	for _, c := range cs {
		s += c.Name()
	}
	return s
}
func use(ms []*Monster) string { return Names(_logos_rt.CovSlice[Creature](ms)) }
func keep(ms []*Monster) []Creature { return _logos_rt.CovSlice[Creature](ms) }
`)
	if !r.hasClone("Names") || !strings.Contains(r.text, `import _logos_rt "logos/rt"`) {
		t.Fatalf("clone = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeConstSliceElementsMustImplement(t *testing.T) {
	imp := newSpecRTImporter(t)
	names := `
func Names(cs []Creature) string {
	s := ""
	for _, c := range cs {
		s += c.Name()
	}
	return s
}
`
	for name, use := range map[string]string{
		"valueLacksPointerMethods": `func use(ms []Monster) string { return Names(_logos_rt.CovSlice[Creature](ms)) }`,
		"unrelatedElement":         `func use(xs []int) string { return Names(_logos_rt.CovSlice[Creature](xs)) }`,
		"interfaceElement":         `func use(xs []Creature) string { return Names(_logos_rt.CovSlice[Creature](xs)) }`,
		"plainSliceArgument":       `func use(xs []Creature) string { _ = _logos_rt.CovSlice[Creature, Creature]; return Names(xs) }`,
		"valueMethodViaPointer":    `func use(rs []*Rock) string { return Names(_logos_rt.CovSlice[Creature](rs)) }`,
	} {
		t.Run(name, func(t *testing.T) {
			r := specRun(t, imp, 0, specCreatures+names+use)
			if len(r.clones()) != 0 {
				t.Fatalf("clones = %v\n%s", r.clones(), r.text)
			}
		})
	}
}

func TestSpecializeConstSliceBlocked(t *testing.T) {
	imp := newSpecRTImporter(t)
	for name, body := range map[string]string{
		"elementNilCompare": `func Names(cs []Creature) int { n := 0; for _, c := range cs { if c == nil { n++ }; n += len(c.Name()) }; return n }`,
		"elementAssert":     `func Names(cs []Creature) int { n := 0; for _, c := range cs { if _, ok := c.(interface{ Hit() }); ok { n++ }; n += len(c.Name()) }; return n }`,
		"elementCopied":     `func Names(cs []Creature) int { n := 0; for _, c := range cs { d := c; n += len(d.Name()) }; return n }`,
		"elementStored":     `func Names(cs []Creature) []Creature { var out []Creature; for _, c := range cs { out = append(out, c) }; _ = cs[0].Name(); return out }`,
		"sliceStored": `var kept []Creature
func Names(cs []Creature) int { kept = cs; return len(cs[0].Name()) }`,
		"sliceResliced": `func Names(cs []Creature) int { return len(cs[1:][0].Name()) }`,
		"slicePassedOn": `func other(cs []Creature) int { return len(cs[0].Name()) }
func Names(cs []Creature) int { return other(cs) + len(cs[0].Name()) }`,
		"sliceCompared": `func Names(cs []Creature) int { if cs == nil { return 0 }; return len(cs[0].Name()) }`,
		"elementStore":  `func Names(cs []Creature) int { cs[0] = nil; return len(cs[1].Name()) }`,
		"noMethodUse":   `func Names(cs []Creature) int { return len(cs) }`,
	} {
		t.Run(name, func(t *testing.T) {
			r := specRun(t, imp, 0, specCreatures+body+`
func use(ms []*Monster) { _ = Names(_logos_rt.CovSlice[Creature](ms)) }
`)
			if len(r.clones()) != 0 {
				t.Fatalf("clones = %v\n%s", r.clones(), r.text)
			}
		})
	}
}

func TestSpecializeEmbeddedMethod(t *testing.T) {
	r := specRun(t, nil, 0, specPrelude+`
type outer struct{ plain }

func F(w Writer) int { return w.Write(nil) }
func use() { _ = F(&outer{}) }
`)
	if len(r.clones()) != 1 {
		t.Fatalf("a promoted method never panics with the wrapper's text: clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeLogosNonNilPointer(t *testing.T) {
	defer func(v bool) { base.Logos = v }(base.Logos)
	base.Logos = true
	r := specRun(t, nil, 0, specPrelude+`
func F(w Writer) int { return w.Write(nil) }
func use() { _ = F(&plain{}) }
`)
	if len(r.clones()) != 1 {
		t.Fatalf("a loGo/s *T of a local type is never nil: clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeConstSliceExplicitTypeArgs(t *testing.T) {
	imp := newSpecRTImporter(t)
	r := specRun(t, imp, 0, specCreatures+`
func Names(cs []Creature) string {
	s := ""
	for _, c := range cs {
		s += c.Name()
	}
	return s
}
func use(ms []*Monster) string { return Names(_logos_rt.CovSlice[Creature, *Monster](ms)) }
`)
	if !r.hasClone("Names") || strings.Contains(r.text, "CovSlice") {
		t.Fatalf("clones = %v\n%s", r.clones(), r.text)
	}
}

func TestSpecializeGate(t *testing.T) {
	defer func(spec, budget int, eject, mode string, runtime bool) {
		base.Debug.LogosSpecialize, base.Debug.InlBudget = spec, budget
		base.LogosEject, base.LogosMode, base.Flag.CompilingRuntime = eject, mode, runtime
	}(base.Debug.LogosSpecialize, base.Debug.InlBudget, base.LogosEject, base.LogosMode, base.Flag.CompilingRuntime)

	base.Debug.LogosSpecialize, base.LogosEject, base.LogosMode, base.Flag.CompilingRuntime = 1, "", "", false
	if !SpecializeEnabled() {
		t.Fatal("flag set: specialization must run")
	}
	base.Debug.LogosSpecialize = 0
	if SpecializeEnabled() {
		t.Fatal("flag clear: specialization must not run")
	}
	base.Debug.LogosSpecialize = 1
	base.LogosEject = t.TempDir()
	if SpecializeEnabled() {
		t.Fatal("eject output must stay plain Go: specialization must not run")
	}
	base.LogosEject, base.LogosMode = "", "hints"
	if SpecializeEnabled() {
		t.Fatal("helper modes must not specialize")
	}
	base.LogosMode, base.Flag.CompilingRuntime = "", true
	if SpecializeEnabled() {
		t.Fatal("the runtime must not specialize")
	}

	for budget, want := range map[int]int{0: specO2CostCap, 120: specO2CostCap, 399: specO2CostCap, 400: 0, 4096: 0} {
		base.Debug.InlBudget = budget
		if got := SpecializeCostCap(); got != want {
			t.Errorf("inline budget %d: cost cap %d, want %d", budget, got, want)
		}
	}
}
