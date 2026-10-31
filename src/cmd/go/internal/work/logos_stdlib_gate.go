// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package work

import (
	"strings"

	"cmd/go/internal/load"
	"cmd/go/internal/search"
)

const (
	logosVariableMakeFlag = "-d=variablemakethreshold="
	logosDebugFlagPrefix  = "-d="
	logosDebugKeySep      = ","
)

// logosForcedGcflags returns the forced compile flags for p. The stack-buffer
// optimization changes slice capacity and allocation counts that the Go
// standard library's own tests pin, and upstream Go 1.27.3 fails those same
// tests when the flag is on, so standard-library packages never receive it.
// The level keeps applying to every package through its other flags.
func logosForcedGcflags(p *load.Package) []string {
	if !logosStandardProvenance(p) {
		return forcedGcflags
	}
	out := make([]string, 0, len(forcedGcflags))
	for _, f := range forcedGcflags {
		out = append(out, logosDropVariableMake(f))
	}
	return out
}

// logosStandardProvenance reports whether p compiles as standard-library
// provenance. A standard package's external test package counts, because its
// tests pin the same capacities and test package construction copies Goroot
// but not Standard. The measurement of a level's own buffer lives outside the
// standard library (the WANT_STACKBUF fixture embedded in
// src/cmd/go/testdata/script/logos_opt_levels.txt), so this strip never hides
// the tier.
func logosStandardProvenance(p *load.Package) bool {
	if p.Standard {
		return true
	}
	return p.ForTest != "" && search.IsStandardImportPath(p.ForTest)
}

// logosDropVariableMake removes the variable-make key from a -d flag value and
// returns the flag unchanged when the key is absent. A value left without keys
// yields the empty string (the StringList in cmd/go/internal/str appends
// unconditionally, it does not drop empty values; tier flag sets always retain
// other keys such as inlbudget, so kept is never empty in practice).
func logosDropVariableMake(flag string) string {
	value, ok := strings.CutPrefix(flag, logosDebugFlagPrefix)
	if !ok {
		return flag
	}
	keys := strings.Split(value, logosDebugKeySep)
	kept := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, found := strings.CutPrefix(k, strings.TrimPrefix(logosVariableMakeFlag, logosDebugFlagPrefix)); found {
			continue
		}
		kept = append(kept, k)
	}
	if len(kept) == len(keys) {
		return flag
	}
	if len(kept) == 0 {
		return ""
	}
	return logosDebugFlagPrefix + strings.Join(kept, logosDebugKeySep)
}
