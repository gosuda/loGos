// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

import (
	"os"
	"strings"
)

const logosFastCgoExperiment = "logosfastcgo"

// RefreshLogosFastCgo enables optimized cgo for managed logos commands.
// Explicit experiment choices, including none, take precedence.
// Plain go and raw tool invocations keep their existing defaults.
func RefreshLogosFastCgo() {
	if !Logos || CmdName == "tool" || ExperimentErr != nil {
		return
	}
	for f := range strings.SplitSeq(logosInitGOEXPERIMENT, ",") {
		switch f {
		case logosFastCgoExperiment, "no" + logosFastCgoExperiment, "none":
			return
		}
	}
	if Experiment.LogosFastCgo {
		return
	}
	logosApplyRawExperiment(appendExperiment(RawGOEXPERIMENT, logosFastCgoExperiment))
}

// DropLogosExperiments removes logos experiments from GOEXPERIMENT, so a
// different toolchain does not inherit names it cannot know.
func DropLogosExperiments() {
	var kept []string
	for f := range strings.SplitSeq(os.Getenv("GOEXPERIMENT"), ",") {
		if f != "" && !strings.HasPrefix(f, "logos") && !strings.HasPrefix(f, "nologos") {
			kept = append(kept, f)
		}
	}
	os.Setenv("GOEXPERIMENT", strings.Join(kept, ","))
}
