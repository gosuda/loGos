// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"
	"strings"
)

func init() {
	bootstrapDirs = append(bootstrapDirs, "cmd/internal/logosowned")
}

// bootstrapLogosVendorModules lists the exact vendored x/mod packages
// required by cmd/internal/logosowned (via golang.org/x/mod/module).
// module imports semver and internal/lazyregexp; both use only the
// standard library. Sources live at src/cmd/vendor/<module> and are
// copied to unvendored bootstrap logical paths bootstrap/<module>.
var bootstrapLogosVendorModules = []string{
	"golang.org/x/mod/module",
	"golang.org/x/mod/semver",
	"golang.org/x/mod/internal/lazyregexp",
}

// bootstrapRewriteLogosImport rewrites an unvendored x/mod import to its
// bootstrap logical path. Unknown paths are returned unchanged.
func bootstrapRewriteLogosImport(path string) string {
	for _, mod := range bootstrapLogosVendorModules {
		if path == mod {
			return "bootstrap/" + path
		}
	}
	return path
}

// bootstrapCopyLogosVendor copies the vendored x/mod sources above into
// unvendored bootstrap logical paths under base, rewriting imports with
// the same bootstrapRewriteFile machinery as the main bootstrap walk.
func bootstrapCopyLogosVendor(base string) {
	for _, mod := range bootstrapLogosVendorModules {
		srcRoot := pathf("%s/src/cmd/vendor/%s", goroot, mod)
		dstRoot := pathf("%s/%s", base, mod)
		filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				fatalf("walking logos vendor dirs failed: %v: %v", path, err)
			}
			rel, err := filepath.Rel(srcRoot, path)
			if err != nil {
				fatalf("rel logos vendor path failed: %v: %v", path, err)
			}
			if info.IsDir() {
				if rel != "." && (info.Name() == "testdata") {
					return filepath.SkipDir
				}
				xmkdirall(pathf("%s/%s", dstRoot, relToDst(rel)))
				return nil
			}
			name := info.Name()
			for _, pre := range ignorePrefixes {
				if strings.HasPrefix(name, pre) {
					return nil
				}
			}
			for _, suf := range ignoreSuffixes {
				if strings.HasSuffix(name, suf) {
					return nil
				}
			}
			text := bootstrapRewriteFile(path)
			writefile(text, pathf("%s/%s", dstRoot, rel), 0)
			return nil
		})
	}
}

// relToDst maps "." (the module root itself) to "" so the root mkdir
// targets dstRoot instead of dstRoot/..
func relToDst(rel string) string {
	if rel == "." {
		return ""
	}
	return rel
}

// installLogos copies the just installed go binary to logos in the same
// directory, using the suffix for the current target.
func installLogos() {
	dir := gorootBin
	suffix := exe
	if goos != gohostos || goarch != gohostarch {
		dir = pathf("%s/bin/%s_%s", goroot, goos, goarch)
		suffix = ""
		if goos == "windows" {
			suffix = ".exe"
		}
	}
	copyfile(pathf("%s/logos"+suffix, dir), pathf("%s/go"+suffix, dir), writeExec)
}
