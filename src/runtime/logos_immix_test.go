// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"internal/cpu"
	"internal/testenv"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

const (
	immixBlock = runtime.LogosImmixBlockBytes
	immixLine  = runtime.LogosImmixLineBytes
	immixLines = runtime.LogosImmixLines
)

var immixGlobal [64]byte

var immixSink int

// immixPattern marks the lines [lo, hi) of b live and every other line free.
func immixPattern(b *runtime.LogosImmixBlock, live ...[2]int) {
	b.ClearLines()
	lines := b.Lines()
	for _, r := range live {
		for i := r[0]; i < r[1]; i++ {
			lines[i] = 1
		}
	}
}

// immixHolePattern is the plan's medium path layout: lines 0-9 live, 10-13
// free, 14-19 live, 20-255 free.
func immixHolePattern(b *runtime.LogosImmixBlock) {
	immixPattern(b, [2]int{0, 10}, [2]int{14, 20})
}

func immixRefHole(lm *runtime.LogosLineMark, from int) (start, end int, ok bool) {
	if from < 0 || from >= immixLines {
		return 0, 0, false
	}
	start = from
	for start < immixLines && lm[start] != 0 {
		start++
	}
	if start == immixLines {
		return 0, 0, false
	}
	end = start
	for end < immixLines && lm[end] == 0 {
		end++
	}
	return start, end, true
}

type immixFinder struct {
	name string
	find func(*runtime.LogosLineMark, int) (int, int, bool)
}

// immixFinders returns every findHole path this CPU can run.
func immixFinders() []immixFinder {
	fs := []immixFinder{
		{"Dispatch", (*runtime.LogosLineMark).FindHole},
		{"Go", (*runtime.LogosLineMark).FindHoleGo},
	}
	if runtime.LogosImmixHasAVX2() {
		fs = append(fs, immixFinder{"AVX2", (*runtime.LogosLineMark).FindHoleAVX2})
	}
	return fs
}

func immixRefFree(lm *runtime.LogosLineMark) int {
	n := 0
	for _, v := range lm {
		if v == 0 {
			n++
		}
	}
	return n
}

// TestLogosImmixGeometry checks the 32 KiB block, the 256 lines of 128 B,
// the 256-byte line header, and that the header lives off the Go heap.
func TestLogosImmixGeometry(t *testing.T) {
	if immixBlock != 32<<10 || immixLine != 128 || immixLines != 256 {
		t.Fatalf("block %d line %d lines %d, want 32768 128 256", immixBlock, immixLine, immixLines)
	}
	if immixBlock != immixLine*immixLines {
		t.Fatalf("block %d != %d lines of %d", immixBlock, immixLines, immixLine)
	}
	if got := unsafe.Sizeof(runtime.LogosLineMark{}); got != 256 {
		t.Fatalf("sizeof logosLineMark = %d, want 256", got)
	}
	b := runtime.LogosImmixNewBlock()
	if b == nil {
		t.Fatal("no metadata for a heap block")
	}
	if b.Base()%immixBlock != 0 {
		t.Fatalf("block base %#x is not 32 KiB aligned", b.Base())
	}
	if !runtime.LogosImmixInHeap(b.Base()) {
		t.Fatalf("block base %#x is not in the heap", b.Base())
	}
	if runtime.LogosImmixInHeap(b.Addr()) {
		t.Fatalf("block metadata %#x is inside the Go heap", b.Addr())
	}
	if b.Addr() >= b.Base() && b.Addr() < b.Base()+immixBlock {
		t.Fatalf("block metadata %#x lies inside its own block", b.Addr())
	}
	if b.FreeLines() != immixLines || b.Pinned() {
		t.Fatalf("fresh block: free %d pinned %v, want %d false", b.FreeLines(), b.Pinned(), immixLines)
	}
}

