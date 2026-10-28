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
	cmd := exec.Command(testenv.GoToolPath(t), "test", "-count=1", "-gcflags=runtime=-lb",
		"-run=^(TestLogosZgcCompiledBarrierInner|TestLogosZgcCompiledIdentityInner)$", "-v", "runtime")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "LOGOS_ZGC_INNER=1", "GOGC=off", "GOEXPERIMENT="+exp)
	out, err := cmd.CombinedOutput()
	for _, name := range []string{"TestLogosZgcCompiledBarrierInner", "TestLogosZgcCompiledIdentityInner"} {
		if err != nil || !strings.Contains(string(out), "--- PASS: "+name) {
			t.Fatalf("inner run of %s under -lb: %v\n%s", name, err, out)
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

	pages := make(map[uintptr]*LogosZgcPage)
	a, d := logosZgcAddrOf(src), logosZgcAddrOf(dst)
	if got := LogosZgcRelocate(logosZgcPageOf(t, pages, a), a, unsafe.Sizeof(*src), d); got != d {
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
