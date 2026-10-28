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

// W6 debug/crash boundary tests.
//
// The compiler half of the W6 contract lives in
// cmd/compile/internal/ssa/logos_loadbarrier.go: debugcall.go and the
// crash-stack entries keep plain loads (LogosNoLBFiles / LogosNoLBFunc).
// The compiled witnesses (no zgcLoadBarrierSlow in debugCallWrap or
// switchToCrashStack) belong to the parent's -lb build and objdump gates.
//
// This file pins the run-time half of the same contract: exclusion from
// the barrier is only sound because the frames' holders are *pinned*, not
// merely skipped. The debug-call path (debugCallWrap, deeply nosplit) and
// the crash path (switchToCrashStack on g0) both carry heap words that
// relocation must not move underneath them: the g structures, the
// debugCallWrapArgs block behind gp.param, and the funcval word read from
// the nosplit frame. If the exclusion ever widens while a holder loses
// its pin, a plain load from a nosplit frame dereferences a moved object
// — a fault under software colors. Here that failure surfaces as a
// failed pin assertion instead.
//
// The pin pass itself refuses relocation when a goroutine is in a state
// it cannot reason about (pinGoroutine returns false); that refusal is
// the load-bearing half of the contract for the *running* debug/crash
// goroutine, so the tests exercise both halves: the state refusal and
// the colored-word pin transition.
//
// The test lives in package runtime_test: package runtime cannot import
// testing (testing imports runtime). Every pin primitive and holder below
// runs through the LogosZgcW6 export seam, which is a one-line
// pass-through to the real pass; all assertions stay here and exercise
// real behavior, never source strings. Direct pinPtr checks below prove
// the primitive resolves colors; they do not prove the cycle discovers
// a holder. Only the PinDiscovers tests prove discovery, by running the
// real pinGoroutine over a synthetic g.

// w6RegisterPage registers the zgc page containing addr, so pinPtr and
// the canonicalization behind it see a real page. Holders may share one
// page: the pages map keys by page start, so each page registers once
// and frees once.
func w6RegisterPage(t *testing.T, pages map[uintptr]*LogosZgcPage, addr uintptr) {
	t.Helper()
	start := addr &^ (LogosZgcPageSize - 1)
	if _, ok := pages[start]; !ok {
		pages[start] = LogosZgcNewPage(start, 0, 16)
		t.Cleanup(func() { LogosZgcFreePage(pages[start]) })
	}
}

// w6PinOne runs one pinPtr and reports whether the page of addr ended up
// pinned.
func w6PinOne(ev *LogosZgcW6Pin, addr uintptr) bool {
	return LogosZgcW6PinOne(ev, addr)
}

// w6PageStart returns the zgc page start containing addr.
func w6PageStart(addr uintptr) uintptr { return addr &^ (LogosZgcPageSize - 1) }

// w6OnAvoid reports whether addr shares a page with any avoid start.
// Discovery assertions pin through PinG, which also pins the g page
// itself, so a holder sharing the g page would look pinned even with
// its discovery wiring deleted. Isolation below keeps that masking out.
func w6OnAvoid(addr uintptr, avoid []uintptr) bool {
	s := w6PageStart(addr)
	for _, a := range avoid {
		if s == a {
			return true
		}
	}
	return false
}

// w6IsolationBound returns a same-shape allocation count that guarantees
// one lands outside every avoid page. Each avoided page holds at most
// pageBytes/minBytes such objects, so this many retained allocations
// must spill onto a fresh page. No bulk spacers: a large object does
// not exhaust the small size class span the candidates come from, so
// spacers cannot bound the search. Every candidate is a real
// fixture-shaped object and stays retained, so consumed span capacity
// cannot be freed and reused behind the bound.
func w6IsolationBound(avoid int) int {
	const minBytes = 8
	return avoid*int(LogosZgcPageSize/minBytes) + 1
}

