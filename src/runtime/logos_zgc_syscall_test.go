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

// Syscall foreign-admission attacks (owner H-runtime). Rows mimic the
// compiler emission exactly: [Slot, Type] pairs where Slot is the
// address of the unsafe.Pointer temp whose uintptr conversion the call
// reads, and Type is the *_type of the referent value (nil for a single
// opaque word). The pre-fix predicted failure for every test below is
// the build failure (undefined zgcSyscallAdmit and siblings); post-fix
// the mode-gated tests prove behavior and the always-on tests prove the
// disabled contract.

//go:noinline
func logosSyscallNonForeign() {}

type logosSyscallIovec struct {
	base *byte
	n    uintptr
}

type logosSyscallMsghdr struct {
	iov *logosSyscallIovec
	n   int
}

var (
	logosSyscallGPayload = new([32]byte)
	logosSyscallGIov     logosSyscallIovec
	logosSyscallGMsg     logosSyscallMsghdr
)

func logosSyscallRows(slot unsafe.Pointer, typ unsafe.Pointer) ([]unsafe.Pointer, unsafe.Pointer, uintptr) {
	rows := []unsafe.Pointer{slot, typ}
	return rows, unsafe.Pointer(&rows[0]), 1
}

func logosSyscallBase(payload *[32]byte) uintptr {
	return uintptr(unsafe.Pointer(payload))
}

// TestLogosZgcSyscallTempRewrite proves the exact post-admission cell
// contract: a stale colored word in the temp is rewritten to the
// canonical copy address at commit, and a second admission is a
// fixpoint.
func TestLogosZgcSyscallTempRewrite(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	payload := logosHeapReservePost()
	logosHeapReserveSink = append(logosHeapReserveSink, payload)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	base := logosSyscallBase(payload)
	if _, _, ok := LogosZgcHeapReserveSpan(base); !ok {
		t.Fatalf("temp payload %#x: no live heap span (want real spanOfHeap)", base)
	}
	if uint64(base) > LogosZgcAddrMask {
		t.Fatalf("temp payload %#x above 4 TB field (mask %#x)", base, LogosZgcAddrMask)
	}
	var temp unsafe.Pointer = unsafe.Pointer(LogosZgcColored(base, 1))
	if uintptr(temp) == base {
		t.Fatal("color fixture did not alter the word")
	}
	slot := unsafe.Pointer(&temp)
	rows, rp, n := logosSyscallRows(slot, nil)
	frame, gen := LogosZgcSyscallAdmit(rp, n)
	if !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if uintptr(temp) != base {
		t.Fatalf("temp not canonicalized: got %#x want %#x", uintptr(temp), base)
	}
	if !LogosZgcSyscallExposed(base) {
		t.Fatal("heap referent not retained while frame live")
	}
	for i := range payload {
		if payload[i] != byte(i+1) {
			t.Fatalf("payload corrupted at %d", i)
		}
	}
	frame2, gen2 := LogosZgcSyscallAdmit(rp, n)
	if !LogosZgcSyscallTokenValid(frame2, gen2) {
		t.Fatal("second admit returned invalid token")
	}
	if uintptr(temp) != base {
		t.Fatalf("fixpoint moved temp: got %#x want %#x", uintptr(temp), base)
	}
	LogosZgcSyscallEnd(frame2, gen2)
	LogosZgcSyscallEnd(frame, gen)
	if LogosZgcSyscallExposed(base) {
		t.Fatal("referent still exposed after balanced End")
	}
	KeepAlive(payload)
	KeepAlive(rows)
}