// TestLogosImmixBlockLookup checks that BlockFor creates once and Lookup
// never creates.
func TestLogosImmixBlockLookup(t *testing.T) {
	base := runtime.LogosImmixNewBlocks(3)
	if runtime.LogosImmixBlockLookup(base) != nil || runtime.LogosImmixBlockLookup(base+2*immixBlock) != nil {
		t.Fatal("Lookup found metadata of a block never created")
	}
	b := runtime.LogosImmixBlockFor(base + 3*immixLine)
	if b == nil || b.Base() != base {
		t.Fatalf("BlockFor(base+384) = %v, want the block at %#x", b, base)
	}
	if again := runtime.LogosImmixBlockFor(base + immixBlock - 1); again != b {
		t.Fatalf("BlockFor of the last byte gave %p, want %p", again, b)
	}
	if got := runtime.LogosImmixBlockLookup(base + 17); got != b {
		t.Fatalf("Lookup = %p, want %p", got, b)
	}
	if runtime.LogosImmixBlockLookup(base+immixBlock) != nil {
		t.Fatal("creating one block created its neighbor")
	}
	if runtime.LogosImmixBlockFor(0) != nil || runtime.LogosImmixBlockLookup(0) != nil {
		t.Fatal("address 0 got block metadata")
	}
	if runtime.LogosImmixBlockFor(^uintptr(0)) != nil {
		t.Fatal("address ^0 got block metadata")
	}
	if runtime.LogosImmixBlockFor(uintptr(unsafe.Pointer(&immixGlobal))) != nil {
		t.Fatal("a data segment address got block metadata")
	}
}

// TestLogosImmixConcurrentBlockFor checks that racing creators agree on
// the metadata of every block.
func TestLogosImmixConcurrentBlockFor(t *testing.T) {
	const blocks, workers = 32, 8
	base := runtime.LogosImmixNewBlocks(blocks)
	got := make([][blocks]*runtime.LogosImmixBlock, workers)
	var done sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		done.Go(func() {
			<-start
			for i := range blocks {
				got[w][i] = runtime.LogosImmixBlockFor(base + uintptr(i)*immixBlock + uintptr(w))
			}
		})
	}
	close(start)
	done.Wait()
	for i := range blocks {
		for w := range workers {
			if got[w][i] == nil || got[w][i] != got[0][i] {
				t.Fatalf("block %d: worker %d got %p, worker 0 got %p", i, w, got[w][i], got[0][i])
			}
		}
		if got[0][i].Base() != base+uintptr(i)*immixBlock {
			t.Fatalf("block %d base %#x, want %#x", i, got[0][i].Base(), base+uintptr(i)*immixBlock)
		}
	}
}

// TestLogosImmixBumpFastPath checks that allocation bumps inside
// [cursor, limit) and never leaves the block.
func TestLogosImmixBumpFastPath(t *testing.T) {
	b := runtime.LogosImmixNewBlock()
	var a runtime.LogosImmixAllocator
	a.Reset(b)
	if a.Cursor() != 0 || a.Limit() != 0 || a.Next() != 0 {
		t.Fatalf("after Reset cursor %#x limit %#x next %d, want zeros", a.Cursor(), a.Limit(), a.Next())
	}
	base, end := b.Base(), b.Base()+immixBlock
	if p := a.Alloc(16); p != base {
		t.Fatalf("first Alloc(16) = %#x, want block base %#x", p, base)
	}
	if a.Cursor() != base+16 || a.Limit() != end {
		t.Fatalf("after first Alloc cursor %#x limit %#x, want %#x %#x", a.Cursor(), a.Limit(), base+16, end)
	}
	n := 1
	for {
		before := a.Cursor()
		p := a.Alloc(16)
		if p == 0 {
			break
		}
		if p != before || a.Cursor() != before+16 {
			t.Fatalf("Alloc #%d = %#x cursor %#x, want %#x %#x", n, p, a.Cursor(), before, before+16)
		}
		if p < base || p+16 > end {
			t.Fatalf("Alloc #%d = %#x escapes block [%#x,%#x)", n, p, base, end)
		}
		n++
	}
	if n != immixBlock/16 {
		t.Fatalf("a free block served %d 16-byte allocations, want %d", n, immixBlock/16)
	}
	if a.Cursor() != end || a.Limit() != end {
		t.Fatalf("exhausted: cursor %#x limit %#x, want both %#x", a.Cursor(), a.Limit(), end)
	}
}

