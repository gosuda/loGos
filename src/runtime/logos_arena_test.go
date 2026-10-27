// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosarena

package runtime_test

import (
	"fmt"
	"internal/goexperiment"
	"math"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

const arenaScanUnit = runtime.LogosArenaUnit

var arenaSink any

func arenaStride(size uintptr) uintptr {
	return (size+arenaScanUnit-1)&^(arenaScanUnit-1) + runtime.LogosArenaScanHeaderBytes
}

func inFreshGoroutine(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan any)
	go func() {
		defer func() { done <- recover() }()
		fn()
	}()
	if r := <-done; r != nil {
		t.Fatalf("panic in goroutine: %v", r)
	}
}

func recoverMsg(fn func()) (msg string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			msg, panicked = fmt.Sprint(r), true
		}
	}()
	fn()
	return
}

func waitLeased(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, leased, _ := runtime.LogosArenaCounts()
		if leased == want || time.Now().After(deadline) {
			return leased
		}
		time.Sleep(time.Millisecond)
	}
}

func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for runtime.NumGoroutine() > want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func fillBytes(p unsafe.Pointer, size uintptr, v byte) {
	b := unsafe.Slice((*byte)(p), size)
	for i := range b {
		b[i] = v
	}
}

func checkBytes(p unsafe.Pointer, size uintptr, v byte) bool {
	for _, x := range unsafe.Slice((*byte)(p), size) {
		if x != v {
			return false
		}
	}
	return true
}

func fillWords(p unsafe.Pointer, size uintptr, v *int) {
	words := unsafe.Slice((**int)(p), size/unsafe.Sizeof(v))
	for i := range words {
		words[i] = v
	}
}

// M18-S1 (#4, AR4): a goroutine that allocates and then parks with empty
// regions returns its chunk. After 100,000 goroutines parked this way, in
// waves because the race detector allows 8128 live goroutines at most, the
// leased count is back at baseline after every wave, and the chunks created
// stay near the number of Ps because parked goroutines recycle through the P
// caches.
func TestLogosArenaParkReturnsChunk(t *testing.T) {
	const (
		waves   = 25
		perWave = 4000
		all     = waves * perWave
	)
	goroutines := runtime.NumGoroutine()
	totalBase, leasedBase, _ := runtime.LogosArenaCounts()
	for w := range waves {
		release := make(chan struct{})
		var started sync.WaitGroup
		started.Add(perWave)
		for range perWave {
			go func() {
				m := runtime.LogosArenaSave()
				runtime.LogosArenaAlloc(byte(0), 64)
				runtime.LogosArenaAllocScan((*int)(nil), 48)
				runtime.LogosArenaRestore(m)
				started.Done()
				<-release
			}()
		}
		started.Wait()
		if leased := waitLeased(t, leasedBase); leased != leasedBase {
			t.Fatalf("wave %d: leased chunks = %d with %d goroutines parked, want baseline %d", w, leased, perWave, leasedBase)
		}
		close(release)
		waitGoroutines(t, goroutines)
	}
	total, _, _ := runtime.LogosArenaCounts()
	if grown, limit := total-totalBase, 4*runtime.GOMAXPROCS(0)+16; grown > limit {
		t.Errorf("%d chunks created for %d goroutines, want at most %d: parked goroutines do not recycle chunks", grown, all, limit)
	}
}

// M18-S1 (AR4): a goroutine that parks while arena memory is live keeps its
// chunk, and returns it when it parks again with empty regions.
func TestLogosArenaParkKeepsLiveChunk(t *testing.T) {
	_, leasedBase, _ := runtime.LogosArenaCounts()
	parked := make(chan struct{})
	wake := make(chan struct{})
	wake2 := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m := runtime.LogosArenaSave()
		runtime.LogosArenaAllocScan((*int)(nil), 32)
		parked <- struct{}{}
		<-wake
		runtime.LogosArenaRestore(m)
		<-wake2
	}()
	<-parked
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, leased, _ := runtime.LogosArenaCounts(); leased != leasedBase+1 {
			t.Fatalf("leased = %d while a goroutine is parked with live arena memory, want %d", leased, leasedBase+1)
		}
		time.Sleep(time.Millisecond)
	}
	wake <- struct{}{}
	if leased := waitLeased(t, leasedBase); leased != leasedBase {
		t.Errorf("leased = %d after the goroutine parked with empty regions, want %d", leased, leasedBase)
	}
	wake2 <- struct{}{}
	<-done
}

// M18-S1: a mark taken while the arena is empty stays valid when the
// goroutine parks, the park returns its chunk, and a later allocation leases
// again.
func TestLogosArenaMarkSurvivesPark(t *testing.T) {
	_, leasedBase, _ := runtime.LogosArenaCounts()
	park := make(chan struct{})
	wake := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m0 := runtime.LogosArenaSave()
		runtime.LogosArenaAlloc(byte(0), 16)
		runtime.LogosArenaRestore(m0)
		if st := runtime.LogosArenaStat(); st.Chunks != 1 || st.NoscanUsed != 0 {
			t.Errorf("after restore: %+v, want one empty chunk", st)
		}
		m := runtime.LogosArenaSave()
		park <- struct{}{}
		<-wake
		if st := runtime.LogosArenaStat(); st.Chunks != 0 {
			t.Errorf("chunks = %d after parking with empty regions, want 0", st.Chunks)
		}
		runtime.LogosArenaAlloc(byte(0), 16)
		runtime.LogosArenaRestore(m)
		if st := runtime.LogosArenaStat(); st.Chunks != 1 || st.NoscanUsed != 0 {
			t.Errorf("after restoring a mark from before the park: %+v, want one empty chunk", st)
		}
	}()
	<-park
	if leased := waitLeased(t, leasedBase); leased != leasedBase {
		t.Errorf("leased = %d with the goroutine parked at an empty mark, want %d", leased, leasedBase)
	}
	wake <- struct{}{}
	<-done
}

