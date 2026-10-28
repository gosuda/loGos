// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"errors"
	"fmt"
	"go/token"
	"reflect"
	"strconv"

	"cmd/compile/internal/syntax"
)

// structtagMalformed is correctness::structtag_malformed (LINT-RULES.md Sec 1.2):
// validates struct field tag literals against reflection conventions defined
// by reflect.StructTag.
var structtagMalformed = Rule{
	ID:    "correctness::structtag_malformed",
	Group: Correctness,
	Go:    true,
	Check: checkStructTagMalformed,
}

func checkStructTagMalformed(pass *Pass) {
	for _, f := range pass.Files {
		syntax.Inspect(f, func(n syntax.Node) bool {
			st, ok := n.(*syntax.StructType)
			if !ok {
				return true
			}
			for i, field := range st.FieldList {
				if i >= len(st.TagList) || st.TagList[i] == nil {
					continue
				}
				tagLit := st.TagList[i]
				tagVal, err := strconv.Unquote(tagLit.Value)
				if err != nil {
					pass.Reportf(tagLit, "struct field tag %#q not compatible with reflect.StructTag.Get: bad syntax", tagLit.Value)
					continue
				}
				if err := validateStructTag(tagVal); err != nil {
					pass.Reportf(tagLit, "struct field tag %#q not compatible with reflect.StructTag.Get: %s", tagVal, err)
					continue
				}
				if field.Name != nil && !token.IsExported(field.Name.Value) {
					for _, enc := range [...]string{"json", "xml"} {
						switch reflect.StructTag(tagVal).Get(enc) {
						case "", "-":
						default:
							pass.Reportf(tagLit, "struct field %s has %s tag but is not exported", field.Name.Value, enc)
						}
					}
				}
			}
			return true
		})
	}
}

var (
	errTagSyntax      = errors.New("bad syntax for struct tag pair")
	errTagKeySyntax   = errors.New("bad syntax for struct tag key")
	errTagValueSyntax = errors.New("bad syntax for struct tag value")
	errTagSpace       = errors.New("key:\"value\" pairs not separated by spaces")
)

func validateStructTag(tag string) error {
	n := 0
	seenKeys := make(map[string]bool)
	for ; tag != ""; n++ {
		if n > 0 && tag != "" && tag[0] != ' ' {
			return errTagSpace
		}
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}

		i = 0
		for i < len(tag) && tag[i] > ' ' && tag[i] != ':' && tag[i] != '"' && tag[i] != 0x7f {
			i++
		}
		if i == 0 {
			return errTagKeySyntax
		}
		if i+1 >= len(tag) || tag[i] != ':' {
			return errTagSyntax
		}
		if tag[i+1] != '"' {
			return errTagValueSyntax
		}
		key := tag[:i]
		if seenKeys[key] {
			return fmt.Errorf("duplicate key %q in struct tag", key)
		}
		seenKeys[key] = true
		tag = tag[i+1:]

		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			return errTagValueSyntax
		}
		qvalue := tag[:i+1]
		tag = tag[i+1:]

		_, err := strconv.Unquote(qvalue)
		if err != nil {
			return errTagValueSyntax
		}
	}
	return nil
}