// TestLogosImmixSizeRounding checks the 16-byte rounding and the size
// bounds.
func TestLogosImmixSizeRounding(t *testing.T) {
	b := runtime.LogosImmixNewBlock()
	var a runtime.LogosImmixAllocator
	a.Reset(b)
	base := b.Base()
	steps := []struct{ size, advance uintptr }{{1, 16}, {16, 16}, {17, 32}, {31, 32}, {33, 48}, {127, 128}, {129, 144}}
	want := base
	for _, s := range steps {
		if p := a.Alloc(s.size); p != want {
			t.Fatalf("Alloc(%d) = %#x, want %#x", s.size, p, want)
		}
		want += s.advance
		if a.Cursor() != want {
			t.Fatalf("after Alloc(%d) cursor %#x, want %#x", s.size, a.Cursor(), want)
		}
	}
	cursor, limit, next := a.Cursor(), a.Limit(), a.Next()
	for _, size := range []uintptr{0, immixBlock + 1, 1 << 20, ^uintptr(0)} {
		if p := a.Alloc(size); p != 0 {
			t.Fatalf("Alloc(%d) = %#x, want 0", size, p)
		}
	}
	if a.Cursor() != cursor || a.Limit() != limit || a.Next() != next {
		t.Fatalf("a rejected size moved the allocator: cursor %#x limit %#x next %d", a.Cursor(), a.Limit(), a.Next())
	}
	a.Reset(b)
	if p := a.Alloc(immixBlock); p != base || a.Cursor() != base+immixBlock || a.Limit() != base+immixBlock {
		t.Fatalf("whole-block Alloc = %#x cursor %#x limit %#x, want %#x and both %#x", p, a.Cursor(), a.Limit(), base, base+immixBlock)
	}
	if p := a.Alloc(16); p != 0 {
		t.Fatalf("Alloc(16) after a whole-block allocation = %#x, want 0", p)
	}
}