// M18-S1 (AR4, AR2): a goroutine that exits without restoring its watermark,
// as after Goexit, still returns its chunk, and the scan memory it left is
// zero.
func TestLogosArenaExitReturnsChunk(t *testing.T) {
	_, leasedBase, _ := runtime.LogosArenaCounts()
	const size = 4096
	var obj unsafe.Pointer
	filled := make(chan struct{})
	x := new(int)
	go func() {
		obj = runtime.LogosArenaAllocScan((*int)(nil), size)
		fillWords(obj, size, x)
		runtime.LogosArenaAlloc(byte(0), 64)
		close(filled)
	}()
	<-filled
	if leased := waitLeased(t, leasedBase); leased != leasedBase {
		t.Fatalf("leased = %d after the goroutine exited, want %d", leased, leasedBase)
	}
	if !checkBytes(obj, size, 0) {
		t.Error("scan memory of an exited goroutine is not zero")
	}
}

// M18-S1 (#106 part one): chunks are manual spans carrying the arena flag;
// heap objects are neither.
func TestLogosArenaChunksAreManualSpans(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		noscan := runtime.LogosArenaAlloc(byte(0), 8)
		scan := runtime.LogosArenaAllocScan((*int)(nil), 8)
		for name, p := range map[string]unsafe.Pointer{"noscan": noscan, "scan": scan} {
			manual, flagged := runtime.LogosArenaSpanKind(p)
			if !manual || !flagged {
				t.Errorf("%s object span: manual=%v flagged=%v, want both true", name, manual, flagged)
			}
			if !runtime.LogosArenaInChunk(p) {
				t.Errorf("%s object is not recognized as an arena address", name)
			}
		}
		heap := new(int)
		arenaSink = heap
		if manual, flagged := runtime.LogosArenaSpanKind(unsafe.Pointer(heap)); manual || flagged {
			t.Errorf("heap object span: manual=%v flagged=%v, want both false", manual, flagged)
		}
		if runtime.LogosArenaInChunk(unsafe.Pointer(heap)) {
			t.Error("heap object is recognized as an arena address")
		}
	})
}

// M18-S1 (#106): SetFinalizer and AddCleanup on an arena object panic with
// the SPEC text, for both regions; a heap object, including one served by
// the mallocgc fallback, is unaffected.
func TestLogosArenaSetFinalizerPanics(t *testing.T) {
	type T struct{ a, b uintptr }
	type S struct {
		p *int
		n uintptr
	}
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		noscan := (*T)(runtime.LogosArenaAlloc(uintptr(0), unsafe.Sizeof(T{})))
		scan := (*S)(runtime.LogosArenaAllocScan((*int)(nil), unsafe.Sizeof(S{})))
		const want = "runtime: SetFinalizer on loGo/s arena object"
		for name, obj := range map[string]any{"noscan": noscan, "scan": scan} {
			msg, panicked := recoverMsg(func() { runtime.SetFinalizer(obj, nil) })
			if !panicked || msg != want {
				t.Errorf("SetFinalizer on %s arena object: panicked=%v msg=%q, want %q", name, panicked, msg, want)
			}
		}
		msg, panicked := recoverMsg(func() { runtime.SetFinalizer(noscan, func(*T) {}) })
		if !panicked || msg != want {
			t.Errorf("SetFinalizer with a finalizer: panicked=%v msg=%q, want %q", panicked, msg, want)
		}
		wantCleanup := "runtime: AddCleanup on loGo/s arena object"
		msg, panicked = recoverMsg(func() { runtime.AddCleanup(noscan, func(int) {}, 0) })
		if !panicked || msg != wantCleanup {
			t.Errorf("AddCleanup on arena object: panicked=%v msg=%q, want %q", panicked, msg, wantCleanup)
		}
		heap := new(T)
		if msg, panicked := recoverMsg(func() { runtime.SetFinalizer(heap, func(*T) {}) }); panicked {
			t.Errorf("SetFinalizer on a heap object panicked: %s", msg)
		}
		big := runtime.LogosArenaAlloc(byte(0), 1<<20)
		if runtime.LogosArenaInChunk(big) {
			t.Fatal("oversize request was served from a chunk")
		}
		if msg, panicked := recoverMsg(func() { runtime.SetFinalizer((*byte)(big), func(*byte) {}) }); panicked {
			t.Errorf("SetFinalizer on a mallocgc fallback object panicked: %s", msg)
		}
	})
}