// w6AllocArgs returns a heap args block on a page distinct from every
// avoid page. Every candidate stays retained in sink.
func w6AllocArgs(t *testing.T, sink *[]any, avoid []uintptr) *[2]uintptr {
	t.Helper()
	bound := w6IsolationBound(len(avoid))
	for range bound {
		a := new([2]uintptr)
		*sink = append(*sink, a)
		if !w6OnAvoid(uintptr(unsafe.Pointer(a)), avoid) {
			return a
		}
	}
	t.Fatal("isolation bound exhausted without leaving the avoided pages")
	return nil
}

// w6AllocClosure returns a real escaping closure with its funcval word,
// isolated off every avoid page. The word comes from the func value
// itself; nothing is fabricated. The body observably uses the captured
// block (incrementing mark[0]) so the capture cannot be optimized away;
// the closure is never called during the pin assertions.
func w6AllocClosure(t *testing.T, sink *[]any, avoid []uintptr, mark *[2]uintptr) (func(), uintptr) {
	t.Helper()
	bound := w6IsolationBound(len(avoid))
	for range bound {
		c := func() { mark[0]++ }
		*sink = append(*sink, c)
		addr := *(*uintptr)(unsafe.Pointer(&c))
		if !w6OnAvoid(addr, avoid) {
			return c, addr
		}
	}
	t.Fatal("isolation bound exhausted without leaving the avoided pages")
	return nil, 0
}

// TestLogosZgcW6DebugCallHoldersPin is the debug-call half: the holder
// set debugCallWrap constructs — the calling g (stored into
// debugCallWrapArgs), the args block itself (passed through gp.param),
// and the funcval word read from the nosplit frame at debugcall.go —
// must all resolve as pinnable through the cycle's pin primitive, even
// when the holder word carries a stale color the way a plain
// (non-barriered) load leaves it. A colored word must pin the same page
// as its canonical address: pinPtr resolves through zgcCanonical. If the
// pin pass ever stopped resolving colors, the excluded plain load would
// dereference a movable object — this assertion is the tripwire.
func TestLogosZgcW6DebugCallHoldersPin(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)

	pages := map[uintptr]*LogosZgcPage{}

	// The calling g: a detached g-shaped record in _Gwaiting, the
	// state the debug-call parking leaves. Not in allgs.
	gp := LogosZgcW6NewG()
	w6RegisterPage(t, pages, LogosZgcW6GAddr(gp))

	// The args block: an ordinary heap object standing in for the
	// debugCallWrapArgs the real path allocates (dispatch, callingG).
	args := new([2]uintptr)
	args[1] = LogosZgcW6GAddr(gp) // callingG, as the real block carries
	sink := []any{args}
	_ = sink
	argsAddr := uintptr(unsafe.Pointer(args))
	w6RegisterPage(t, pages, argsAddr)

	// The funcval word: a real heap-allocated closure environment, the
	// word the nosplit frame reads behind &fn. A func value's data word
	// names the closure's funcval; retaining the closure in sink keeps
	// the environment alive for the pin assertions below. Primitive
	// coverage only: this holder is fed to pinPtr directly, exactly like
	// the g and the args block above.
	closure := func() { args[0]++ }
	sink = append(sink, closure)
	defer func() { KeepAlive(sink) }()
	fnAddr := *(*uintptr)(unsafe.Pointer(&closure))
	w6RegisterPage(t, pages, fnAddr)

	// Colored variants: canonical address + good color, the way a
	// plain load finds a stale holder in software encoding.
	colored := func(a uintptr) uintptr { return a | LogosZgcGoodBits() }

	ev := LogosZgcW6PinNew()
	defer LogosZgcW6PinFree(ev)

	// Holder words: the canonical and the colored form of each. The
	// pin pass resolves the color through zgcCanonical, so both forms
	// of a holder must pin, and both must land on the same page.
	for _, obj := range []struct{ canon, colored uintptr }{
		{LogosZgcW6GAddr(gp), colored(LogosZgcW6GAddr(gp))}, // calling g
		{argsAddr, colored(argsAddr)},                       // args block
		{fnAddr, colored(fnAddr)},                           // funcval
	} {
		if !w6PinOne(ev, obj.canon) {
			t.Fatalf("holder word %#x did not pin its page: a debug-call holder would be movable while its plain load is excluded from the barrier", obj.canon)
		}
		if !w6PinOne(ev, obj.colored) {
			t.Fatalf("colored holder word %#x did not pin its page: the pin pass stopped resolving colors, and the excluded plain load still dereferences this object", obj.colored)
		}
	}
}

