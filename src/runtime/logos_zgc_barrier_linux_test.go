// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && linux && (amd64 || arm64)

package runtime_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	. "runtime"
)

// TestLogosZgcCompiledBarrier builds this package with -lb and runs
// TestLogosZgcCompiledBarrierInner in it, so the barrier under test is the
// one the compiler emits, not the runtime's model of it.
func TestLogosZgcCompiledBarrier(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") != "" {
		t.Skip("inner run")
	}
	testenv.MustHaveGoBuild(t)
	exp := os.Getenv("GOEXPERIMENT")
	if !slices.Contains(strings.Split(exp, ","), "logosgcmoving") {
		exp = strings.TrimPrefix(exp+",logosgcmoving", ",")
	}
	// -ldflags=-X=runtime.logosGCMode=zgc is required: without zgc mode
	// the heap is not restricted to the low window and heap objects can
	// land above the 4 TB granule range, where zgcNewPage throws instead
	// of registering the fixture's pages. Colors and identity are still
	// driven by the tests themselves (LogosZgcReset/
	// LogosZgcSelectSoftware, LogosZgcIdentityKeysOn).
	cmd := exec.Command(testenv.GoToolPath(t), "test", "-count=1", "-gcflags=all=-lb",
		"-ldflags=-X=runtime.logosGCMode=zgc",
		"-run=^(TestLogosZgcCompiledBarrierInner|TestLogosZgcCompiledIdentityInner|TestLogosZgcCompiledReflectcallArgInner|TestLogosZgcCompiledCallRetResultInner|TestLogosZgcCompiledDeferRecordInner)$", "-v", "runtime")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "LOGOS_ZGC_INNER=1", "GOGC=off", "GOEXPERIMENT="+exp)
	out, err := cmd.CombinedOutput()
	// The loop below checks names in list order, so a whole-binary crash
	// would otherwise always be reported against BarrierInner. Report the
	// last "=== RUN" line too: with -v that names the inner test that was
	// actually running when the binary died (empty when it died before the
	// first test started, e.g. in runtime or package init).
	lastRun := ""
	for line := range strings.SplitSeq(string(out), "\n") {
		if name, ok := strings.CutPrefix(line, "=== RUN   "); ok {
			lastRun = name
		}
	}
	for _, name := range []string{"TestLogosZgcCompiledBarrierInner", "TestLogosZgcCompiledIdentityInner", "TestLogosZgcCompiledReflectcallArgInner", "TestLogosZgcCompiledCallRetResultInner", "TestLogosZgcCompiledDeferRecordInner"} {
		if err != nil || !strings.Contains(string(out), "--- PASS: "+name) {
			t.Fatalf("inner run of %s under -lb (last started inner test %q): %v\n%s", name, lastRun, err, out)
		}
	}
}

type logosZgcPairKey struct {
	p *logosZgcNode
	n int
}

// logosZgcIntRef is a pointer key type no other map in the binary uses,
// so reflect.MapOf builds its map with runtime.typehash.
type logosZgcIntRef *int

