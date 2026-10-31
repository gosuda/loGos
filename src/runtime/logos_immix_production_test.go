// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"fmt"
	"internal/goarch"
	"internal/goexperiment"
	"internal/runtime/gc"
	"internal/testenv"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"unsafe"
)

// Production serving attacks for the immix collector (M19-S5, M19-S8).
//
// logos_immix_test.go checks the block and line layer through a stand-alone
// allocator. These tests check what the collector does: the spans that
// mallocgc hands out, the addresses it serves from them, the plane state it
// keeps, the counters it reports, and the objects it moves. Every observation
// is of live allocator state. The inspectors in export_logos_immix_test.go copy
// span and plane state and write nothing, no test forces the cached mode, and
// no test seeds a flag, a bitmap, or a line.
//
// They need a test binary built with GOEXPERIMENT=logosgcmoving and linked
// with -ldflags=-X=runtime.logosGCMode=immix, the way the linker selects the
// mode. Each scenario runs in its own copy of the test binary, the way
// TestLogosRelocationCounters does, with GOMAXPROCS(1) and GC percent -1, so
// that no other goroutine allocates in the size class under test and no
// background cycle starts. A binary without the mode skips them, unless
// LOGOS_IMMIX_REQUIRE is set, which turns the skip into a failure.
//
// Eligibility is stated here from the specification, not read from the
// production predicate: spans of size classes 3 and above are 32 KiB blocks,
// except that under Green Tea the inline-mark classes (512 bytes and less)
// stay in 8 KiB spans, because the 128-byte mark tail does not fit a block.
// Tiny, large, and Green Tea inline-mark classes are checked to stay out, and
// every eligible class is checked to be served from a block.
const (
	immixProdChildEnv = "TEST_LOGOS_IMMIX_PRODUCTION_CHILD"
	immixProdRequire  = "LOGOS_IMMIX_REQUIRE"

	immixProdPage   = gc.PageSize
	immixProdBlock  = runtime.LogosImmixBlockBytes
	immixProdLine   = runtime.LogosImmixLineBytes
	immixProdLines  = runtime.LogosImmixLines
	immixProdSmall  = gc.MinSizeForMallocHeader
	immixProdHeader = gc.MallocHeaderSize
	immixProdPtr    = goarch.PtrSize
	immixProdBump   = 62
)

var immixProdScenarios = []struct {
	name string
	run  func(*testing.T)
}{
	{"geometry", immixProdGeometry},
	{"bump", immixProdBumpScenario},
	{"holes", immixProdHoles},
	{"counters", immixProdCounters},
	{"exclusions", immixProdExclusions},
	{"dirty", immixProdDirty},
}

var (
	immixProdKeep    []unsafe.Pointer
	immixProdSkipped []unsafe.Pointer
	immixProdHead    unsafe.Pointer
	immixProdGarbage []byte
	immixProdGroups  []*immixProdGroup
	immixProdBigs    [][]byte
	immixProdBigScan unsafe.Pointer
	immixProdValue   = new(byte)
	immixProdDirtyA  []unsafe.Pointer
	immixProdDirtyB  []unsafe.Pointer
	immixProdDirtyH  []unsafe.Pointer
)

// immixProdRequireMode skips, or fails under LOGOS_IMMIX_REQUIRE, unless the
// binary is the immix build.
func immixProdRequireMode(t *testing.T) {
	t.Helper()
	if runtime.LogosImmixMovingExperiment() && runtime.LogosHeapObjectsCanMove() && runtime.LogosImmixOn() {
		return
	}
	if os.Getenv(immixProdRequire) != "" {
		t.Fatal("not an immix test binary: build it with GOEXPERIMENT=logosgcmoving and -ldflags=-X=runtime.logosGCMode=immix")
	}
	t.Skip("needs GOEXPERIMENT=logosgcmoving and -ldflags=-X=runtime.logosGCMode=immix")
}

// TestLogosImmixProduction runs every scenario in its own copy of the test
// binary. The child selects its scenario from the environment.
func TestLogosImmixProduction(t *testing.T) {
	if name := os.Getenv(immixProdChildEnv); name != "" {
		immixProdChild(t, name)
		return
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no subprocesses")
	}
	immixProdRequireMode(t)
	testenv.MustHaveExec(t)
	for _, sc := range immixProdScenarios {
		t.Run(sc.name, func(t *testing.T) {
			cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestLogosImmixProduction$", "-test.v")
			cmd.Env = append(os.Environ(), immixProdChildEnv+"="+sc.name)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "--- PASS: TestLogosImmixProduction (") || strings.Contains(string(out), "--- SKIP") {
				t.Fatalf("%v\n%s", err, out)
			}
			t.Logf("%s", strings.TrimSpace(string(out)))
		})
	}
}

func immixProdChild(t *testing.T, name string) {
	immixProdRequireMode(t)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	for _, sc := range immixProdScenarios {
		if sc.name == name {
			sc.run(t)
			return
		}
	}
	t.Fatalf("unknown scenario %q", name)
}

// immixProdClass is one size class in one of its two span variants.
type immixProdClass struct {
	index int8
	size  uintptr
	scan  bool
}

func (c immixProdClass) String() string {
	kind := "noscan"
	if c.scan {
		kind = "scan"
	}
	return fmt.Sprintf("class %d (%d bytes, %s)", c.index, c.size, kind)
}

// user is the size of the type that is allocated in the class: a scan object
// above 512 bytes carries an 8-byte malloc header, and the largest class is
// reached by a pointer-free object of 32760 bytes, because anything larger is
// a large object.
func (c immixProdClass) user() uintptr {
	switch {
	case c.scan && c.size > immixProdSmall:
		return c.size - immixProdHeader
	case !c.scan && c.size == 32768:
		return 32760
	}
	return c.size
}

// hdr is the distance from the first byte of a slot to the object.
func (c immixProdClass) hdr() uintptr {
	if c.scan && c.size > immixProdSmall {
		return immixProdHeader
	}
	return 0
}

func (c immixProdClass) slot(p unsafe.Pointer) uintptr { return uintptr(p) - c.hdr() }

func (c immixProdClass) typ() reflect.Type {
	if c.scan {
		return reflect.ArrayOf(int(c.user()/immixProdPtr), reflect.TypeFor[*byte]())
	}
	return reflect.ArrayOf(int(c.user()), reflect.TypeFor[byte]())
}

// wantActive states which classes the immix collector serves from blocks.
func (c immixProdClass) wantActive() bool {
	if c.index < 3 {
		return false
	}
	return !(goexperiment.GreenTeaGC && c.size <= immixProdSmall)
}

// wantElems is the number of slots of a block: the heap bits of a small scan
// object take the last 512 bytes of the span.
func (c immixProdClass) wantElems() int {
	if c.scan && c.size <= immixProdSmall {
		return int((immixProdBlock - immixProdBlock/immixProdPtr/8) / c.size)
	}
	return int(immixProdBlock / c.size)
}

