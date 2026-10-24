// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// lowerStaticText type-checks and lowers one .lgo source and returns the
// ejected text, which carries every generated declaration.
func lowerStaticText(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.lgo")
	if err := os.WriteFile(path, []byte(src), 0666); err != nil {
		t.Fatal(err)
	}
	var perrs []error
	f, err := syntax.ParseFile(path, func(err error) { perrs = append(perrs, err) }, nil, syntax.CheckBranches|syntax.LogosMode)
	if err != nil {
		t.Fatal(errors.Join(append(perrs, err)...))
	}
	info := &types2.Info{
		Defs:       make(map[*syntax.Name]types2.Object),
		Uses:       make(map[*syntax.Name]types2.Object),
		Types:      make(map[syntax.Expr]types2.TypeAndValue),
		Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
		Scopes:     make(map[syntax.Node]*types2.Scope),
	}
	var cerrs []error
	imp := &testImporter{pkgs: make(map[string]*types2.Package), busy: make(map[string]bool)}
	conf := types2.Config{
		Importer: imp,
		Error: func(err error) {
			if !types2.LogosWarnOnly(err) {
				cerrs = append(cerrs, err)
			}
		},
		Logos: true,
	}
	files := []*syntax.File{f}
	pkg, _ := conf.Check("main", files, info)
	if err := errors.Join(cerrs...); err != nil {
		t.Fatalf("type check: %v", err)
	}
	data := []byte(src)
	_, edits := Lower(pkg, info, files, map[*syntax.File][]byte{f: data})
	out, err := ApplyEdits(data, f.Pos().FileBase(), edits[f])
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	return string(out)
}

// methodBody returns the text of one generated function, from its func
// line to its closing brace line, or fails the test.
func methodBody(t *testing.T, text, head string) string {
	t.Helper()
	start := strings.Index(text, head)
	if start < 0 {
		t.Fatalf("generated text has no %q", head)
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("function %q has no closing brace", head)
	}
	return text[start : start+end+2]
}

const userSource = "package p\n\ntype User struct {\n" +
	"\tID   int64    `json:\"id\"`\n" +
	"\tName string   `json:\"name\"`\n" +
	"\tRole *string? `json:\"role,omitempty\"`\n}\n"

// TestUserFixedBytes pins the literals the generator emits for the spec
// User shape: {"id": (6 bytes), ,"name":" (9) plus " (1), optional
// ,"role":" (9) plus " (1), and } (1): 17 fixed bytes without the
// optional field and 27 with it.
func TestUserFixedBytes(t *testing.T) {
	text := lowerStaticText(t, userSource)
	body := methodBody(t, text, "func (_logos_v User) MarshalJSON() ([]byte, error) {")
	for _, want := range []string{
		"_logos_buf = append(_logos_buf, `{\"id\":`...)",
		"_logos_buf = append(_logos_buf, `,\"name\":\"`...)",
		"_logos_buf = append(_logos_buf, `,\"role\":\"`...)",
		"_logos_buf = append(_logos_buf, '}')",
		"_logos_n := 17",
		"_logos_n += 10",
		"_logos_buf := make([]byte, 0, _logos_n)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MarshalJSON lacks %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, "_logos_buf = append(_logos_buf, '\"')"); got != 2 {
		t.Errorf("closing quotes: got %d, want 2 (name and role)", got)
	}
	if n := len(`{"id":`) + len(`,"name":"`) + 1 + 1; n != 17 {
		t.Fatalf("fixed bytes without role = %d, want 17", n)
	}
	if n := 17 + len(`,"role":"`) + 1; n != 27 {
		t.Fatalf("fixed bytes with role = %d, want 27", n)
	}
	for _, banned := range []string{"reflect", "MarshalField", "OmitEmpty", "OmitZero"} {
		if strings.Contains(body, banned) {
			t.Errorf("fast path of User mentions %q:\n%s", banned, body)
		}
	}
}

