// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package config

import (
	"encoding/json"
	"maps"
	"slices"
)

// LintPolicyVersion is the version of the policy JSON the go command passes
// to the compiler as -logos.cfg. The compiler (cmd/compile/internal/logos/lint,
// ParsePolicy) reads the same schema.
const LintPolicyVersion = 1

// lintGroups lists the ten group keys of the [lints] table. The native
// compiler (cmd/compile/internal/logos/lint) knows only the first nine:
// its Groups census and ParsePolicy must keep rejecting "nogo", so the
// -logos.cfg JSON below never carries it. The tenth key, "nogo", is the
// Go-side helper policy for lib/logos-nogo (module logos/nogo): one
// severity governing every helper analyzer in both Go and loGo/s packages
// via its canonical nogo::<lowercase-analyzer> IDs (for example,
// nogo::sa1019, nogo::printf).
var lintGroups = []string{
	"correctness", "concurrency", "slop", "complexity", "perf",
	"style", "suspicious", "portability", "pedantic", "nogo",
}

// lintThresholds lists the six integer keys of [lints.thresholds].
var lintThresholds = []string{
	"max_nesting_depth", "max_cognitive_complexity", "max_function_lines",
	"max_function_params", "max_comptime_ast_nodes", "max_ctfe_steps",
}

// forbidGroups are the groups whose default severity is forbid. A forbid
// group cannot be set to any other severity.
var forbidGroups = []string{"correctness", "concurrency"}

// Lints holds the [lints] table and the [lints.thresholds] table. Only keys
// present in the file are stored: the compiler applies the defaults, so an
// absent table and an empty one mean the same. Groups["nogo"], when present,
// holds the helper severity ("forbid", "deny", "warn", or "allow"); when
// absent the helper defaults to "deny", matching the compiler's
// DefaultSeverity fallback for an unclassified group.
type Lints struct {
	// Groups maps a group name to "forbid", "deny", "warn", or "allow".
	// The "nogo" entry, when present, governs the helper only and never
	// reaches the native compiler policy.
	Groups map[string]string
	// Thresholds maps a threshold key to its positive value.
	Thresholds map[string]int
}

// lintPolicy is the JSON schema of -logos.cfg.
type lintPolicy struct {
	Version    int               `json:"version"`
	Groups     map[string]string `json:"groups"`
	Thresholds map[string]int    `json:"thresholds"`
}

// PolicyJSON returns the native -logos.cfg policy for the lints table: the
// nine compiler groups only, never "nogo". The output is deterministic: it
// feeds the build action ID. The --pedantic flag of logos build and logos
// lint overrides the pedantic key: --pedantic raises the group from allow
// to warn and leaves a stricter configured severity alone, and
// --pedantic=false forces allow. The "nogo" entry is filtered here because
// the native compiler rejects unknown groups; it stays in Lints.Groups for
// the helper driver, which reads the configured severity directly.
func (l Lints) PolicyJSON(pedantic PedanticMode) ([]byte, error) {
	p := lintPolicy{
		Version:    LintPolicyVersion,
		Groups:     map[string]string{},
		Thresholds: map[string]int{},
	}
	for k, v := range l.Groups {
		if k == "nogo" {
			continue
		}
		p.Groups[k] = v
	}
	maps.Copy(p.Thresholds, l.Thresholds)
	switch pedantic {
	case PedanticOn:
		if cur, ok := p.Groups["pedantic"]; !ok || cur == "allow" {
			p.Groups["pedantic"] = "warn"
		}
	case PedanticOff:
		p.Groups["pedantic"] = "allow"
	}
	return json.Marshal(p)
}

// PedanticMode is the state of the --pedantic flag.
type PedanticMode uint8

const (
	// PedanticDefault is the flag absent: logos.toml decides.
	PedanticDefault PedanticMode = iota
	// PedanticOn is --pedantic or --pedantic=true.
	PedanticOn
	// PedanticOff is --pedantic=false.
	PedanticOff
)

// isLintGroup reports whether key is a [lints] group key.
func isLintGroup(key string) bool {
	return slices.Contains(lintGroups, key)
}

// isLintThreshold reports whether key is a [lints.thresholds] key.
func isLintThreshold(key string) bool {
	return slices.Contains(lintThresholds, key)
}

// isForbidGroup reports whether group defaults to forbid.
func isForbidGroup(group string) bool {
	return slices.Contains(forbidGroups, group)
}