// TestLogosZgcCompiledIdentityInner runs only inside
// TestLogosZgcCompiledBarrier: the maps below are compiled with -lb, so
// their hashers and key equality are the ones the compiler emits for
// pointer words (runtime.zgcptrhash, runtime.zgcptrequal) and every map
// operation on them takes the hasher path. Keys are inserted by the source
// address, the object relocates, and lookups and assignments by the
// copy's address find the original entries: a pointer key, a struct with
// a pointer and an int, an array of pointers, an interior pointer, and a
// reflect.MapOf map hashed by runtime.typehash.
func TestLogosZgcCompiledIdentityInner(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") == "" {
		t.Skip("runs inside TestLogosZgcCompiledBarrier")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer LogosZgcReset()
	LogosZgcIdentityKeysOn()
	defer LogosZgcIdentityKeysOff()
	LogosZgcSelectSoftware(LogosZgcRemapped)

	src, dst := &logosZgcNode{val: 11}, new(logosZgcNode)
	others := make([]*logosZgcNode, 100)
	for i := range others {
		others[i] = &logosZgcNode{val: i}
	}
	byPtr := map[*logosZgcNode]int{src: 1}
	byPair := map[logosZgcPairKey]int{{src, 5}: 2}
	byArr := map[[2]*logosZgcNode]int{{src, others[0]}: 3}
	byInner := map[*int]int{&src.val: 4}
	byRef := reflect.MakeMap(reflect.MapOf(reflect.TypeFor[logosZgcIntRef](), reflect.TypeFor[int]()))
	byRef.SetMapIndex(reflect.ValueOf(logosZgcIntRef(&src.val)), reflect.ValueOf(5))
	for i, o := range others {
		byPtr[o] = 100 + i
		byPair[logosZgcPairKey{o, 5}] = 100 + i
		byArr[[2]*logosZgcNode{o, others[0]}] = 100 + i
		byInner[&o.val] = 100 + i
		byRef.SetMapIndex(reflect.ValueOf(logosZgcIntRef(&o.val)), reflect.ValueOf(100+i))
	}
	logosZgcSink = []any{src, dst, others, byPtr, byPair, byArr, byInner, byRef.Interface()}
	defer func() { logosZgcSink = nil }()

	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	// Register the source granule explicitly: the page base must be one
	// aligned granule inside the 4 TB range (heap-derived starts are only
	// registrable when the heap itself sits low, i.e. with zgc mode
	// linked). Fail loud here with the test's context instead of tripping
	// the runtime throw mid-fixture. A mapped raw page cannot stand in:
	// the identity hasher resolves keys through heap spans, and a
	// non-span word falls back to address hashing, so only the heap
	// granule holding the key objects preserves the contract below.
	start := a &^ (LogosZgcPageSize - 1)
	if start&(LogosZgcPageSize-1) != 0 {
		t.Fatalf("identity source granule %#x misaligned: want 2 MB alignment", start)
	}
	if start > uintptr(LogosZgcAddrMask)-(LogosZgcPageSize-1) {
		t.Fatalf("identity source granule %#x outside the 4 TB range: heap not low (need zgc mode)", start)
	}
	pg := LogosZgcNewPage(start, 0, 16)
	t.Cleanup(func() { LogosZgcDropPage(pg) })
	if got := LogosZgcRelocate(pg, a, unsafe.Sizeof(*src), d); got != d {
		t.Fatalf("relocation published %#x, want %#x", got, d)
	}

	if v, ok := byPtr[dst]; !ok || v != 1 {
		t.Fatalf("pointer key: lookup by the copy = %d, %v", v, ok)
	}
	if v, ok := byPair[logosZgcPairKey{dst, 5}]; !ok || v != 2 {
		t.Fatalf("struct key: lookup by the copy = %d, %v", v, ok)
	}
	if v, ok := byArr[[2]*logosZgcNode{dst, others[0]}]; !ok || v != 3 {
		t.Fatalf("array key: lookup by the copy = %d, %v", v, ok)
	}
	if v, ok := byInner[&dst.val]; !ok || v != 4 {
		t.Fatalf("interior key: lookup by the copy = %d, %v", v, ok)
	}
	if v := byRef.MapIndex(reflect.ValueOf(logosZgcIntRef(&dst.val))); !v.IsValid() || v.Int() != 5 {
		t.Fatalf("reflect.MapOf key: lookup by the copy = %v", v)
	}
	n := len(byPtr)
	byPtr[dst] = 9
	if len(byPtr) != n || byPtr[src] != 9 {
		t.Fatalf("assign by the copy added a key (%d -> %d) instead of updating", n, len(byPtr))
	}
	delete(byPtr, dst)
	if _, ok := byPtr[src]; ok || len(byPtr) != n-1 {
		t.Fatal("delete by the copy left the source entry")
	}
	for i, o := range others {
		if byPtr[o] != 100+i || byPair[logosZgcPairKey{o, 5}] != 100+i || byInner[&o.val] != 100+i {
			t.Fatalf("unmoved key %d lost its entry", i)
		}
	}
}

