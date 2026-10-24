// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package logosowned carries the eject transport for generated owner
// source: one compiler action writes a versioned JSON sidecar named
// FileName in its per-package staging directory, and the eject
// coordinator collects the sidecars before staging is removed. The
// document identifies generated helper files by content hash, never by
// a self-referential type name.
package logosowned

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"slices"
	"strings"

	"golang.org/x/mod/module"
)

// The wire format uses encoding/json, not encoding/json/v2: the
// bootstrap toolchain builds this package for cmd/compile, and v2 is an
// experiment there (see editmap.go for the same rule in cmd/compile).
// cmd/go builds in every experiment configuration, so a gated import
// would break GOEXPERIMENT=nojsonv2.

// FileName is the sidecar name one compiler action writes in its
// per-package staging directory.
const FileName = "logos_owned.json"

// version is the only envelope version Decode accepts.
const version = 1

// Declaration names one checked source declaration on which a helper
// depends. Owner is the canonical package import path holding the
// declaration. File is the physical declaration file's basename (for
// example private_amd64.go), never a virtual //line filename.
type Declaration struct {
	Owner string `json:"owner"`
	File  string `json:"file"`
}

// Request describes one generated helper file owned by the package at
// Owner. Package is the Go package identifier of both the owner and
// the Source file. ID is the SHA-256 hex of the formatted Source
// bytes, so the file name derives from content, not from a type name.
// Declarations is the checked declaration provenance: every source
// declaration the helper was generated from, canonicalized and sorted.
// An empty (non-null) array is valid for a helper with no source
// declaration dependency. Null or missing declarations are invalid.
type Request struct {
	Owner        string        `json:"owner"`
	Package      string        `json:"package"`
	ID           string        `json:"id"`
	Source       string        `json:"source"`
	Declarations []Declaration `json:"declarations"`
}

// envelope is the canonical framed document. Requests is a pointer so
// Decode can tell a present (possibly empty) array from a missing or
// null one; the schema requires the array.
type envelope struct {
	Version  int        `json:"version"`
	Requests *[]Request `json:"requests"`
}

// NewRequest formats source and returns the Request identifying it.
// The ID is the SHA-256 hex of the formatted bytes, so identical
// sources share an ID and any content change is a loud mismatch at
// decode time, never a silent overwrite. declarations carries the
// checked provenance; it must be non-nil (empty allowed) and is
// canonicalized (sorted, deduplicated) before storage. The ID hashes
// only the formatted Source, so the same Source with distinct
// provenance keeps the same ID and survives until condition
// composition merges same-body variants.
func NewRequest(owner, pkg, source string, declarations []Declaration) (Request, error) {
	if err := checkOwner(owner); err != nil {
		return Request{}, err
	}
	if err := checkPackageName(pkg); err != nil {
		return Request{}, err
	}
	if declarations == nil {
		return Request{}, fmt.Errorf("logosowned: missing declarations array (empty allowed, null rejected)")
	}
	canon, err := canonicalizeDeclarations(declarations)
	if err != nil {
		return Request{}, err
	}
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return Request{}, fmt.Errorf("logosowned: source does not parse: %v", err)
	}
	if err := checkPackageClause(pkg, formatted); err != nil {
		return Request{}, err
	}
	sum := sha256.Sum256(formatted)
	return Request{
		Owner:        owner,
		Package:      pkg,
		ID:           hex.EncodeToString(sum[:]),
		Source:       string(formatted),
		Declarations: canon,
	}, nil
}

// canonicalizeDeclarations validates every declaration and returns the
// sorted, deduplicated list. A non-nil empty input stays a non-nil
// empty output so Encode renders [] rather than null.
func canonicalizeDeclarations(decls []Declaration) ([]Declaration, error) {
	if decls == nil {
		return nil, fmt.Errorf("missing declarations array")
	}
	for i := range decls {
		if err := checkDeclaration(&decls[i]); err != nil {
			return nil, fmt.Errorf("declaration %d: %v", i, err)
		}
	}
	out := slices.Clone(decls)
	slices.SortFunc(out, func(a, b Declaration) int {
		if c := strings.Compare(a.Owner, b.Owner); c != 0 {
			return c
		}
		return strings.Compare(a.File, b.File)
	})
	out = slices.CompactFunc(out, func(a, b Declaration) bool {
		return a.Owner == b.Owner && a.File == b.File
	})
	if out == nil {
		out = []Declaration{}
	}
	return out, nil
}

// Encode frames reqs as the canonical versioned document. Every
// request is validated, so a malformed owner, package, ID, source, or
// declaration list fails here instead of reaching the coordinator.
func Encode(reqs []Request) ([]byte, error) {
	if reqs == nil {
		reqs = []Request{}
	}
	for i := range reqs {
		if err := checkRequest(&reqs[i]); err != nil {
			return nil, fmt.Errorf("logosowned: request %d: %v", i, err)
		}
	}
	return json.Marshal(envelope{Version: version, Requests: &reqs})
}

