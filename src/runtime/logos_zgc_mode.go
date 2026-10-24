// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// logosZgcMode reports whether the linker selected the zgc collector.
func logosZgcMode() bool {
	return logosGCMode == "zgc"
}

// zgcCycleEnabled reports whether the zgc concurrent cycle runs: the
// moving-collector experiment plus the zgc mode. The closed -gc=zgc command
// gate (logosGCPendingPhase) is the only gate; there is no runtime switch.
func zgcCycleEnabled() bool {
	return logosZgcMode()
}