type logosZgcNode struct {
	next *logosZgcNode
	val  int
}

type logosZgcHolder struct {
	p   *logosZgcNode
	in  *int
	buf *[8]byte
	ext []byte
}

//go:noinline
func logosZgcLoadP(h *logosZgcHolder) *logosZgcNode { return h.p }

//go:noinline
func logosZgcLoadIn(h *logosZgcHolder) *int { return h.in }

// logosZgcRaw returns the word stored in a pointer slot. It loads a
// uintptr, which takes no barrier.
func logosZgcRaw[T any](slot **T) uintptr { return *(*uintptr)(unsafe.Pointer(slot)) }

func logosZgcSetRaw[T any](slot **T, w uintptr) { *(*uintptr)(unsafe.Pointer(slot)) = w }

// TestLogosZgcCompiledBarrierInner runs only inside
// TestLogosZgcCompiledBarrier, which compiles this package with -lb and
// GOGC=off: the stock collector does not read colors, so no collection
// may run while a slot holds a colored word.
//
// It is the M19-S7 compiler attack at run time: with zgc unconfigured the
// barrier keeps every word and never heals; once configured, an uncolored
// or stale word of a relocated object heals to the good-colored copy and
// the load returns the copy's canonical address, the healed word then
// takes the fast path, an interior pointer keeps its offset, nil and a
// pointer to mmap memory stay unchanged, and a pointer loaded through the
// barrier reaches a syscall untagged (#29) where the colored word itself
// faults.
func TestLogosZgcCompiledBarrierInner(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") == "" {
		t.Skip("runs inside TestLogosZgcCompiledBarrier")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer LogosZgcReset()
	LogosZgcReset()

	src, dst := &logosZgcNode{val: 1}, &logosZgcNode{val: 2}
	h := &logosZgcHolder{p: src, in: &src.val}
	logosZgcSink = []any{src, dst, h}
	defer func() { logosZgcSink = nil }()
	before := LogosZgcHeals()
	if logosZgcLoadP(h) != src || logosZgcRaw(&h.p) != logosZgcAddrOf(src) || LogosZgcHeals() != before {
		t.Fatalf("unconfigured barrier changed the load or healed")
	}

	pages := make(map[uintptr]*LogosZgcPage)
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	LogosZgcForwardInsert(logosZgcPageOf(t, pages, a), a, d)
	logosZgcPageOf(t, pages, d)
	LogosZgcSelectSoftware(LogosZgcRemapped)
	good := uintptr(LogosZgcSoftBits(LogosZgcRemapped))

	for _, w := range []uintptr{a, a + uintptr(LogosZgcSoftBits(LogosZgcMark0))} {
		logosZgcSetRaw(&h.p, w)
		before = LogosZgcHeals()
		if got := logosZgcLoadP(h); got != dst || got.val != 2 {
			t.Fatalf("word %#x: compiled load = %p, want relocated %p", w, got, dst)
		}
		if logosZgcRaw(&h.p) != d+good || LogosZgcHeals() != before+1 {
			t.Fatalf("word %#x: slot %#x heals %d, want %#x and one heal", w, logosZgcRaw(&h.p), LogosZgcHeals()-before, d+good)
		}
		before = LogosZgcHeals()
		if logosZgcLoadP(h) != dst || LogosZgcHeals() != before {
			t.Fatalf("healed word took the slow path")
		}
	}
	if got := logosZgcLoadIn(h); got != &dst.val {
		t.Fatalf("interior pointer loaded %p, want %p", got, &dst.val)
	}

	h.p = nil
	before = LogosZgcHeals()
	if logosZgcLoadP(h) != nil || logosZgcRaw(&h.p) != 0 || LogosZgcHeals() != before {
		t.Fatalf("nil load healed or changed the slot")
	}

	ext, err := syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(ext)
	ext[100] = 42
	h.ext = ext
	extWord := logosZgcAddrOf(&ext[0])
	if h.ext[100] != 42 || logosZgcAddrOf(&h.ext[0]) != extWord {
		t.Fatalf("mmap pointer %#x did not survive the barrier", extWord)
	}

	logosZgcCheckSyscallUntag(t, h, pages, good)
	h.p, h.in, h.buf, h.ext = nil, nil, nil, nil
}

