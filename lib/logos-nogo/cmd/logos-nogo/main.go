// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Binary logos-nogo performs static analysis over a single package.
//
// It is the per-package helper invoked by cmd/go's logos lint integration:
// cmd/go assembles export-data (-import), object archives (-archive), and
// dependency facts (-facts, -bundle) plus the target configuration
// (-GOOS/-GOARCH/-GOVERSION/-tags/-race/-msan), then this binary runs the
// full analyzer registry over the named package's sources and emits the
// package's own facts file plus its findings set.
//
// Analyzer failures are never reported as clean: a failed analysis (non-nil
// error from check.Package) exits nonzero without writing outputs, and
// per-analyzer panics surface as error findings in the findings stream via
// the driver's recovery path, never as an empty result.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/mod/modfile"
	"logos/nogo/internal/check"
	"logos/nogo/internal/flags"
)

var (
	pkgName        = flag.String("package", "main", "package import path for analysis (required for a package run)")
	listMode       = flag.Bool("list", false, "print the deterministic analyzer census (one name per line) and exit")
	versionFlag    = flag.String("V", "", "print tool identity; only -V=full is supported")
	findingsOut    = flag.String("findings", "", "findings output file (default stdout)")
	factsOut       = flag.String("facts-output", "", "facts output file for this package (discarded when empty)")
	jsonOut        = flag.Bool("json", false, "encode findings as JSON instead of gob")
	textOut        = flag.Bool("text", false, "print findings as human-readable text")
	sourceHashRoot = flag.String("source-hash", "", "print deterministic source digest for root and exit")
)

// resolveGOVERSION sets flags.GOVERSION from flags.GOVERSIONModFile, when set.
//
// Only one of -GOVERSION or -GOVERSION-mod-file may be given. This mirrors the
// reference cli.resolveGOVERSION; cmd/go normally passes -GOVERSION directly.
func resolveGOVERSION() error {
	if flags.GOVERSIONModFile == "" {
		return nil
	}
	b, err := os.ReadFile(flags.GOVERSIONModFile)
	if err != nil {
		return err
	}
	m, err := modfile.Parse(flags.GOVERSIONModFile, b, nil)
	if err != nil {
		return err
	}
	if m.Go == nil {
		return fmt.Errorf("go directive not found in %s", flags.GOVERSIONModFile)
	}
	if flags.GOVERSION != "" {
		return fmt.Errorf("only one of -GOVERSION or -GOVERSION-mod-file may be set")
	}
	flags.GOVERSION = "go" + m.Go.Version
	return nil
}

// toolIdentity returns the full tool identity string.
//
// It replicates the -V protocol required by "go vet" (see
// golang.org/x/tools/go/analysis/internal/analysisflags): the executable
// path plus the sha256 of the executable bytes, so the build system can
// track helper changes by content. The analyzer pins and census live in
// go.mod and behind -list; this line stays protocol-pure so existing
// tool-tracking machinery parses it unchanged.
func toolIdentity() (string, error) {
	progname, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(progname)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	return fmt.Sprintf("%s version devel comments-go-here buildID=%02x",
		progname, h.Sum(nil)), nil
}

// openOutput opens name for writing, or returns def when name is empty.
//
// The caller must call close; it is a no-op for the default sink. An empty
// name selects def (io.Discard for facts without dependents, os.Stdout for
// findings), so all outputs flow through this single path.
func openOutput(name string, def io.Writer) (w io.Writer, close func() error, err error) {
	if name == "" {
		return def, func() error { return nil }, nil
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// removePartial deletes a named partial output, if named.
//
// Unnamed outputs (discard/stdout) need no cleanup.
func removePartial(name string) {
	if name != "" {
		_ = os.Remove(name)
	}
}

// dropOutput closes a failed output and removes its partial file, if named.
//
// Use only on paths where close has not run yet; close-failure paths must
// call removePartial instead to avoid closing twice. A failed run therefore
// never leaves a partial named file behind for downstream consumers to
// mistake for a complete result.
func dropOutput(name string, close func() error) {
	_ = close()
	removePartial(name)
}

func run() int {
	flag.Parse()

	if *versionFlag != "" {
		if *versionFlag != "full" {
			fmt.Fprintf(os.Stderr, "logos-nogo: unsupported -V=%q (only -V=full)\n", *versionFlag)
			return 2
		}
		id, err := toolIdentity()
		if err != nil {
			fmt.Fprintf(os.Stderr, "logos-nogo: %v\n", err)
			return 1
		}
		fmt.Println(id)
		return 0
	}

	if *sourceHashRoot != "" {
		digest, err := SourceHash(*sourceHashRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "logos-nogo: source hash: %v\n", err)
			return 1
		}
		fmt.Println(digest)
		return 0
	}

	if *listMode {
		for _, name := range check.AnalyzerNames() {
			fmt.Println(name)
		}
		return 0
	}

	srcs := flag.Args()
	if len(srcs) == 0 {
		fmt.Fprintf(os.Stderr, "logos-nogo: no source files given (usage: logos-nogo -package <path> [-findings f] [-facts-output f] <srcs...>)\n")
		return 2
	}
	if *pkgName == "" {
		fmt.Fprintf(os.Stderr, "logos-nogo: -package must not be empty\n")
		return 2
	}

	if err := resolveGOVERSION(); err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: %v\n", err)
		return 1
	}

	// Perform the analysis. An error here means the analysis is
	// incomplete: fail explicitly without writing outputs, never report a
	// (possibly empty) subset as clean.
	findings, factData, err := check.Package(*pkgName, srcs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: analysis of %s failed: %v\n", *pkgName, err)
		return 1
	}

	// Publish this package's facts for downstream dependents.
	factsSink, closeFacts, err := openOutput(*factsOut, io.Discard)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: opening facts output: %v\n", err)
		return 1
	}
	if err := factData.Serialize(factsSink); err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: writing facts: %v\n", err)
		dropOutput(*factsOut, closeFacts)
		return 1
	}
	if err := closeFacts(); err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: closing facts output: %v\n", err)
		removePartial(*factsOut)
		return 1
	}

	// Publish findings. The default gob encoding (like the reference
	// internal format) feeds machine consumers; -json selects JSON and
	// -text selects human-readable lines.
	findingsSink, closeFindings, err := openOutput(*findingsOut, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: opening findings output: %v\n", err)
		return 1
	}
	if *textOut {
		findings.Sort()
		var werr error
		for _, finding := range findings {
			if _, err := fmt.Fprintf(findingsSink, "%s\n", finding.String()); err != nil {
				werr = err
				break
			}
		}
		if werr != nil {
			fmt.Fprintf(os.Stderr, "logos-nogo: writing findings: %v\n", werr)
			dropOutput(*findingsOut, closeFindings)
			return 1
		}
	} else if err := check.WriteFindingsTo(findingsSink, findings, *jsonOut); err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: writing findings: %v\n", err)
		dropOutput(*findingsOut, closeFindings)
		return 1
	}
	if err := closeFindings(); err != nil {
		fmt.Fprintf(os.Stderr, "logos-nogo: closing findings output: %v\n", err)
		removePartial(*findingsOut)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run())
}