// immixProdClasses lists every small size class from the first non-tiny class
// in both span variants, from the runtime's own class table.
func immixProdClasses() []immixProdClass {
	var out []immixProdClass
	for i := 3; i < gc.NumSizeClasses; i++ {
		size := uintptr(gc.SizeClassToSize[i])
		out = append(out, immixProdClass{int8(i), size, false}, immixProdClass{int8(i), size, true})
	}
	return out
}

func immixProdSpan(t *testing.T, p unsafe.Pointer) runtime.LogosImmixSpan {
	t.Helper()
	st, ok := runtime.LogosImmixSpanOf(p)
	if !ok {
		t.Fatalf("%p is not in an in-use span", p)
	}
	return st
}

func immixProdFill(c immixProdClass, p unsafe.Pointer, id int) {
	if c.scan {
		w := unsafe.Slice((*unsafe.Pointer)(p), c.user()/immixProdPtr)
		w[len(w)-1] = p
		return
	}
	b := unsafe.Slice((*byte)(p), c.user())
	for i := range b {
		b[i] = byte(id) + byte(i)
	}
}

func immixProdIntact(c immixProdClass, p unsafe.Pointer, id int) bool {
	if c.scan {
		w := unsafe.Slice((*unsafe.Pointer)(p), c.user()/immixProdPtr)
		return w[len(w)-1] == p
	}
	b := unsafe.Slice((*byte)(p), c.user())
	for i := range b {
		if b[i] != byte(id)+byte(i) {
			return false
		}
	}
	return true
}

func immixProdZero(c immixProdClass, p unsafe.Pointer) bool {
	if c.scan {
		for _, w := range unsafe.Slice((*unsafe.Pointer)(p), c.user()/immixProdPtr) {
			if w != nil {
				return false
			}
		}
		return true
	}
	for _, b := range unsafe.Slice((*byte)(p), c.user()) {
		if b != 0 {
			return false
		}
	}
	return true
}

// immixProdFindFresh allocates until an object is the first slot of a span
// that holds nothing else, and returns it.
func immixProdFindFresh(t *testing.T, c immixProdClass, typ reflect.Type) unsafe.Pointer {
	t.Helper()
	limit := 2*c.wantElems() + 8
	skipped := make([]unsafe.Pointer, 0, limit)
	for range limit {
		p := reflect.New(typ).UnsafePointer()
		st := immixProdSpan(t, p)
		if st.Active && st.AllocCount == 1 && st.FreeIndex == 1 && c.slot(p) == st.Base {
			immixProdSkipped = skipped
			return p
		}
		skipped = append(skipped, p)
	}
	immixProdSkipped = skipped
	t.Fatalf("%v: no fresh span in %d allocations", c, limit)
	return nil
}

// immixProdGeometry checks that every eligible class is served from a
// 32 KiB-aligned block span that has block metadata and a recorded slot
// geometry, and that tiny, large, and Green Tea inline-mark classes are not.
func immixProdGeometry(t *testing.T) {
	classes := immixProdClasses()
	typs := make([]reflect.Type, len(classes))
	for i, c := range classes {
		typs[i] = c.typ()
		reflect.New(typs[i])
	}
	runtime.GC()
	active := 0
	for i, c := range classes {
		for range 3 {
			p := reflect.New(typs[i]).UnsafePointer()
			immixProdKeep = append(immixProdKeep, p)
			if immixProdCheckGeometry(t, c, p) {
				active++
			}
		}
	}
	if active == 0 {
		t.Fatal("no allocation was served by an immix span")
	}

	tiny := new([8]byte)
	word := new(*byte)
	immixProdKeep = append(immixProdKeep, unsafe.Pointer(tiny), unsafe.Pointer(word))
	for _, e := range []struct {
		name string
		p    unsafe.Pointer
		sc   int8
	}{{"tiny block", unsafe.Pointer(tiny), 2}, {"8-byte pointer", unsafe.Pointer(word), 1}} {
		st := immixProdSpan(t, e.p)
		if int8(st.SizeClass) != e.sc || st.Active || st.HasBlock {
			t.Errorf("%s: size class %d active=%v block=%v, want class %d outside the blocks", e.name, st.SizeClass, st.Active, st.HasBlock, e.sc)
		}
	}

	big := make([]byte, 40000)
	immixProdBigs = append(immixProdBigs, big)
	immixProdBigScan = reflect.New(reflect.ArrayOf(5000, reflect.TypeFor[*byte]())).UnsafePointer()
	for _, e := range []struct {
		name string
		p    unsafe.Pointer
	}{{"large noscan", unsafe.Pointer(&big[0])}, {"large scan", immixProdBigScan}} {
		st := immixProdSpan(t, e.p)
		if st.SizeClass != 0 || st.Active || st.HasBlock || st.BlockBase != 0 || st.NPages*immixProdPage < 40000 || uintptr(e.p) != st.Base {
			t.Errorf("%s: %+v, want a size-class-0 span without immix state that starts at the object", e.name, st)
		}
	}
}

// immixProdCheckGeometry reports whether p was served by an immix block.
func immixProdCheckGeometry(t *testing.T, c immixProdClass, p unsafe.Pointer) bool {
	t.Helper()
	st := immixProdSpan(t, p)
	if st.SizeClass != c.index || st.ElemSize != c.size || st.NoScan == c.scan {
		t.Fatalf("%v: object %p is in size class %d of %d bytes (noscan=%v)", c, p, st.SizeClass, st.ElemSize, st.NoScan)
	}
	if !c.wantActive() {
		if st.Active || st.HasBlock || st.BlockBase != 0 {
			t.Fatalf("%v: excluded class carries immix state: %+v", c, st)
		}
		return false
	}
	slot := c.slot(p)
	switch {
	case !st.Active || !st.HasBlock || !st.BlockRegistered:
		t.Fatalf("%v: eligible class is not served from a registered block: %+v", c, st)
	case st.NPages != runtime.LogosImmixPagesPerBlock || st.NPages*immixProdPage != immixProdBlock:
		t.Fatalf("%v: span has %d pages, want one block of %d bytes", c, st.NPages, immixProdBlock)
	case st.Base%immixProdBlock != 0 || st.BlockBase != st.Base:
		t.Fatalf("%v: span base %#x with block base %#x is not a 32 KiB boundary", c, st.Base, st.BlockBase)
	case int(st.NElems) != c.wantElems():
		t.Fatalf("%v: span has %d slots, want %d", c, st.NElems, c.wantElems())
	case slot < st.Base || (slot-st.Base)%st.ElemSize != 0 || slot+st.ElemSize > st.Base+uintptr(st.NElems)*st.ElemSize:
		t.Fatalf("%v: object %p (slot %#x) is not a slot of span %#x+%d*%d", c, p, slot, st.Base, st.NElems, st.ElemSize)
	case st.Limit != st.Base+uintptr(st.NElems)*st.ElemSize:
		t.Fatalf("%v: object area ends at %#x, want %#x", c, st.Limit, st.Base+uintptr(st.NElems)*st.ElemSize)
	}
	occ, _, _, _, ok := runtime.LogosImmixPlanes(p)
	if !ok {
		t.Fatalf("%v: no block metadata for the block of %p", c, p)
	}
	for j := (slot - st.Base) / immixProdLine; j <= (slot+st.ElemSize-1-st.Base)/immixProdLine; j++ {
		if occ[j] == 0 {
			t.Fatalf("%v: line %d of slot %#x is free in the occupancy plane of a served slot", c, j, slot)
		}
	}
	return true
}