// M18-S1 (#59): cgo pointer checks accept arena addresses. An arena address
// is Go memory for the checks, so storing into arena memory is not a store
// into non-Go memory, and no argument, result, or copy involving arena
// memory panics.
func TestLogosArenaCgoChecksAcceptArenaAddresses(t *testing.T) {
	type holder struct{ p *int }
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		leaf := (*int)(runtime.LogosArenaAlloc(0, 8))
		h := (*holder)(runtime.LogosArenaAllocScan((*int)(nil), unsafe.Sizeof(holder{})))
		h.p = leaf
		bytes := unsafe.Slice((*byte)(runtime.LogosArenaAlloc(byte(0), 32)), 32)
		heapLeaf := new(int)
		if !runtime.LogosArenaIsGoPointer(unsafe.Pointer(leaf)) || !runtime.LogosArenaIsGoPointer(unsafe.Pointer(h)) {
			t.Error("an arena address is not a Go pointer for the cgo checks")
		}
		cases := []struct {
			name string
			fn   func()
		}{
			{"pointer to noscan object", func() { runtime.LogosArenaCgoCheckPointer(leaf, true) }},
			{"pointer to scan object", func() { runtime.LogosArenaCgoCheckPointer(h, true) }},
			{"unsafe.Pointer to arena", func() { runtime.LogosArenaCgoCheckPointer(unsafe.Pointer(h), true) }},
			{"slice over arena bytes", func() { runtime.LogosArenaCgoCheckPointer(&bytes[0], bytes) }},
			{"arena struct holding an arena pointer", func() { runtime.LogosArenaCgoCheckPointer(*h, nil) }},
			{"result holding an arena pointer", func() { runtime.LogosArenaCgoCheckResult(h) }},
			{"heap pointer stored into arena memory", func() {
				runtime.LogosArenaCgoCheckPtrWrite((*unsafe.Pointer)(unsafe.Pointer(&h.p)), unsafe.Pointer(heapLeaf))
			}},
			{"arena pointer stored into heap memory", func() {
				var slot *int
				runtime.LogosArenaCgoCheckPtrWrite((*unsafe.Pointer)(unsafe.Pointer(&slot)), unsafe.Pointer(leaf))
			}},
		}
		for _, c := range cases {
			if msg, panicked := recoverMsg(c.fn); panicked {
				t.Errorf("%s: cgo check panicked: %s", c.name, msg)
			}
		}
	})
}

// M18-S1 (PLAN:547): exact fit. Two noscan objects that fill the chunk to
// its last byte stay in one chunk with no free space left.
func TestLogosArenaExactFit(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		const capacity = runtime.LogosArenaCapacity
		a := runtime.LogosArenaAlloc(byte(0), capacity-8)
		b := runtime.LogosArenaAlloc(byte(0), 8)
		st := runtime.LogosArenaStat()
		if st.Chunks != 1 || st.Free != 0 || st.NoscanUsed != capacity {
			t.Errorf("after an exact fill: %+v, want one chunk, no free space, %d bytes used", st, capacity)
		}
		fillBytes(a, capacity-8, 0x11)
		fillBytes(b, 8, 0x22)
		if !checkBytes(a, capacity-8, 0x11) || !checkBytes(b, 8, 0x22) {
			t.Error("objects of an exact fill overlap")
		}
	})
}

// M18-S1 (PLAN:547): exact fit across both regions. A scan object takes
// exactly the space the noscan region leaves.
func TestLogosArenaExactFitBothRegions(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		const noscan = 4096
		runtime.LogosArenaAlloc(byte(0), noscan)
		scanSize := uintptr(runtime.LogosArenaCapacity-noscan) - runtime.LogosArenaScanHeaderBytes
		p := runtime.LogosArenaAllocScan((*int)(nil), scanSize)
		st := runtime.LogosArenaStat()
		if st.Chunks != 1 || st.Free != 0 {
			t.Errorf("after an exact dual-region fill: %+v, want one chunk, no free space", st)
		}
		if !runtime.LogosArenaInChunk(p) {
			t.Error("the exactly fitting scan object is not in the chunk")
		}
	})
}

// M18-S1 (PLAN:547): one byte over. A noscan object one byte larger than the
// space left links a fresh chunk, and the objects on both chunks stay valid.
func TestLogosArenaOneByteOverflowLinksChunk(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		const capacity = runtime.LogosArenaCapacity
		a := runtime.LogosArenaAlloc(byte(0), capacity-8)
		fillBytes(a, capacity-8, 0x11)
		b := runtime.LogosArenaAlloc(byte(0), 9)
		fillBytes(b, 9, 0x22)
		st := runtime.LogosArenaStat()
		if st.Chunks != 2 || st.NoscanUsed != 9 {
			t.Errorf("after the overflow: %+v, want two chunks and 9 bytes in the newest", st)
		}
		if !runtime.LogosArenaInChunk(b) {
			t.Error("the overflowing object was not served from a chunk")
		}
		if !checkBytes(a, capacity-8, 0x11) || !checkBytes(b, 9, 0x22) {
			t.Error("an object was damaged by the chunk link")
		}
		runtime.LogosArenaRestore(m)
		if st := runtime.LogosArenaStat(); st.Chunks != 1 || st.NoscanUsed != 0 || st.ScanUsed != 0 {
			t.Errorf("after restore: %+v, want the first chunk empty and the overflow chunk returned", st)
		}
	})
}

// M18-S1 (PLAN:547): the same overflow in the scan region.
func TestLogosArenaScanOverflowLinksChunk(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		x := new(int)
		first := uintptr(runtime.LogosArenaCapacity) - runtime.LogosArenaScanHeaderBytes
		a := runtime.LogosArenaAllocScan((*int)(nil), first)
		fillWords(a, first, x)
		b := runtime.LogosArenaAllocScan((*int)(nil), 8)
		fillWords(b, 8, x)
		if st := runtime.LogosArenaStat(); st.Chunks != 2 {
			t.Errorf("chunks = %d after a scan overflow, want 2", st.Chunks)
		}
		words := unsafe.Slice((**int)(a), first/8)
		for i, w := range words {
			if w != x {
				t.Fatalf("word %d of the first scan object was damaged", i)
			}
		}
		if *(**int)(b) != x {
			t.Error("the second scan object was damaged")
		}
	})
}