// TestCopyDeclarationKeepsReflectivePath checks that `type M T` has no
// generated methods, so a test can compare T with its reflective twin.
func TestCopyDeclarationKeepsReflectivePath(t *testing.T) {
	text := lowerStaticText(t, userSource+"\ntype mirror User\n")
	if strings.Contains(text, "func (_logos_v mirror)") || strings.Contains(text, "func (_logos_v *mirror)") {
		t.Fatalf("copy declaration got generated methods:\n%s", text)
	}
	if !strings.Contains(text, "func (_logos_v User) MarshalJSON()") {
		t.Fatalf("User lost its generated method:\n%s", text)
	}
}

// TestUnsupportedKindsSkipGeneration covers the three skips of SPEC Sec
// 12.1: interface fields, maps with non-string keys, embedded structs.
func TestUnsupportedKindsSkipGeneration(t *testing.T) {
	cases := map[string]string{
		"interface": "type S struct {\n\tA any `json:\"a\"`\n}\n",
		"intkeymap": "type S struct {\n\tM map[int]string `json:\"m\"`\n}\n",
		"embedded":  "type In struct{ X int `json:\"x\"` }\ntype S struct {\n\tIn\n\tY int `json:\"y\"`\n}\n",
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			text := lowerStaticText(t, "package p\n\n"+decl)
			if strings.Contains(text, "func (_logos_v S)") || strings.Contains(text, "func (_logos_v *S)") {
				t.Fatalf("type S was generated:\n%s", text)
			}
		})
	}
}

// requiredStruct declares a struct of n required non-nil pointer fields.
func requiredStruct(n int) string {
	var b strings.Builder
	b.WriteString("package p\n\ntype R struct {\n")
	for i := range n {
		fmt.Fprintf(&b, "\tF%d *int `json:\"f%d\"`\n", i, i)
	}
	b.WriteString("}\n")
	return b.String()
}

// TestSeenMask checks the mask sizing and the per-key bit set of the
// generated decoder: one word for up to 64 required fields, two for 65.
func TestSeenMask(t *testing.T) {
	for _, tc := range []struct {
		n     int
		words int
	}{{1, 1}, {64, 1}, {65, 2}} {
		t.Run(fmt.Sprint(tc.n), func(t *testing.T) {
			text := lowerStaticText(t, requiredStruct(tc.n))
			body := methodBody(t, text, "func (_logos_v *R) UnmarshalJSON(_logos_data []byte) error {")
			if want := fmt.Sprintf("var _logos_seen [%d]uint64", tc.words); !strings.Contains(body, want) {
				t.Errorf("decoder lacks %q:\n%s", want, body)
			}
			for _, i := range []int{0, tc.n - 1} {
				want := fmt.Sprintf("_logos_seen[%d] |= 1 << %d", i/64, i%64)
				if !strings.Contains(body, want) {
					t.Errorf("decoder lacks %q for key %d", want, i)
				}
			}
			for w := range tc.words {
				if want := fmt.Sprintf("_logos_seen[%[1]d]&_logos_required%[1]d != _logos_required%[1]d", w); !strings.Contains(body, want) {
					t.Errorf("closing check lacks %q", want)
				}
			}
			if want := "Field: _logos_jsonmissing_R(_logos_seen)}"; !strings.Contains(body, want) {
				t.Errorf("closing check lacks %q: it must name the first missing field", want)
			}
		})
	}
}

// keyedStruct declares a struct of n tagged scalar fields.
func keyedStruct(n int) string {
	var b strings.Builder
	b.WriteString("package p\n\ntype K struct {\n")
	for i := range n {
		fmt.Fprintf(&b, "\tF%d int `json:\"f%d\"`\n", i, i)
	}
	b.WriteString("}\n")
	return b.String()
}