// TestLogosZgcSyscallStackChain proves stack->stack->heap traversal:
// temp -> msg (stack) -> iov (stack) -> payload (heap). The heap
// payload is reachable only through the two stack cells, so exposure
// proves the complete chain, and the scalar beside the pointers proves
// precise (non-clobbering) canonicalization.
func TestLogosZgcSyscallStackChain(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	payload := new([32]byte)
	for i := range payload {
		payload[i] = byte(0xa0 + i)
	}
	base := logosSyscallBase(payload)
	var iov logosSyscallIovec
	iov.base = (*byte)(unsafe.Pointer(payload))
	iov.n = 32
	var msg logosSyscallMsghdr
	msg.iov = &iov
	msg.n = 7
	var temp unsafe.Pointer = unsafe.Pointer(&msg)
	typ := LogosZgcSyscallTypeOf(msg)
	rows, rp, n := logosSyscallRows(unsafe.Pointer(&temp), typ)
	frame, gen := LogosZgcSyscallAdmit(rp, n)
	if !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if uintptr(temp) != uintptr(unsafe.Pointer(&msg)) {
		t.Fatalf("temp moved off referent: got %#x", uintptr(temp))
	}
	if msg.iov != &iov {
		t.Fatal("stack cell msg.iov rewritten to wrong address")
	}
	if msg.n != 7 {
		t.Fatalf("scalar clobbered by typed walk: got %d", msg.n)
	}
	if iov.base != (*byte)(unsafe.Pointer(payload)) {
		t.Fatal("stack cell iov.base rewritten to wrong address")
	}
	if iov.n != 32 {
		t.Fatalf("scalar clobbered by chain walk: got %d", iov.n)
	}
	if !LogosZgcSyscallExposed(base) {
		t.Fatal("chain heap payload not retained")
	}
	for i := range payload {
		if payload[i] != byte(0xa0+i) {
			t.Fatalf("payload corrupted at %d", i)
		}
	}
	LogosZgcSyscallEnd(frame, gen)
	if LogosZgcSyscallExposed(base) {
		t.Fatal("chain payload still exposed after End")
	}
	KeepAlive(payload)
	KeepAlive(rows)
}

// TestLogosZgcSyscallGlobalChain proves global->global->heap traversal
// using authoritative module masks. The payload is reachable only
// through package-level globals.
func TestLogosZgcSyscallGlobalChain(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	for i := range logosSyscallGPayload {
		logosSyscallGPayload[i] = byte(0x50 + i)
	}
	base := logosSyscallBase(logosSyscallGPayload)
	logosSyscallGIov.base = (*byte)(unsafe.Pointer(logosSyscallGPayload))
	logosSyscallGIov.n = 32
	logosSyscallGMsg.iov = &logosSyscallGIov
	logosSyscallGMsg.n = 9
	var temp unsafe.Pointer = unsafe.Pointer(&logosSyscallGMsg)
	typ := LogosZgcSyscallTypeOf(logosSyscallGMsg)
	rows, rp, n := logosSyscallRows(unsafe.Pointer(&temp), typ)
	frame, gen := LogosZgcSyscallAdmit(rp, n)
	if !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if !LogosZgcSyscallExposed(base) {
		t.Fatal("global-chain heap payload not retained")
	}
	if logosSyscallGMsg.n != 9 || logosSyscallGIov.n != 32 {
		t.Fatal("global scalars clobbered by chain walk")
	}
	for i := range logosSyscallGPayload {
		if logosSyscallGPayload[i] != byte(0x50+i) {
			t.Fatalf("global payload corrupted at %d", i)
		}
	}
	LogosZgcSyscallEnd(frame, gen)
	if LogosZgcSyscallExposed(base) {
		t.Fatal("global-chain payload still exposed after End")
	}
	KeepAlive(rows)
}

// TestLogosZgcSyscallPointerFreeBacking proves a pointer-free heap
// backing is retained: there is no Pointers() filter on this lane, so
// both the nil-Type and the scalar-element-Type rows must pin.
func TestLogosZgcSyscallPointerFreeBacking(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	buf := make([]byte, 64)
	for i := range buf {
		buf[i] = byte(i*3 + 1)
	}
	backing := uintptr(unsafe.Pointer(&buf[0]))
	for _, typ := range []unsafe.Pointer{nil, LogosZgcSyscallTypeOf(uint8(0))} {
		var temp unsafe.Pointer = unsafe.Pointer(&buf[0])
		rows, rp, n := logosSyscallRows(unsafe.Pointer(&temp), typ)
		frame, gen := LogosZgcSyscallAdmit(rp, n)
		if !LogosZgcSyscallTokenValid(frame, gen) {
			t.Fatalf("admit returned invalid token (typ nil=%v)", typ == nil)
		}
		if !LogosZgcSyscallExposed(backing) {
			t.Fatalf("pointer-free backing not retained (typ nil=%v)", typ == nil)
		}
		for i := range buf {
			if buf[i] != byte(i*3+1) {
				t.Fatalf("backing corrupted at %d (typ nil=%v)", i, typ == nil)
			}
		}
		LogosZgcSyscallEnd(frame, gen)
		if LogosZgcSyscallExposed(backing) {
			t.Fatalf("backing still exposed after End (typ nil=%v)", typ == nil)
		}
		KeepAlive(rows)
	}
	KeepAlive(buf)
}