// M18-S1 (PLAN:547): when no chunk can be had, an allocation falls back to
// mallocgc and still succeeds with zeroed, writable memory, and earlier
// arena objects stay valid.
func TestLogosArenaEmptyPoolFallsBackToMallocgc(t *testing.T) {
	total, _, pooled := runtime.LogosArenaCounts()
	old := runtime.LogosArenaSetLimit(int64(total) + 1)
	defer runtime.LogosArenaSetLimit(old)
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		const size = 60000
		var arena []unsafe.Pointer
		var heap unsafe.Pointer
		for i := 0; i <= pooled+2; i++ {
			p := runtime.LogosArenaAlloc(byte(0), size)
			if !runtime.LogosArenaInChunk(p) {
				heap = p
				break
			}
			fillBytes(p, size, byte(i+1))
			arena = append(arena, p)
		}
		if len(arena) == 0 {
			t.Fatal("the first allocation fell back: no arena object exists to check against the fallback")
		}
		if heap == nil {
			t.Fatalf("no fallback after %d chunk-sized allocations with the limit one above the current chunk count", len(arena))
		}
		if !checkBytes(heap, size, 0) {
			t.Error("fallback memory is not zero")
		}
		fillBytes(heap, size, 0xEE)
		for i, p := range arena {
			if !checkBytes(p, size, byte(i+1)) {
				t.Errorf("arena object %d was damaged by the fallback", i)
			}
		}
	})
}

// M18-S1: an object that cannot fit any chunk goes to mallocgc without
// leasing a chunk, and zero-size requests return the zero base.
func TestLogosArenaOversizeAndZeroSize(t *testing.T) {
	inFreshGoroutine(t, func() {
		const over = runtime.LogosArenaCapacity + 1
		p := runtime.LogosArenaAlloc(byte(0), over)
		if runtime.LogosArenaInChunk(p) || !checkBytes(p, over, 0) {
			t.Error("oversize noscan request: want zeroed heap memory")
		}
		scanOver := uintptr(runtime.LogosArenaChunkBytes)
		q := runtime.LogosArenaAllocScan((*int)(nil), scanOver)
		if runtime.LogosArenaInChunk(q) || !checkBytes(q, scanOver, 0) {
			t.Error("oversize scan request: want zeroed heap memory")
		}
		if st := runtime.LogosArenaStat(); st.Chunks != 0 {
			t.Errorf("chunks = %d after oversize requests, want none leased", st.Chunks)
		}
		if runtime.LogosArenaAlloc(byte(0), 0) == nil || runtime.LogosArenaAllocScan((*int)(nil), 0) == nil {
			t.Error("zero-size request returned nil")
		}
	})
}

// M18-S1: noscan memory is zero when handed out, even over bytes that a
// popped frame left dirty, and the watermark is LIFO.
func TestLogosArenaNoscanReuseIsZeroed(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		p := runtime.LogosArenaAlloc(byte(0), 256)
		fillBytes(p, 256, 0xFF)
		runtime.LogosArenaRestore(m)
		m = runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		q := runtime.LogosArenaAlloc(byte(0), 256)
		if q != p {
			t.Errorf("restored watermark gave %p, want the freed address %p", q, p)
		}
		if !checkBytes(q, 256, 0) {
			t.Error("reused noscan memory is not zero")
		}
	})
}

// M18-S1 (AR2, AR3): scan objects sit densely, each behind a header holding
// its type word and size, and a restore zeroes the popped scan memory.
func TestLogosArenaScanLayoutAndZeroing(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		x := new(int)
		sizes := []uintptr{8, 24, 40, 128}
		objs := make([]unsafe.Pointer, len(sizes))
		for i, size := range sizes {
			objs[i] = runtime.LogosArenaAllocScan((*int)(nil), size)
			fillWords(objs[i], size, x)
		}
		typ := runtime.LogosArenaType((*int)(nil))
		var strides uintptr
		for i, p := range objs {
			gotTyp, gotSize := runtime.LogosArenaScanHeader(p)
			if gotTyp != typ || gotSize != sizes[i] {
				t.Errorf("object %d header = (%p, %d), want (%p, %d)", i, gotTyp, gotSize, typ, sizes[i])
			}
			strides += arenaStride(sizes[i])
			if i > 0 {
				if gap := uintptr(objs[i-1]) - uintptr(p); gap != arenaStride(sizes[i]) {
					t.Errorf("objects %d and %d are %d bytes apart, want %d", i-1, i, gap, arenaStride(sizes[i]))
				}
			}
		}
		if st := runtime.LogosArenaStat(); st.ScanUsed != strides {
			t.Errorf("ScanUsed = %d, want %d", st.ScanUsed, strides)
		}
		lo := uintptr(objs[len(objs)-1]) - runtime.LogosArenaScanHeaderBytes
		hi := uintptr(objs[0]) + (sizes[0]+arenaScanUnit-1)&^(arenaScanUnit-1)
		runtime.LogosArenaRestore(m)
		if st := runtime.LogosArenaStat(); st.ScanUsed != 0 {
			t.Errorf("ScanUsed = %d after restore, want 0", st.ScanUsed)
		}
		if !checkBytes(unsafe.Pointer(lo), hi-lo, 0) {
			t.Error("popped scan memory is not zero after restore")
		}
	})
}

// M18-S1 (AR2): restoring a mark that precedes a chunk link pops the linked
// chunks and zeroes their scan memory before they can be reused.
func TestLogosArenaRestorePopsLinkedChunks(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		x := new(int)
		var objs []unsafe.Pointer
		const size = 30000
		for range 4 {
			p := runtime.LogosArenaAllocScan((*int)(nil), size)
			fillWords(p, size, x)
			objs = append(objs, p)
		}
		if st := runtime.LogosArenaStat(); st.Chunks < 2 {
			t.Fatalf("chunks = %d, want the scan objects to span several chunks", st.Chunks)
		}
		runtime.LogosArenaRestore(m)
		if st := runtime.LogosArenaStat(); st.Chunks != 1 || st.ScanUsed != 0 || st.NoscanUsed != 0 {
			t.Errorf("after restore: %+v, want one empty chunk", st)
		}
		for i, p := range objs {
			if !checkBytes(p, size, 0) {
				t.Errorf("scan object %d memory is not zero after restore", i)
			}
		}
	})
}

