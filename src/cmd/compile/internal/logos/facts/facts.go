// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package facts implements the __.LOGOS facts sidecar (M3-S1).
//
// Facts travel in the __.LOGOS archive member beside __.PKGDEF: a text
// header of the package's exported declarations in loGo/s syntax
// (nilability, const, unions, tuples, property interfaces), per-function
// facts, and comptime generic sources, bound to the __.PKGDEF fingerprint
// (T-Sec6). S1 defines the format and the transport; S2-S7 populate the
// records through SetCurrent. Until then every compiler-object archive
// carries a header-only member so the binding itself is testable end to
// end.
//
// Wire format (record lines sorted byte-wise; comptime bodies
// length-delimited):
//
//	logos-facts 3
//	fingerprint <hex of the __.PKGDEF linker fingerprint>
//	opt <object> <path>
//	const <object> <path>
//	fn <object> <flag>[,<flag>...]
//	readonly <object> <path>
//	prop <interface> <accessor-function-prefix>
//	conforms <struct> <interface-package-path> <interface>
//	comptime <object> <byte length>
//	<source bytes>
//	constfn <object> <byte length>
//	<source bytes>
//	default <type> <field> <byte length>
//	<default expression source text>
//
// <object> is Name, Type.Field, (*Type).Method, or Type.Method; <path> is
// dot-separated steps R<i> result, P<i> parameter, F<name> field, E element,
// D dereference of one pointer or channel container, K map key, M<name>
// interface method, T<i> tuple element, G<i> type-parameter constraint,
// H<i> receiver type-parameter constraint. Flags:
// ReturnsNonNil, AlwaysNilError, PureConst, ReceiverMutated, Readonly,
// AssertTP=<hex mask>, AssertRTP=<hex mask>,
// ParamMutated=<hex mask>, MayYield=0|1.
// A fn Readonly record carries a readonly requirement (interface I.M
// obligation) or a proven readonly capability (concrete T.M / (*T).M).
// Owner-qualified keys are stable: I.M for interfaces, methodObject-style
// T.M / (*T).M for concrete methods. A readonly record carries the same
// obligation for a nested or generic-constrained method addressed by
// object-root structural path. Version 2 added the Readonly fn flag
// for semantic method obligations; version 3 is a clean cutover adding
// the AssertTP and AssertRTP fn flags for generic assertion effects;
// version and fingerprint checks reject mismatched members with no
// compat shim.
// AssertTP records which function type parameters feed a type-assertion
// or type-switch target of the declaration; AssertRTP records the same
// for receiver type parameters. Each mask is an arbitrary-arity
// big-endian bit number in canonical lowercase hex (bit i set = type
// parameter i flows into an assertion target); there is no 64-bit
// limit. Known semantics: flag present = analyzed, flag absent =
// unknown, and consumers fail closed on unknown. AssertTP=0,AssertRTP=0
// means analyzed with proven absence of such flows. Assert flags say
// nothing about mutation: an assertion-only record must never certify
// mutation purity (ParamMutated stays absent = unknown mutation).
package facts

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"cmd/internal/goobj"
)

const (
	// Version is the facts format version written by Encode.
	Version = 3
	// MemberName is the archive member carrying facts, beside __.PKGDEF.
	MemberName = "__.LOGOS"

	arHeaderSize = 60
	arMagic      = "!<arch>\n"

	// maxMemberBytes bounds the __.LOGOS member FindMember will read.
	maxMemberBytes = 64 << 20
)

var versionLine = "logos-facts " + strconv.Itoa(Version)

// PathFact is one opt or const record: the object position carrying an
// option or const fact, and the dot-separated path to it. Each step
// descends one position: R<i> result, P<i> parameter, F<name> field, E
// option or view element (the ? qualifier), K map key, D one pointer or
// channel container, M<name> interface method, T<i> tuple element,
// G<i> type-parameter constraint, H<i> receiver type-parameter constraint.
type PathFact struct {
	Object string
	Path   string
}

// ReadonlyFact is one readonly record: the exported root object and the
// structural path to a nested or generic-constrained interface method
// carrying the readonly obligation. Object uses the same grammar as opt
// and const records; Path uses the same steps extended with G<i> and H<i>
// for type-parameter constraints. At least one M<name> step selects the
// target method; G<i>/H<i> selects a type-parameter constraint.
type ReadonlyFact struct {
	Object string
	Path   string
}