func logosZgcCheckSyscallUntag(t *testing.T, h *logosZgcHolder, pages map[uintptr]*LogosZgcPage, good uintptr) {
	b := &[8]byte{'z', 'g', 'c', '-', 'o', 'k', '!', '\n'}
	logosZgcSink = append(logosZgcSink, b)
	ba := logosZgcAddrOf(b)
	logosZgcPageOf(t, pages, ba)
	logosZgcSetRaw(&h.buf, ba+good)
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if _, _, errno := syscall.Syscall(syscall.SYS_WRITE, uintptr(fds[1]), ba+good, 8); errno != syscall.EFAULT {
		t.Fatalf("write of the colored word itself: errno %v, want EFAULT", errno)
	}
	if n, err := syscall.Write(fds[1], h.buf[:]); n != 8 || err != nil {
		t.Fatalf("write through the barrier: %d, %v", n, err)
	}
	var got [8]byte
	if n, err := syscall.Read(fds[0], got[:]); n != 8 || err != nil || got != *b {
		t.Fatalf("read back %q (%d, %v), want %q", got[:n], n, err, b[:])
	}
}

// TestLogosZgcCompiledReflectcallArgInner runs only inside
// TestLogosZgcCompiledBarrier, which compiles this package with -lb: the
// loads below take the compiler-emitted barrier, the real Go side of the
// reflectcall argument-frame bypass (asm_amd64.s TEXT ·reflectcall CALLFN
// copies the frame with REP MOVSB and dereferences fn with
// MOVQ f+8(FP),DX / MOVQ (DX),R12; asm_arm64.s mirrors them). The test
// stores a colored stale word into a heap source slot with a raw,
// barrier-free store — never pre-canonicalized, so the regression input
// survives — lets the compiler/load path (boxing into the reflect.Call
// argument) feed the call, and compares the raw word the callee receives.
// Every witness observation is a raw uintptr load: a typed read would take
// the barrier and heal, hiding a regression. A colored word reaching the
// asm frame fails the copy-address assertions.
func TestLogosZgcCompiledReflectcallArgInner(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") == "" {
		t.Skip("runs inside TestLogosZgcCompiledBarrier")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer LogosZgcReset()
	LogosZgcReset()

	src, dst := &logosZgcNode{val: 1}, &logosZgcNode{val: 2}
	var sawRaw uintptr
	probe := func(p *logosZgcNode) uintptr {
		sawRaw = *(*uintptr)(unsafe.Pointer(&p))
		return sawRaw
	}
	logosZgcSink = []any{src, dst, probe}
	defer func() { logosZgcSink = nil }()

	pages := make(map[uintptr]*LogosZgcPage)
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	LogosZgcForwardInsert(logosZgcPageOf(t, pages, a), a, d)
	logosZgcPageOf(t, pages, d)
	LogosZgcSelectSoftware(LogosZgcRemapped)
	good := uintptr(LogosZgcSoftBits(LogosZgcRemapped))

	slot := new(*logosZgcNode)
	*slot = src
	logosZgcSink = append(logosZgcSink, slot)
	for _, c := range []uint8{LogosZgcMark0, LogosZgcMark1, LogosZgcFinal} {
		colored := LogosZgcColored(a, c)
		logosZgcSetRaw(slot, colored)
		if logosZgcRaw(slot) != colored {
			t.Fatalf("color %#x did not stick in the source slot: setup lost the regression input", c)
		}
		// The Go-side supply is this compiled barrier load. fn stays
		// canonical throughout: asm dereferences it raw (MOVQ (DX)),
		// so a colored fn would fault, not fail — only the frame
		// words take the regression input, and bare arg bytes never
		// fault (REP MOVSB copies, the Go callee reads without
		// dereferencing).
		healedArg := *slot
		if logosZgcAddrOf(healedArg) != d {
			t.Fatalf("color %#x: barrier load %#x, want copy %#x (colored would reach the asm frame)", c, logosZgcAddrOf(healedArg), d)
		}
		if logosZgcRaw(slot) != d+good {
			t.Fatalf("color %#x: source slot %#x, want healed %#x", c, logosZgcRaw(slot), d+good)
		}
		sawRaw = 0
		out := reflect.ValueOf(probe).Call([]reflect.Value{reflect.ValueOf(healedArg)})
		if sawRaw != d {
			t.Fatalf("color %#x: callee raw word %#x, want copy %#x (asm frame corrupted the word)", c, sawRaw, d)
		}
		if out[0].Uint() != uint64(d) {
			t.Fatalf("color %#x: scalar result %#x, want %#x", c, out[0].Uint(), d)
		}
	}
	KeepAlive(src)
	KeepAlive(dst)
}

