// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"os"
	"testing"
	"time"
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

var logosSyscallChurnSink uint64

// logosSyscallChurn allocates garbage until stop closes, widening the
// relocation window while a frame is live. Single writer, never read,
// so no additional synchronization is needed.
func logosSyscallChurn(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			b := make([]byte, 1<<16)
			var s uint64
			for j := 0; j < len(b); j += 4096 {
				b[j] = byte(j)
				s += uint64(b[j])
			}
			logosSyscallChurnSink = s
		}
	}
}

// logosSyscallBlockingPump moves all of wbuf through a real pipe into
// rbuf while the caller's admission frame is live: the writer issues
// blocking Write syscalls and a parked reader drains, so both buffers
// are kernel-observed mid-invocation under GC churn. Both sides slice
// the caller's own live Go buffers at use time, so the collector keeps
// tracking each backing across every GC point: no address is frozen
// across a collection and no uintptr is laundered back into a pointer.
// An early release or a stale reload corrupts the byte comparison
// instead of merely mismatching registry bookkeeping.
//
// Both directions carry a deadline: a dead peer (reader that stopped
// draining after an error) turns into a prompt diagnostic instead of a
// wedged test that only the go test timeout ends.
func logosSyscallBlockingPump(t *testing.T, wbuf, rbuf []byte, chunk int) {
	t.Helper()
	total := len(wbuf)
	if len(rbuf) != total {
		t.Fatalf("pump buffers differ: %d write bytes, %d read bytes", total, len(rbuf))
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	type rdRes struct {
		n   int
		err error
	}
	const ioTimeout = 30 * time.Second
	if err := r.SetReadDeadline(time.Now().Add(ioTimeout)); err != nil {
		t.Fatal(err)
	}
	if err := w.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		t.Fatal(err)
	}
	resCh := make(chan rdRes, 1)
	go func() {
		n := 0
		for n < total {
			m := min(chunk, total-n)
			k, err := r.Read(rbuf[n : n+m])
			if k > 0 {
				n += k
			}
			if err != nil {
				resCh <- rdRes{n, err}
				return
			}
		}
		resCh <- rdRes{n, nil}
	}()
	stop := make(chan struct{})
	defer close(stop)
	go logosSyscallChurn(stop)
	for off := 0; off < total; {
		m := min(chunk, total-off)
		GC()
		k, err := w.Write(wbuf[off : off+m])
		if err != nil {
			res := <-resCh
			t.Fatalf("pipe write at off %d: %v (reader: %d bytes, %v)", off, err, res.n, res.err)
		}
		if k <= 0 {
			t.Fatalf("pipe short write at off %d: %d", off, k)
		}
		off += k
	}
	w.Close()
	res := <-resCh
	if res.err != nil || res.n != total {
		t.Fatalf("pipe read: got %d bytes err %v, want %d", res.n, res.err, total)
	}
}

// TestLogosZgcSyscallScalarTagPreserved proves a scalar uintptr argument
// with tag bits survives the admission round trip bit-identical: an
// opaque tagged word passed through rows as a single-word root must
// come back with every bit intact after both Admit and End. The word
// is genuinely admission-observed (it names a row slot), so a
// clobbering or stripping commit fails the comparison instead of
// passing vacuously; the neighboring foreign mapping pins the
// must-not-retain, must-not-mutate side of the same contract.
func TestLogosZgcSyscallScalarTagPreserved(t *testing.T) {
	LogosZgcForeignInit()
	payload := new([32]byte)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	base := uintptr(unsafe.Pointer(payload))
	mbase, mraw := LogosZgcMapPagesRaw(1)
	const mpat = uintptr(0x1122334455667788)
	LogosZgcStore(mbase, mpat)
	tagged := mbase | (uintptr(0x5a) << 56)
	// The tagged word is a scalar, so it lives in a noscan uintptr
	// cell: a tagged word is not a valid heap pointer and must never
	// occupy a GC-scanned pointer slot.
	var stag uintptr = tagged
	var temp unsafe.Pointer = unsafe.Pointer(payload)
	rows := []unsafe.Pointer{unsafe.Pointer(&temp), nil, unsafe.Pointer(&stag), nil}
	frame, gen := LogosZgcSyscallAdmit(unsafe.Pointer(&rows[0]), 2)
	if LogosZgcSyscallMode() && !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if stag != tagged {
		t.Fatalf("tagged opaque word rewritten by admission: got %#x want %#x", stag, tagged)
	}
	if LogosZgcWord(mbase) != mpat {
		t.Fatal("foreign mapping mutated by admission")
	}
	if LogosZgcSyscallExposed(mbase) {
		t.Fatal("foreign mapping wrongly exposed")
	}
	if LogosZgcSyscallTokenValid(frame, gen) {
		if !LogosZgcSyscallExposed(base) {
			t.Fatal("named referent not retained")
		}
		LogosZgcSyscallEnd(frame, gen)
		if LogosZgcSyscallExposed(base) {
			t.Fatal("referent still exposed after End")
		}
	}
	if stag != tagged {
		t.Fatalf("tagged opaque word changed across End: got %#x want %#x", stag, tagged)
	}
	LogosZgcUnmapPages(mraw, 1)
	KeepAlive(payload)
	KeepAlive(rows)
}