// immixProdBumpScenario fills one fresh span of every eligible class and checks
// the strides, the cursor and limit, the occupancy lines, the heap-bits carve,
// and that the slots do not overlap.
func immixProdBumpScenario(t *testing.T) {
	carves := 0
	for _, c := range immixProdClasses() {
		if c.wantActive() && immixProdBumpClass(t, c) {
			carves++
		}
	}
	if !goexperiment.GreenTeaGC && carves == 0 {
		t.Fatal("no pointer-bearing small class exercised the heap-bits carve")
	}
}

func immixProdBumpClass(t *testing.T, c immixProdClass) (carve bool) {
	t.Helper()
	typ := c.typ()
	reflect.New(typ)
	runtime.GC()
	want := c.wantElems()
	objs := make([]unsafe.Pointer, 0, want+1)
	first := immixProdFindFresh(t, c, typ)
	st0 := immixProdSpan(t, first)
	base, elem, n := st0.Base, st0.ElemSize, int(st0.NElems)
	if n != want {
		t.Fatalf("%v: fresh span has %d slots, want %d", c, n, want)
	}
	limit := uint32(uintptr(n) * elem)
	if st0.CursorLimit != limit {
		t.Fatalf("%v: bump limit %d after refill, want %d", c, st0.CursorLimit, limit)
	}
	objs = append(objs, first)
	for k := 1; k < n; k++ {
		p := reflect.New(typ).UnsafePointer()
		objs = append(objs, p)
		st := immixProdSpan(t, p)
		switch {
		case c.slot(p) != base+uintptr(k)*elem:
			t.Fatalf("%v: allocation %d is at %#x, want slot %#x (stride %d from %#x)", c, k, c.slot(p), base+uintptr(k)*elem, elem, base)
		case st.Base != base || !st.Active:
			t.Fatalf("%v: allocation %d left the span %#x: %+v", c, k, base, st)
		case int(st.AllocCount) != k+1:
			t.Fatalf("%v: allocation %d: alloc count %d, want %d", c, k, st.AllocCount, k+1)
		case int(st.FreeIndex) > k+1:
			t.Fatalf("%v: allocation %d: freeindex %d is past a free slot", c, k, st.FreeIndex)
		case st.CursorLimit != limit:
			t.Fatalf("%v: allocation %d: bump limit %d, want %d", c, k, st.CursorLimit, limit)
		case k <= immixProdBump && (int(st.FreeIndex) != k+1 || uintptr(st.Cursor) != uintptr(k+1)*elem):
			t.Fatalf("%v: bump allocation %d: freeindex %d cursor %d, want %d and %d", c, k, st.FreeIndex, st.Cursor, k+1, uintptr(k+1)*elem)
		}
	}
	if st := immixProdSpan(t, objs[n-1]); int(st.AllocCount) != n {
		t.Fatalf("%v: full span has alloc count %d, want %d", c, st.AllocCount, n)
	}

	occ, _, _, _, ok := runtime.LogosImmixPlanes(objs[0])
	if !ok {
		t.Fatalf("%v: no block metadata", c)
	}
	used := (uintptr(n)*elem + immixProdLine - 1) / immixProdLine
	for j := range occ {
		if (occ[j] != 0) != (uintptr(j) < used) {
			t.Fatalf("%v: line %d occupied=%v in a full span whose slots cover %d lines", c, j, occ[j] != 0, used)
		}
	}
	if c.scan && c.size <= immixProdSmall {
		switch {
		case st0.HeapBitsLo == 0 || st0.HeapBitsHi != base+immixProdBlock || st0.HeapBitsHi-st0.HeapBitsLo != immixProdBlock/immixProdPtr/8:
			t.Fatalf("%v: heap-bits carve [%#x,%#x) of span %#x is not the last 512 bytes", c, st0.HeapBitsLo, st0.HeapBitsHi, base)
		case base+uintptr(n)*elem > st0.HeapBitsLo:
			t.Fatalf("%v: slots end at %#x, inside the heap-bits carve at %#x", c, base+uintptr(n)*elem, st0.HeapBitsLo)
		}
		carve = true
	} else if st0.HeapBitsLo != 0 {
		t.Fatalf("%v: span has a heap-bits carve [%#x,%#x)", c, st0.HeapBitsLo, st0.HeapBitsHi)
	}

	if est := immixProdSpan(t, reflect.New(typ).UnsafePointer()); est.Base == base {
		t.Fatalf("%v: a full span served another object", c)
	}

	for k, p := range objs {
		immixProdFill(c, p, k)
	}
	if c.scan {
		for k := 1; k < n; k++ {
			*(*unsafe.Pointer)(objs[k]) = objs[k-1]
		}
		immixProdHead = objs[n-1]
		clear(objs)
		runtime.GC()
		cnt := 0
		for q := immixProdHead; q != nil && cnt <= n; q = *(*unsafe.Pointer)(q) {
			cnt++
		}
		immixProdHead = nil
		if cnt != n {
			t.Fatalf("%v: a chain through the %d slots of a span holds %d objects after a collection", c, n, cnt)
		}
		return carve
	}
	for k, p := range objs {
		if !immixProdIntact(c, p, k) {
			t.Fatalf("%v: slot %d was overwritten by a neighbor", c, k)
		}
	}
	runtime.GC()
	for k, p := range objs {
		if !immixProdIntact(c, p, k) {
			t.Fatalf("%v: slot %d lost its contents across a collection", c, k)
		}
	}
	return carve
}

type immixProdHoleCase struct {
	c    immixProdClass
	name string
	drop func(k int) bool
	// holeFrom, when positive, is the first slot that only the hole path can
	// serve: the span refills through the classic scan, the bump path serves
	// the free slots of the 64-slot cache window, and at the window's end the
	// free run begins on a line boundary with no free slot before it, so the
	// hole path serves it and the free index stays at holeFrom.
	holeFrom int
}

// immixProdHoleCases lists the sweep patterns. The 128-byte class has one
// slot per line and 252 or 256 slots, so a freed run that crosses the 64-slot
// window starts on a line boundary. Without Green Tea it is a block class;
// with it the class is not, and the hole path cannot serve a slot of any block
// class, because every block class has at most 56 slots.
func immixProdHoleCases() []immixProdHoleCase {
	scatter := func(k int) bool { return k%4 == 3 }
	cases := []immixProdHoleCase{
		{immixProdClass{33, 1152, false}, "scatter", scatter, 0},
		{immixProdClass{33, 1152, true}, "scatter", scatter, 0},
		{immixProdClass{33, 1152, false}, "run", func(k int) bool { return k >= 8 && k < 14 }, 0},
	}
	if !goexperiment.GreenTeaGC {
		run := func(k int) bool { return k >= 100 && k < 200 }
		cases = append(cases,
			immixProdHoleCase{immixProdClass{10, 128, false}, "scatter", scatter, 0},
			immixProdHoleCase{immixProdClass{10, 128, false}, "run", run, 127},
			immixProdHoleCase{immixProdClass{10, 128, true}, "run", run, 127},
		)
	}
	return cases
}