// TestLogosZgcCompiledCallRetResultInner runs only inside
// TestLogosZgcCompiledBarrier, which compiles this package with -lb: the
// callee's return-value load takes the compiler-emitted barrier, the real
// Go side feeding the callRet result path (asm_amd64.s TEXT callRet<>
// shuffles the result registers into runtime·reflectcallmove, which copies
// the result bytes; asm_arm64.s mirrors it). The test stores a colored
// stale word into a heap source slot with a raw, barrier-free store —
// never pre-canonicalized — lets the callee's typed return load feed the
// result copy, and compares the raw word of the returned reflect.Value.
// Every witness observation is raw: typed reads heal and hide regressions.
// A colored word reaching the asm result copy fails the assertions.
func TestLogosZgcCompiledCallRetResultInner(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") == "" {
		t.Skip("runs inside TestLogosZgcCompiledBarrier")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer LogosZgcReset()
	LogosZgcReset()

	src, dst := &logosZgcNode{val: 3}, &logosZgcNode{val: 4}
	logosZgcSink = []any{src, dst}
	defer func() { logosZgcSink = nil }()

	pages := make(map[uintptr]*LogosZgcPage)
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	LogosZgcForwardInsert(logosZgcPageOf(t, pages, a), a, d)
	logosZgcPageOf(t, pages, d)
	LogosZgcSelectSoftware(LogosZgcRemapped)
	good := uintptr(LogosZgcSoftBits(LogosZgcRemapped))

	slot := new(*logosZgcNode)
	*slot = src
	logosZgcSink = append(logosZgcSink, slot)
	for _, c := range []uint8{LogosZgcMark0, LogosZgcMark1, LogosZgcFinal} {
		colored := LogosZgcColored(a, c)
		logosZgcSetRaw(slot, colored)
		if logosZgcRaw(slot) != colored {
			t.Fatalf("color %#x did not stick in the source slot: setup lost the regression input", c)
		}
		mkret := func() *logosZgcNode { return *slot }
		out := reflect.ValueOf(mkret).Call(nil)
		if raw := uintptr(out[0].UnsafePointer()); raw != d {
			t.Fatalf("color %#x: returned raw word %#x, want copy %#x (colored reached the asm result copy)", c, raw, d)
		}
		if logosZgcRaw(slot) != d+good {
			t.Fatalf("color %#x: source slot %#x, want healed %#x", c, logosZgcRaw(slot), d+good)
		}
	}
	KeepAlive(src)
	KeepAlive(dst)
}