// TestLogosZgcSyscallBufferRetentionCanonicalReload proves a buffer
// passed to a syscall is retained for the whole invocation and that
// the reload order is canonical: a stale colored word in the temp is
// rewritten to the canonical copy at Admit, the kernel observes only
// the live reloaded buffers, and the admitted cells stay stable across
// the call. The transfer runs through a real blocking pipe under GC
// churn, so an early release fails as corrupted bytes.
//
// The stale words live in uintptr cells on purpose: a colored word is
// not a valid heap pointer, and storing one in a GC-scanned pointer
// slot makes the collector throw "found bad pointer in Go heap" as soon
// as that stack is scanned (debug.invalidptr defaults to 1). The wire
// shape only needs the address of the word cell, so noscan cells are
// faithful to production emission and keep the fixture out of the
// pointer map.
func TestLogosZgcSyscallBufferRetentionCanonicalReload(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	const total = 128 << 10
	const chunk = 8 << 10
	wbuf := make([]byte, total)
	for i := range wbuf {
		wbuf[i] = byte(i*7 + 3)
	}
	rbuf := make([]byte, total)
	wbase := uintptr(unsafe.Pointer(&wbuf[0]))
	rbase := uintptr(unsafe.Pointer(&rbuf[0]))
	var wcell uintptr = LogosZgcColored(wbase, 1)
	var rcell uintptr = LogosZgcColored(rbase, 1)
	if wcell == wbase || rcell == rbase {
		t.Fatal("color fixture did not alter the words")
	}
	rows := []unsafe.Pointer{unsafe.Pointer(&wcell), nil, unsafe.Pointer(&rcell), nil}
	frame, gen := LogosZgcSyscallAdmit(unsafe.Pointer(&rows[0]), 2)
	if !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	// Canonical reload: commit rewrote the stale words to uncolored
	// canonical addresses. A relocation may legitimately land on a
	// different canonical copy, so the contract proven here is "the
	// stale color bits are gone", not "identical to the pre-admit
	// source"; the read path below goes through the live slices, which
	// the collector keeps tracking either way.
	if wcell&^uintptr(LogosZgcAddrMask) != 0 || rcell&^uintptr(LogosZgcAddrMask) != 0 {
		t.Fatalf("cells still colored after Admit: got %#x %#x (mask %#x)", wcell, rcell, LogosZgcAddrMask)
	}
	waddr, raddr := wcell, rcell
	// Exposure is queried through the post-Admit canonical cells: the
	// pre-admission words may name a relocation source, which
	// zgcForeignHeapBase canonicalizes away and would report as
	// unexposed even while the invocation holds the copy.
	if !LogosZgcSyscallExposed(wcell) || !LogosZgcSyscallExposed(rcell) {
		t.Fatal("buffers not retained while frame live")
	}
	logosSyscallBlockingPump(t, wbuf, rbuf, chunk)
	// Address stability across the call: the invocation must not move
	// or rewrite the cells it canonicalized. A stale reload (the defect
	// this row owns) fails here even though the transfer itself used
	// the collector-tracked slices.
	if wcell != waddr || rcell != raddr {
		t.Fatalf("cells moved across the call: got %#x %#x want %#x %#x", wcell, rcell, waddr, raddr)
	}
	if !LogosZgcSyscallExposed(wcell) || !LogosZgcSyscallExposed(rcell) {
		t.Fatal("buffers released before End (freed early)")
	}
	for i := range rbuf {
		if rbuf[i] != byte(i*7+3) {
			t.Fatalf("round trip corrupted at %d", i)
		}
	}
	LogosZgcSyscallEnd(frame, gen)
	if LogosZgcSyscallExposed(wcell) || LogosZgcSyscallExposed(rcell) {
		t.Fatal("buffers still exposed after End")
	}
	KeepAlive(wbuf)
	KeepAlive(rbuf)
	KeepAlive(rows)
}

// TestLogosZgcSyscallInvocationLifetime proves the invocation lifetime:
// the buffer stays exposed from Admit through the completion of real
// blocking I/O and is released exactly at End. A buffer freed or
// rewritten early fails the mid-lifetime exposure and content checks;
// a leaked End fails the post-End release check.
func TestLogosZgcSyscallInvocationLifetime(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle")
	}
	LogosZgcForeignInit()
	const total = 32 << 10
	const chunk = 8 << 10
	wbuf := make([]byte, total)
	for i := range wbuf {
		wbuf[i] = byte(i*11 + 5)
	}
	rbuf := make([]byte, total)
	var wcell uintptr = uintptr(unsafe.Pointer(&wbuf[0]))
	var rcell uintptr = uintptr(unsafe.Pointer(&rbuf[0]))
	rows := []unsafe.Pointer{unsafe.Pointer(&wcell), nil, unsafe.Pointer(&rcell), nil}
	frame, gen := LogosZgcSyscallAdmit(unsafe.Pointer(&rows[0]), 2)
	if !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if !LogosZgcSyscallExposed(wcell) || !LogosZgcSyscallExposed(rcell) {
		t.Fatal("buffers not exposed right after Admit")
	}
	logosSyscallBlockingPump(t, wbuf, rbuf, chunk)
	// Still live after the kernel finished touching both buffers: an
	// early free or rewrite would already have dropped exposure or
	// corrupted the payload.
	if !LogosZgcSyscallExposed(wcell) || !LogosZgcSyscallExposed(rcell) {
		t.Fatal("buffers released before End (freed early)")
	}
	for i := range rbuf {
		if rbuf[i] != byte(i*11+5) {
			t.Fatalf("payload rewritten mid-lifetime at %d", i)
		}
	}
	LogosZgcSyscallEnd(frame, gen)
	if LogosZgcSyscallExposed(wcell) || LogosZgcSyscallExposed(rcell) {
		t.Fatal("buffers still exposed after End (lifetime leak)")
	}
	for i := range rbuf {
		if rbuf[i] != byte(i*11+5) {
			t.Fatalf("payload corrupted across End at %d", i)
		}
	}
	KeepAlive(wbuf)
	KeepAlive(rbuf)
	KeepAlive(rows)
}
