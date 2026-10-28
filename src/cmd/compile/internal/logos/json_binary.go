// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"fmt"
	"strings"

	"cmd/compile/internal/syntax"
	"cmd/compile/internal/types2"
)

// lowerStaticBinary emits binary methods only when every JSON-visible field
// has a representation defined by M11-S3.
func (fl *fileLower) lowerStaticBinary(_ *syntax.TypeDecl, named *types2.Named, js *jsonStruct, out *strings.Builder) {
	if !types2.LogosBinaryTypeOK(named) {
		return
	}
	for _, f := range js.fields {
		for _, m := range types2.LogosBinaryMethodNames() {
			if f.goName == m {
				return
			}
		}
	}
	needMarshal := !userDeclaresMethod(named, "MarshalBinary")
	needAppend := !userDeclaresMethod(named, "AppendBinary")
	needUnmarshal := !userDeclaresMethod(named, "UnmarshalBinary")
	if !needMarshal && !needAppend && !needUnmarshal {
		return
	}
	g := binaryGenerator{fl: fl, prefix: "_logos_bin_" + js.name, ids: make(map[types2.Type]int)}
	id, ok := g.codec(named)
	if !ok {
		return
	}
	out.WriteString(g.text.String())
	if needMarshal {
		fmt.Fprintf(out, "func (_logos_v %s) MarshalBinary() ([]byte, error) { return %s_e%d([]byte{1}, _logos_v) }\n", js.name, g.prefix, id)
	}
	if needAppend {
		fmt.Fprintf(out, "func (_logos_v %s) AppendBinary(dst []byte) ([]byte, error) { return %s_e%d(append(dst, 1), _logos_v) }\n", js.name, g.prefix, id)
	}
	if needUnmarshal {
		fmt.Fprintf(out, "func (_logos_v *%s) UnmarshalBinary(src []byte) error { off := 0; if len(src) == 0 { return _logos_bfmt.Errorf(\"binary field $version at offset 0: truncated input\") }; if src[0] != 1 { return _logos_bfmt.Errorf(\"binary field $version at offset 0: wrong version %%d\", src[0]) }; off++; if err := %s_d%d(src, &off, _logos_v, %q); err != nil { return err }; if off != len(src) { return _logos_bfmt.Errorf(\"binary field $trailing at offset %%d: trailing bytes\", off) }; return nil }\n", js.name, g.prefix, id, js.name)
	}
}

type binaryGenerator struct {
	fl     *fileLower
	prefix string
	ids    map[types2.Type]int
	text   strings.Builder
}