// TestLogosZgcSyscallScalarNonmutation proves unnamed scalars and
// foreign (non-heap) addresses pass through bit-identical: only named
// temp cells may change.
func TestLogosZgcSyscallScalarNonmutation(t *testing.T) {
	LogosZgcForeignInit()
	payload := new([32]byte)
	backing := logosSyscallBase(payload)
	var temp unsafe.Pointer = unsafe.Pointer(payload)
	rows, rp, n := logosSyscallRows(unsafe.Pointer(&temp), nil)
	const s1 = uintptr(0x123456789abcdef)
	s2 := uintptr(0x0fedcba987654321)
	victim := new(uint64)
	*victim = 0xdeadbeefcafe0123
	vbase := uintptr(unsafe.Pointer(victim))
	mbase, mraw := LogosZgcMapPagesRaw(1)
	const mpat = uintptr(0x1122334455667788)
	LogosZgcStore(mbase, mpat)
	frame, gen := LogosZgcSyscallAdmit(rp, n)
	if LogosZgcSyscallMode() && !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if s1 != uintptr(0x123456789abcdef) || s2 != uintptr(0x0fedcba987654321) {
		t.Fatal("scalar mutated by admission")
	}
	if *victim != 0xdeadbeefcafe0123 {
		t.Fatal("unnamed heap word mutated by admission")
	}
	if LogosZgcWord(mbase) != mpat {
		t.Fatal("foreign mapping mutated by admission")
	}
	if LogosZgcSyscallExposed(vbase) {
		t.Fatal("unnamed heap object wrongly exposed")
	}
	if LogosZgcSyscallExposed(mbase) {
		t.Fatal("foreign mapping wrongly exposed")
	}
	if LogosZgcSyscallTokenValid(frame, gen) {
		if !LogosZgcSyscallExposed(backing) {
			t.Fatal("named referent not retained")
		}
		LogosZgcSyscallEnd(frame, gen)
	}
	LogosZgcUnmapPages(mraw, 1)
	KeepAlive(payload)
	KeepAlive(rows)
	KeepAlive(victim)
}

// TestLogosZgcSyscallIndirectGating proves the dynamic spill gate: a
// func value whose target lacks the foreign FuncFlag returns the
// invalid token with strictly zero mutation, preserving hash/address
// consumers. A nil code pointer behaves the same.
func TestLogosZgcSyscallIndirectGating(t *testing.T) {
	LogosZgcForeignInit()
	payload := new([32]byte)
	base := logosSyscallBase(payload)
	fn := logosSyscallNonForeign
	code := *(*unsafe.Pointer)(unsafe.Pointer(&fn))
	for _, target := range []unsafe.Pointer{code, nil} {
		stale := unsafe.Pointer(LogosZgcColored(base, 1))
		var temp unsafe.Pointer = stale
		rows, rp, n := logosSyscallRows(unsafe.Pointer(&temp), nil)
		frame, gen := LogosZgcSyscallAdmitIndirect(target, rp, n)
		if LogosZgcSyscallTokenValid(frame, gen) {
			t.Fatalf("indirect admitted non-foreign target %v", target)
		}
		if temp != stale {
			t.Fatalf("indirect reject mutated the temp: got %#x want %#x", uintptr(temp), uintptr(stale))
		}
		if LogosZgcSyscallExposed(base) {
			t.Fatal("indirect reject retained the referent")
		}
		KeepAlive(rows)
	}
	KeepAlive(payload)
}