// FnFact is one fn record: per-function facts for the named object.
type FnFact struct {
	Object          string
	ReturnsNonNil   bool
	AlwaysNilError  bool
	PureConst       bool
	ReceiverMutated bool
	Readonly        bool
	// AssertTP is the normalized lowercase hex mask of function type
	// parameters feeding an assertion/type-switch target; "" when absent.
	AssertTP string
	// AssertRTP is the normalized lowercase hex mask of receiver type
	// parameters feeding an assertion/type-switch target; "" when absent.
	AssertRTP string
	// HasAssert is the Known bit: true when the assertion effects were
	// analyzed (flags present on the wire, "0" when proven-zero).
	HasAssert bool
	// ParamMutated is the normalized lowercase hex mutation mask; "" is absent.
	ParamMutated string
	MayYield     bool
	HasMayYield  bool
}

// MayYieldOnly reports whether MayYield is the only mutation-relevant fact
// of fn. Assertion-effect flags (AssertTP/AssertRTP) are not mutation
// facts, so a record carrying only assert flags plus MayYield still
// reports true. Such a record says nothing about mutation, nilability,
// or purity of values, so a consumer of those facts must treat the
// function as unanalyzed.
func (fn FnFact) MayYieldOnly() bool {
	return fn.HasMayYield && !fn.ReturnsNonNil && !fn.AlwaysNilError && !fn.PureConst && !fn.ReceiverMutated && !fn.Readonly && fn.ParamMutated == ""
}

// PropFact is one prop record: an interface and its accessor-function prefix.
type PropFact struct {
	Interface string
	Prefix    string
}

// ConformsFact is one conforms record: a struct type of this package and
// one property interface it declares conformance to, named as the
// interface's package path and type name so an importer in another
// package can match its own spelling of the same interface.
type ConformsFact struct {
	Type      string
	IfacePath string
	Iface     string
}

// ComptimeFact is one comptime record: the source of an exported comptime
// generic and the declarations it references.
type ComptimeFact struct {
	Object string
	Source []byte
}

// ConstFuncFact is one constfn record: the closed source of an exported
// receiverless non-generic const function. The source is a single
// parseable Logos file carrying the function's same-package declaration
// closure; a validated root `const func` body is the capability record.
type ConstFuncFact struct {
	Object string
	Source []byte
}

// Facts is the decoded content of one __.LOGOS member.
type Facts struct {
	Fingerprint goobj.FingerprintType
	Opts        []PathFact
	Consts      []PathFact
	Fns         []FnFact
	Readonlys   []ReadonlyFact
	Props       []PropFact
	Conforms    []ConformsFact
	Comptimes   []ComptimeFact
	ConstFuncs  []ConstFuncFact
	Defaults    []DefaultFact
	// GoPackage marks a package compiled from .go sources. Such a package
	// carries per-function backend facts such as MayYield and nothing
	// that makes it a loGo/s package, so importers must not read it as one.
	GoPackage bool
}

// DefaultFact is one default record: a defaulted field of an exported
// struct type and its default expression source text.
type DefaultFact struct {
	Type  string
	Field string
	Text  string
}