// TestLogosZgcW6PinDiscoversDeferClosure proves the cycle discovers a
// linked defer closure: the synthetic g carries one heap defer record
// calling a real closure, and the real pinGoroutine must pin the
// closure page. Passes while the defer pin loop stands; fails if that
// wiring is ever removed. The g, args, and closure pages are isolated
// so the g self pin cannot mask a missing defer walk, and the count
// pins the exact set: g page plus closure page, nothing else.
func TestLogosZgcW6PinDiscoversDeferClosure(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)

	pages := map[uintptr]*LogosZgcPage{}
	var sink []any
	defer func() { KeepAlive(sink) }()

	gp := LogosZgcW6NewG()
	gAddr := LogosZgcW6GAddr(gp)
	w6RegisterPage(t, pages, gAddr)

	args := w6AllocArgs(t, &sink, []uintptr{w6PageStart(gAddr)})
	argsAddr := uintptr(unsafe.Pointer(args))
	w6RegisterPage(t, pages, argsAddr)

	closure, fnAddr := w6AllocClosure(t, &sink, []uintptr{w6PageStart(gAddr), w6PageStart(argsAddr)}, args)
	w6RegisterPage(t, pages, fnAddr)

	LogosZgcW6PushDefer(gp, closure)

	ev := LogosZgcW6PinNew()
	defer LogosZgcW6PinFree(ev)
	if ok := LogosZgcW6PinG(ev, gp); !ok {
		t.Fatal("pin pass refused a _Gwaiting synthetic g carrying a defer record")
	}
	if !LogosZgcW6PinHas(ev, fnAddr) {
		t.Fatal("PinG did not pin the defer closure page: the defer chain walk is gone")
	}
	if n := LogosZgcW6PinCount(ev); n != 2 {
		t.Fatalf("PinG pinned %d pages, want exactly the g page and the closure page", n)
	}
}

// TestLogosZgcW6CrashStackStateRefusal is the crash half: the pin pass
// must refuse a goroutine whose state it cannot relocate around. The
// crash path runs on g0 (never in allgs, never scan-walked) while the
// runtime is already crashing; its protection is exactly this refusal —
// collectPins returning false moves nothing. A synthetic g in _Grunning
// (a state the pass does not accept) must therefore report false, and
// nothing may be pinned from it.
func TestLogosZgcW6CrashStackStateRefusal(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)

	pages := map[uintptr]*LogosZgcPage{}
	gp := LogosZgcW6NewG()
	w6RegisterPage(t, pages, LogosZgcW6GAddr(gp))

	// The same fixture, switched to _Grunning: the state a crash-path
	// g presents (executing runtime code when the crash starts).
	LogosZgcW6SetRunning(gp, true)

	ev := LogosZgcW6PinNew()
	defer LogosZgcW6PinFree(ev)
	if ok := LogosZgcW6PinG(ev, gp); ok {
		t.Fatal("pin pass accepted a _Grunning synthetic g: the refusal that protects the crash-path holder is gone")
	}
	// The refusal happens after the g itself is pinned (pinPtr runs
	// first), and nothing else may join the set: the holder word the
	// crash frame carries is covered by the g page, and the pass must
	// not have walked defer/waiting/stack chains it declined to
	// handle.
	if LogosZgcW6PinCount(ev) != 1 || !LogosZgcW6PinHas(ev, LogosZgcW6GAddr(gp)) {
		t.Fatalf("refused pin pass pinned %d pages, want exactly the g page itself", LogosZgcW6PinCount(ev))
	}

	// Control: the accepted states must still pin (the fixture back in
	// _Gwaiting, as the debug-call parking leaves it).
	LogosZgcW6SetRunning(gp, false)
	ev2 := LogosZgcW6PinNew()
	defer LogosZgcW6PinFree(ev2)
	if ok := LogosZgcW6PinG(ev2, gp); !ok {
		t.Fatal("pin pass refused a _Gwaiting synthetic g: the debug-call parked state would block every relocation")
	}
	if !LogosZgcW6PinHas(ev2, LogosZgcW6GAddr(gp)) {
		t.Fatal("accepted pin pass did not pin the g itself")
	}
}

