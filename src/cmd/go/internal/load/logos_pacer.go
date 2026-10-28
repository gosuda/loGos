// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package load

import (
	"cmd/go/internal/cfg"
	"cmd/go/internal/fsys"
	"cmd/go/internal/logos/config"
	"slices"
	"strconv"
	"strings"
)

// LogosPacerInactiveTag is the compiler-owned internal tool tag that selects
// the inactive wasm pacer (runtime/logos_pacer_inactive_wasm.go). It is never
// a user flag: initLogosGC rejects an explicit -tags use of it.
const LogosPacerInactiveTag = "logosgc.nongreentea"

// LogosPacerOmitElastic reports whether the runtime's elastic implementation
// may be omitted for the resolved collector mode. An empty mode is plain go
// with no selection and keeps the full implementation through the general
// known-nongreentea check below. Only a wasm build whose mode is known and is
// not greentea may omit it. A build that may run code through toolexec or an
// overlay keeps the full implementation, as does any build whose possible
// per-package linker flag set contains anything other than the boolean -s/-w
// spellings (bare or =bool). That conservatively preserves every raw -X,
// package-pattern and custom-linker path without parsing semantic overrides.
// BuildLdflags.values holds every -ldflags occurrence; String only reports
// the last, so the query lives here next to the owning data.
func LogosPacerOmitElastic(mode string) bool {
	if cfg.Goarch != "wasm" {
		return false
	}
	if !slices.Contains(config.GCModes, mode) || mode == "greentea" {
		return false
	}
	if len(cfg.BuildToolexec) != 0 {
		return false
	}
	if fsys.OverlayFile != "" {
		return false
	}
	return !logosPacerHasCustomLdflags()
}

// logosPacerHasCustomLdflags reports whether any possible per-package linker
// flag set contains anything other than the boolean -s/-w spellings.
func logosPacerHasCustomLdflags() bool {
	for _, v := range BuildLdflags.values {
		for _, f := range v.flags {
			name, value, hasValue := strings.Cut(f, "=")
			if name != "-s" && name != "-w" {
				return true
			}
			if !hasValue {
				continue
			}
			if _, err := strconv.ParseBool(value); err != nil {
				return true
			}
		}
	}
	return false
}
