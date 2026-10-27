// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import "unsafe"

func (span *mspan) logosExcludeRelocation(kind uint8) {}

func logosPinObject(p unsafe.Pointer) {}

func logosPinSudog(s *sudog) *sudog { return s }

func logosPinChan(c *hchan, mem uintptr, elem *_type) {}

func logosZgcAttachSpecial(p unsafe.Pointer) unsafe.Pointer { return p }

func zgcAttachResolve(p unsafe.Pointer) (uintptr, *mspan) {
	base, s, _ := findObject(uintptr(p), 0, 0)
	return base, s
}

func zgcAttachEnsureCopy(p unsafe.Pointer) unsafe.Pointer { return p }

func zgcAttachStable(s *mspan, p uintptr) bool { return true }

func zgcSpanQuarantined(s *mspan) bool { return false }

func zgcSweepGuard(s *mspan) bool { return false }
