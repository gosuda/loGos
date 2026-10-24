// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"testing"
	"unsafe"

	. "runtime"
)

// Attach-handshake tests: finalizer installation must reach the copy's
// span without throwing, in preemptible context, whether or not a cycle
// is in flight. The registered-page copy itself runs under -gc=zgc;
// these tests pin the reachable machinery: passthrough when nothing is
// registered, the published-forward fast path, and the preemptible
// gate the hoisted pre-lock copies rely on.

func TestLogosZgcAttachPassthrough(t *testing.T) {
	obj := new([16]byte)
	fired := false
	SetFinalizer(obj, func(o *[16]byte) { fired = true })
	SetFinalizer(obj, nil)
	GC()
	GC()
	if fired {
		t.Error("removed finalizer ran")
	}
	// Re-install and leave it: the NoReloc hook must not break the
	// ordinary finalizer lifecycle.
	SetFinalizer(obj, func(o *[16]byte) {})
	SetFinalizer(obj, nil)
}

func TestLogosZgcAttachCanCopyNormalContext(t *testing.T) {
	// Tests run on a user goroutine with a P, unlocked: the hoisted
	// pre-lock copies in addCleanup and SetFinalizer must pass the
	// gate here. g0, signal, and P-less contexts stay refused.
	if !LogosZgcAttachCanCopy() {
		t.Error("attach copy refused in normal preemptible context")
	}
}

func TestLogosZgcAttachEnsureCopyPublished(t *testing.T) {
	if !LogosZgcCycleEnabled() {
		t.Skip("needs the zgc cycle; run under -gc=zgc")
	}
	LogosZgcSelectSoftware(LogosZgcRemapped)
	base, free := logosZgcPlace(t)
	defer free()
	pg := LogosZgcNewPage(base, 0, 4)
	defer LogosZgcDropPage(pg)
	from, to := base+0x100, base+LogosZgcPageSize+0x200
	if LogosZgcPageFor(from) != pg {
		t.Fatalf("page for %#x is not the registered page", from)
	}
	LogosZgcForwardInsert(pg, from, to)
	if got := LogosZgcAttachEnsureCopy(from); got != to {
		t.Errorf("ensureCopy(%#x) = %#x, want published %#x", from, got, to)
	}
	// Ordinary heap words pass through: the object is not on the page.
	obj := new([16]byte)
	up := uintptr(unsafe.Pointer(obj))
	if got := LogosZgcAttachEnsureCopy(up); got != up {
		t.Errorf("ensureCopy(%#x) = %#x, want identity", up, got)
	}
}

func TestLogosZgcAttachCleanupLockPath(t *testing.T) {
	// AddCleanup reaches addspecial holding the M lock: the hoisted
	// pre-lock copy must already have run, so the backstop passes
	// through instead of throwing.
	obj := new([16]byte)
	c := AddCleanup(obj, func(v int) {}, 1)
	c.Stop()
}
