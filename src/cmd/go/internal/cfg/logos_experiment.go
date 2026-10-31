// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

import (
	"os"
	"strings"
)

// appendExperiment appends an experiment to a GOEXPERIMENT value. One
// path handles the empty and non-empty base, so the arena and
// preemptible refreshes share it instead of each branching on "".
func appendExperiment(raw, exp string) string {
	if raw == "" {
		return exp
	}
	return raw + "," + exp
}

// logosApplyRawExperiment installs raw as the effective GOEXPERIMENT,
// recomputes the experiment tags and the clean cache-key value, and
// publishes the value to the process and tool environments. It mirrors
// the tail of RefreshLogosArena so both refreshes propagate to the
// compiler subprocesses.
func logosApplyRawExperiment(raw string) {
	if raw == RawGOEXPERIMENT {
		return
	}
	RawGOEXPERIMENT = raw
	recomputeExperiment()
	os.Setenv("GOEXPERIMENT", RawGOEXPERIMENT)
	for i := range CmdEnv {
		if CmdEnv[i].Name == "GOEXPERIMENT" {
			CmdEnv[i].Value = RawGOEXPERIMENT
		}
	}
	// Mirror into OrigEnv: main.invoke snapshots OrigEnv before MkEnv
	// publishes the managed defaults, and user binaries (go test, go run)
	// execute with OrigEnv, not CmdEnv. Without this, test-spawned child
	// builds never see the default: the outer test binary compiles with
	// the default on while an inner plain-go build compiles the fallback.
	mirrored := false
	for i := range OrigEnv {
		if strings.HasPrefix(OrigEnv[i], "GOEXPERIMENT=") {
			OrigEnv[i] = "GOEXPERIMENT=" + raw
			mirrored = true
			break
		}
	}
	if !mirrored {
		OrigEnv = append(OrigEnv, "GOEXPERIMENT="+raw)
	}
}