// immixProdHoles fills a span, drops part of it, sweeps, and checks that the
// next allocations of the class return exactly the freed addresses, lowest
// first, zeroed, that the occupancy plane is the exact line set of the
// allocated slots after the sweep and after every reuse, and that the free
// index never advances past a free slot.
func immixProdHoles(t *testing.T) {
	for _, hc := range immixProdHoleCases() {
		t.Run(fmt.Sprintf("size%d_scan%v_%s", hc.c.size, hc.c.scan, hc.name), func(t *testing.T) {
			immixProdHoleCase1(t, hc)
		})
	}
}

// immixProdSpanModel is the test's own record of which slots of one span it
// allocated: the oracle for the occupancy plane.
type immixProdSpanModel struct {
	anchor     unsafe.Pointer
	base, elem uintptr
	live       []bool
}

// check verifies that the occupancy plane of the block of the anchor is
// exactly the set of lines that the allocated slots overlap. It formats
// nothing unless it fails, because the caller is counting allocations of a
// class.
func (m *immixProdSpanModel) check(t *testing.T, what string, step int) {
	t.Helper()
	occ, _, _, _, ok := runtime.LogosImmixPlanes(m.anchor)
	if !ok {
		t.Fatalf("%s %d: no block metadata", what, step)
	}
	var want [immixProdLines]bool
	for k, l := range m.live {
		if !l {
			continue
		}
		lo, hi := uintptr(k)*m.elem/immixProdLine, (uintptr(k+1)*m.elem-1)/immixProdLine
		for j := lo; j <= hi; j++ {
			want[j] = true
		}
	}
	for j := range occ {
		if (occ[j] != 0) != want[j] {
			t.Fatalf("%s %d: line %d occupied=%v, want %v for the allocated slots of span %#x", what, step, j, occ[j] != 0, want[j], m.base)
		}
	}
}

func immixProdHoleCase1(t *testing.T, hc immixProdHoleCase) {
	c := hc.c
	typ := c.typ()
	reflect.New(typ)
	runtime.GC()
	runtime.GC()
	n := c.wantElems()
	objs := make([]unsafe.Pointer, n)
	live := make([]bool, n)
	freed := make([]uintptr, 0, n)
	first := immixProdFindFresh(t, c, typ)
	st0 := immixProdSpan(t, first)
	base, elem := st0.Base, st0.ElemSize
	if int(st0.NElems) != n {
		t.Fatalf("%v: span has %d slots, want %d", c, st0.NElems, n)
	}
	objs[0] = first
	for k := 1; k < n; k++ {
		p := reflect.New(typ).UnsafePointer()
		if c.slot(p) != base+uintptr(k)*elem {
			t.Fatalf("%v: allocation %d at %#x, want %#x", c, k, c.slot(p), base+uintptr(k)*elem)
		}
		objs[k] = p
	}
	for k, p := range objs {
		immixProdFill(c, p, k)
		live[k] = true
	}
	nLive := n
	for k := range objs {
		if hc.drop(k) {
			live[k] = false
			freed = append(freed, base+uintptr(k)*elem)
			objs[k] = nil
			nLive--
		}
	}
	anchor := -1
	for k, l := range live {
		if l {
			anchor = k
			break
		}
	}
	if anchor < 0 || len(freed) == 0 {
		t.Fatalf("%v: pattern %s drops %d of %d slots", c, hc.name, len(freed), n)
	}

	runtime.GC()
	for k, p := range objs {
		if p == nil {
			continue
		}
		if c.slot(p) != base+uintptr(k)*elem || !immixProdIntact(c, p, k) {
			t.Fatalf("%v: survivor %d moved to %#x or changed although its block keeps %d of %d slots", c, k, c.slot(p), nLive, n)
		}
	}
	st := immixProdSpan(t, objs[anchor])
	switch {
	case int(st.AllocCount) != nLive:
		t.Fatalf("%v: swept span holds %d objects, want the %d survivors", c, st.AllocCount, nLive)
	case st.Cached || st.FreeIndex != 0:
		t.Fatalf("%v: swept span cached=%v freeindex=%d", c, st.Cached, st.FreeIndex)
	case st.Cursor != 0 || st.Next != 0 || st.CursorLimit != uint32(uintptr(n)*elem):
		t.Fatalf("%v: sweep left cursor %d next %d limit %d, want 0 0 %d", c, st.Cursor, st.Next, st.CursorLimit, uintptr(n)*elem)
	}
	model := &immixProdSpanModel{anchor: objs[anchor], base: base, elem: elem, live: live}
	model.check(t, "after the sweep", 0)

	for i := range freed {
		p := reflect.New(typ).UnsafePointer()
		slot := c.slot(p)
		if slot != freed[i] {
			t.Fatalf("%v: allocation %d after the sweep is at %#x, want the freed address %#x (lowest first)", c, i, slot, freed[i])
		}
		k := int((slot - base) / elem)
		objs[k] = p
		if !immixProdZero(c, p) {
			t.Fatalf("%v: the object reused at %#x was not zeroed", c, slot)
		}
		live[k] = true
		sp := immixProdSpan(t, p)
		if sp.Base != base || int(sp.AllocCount) != nLive+i+1 {
			t.Fatalf("%v: reuse %d: span %#x alloc count %d, want span %#x and %d", c, i, sp.Base, sp.AllocCount, base, nLive+i+1)
		}
		for j := 0; j < int(sp.FreeIndex); j++ {
			if !live[j] {
				t.Fatalf("%v: reuse %d: freeindex %d is past the free slot %d", c, i, sp.FreeIndex, j)
			}
		}
		if hc.holeFrom > 0 && k >= hc.holeFrom && int(sp.FreeIndex) != hc.holeFrom {
			t.Fatalf("%v: reuse %d of slot %d: freeindex %d, want the hole path to leave it at %d", c, i, k, sp.FreeIndex, hc.holeFrom)
		}
		model.check(t, "after reuse", i)
		immixProdFill(c, p, k)
	}
	if sp := immixProdSpan(t, reflect.New(typ).UnsafePointer()); sp.Base == base {
		t.Fatalf("%v: the span served an object beyond its %d slots", c, n)
	}
	runtime.GC()
	for k, p := range objs {
		if c.slot(p) != base+uintptr(k)*elem || !immixProdIntact(c, p, k) {
			t.Fatalf("%v: slot %d moved or changed after the reuse", c, k)
		}
	}
	if sp := immixProdSpan(t, objs[0]); int(sp.AllocCount) != n {
		t.Fatalf("%v: refilled span holds %d objects, want %d", c, sp.AllocCount, n)
	}
}

func immixProdDelta(sc int, a, b *runtime.MemStats) (mallocs, frees uint64) {
	return b.BySize[sc].Mallocs - a.BySize[sc].Mallocs, b.BySize[sc].Frees - a.BySize[sc].Frees
}

