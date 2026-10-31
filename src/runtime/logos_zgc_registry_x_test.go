// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"runtime"
	"testing"
)

// TestLogosZgcPageStateRegistry reports the page-state registry probe: a
// page must keep its own state index while other pages are registered and
// forgotten around it.
func TestLogosZgcPageStateRegistry(t *testing.T) {
	if msg := runtime.LogosZgcRegistryProbe(); msg != "" {
		t.Fatal(msg)
	}
}