// TestLogosImmixMediumPath checks hole hunting at hole end, exact fit, and
// one byte over, on the layout lines 0-9 live, 10-13 free, 14-19 live,
// 20-255 free.
func TestLogosImmixMediumPath(t *testing.T) {
	b := runtime.LogosImmixNewBlock()
	base := b.Base()
	line := func(i int) uintptr { return base + uintptr(i)*immixLine }
	var a runtime.LogosImmixAllocator

	t.Run("ExactFit", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		if p := a.Alloc(4 * immixLine); p != line(10) {
			t.Fatalf("Alloc(512) = %#x, want line 10 %#x", p, line(10))
		}
		if a.Cursor() != line(14) || a.Limit() != line(14) || a.Next() != 14 {
			t.Fatalf("cursor %#x limit %#x next %d, want %#x %#x 14", a.Cursor(), a.Limit(), a.Next(), line(14), line(14))
		}
		if p := a.Alloc(16); p != line(20) {
			t.Fatalf("Alloc(16) after the exact fit = %#x, want line 20 %#x", p, line(20))
		}
		if a.Limit() != line(256) {
			t.Fatalf("limit %#x, want block end %#x", a.Limit(), line(256))
		}
	})

	t.Run("HoleEnd", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		if p := a.Alloc(496); p != line(10) {
			t.Fatalf("Alloc(496) = %#x, want line 10 %#x", p, line(10))
		}
		if p := a.Alloc(16); p != line(10)+496 {
			t.Fatalf("Alloc(16) at the hole end = %#x, want %#x", p, line(10)+496)
		}
		if a.Cursor() != line(14) || a.Cursor() != a.Limit() {
			t.Fatalf("cursor %#x limit %#x, want both %#x", a.Cursor(), a.Limit(), line(14))
		}
		if p := a.Alloc(16); p != line(20) {
			t.Fatalf("Alloc(16) past the hole = %#x, want line 20 %#x", p, line(20))
		}
	})

	t.Run("OneOver", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		if p := a.Alloc(4*immixLine + 1); p != line(20) {
			t.Fatalf("Alloc(513) = %#x, want line 20 %#x", p, line(20))
		}
		if a.Cursor() != line(20)+528 || a.Limit() != line(256) {
			t.Fatalf("cursor %#x limit %#x, want %#x %#x", a.Cursor(), a.Limit(), line(20)+528, line(256))
		}
		if p := a.Alloc(16); p != line(20)+528 {
			t.Fatalf("Alloc(16) after one-over = %#x, want %#x", p, line(20)+528)
		}
	})

	t.Run("RoundedFit", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		if p := a.Alloc(497); p != line(10) || a.Cursor() != line(14) {
			t.Fatalf("Alloc(497) = %#x cursor %#x, want %#x %#x", p, a.Cursor(), line(10), line(14))
		}
	})

	t.Run("SkipsSmallHoles", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		if p := a.Alloc(8 * immixLine); p != line(20) {
			t.Fatalf("Alloc(1024) = %#x, want line 20 %#x", p, line(20))
		}
	})

	t.Run("StaleHoleIsAbandoned", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		a.Alloc(16)
		if p := a.Alloc(4 * immixLine); p != line(20) {
			t.Fatalf("Alloc(512) after a partial hole = %#x, want line 20 %#x", p, line(20))
		}
	})

	t.Run("FullyLive", func(t *testing.T) {
		immixPattern(b, [2]int{0, immixLines})
		a.Reset(b)
		for _, size := range []uintptr{1, 16, immixLine, immixBlock} {
			if p := a.Alloc(size); p != 0 {
				t.Fatalf("Alloc(%d) on a fully live block = %#x, want 0", size, p)
			}
		}
		if a.Cursor() != 0 || a.Limit() != 0 {
			t.Fatalf("cursor %#x limit %#x after failures, want zeros", a.Cursor(), a.Limit())
		}
	})

	t.Run("LastLineOnly", func(t *testing.T) {
		immixPattern(b, [2]int{0, immixLines - 1})
		a.Reset(b)
		if p := a.Alloc(immixLine + 1); p != 0 {
			t.Fatalf("Alloc(129) with one free line = %#x, want 0", p)
		}
		if p := a.Alloc(immixLine); p != line(255) {
			t.Fatalf("Alloc(128) = %#x, want line 255 %#x", p, line(255))
		}
		if a.Cursor() != line(256) || a.Limit() != line(256) {
			t.Fatalf("cursor %#x limit %#x, want both %#x", a.Cursor(), a.Limit(), line(256))
		}
	})

	t.Run("FailureKeepsSearchPosition", func(t *testing.T) {
		immixPattern(b, [2]int{0, 10}, [2]int{12, 100}, [2]int{102, immixLines})
		a.Reset(b)
		if p := a.Alloc(3 * immixLine); p != 0 {
			t.Fatalf("Alloc(384) with two holes of 2 lines = %#x, want 0", p)
		}
		if p := a.Alloc(2 * immixLine); p != line(10) {
			t.Fatalf("Alloc(256) after a failed medium request = %#x, want line 10 %#x", p, line(10))
		}
	})

	t.Run("ResetRestartsSearch", func(t *testing.T) {
		immixHolePattern(b)
		a.Reset(b)
		a.Alloc(4 * immixLine)
		a.Alloc(16)
		a.Reset(b)
		if a.Cursor() != 0 || a.Limit() != 0 || a.Next() != 0 {
			t.Fatalf("Reset left cursor %#x limit %#x next %d", a.Cursor(), a.Limit(), a.Next())
		}
		if p := a.Alloc(16); p != line(10) {
			t.Fatalf("Alloc(16) after Reset = %#x, want line 10 %#x", p, line(10))
		}
	})
}