// Encode renders the facts in canonical form: header, fingerprint, then
// record lines sorted byte-wise. A nil f emits a header-only member, which
// is what the writer produces until S2-S7 populate records.
func Encode(fp goobj.FingerprintType, f *Facts) []byte {
	type record struct {
		line string
		body []byte // set only for comptime records
	}
	var recs []record
	// Encode emits only records Decode accepts. Vacuous records assert
	// nothing and are skipped, so output always round-trips while Decode
	// stays strict enough to catch producer bugs on the read path.
	if f != nil {
		for _, o := range f.Opts {
			if o.Object != "" && o.Path != "" {
				recs = append(recs, record{line: "opt " + o.Object + " " + o.Path})
			}
		}
		for _, c := range f.Consts {
			if c.Object != "" && c.Path != "" {
				recs = append(recs, record{line: "const " + c.Object + " " + c.Path})
			}
		}
		for _, fn := range f.Fns {
			if fl := fn.flags(); fn.Object != "" && fl != "" {
				recs = append(recs, record{line: "fn " + fn.Object + " " + fl})
			}
		}
		for _, r := range f.Readonlys {
			if r.Object != "" && r.Path != "" {
				recs = append(recs, record{line: "readonly " + r.Object + " " + r.Path})
			}
		}
		for _, p := range f.Props {
			if p.Interface != "" && p.Prefix != "" {
				recs = append(recs, record{line: "prop " + p.Interface + " " + p.Prefix})
			}
		}
		for _, c := range f.Conforms {
			if c.Type != "" && c.IfacePath != "" && c.Iface != "" {
				recs = append(recs, record{line: "conforms " + c.Type + " " + c.IfacePath + " " + c.Iface})
			}
		}
		for _, c := range f.Comptimes {
			if c.Object != "" && len(c.Source) > 0 {
				recs = append(recs, record{line: "comptime " + c.Object + " " + strconv.Itoa(len(c.Source)), body: c.Source})
			}
		}
		for _, c := range f.ConstFuncs {
			if c.Object != "" && len(c.Source) > 0 {
				recs = append(recs, record{line: "constfn " + c.Object + " " + strconv.Itoa(len(c.Source)), body: c.Source})
			}
		}
		if f.GoPackage {
			recs = append(recs, record{line: "lang go"})
		}
		for _, d := range f.Defaults {
			if d.Type != "" && d.Field != "" && d.Text != "" {
				recs = append(recs, record{line: "default " + d.Type + " " + d.Field + " " + strconv.Itoa(len(d.Text)), body: []byte(d.Text)})
			}
		}
	}
	slices.SortFunc(recs, func(a, b record) int { return cmp.Compare(a.line, b.line) })

	var b bytes.Buffer
	b.WriteString(versionLine + "\n")
	fmt.Fprintf(&b, "fingerprint %x\n", fp[:])
	for _, r := range recs {
		b.WriteString(r.line + "\n")
		if r.body != nil {
			b.Write(r.body)
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// flags renders the flag set in canonical order.
func (fn FnFact) flags() string {
	var b strings.Builder
	add := func(s string) {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s)
	}
	if fn.ReturnsNonNil {
		add("ReturnsNonNil")
	}
	if fn.AlwaysNilError {
		add("AlwaysNilError")
	}
	if fn.PureConst {
		add("PureConst")
	}
	if fn.ReceiverMutated {
		add("ReceiverMutated")
	}
	if fn.Readonly {
		add("Readonly")
	}
	// Both masks or neither: a producer defect that sets HasAssert with a
	// missing mask must not manufacture a complete "0" pair through
	// normHex's empty-to-"0" mapping; the record stays unknown instead
	// and Decode rejects genuinely partial pairs on the wire.
	if fn.HasAssert && fn.AssertTP != "" && fn.AssertRTP != "" {
		add("AssertTP=" + normHex(fn.AssertTP))
		add("AssertRTP=" + normHex(fn.AssertRTP))
	}
	if fn.ParamMutated != "" {
		add("ParamMutated=" + normHex(fn.ParamMutated))
	}
	if fn.HasMayYield {
		if fn.MayYield {
			add("MayYield=1")
		} else {
			add("MayYield=0")
		}
	}
	return b.String()
}

// parseFlag sets one flag on fn.
func (fn *FnFact) parseFlag(flag string) error {
	switch flag {
	case "ReturnsNonNil":
		fn.ReturnsNonNil = true
	case "AlwaysNilError":
		fn.AlwaysNilError = true
	case "PureConst":
		fn.PureConst = true
	case "ReceiverMutated":
		fn.ReceiverMutated = true
	case "Readonly":
		fn.Readonly = true
	default:
		if rest, ok := strings.CutPrefix(flag, "ParamMutated="); ok {
			if !isHexMask(rest) {
				return fmt.Errorf("bad ParamMutated flag %q (want hex mask)", flag)
			}
			fn.ParamMutated = normHex(rest)
			return nil
		}
		if rest, ok := strings.CutPrefix(flag, "AssertTP="); ok {
			if !isHexMask(rest) {
				return fmt.Errorf("bad AssertTP flag %q (want hex mask)", flag)
			}
			fn.AssertTP = normHex(rest)
			fn.HasAssert = true
			return nil
		}
		if rest, ok := strings.CutPrefix(flag, "AssertRTP="); ok {
			if !isHexMask(rest) {
				return fmt.Errorf("bad AssertRTP flag %q (want hex mask)", flag)
			}
			fn.AssertRTP = normHex(rest)
			fn.HasAssert = true
			return nil
		}
		if rest, ok := strings.CutPrefix(flag, "MayYield="); ok {
			if rest != "0" && rest != "1" {
				return fmt.Errorf("bad MayYield flag %q (want 0 or 1)", flag)
			}
			fn.HasMayYield = true
			fn.MayYield = rest == "1"
			return nil
		}
		return fmt.Errorf("unknown fn flag %q", flag)
	}
	return nil
}

// HighestAssertBit returns the index of the highest set bit of a hex
// mask string and whether s is a valid mask. Consumers reject summaries
// whose highest bit lies outside the declaration's real parameter arity
// instead of silently ignoring the excess bits.
func HighestAssertBit(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	if strings.Trim(s, "0123456789abcdef") != "" {
		return 0, false
	}
	trimmed := strings.TrimLeft(s, "0")
	if trimmed == "" {
		return -1, true // proven zero: no bit set
	}
	top := 4*(len(trimmed)-1) + bits.Len8(uint8(nibbleValue(trimmed[0]))) - 1
	return top, true
}

func nibbleValue(c byte) int {
	if c > '9' {
		return int(c - 'a' + 10)
	}
	return int(c - '0')
}

// isHexMask reports whether s is a non-empty run of hex digits. A mask
// has any digit count: normHex strips leading zeros, so "1" is valid.
func isHexMask(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdefABCDEF") == ""
}

// normHex lowercases a hex mask and strips leading zeros.
func normHex(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimLeft(s, "0")
	if s == "" {
		s = "0"
	}
	return s
}

type decoder struct {
	data []byte
	off  int
	line int
}

// header consumes one '\n'-terminated line, reporting its line number.
func (d *decoder) header() (ln int, s string, err error) {
	ln = d.line
	i := bytes.IndexByte(d.data[d.off:], '\n')
	if i < 0 {
		return ln, "", fmt.Errorf("logos facts line %d: truncated record %q", ln, d.data[d.off:])
	}
	s = string(d.data[d.off : d.off+i])
	d.off += i + 1
	d.line++
	return ln, s, nil
}

// Decode parses one __.LOGOS member. Every error names the line number.
func Decode(data []byte) (*Facts, error) {
	d := &decoder{data: data, line: 1}
	f := &Facts{}

	_, magic, err := d.header()
	if err != nil {
		if len(data) == 0 {
			return nil, fmt.Errorf("logos facts line 1: empty facts member, want %q", versionLine)
		}
		return nil, err
	}
	if magic != versionLine {
		return nil, fmt.Errorf("logos facts line 1: unsupported version %q (want %q)", magic, versionLine)
	}

	_, fpLine, err := d.header()
	if err != nil {
		return nil, err
	}
	fpFields := strings.Fields(fpLine)
	if len(fpFields) != 2 || fpFields[0] != "fingerprint" {
		return nil, fmt.Errorf(`logos facts line 2: bad fingerprint line %q (want "fingerprint <hex>")`, fpLine)
	}
	raw, err := hex.DecodeString(fpFields[1])
	if err != nil || len(raw) != len(f.Fingerprint) {
		return nil, fmt.Errorf("logos facts line 2: bad fingerprint %q (want %d hex chars)", fpFields[1], 2*len(f.Fingerprint))
	}
	copy(f.Fingerprint[:], raw)

	prev := ""
	for d.off < len(d.data) {
		ln, line, err := d.header()
		if err != nil {
			return nil, err
		}
		if line == "" {
			return nil, fmt.Errorf("logos facts line %d: blank line", ln)
		}
		if line <= prev {
			return nil, fmt.Errorf("logos facts line %d: unsorted records (%q after %q)", ln, line, prev)
		}
		prev = line
		fields := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "opt ") || strings.HasPrefix(line, "const ") || strings.HasPrefix(line, "readonly "):
			if len(fields) != 3 {
				return nil, fmt.Errorf("logos facts line %d: bad %s record %q (want %q)", ln, fields[0], line, fields[0]+" <object> <path>")
			}
			if !validObject(fields[1]) {
				return nil, fmt.Errorf("logos facts line %d: bad object %q", ln, fields[1])
			}
			if !validPath(fields[2]) {
				return nil, fmt.Errorf("logos facts line %d: bad path %q", ln, fields[2])
			}
			switch fields[0] {
			case "opt":
				f.Opts = append(f.Opts, PathFact{Object: fields[1], Path: fields[2]})
			case "const":
				f.Consts = append(f.Consts, PathFact{Object: fields[1], Path: fields[2]})
			default:
				f.Readonlys = append(f.Readonlys, ReadonlyFact{Object: fields[1], Path: fields[2]})
			}
		case line == "lang go":
			f.GoPackage = true
		case strings.HasPrefix(line, "fn "):
			if len(fields) != 3 {
				return nil, fmt.Errorf("logos facts line %d: bad fn record %q (want %q)", ln, line, "fn <object> <flag>[,<flag>...]")
			}
			var fn FnFact
			fn.Object = fields[1]
			if !validObject(fn.Object) {
				return nil, fmt.Errorf("logos facts line %d: bad object %q", ln, fn.Object)
			}
			for _, flag := range strings.Split(fields[2], ",") {
				if ferr := fn.parseFlag(flag); ferr != nil {
					return nil, fmt.Errorf("logos facts line %d: %v", ln, ferr)
				}
			}
			// Trust boundary: an assertion-effect record carries both
			// masks or neither. A partial pair must not certify the
			// missing side as proven zero; neither flag stays unknown.
			if (fn.AssertTP == "") != (fn.AssertRTP == "") {
				return nil, fmt.Errorf("logos facts line %d: incomplete assertion-effect pair on %q (want both AssertTP and AssertRTP or neither)", ln, fn.Object)
			}
			f.Fns = append(f.Fns, fn)
		case strings.HasPrefix(line, "prop "):
			if len(fields) != 3 {
				return nil, fmt.Errorf(`logos facts line %d: bad prop record %q (want "prop <interface> <prefix>")`, ln, line)
			}
			if !validObject(fields[1]) {
				return nil, fmt.Errorf("logos facts line %d: bad interface %q", ln, fields[1])
			}
			if !validPrefix(fields[2]) {
				return nil, fmt.Errorf("logos facts line %d: bad accessor prefix %q", ln, fields[2])
			}
			f.Props = append(f.Props, PropFact{Interface: fields[1], Prefix: fields[2]})
		case strings.HasPrefix(line, "conforms "):
			if len(fields) != 4 {
				return nil, fmt.Errorf(`logos facts line %d: bad conforms record %q (want "conforms <struct> <package> <interface>")`, ln, line)
			}
			if !validObject(fields[1]) {
				return nil, fmt.Errorf("logos facts line %d: bad struct %q", ln, fields[1])
			}
			if fields[2] == "" {
				return nil, fmt.Errorf("logos facts line %d: bad package %q", ln, fields[2])
			}
			if !isIdent(fields[3]) {
				return nil, fmt.Errorf("logos facts line %d: bad interface %q", ln, fields[3])
			}
			f.Conforms = append(f.Conforms, ConformsFact{Type: fields[1], IfacePath: fields[2], Iface: fields[3]})
		case strings.HasPrefix(line, "default "):
			if len(fields) != 4 {
				return nil, fmt.Errorf(`logos facts line %d: bad default record %q (want "default <type> <field> <length>")`, ln, line)
			}
			if !validObject(fields[1]) {
				return nil, fmt.Errorf("logos facts line %d: bad type %q", ln, fields[1])
			}
			if !isIdent(fields[2]) {
				return nil, fmt.Errorf("logos facts line %d: bad field %q", ln, fields[2])
			}
			n, aerr := strconv.Atoi(fields[3])
			if aerr != nil || n < 0 {
				return nil, fmt.Errorf("logos facts line %d: bad default length %q", ln, fields[3])
			}
			if len(d.data)-d.off < n {
				return nil, fmt.Errorf("logos facts line %d: truncated default body for %q (want %d bytes, have %d)", ln, fields[1], n, len(d.data)-d.off)
			}
			text := string(bytes.Clone(d.data[d.off : d.off+n]))
			d.off += n
			d.line += bytes.Count([]byte(text), []byte{'\n'})
			if d.off < len(d.data) {
				if d.data[d.off] != '\n' {
					return nil, fmt.Errorf("logos facts line %d: want newline after default body", ln)
				}
				d.off++
				d.line++
			}
			f.Defaults = append(f.Defaults, DefaultFact{Type: fields[1], Field: fields[2], Text: text})
		case strings.HasPrefix(line, "comptime "):
			if len(fields) != 3 {
				return nil, fmt.Errorf(`logos facts line %d: bad comptime record %q (want "comptime <object> <length>")`, ln, line)
			}
			if !validObject(fields[1]) {
				return nil, fmt.Errorf("logos facts line %d: bad object %q", ln, fields[1])
			}
			n, aerr := strconv.Atoi(fields[2])
			if aerr != nil || n < 0 {
				return nil, fmt.Errorf("logos facts line %d: bad comptime length %q", ln, fields[2])
			}
			if len(d.data)-d.off < n {
				return nil, fmt.Errorf("logos facts line %d: truncated comptime body for %q (want %d bytes, have %d)", ln, fields[1], n, len(d.data)-d.off)
			}
			src := bytes.Clone(d.data[d.off : d.off+n])
			d.off += n
			d.line += bytes.Count(src, []byte{'\n'})
			if d.off < len(d.data) {
				if d.data[d.off] != '\n' {
					return nil, fmt.Errorf("logos facts line %d: want newline after comptime body", ln)
				}
				d.off++
				d.line++
			}
			f.Comptimes = append(f.Comptimes, ComptimeFact{Object: fields[1], Source: src})
		case strings.HasPrefix(line, "constfn "):
			if len(fields) != 3 {
				return nil, fmt.Errorf(`logos facts line %d: bad constfn record %q (want "constfn <object> <length>")`, ln, line)
			}
			if !validObject(fields[1]) {
				return nil, fmt.Errorf("logos facts line %d: bad object %q", ln, fields[1])
			}
			n, aerr := strconv.Atoi(fields[2])
			if aerr != nil || n <= 0 {
				return nil, fmt.Errorf("logos facts line %d: bad constfn length %q", ln, fields[2])
			}
			if len(d.data)-d.off < n {
				return nil, fmt.Errorf("logos facts line %d: truncated constfn body for %q (want %d bytes, have %d)", ln, fields[1], n, len(d.data)-d.off)
			}
			src := bytes.Clone(d.data[d.off : d.off+n])
			d.off += n
			d.line += bytes.Count(src, []byte{'\n'})
			if d.off >= len(d.data) || d.data[d.off] != '\n' {
				return nil, fmt.Errorf("logos facts line %d: want newline after constfn body", ln)
			}
			d.off++
			d.line++
			for _, c := range f.ConstFuncs {
				if c.Object == fields[1] {
					return nil, fmt.Errorf("logos facts line %d: duplicate constfn object %q", ln, fields[1])
				}
			}
			f.ConstFuncs = append(f.ConstFuncs, ConstFuncFact{Object: fields[1], Source: src})
		default:
			kind := line
			if k := strings.Fields(line); len(k) > 0 {
				kind = k[0]
			}
			return nil, fmt.Errorf("logos facts line %d: unknown record %q", ln, kind)
		}
	}
	return f, nil
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) {
			continue
		}
		if i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return true
}