// TestKeyDispatch checks that keys bind without reflection or validate
// tags: up to 8 keys dispatch by a switch, more by the perfect-hash table.
func TestKeyDispatch(t *testing.T) {
	small := lowerStaticText(t, keyedStruct(8))
	if body := methodBody(t, small, "func _logos_jsonkey_K(k string) int {"); !strings.Contains(body, "switch k {") {
		t.Errorf("8 keys must dispatch by switch:\n%s", body)
	}
	if strings.Contains(small, "ConstTable") {
		t.Errorf("8 keys must not build a table")
	}
	large := lowerStaticText(t, keyedStruct(9))
	body := methodBody(t, large, "func _logos_jsonkey_K(k string) int {")
	if strings.Contains(body, "switch k {") || !strings.Contains(body, "_logos_rt.HashString(k)") {
		t.Errorf("9 keys must dispatch by table probe:\n%s", body)
	}
	if !strings.Contains(large, rodataMarker+"\nvar _logos_jsonkeys_K = _logos_rt.ConstTable[string, int]{") {
		t.Errorf("9 keys must build a read-only ConstTable")
	}
	for _, text := range []string{small, large} {
		if strings.Contains(text, "reflect") || strings.Contains(text, "validate") {
			t.Errorf("key dispatch mentions reflect or validate tags")
		}
	}
}

// TestFloatSliceSizeHasNoLoop checks that a float slice is sized by its
// fixed per-element bound, so no size loop leaves an element unused.
func TestFloatSliceSizeHasNoLoop(t *testing.T) {
	text := lowerStaticText(t, "package p\n\ntype F struct {\n\tV []float64 `json:\"v\"`\n}\n")
	body := methodBody(t, text, "func (_logos_v F) MarshalJSON() ([]byte, error) {")
	if !strings.Contains(body, "_logos_n += len(_logos_f0) * (_logos_json.MaxFloatLen64 + 1)") {
		t.Errorf("float slice size is not the per-element bound:\n%s", body)
	}
	if got := strings.Count(body, "for _logos_i, _logos_x := range _logos_f0 {"); got != 1 {
		t.Errorf("float slice has %d element loops, want 1 (emit only):\n%s", got, body)
	}
}

// TestNoValidateTagsNeeded checks that a struct with a non-nil field and
// no tag at all still enforces its required field.
func TestNoValidateTagsNeeded(t *testing.T) {
	text := lowerStaticText(t, "package p\n\ntype P struct{ X int }\ntype W struct {\n\tInner *P\n}\n")
	body := methodBody(t, text, "func (_logos_v *W) UnmarshalJSON(_logos_data []byte) error {")
	if !strings.Contains(body, `MissingFieldError{Type: "W", Field: "Inner"}`) && !strings.Contains(text, "_logos_jsonmissing_W") {
		t.Fatalf("W does not enforce Inner:\n%s", body)
	}
}

// TestTagSupportRules checks that a json tag the two encoding/json modes
// read differently keeps the whole struct on the reflective path: a name
// invalid under the v1 rule, an option other than omitempty, omitzero, and
// string, a repeated or empty option, and a tag on an unexported field.
func TestTagSupportRules(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		want bool
	}{
		{"plain", "`json:\"a\"`", true},
		{"options", "`json:\"a,omitempty,string\"`", true},
		{"emptyname", "`json:\",omitzero\"`", true},
		{"skipped", "`json:\"-\"`", true},
		{"dashname", "`json:\"-,\"`", false},
		{"symbolname", "`json:\"a€\"`", false},
		{"quotedname", "`json:\"'a'\"`", false},
		{"unknownoption", "`json:\"a,inline\"`", false},
		{"repeated", "`json:\"a,omitempty,omitempty\"`", false},
		{"emptyoption", "`json:\"a,,omitempty\"`", false},
		{"trailingcomma", "`json:\"a,\"`", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := lowerStaticText(t, "package p\n\ntype S struct {\n\tA int "+c.tag+"\n}\n")
			if got := strings.Contains(text, "func (_logos_v S) MarshalJSON()"); got != c.want {
				t.Fatalf("generated = %v, want %v:\n%s", got, c.want, text)
			}
		})
	}
	text := lowerStaticText(t, "package p\n\ntype S struct {\n\tA int `json:\"a\"`\n\thidden int `json:\"h\"`\n}\n")
	if strings.Contains(text, "func (_logos_v S) MarshalJSON()") {
		t.Fatalf("a tagged unexported field must keep the reflective path:\n%s", text)
	}
}

