// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SourceHash returns the deterministic content digest of the helper module
// rooted at root.
//
// Every regular file under root feeds the digest except whole testdata
// subtrees, which never affect the built helper binary. Both content and
// slash-normalized relative path feed the hash, so renames invalidate the
// digest. The file list is sorted, so the result is stable across walks.
//
// Module archives rename every go.mod to _go.mod (see cmd/distpack
// Archive.RenameGoMod). A distributed install therefore holds
// lib/logos-nogo/_go.mod where the source tree holds lib/logos-nogo/go.mod.
// SourceHash maps a trailing _go.mod back to go.mod before hashing, so the
// sidecar digest written at build time matches the digest recomputed from an
// installed tree. Both names present under one directory is an error.
func SourceHash(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source hash root %q is not a directory", root)
	}
	type entry struct {
		norm string
		full string
	}
	var entries []entry
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		norm := filepath.ToSlash(rel)
		if norm == "_go.mod" {
			norm = "go.mod"
		} else if dir, ok := strings.CutSuffix(norm, "/_go.mod"); ok {
			norm = dir + "/go.mod"
		}
		entries = append(entries, entry{norm: norm, full: path})
		return nil
	})
	if err != nil {
		return "", err
	}
	slices.SortFunc(entries, func(a, b entry) int {
		return strings.Compare(a.norm, b.norm)
	})
	seen := make(map[string]string, len(entries))
	h := sha256.New()
	for _, e := range entries {
		if prev, dup := seen[e.norm]; dup {
			return "", fmt.Errorf("duplicate normalized path %q (%q and %q)", e.norm, prev, e.full)
		}
		seen[e.norm] = e.full
		data, err := os.ReadFile(e.full)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "file %s %x\n", e.norm, sha256.Sum256(data))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
