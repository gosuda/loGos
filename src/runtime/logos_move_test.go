// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
	"weak"
)

func unsafePointer[T any](p *T) unsafe.Pointer { return unsafe.Pointer(p) }

// logosWaitForGC forces collections until done is closed. It never assumes
// that a particular collection runs a finalizer or cleanup.
func logosWaitForGC(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		runtime.GC()
		select {
		case <-done:
			return
		case <-deadline:
			t.Fatalf("%s did not run", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestLogosNoRelocFinalizerSetAndRemove checks #175: attaching a finalizer
// flags the span, and removing the finalizer does not clear the flag.
func TestLogosNoRelocFinalizerSetAndRemove(t *testing.T) {
	if !runtime.LogosMovingExperiment() {
		t.Skip("relocation exclusion is inactive when GOEXPERIMENT=logosgcmoving is off")
	}
	x := new([1000]byte)
	if runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Skip("size class span already flagged by an earlier test")
	}
	runtime.SetFinalizer(x, func(*[1000]byte) {})
	if !runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Fatal("span not flagged after SetFinalizer")
	}
	runtime.SetFinalizer(x, nil)
	if !runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Fatal("removing the finalizer cleared the flag")
	}
	runtime.KeepAlive(x)
}

// TestLogosNoRelocCleanupSetAndStop checks #175: attaching a cleanup flags
// the span, and Cleanup.Stop does not clear the flag.
func TestLogosNoRelocCleanupSetAndStop(t *testing.T) {
	if !runtime.LogosMovingExperiment() {
		t.Skip("relocation exclusion is inactive when GOEXPERIMENT=logosgcmoving is off")
	}
	x := new([2000]byte)
	if runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Skip("size class span already flagged by an earlier test")
	}
	c := runtime.AddCleanup(x, func(int) {}, 0)
	if !runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Fatal("span not flagged after AddCleanup")
	}
	c.Stop()
	if !runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Fatal("Cleanup.Stop cleared the flag")
	}
	runtime.KeepAlive(x)
}

// TestLogosNoRelocIgnoresWeakHandle checks that only finalizer and cleanup
// specials flag a span.
func TestLogosNoRelocIgnoresWeakHandle(t *testing.T) {
	if !runtime.LogosMovingExperiment() {
		t.Skip("relocation exclusion is inactive when GOEXPERIMENT=logosgcmoving is off")
	}
	x := new([3000]byte)
	if runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Skip("size class span already flagged by an earlier test")
	}
	wp := weak.Make(x)
	if runtime.LogosSpanNoReloc(unsafePointer(x)) {
		t.Fatal("weak handle flagged the span")
	}
	if wp.Value() != x {
		t.Fatal("weak pointer lost its target")
	}
	runtime.KeepAlive(x)
}

// TestLogosNoRelocClearedByInit checks #175: only mspan.init clears the flag.
func TestLogosNoRelocClearedByInit(t *testing.T) {
	if !runtime.LogosMovingExperiment() {
		t.Skip("relocation exclusion is inactive when GOEXPERIMENT=logosgcmoving is off")
	}
	before, after := runtime.LogosSpanInitClearsNoReloc()
	if !before {
		t.Fatal("flag not set before init")
	}
	if after {
		t.Fatal("mspan.init did not clear the flag")
	}
}

type logosResurrect struct {
	pad [100]byte
	v   int
}

var logosResurrectGlobal atomic.Pointer[logosResurrect]