// TestGeneratedNamesAreReserved checks that every generated receiver,
// parameter, and local carries the reserved prefix, so a type named like
// a generated local still compiles.
func TestGeneratedNamesAreReserved(t *testing.T) {
	text := lowerStaticText(t, "package p\n\ntype data struct {\n\tN int `json:\"n\"`\n}\n")
	for _, want := range []string{
		"func (_logos_v *data) UnmarshalJSON(_logos_data []byte) error {",
		"_logos_s := _logos_json.NewScanner(_logos_data, (*data)(nil))",
		"func (_logos_v data) MarshalJSON() ([]byte, error) {",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated text lacks %q:\n%s", want, text)
		}
	}
}

// TestConstPointerFieldTriggers checks that a `const *T` field counts as a
// non-nil pointer field, like the lowering does.
func TestConstPointerFieldTriggers(t *testing.T) {
	text := lowerStaticText(t, "package p\n\ntype C struct {\n\tP const *int\n}\n")
	if !strings.Contains(text, "func (_logos_v *C) UnmarshalJSON(") {
		t.Fatalf("a const pointer field must trigger generation:\n%s", text)
	}
}

// TestUnionClassification checks that closed unions are admitted for static JSON
// and classify as shapeUnion, while open interfaces and ~-term unions are rejected.
func TestUnionClassification(t *testing.T) {
	checkStruct := func(t *testing.T, src, typeName string) (bool, jsonShape) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "x.lgo")
		if err := os.WriteFile(path, []byte(src), 0666); err != nil {
			t.Fatal(err)
		}
		var perrs []error
		f, err := syntax.ParseFile(path, func(err error) { perrs = append(perrs, err) }, nil, syntax.CheckBranches|syntax.LogosMode)
		if err != nil {
			t.Fatal(errors.Join(append(perrs, err)...))
		}
		info := &types2.Info{
			Defs:       make(map[*syntax.Name]types2.Object),
			Uses:       make(map[*syntax.Name]types2.Object),
			Types:      make(map[syntax.Expr]types2.TypeAndValue),
			Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
			Scopes:     make(map[syntax.Node]*types2.Scope),
		}
		var cerrs []error
		imp := newTestImporter(t)
		conf := types2.Config{
			Importer: imp,
			Error: func(err error) {
				if !types2.LogosWarnOnly(err) {
					cerrs = append(cerrs, err)
				}
			},
			Logos: true,
		}
		files := []*syntax.File{f}
		pkg, _ := conf.Check("main", files, info)
		if err := errors.Join(cerrs...); err != nil {
			t.Fatalf("type check: %v", err)
		}
		obj := pkg.Scope().Lookup(typeName)
		if obj == nil {
			t.Fatalf("type %q not found", typeName)
		}
		named := obj.Type().(*types2.Named)
		st := named.Underlying().(*types2.Struct)
		var rhs syntax.Expr
		for _, decl := range f.DeclList {
			if td, ok := decl.(*syntax.TypeDecl); ok && td.Name.Value == typeName {
				rhs = td.Type
				break
			}
		}
		if rhs == nil {
			t.Fatalf("syntax declaration for %q not found", typeName)
		}
		fields, ok := types2.LogosStaticStruct(info, named, rhs)
		fl := &fileLower{file: f, pkg: &packageLower{info: info, tpkg: pkg}}
		shape := fl.classifyJSONField(st.Field(0).Type())
		if ok {
			js := fl.buildJSONStruct(typeName, st, fields)
			if len(js.fields) > 0 {
				shape = js.fields[0].shape
			}
		}
		return ok, shape
	}

	t.Run("ClosedUnionAdmitted", func(t *testing.T) {
		src := `package p

type Circle struct {
	R int ` + "`" + `json:"r"` + "`" + `
}

type Square struct {
	S int ` + "`" + `json:"s"` + "`" + `
}

type Shape interface {
	Circle | Square
}

type Container struct {
	Sh Shape ` + "`" + `json:"sh"` + "`" + `
}
`
		ok, shape := checkStruct(t, src, "Container")
		if !ok {
			t.Fatalf("Container with closed union Shape was not admitted")
		}
		if shape.kind != shapeUnion {
			t.Fatalf("Container.Sh classified as %v, want shapeUnion", shape.kind)
		}
	})

	t.Run("OpenInterfaceRejected", func(t *testing.T) {
		src := `package p

type Reader interface {
	Read() int
}

type Container struct {
	R Reader ` + "`" + `json:"r"` + "`" + `
}
`
		ok, shape := checkStruct(t, src, "Container")
		if ok {
			t.Fatalf("Container with open interface Reader was admitted")
		}
		if shape.kind == shapeUnion {
			t.Fatalf("open interface Reader classified as shapeUnion")
		}
	})

	t.Run("AnyInterfaceRejected", func(t *testing.T) {
		src := `package p

type Container struct {
	A any ` + "`" + `json:"a"` + "`" + `
}
`
		ok, shape := checkStruct(t, src, "Container")
		if ok {
			t.Fatalf("Container with any interface was admitted")
		}
		if shape.kind == shapeUnion {
			t.Fatalf("any classified as shapeUnion")
		}
	})

	t.Run("TildeUnionRejected", func(t *testing.T) {
		src := `package p

type Approx interface {
	~int | ~string
}
`
		path := filepath.Join(t.TempDir(), "x.lgo")
		if err := os.WriteFile(path, []byte(src), 0666); err != nil {
			t.Fatal(err)
		}
		var perrs []error
		f, err := syntax.ParseFile(path, func(err error) { perrs = append(perrs, err) }, nil, syntax.CheckBranches|syntax.LogosMode)
		if err != nil {
			t.Fatal(errors.Join(append(perrs, err)...))
		}
		info := &types2.Info{
			Defs:       make(map[*syntax.Name]types2.Object),
			Uses:       make(map[*syntax.Name]types2.Object),
			Types:      make(map[syntax.Expr]types2.TypeAndValue),
			Selections: make(map[*syntax.SelectorExpr]*types2.Selection),
			Scopes:     make(map[syntax.Node]*types2.Scope),
		}
		var cerrs []error
		imp := newTestImporter(t)
		conf := types2.Config{
			Importer: imp,
			Error: func(err error) {
				if !types2.LogosWarnOnly(err) {
					cerrs = append(cerrs, err)
				}
			},
			Logos: true,
		}
		files := []*syntax.File{f}
		pkg, _ := conf.Check("main", files, info)
		if err := errors.Join(cerrs...); err != nil {
			t.Fatalf("type check: %v", err)
		}
		obj := pkg.Scope().Lookup("Approx")
		if obj == nil {
			t.Fatalf("type Approx not found")
		}
		approx := obj.Type()
		fl := &fileLower{file: f, pkg: &packageLower{info: info, tpkg: pkg}}
		shape := fl.classifyJSONField(approx)
		if shape.kind == shapeUnion {
			t.Fatalf("~-term union classified as shapeUnion")
		}
		if shape.kind != shapeFallback {
			t.Fatalf("~-term union classified as %v, want shapeFallback", shape.kind)
		}
		if s := fl.classifyShape(approx); s.kind != shapeFallback {
			t.Fatalf("classifyShape(~-term union) = %v, want shapeFallback", s.kind)
		}
	})

	t.Run("LockUnionRejected", func(t *testing.T) {
		src := `package p

import "sync"

type LockedMember struct {
	Mu sync.Mutex
	V  int ` + "`" + `json:"v"` + "`" + `
}

type NormalMember struct {
	V int ` + "`" + `json:"v"` + "`" + `
}

type LockUnion interface {
	LockedMember | NormalMember
}

type Container struct {
	U LockUnion ` + "`" + `json:"u"` + "`" + `
}
`
		ok, _ := checkStruct(t, src, "Container")
		if ok {
			t.Fatalf("Container with lock-carrying union member was admitted")
		}
	})
}