// M18-S1 (#4): chunks cached in the Ps are freed at the end of a GC cycle, so
// a burst of leases does not stay resident, and chunk bytes show up in
// StackInuse while leased.
func TestLogosArenaGCFreesCachedChunks(t *testing.T) {
	const n = 512
	goroutines := runtime.NumGoroutine()
	runtime.GC()
	_, leasedBase, _ := runtime.LogosArenaCounts()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	hold := make(chan struct{})
	var held, finished sync.WaitGroup
	held.Add(n)
	finished.Add(n)
	for range n {
		go func() {
			defer finished.Done()
			m := runtime.LogosArenaSave()
			runtime.LogosArenaAlloc(byte(0), 100)
			held.Done()
			<-hold
			runtime.LogosArenaRestore(m)
		}()
	}
	held.Wait()
	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	if got, want := during.StackInuse-before.StackInuse, uint64(n)*runtime.LogosArenaChunkBytes; got < want {
		t.Errorf("StackInuse grew by %d with %d chunks leased, want at least %d", got, n, want)
	}
	if _, leased, _ := runtime.LogosArenaCounts(); leased < leasedBase+n {
		t.Errorf("leased = %d with %d goroutines holding arena memory, want at least %d", leased, n, leasedBase+n)
	}
	close(hold)
	finished.Wait()
	waitGoroutines(t, goroutines)
	waitLeased(t, leasedBase)
	runtime.GC()
	total, leased, pooled := runtime.LogosArenaCounts()
	if pooled != 0 || total != leased {
		t.Errorf("after GC: total=%d leased=%d pooled=%d, want every cached chunk freed", total, leased, pooled)
	}
}

// M18-S1: shrinking GOMAXPROCS destroys Ps, and the chunks cached in a
// destroyed P are freed with it; every chunk stays accounted for.
func TestLogosArenaProcResizeFreesCachedChunks(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	if procs < 2 {
		t.Skip("needs at least two Ps")
	}
	goroutines := runtime.NumGoroutine()
	const n = 256
	hold := make(chan struct{})
	var held, finished sync.WaitGroup
	held.Add(n)
	finished.Add(n)
	for range n {
		go func() {
			defer finished.Done()
			m := runtime.LogosArenaSave()
			runtime.LogosArenaAlloc(byte(0), 100)
			held.Done()
			<-hold
			runtime.LogosArenaRestore(m)
		}()
	}
	held.Wait()
	close(hold)
	finished.Wait()
	waitGoroutines(t, goroutines)
	before := runtime.LogosArenaPoolCounts()
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	total, leased, pooled := runtime.LogosArenaCounts()
	if total != leased+pooled {
		t.Errorf("after shrinking to one P: total=%d leased=%d pooled=%d, want total = leased + pooled", total, leased, pooled)
	}
	if pooled > before[0] {
		t.Errorf("pooled = %d after shrinking to one P, which held %d: a chunk outlived its destroyed P", pooled, before[0])
	}
}

// M18-S1 (AR2): a scan object that lands on bytes an earlier frame dirtied
// through the noscan region still reads as zero, so no stale word reaches
// the stack scan as a pointer.
func TestLogosArenaScanOverNoscanDirt(t *testing.T) {
	inFreshGoroutine(t, func() {
		m := runtime.LogosArenaSave()
		defer runtime.LogosArenaRestore(m)
		const dirty = 60000
		p := runtime.LogosArenaAlloc(byte(0), dirty)
		fillBytes(p, dirty, 0xFF)
		runtime.LogosArenaRestore(m)
		const size = 30000
		q := runtime.LogosArenaAllocScan((*int)(nil), size)
		if uintptr(q)+size <= uintptr(p) || uintptr(q) >= uintptr(p)+dirty {
			t.Fatalf("scan object %p does not overlay the dirtied bytes at %p: the test no longer probes the overlay", q, p)
		}
		if !checkBytes(q, size, 0) {
			t.Error("scan object over dirtied noscan bytes is not zero")
		}
	})
}

// M18-S2 (SPEC Sec 8.3): the consent token is one-shot. Taking it returns
// what the caller set and clears it, so a later take sees no consent.
func TestLogosArenaRetTokenIsOneShot(t *testing.T) {
	inFreshGoroutine(t, func() {
		if runtime.LogosArenaRetTake() {
			t.Error("fresh goroutine holds a consent token")
		}
		runtime.LogosArenaRetSet(true)
		if !runtime.LogosArenaRetTake() {
			t.Error("take after set(true) = false")
		}
		if runtime.LogosArenaRetTake() {
			t.Error("second take after one set = true: the token was not cleared")
		}
		runtime.LogosArenaRetSet(true)
		runtime.LogosArenaRetSet(false)
		if runtime.LogosArenaRetTake() {
			t.Error("take after set(true) then set(false) = true")
		}
	})
}

