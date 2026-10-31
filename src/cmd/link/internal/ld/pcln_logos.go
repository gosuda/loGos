// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ld

import (
	"internal/abi"
	"strings"
)

// Clone suffixes (SPEC T-203.4, T-205, red-team #9, #74). Values are the
// single source of truth in internal/abi (Logos*Infix); the local names
// stay as short aliases so call sites read plainly. The ASCII spelling
// must stay in sync with cfCopyInfix in
// cmd/compile/internal/logos/comptime.go. The dotted ASCII form
// ".logosspec" is ordinary text and is never a suffix.
const (
	logosSpecDisplayInfix = abi.LogosSpecDisplayInfix
	logosCtDisplayInfix   = abi.LogosCtDisplayInfix
	// logosCtASCIIInfix is the source-level spelling of a comptime
	// copy suffix.
	logosCtASCIIInfix = abi.LogosCtASCIIInfix
)

// logosCloneInfixes lists every terminal clone suffix. One table-driven
// path handles them all: adding a suffix spelling is a table entry, not a
// new branch.
var logosCloneInfixes = []string{
	logosSpecDisplayInfix,
	logosCtDisplayInfix,
	logosCtASCIIInfix,
}

// Naming rule (SPEC T-203.4, T-205, red-team #9, #74): pclntab
// (runtime.funcnametab) strips every terminal clone suffix to the base so
// traces print <f> and FuncForPC resolves to the base name. The link
// symbol and the symbol table (go tool nm) keep the full canonical
// suffix, so every clone stays stable, unique, and resolvable at link
// time; only the pclntab copy is shortened. One generateFuncnametab path
// in pcln.go serves shared, plugin, and internal links, so the base
// display survives every link mode. logosCloneBaseName below performs the
// identical strip for capability sink identity (capSinkName); the two
// must stay identical.
func logosFuncDisplayName(name string) string {
	return logosCloneBaseName(name)
}

// logosCloneBaseName returns the base name with every terminal clone
// suffix stripped: the middle-dot infixes ·logosspec and ·logosct, and the
// source-level comptime infix _logosct (a clone of a comptime copy reads
// <f>_logosct<M>·logosspec<N>), each with a nonempty ASCII decimal
// sequence number. It preserves everything else: package path, method
// receiver, and generic shape spelling. Capability sink matching
// (capSinkName) uses the base for sink identity; pclntab storage uses
// logosFuncDisplayName above, which performs the identical strip.
func logosCloneBaseName(name string) string {
	orig := name
	for {
		i := len(name)
		for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
			i--
		}
		if i == len(name) {
			return name
		}
		prefix := name[:i]
		var base string
		stripped := false
		for _, infix := range logosCloneInfixes {
			if b, ok := strings.CutSuffix(prefix, infix); ok {
				base, stripped = b, true
				break
			}
		}
		if !stripped {
			return name
		}
		if base == "" {
			return orig
		}
		name = base
	}
}