// TestLogosFinalizerResurrectionAndReRegistration checks that upstream
// resurrection and re-registration survive the flag (T-0.1).
func TestLogosFinalizerResurrectionAndReRegistration(t *testing.T) {
	done := make(chan struct{})
	var runs atomic.Int32
	var fn func(*logosResurrect)
	fn = func(p *logosResurrect) {
		if runs.Add(1) == 1 {
			logosResurrectGlobal.Store(p)
			runtime.SetFinalizer(p, fn)
			logosResurrectGlobal.Store(nil)
			return
		}
		logosResurrectGlobal.Store(p)
		close(done)
	}
	func() {
		x := &logosResurrect{v: 42}
		runtime.SetFinalizer(x, fn)
	}()
	logosWaitForGC(t, done, "re-registered finalizer")
	p := logosResurrectGlobal.Load()
	if p == nil || p.v != 42 {
		t.Fatalf("resurrected object corrupt: %+v", p)
	}
	if runs.Load() != 2 {
		t.Fatalf("finalizer ran %d times, want 2", runs.Load())
	}
}

// TestLogosFinalizerThenCleanupReclaims checks that an excluded span still
// reclaims ordinarily: after the finalizer ran and the object died, its
// cleanup (which runs only after finalization) runs too.
func TestLogosFinalizerThenCleanupReclaims(t *testing.T) {
	fin := make(chan struct{})
	clean := make(chan struct{})
	var sibling weak.Pointer[[5000]byte]
	func() {
		x := new([5000]byte)
		y := new([5000]byte)
		sibling = weak.Make(y)
		runtime.SetFinalizer(x, func(*[5000]byte) { close(fin) })
		runtime.AddCleanup(x, func(chan struct{}) { close(clean) }, clean)
	}()
	logosWaitForGC(t, fin, "finalizer")
	logosWaitForGC(t, clean, "cleanup after finalizer")
	if sibling.Value() != nil {
		t.Fatal("unreferenced sibling in the flagged span survived")
	}
}

// TestLogosCleanupStopPreventsCall checks that Stop still cancels a cleanup
// on a flagged span.
func TestLogosCleanupStopPreventsCall(t *testing.T) {
	ran := make(chan struct{}, 1)
	func() {
		x := new([7000]byte)
		c := runtime.AddCleanup(x, func(chan struct{}) { ran <- struct{}{} }, ran)
		c.Stop()
	}()
	for range 5 {
		runtime.GC()
	}
	select {
	case <-ran:
		t.Fatal("stopped cleanup ran")
	default:
	}
}

// buildLogosMove builds testdata/testlogosmove with the moving experiment.
// The immix variant is linked with -X=runtime.logosGCMode=immix, the way the
// linker selects the mode; the plain variant is an ordinary Go program.
func buildLogosMove(t *testing.T, immix bool) string {
	t.Helper()
	name := "plain"
	args := []string{"build"}
	exp := os.Getenv("GOEXPERIMENT")
	if immix {
		name = "immix"
		args = append(args, "-ldflags=-X=runtime.logosGCMode=immix")
		exp = strings.TrimPrefix(exp+",logosgcmoving", ",")
	}
	exe := filepath.Join(t.TempDir(), name+".exe")
	args = append(args, "-o", exe)
	cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
	cmd.Dir = "testdata/testlogosmove"
	cmd.Env = append(os.Environ(), "GOEXPERIMENT="+exp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", cmd, err, out)
	}
	return exe
}