// M18-S2: the flat word pair from LogosArenaMarkSave round-trips through
// LogosArenaMarkRestore: allocating after the save and restoring returns
// the watermark to exactly the saved words, for both regions.
func TestLogosArenaMarkWordPairRoundTrip(t *testing.T) {
	inFreshGoroutine(t, func() {
		chunk, noscan, scan := runtime.LogosArenaMarkSave()
		if chunk != 0 || noscan != 0 || scan != 0 {
			t.Errorf("empty arena MarkSave = (%d, %d, %d), want zero triple", chunk, noscan, scan)
		}
		if p := runtime.LogosArenaAlloc(byte(0), 64); p == nil {
			t.Fatal("LogosArenaAlloc returned nil")
		}
		if st := runtime.LogosArenaStat(); st.NoscanUsed == 0 {
			t.Error("NoscanUsed = 0 after allocation, want > 0")
		}
		runtime.LogosArenaMarkRestore(chunk, noscan, scan)
		if st := runtime.LogosArenaStat(); st.NoscanUsed != 0 || st.ScanUsed != 0 {
			t.Errorf("after restoring the zero triple: NoscanUsed=%d ScanUsed=%d, want 0, 0", st.NoscanUsed, st.ScanUsed)
		}
		if p := runtime.LogosArenaAlloc(byte(0), 32); p == nil {
			t.Fatal("LogosArenaAlloc returned nil")
		}
		base := runtime.LogosArenaStat()
		chunk, noscan, scan = runtime.LogosArenaMarkSave()
		if chunk == 0 {
			t.Error("MarkSave after allocation returned a zero chunk word")
		}
		if p := runtime.LogosArenaAlloc(byte(0), 64); p == nil {
			t.Fatal("LogosArenaAlloc returned nil")
		}
		if p := runtime.LogosArenaAllocScan((*int)(nil), 64); p == nil {
			t.Fatal("LogosArenaAllocScan returned nil")
		}
		runtime.LogosArenaMarkRestore(chunk, noscan, scan)
		if st := runtime.LogosArenaStat(); st != base {
			t.Errorf("after word-pair restore: stat = %+v, want %+v", st, base)
		}
	})
}

// M18-S2: an exiting goroutine clears an unconsumed token, so a goroutine
// that reuses its g never starts with someone else's consent.
func TestLogosArenaExitClearsToken(t *testing.T) {
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			runtime.LogosArenaRetSet(true)
			runtime.Goexit()
		})
	}
	wg.Wait()
	var stale atomic.Int32
	for range 64 {
		wg.Go(func() {
			if runtime.LogosArenaRetTake() {
				stale.Add(1)
			}
		})
	}
	wg.Wait()
	if n := stale.Load(); n != 0 {
		t.Errorf("%d goroutines started with a stale consent token", n)
	}
}

// M18-S2/S3: g.logosArena sits immediately after stackguard1, at offset
// 4*PtrSize (stack is two words, then stackguard0, then stackguard1). The
// compiler's inline bump (S8) hard-codes this offset; this test is its pin.
func TestLogosArenaFieldOffset(t *testing.T) {
	if want := 4 * unsafe.Sizeof(uintptr(0)); runtime.LogosArenaFieldOffset != want {
		t.Errorf("unsafe.Offsetof(g{}.logosArena) = %d, want %d (4*PtrSize)", runtime.LogosArenaFieldOffset, want)
	}
}

//go:noinline
func arenaEligibleMake(n int) uintptr {
	b := make([]byte, n)
	if len(b) > 0 {
		b[0] = 42
	}
	during := runtime.LogosArenaStat().NoscanUsed
	if len(b) > 0 && b[0] != 42 {
		panic("memory corrupted")
	}
	return during
}

// M18-S3: an arena-eligible make of dynamic size allocates from the arena
// bump allocator, advancing the watermark, and restores the watermark when
// the frame exits.
func TestLogosArenaDynamicMakeWatermark(t *testing.T) {
	if !goexperiment.LogosArena {
		t.Skip("skipping test: GOEXPERIMENT=logosarena not enabled")
	}
	inFreshGoroutine(t, func() {
		before := runtime.LogosArenaStat().NoscanUsed

		// First call: dynamic make of 1024 bytes.
		during1 := arenaEligibleMake(1024)
		after1 := runtime.LogosArenaStat().NoscanUsed

		if during1 <= before {
			t.Errorf("call 1: watermark during make = %d, want > %d (watermark did not advance)", during1, before)
		}
		if after1 != before {
			t.Errorf("call 1: watermark after frame exit = %d, want %d (watermark did not fall back)", after1, before)
		}

		// Second call: dynamic make of 2048 bytes.
		during2 := arenaEligibleMake(2048)
		after2 := runtime.LogosArenaStat().NoscanUsed

		if during2 <= before {
			t.Errorf("call 2: watermark during make = %d, want > %d (watermark did not advance)", during2, before)
		}
		if after2 != before {
			t.Errorf("call 2: watermark after frame exit = %d, want %d (watermark did not fall back)", after2, before)
		}
	})
}

// The make seam attacks below drive the public make([]T, len, cap) syntax
// through sites the compiler places in the arena (non-escaping, dynamic
// capacity), and compare every panic with the heap path's. They never touch
// a slice an overflowing make returned: an unexpected success ends the
// subtest at once, so a wrapped size cannot turn into an out-of-bounds write.

const (
	arenaMakeLenPanic = "runtime error: makeslice: len out of range"
	arenaMakeCapPanic = "runtime error: makeslice: cap out of range"

	// arenaMakeAllocSlack bounds the heap bytes a panicking make may
	// allocate: the panic value and test bookkeeping, never the slice.
	arenaMakeAllocSlack = 1 << 20
)

type arenaMakeCase struct {
	len, cap int64
	want     string
}

type arenaElem24 [3]int64

//go:noinline
func arenaSeamInt64(n, c int) (length, capacity int, used uintptr) {
	b := make([]int64, n, c)
	return len(b), cap(b), runtime.LogosArenaStat().NoscanUsed
}

//go:noinline
func arenaSeamElem24(n, c int) (int, int) {
	b := make([]arenaElem24, n, c)
	return len(b), cap(b)
}

//go:noinline
func arenaSeamByte(n, c int) (int, int) {
	b := make([]byte, n, c)
	return len(b), cap(b)
}

//go:noinline
func arenaSeamUint32Wide(n, c uint64) (int, int) {
	b := make([]uint32, n, c)
	return len(b), cap(b)
}