// Decode validates the framing and every request of data. Unknown
// fields, a wrong version, and trailing content are errors, as are an
// invalid owner, package, ID, declaration list, or a content hash
// mismatch. A trailing newline is accepted, so a producer that ends
// the file with one still decodes.
func Decode(data []byte) ([]Request, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("logosowned: decode sidecar: %v", err)
	}
	if env.Version != version {
		return nil, fmt.Errorf("logosowned: unsupported version %d", env.Version)
	}
	if env.Requests == nil {
		return nil, fmt.Errorf("logosowned: missing requests array")
	}
	reqs := *env.Requests
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("logosowned: trailing data after document")
	}
	for i := range reqs {
		if err := checkRequest(&reqs[i]); err != nil {
			return nil, fmt.Errorf("logosowned: request %d: %v", i, err)
		}
	}
	return reqs, nil
}

// checkRequest validates one request, including that ID is the
// content hash of Source, the package clause names Package, and the
// declaration list is present with valid entries.
func checkRequest(r *Request) error {
	if err := checkOwner(r.Owner); err != nil {
		return err
	}
	if err := checkPackageName(r.Package); err != nil {
		return err
	}
	if err := checkID(r.ID); err != nil {
		return err
	}
	if r.Source == "" {
		return fmt.Errorf("empty source")
	}
	if r.Declarations == nil {
		return fmt.Errorf("missing declarations array (empty allowed, null rejected)")
	}
	for i := range r.Declarations {
		if err := checkDeclaration(&r.Declarations[i]); err != nil {
			return fmt.Errorf("declaration %d: %v", i, err)
		}
	}
	// Declarations must already be canonical: sorted and deduplicated.
	if !slices.IsSortedFunc(r.Declarations, func(a, b Declaration) int {
		if c := strings.Compare(a.Owner, b.Owner); c != 0 {
			return c
		}
		return strings.Compare(a.File, b.File)
	}) {
		return fmt.Errorf("declarations not canonical (sorted)")
	}
	for i := 1; i < len(r.Declarations); i++ {
		if r.Declarations[i] == r.Declarations[i-1] {
			return fmt.Errorf("duplicate declaration %q:%q", r.Declarations[i].Owner, r.Declarations[i].File)
		}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "owned.go", []byte(r.Source), parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("source does not parse: %v", err)
	}
	if f.Name.Name != r.Package {
		return fmt.Errorf("package clause %q does not match %q", f.Name.Name, r.Package)
	}
	sum := sha256.Sum256([]byte(r.Source))
	if got := hex.EncodeToString(sum[:]); got != r.ID {
		return fmt.Errorf("id mismatch: want content hash %s", got)
	}
	return nil
}

// checkDeclaration validates one provenance entry: a canonical import
// path owner and a physical basename file.
func checkDeclaration(d *Declaration) error {
	if err := checkOwner(d.Owner); err != nil {
		return err
	}
	if err := checkDeclarationFile(d.File); err != nil {
		return err
	}
	return nil
}

// checkDeclarationFile accepts only a safe physical source basename:
// no directory, no virtual //line target. Extension membership is
// decided by the actual package plan, not here.
func checkDeclarationFile(file string) error {
	if file == "" {
		return fmt.Errorf("empty declaration file")
	}
	if file == "." || file == ".." {
		return fmt.Errorf("invalid declaration file %q", file)
	}
	if strings.ContainsAny(file, `/\`) {
		return fmt.Errorf("declaration file %q is not a basename", file)
	}
	if strings.Contains(file, ":") {
		return fmt.Errorf("declaration file %q is not a physical basename", file)
	}
	return nil
}

// checkOwner rejects anything module.CheckImportPath rejects, so the
// owner is always a canonical import path the coordinator can resolve.
func checkOwner(owner string) error {
	if owner == "" {
		return fmt.Errorf("empty owner")
	}
	if err := module.CheckImportPath(owner); err != nil {
		return fmt.Errorf("invalid owner import path %q: %v", owner, err)
	}
	return nil
}

// checkPackageName accepts only a Go package identifier: a valid
// identifier that is not a keyword.
func checkPackageName(name string) error {
	if name == "" {
		return fmt.Errorf("empty package name")
	}
	if name == "_" {
		return fmt.Errorf("blank package name")
	}
	if !token.IsIdentifier(name) || token.Lookup(name) != token.IDENT {
		return fmt.Errorf("invalid package name %q", name)
	}
	return nil
}

// checkPackageClause parses formatted Go source far enough to read its
// package clause and reports a mismatch with pkg.
func checkPackageClause(pkg string, formatted []byte) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "owned.go", formatted, parser.PackageClauseOnly)
	if err != nil {
		return fmt.Errorf("logosowned: source does not parse: %v", err)
	}
	if f.Name.Name != pkg {
		return fmt.Errorf("logosowned: package clause %q does not match %q", f.Name.Name, pkg)
	}
	return nil
}

// checkID accepts only a 64-character lowercase hex digest.
func checkID(id string) error {
	if len(id) != 64 {
		return fmt.Errorf("invalid id length %d", len(id))
	}
	for i := range len(id) {
		c := id[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return fmt.Errorf("invalid id character %q", c)
	}
	return nil
}
