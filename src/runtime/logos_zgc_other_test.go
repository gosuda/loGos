// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && !linux && (amd64 || arm64)

package runtime_test

import "testing"

func logosZgcReportRSS(b *testing.B) {
	b.Log("rss metrics need /proc/self/statm; not recorded on this OS")
}