// TestLogosZgcSyscallEmpty proves the token-free empty set: no rows
// means no ownership on either path and End pairs with nothing.
func TestLogosZgcSyscallEmpty(t *testing.T) {
	frame, gen := LogosZgcSyscallAdmit(nil, 0)
	if LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("empty admit minted ownership")
	}
	LogosZgcSyscallEnd(frame, gen)
	nframe, ngen := LogosZgcSyscallAdmitNoSplit(nil, 0)
	if LogosZgcSyscallTokenValid(nframe, ngen) {
		t.Fatal("empty nosplit admit minted ownership")
	}
	LogosZgcSyscallEndNoSplit(nframe, ngen)
}

// TestLogosZgcSyscallNested proves balanced nested ownership: two live
// frames retain independently and release in LIFO order.
func TestLogosZgcSyscallNested(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	a := new([16]byte)
	b := new([16]byte)
	abase := uintptr(unsafe.Pointer(a))
	bbase := uintptr(unsafe.Pointer(b))
	var tempA unsafe.Pointer = unsafe.Pointer(a)
	rowsA, rpA, nA := logosSyscallRows(unsafe.Pointer(&tempA), nil)
	var tempB unsafe.Pointer = unsafe.Pointer(b)
	rowsB, rpB, nB := logosSyscallRows(unsafe.Pointer(&tempB), nil)
	fA, gA := LogosZgcSyscallAdmit(rpA, nA)
	fB, gB := LogosZgcSyscallAdmit(rpB, nB)
	if !LogosZgcSyscallTokenValid(fA, gA) || !LogosZgcSyscallTokenValid(fB, gB) {
		t.Fatal("nested admit returned invalid token")
	}
	if !LogosZgcSyscallExposed(abase) || !LogosZgcSyscallExposed(bbase) {
		t.Fatal("nested frames do not retain independently")
	}
	LogosZgcSyscallEnd(fB, gB)
	if LogosZgcSyscallExposed(bbase) {
		t.Fatal("inner frame still exposed after inner End")
	}
	if !LogosZgcSyscallExposed(abase) {
		t.Fatal("outer frame lost retention after inner End")
	}
	LogosZgcSyscallEnd(fA, gA)
	if LogosZgcSyscallExposed(abase) {
		t.Fatal("outer frame still exposed after outer End")
	}
	KeepAlive(a)
	KeepAlive(b)
	KeepAlive(rowsA)
	KeepAlive(rowsB)
}

// TestLogosZgcSyscallNoSplit proves the restricted leaf: token-free on
// success, never allocating, never minting ownership. With the cycle
// disabled the leaf is a passthrough; with the cycle enabled an outer
// preemptible admission pins the referent so the covered leaf succeeds
// without ownership.
func TestLogosZgcSyscallNoSplit(t *testing.T) {
	LogosZgcForeignInit()
	payload := new([16]byte)
	base := uintptr(unsafe.Pointer(payload))
	var temp unsafe.Pointer = unsafe.Pointer(payload)
	rows, rp, n := logosSyscallRows(unsafe.Pointer(&temp), nil)
	if !LogosZgcSyscallMode() {
		frame, gen := LogosZgcSyscallAdmitNoSplit(rp, n)
		if LogosZgcSyscallTokenValid(frame, gen) {
			t.Fatal("disabled nosplit leaf minted ownership")
		}
		if uintptr(temp) != base {
			t.Fatal("disabled nosplit leaf mutated the temp")
		}
		LogosZgcSyscallEndNoSplit(frame, gen)
		KeepAlive(payload)
		KeepAlive(rows)
		return
	}
	outer, ogen := LogosZgcSyscallAdmit(rp, n)
	if !LogosZgcSyscallTokenValid(outer, ogen) {
		t.Fatal("outer admit returned invalid token")
	}
	frame, gen := LogosZgcSyscallAdmitNoSplit(rp, n)
	if LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("covered nosplit leaf minted ownership; leaf must stay token-free")
	}
	if uintptr(temp) != base {
		t.Fatalf("nosplit leaf moved the temp: got %#x want %#x", uintptr(temp), base)
	}
	LogosZgcSyscallEndNoSplit(frame, gen)
	LogosZgcSyscallEnd(outer, ogen)
	if LogosZgcSyscallExposed(base) {
		t.Fatal("referent still exposed after outer End")
	}
	KeepAlive(payload)
	KeepAlive(rows)
}