// TestLogosImmixStoreBarrier checks that the production barrier marks only
// its owning line in the active dirty parity table (atomic OR + bounded
// enlist) and never marks the off-heap line header. It forces the cached
// immix mode to exercise the real production gate (one byte test), not a
// low-level marker bypass; lines must stay clear because the barrier no
// longer writes the recompute view.
func TestLogosImmixStoreBarrier(t *testing.T) {
	if !runtime.LogosImmixMovingExperiment() {
		t.Skip("logos moving experiment off; production barrier correctly disabled")
	}
	prev := runtime.LogosImmixForceOnForTest()
	defer runtime.LogosImmixRestoreOnForTest(prev)
	base := runtime.LogosImmixNewBlocks(1)
	b := runtime.LogosImmixBlockFor(base)
	b.ClearLines()
	b.ClearDirtyAll()
	dst := base + 3*immixLine + 17
	runtime.LogosImmixStoreBarrier(dst, unsafe.Sizeof(uintptr(0)))
	dirty := b.DirtyActive()
	for i, mark := range *dirty {
		if want := i == 3; (mark != 0) != want {
			t.Fatalf("dirty line %d = %d, want marked=%v", i, mark, want)
		}
	}
	if *b.Lines() != (runtime.LogosLineMark{}) {
		t.Fatal("production barrier wrote legacy lines; it must write dirty parity only")
	}
	b.ClearDirtyAll()
	runtime.LogosImmixStoreBarrier(b.Addr(), unsafe.Sizeof(uintptr(0)))
	if *b.DirtyActive() != (runtime.LogosLineMark{}) {
		t.Fatal("store barrier marked dirty for its off-heap header destination")
	}
	if *b.Lines() != (runtime.LogosLineMark{}) {
		t.Fatal("store barrier marked lines for its off-heap header destination")
	}
	if runtime.LogosImmixInHeap(b.Addr()) {
		t.Fatalf("off-heap line header %#x is classified as heap", b.Addr())
	}
}

// TestLogosImmixLineMarks checks markRange, clearLines, and freeLines.
func TestLogosImmixLineMarks(t *testing.T) {
	b := runtime.LogosImmixNewBlock()
	base := b.Base()
	end := base + immixBlock
	cases := []struct {
		name   string
		lo, hi uintptr
		first  int
		last   int
	}{
		{"InsideOneLine", base + 130, base + 140, 1, 1},
		{"CrossesLines", base + 130, base + 260, 1, 2},
		{"HiOnBoundary", base + 0, base + 256, 0, 1},
		{"HiOneOverBoundary", base + 0, base + 257, 0, 2},
		{"SingleByte", base + 255, base + 256, 1, 1},
		{"WholeBlock", base, end, 0, 255},
		{"ClipLow", base - 100, base + 1, 0, 0},
		{"ClipHigh", end - 1, end + 4000, 255, 255},
		{"ClipBoth", base - 5, end + 5, 0, 255},
		{"Empty", base + 200, base + 200, 1, 0},
		{"Inverted", base + 300, base + 200, 1, 0},
		{"BelowBlock", base - 4000, base, 1, 0},
		{"AboveBlock", end, end + 4000, 1, 0},
	}
	for _, c := range cases {
		b.ClearLines()
		b.MarkRange(c.lo, c.hi)
		lines := b.Lines()
		marked := 0
		for i, v := range lines {
			want := c.first <= i && i <= c.last
			if (v != 0) != want {
				t.Fatalf("%s: line %d = %d, want marked=%v", c.name, i, v, want)
			}
			if v != 0 {
				marked++
			}
		}
		if got := b.FreeLines(); got != immixLines-marked {
			t.Fatalf("%s: FreeLines = %d, want %d", c.name, got, immixLines-marked)
		}
	}
	b.MarkRange(base, end)
	if b.FreeLines() != 0 {
		t.Fatalf("FreeLines of a fully marked block = %d, want 0", b.FreeLines())
	}
	b.ClearLines()
	if b.FreeLines() != immixLines {
		t.Fatalf("FreeLines after ClearLines = %d, want %d", b.FreeLines(), immixLines)
	}
	if lines := b.Lines(); *lines != (runtime.LogosLineMark{}) {
		t.Fatal("ClearLines left a nonzero line byte")
	}
}