// TestUnionUnmarshalGenerated verifies that a struct holding a closed union
// generates an UnmarshalJSON with trials in declaration order, scratch values
// per trial, and an ambiguity error reference (SPEC Sec 12.2, T-Sec12, T-203.1).
func TestUnionUnmarshalGenerated(t *testing.T) {
	src := `package p

type Circle struct {
	R int ` + "`" + `json:"r"` + "`" + `
}

type Square struct {
	S int ` + "`" + `json:"s"` + "`" + `
}

type Shape interface {
	Circle | Square
}

type Container struct {
	Sh Shape ` + "`" + `json:"sh"` + "`" + `
}
`
	text := lowerStaticText(t, src)
	body := methodBody(t, text, "func (_logos_v *Container) UnmarshalJSON(_logos_data []byte) error {")

	if !strings.Contains(body, "_logos_raw := _logos_s.Value()") {
		t.Fatalf("UnmarshalJSON lacks raw span capture via _logos_s.Value():\n%s", body)
	}

	cIdx := strings.Index(body, "var _logos_scratch0 Circle")
	sIdx := strings.Index(body, "var _logos_scratch1 Square")
	if cIdx < 0 || sIdx < 0 || cIdx >= sIdx {
		t.Fatalf("trials must be in declaration order (Circle before Square):\n%s", body)
	}
	if !strings.Contains(body, "_logos_scratch0.UnmarshalJSON(_logos_raw)") ||
		!strings.Contains(body, "_logos_scratch1.UnmarshalJSON(_logos_raw)") {
		t.Fatalf("trials must decode into fresh scratch values:\n%s", body)
	}

	if count := strings.Count(body, "UnionTrialError"); count < 2 {
		t.Fatalf("UnmarshalJSON must reference UnionTrialError for zero and multiple matches (got %d):\n%s", count, body)
	}

	if !strings.Contains(body, "_logos_jsonkey_Circle") || !strings.Contains(body, "_logos_jsonkey_Square") {
		t.Fatalf("UnmarshalJSON must check modeled field presence via key helpers:\n%s", body)
	}
	// A key modeled by a sibling member but not this one disqualifies the trial:
	// Circle's trial must consult _logos_jsonkey_Square and vice versa.
	if !strings.Contains(body, "_logos_jsonkey_Square(_logos_chk.Key())") ||
		!strings.Contains(body, "_logos_jsonkey_Circle(_logos_chk.Key())") {
		t.Fatalf("UnmarshalJSON must disqualify on sibling-modeled keys:\n%s", body)
	}
	// JSON null must clear an optional union field, not leave the prior value.
	if !strings.Contains(body, "_logos_v.Sh = nil") {
		t.Fatalf("UnmarshalJSON must clear union field on null:\n%s", body)
	}

	if !strings.Contains(body, "_logos_v.Sh = _logos_target") {
		t.Fatalf("UnmarshalJSON must assign target on single success:\n%s", body)
	}
	for _, banned := range []string{"_logos_s.Fallback(&_logos_v.Sh)"} {
		if strings.Contains(body, banned) {
			t.Fatalf("union field took forbidden fallback path:\n%s", body)
		}
	}
}