func (g *binaryGenerator) codec(t types2.Type) (int, bool) {
	t = types2.Unalias(t)
	if id, ok := g.ids[t]; ok {
		return id, true
	}
	id := len(g.ids)
	g.ids[t] = id
	name, ok := g.fl.spellLowered(t)
	if !ok {
		return 0, false
	}
	var enc, dec strings.Builder
	e := func(format string, args ...any) { fmt.Fprintf(&enc, format+"\n", args...) }
	d := func(format string, args ...any) { fmt.Fprintf(&dec, format+"\n", args...) }
	child := func(et types2.Type, value, target, field string) bool {
		c, ok := g.codec(et)
		if !ok {
			return false
		}
		e("dst, err = %s_e%d(dst, %s); if err != nil { return dst, err }", g.prefix, c, value)
		d("if err := %s_d%d(src, off, %s, %s); err != nil { return err }", g.prefix, c, target, field)
		return true
	}
	fail := "return _logos_bfmt.Errorf(\"binary field %s at offset %d: truncated input\", field, *off)"
	if opt := g.fl.asOption(t); opt != nil && keepMapRead(opt.Elem) {
		payload, ok := g.codec(opt.Elem)
		if !ok {
			return 0, false
		}
		fmt.Fprintf(&g.text, "func %s_e%d(dst []byte, v %s) ([]byte, error) { return %s_e%d(dst, v) }\n", g.prefix, id, name, g.prefix, payload)
		fmt.Fprintf(&g.text, "func %s_d%d(src []byte, off *int, v *%s, field string) error { return %s_d%d(src, off, v, field) }\n", g.prefix, id, name, g.prefix, payload)
		return id, true
	}
	if opt := g.fl.asOption(t); opt != nil {
		e("if !v.Present { return append(dst, 0), nil }; dst = append(dst, 1)")
		d("if *off >= len(src) { %s }; presence := src[*off]; *off++; if presence > 1 { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: bad presence byte %%d\", field, *off-1, presence) }; *v = %s{}; if presence == 0 { return nil }; v.Present = true", fail, name)
		if !child(opt.Elem, "v.Value", "&v.Value", "field") {
			return 0, false
		}
	} else {
		u := t.Underlying()
		switch x := u.(type) {
		case *types2.Basic:
			s, _ := basicScalar(x)
			switch s {
			case scalarBool:
				e("if v { dst = append(dst, 1) } else { dst = append(dst, 0) }")
				d("if *off >= len(src) { %s }; b := src[*off]; *off++; if b > 1 { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: bad bool byte %%d\", field, *off-1, b) }; *v = %s(b == 1)", fail, name)
			case scalarString:
				e("dst = _logos_bbinary.AppendUvarint(dst, uint64(len(v))); dst = append(dst, v...)")
				d("n, used := _logos_bbinary.Uvarint(src[*off:]); if used <= 0 { %s }; *off += used; if n > uint64(len(src)-*off) { %s }; *v = %s(string(src[*off:*off+int(n)])); *off += int(n)", fail, fail, name)
			default:
				width, bits, decode := 8, "uint64(v)", "n"
				if s == scalarFloat32 {
					width, bits, decode = 4, "uint64(_logos_bmath.Float32bits(float32(v)))", "_logos_bmath.Float32frombits(uint32(n))"
				}
				if s == scalarFloat64 {
					bits, decode = "_logos_bmath.Float64bits(float64(v))", "_logos_bmath.Float64frombits(n)"
				}
				e("n := %s; for i := uint(0); i < %d; i++ { dst = append(dst, byte(n >> (8*i))) }", bits, width)
				d("if len(src)-*off < %d { %s }; var n uint64; for i := uint(0); i < %d; i++ { n |= uint64(src[*off+int(i)]) << (8*i) }; *off += %d; *v = %s(%s)", width, fail, width, width, name, decode)
			}
		case *types2.Pointer:
			pointed, ok := g.typeName(x.Elem())
			if !ok {
				return 0, false
			}
			e("if v == nil { return append(dst, 0), nil }; dst = append(dst, 1)")
			d("if *off >= len(src) { %s }; p := src[*off]; *off++; if p > 1 { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: bad presence byte %%d\", field, *off-1, p) }; if p == 0 { *v = nil; return nil }; *v = new(%s)", fail, pointed)
			if !child(x.Elem(), "*v", "*v", "field") {
				return 0, false
			}
		case *types2.Struct:
			for i := range x.NumFields() {
				if _, ok := types2.LogosJSONFieldOf(x, i); !ok {
					continue
				}
				f := x.Field(i)
				field := "field + " + fmt.Sprintf("%q", "."+f.Name())
				if !child(f.Type(), "v."+f.Name(), "&v."+f.Name(), field) {
					return 0, false
				}
			}
		case *types2.Slice:
			if b, ok := x.Elem().Underlying().(*types2.Basic); ok && b.Kind() == types2.Uint8 {
				e("dst = _logos_bbinary.AppendUvarint(dst, uint64(len(v))); dst = append(dst, v...)")
				d("n, used := _logos_bbinary.Uvarint(src[*off:]); if used <= 0 { %s }; *off += used; if n > uint64(len(src)-*off) { %s }; *v = append(make(%s, 0, int(n)), src[*off:*off+int(n)]...); *off += int(n)", fail, fail, name)
				break
			}
			if g.isZeroWire(x.Elem()) {
				// Zero-wire elements encode zero bytes, so remaining input
				// gives no lawful bound (SPEC Sec 12.3). Cap the count itself
				// at the 1 MiB prealloc bound (unit 1) and additionally cap
				// the single-allocation byte total (count times element size)
				// at 64 MiB, so a tiny header cannot drive an arbitrarily large
				// make through fat ignored-field storage while a 1M ZeroPad
				// (8 MiB) stays lawful; each field is checked on its own, so
				// sums across fields are never rejected. Reject beyond either
				// cap with a count-out-of-range error, encoder symmetric, in
				// constant time without per-element iteration. Accepted wire
				// bytes are unchanged.
				elemType, ok := g.typeName(x.Elem())
				if !ok {
					return 0, false
				}
				e("dst = _logos_bbinary.AppendUvarint(dst, uint64(len(v)))")
				e("if uint64(len(v)) > (1 << 20) { return dst, _logos_bfmt.Errorf(\"binary count %%d out of range\", len(v)) }")
				e("_logos_zsize := uint64(_logos_bunsafe.Sizeof(*new(%s))); if _logos_zsize == 0 { _logos_zsize = 1 }", elemType)
				e("if uint64(len(v)) > (1 << 26)/_logos_zsize { return dst, _logos_bfmt.Errorf(\"binary count %%d out of range\", len(v)) }")
				d("n, used := _logos_bbinary.Uvarint(src[*off:]); if used <= 0 { %s }; *off += used", fail)
				d("if n > uint64(int(^uint(0) >> 1)) { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: count %%d out of range\", field, *off-used, n) }")
				d("if n > (1 << 20) { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: count %%d out of range\", field, *off-used, n) }")
				d("_logos_zsize := uint64(_logos_bunsafe.Sizeof(*new(%s))); if _logos_zsize == 0 { _logos_zsize = 1 }", elemType)
				d("if n > (1 << 26)/_logos_zsize { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: count %%d out of range\", field, *off-used, n) }")
				d("*v = make(%s, int(n))", name)
				break
			}
			elemType, ok := g.typeName(x.Elem())
			if !ok {
				return 0, false
			}
			e("dst = _logos_bbinary.AppendUvarint(dst, uint64(len(v))); for _, elem := range v {")
			d("n, used := _logos_bbinary.Uvarint(src[*off:]); if used <= 0 { %s }; *off += used; if n > uint64(len(src)-*off) { %s }; var elem %s; size := uint64(_logos_bunsafe.Sizeof(elem)); if size == 0 { size = 1 }; cap64 := min(n, uint64(len(src)-*off)/2, (1<<20)/size); *v = make(%s, 0, int(cap64)); for i := uint64(0); i < n; i++ {", fail, fail, elemType, name)
			if !child(x.Elem(), "elem", "&elem", "field") {
				return 0, false
			}
			e("}")
			d("*v = append(*v, elem) }")
		case *types2.Array:
			elemType, ok := g.typeName(x.Elem())
			if !ok {
				return 0, false
			}
			e("dst = _logos_bbinary.AppendUvarint(dst, uint64(len(v))); for _, elem := range v {")
			d("n, used := _logos_bbinary.Uvarint(src[*off:]); if used <= 0 { %s }; *off += used; if n != %d { return _logos_bfmt.Errorf(\"binary field %%s at offset %%d: array count %%d, want %d\", field, *off-used, n) }; for i := range *v { var elem %s", fail, x.Len(), x.Len(), elemType)
			if !child(x.Elem(), "elem", "&elem", "field") {
				return 0, false
			}
			e("}")
			d("(*v)[i] = elem }")
		case *types2.Map:
			keyType, keyOK := g.typeName(x.Key())
			valueType, valueOK := g.typeName(x.Elem())
			if !keyOK || !valueOK {
				return 0, false
			}
			e("dst = _logos_bbinary.AppendUvarint(dst, uint64(len(v))); keys := make([]string, 0, len(v)); for key := range v { keys = append(keys, string(key)) }; _logos_bslices.Sort(keys); for _, raw := range keys { key := %s(raw); elem := v[key]", keyType)
			d("n, used := _logos_bbinary.Uvarint(src[*off:]); if used <= 0 { %s }; *off += used; *v = make(%s); for i := uint64(0); i < n; i++ { var key %s; var elem %s", fail, name, keyType, valueType)
			if !child(x.Key(), "key", "&key", "field") || !child(x.Elem(), "elem", "&elem", "field") {
				return 0, false
			}
			e("}")
			d("(*v)[key] = elem }")
		default:
			return 0, false
		}
	}
	fmt.Fprintf(&g.text, "func %s_e%d(dst []byte, v %s) ([]byte, error) { var err error; _ = err\n%sreturn dst, nil\n}\n", g.prefix, id, name, enc.String())
	fmt.Fprintf(&g.text, "func %s_d%d(src []byte, off *int, v *%s, field string) error {\n%sreturn nil\n}\n", g.prefix, id, name, dec.String())
	return id, true
}