// TestLogosImmixFindHoleTable checks findHole on hand-built layouts against
// the reference scan, through the dispatching, Go, and AVX2 paths.
func TestLogosImmixFindHoleTable(t *testing.T) {
	type table struct {
		name string
		live [][2]int
	}
	tables := []table{
		{"AllFree", nil},
		{"AllLive", [][2]int{{0, immixLines}}},
		{"OnlyLine0Free", [][2]int{{1, immixLines}}},
		{"OnlyLine255Free", [][2]int{{0, 255}}},
		{"OnlyLine63Free", [][2]int{{0, 63}, {64, immixLines}}},
		{"OnlyLine64Free", [][2]int{{0, 64}, {65, immixLines}}},
		{"HoleSpansWordBoundary", [][2]int{{0, 60}, {70, immixLines}}},
		{"HoleSpansAllWords", [][2]int{{0, 1}, {255, immixLines}}},
		{"PlanLayout", [][2]int{{0, 10}, {14, 20}}},
	}
	b := runtime.LogosImmixNewBlock()
	for _, tc := range tables {
		immixPattern(b, tc.live...)
		lm := b.Lines()
		for _, f := range immixFinders() {
			for from := -2; from < immixLines+3; from++ {
				ws, we, wok := immixRefHole(lm, from)
				gs, ge, gok := f.find(lm, from)
				if gs != ws || ge != we || gok != wok {
					t.Fatalf("%s/%s from %d: got (%d,%d,%v), want (%d,%d,%v)", tc.name, f.name, from, gs, ge, gok, ws, we, wok)
				}
			}
		}
		if got, want := b.FreeLines(), immixRefFree(lm); got != want {
			t.Fatalf("%s: FreeLines = %d, want %d", tc.name, got, want)
		}
	}
}

// TestLogosImmixFindHoleFromInsideHole checks that a search that starts in
// the middle of a hole reports start == from.
func TestLogosImmixFindHoleFromInsideHole(t *testing.T) {
	b := runtime.LogosImmixNewBlock()
	immixHolePattern(b)
	lm := b.Lines()
	for _, f := range immixFinders() {
		for from := 10; from < 14; from++ {
			if s, e, ok := f.find(lm, from); !ok || s != from || e != 14 {
				t.Fatalf("%s from %d: got (%d,%d,%v), want (%d,14,true)", f.name, from, s, e, ok, from)
			}
		}
		if s, e, ok := f.find(lm, 14); !ok || s != 20 || e != immixLines {
			t.Fatalf("%s from 14: got (%d,%d,%v), want (20,256,true)", f.name, s, e, ok)
		}
	}
}

// TestLogosImmixFindHoleRandom compares the AVX2 path, the Go fallback, and
// the reference over 1000 random layouts with every nonzero byte value.
func TestLogosImmixFindHoleRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 8))
	densities := []float64{0.02, 0.2, 0.5, 0.8, 0.98}
	avx2 := runtime.LogosImmixHasAVX2()
	b := runtime.LogosImmixNewBlock()
	for n := range 1000 {
		lm := b.Lines()
		density := densities[n%len(densities)]
		for i := range lm {
			lm[i] = 0
			if rng.Float64() < density {
				lm[i] = uint8(1 + rng.IntN(255))
			}
		}
		if got, want := b.FreeLines(), immixRefFree(lm); got != want {
			t.Fatalf("pattern %d: FreeLines = %d, want %d", n, got, want)
		}
		for from := range immixLines {
			ws, we, wok := immixRefHole(lm, from)
			gs, ge, gok := lm.FindHoleGo(from)
			if gs != ws || ge != we || gok != wok {
				t.Fatalf("pattern %d from %d: Go (%d,%d,%v), want (%d,%d,%v)", n, from, gs, ge, gok, ws, we, wok)
			}
			ds, de, dok := lm.FindHole(from)
			if ds != ws || de != we || dok != wok {
				t.Fatalf("pattern %d from %d: dispatch (%d,%d,%v), want (%d,%d,%v)", n, from, ds, de, dok, ws, we, wok)
			}
			if !avx2 {
				continue
			}
			as, ae, aok := lm.FindHoleAVX2(from)
			if as != ws || ae != we || aok != wok {
				t.Fatalf("pattern %d from %d: AVX2 (%d,%d,%v), want (%d,%d,%v)", n, from, as, ae, aok, ws, we, wok)
			}
		}
	}
}