//go:noinline
func arenaSeamFill(n, c int) bool {
	b := make([]int64, n, c)
	if len(b) != n || cap(b) != c {
		return false
	}
	b = b[:cap(b)]
	for i := range b {
		if b[i] != 0 {
			return false
		}
		b[i] = int64(i) + 1
	}
	for i := range b {
		if b[i] != int64(i)+1 {
			return false
		}
	}
	return true
}

//go:noinline
func heapSeamInt64(n, c int) { arenaSink = make([]int64, n, c) }

//go:noinline
func heapSeamElem24(n, c int) { arenaSink = make([]arenaElem24, n, c) }

//go:noinline
func heapSeamByte(n, c int) { arenaSink = make([]byte, n, c) }

//go:noinline
func heapSeamUint32Wide(n, c uint64) { arenaSink = make([]uint32, n, c) }

// arenaMakeMustPanic runs fn, which holds one make, and requires the panic
// message want and no enormous allocation. It stops the subtest when fn
// returns normally, before anything could use the slice.
func arenaMakeMustPanic(t *testing.T, what, want string, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	msg, panicked := recoverMsg(fn)
	runtime.ReadMemStats(&after)
	if !panicked {
		t.Fatalf("%s: make returned without panic, want %q", what, want)
	}
	if msg != want {
		t.Errorf("%s: panic %q, want %q", what, msg, want)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > arenaMakeAllocSlack {
		t.Errorf("%s: panicking make allocated %d heap bytes, want at most %d", what, grew, arenaMakeAllocSlack)
	}
}

// arenaMakeOverflowCases lists len/cap pairs for elements of size bytes
// (size >= 2) whose make must panic. The expected message follows makeslice:
// the len message when len alone is out of range, else the cap message.
// Every capacity comes from the runtime's own bounds, not from a literal.
// Each pair either fails an unchecked lowering's own len/cap checks or wraps
// the byte size to zero or a tiny value, so an unchecked lowering never
// reaches the allocator with a huge size here.
func arenaMakeOverflowCases(size uintptr) []arenaMakeCase {
	// wrap is the smallest capacity whose byte size overflows uintptr.
	wrap := int64(^uintptr(0)/size + 1)
	minInt := int64(math.MinInt)
	return []arenaMakeCase{
		{0, wrap, arenaMakeCapPanic},
		{0, wrap + 1, arenaMakeCapPanic},
		{1, wrap, arenaMakeCapPanic},
		{1, wrap + 1, arenaMakeCapPanic},
		{wrap, wrap, arenaMakeLenPanic},
		{wrap + 1, wrap + 1, arenaMakeLenPanic},
		{wrap + 1, wrap + 2, arenaMakeLenPanic},
		{wrap, 1, arenaMakeLenPanic},
		{0, minInt, arenaMakeCapPanic},
		{minInt, 0, arenaMakeLenPanic},
		{minInt, minInt, arenaMakeLenPanic},
		{-1, 1, arenaMakeLenPanic},
		{-1, -1, arenaMakeLenPanic},
		{1, -1, arenaMakeCapPanic},
		{2, 1, arenaMakeCapPanic},
	}
}

func arenaMakeRun(t *testing.T, size uintptr, arena, heap func(n, c int)) {
	for _, tc := range arenaMakeOverflowCases(size) {
		t.Run(fmt.Sprintf("len=%d,cap=%d", tc.len, tc.cap), func(t *testing.T) {
			n, c := int(tc.len), int(tc.cap)
			arenaMakeMustPanic(t, "heap control", tc.want, func() { heap(n, c) })
			arenaMakeMustPanic(t, "arena make", tc.want, func() { arena(n, c) })
		})
	}
}

// TestLogosArenaMakeOverflow attacks the dynamic arena make with capacities
// whose byte size wraps uintptr to zero or a tiny positive size. makeslice
// panics on all of them, with the len message when len alone is out of
// range; the arena path must panic identically instead of returning a slice
// whose cap exceeds its storage.
func TestLogosArenaMakeOverflow(t *testing.T) {
	if !goexperiment.LogosArena {
		t.Skip("skipping test: GOEXPERIMENT=logosarena not enabled")
	}
	t.Run("int64", func(t *testing.T) {
		arenaMakeRun(t, 8, func(n, c int) { arenaSeamInt64(n, c) }, heapSeamInt64)
	})
	t.Run("elem24", func(t *testing.T) {
		arenaMakeRun(t, 24, func(n, c int) { arenaSeamElem24(n, c) }, heapSeamElem24)
	})
}

// TestLogosArenaMakeOverflowWide covers unsigned 64-bit len and cap, whose
// conversion to int must keep the makeslice64 verdicts. There is no fixed
// expectation: the heap path is the reference for every pair.
func TestLogosArenaMakeOverflowWide(t *testing.T) {
	if !goexperiment.LogosArena {
		t.Skip("skipping test: GOEXPERIMENT=logosarena not enabled")
	}
	top := uint64(1) << 63
	wrap := uint64(1) << 62
	for _, tc := range [][2]uint64{
		{0, wrap}, {wrap, wrap}, {0, top}, {top, top}, {top, 0}, {1, top + 1}, {wrap + 1, wrap + 1},
	} {
		t.Run(fmt.Sprintf("len=%d,cap=%d", tc[0], tc[1]), func(t *testing.T) {
			n, c := tc[0], tc[1]
			msg, panicked := recoverMsg(func() { heapSeamUint32Wide(n, c) })
			if !panicked {
				t.Fatal("heap control: make returned without panic")
			}
			arenaMakeMustPanic(t, "arena make", msg, func() { arenaSeamUint32Wide(n, c) })
		})
	}
}

// TestLogosArenaMakeOverflowMaxAlloc covers sizes past maxAlloc that wrap
// uintptr to a huge value or do not wrap at all. An unchecked lowering hands
// those to the allocator, which aborts the process, so the test first
// proves the lowering checks sizes, with a capacity that wraps to zero, and
// fails before any such size is attempted when it does not.
func TestLogosArenaMakeOverflowMaxAlloc(t *testing.T) {
	if !goexperiment.LogosArena {
		t.Skip("skipping test: GOEXPERIMENT=logosarena not enabled")
	}
	wrapsToZero := int(^uintptr(0)/8 + 1)
	if _, panicked := recoverMsg(func() { arenaSeamInt64(0, wrapsToZero) }); !panicked {
		t.Fatal("make([]int64, 0, cap) with a wrapping byte size returned without panic; refusing to attempt sizes past maxAlloc")
	}
	// The smallest int64 capacity past maxAlloc. The constant fits int on
	// 32-bit and 64-bit; on 32-bit its byte size also wraps, which panics too.
	const pastInt64 = int(runtime.LogosArenaMaxAlloc/8 + 1)
	for _, tc := range []struct {
		name string
		n, c int
		want string
	}{
		{"cap", 0, pastInt64, arenaMakeCapPanic},
		{"len", pastInt64, pastInt64, arenaMakeLenPanic},
		{"maxint-cap", 0, math.MaxInt, arenaMakeCapPanic},
		{"maxint-len", math.MaxInt, math.MaxInt, arenaMakeLenPanic},
	} {
		t.Run("int64/"+tc.name, func(t *testing.T) {
			arenaMakeMustPanic(t, "heap control", tc.want, func() { heapSeamInt64(tc.n, tc.c) })
			arenaMakeMustPanic(t, "arena make", tc.want, func() { arenaSeamInt64(tc.n, tc.c) })
		})
	}
	if bits.UintSize == 64 {
		// maxAlloc fits int only on 64-bit; one byte past it needs no wrap.
		maxAlloc := runtime.LogosArenaMaxAlloc
		pastByte := int(maxAlloc) + 1
		t.Run("byte/cap", func(t *testing.T) {
			arenaMakeMustPanic(t, "heap control", arenaMakeCapPanic, func() { heapSeamByte(0, pastByte) })
			arenaMakeMustPanic(t, "arena make", arenaMakeCapPanic, func() { arenaSeamByte(0, pastByte) })
		})
		t.Run("byte/len", func(t *testing.T) {
			arenaMakeMustPanic(t, "heap control", arenaMakeLenPanic, func() { heapSeamByte(pastByte, pastByte) })
			arenaMakeMustPanic(t, "arena make", arenaMakeLenPanic, func() { arenaSeamByte(pastByte, pastByte) })
		})
	}
}

// TestLogosArenaMakeSeamPlacement pins that the probe site really is placed
// in the arena on amd64, the only architecture that lowers the make, so the
// overflow attacks above run against the arena path, and that valid makes
// still return zeroed, writable, correctly sized storage.
func TestLogosArenaMakeSeamPlacement(t *testing.T) {
	if !goexperiment.LogosArena {
		t.Skip("skipping test: GOEXPERIMENT=logosarena not enabled")
	}
	if runtime.GOARCH != "amd64" {
		t.Skip("skipping test: arena make lowering is amd64-only")
	}
	inFreshGoroutine(t, func() {
		before := runtime.LogosArenaStat().NoscanUsed
		n, c, during := arenaSeamInt64(64, 96)
		after := runtime.LogosArenaStat().NoscanUsed
		if n != 64 || c != 96 {
			t.Errorf("make([]int64, 64, 96) has len %d cap %d", n, c)
		}
		if want := before + 96*8; during < want {
			t.Errorf("watermark during make = %d, want at least %d (probe site is not arena-placed)", during, want)
		}
		if after != before {
			t.Errorf("watermark after frame exit = %d, want %d", after, before)
		}
		for _, tc := range [][2]int{{0, 0}, {1, 1}, {3, 5}, {64, 64}, {513, 1024}, {int(runtime.LogosArenaChunkBytes/8) + 1, int(runtime.LogosArenaChunkBytes/8) + 1}} {
			if !arenaSeamFill(tc[0], tc[1]) {
				t.Errorf("make([]int64, %d, %d) is not zeroed, writable, and cap-sized", tc[0], tc[1])
			}
		}
	})
}

// TestLogosArenaMakeSliceEntry attacks the private runtime entry directly
// with the same pairs, plus int64 values that do not fit int on 32-bit.
func TestLogosArenaMakeSliceEntry(t *testing.T) {
	if !goexperiment.LogosArena {
		t.Skip("skipping test: GOEXPERIMENT=logosarena not enabled")
	}
	for _, size := range []uintptr{8, 24} {
		elem := any(int64(0))
		if size == 24 {
			elem = arenaElem24{}
		}
		for _, tc := range arenaMakeOverflowCases(size) {
			t.Run(fmt.Sprintf("size=%d/len=%d,cap=%d", size, tc.len, tc.cap), func(t *testing.T) {
				arenaMakeMustPanic(t, "entry", tc.want, func() { runtime.LogosArenaMakeSlice(elem, tc.len, tc.cap) })
			})
		}
	}
	if bits.UintSize == 32 {
		wide := int64(1) << 32
		t.Run("truncated-len", func(t *testing.T) {
			arenaMakeMustPanic(t, "entry", arenaMakeLenPanic, func() { runtime.LogosArenaMakeSlice(int64(0), wide, wide) })
		})
		t.Run("truncated-cap", func(t *testing.T) {
			arenaMakeMustPanic(t, "entry", arenaMakeCapPanic, func() { runtime.LogosArenaMakeSlice(int64(0), 0, wide) })
		})
	}
}
