// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

import "strings"

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