// TestLogosZgcCompiledDeferRecordInner runs only inside
// TestLogosZgcCompiledBarrier, which compiles this package with -lb. Its
// subjects are the heap defer record's words: panic.go nextDefer returns
// d.fn from the record, and the recorded call arguments travel in the
// record beside it; evacuation-time readers take such words without a
// barrier (pinGoroutine P8 pins the referents as mitigation). A real defer
// statement carries both words into the record. The regression inputs are
// raw, barrier-free colored stores — never pre-canonicalized — and every
// witness observation is a raw uintptr load, since typed reads heal and
// hide regressions. The fn word is checked raw BEFORE the deferred call
// runs, so a healing regression fails safely here instead of faulting
// later at the raw fn dereference; the arg word travels into the record
// through the defer statement's own capture load with no prior check and
// is observed raw at the callee, so a capture regression fails there
// (never faults: record bytes are copied, never dereferenced). A colored
// word reaching the record fails the assertions.
func TestLogosZgcCompiledDeferRecordInner(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") == "" {
		t.Skip("runs inside TestLogosZgcCompiledBarrier")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer LogosZgcReset()
	LogosZgcReset()

	src, dst := &logosZgcNode{val: 5}, &logosZgcNode{val: 6}
	var sawRaw uintptr
	probe := func(p *logosZgcNode) {
		sawRaw = *(*uintptr)(unsafe.Pointer(&p))
	}
	fnslot := new(func(*logosZgcNode))
	*fnslot = probe
	logosZgcSink = []any{src, dst, probe, fnslot}
	defer func() { logosZgcSink = nil }()

	fvCanon := *(*uintptr)(unsafe.Pointer(fnslot))
	pages := make(map[uintptr]*LogosZgcPage)
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	LogosZgcForwardInsert(logosZgcPageOf(t, pages, a), a, d)
	logosZgcPageOf(t, pages, d)
	logosZgcPageOf(t, pages, fvCanon)
	LogosZgcSelectSoftware(LogosZgcRemapped)
	good := uintptr(LogosZgcSoftBits(LogosZgcRemapped))

	slot := new(*logosZgcNode)
	*slot = src
	logosZgcSink = append(logosZgcSink, slot)
	for _, c := range []uint8{LogosZgcMark0, LogosZgcMark1, LogosZgcFinal} {
		coloredFn := LogosZgcColored(fvCanon, c)
		*(*uintptr)(unsafe.Pointer(fnslot)) = coloredFn
		if got := *(*uintptr)(unsafe.Pointer(fnslot)); got != coloredFn {
			t.Fatalf("color %#x did not stick in the fn source slot: setup lost the regression input", c)
		}
		fn := *fnslot
		if got := *(*uintptr)(unsafe.Pointer(&fn)); got != fvCanon {
			t.Fatalf("color %#x: barrier fn load %#x, want canonical %#x (colored would reach the defer record)", c, got, fvCanon)
		}
		if got := *(*uintptr)(unsafe.Pointer(fnslot)); got != fvCanon+good {
			t.Fatalf("color %#x: fn source slot %#x, want recolored %#x", c, got, fvCanon+good)
		}
		coloredArg := LogosZgcColored(a, c)
		logosZgcSetRaw(slot, coloredArg)
		if logosZgcRaw(slot) != coloredArg {
			t.Fatalf("color %#x did not stick in the arg source slot: setup lost the regression input", c)
		}
		sawRaw = 0
		func() {
			defer fn(*slot)
		}()
		if sawRaw != d {
			t.Fatalf("color %#x: deferred callee raw word %#x, want copy %#x (colored reached the defer record)", c, sawRaw, d)
		}
		if logosZgcRaw(slot) != d+good {
			t.Fatalf("color %#x: arg source slot %#x, want healed %#x", c, logosZgcRaw(slot), d+good)
		}
	}
	KeepAlive(src)
	KeepAlive(dst)
}