// TestLogosZgcW6PinDiscoversParam proves the cycle discovers the args
// block behind gp.param: debugCallWrap stashes debugCallWrapArgs there
// (debugcall.go) and debugCallWrap1 plain-loads it back, while the
// reader is excluded from the load barrier. The real pinGoroutine must
// pin the args page from the param word alone. EXPECTED FAILURE until
// the param pin lands: pinGoroutine visits gp, defer chain, waiters,
// and conservative stack, but never gp.param, so the args page stays
// unpinned. The args page is isolated from the g page so the g self
// pin cannot mask the missing discovery.
func TestLogosZgcW6PinDiscoversParam(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)

	pages := map[uintptr]*LogosZgcPage{}
	var sink []any
	defer func() { KeepAlive(sink) }()

	gp := LogosZgcW6NewG()
	gAddr := LogosZgcW6GAddr(gp)
	w6RegisterPage(t, pages, gAddr)

	args := w6AllocArgs(t, &sink, []uintptr{w6PageStart(gAddr)})
	args[1] = gAddr // callingG, as the real block carries
	argsAddr := uintptr(unsafe.Pointer(args))
	w6RegisterPage(t, pages, argsAddr)

	// The protocol stash, on the real field.
	LogosZgcW6SetParam(gp, unsafe.Pointer(args))
	if LogosZgcW6GetParam(gp) != unsafe.Pointer(args) {
		t.Fatal("gp.param did not take the args block")
	}

	ev := LogosZgcW6PinNew()
	defer LogosZgcW6PinFree(ev)
	if ok := LogosZgcW6PinG(ev, gp); !ok {
		t.Fatal("pin pass refused a _Gwaiting synthetic g with param set")
	}
	if !LogosZgcW6PinHas(ev, argsAddr) {
		t.Fatal("PinG did not pin the gp.param target page: pinGoroutine does not visit gp.param")
	}
}

// TestLogosZgcW6ClearedParamStaysUnpinned is the negative control for
// param discovery: once debugCallWrap1 consumes the block it clears
// gp.param to nil, and the args page must drop out of the pin set. It
// passes before and after the param pin lands, and proves the positive
// above comes from the param word rather than page sharing.
func TestLogosZgcW6ClearedParamStaysUnpinned(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcSelectSoftware(LogosZgcMark0)

	pages := map[uintptr]*LogosZgcPage{}
	var sink []any
	defer func() { KeepAlive(sink) }()

	gp := LogosZgcW6NewG()
	gAddr := LogosZgcW6GAddr(gp)
	w6RegisterPage(t, pages, gAddr)

	args := w6AllocArgs(t, &sink, []uintptr{w6PageStart(gAddr)})
	args[1] = gAddr // callingG, as the real block carries
	argsAddr := uintptr(unsafe.Pointer(args))
	w6RegisterPage(t, pages, argsAddr)

	// The protocol consume, on the real field: param takes the block,
	// then is cleared once consumed (debugCallWrap1: gp.param = nil).
	LogosZgcW6SetParam(gp, unsafe.Pointer(args))
	if LogosZgcW6GetParam(gp) != unsafe.Pointer(args) {
		t.Fatal("gp.param did not take the args block")
	}
	LogosZgcW6SetParam(gp, nil)

	ev := LogosZgcW6PinNew()
	defer LogosZgcW6PinFree(ev)
	if ok := LogosZgcW6PinG(ev, gp); !ok {
		t.Fatal("pin pass refused a _Gwaiting synthetic g with param cleared")
	}
	if LogosZgcW6PinHas(ev, argsAddr) {
		t.Fatal("PinG pinned the args page with param cleared: page sharing is masking discovery, not the param word")
	}
	if !LogosZgcW6PinHas(ev, gAddr) {
		t.Fatal("PinG did not pin the synthetic g itself")
	}
}
