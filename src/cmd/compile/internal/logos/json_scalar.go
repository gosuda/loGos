// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package logos

import (
	"fmt"
	"strings"
)

// scalarCodec spells the encode and decode code of one modeled scalar: a
// bool, an integer, a float, or a string, with the `string` tag option
// folded into the codec when quoted.
type scalarCodec struct {
	kind   scalarKind
	quoted bool
}

// codec returns the scalar codec of a field value, pointee, or element.
func (f *jsonField) codec() scalarCodec {
	return scalarCodec{kind: f.shape.scalar, quoted: f.quoted}
}

// elemCodec returns the codec of a slice element, which never quotes.
func (f *jsonField) elemCodec() scalarCodec {
	return scalarCodec{kind: f.shape.scalar}
}

// open is the text written after the key before the value body: the
// opening quote of a string, or of any scalar under the `string` option.
func (c scalarCodec) open() string {
	if c.kind == scalarString || c.quoted {
		return `"`
	}
	return ""
}

// closeText is the text written after the value body.
func (c scalarCodec) closeText() string {
	return c.open()
}

// isFloat reports whether the codec encodes a float.
func (c scalarCodec) isFloat() bool {
	return c.kind == scalarFloat32 || c.kind == scalarFloat64
}

// floatBits returns the bit size of a float codec.
func (c scalarCodec) floatBits() int {
	if c.kind == scalarFloat32 {
		return 32
	}
	return 64
}

// checkStmt spells the unsupported-value check of a float x, or nothing
// for other kinds. The subtraction is NaN exactly for NaN and infinities,
// so a finite value never boxes into an interface.
func (c scalarCodec) checkStmt(x string) string {
	if !c.isFloat() {
		return ""
	}
	return fmt.Sprintf("if %[1]s-%[1]s != 0 { return nil, %[2]s.FloatErr(%[1]s, float64(%[1]s), %[3]d) }", x, jsonAlias, c.floatBits())
}

// sizeStmt spells the statement adding the length of the value body of x.
func (c scalarCodec) sizeStmt(x string) string {
	switch c.kind {
	case scalarBool:
		return fmt.Sprintf("if %s { _logos_n += 4 } else { _logos_n += 5 }", x)
	case scalarInt:
		return fmt.Sprintf("_logos_n += %s.IntLen(int64(%s))", jsonAlias, x)
	case scalarUint:
		return fmt.Sprintf("_logos_n += %s.UintLen(uint64(%s))", jsonAlias, x)
	case scalarFloat32:
		return "_logos_n += " + jsonAlias + ".MaxFloatLen32"
	case scalarFloat64:
		return "_logos_n += " + jsonAlias + ".MaxFloatLen64"
	}
	return fmt.Sprintf("_logos_n += %s.%s(string(%s))", jsonAlias, c.stringLen(), x)
}

// stringLen names the length helper of a string codec.
func (c scalarCodec) stringLen() string {
	if c.quoted {
		return "QuotedLen"
	}
	return "EscapedLen"
}

// stringAppend names the append helper of a string codec.
func (c scalarCodec) stringAppend() string {
	if c.quoted {
		return "AppendQuoted"
	}
	return "AppendEscaped"
}

// emitStmt spells the statement appending the value body of x to _logos_buf.
func (c scalarCodec) emitStmt(x string) string {
	switch c.kind {
	case scalarBool:
		return fmt.Sprintf("if %s { _logos_buf = append(_logos_buf, \"true\"...) } else { _logos_buf = append(_logos_buf, \"false\"...) }", x)
	case scalarInt:
		return fmt.Sprintf("_logos_buf = %s.AppendInt(_logos_buf, int64(%s))", jsonAlias, x)
	case scalarUint:
		return fmt.Sprintf("_logos_buf = %s.AppendUint(_logos_buf, uint64(%s))", jsonAlias, x)
	case scalarFloat32, scalarFloat64:
		return fmt.Sprintf("_logos_buf = %s.AppendFloat(_logos_buf, float64(%s), %d)", jsonAlias, x, c.floatBits())
	}
	return fmt.Sprintf("_logos_buf = %s.%s(_logos_buf, string(%s))", jsonAlias, c.stringAppend(), x)
}

// decodeFn names the generic reader of the codec.
func (c scalarCodec) decodeFn() string {
	name := [...]string{"Bool", "Int", "Uint", "Float", "Float", "String"}[c.kind]
	if c.quoted {
		name += "Q"
	}
	return jsonAlias + "." + name
}

// isZeroCond spells the condition that x is the zero value of the codec.
func (c scalarCodec) isZeroCond(x string) string {
	switch c.kind {
	case scalarBool:
		return "!" + x
	case scalarString:
		return x + ` == ""`
	}
	return x + " == 0"
}

// keyLiteral spells the bytes of `"key":` as encoding/json writes them with
// HTML escaping, for use inside a raw string literal.
func keyLiteral(key string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range key {
		switch r {
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		case '\u2028':
			b.WriteString(`\u2028`)
		case '\u2029':
			b.WriteString(`\u2029`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(`":`)
	return b.String()
}
