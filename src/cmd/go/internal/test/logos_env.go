// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

import (
	"cmd/go/internal/cfg"
	"cmd/go/internal/work"
)

func logosAppendTestEnv(env []string) []string {
	if !cfg.Logos {
		return env
	}
	return append(env, work.LogosTargetArchEnv()...)
}