// TestLogosImmixFindHoleByteValues checks that every nonzero byte value
// counts as live, including values with only the high bit or only bit 0 set.
func TestLogosImmixFindHoleByteValues(t *testing.T) {
	b := runtime.LogosImmixNewBlock()
	lm := b.Lines()
	finders := immixFinders()
	for v := 1; v < 256; v++ {
		for pos := 0; pos < immixLines; pos += 17 {
			*lm = runtime.LogosLineMark{}
			lm[pos] = uint8(v)
			wantFree := immixLines - 1
			if got := b.FreeLines(); got != wantFree {
				t.Fatalf("byte %#x at %d: FreeLines = %d, want %d", v, pos, got, wantFree)
			}
			if pos+1 == immixLines {
				continue
			}
			for _, f := range finders {
				if s, e, ok := f.find(lm, pos); !ok || s != pos+1 {
					t.Fatalf("byte %#x at %d: %s find(%d) = (%d,%d,%v), want start %d", v, pos, f.name, pos, s, e, ok, pos+1)
				}
			}
		}
	}
}

// TestLogosImmixObjdumpHoleScan checks that the AVX2 hole scan is the
// instruction sequence the plan names.
func TestLogosImmixObjdumpHoleScan(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the AVX2 hole scan exists on amd64 only")
	}
	if !cpu.X86.HasAVX2 {
		t.Skip("CPU lacks AVX2")
	}
	testenv.MustHaveGoBuild(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `runtime\.logosHoleScanAVX2`, exe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool objdump: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []struct {
		op    string
		count int
	}{{"VPCMPEQB", 8}, {"VPMOVMSKB", 8}, {"VPXOR", 1}, {"VZEROUPPER", 1}} {
		if got := strings.Count(text, want.op); got != want.count {
			t.Errorf("objdump has %d %s, want %d", got, want.op, want.count)
		}
	}
	if t.Failed() {
		t.Logf("objdump output:\n%s", text)
	}
}

// TestLogosImmixPin checks that pinning sets blockPinned on the block of
// the address and on no other block.
func TestLogosImmixPin(t *testing.T) {
	base := runtime.LogosImmixNewBlocks(3)
	a := runtime.LogosImmixBlockFor(base)
	neighbor := runtime.LogosImmixBlockFor(base + immixBlock)
	if a.Pinned() || neighbor.Pinned() {
		t.Fatal("fresh blocks are pinned")
	}
	if !runtime.LogosImmixPinAddr(base + 5000) {
		t.Fatal("PinAddr of a heap address reported false")
	}
	if !a.Pinned() {
		t.Fatal("PinAddr did not pin the block of the address")
	}
	if neighbor.Pinned() {
		t.Fatal("PinAddr pinned the neighbor block")
	}
	if a.FreeLines() != immixLines {
		t.Fatalf("PinAddr changed the line marks: free %d", a.FreeLines())
	}
	fresh := base + 2*immixBlock
	if runtime.LogosImmixBlockLookup(fresh) != nil {
		t.Fatal("a block that nothing touched has metadata")
	}
	if !runtime.LogosImmixPinAddr(fresh + 1) {
		t.Fatal("PinAddr of an untouched heap block reported false")
	}
	if b := runtime.LogosImmixBlockLookup(fresh); b == nil || !b.Pinned() {
		t.Fatalf("PinAddr did not create a pinned block: %v", b)
	}
	if runtime.LogosImmixBlockFor(base) != a || !a.Pinned() {
		t.Fatal("pin bit lost on a second BlockFor")
	}
	for name, addr := range map[string]uintptr{
		"zero":   0,
		"global": uintptr(unsafe.Pointer(&immixGlobal)),
		"max":    ^uintptr(0),
	} {
		if runtime.LogosImmixPinAddr(addr) {
			t.Fatalf("PinAddr(%s) reported true for an address outside the heap", name)
		}
		if runtime.LogosImmixBlockLookup(addr) != nil {
			t.Fatalf("PinAddr(%s) created metadata", name)
		}
	}
}

// BenchmarkLogosImmixHoleScan reports findHole latency on a block with 64
// alternating holes. The 3 ns target is recorded, not asserted.
func BenchmarkLogosImmixHoleScan(b *testing.B) {
	blk := runtime.LogosImmixNewBlock()
	lm := blk.Lines()
	for i := range lm {
		lm[i] = 0
		if i%4 < 2 {
			lm[i] = 1
		}
	}
	from, sum := 0, 0
	for b.Loop() {
		start, end, ok := lm.FindHole(from)
		if !ok {
			from = 0
			continue
		}
		from = end
		sum += start
	}
	immixSink = sum
}
