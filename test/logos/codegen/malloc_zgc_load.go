// asmcheck -goexperiment logosgcmoving -gcflags=-lb

//go:build amd64 || arm64

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codegen

func runtimePrefixUserLoad(slot **int) *int {
	// amd64:"CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	// arm64:"CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	return *slot
}
