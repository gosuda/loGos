// asmcheck -gcflags=-lb

//go:build goexperiment.logosgcmoving

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package codegen

import "sync/atomic"

// M19-S7 compiler attack (SPEC Sec 8.4, Sec 19.2, T-301.2): under -lb,
// which -gc=zgc passes, a pointer load tests the loaded word against the
// good color, routes a mismatch to the self-heal call, and unmasks the
// color bits with one AND. A non-pointer load stays plain.

func loadPtr(slot **int) *int {
	// amd64:"MOVQ\\s+runtime\\.zgcGoodMask\\(SB\\)" "XORQ" "MOVQ\\s+runtime\\.zgcColorMaskBits\\(SB\\)" "TESTQ" "J(EQ|NE)\\s" "MOVQ\\s+runtime\\.zgcUnmaskBits\\(SB\\)" "ANDQ" "CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	// arm64:"MOVD\\s+runtime\\.zgcGoodMask\\(SB\\)" "EOR" "MOVD\\s+runtime\\.zgcColorMaskBits\\(SB\\)" "TST" "B(EQ|NE)\\s" "MOVD\\s+runtime\\.zgcUnmaskBits\\(SB\\)" "AND\\s" "CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	return *slot
}

func loadString(s *string) string {
	// amd64:"CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	return *s
}

func loadAtomic(p *atomic.Pointer[int]) *int {
	// amd64:"CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	return p.Load()
}

type link struct{ next *link }

// A load whose address comes from another pointer load takes a barrier of
// its own, after the barrier of the first.
func loadChain(p *link) *link {
	// amd64:2"CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	return p.next.next
}

// A load after a join of paths, one of which stores, still takes its barrier.
func loadAfterStore(slot **int, dst *int, c bool) *int {
	if c {
		*dst = 1
	}
	// amd64:"CALL\\s+runtime\\.zgcLoadBarrierSlow\\(SB\\)"
	return *slot
}

func loadWord(slot *uintptr) uintptr {
	// amd64:-"zgcGoodMask" -"zgcLoadBarrierSlow"
	// arm64:-"zgcGoodMask" -"zgcLoadBarrierSlow"
	return *slot
}
