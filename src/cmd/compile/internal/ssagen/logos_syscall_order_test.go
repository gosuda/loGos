// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"testing"
)

// The six order rows: each call shape classifies distinctly at order
// time. The classifier is the order-pass contract: inlined wins over
// shape, go/defer win over static/dynamic, the rest split by
// staticness and package.

func TestLogosSyscallOrderDirect(t *testing.T) {
	if got := LogosSyscallClassify(false, "call", true, true, false); got != LogosSyscallDirect {
		t.Fatalf("direct classify = %v, want direct", LogosSyscallKindString(got))
	}
}

func TestLogosSyscallOrderImported(t *testing.T) {
	if got := LogosSyscallClassify(false, "call", true, false, false); got != LogosSyscallImported {
		t.Fatalf("imported classify = %v, want imported", LogosSyscallKindString(got))
	}
}

func TestLogosSyscallOrderIndirect(t *testing.T) {
	if got := LogosSyscallClassify(false, "call", false, false, false); got != LogosSyscallIndirect {
		t.Fatalf("closure classify = %v, want indirect", LogosSyscallKindString(got))
	}
	if got := LogosSyscallClassify(false, "call", true, true, true); got != LogosSyscallIndirect {
		t.Fatalf("interface classify = %v, want indirect", LogosSyscallKindString(got))
	}
}

func TestLogosSyscallOrderGo(t *testing.T) {
	if got := LogosSyscallClassify(false, "go", true, true, false); got != LogosSyscallGo {
		t.Fatalf("go classify = %v, want go", LogosSyscallKindString(got))
	}
}

func TestLogosSyscallOrderDefer(t *testing.T) {
	if got := LogosSyscallClassify(false, "defer", true, true, false); got != LogosSyscallDefer {
		t.Fatalf("defer classify = %v, want defer", LogosSyscallKindString(got))
	}
}

func TestLogosSyscallOrderInlined(t *testing.T) {
	if got := LogosSyscallClassify(true, "call", true, true, false); got != LogosSyscallInlined {
		t.Fatalf("inlined classify = %v, want inlined", LogosSyscallKindString(got))
	}
}

// The recorder preserves walk/program order across mixed kinds: flush
// output order is strictly increasing and emission never reorders the
// existing call sequence.

func TestLogosSyscallOrderPreservesEmissionOrder(t *testing.T) {
	var r LogosSyscallOrderRecorder
	kinds := []LogosSyscallCallKind{
		LogosSyscallDirect,
		LogosSyscallImported,
		LogosSyscallIndirect,
		LogosSyscallGo,
		LogosSyscallDefer,
		LogosSyscallInlined,
	}
	for _, k := range kinds {
		r.LogosSyscallNote(k, LogosSyscallKindString(k))
	}
	got := r.LogosSyscallOrdered()
	if len(got) != len(kinds) {
		t.Fatalf("ordered len = %d, want %d", len(got), len(kinds))
	}
	for i, k := range kinds {
		if got[i].Kind != k || got[i].Order != i {
			t.Fatalf("ordered[%d] = (%v,%d), want (%v,%d)", i, got[i].Kind, got[i].Order, k, i)
		}
	}
	emit := r.LogosSyscallFlush()
	for i := range emit {
		if emit[i].Order != i {
			t.Fatalf("flush[%d].Order = %d, want %d", i, emit[i].Order, i)
		}
	}
}

// Async lifetime: go/defer notes outlive their scheduling call, so the
// consumer must not emit a short Admit/End pair around newproc or
// deferproc. Synchronous notes admit around the call itself.

func TestLogosSyscallGoDeferExtendedLifetime(t *testing.T) {
	var r LogosSyscallOrderRecorder
	r.LogosSyscallNote(LogosSyscallGo, "go")
	r.LogosSyscallNote(LogosSyscallDefer, "defer")
	r.LogosSyscallNote(LogosSyscallDirect, "direct")
	r.LogosSyscallNote(LogosSyscallInlined, "inlined")
	emit := r.LogosSyscallFlush()
	if !emit[0].Async || !emit[1].Async {
		t.Fatal("go/defer must be async: admission outlives the scheduling call")
	}
	if emit[2].Async || emit[3].Async {
		t.Fatal("direct/inlined must be sync: admission pairs around the call")
	}
}

// Note: the former SSA-hook observer tests (nil-safe Observe and
// inlined-foreign gating via KindOfNode) were deleted with the
// write-only per-function side table. Ordinary inlined bodies gain no
// provenance because logosSyscallForeignCall — the predicate production
// admission actually consumes — admits nothing without KeepAlive or a
// qualifying unsafe.Pointer-to-uintptr conversion.