func immixProdCounters(t *testing.T) {
	cases := []immixProdClass{
		{33, 1152, false}, {33, 1152, true}, {38, 2048, false}, {44, 4096, true},
	}
	if !goexperiment.GreenTeaGC {
		cases = append(cases, immixProdClass{9, 112, false}, immixProdClass{9, 112, true})
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("size%d_scan%v", c.size, c.scan), func(t *testing.T) {
			immixProdCountersClass(t, c)
		})
	}
}

// immixProdCountersClass checks the exact MemStats.BySize counters of one
// class through allocation, a sweep that frees half of it, reuse of freed
// slots, and a final sweep. The child is the only allocator of the class.
func immixProdCountersClass(t *testing.T, c immixProdClass) {
	typ := c.typ()
	reflect.New(typ)
	sc := int(c.index)
	var m0, m1, m2, m3, m4 runtime.MemStats
	n := 3*c.wantElems() + 5
	keep := make([]unsafe.Pointer, n)
	runtime.GC()
	runtime.GC()
	// The baseline is read after the collections, not before: it must not
	// include frees of warm-up garbage from the span under test.
	runtime.ReadMemStats(&m0)
	if sc >= len(m0.BySize) || uintptr(m0.BySize[sc].Size) != c.size {
		t.Fatalf("%v: MemStats.BySize has no entry of %d bytes at the class index", c, c.size)
	}
	for i := range keep {
		keep[i] = reflect.New(typ).UnsafePointer()
	}
	runtime.ReadMemStats(&m1)
	if mal, fre := immixProdDelta(sc, &m0, &m1); mal != uint64(n) || fre != 0 {
		t.Fatalf("%v: after %d allocations BySize reports mallocs=%d frees=%d, want %d and 0", c, n, mal, fre, n)
	}
	for i := range keep {
		immixProdFill(c, keep[i], i)
	}
	dropped := 0
	for i := 0; i < n; i += 2 {
		keep[i] = nil
		dropped++
	}
	runtime.GC()
	runtime.ReadMemStats(&m2)
	if mal, fre := immixProdDelta(sc, &m0, &m2); mal != uint64(n) || fre != uint64(dropped) {
		t.Fatalf("%v: after dropping %d and a collection BySize reports mallocs=%d frees=%d, want %d and %d", c, dropped, mal, fre, n, dropped)
	}
	reused := dropped / 2
	for i, k := 0, 0; i < reused; i, k = i+1, k+2 {
		keep[k] = reflect.New(typ).UnsafePointer()
	}
	runtime.ReadMemStats(&m3)
	if mal, fre := immixProdDelta(sc, &m0, &m3); mal != uint64(n+reused) || fre != uint64(dropped) {
		t.Fatalf("%v: after reusing %d slots BySize reports mallocs=%d frees=%d, want %d and %d", c, reused, mal, fre, n+reused, dropped)
	}
	for i := 1; i < n; i += 2 {
		if !immixProdIntact(c, keep[i], i) {
			t.Fatalf("%v: survivor %d lost its contents", c, i)
		}
	}
	clear(keep)
	runtime.GC()
	runtime.ReadMemStats(&m4)
	if mal, fre := immixProdDelta(sc, &m0, &m4); mal != uint64(n+reused) || fre != uint64(n+reused) {
		t.Fatalf("%v: after dropping everything BySize reports mallocs=%d frees=%d, want %d for both", c, mal, fre, n+reused)
	}
}

// immixProdBlob is a pointer-free object of exactly 1152 bytes, a size class
// with 28 slots in a 32 KiB block of 252 lines, a 4-line unused tail.
type immixProdBlob struct {
	id  uintptr
	pad [1152 - unsafe.Sizeof(uintptr(0))]byte
}

// immixProdGroup is a set of survivors that share one exclusion property.
type immixProdGroup struct {
	name   string
	ptrs   []unsafe.Pointer
	ids    []uintptr
	before []uintptr
}

// immixProdFillBlocks allocates n spans of perBlock slots, one after the
// other, and returns the object of every slot. It fails unless each span is
// fresh and every object is the next slot of it, so every later statement
// about a span is about exactly the objects that the caller sees.
func immixProdFillBlocks(t *testing.T, alloc func() unsafe.Pointer, hdr uintptr, n, perBlock int) [][]unsafe.Pointer {
	t.Helper()
	blocks := make([][]unsafe.Pointer, 0, n)
	skipped := make([]unsafe.Pointer, 0, 8*perBlock+32)
	for len(blocks) < n {
		cur := make([]unsafe.Pointer, 0, perBlock)
		var base, elem uintptr
		for try := 0; base == 0; try++ {
			if try > 4*perBlock+16 {
				t.Fatalf("no fresh span in %d allocations", try)
			}
			p := alloc()
			st := immixProdSpan(t, p)
			if !st.Active {
				t.Fatalf("object %p is not in an immix span: %+v", p, st)
			}
			if st.AllocCount == 1 && st.FreeIndex == 1 && uintptr(p)-hdr == st.Base {
				base, elem = st.Base, st.ElemSize
				if int(st.NElems) != perBlock {
					t.Fatalf("span has %d slots, want %d", st.NElems, perBlock)
				}
				cur = append(cur, p)
				continue
			}
			skipped = append(skipped, p)
		}
		for k := 1; k < perBlock; k++ {
			p := alloc()
			if uintptr(p)-hdr != base+uintptr(k)*elem {
				t.Fatalf("slot %d of span %#x was served at %#x", k, base, uintptr(p)-hdr)
			}
			cur = append(cur, p)
		}
		blocks = append(blocks, cur)
	}
	immixProdSkipped = skipped
	return blocks
}

func immixProdBlobFill(p unsafe.Pointer, id uintptr) {
	b := (*immixProdBlob)(p)
	b.id = id
	for i := range b.pad {
		b.pad[i] = byte(id) + byte(i)
	}
}

