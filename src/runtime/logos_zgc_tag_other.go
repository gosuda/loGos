// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && !linux && (amd64 || arm64)

package runtime

const zgcErrNoTagging = 38

// zgcEnableHardwareTagging reports that this OS has no hardware pointer
// tagging interface, so zgc uses software unmasking.
func zgcEnableHardwareTagging() (ok bool, errno uintptr) {
	return false, zgcErrNoTagging
}