// ValidObject reports whether s names a record object in the grammar
// Decode accepts, so a producer can skip names it cannot spell.
func ValidObject(s string) bool { return validObject(s) }

func validObject(s string) bool {
	recv, _, _ := strings.Cut(s, "@")
	if before, after, ok := strings.CutLast(recv, "."); ok {
		if !isIdent(after) {
			return false
		}
		recv = before
	}
	if recv == "" {
		return false
	}
	if isIdent(recv) {
		return true
	}
	if len(recv) >= 2 && recv[0] == '(' && recv[len(recv)-1] == ')' {
		inner := strings.TrimPrefix(recv[1:len(recv)-1], "*")
		return isIdent(inner)
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// validStep reports whether s is one path step: R<i>, P<i>, F<name>, E, D,
// K, M<name>, T<i>, G<i>, or H<i>.
func validStep(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case 'R', 'P', 'T', 'G', 'H':
		return isDigits(s[1:])
	case 'F', 'M':
		return isIdent(s[1:])
	case 'D', 'E', 'K':
		return len(s) == 1
	}
	return false
}

// validPath reports whether s is a dot-separated path of valid steps.
func validPath(s string) bool {
	if s == "" {
		return false
	}
	for step := range strings.SplitSeq(s, ".") {
		if !validStep(step) {
			return false
		}
	}
	return true
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '.' && r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

var (
	currentMu    sync.RWMutex
	currentFacts *Facts

	importedMu sync.RWMutex
	imported   = make(map[string]*Facts)
)

// Current returns the facts collected for the package being compiled, or
// nil when no producer has run. S2-S7 producers install it through
// SetCurrent before the object dump.
func Current() *Facts {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return currentFacts
}

// SetCurrent installs the facts for the package being compiled. Later
// slices call it during lowering, before dumpobj1 writes the archive.
func SetCurrent(f *Facts) {
	currentMu.Lock()
	defer currentMu.Unlock()
	currentFacts = f
}

// Import verifies a __.LOGOS member against the __.PKGDEF export data,
// decodes it, and records it under path for the importer overlay (S2-S7).
// A fingerprint mismatch means the archive mixes export data and facts
// from different builds of the package.
func Import(path, exportData string, member []byte) (*Facts, error) {
	f, err := Decode(member)
	if err != nil {
		return nil, fmt.Errorf("logos: bad facts for %s: %v", path, err)
	}
	var want goobj.FingerprintType
	if len(exportData) < len(want) {
		return nil, fmt.Errorf("logos: stale facts for %s (export data too short for fingerprint)", path)
	}
	copy(want[:], exportData[len(exportData)-len(want):])
	if f.Fingerprint != want {
		return nil, fmt.Errorf("logos: stale facts for %s (fingerprint mismatch)", path)
	}
	importedMu.Lock()
	defer importedMu.Unlock()
	imported[path] = f
	return f, nil
}

// Lookup returns the facts Import recorded for path.
func Lookup(path string) (*Facts, bool) {
	importedMu.RLock()
	defer importedMu.RUnlock()
	f, ok := imported[path]
	return f, ok
}

// FindMember returns the raw __.LOGOS member of the archive f, or nil when
// the archive predates the sidecar. It seeks, so the caller's file offset
// is not preserved.
func FindMember(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("logos: cannot seek facts archive: %v", err)
	}
	var sig [len(arMagic)]byte
	if _, err := io.ReadFull(f, sig[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("logos: cannot read facts archive: %v", err)
	}
	if string(sig[:]) != arMagic {
		return nil, nil
	}
	for {
		var hdr [arHeaderSize]byte
		_, err := io.ReadFull(f, hdr[:])
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("logos: cannot read facts archive header: %v", err)
		}
		if hdr[58] != '`' || hdr[59] != '\n' {
			return nil, fmt.Errorf("logos: corrupt facts archive header")
		}
		name := strings.TrimRight(string(hdr[:16]), " ")
		size, err := strconv.ParseUint(strings.TrimRight(string(hdr[48:58]), " "), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("logos: corrupt facts archive size for %q", name)
		}
		if name == MemberName {
			if size > maxMemberBytes {
				return nil, fmt.Errorf("logos: facts member too large (%d bytes)", size)
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(f, data); err != nil {
				return nil, fmt.Errorf("logos: truncated facts member: %v", err)
			}
			return data, nil
		}
		if _, err := f.Seek(int64(size+(size&1)), io.SeekCurrent); err != nil {
			return nil, fmt.Errorf("logos: cannot skip facts archive member %q: %v", name, err)
		}
	}
}