func immixProdBlobIntact(p unsafe.Pointer, id uintptr) bool {
	b := (*immixProdBlob)(p)
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

func immixProdNewGroup(name string, block []unsafe.Pointer, base uintptr, keep func(k int) bool) *immixProdGroup {
	g := &immixProdGroup{name: name}
	for k, p := range block {
		if keep(k) {
			id := base + uintptr(k) + 1
			immixProdBlobFill(p, id)
			g.ptrs = append(g.ptrs, p)
			g.ids = append(g.ids, id)
		}
	}
	return g
}

func (g *immixProdGroup) record() {
	g.before = g.before[:0]
	for _, p := range g.ptrs {
		g.before = append(g.before, uintptr(p))
	}
}

func (g *immixProdGroup) moved() int {
	n := 0
	for i, p := range g.ptrs {
		if uintptr(p) != g.before[i] {
			n++
		}
	}
	return n
}

func (g *immixProdGroup) intact() bool {
	for i, p := range g.ptrs {
		if !immixProdBlobIntact(p, g.ids[i]) {
			return false
		}
	}
	return true
}

func immixProdAddrs(ps []unsafe.Pointer) []uintptr {
	out := make([]uintptr, len(ps))
	for i, p := range ps {
		out[i] = uintptr(p)
	}
	return out
}

func immixProdMovedFrom(ps []unsafe.Pointer, before []uintptr) int {
	n := 0
	for i, p := range ps {
		if uintptr(p) != before[i] {
			n++
		}
	}
	return n
}

// immixProdExclusions compares survivors of sparse blocks that nothing holds
// in place with survivors of blocks that a finalizer, a cleanup, a pin, or
// their size excludes from relocation, and with survivors of dense blocks. The
// quiet groups are the control: they must move, so that the groups that do
// not move are held by their exclusion and not by an evacuation that never
// ran. The span state read through the exports is the production state.
func immixProdExclusions(t *testing.T) {
	const nBlocks, perBlock = 8, 28
	alloc := func() unsafe.Pointer { return unsafe.Pointer(new(immixProdBlob)) }
	immixProdBigs = make([][]byte, 3)
	bigAddr := make([]uintptr, len(immixProdBigs)+1)
	for i := range immixProdBigs {
		immixProdBigs[i] = make([]byte, 40000)
		immixProdBigs[i][0] = byte(i + 1)
		bigAddr[i] = uintptr(unsafe.Pointer(&immixProdBigs[i][0]))
	}
	immixProdBigScan = reflect.New(reflect.ArrayOf(5000, reflect.TypeFor[*byte]())).UnsafePointer()
	bigAddr[len(immixProdBigs)] = uintptr(immixProdBigScan)
	runtime.GC()
	runtime.GC()

	blocks := immixProdFillBlocks(t, alloc, 0, nBlocks, perBlock)
	sparse := func(k int) bool { return k%10 == 0 }
	dense := func(k int) bool { return k%2 == 0 || k == perBlock-1 }
	host0 := immixProdNewGroup("host0", blocks[0], 0, dense)
	host1 := immixProdNewGroup("host1", blocks[1], 100, dense)
	quiet0 := immixProdNewGroup("quiet0", blocks[2], 200, sparse)
	quiet1 := immixProdNewGroup("quiet1", blocks[3], 300, sparse)
	fin := immixProdNewGroup("finalizer", blocks[4], 400, sparse)
	cleanup := immixProdNewGroup("cleanup", blocks[5], 500, sparse)
	pinned := immixProdNewGroup("pinned", blocks[6], 600, sparse)
	grps := []*immixProdGroup{host0, host1, quiet0, quiet1, fin, cleanup, pinned}
	immixProdGroups = grps
	for _, b := range blocks {
		clear(b)
	}
	blocks = nil
	for _, g := range grps {
		g.record()
	}
	runtime.SetFinalizer((*immixProdBlob)(fin.ptrs[1]), func(*immixProdBlob) {})
	cl := runtime.AddCleanup((*immixProdBlob)(cleanup.ptrs[1]), func(int) {}, 0)
	var pin runtime.Pinner
	pin.Pin((*immixProdBlob)(pinned.ptrs[1]))

	stateOf := func(g *immixProdGroup) runtime.LogosImmixSpan {
		return immixProdSpan(t, g.ptrs[1])
	}
	checkLarge := func(step string) {
		t.Helper()
		for i := range immixProdBigs {
			if uintptr(unsafe.Pointer(&immixProdBigs[i][0])) != bigAddr[i] || immixProdBigs[i][0] != byte(i+1) {
				t.Fatalf("%s: large object %d moved from %#x or lost its contents", step, i, bigAddr[i])
			}
		}
		if uintptr(immixProdBigScan) != bigAddr[len(immixProdBigs)] {
			t.Fatalf("%s: large pointer-bearing object moved from %#x", step, bigAddr[len(immixProdBigs)])
		}
		st := immixProdSpan(t, immixProdBigScan)
		if st.SizeClass != 0 || st.Active || st.HasBlock {
			t.Fatalf("%s: large object is in an immix span: %+v", step, st)
		}
	}
	checkIntact := func(step string) {
		t.Helper()
		for _, g := range grps {
			if !g.intact() {
				t.Fatalf("%s: group %s lost its contents", step, g.name)
			}
		}
	}

	runtime.GC()
	checkIntact("first collection")
	checkLarge("first collection")
	for _, g := range []*immixProdGroup{host0, host1} {
		if m := g.moved(); m != 0 {
			t.Fatalf("dense group %s moved %d of %d survivors: a block that keeps half of its lines is not evacuated", g.name, m, len(g.ptrs))
		}
	}
	moved := quiet0.moved() + quiet1.moved()
	if moved*2 < len(quiet0.ptrs)+len(quiet1.ptrs) {
		t.Fatalf("the control moved %d of %d survivors of unexcluded sparse blocks: the evacuation did not run", moved, len(quiet0.ptrs)+len(quiet1.ptrs))
	}
	for _, g := range []*immixProdGroup{fin, cleanup} {
		st := stateOf(g)
		if m := g.moved(); m != 0 {
			t.Fatalf("group %s moved %d of %d survivors although its span has a special: %+v", g.name, m, len(g.ptrs), st)
		}
		if !st.Active || !st.HasBlock || !st.NoReloc || !st.Specials {
			t.Fatalf("group %s: span state %+v, want an immix span with a special and the relocation exclusion", g.name, st)
		}
	}
	{
		st := stateOf(pinned)
		_, _, _, blockPinned, _ := runtime.LogosImmixPlanes(pinned.ptrs[1])
		if m := pinned.moved(); m != 0 {
			t.Fatalf("pinned group moved %d of %d survivors: %+v", m, len(pinned.ptrs), st)
		}
		if !st.Pinned || !blockPinned {
			t.Fatalf("pinned group: span pinned=%v block pinned=%v, want both after a collection", st.Pinned, blockPinned)
		}
	}

	pin.Unpin()
	pinOld := uintptr(pinned.ptrs[1])
	pinned.record()
	runtime.GC()
	checkIntact("after unpin")
	checkLarge("after unpin")
	if m := pinned.moved(); m*2 < len(pinned.ptrs) {
		t.Fatalf("unpinned group moved %d of %d survivors: the pin of its block was not released", m, len(pinned.ptrs))
	}
	if _, _, _, blockPinned, ok := runtime.LogosImmixPlanesAt(pinOld); !ok || blockPinned {
		t.Fatalf("the block of the unpinned object keeps its pin bit (metadata=%v pinned=%v)", ok, blockPinned)
	}
	for _, g := range []*immixProdGroup{fin, cleanup} {
		if m := g.moved(); m != 0 {
			t.Fatalf("group %s moved %d survivors once the pin was gone: the exclusion of a special is not sticky", g.name, m)
		}
	}

	runtime.SetFinalizer((*immixProdBlob)(fin.ptrs[1]), nil)
	cl.Stop()
	runtime.GC()
	checkIntact("after removal")
	for _, g := range []*immixProdGroup{fin, cleanup} {
		st := stateOf(g)
		if m := g.moved(); m != 0 || !st.NoReloc {
			t.Fatalf("group %s moved %d survivors after the special was removed (state %+v): removal must not clear the exclusion", g.name, m, st)
		}
	}
	runtime.KeepAlive(&pin)
}

// immixProdLineWord is one 128-byte stretch of an immixProdDirtyObj with a
// pointer at its start.
type immixProdLineWord struct {
	p   *byte
	pad [120]byte
}

// immixProdDirtyObj is a pointer-bearing object of 1144 bytes. With the
// 8-byte malloc header it fills a 1152-byte slot, nine lines, and it holds a
// pointer field in each of them: a store to every field dirties every line of
// its slot.
type immixProdDirtyObj struct {
	l    [8]immixProdLineWord
	tail struct {
		p   *byte
		pad [112]byte
	}
}

var (
	_ [1144 - unsafe.Sizeof(immixProdDirtyObj{})]byte
	_ [unsafe.Sizeof(immixProdDirtyObj{}) - 1144]byte
)

// immixProdDirty checks the dirty plane over two collections with a mark
// window that the test opens deterministically: with one P, the goroutine that
// triggers the cycle keeps the P until it yields, and the cycle cannot reach
// mark termination before then. Inside the window it stores pointers into
// objects that it then leaves unreachable, so the lines of those objects are
// dirty but unmarked. Quiet objects are never written, so the window adds no
// active-table marks on their lines; the other parity still holds the
// previous cycle's dirt until this cycle's termination clears it, so only
// the active table is asserted mid-window.
//
// The mark of the window selects blocks with the dirty lines counted as
// occupied, so the sparse blocks whose dead objects were written stay in
// place, and the sparse blocks that were never written move (the control).
// The table of the window is cleared one termination later, so the next
// cycle sees the written blocks as sparse again and moves them.
func immixProdDirty(t *testing.T) {
	for attempt := 0; attempt < 6; attempt++ {
		if immixProdDirtyTrial(t) {
			return
		}
	}
	t.Fatal("the mark window could not be entered in 6 attempts")
}

func immixProdDirtyTrial(t *testing.T) bool {
	const (
		nA, nB, nH = 3, 3, 3
		perBlock   = 28
		dead       = perBlock - 3
	)
	immixProdDirtyA, immixProdDirtyB, immixProdDirtyH = nil, nil, nil
	alloc := func() unsafe.Pointer { return unsafe.Pointer(new(immixProdDirtyObj)) }
	runtime.GC()
	runtime.GC()
	blocks := immixProdFillBlocks(t, alloc, immixProdHeader, nA+nB+nH+1, perBlock)
	immixProdDirtyA = make([]unsafe.Pointer, 0, nA*3)
	immixProdDirtyB = make([]unsafe.Pointer, 0, nB*3)
	immixProdDirtyH = make([]unsafe.Pointer, 0, nH*perBlock)
	var held [nA * dead]*immixProdDirtyObj
	var daddr [nA * dead]uintptr
	var survA, survB [nA * 3]uintptr
	hi, ia, ib := 0, 0, 0
	for b := range blocks {
		for k, p := range blocks[b] {
			switch {
			case b < nA:
				if k%10 == 0 {
					immixProdDirtyA = append(immixProdDirtyA, p)
					survA[ia] = uintptr(p) - immixProdHeader
					ia++
				} else {
					held[hi] = (*immixProdDirtyObj)(p)
					daddr[hi] = uintptr(p) - immixProdHeader
					hi++
				}
			case b < nA+nB:
				if k%10 == 0 {
					immixProdDirtyB = append(immixProdDirtyB, p)
					survB[ib] = uintptr(p) - immixProdHeader
					ib++
				}
			case b < nA+nB+nH:
				if k%2 == 0 || k == perBlock-1 {
					immixProdDirtyH = append(immixProdDirtyH, p)
				}
			}
			blocks[b][k] = nil
		}
	}
	blocks = nil
	beforeA, beforeB, beforeH := immixProdAddrs(immixProdDirtyA), immixProdAddrs(immixProdDirtyB), immixProdAddrs(immixProdDirtyH)
	e0, c0 := runtime.LogosImmixDirtyEpoch(), runtime.LogosGCCycles()
	ev0, _ := runtime.LogosEvacTotals()

	debug.SetGCPercent(1)
	for i := 0; runtime.LogosImmixDirtyEpoch() == e0; i++ {
		if i > 100000 {
			debug.SetGCPercent(-1)
			t.Fatal("no collection started")
		}
		immixProdGarbage = make([]byte, 4096)
	}

	// The window: nothing below allocates, yields, or calls into the test
	// framework until the observations are taken.
	ek := runtime.LogosImmixDirtyEpoch()
	open := runtime.LogosGCMarkActive() && ek == e0+1
	v := immixProdValue
	stored := 0
	if open {
		for i := range held {
			o := held[i]
			for j := range o.l {
				o.l[j].p = v
			}
			o.tail.p = v
			held[i] = nil
			stored++
		}
	}
	// Mid-window the old parity still holds the previous cycle's dirt by
	// design: it is cleared at this cycle's termination, after selection
	// has read the active table. Only the active table can carry this
	// window's marks (the barrier writes dirty[global&1], and the global
	// cannot flip while the world runs), so barrier precision, including
	// over-marking, fails the test on the active table alone here.
	// Old-parity hits are logged for forensics but do not fail: they are
	// asserted only after the clearing termination below, where persisting
	// dirt proves a lost enlistment instead of a stale table.
	var bad struct {
		dirtLines, dirtLinesOld, epochA, survDirt, survDirtOld, bDirt, bDirtOld, epochB int
	}
	if open {
		for i := range daddr {
			_, dirty, ep, _, _ := runtime.LogosImmixPlanesAt(daddr[i])
			first := (daddr[i] % immixProdBlock) / immixProdLine
			for j := first; j < first+9; j++ {
				if dirty[ek&1][j] == 0 {
					bad.dirtLines++
				}
				if dirty[(ek+1)&1][j] != 0 {
					bad.dirtLinesOld++
				}
			}
			if ep != ek {
				bad.epochA++
			}
		}
		for _, s := range survA {
			_, dirty, _, _, _ := runtime.LogosImmixPlanesAt(s)
			first := (s % immixProdBlock) / immixProdLine
			for j := first; j < first+9; j++ {
				if dirty[ek&1][j] != 0 {
					bad.survDirt++
				}
				if dirty[(ek+1)&1][j] != 0 {
					bad.survDirtOld++
				}
			}
		}
		for _, s := range survB {
			_, dirty, ep, _, _ := runtime.LogosImmixPlanesAt(s)
			for j := range dirty[ek&1] {
				if dirty[ek&1][j] != 0 {
					bad.bDirt++
				}
				if dirty[(ek+1)&1][j] != 0 {
					bad.bDirtOld++
				}
			}
			if ep == ek {
				bad.epochB++
			}
		}
	}
	intact := open && runtime.LogosGCMarkActive() && runtime.LogosImmixDirtyEpoch() == ek && runtime.LogosGCCycles() == c0+1
	for i := 0; runtime.LogosGCMarkActive(); i++ {
		if i > 1000000 {
			t.Fatal("the mark phase did not finish")
		}
		runtime.Gosched()
	}
	debug.SetGCPercent(-1)
	if !intact || stored != len(held) {
		return false
	}

	if bad.dirtLines != 0 || bad.epochA != 0 {
		t.Errorf("window stores: %d dirty lines missing from the active table, %d blocks not enlisted in epoch %d", bad.dirtLines, bad.epochA, ek)
	}
	if bad.survDirt != 0 || bad.bDirt != 0 || bad.epochB != 0 {
		t.Errorf("quiet objects: %d active-table dirty lines on unwritten survivors, %d in unwritten blocks, %d unwritten blocks enlisted in epoch %d", bad.survDirt, bad.bDirt, bad.epochB, ek)
	}
	if bad.dirtLinesOld != 0 || bad.survDirtOld != 0 || bad.bDirtOld != 0 {
		t.Logf("previous-cycle dirt still present mid-window (clearing at termination decides it): window=%d survivors=%d blocks=%d", bad.dirtLinesOld, bad.survDirtOld, bad.bDirtOld)
	}
	// The mark-phase wait above ends after this cycle's termination STW,
	// which clears the old table for every block enlisted in the previous
	// cycle. Any stale previous-cycle dirt seen mid-window therefore must
	// be gone here; if it persists, its enlistment was lost and the
	// bounded clear can never reach it. The active table is deliberately
	// not asserted on survA blocks: it keeps the window table for one more
	// termination by design (checked after the next cycle below). survB
	// blocks were never written in this cycle, so both tables must read
	// clean.
	for _, s := range survA {
		_, dirty, _, _, _ := runtime.LogosImmixPlanesAt(s)
		first := (s % immixProdBlock) / immixProdLine
		for j := first; j < first+9; j++ {
			if dirty[(ek+1)&1][j] != 0 {
				t.Fatalf("quiet survivor %#x line %d still dirty in the old table after the window termination cleared it: stale dirt was never cleared", s, j)
			}
		}
	}
	for _, s := range survB {
		_, dirty, _, _, _ := runtime.LogosImmixPlanesAt(s)
		for j := range dirty[0] {
			if dirty[0][j] != 0 || dirty[1][j] != 0 {
				t.Fatalf("unwritten block %#x line %d still dirty after the window termination (%d %d): stale dirt was never cleared", s&^(immixProdBlock-1), j, dirty[0][j], dirty[1][j])
			}
		}
	}
	if ev1, _ := runtime.LogosEvacTotals(); ev1 != ev0+1 || runtime.LogosImmixDirtyEpoch() != ek {
		t.Fatalf("the window cycle ran %d evacuations at epoch %d, want 1 at epoch %d: another cycle intervened", ev1-ev0, runtime.LogosImmixDirtyEpoch(), ek)
	}
	if m := immixProdMovedFrom(immixProdDirtyA, beforeA); m != 0 {
		t.Errorf("%d of %d survivors of blocks whose dead objects were written in the mark window moved: the dirty lines were not counted as occupied", m, len(beforeA))
	}
	if m := immixProdMovedFrom(immixProdDirtyH, beforeH); m != 0 {
		t.Errorf("%d of %d survivors of dense blocks moved", m, len(beforeH))
	}
	if m := immixProdMovedFrom(immixProdDirtyB, beforeB); m*2 < len(beforeB) {
		t.Errorf("the control moved %d of %d survivors of quiet sparse blocks: the evacuation did not run", m, len(beforeB))
	}
	for _, d := range daddr {
		_, dirty, _, _, _ := runtime.LogosImmixPlanesAt(d)
		first := (d % immixProdBlock) / immixProdLine
		for j := first; j < first+9; j++ {
			if dirty[ek&1][j] == 0 || dirty[(ek+1)&1][j] != 0 {
				t.Fatalf("after the window cycle the line %d of %#x has active=%d other=%d, want the table of the window kept for one more termination and the other clean", j, d, dirty[ek&1][j], dirty[(ek+1)&1][j])
			}
		}
	}

	runtime.GC()
	if ev2, _ := runtime.LogosEvacTotals(); ev2 != ev0+2 || runtime.LogosImmixDirtyEpoch() != ek+1 {
		t.Fatalf("two cycles ran %d evacuations at epoch %d, want 2 at epoch %d", ev2-ev0, runtime.LogosImmixDirtyEpoch(), ek+1)
	}
	for _, s := range survA {
		_, dirty, _, _, _ := runtime.LogosImmixPlanesAt(s)
		for j := range dirty[0] {
			if dirty[0][j] != 0 || dirty[1][j] != 0 {
				t.Fatalf("block %#x line %d is still dirty after two cycles: the table of the window was not cleared (%d %d)", s&^(immixProdBlock-1), j, dirty[0][j], dirty[1][j])
			}
		}
	}
	if m := immixProdMovedFrom(immixProdDirtyA, beforeA); m*2 < len(beforeA) {
		t.Errorf("only %d of %d survivors of the written blocks moved in the next cycle: stale dirt still pins them", m, len(beforeA))
	}
	immixProdDirtyA, immixProdDirtyB, immixProdDirtyH = nil, nil, nil
	return true
}

func buildLogosImmixHelper(t *testing.T, immix bool) string {
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
	cmd.Dir = "testdata/testlogosimmix"
	cmd.Env = append(os.Environ(), "GOEXPERIMENT="+exp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", cmd, err, out)
	}
	return exe
}

func runLogosImmixHelper(t *testing.T, exe, scenario, expect string, env ...string) {
	t.Helper()
	greentea := "0"
	if goexperiment.GreenTeaGC {
		greentea = "1"
	}
	cmd := testenv.Command(t, exe, scenario)
	set := []string{"LOGOS_IMMIX_EXPECT=" + expect, "LOGOS_IMMIX_GREENTEA=" + greentea, "GOMAXPROCS=1"}
	cmd.Env = append(os.Environ(), append(set, env...)...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("%s: %v\n%s", scenario, err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

var logosImmixHelperScenarios = []string{"aligned", "reuse", "counters", "exclusions"}

// TestLogosImmixHelperScenarios runs the pure-Go helper program under immix,
// linked with -X=runtime.logosGCMode=immix the way the linker selects the
// mode, and under the plain collector as the control. It does not depend on
// how the test binary itself is linked.
func TestLogosImmixHelperScenarios(t *testing.T) {
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no subprocesses")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("the moving collectors exist on amd64 and arm64")
	}
	testenv.MustHaveGoBuild(t)
	immix, plain := buildLogosImmixHelper(t, true), buildLogosImmixHelper(t, false)
	for _, s := range logosImmixHelperScenarios {
		t.Run("immix/"+s, func(t *testing.T) {
			runLogosImmixHelper(t, immix, s, "1")
		})
		t.Run("plain/"+s, func(t *testing.T) {
			runLogosImmixHelper(t, plain, s, "0")
		})
	}
	t.Run("immix/exclusions/checkmark", func(t *testing.T) {
		runLogosImmixHelper(t, immix, "exclusions", "1", "GODEBUG=gccheckmark=1")
	})
}