func runLogosMove(t *testing.T, exe, scenario, expectMove string, env ...string) {
	t.Helper()
	cmd := testenv.Command(t, exe, scenario)
	cmd.Env = append(os.Environ(), append([]string{"LOGOS_EXPECT_MOVE=" + expectMove}, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("%s: %v\n%s", scenario, err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

var logosMoveScenarios = []string{
	"canmove", "evacuate", "pinner", "mapkey", "syscall", "weakunique",
	"finreg", "finqueued", "finresurrect", "finreclaim", "cleanupmoved", "cleanupstop",
	"deferheld", "panicheld", "ctxheld", "rootspreserved",
	"interiorheld", "boxroots", "sizemix", "attachrace",
	"cbcorigin", "cbpinnersep", "cbretire",
}

// TestLogosMoveScenarios runs every scenario under immix with and without
// the verification pass, and the same program under the plain collector,
// where nothing may move (#159, #160, #175, T-0.1).
func TestLogosMoveScenarios(t *testing.T) {
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no subprocesses")
	}
	testenv.MustHaveGoBuild(t)
	immix, plain := buildLogosMove(t, true), buildLogosMove(t, false)
	for _, s := range logosMoveScenarios {
		t.Run("immix/"+s, func(t *testing.T) {
			runLogosMove(t, immix, s, "1", "GOMAXPROCS=1")
			runLogosMove(t, immix, s, "1", "GOMAXPROCS=4", "GODEBUG=gccheckmark=1")
		})
		t.Run("plain/"+s, func(t *testing.T) {
			runLogosMove(t, plain, s, "0", "GOMAXPROCS=2")
		})
	}
}

type logosMoveNode struct {
	next *logosMoveNode
	v    int
	pad  [5]uintptr
}

var logosMoveSink *logosMoveNode

// logosMoveSparse leaves a dense group with holes and a sparse group of 1 in
// 10 survivors, so that the sparse group's blocks are fragmented.
func logosMoveSparse() []*logosMoveNode {
	dense := make([]*logosMoveNode, 20000)
	for i := range dense {
		dense[i] = &logosMoveNode{v: i}
		if i%5 >= 3 {
			dense[i] = nil
		}
	}
	var sparse []*logosMoveNode
	for i := range 20000 {
		n := &logosMoveNode{v: i}
		if i%10 == 0 {
			sparse = append(sparse, n)
		}
	}
	logosMoveSink = dense[0]
	return sparse
}

// TestLogosFastCallPinSurvivesEvacuation checks #160 in the moving mode: the
// Go pointer argument of a fast call stays where it is while the call is in
// flight, and nothing else about the survivors breaks. It runs only in a test
// binary linked with -X=runtime.logosGCMode=immix and GOEXPERIMENT=logosgcmoving.
func TestLogosFastCallPinSurvivesEvacuation(t *testing.T) {
	if !runtime.LogosHeapObjectsCanMove() {
		t.Skip("needs the immix collector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	sparse := logosMoveSparse()
	arg := sparse[len(sparse)/2]
	at := uintptr(unsafe.Pointer(arg))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, _, ok := runtime.LogosEnterFastCallForTest(unsafe.Pointer(arg), 1); !ok {
		t.Fatal("no P to record the fast call on")
	}
	runtime.GC()
	runtime.LogosExitFastCallForTest()
	if uintptr(unsafe.Pointer(arg)) != at {
		t.Fatal("the argument of a fast call in flight moved")
	}
	for i, n := range sparse {
		if n.v != i*10 {
			t.Fatalf("survivor %d holds %d", i, n.v)
		}
	}
}

// logosRelocBlob is a pointer-free object of exactly 1152 bytes, a size class
// with 7 objects per 8 KiB span, so that a partly filled span has unallocated
// slots and a 32 KiB block holds four spans.
type logosRelocBlob struct {
	id  uintptr
	pad [1152 - unsafe.Sizeof(uintptr(0))]byte
}

// logosRelocKeep keeps the tracked blobs reachable. It is a global so that
// the allocations escape and every roots-walk of the moving collector finds
// it.
var logosRelocKeep []*logosRelocBlob

func logosRelocFill(b *logosRelocBlob, id uintptr) {
	b.id = id
	for i := range b.pad {
		b.pad[i] = byte(id) + byte(i)
	}
}

func logosRelocIntact(b *logosRelocBlob, id uintptr) bool {
	if b.id != id {
		return false
	}
	for i := range b.pad {
		if b.pad[i] != byte(id)+byte(i) {
			return false
		}
	}
	return true
}

// TestLogosRelocationCounters checks that moving an object is not an
// allocation. runtime.MemStats counts heap objects allocated and freed, and
// Mallocs-Frees is the number of live objects. In a window in which the
// program allocates nothing and one collection runs, Mallocs of a size class
// must not grow and Frees of it must grow by the number of objects that died
// and no more, whether or not the collector relocated survivors out of
// uncached spans. The evacuation never moves objects out of a span that an
// mcache holds, and this test does not cover that case.
//
// Each trial allocates 700 blobs of one size class and keeps every tenth (the
// sparse group, whose 32 KiB blocks are fragmented), then a dense group that
// ends in a partly filled span. ReadMemStats flushes the mcaches, so every
// span is uncached and the partly filled one holds slots that no allocation
// ever counted, next to slots that an allocation counted and the mark finds
// dead. The window is one runtime.GC between two ReadMemStats calls with GC
// percent -1. The class counters (MemStats.BySize) are the exact oracles, so
// an allocation in another class cannot disturb them; the aggregate Mallocs and
// TotalAlloc deltas are logged and not asserted, because other runtime
// goroutines may allocate in other classes. TotalAlloc of a class is
// Mallocs*size, so the class assertions cover the bytes of the class; the
// runtime's internal byte counters are cross-checked on every ReadMemStats by
// the doubleCheckReadMemStats mode that gc_test.go enables for this binary.
// The program's own count of the
// objects it dropped, and the contents of every survivor, are independent of
// the collector. Trials differ
// in the number of dense objects, which moves the partly filled span within
// its 32 KiB block. The plain collector is the control: it moves nothing and
// must pass the same checks. Under immix, some tracked survivor must change
// address across the trials, so that a collector that stopped evacuating
// cannot pass.
//
// The class counters are exact only if no other goroutine of the process
// allocates in the class during a window. So the whole test runs in its own
// copy of the test process: the test binary starts itself again and runs only
// this test, the way TestArenaCollision starts a copy of itself. The child
// inherits the environment, and with it GODEBUG, and it is the same binary,
// so it runs the same collector mode. The child sets GOMAXPROCS(1) for its
// trials, as testing.AllocsPerRun does, and each trial runs a GC warmup before
// its setup, so the collector's workers exist before the window. Creating an M
// allocates two profiling buffers of this class (allocm, mcommoninit,
// makeProfStackFP), so a window that starts a thread would count them. The
// child runs every trial and fails unless each passes. Under immix it also
// fails unless some tracked object changed address across the trials, and the
// parent fails unless the child passes.
func TestLogosRelocationCounters(t *testing.T) {
	if os.Getenv("TEST_LOGOS_RELOCATION_CHILD") == "" {
		cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestLogosRelocationCounters$", "-test.v")
		cmd.Env = append(os.Environ(), "TEST_LOGOS_RELOCATION_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestLogosRelocationCounters (") {
			t.Fatalf("%v\n%s", err, out)
		}
		t.Logf("%s", strings.TrimSpace(string(out)))
		return
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	const trials = 4
	observed := 0
	for trial := range trials {
		observed += logosRelocTrial(t, trial)
	}
	if runtime.LogosHeapObjectsCanMove() && observed == 0 {
		t.Fatalf("no tracked object changed address in %d trials: the window did not exercise the evacuation", trials)
	}
}

// logosRelocTrial runs one trial in the child and returns how many tracked
// objects changed address in its window.
func logosRelocTrial(t *testing.T, trial int) int {
	const (
		spanObjs  = 7   // objects of the class per 8 KiB span
		sparseN   = 700 // 100 whole spans
		keepEvery = 10
		keepN     = sparseN / keepEvery
		hostBase  = 393 // 56 whole spans and one object of the next
		hostID    = 1 << 20
	)
	var base, before, after runtime.MemStats
	runtime.ReadMemStats(&base)
	sc := -1
	for i, b := range base.BySize {
		if uintptr(b.Size) == unsafe.Sizeof(logosRelocBlob{}) {
			sc = i
		}
	}
	if sc < 0 {
		t.Fatalf("no size class of %d bytes", unsafe.Sizeof(logosRelocBlob{}))
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	defer func() { logosRelocKeep = nil }()
	canMove := runtime.LogosHeapObjectsCanMove()
	// One more span per trial moves the partly filled span to another place
	// in its 32 KiB block and keeps it one object full.
	hostN := hostBase + spanObjs*trial
	logosRelocKeep = make([]*logosRelocBlob, 0, keepN+hostN)
	addrs := make([]uintptr, 0, keepN+hostN)
	// Settle the sweep, so that the baseline starts without free slots to
	// reuse.
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&base)
	for i := range sparseN {
		b := new(logosRelocBlob)
		logosRelocFill(b, uintptr(i))
		if i%keepEvery == 0 {
			logosRelocKeep = append(logosRelocKeep, b)
		}
	}
	for j := range hostN {
		b := new(logosRelocBlob)
		logosRelocFill(b, hostID+uintptr(j))
		logosRelocKeep = append(logosRelocKeep, b)
	}
	for _, p := range logosRelocKeep {
		addrs = append(addrs, uintptr(unsafe.Pointer(p)))
	}

	runtime.ReadMemStats(&before)
	cycles0, moved0 := runtime.LogosEvacTotals()
	runtime.GC()
	runtime.ReadMemStats(&after)
	cycles1, moved1 := runtime.LogosEvacTotals()

	observed := 0
	for i, p := range logosRelocKeep {
		if uintptr(unsafe.Pointer(p)) != addrs[i] {
			observed++
		}
		want := uintptr(i * keepEvery)
		if i >= keepN {
			want = hostID + uintptr(i-keepN)
		}
		if !logosRelocIntact(p, want) {
			t.Fatalf("tracked object %d lost its contents or its reference", i)
		}
	}

	setup := before.BySize[sc].Mallocs - base.BySize[sc].Mallocs
	if setup != uint64(sparseN+hostN) || before.BySize[sc].Frees != base.BySize[sc].Frees {
		t.Fatalf("setup of size class %d counted %d mallocs and %d frees, want %d and 0: the class is not isolated",
			sc, setup, before.BySize[sc].Frees-base.BySize[sc].Frees, sparseN+hostN)
	}
	dM := after.BySize[sc].Mallocs - before.BySize[sc].Mallocs
	dF := after.BySize[sc].Frees - before.BySize[sc].Frees
	dead := int64(sparseN - keepN)
	dMallocs := after.Mallocs - before.Mallocs
	dTotalAlloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("trial %d: class %d window: Mallocs +%d Frees +%d, %d objects died; all classes: Mallocs +%d TotalAlloc +%d; collector: %d evacuations, %d objects moved; %d of %d tracked objects changed address",
		trial, sc, dM, dF, dead, dMallocs, dTotalAlloc, cycles1-cycles0, moved1-moved0, observed, len(logosRelocKeep))

	if canMove {
		if cycles1-cycles0 != 1 {
			t.Fatalf("the window ran %d evacuations, want 1", cycles1-cycles0)
		}
		if moved1-moved0 < uint64(observed) {
			t.Fatalf("the collector reports %d moved objects, but %d tracked objects changed address", moved1-moved0, observed)
		}
	} else if cycles1 != cycles0 || observed != 0 {
		t.Fatalf("the plain collector evacuated %d times and moved %d tracked objects", cycles1-cycles0, observed)
	}
	if dM != 0 {
		t.Errorf("Mallocs of size class %d grew by %d in a window in which the program allocated nothing; %d objects moved, %d of them tracked",
			sc, dM, moved1-moved0, observed)
	}
	if dF != uint64(dead) {
		t.Errorf("Frees of size class %d grew by %d, want %d, the objects the program dropped; %d objects moved, %d of them tracked",
			sc, dF, dead, moved1-moved0, observed)
	}
	if int64(dM)-int64(dF) != -dead {
		t.Errorf("live objects of size class %d changed by %d, want %d", sc, int64(dM)-int64(dF), -dead)
	}
	return observed
}