// isZeroWire reports whether values of t encode to zero wire bytes under
// the actual binary format: basics, pointers, options, slices, maps, and
// arrays are nonzero because each carries at least a presence bit, count
// header, or scalar payload; a struct is zero-wire only when every
// JSON-visible field is itself zero-wire, which includes the vacuously
// empty ignored-only struct.
func (g *binaryGenerator) isZeroWire(t types2.Type) bool {
	return g.zeroWire(types2.Unalias(t), make(map[types2.Type]bool))
}

func (g *binaryGenerator) zeroWire(t types2.Type, seen map[types2.Type]bool) bool {
	t = types2.Unalias(t)
	if seen[t] {
		return false
	}
	seen[t] = true
	defer delete(seen, t)
	if opt := g.fl.asOption(t); opt != nil {
		if keepMapRead(opt.Elem) {
			return g.zeroWire(opt.Elem, seen)
		}
		return false
	}
	if cv, ok := t.(*types2.ConstView); ok {
		return g.zeroWire(cv.Elem, seen)
	}
	u := t.Underlying()
	if cv, ok := u.(*types2.ConstView); ok {
		return g.zeroWire(cv.Elem, seen)
	}
	switch x := u.(type) {
	case *types2.Basic:
		return false
	case *types2.Pointer:
		return false
	case *types2.Slice:
		return false
	case *types2.Array:
		return false
	case *types2.Map:
		return false
	case *types2.Struct:
		for i := range x.NumFields() {
			if _, ok := types2.LogosJSONFieldOf(x, i); !ok {
				continue
			}
			if !g.zeroWire(x.Field(i).Type(), seen) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (g *binaryGenerator) typeName(t types2.Type) (string, bool) {
	return g.fl.spellLowered(t)
}

// binaryImportTable lists the marker, alias, and path of every package the
// generated binary helpers can reference.
var binaryImportTable = []struct{ marker, alias, path string }{
	{"_logos_bfmt.", "_logos_bfmt", "fmt"},
	{"_logos_bbinary.", "_logos_bbinary", "encoding/binary"},
	{"_logos_bmath.", "_logos_bmath", "math"},
	{"_logos_bunsafe.", "_logos_bunsafe", "unsafe"},
	{"_logos_bslices.", "_logos_bslices", "slices"},
}

// binaryImports supplies just the packages referenced by generated helpers.
func (fl *fileLower) binaryImports(pos syntax.Pos) []*syntax.ImportDecl {
	var out []*syntax.ImportDecl
	for _, item := range binaryImportTable {
		if !strings.Contains(fl.constText, item.marker) {
			continue
		}
		out = append(out, at(&syntax.ImportDecl{LocalPkgName: nameOf(pos, item.alias), Path: litOf(pos, fmt.Sprintf("%q", item.path), syntax.StringLit)}, pos))
	}
	return out
}
