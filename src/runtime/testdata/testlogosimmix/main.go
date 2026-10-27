// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command testlogosimmix is the pure-Go program behind
// TestLogosImmixHelperScenarios (M19-S5/S8). It uses only public runtime
// APIs and the addresses of the objects it allocates, so every observation is
// of the live allocator: where objects land, whether freed addresses come
// back, what runtime.MemStats counts, and which objects a collection moves.
//
// LOGOS_IMMIX_EXPECT=1 requires the immix behavior and LOGOS_IMMIX_EXPECT=0
// runs the same program as a control against the plain collector.
// LOGOS_IMMIX_GREENTEA=1 says that the program was built with the Green Tea
// collector, which keeps the inline-mark-bit classes of 512 bytes and less out
// of the immix blocks. The parent starts it with GOMAXPROCS=1, so that no other
// goroutine allocates in the size class under test.
// LOGOS_IMMIX_DIAG=1 prints reuse-scenario retention diagnostics to stderr
// (N, first want/got addresses, BySize deltas, root lengths) without changing
// any assertion, model, collection, or the standard output.
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"unsafe"
)

const (
	blockBytes = 32768
	spanBytes  = 8192
	blobSize   = 1152
	perBlock   = blockBytes / blobSize
)

var (
	immix    = os.Getenv("LOGOS_IMMIX_EXPECT") == "1"
	greentea = os.Getenv("LOGOS_IMMIX_GREENTEA") == "1"
)

// blob is a pointer-free object of exactly 1152 bytes, size class 33, whose
// 32 KiB block holds 28 of them in 252 lines of 128 bytes and a 512-byte
// unused tail.
type blob struct {
	id  uintptr
	pad [blobSize - unsafe.Sizeof(uintptr(0))]byte
}

// sink keeps an allocation on the heap and, with the roots below, reachable.
var (
	sink     unsafe.Pointer
	rootSkip []*blob
	rootAll  []*blob
	rootBigs [][]byte
	rootGrps []*group
)

func fill(b *blob, id uintptr) {
	b.id = id
	for i := range b.pad {
		b.pad[i] = byte(id) + byte(i)
	}
}

func intact(b *blob, id uintptr) bool {
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

func zeroed(b *blob) bool {
	if b.id != 0 {
		return false
	}
	for _, c := range b.pad {
		if c != 0 {
			return false
		}
	}
	return true
}

func addr(b *blob) uintptr { return uintptr(unsafe.Pointer(b)) }

// startAlign is the alignment of the first object of a fresh span: a 32 KiB
// block under immix and an 8 KiB span under the plain collector.
func startAlign() uintptr {
	if immix {
		return blockBytes
	}
	return spanBytes
}

// alignedFor allocates three blocks of objects of type T, which is a
// pointer-free array of the given size in an eligible size class, and checks
// that the first object of a fresh span sits at a 32 KiB boundary, that the
// following objects are exactly size bytes apart, and that the next span
// starts at another boundary.
func alignedFor[T any](size uintptr) error {
	per := blockBytes / size
	objs := make([]*T, 0, 3*per+8)
	var base uintptr
	for tries := uintptr(0); base == 0; tries++ {
		if tries > 4*per+16 {
			return fmt.Errorf("size %d: no object at a block start in %d allocations", size, tries)
		}
		p := new(T)
		sink = unsafe.Pointer(p)
		if uintptr(unsafe.Pointer(p))%blockBytes == 0 {
			objs = append(objs, p)
			base = uintptr(unsafe.Pointer(p))
		}
	}
	for blk := 0; blk < 3; blk++ {
		for k := uintptr(1); k < per; k++ {
			p := new(T)
			sink = unsafe.Pointer(p)
			objs = append(objs, p)
			if got, want := uintptr(unsafe.Pointer(p)), base+k*size; got != want {
				return fmt.Errorf("size %d block %d object %d at %#x, want %#x (stride %d from the block start %#x)", size, blk, k, got, want, size, base)
			}
		}
		if blk == 2 {
			break
		}
		p := new(T)
		sink = unsafe.Pointer(p)
		objs = append(objs, p)
		next := uintptr(unsafe.Pointer(p))
		if next%blockBytes != 0 || next == base {
			return fmt.Errorf("size %d: the object after block %d is at %#x, want a new 32 KiB boundary (block %#x)", size, blk, next, base)
		}
		base = next
	}
	runtime.KeepAlive(objs)
	return nil
}

func aligned() error {
	if !immix {
		return alignedPlainControl()
	}
	for _, f := range []func() error{
		func() error { return alignedFor[[1152]byte](1152) },
		func() error { return alignedFor[[576]byte](576) },
		func() error { return alignedFor[[4096]byte](4096) },
		func() error { return alignedFor[[8192]byte](8192) },
	} {
		if err := f(); err != nil {
			return err
		}
	}
	if !greentea {
		// Without Green Tea the small classes are served from blocks too.
		for _, f := range []func() error{
			func() error { return alignedFor[[24]byte](24) },
			func() error { return alignedFor[[112]byte](112) },
			func() error { return alignedFor[[512]byte](512) },
		} {
			if err := f(); err != nil {
				return err
			}
		}
	}
	return nil
}

// alignedPlainControl shows that the block placement above is the immix
// signature: under the plain collector 1152-byte objects live in 8 KiB spans,
// so their offsets in a 32 KiB window are not multiples of the object size.
func alignedPlainControl() error {
	objs := make([]*blob, 0, 128)
	bad := 0
	for range 112 {
		p := new(blob)
		sink = unsafe.Pointer(p)
		objs = append(objs, p)
		if (addr(p)%blockBytes)%blobSize != 0 {
			bad++
		}
	}
	runtime.KeepAlive(objs)
	if bad == 0 {
		return fmt.Errorf("every object of the plain collector sits at a block slot offset: the control cannot tell the collectors apart")
	}
	return nil
}

// findStart allocates blobs until one sits where a fresh span begins, and
// returns it.
func findStart() (*blob, error) {
	skipped := make([]*blob, 0, 4*perBlock+16)
	for tries := 0; tries < 4*perBlock+16; tries++ {
		p := new(blob)
		sink = unsafe.Pointer(p)
		if addr(p)%startAlign() == 0 {
			rootSkip = skipped
			return p, nil
		}
		skipped = append(skipped, p)
	}
	rootSkip = skipped
	return nil, fmt.Errorf("no blob at a span start")
}

func diagOn() bool {
	v := os.Getenv("LOGOS_IMMIX_DIAG")
	return v != "" && v != "0"
}

// diagf uses stderr so stdout still ends with exactly "OK".
func diagf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "logosimmix-diag: "+format+"\n", args...)
}

func blobSizeClass() int {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	for i, b := range ms.BySize {
		if uintptr(b.Size) == blobSize {
			return i
		}
	}
	return -1
}

// reuse fills two blocks (eight spans under the plain collector), drops every
// fourth object, collects, and checks that the next allocations of the class
// are served from the spans that held the dropped objects, zeroed, next to
// survivors that kept their contents. Survivors in dense blocks must keep
// their addresses; survivors in sparse-tail blocks may move or stay because
// evacuating a sparse block is legal. Dense means live lines above half the
// block (the production threshold freeLines <= 128): with 1152-byte objects
// of 9 lines each that is 15 or more survivors in a 28-slot block.
func reuse() error {
	debug.SetGCPercent(-1)
	const total = 2 * perBlock
	diag := diagOn()
	sc := blobSizeClass()
	if sc < 0 {
		return fmt.Errorf("no size class of %d bytes", blobSize)
	}
	var pre, post runtime.MemStats
	objs := make([]*blob, 0, total)
	dropped := make([]uintptr, 0, total)
	runtime.GC()
	runtime.GC()
	first, err := findStart()
	if err != nil {
		return err
	}
	objs = append(objs, first)
	for len(objs) < total {
		p := new(blob)
		sink = unsafe.Pointer(p)
		objs = append(objs, p)
	}
	rootAll = objs
	for i, p := range objs {
		fill(p, uintptr(i+1))
	}
	before := make([]uintptr, total)
	for i, p := range objs {
		before[i] = addr(p)
		if i%4 == 3 {
			dropped = append(dropped, addr(p))
			objs[i] = nil
		}
	}
	// The last bump allocation left sink pointing at objs[total-1], which is
	// dropped (total-1 is 55, and 55%4 == 3). Clearing sink before the
	// collection keeps every dropped slot freeable; otherwise the sink-held
	// slot would survive and only len(dropped)-1 slots could be reused.
	sink = nil
	align := startAlign()
	mask := align - 1
	droppedSpans := make(map[uintptr]bool, len(dropped))
	for _, a := range dropped {
		droppedSpans[a&^mask] = true
	}
	runtime.ReadMemStats(&pre)
	if diag {
		preM, preF := pre.BySize[sc].Mallocs, pre.BySize[sc].Frees
		diagf("reuse pre N=%d dropped=%d droppedSpans=%d class=%d size=%d Mallocs=%d Frees=%d rootSkipLen=%d rootSkipKept=%v rootAllLen=%d",
			total, len(dropped), len(droppedSpans), sc, blobSize, preM, preF, len(rootSkip), len(rootSkip) > 0, len(rootAll))
	}
	runtime.GC()
	// The post-GC baseline is read after the collection, not before: it is the
	// baseline for the refill that follows and the endpoint for the sweep
	// delta. Reading it before the GC would mix the sweep's frees into the
	// refill's mallocs.
	runtime.ReadMemStats(&post)
	if mal, fre := delta(sc, &pre, &post); mal != 0 || fre != uint64(len(dropped)) {
		return fmt.Errorf("sweep of %d dropped objects reports mallocs delta=%d frees delta=%d, want 0 and %d", len(dropped), mal, fre, len(dropped))
	}
	if diag {
		preM, preF := pre.BySize[sc].Mallocs, pre.BySize[sc].Frees
		postM, postF := post.BySize[sc].Mallocs, post.BySize[sc].Frees
		diagf("reuse post N=%d dropped=%d droppedSpans=%d class=%d Mallocs=%d->%d Frees=%d->%d dF=%d wantDropped=%d rootSkipKept=%v",
			total, len(dropped), len(droppedSpans), sc, preM, postM, preF, postF, postF-preF, len(dropped), len(rootSkip) > 0)
	}
	if !immix {
		for i, p := range objs {
			if p == nil {
				continue
			}
			if addr(p) != before[i] {
				return fmt.Errorf("survivor %d moved from %#x to %#x under the plain collector, which never moves objects", i, before[i], addr(p))
			}
			if !intact(p, uintptr(i+1)) {
				return fmt.Errorf("survivor %d lost its contents across the collection", i)
			}
		}
	} else {
		bmask := uintptr(blockBytes) - 1
		byBlock := make(map[uintptr][]int, 4)
		for i := range objs {
			byBlock[before[i]&^bmask] = append(byBlock[before[i]&^bmask], i)
		}
		for base, idxs := range byBlock {
			live := 0
			for _, i := range idxs {
				if objs[i] != nil {
					live++
				}
			}
			liveLines := live * int(blobSize/128)
			dense := liveLines > 256/2
			if diag {
				diagf("reuse block %#x slots=%d live=%d liveLines=%d dense=%v", base, len(idxs), live, liveLines, dense)
			}
			if !dense {
				continue
			}
			for _, i := range idxs {
				if objs[i] == nil {
					continue
				}
				if addr(objs[i]) != before[i] {
					return fmt.Errorf("survivor %d moved from %#x to %#x although its block %#x holds %d survivors (%d live lines, dense above half of 256)", i, before[i], addr(objs[i]), base, live, liveLines)
				}
			}
		}
		for i, p := range objs {
			if p == nil {
				continue
			}
			if !intact(p, uintptr(i+1)) {
				return fmt.Errorf("survivor %d lost its contents across the collection", i)
			}
		}
	}
	fresh := make([]*blob, 0, len(dropped))
	seen := make(map[uintptr]bool, len(dropped))
	for range dropped {
		p := new(blob)
		sink = unsafe.Pointer(p)
		fresh = append(fresh, p)
		a := addr(p)
		// Only immix promises hole-driven reuse: the plain collector may
		// serve an allocation from any span its lazy sweeping has not
		// reached, so span membership is an immix-only assertion.
		if immix && !droppedSpans[a&^mask] {
			return fmt.Errorf("allocation after the sweep landed at %#x in span %#x, which held no dropped object (dropped spans %d)", a, a&^mask, len(droppedSpans))
		}
		if seen[a] {
			return fmt.Errorf("freed address %#x was handed out twice", a)
		}
		seen[a] = true
		if !zeroed(p) {
			return fmt.Errorf("the object reused at %#x was not zeroed", a)
		}
	}
	if diag {
		diagf("reuse refill N=%d refilled=%d droppedSpans=%d distinctAddrs=%d allInDroppedSpans=true", total, len(fresh), len(droppedSpans), len(seen))
	}
	var afterRefill runtime.MemStats
	runtime.ReadMemStats(&afterRefill)
	if mal, fre := delta(sc, &post, &afterRefill); mal != uint64(len(dropped)) || fre != 0 {
		return fmt.Errorf("refill of %d slots reports mallocs delta=%d frees delta=%d, want %d and 0", len(dropped), mal, fre, len(dropped))
	}
	for i, p := range objs {
		if p != nil && !intact(p, uintptr(i+1)) {
			return fmt.Errorf("survivor %d changed while the freed slots were reused", i)
		}
	}
	runtime.KeepAlive(fresh)
	return nil
}

func delta(sc int, a, b *runtime.MemStats) (mallocs, frees uint64) {
	return b.BySize[sc].Mallocs - a.BySize[sc].Mallocs, b.BySize[sc].Frees - a.BySize[sc].Frees
}

// counters checks the exact per-class counters of runtime.MemStats through
// allocation, a sweep, reuse of the freed slots, and a final sweep. The
// program is the only allocator in the class, so every delta is exact.
func counters() error {
	debug.SetGCPercent(-1)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sc := -1
	for i, b := range ms.BySize {
		if uintptr(b.Size) == blobSize {
			sc = i
		}
	}
	if sc < 0 {
		return fmt.Errorf("no size class of %d bytes", blobSize)
	}
	n := 3*perBlock + 5
	keep := make([]*blob, n)
	rootAll = keep
	runtime.GC()
	runtime.GC()
	var m0, m1, m2, m3, m4 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for i := range keep {
		keep[i] = new(blob)
		fill(keep[i], uintptr(i+1))
	}
	runtime.ReadMemStats(&m1)
	if mal, fre := delta(sc, &m0, &m1); mal != uint64(n) || fre != 0 {
		return fmt.Errorf("after %d allocations BySize reports mallocs=%d frees=%d, want %d and 0", n, mal, fre, n)
	}
	dropped := 0
	for i := 0; i < n; i += 2 {
		keep[i] = nil
		dropped++
	}
	runtime.GC()
	runtime.ReadMemStats(&m2)
	if mal, fre := delta(sc, &m0, &m2); mal != uint64(n) || fre != uint64(dropped) {
		return fmt.Errorf("after dropping %d and a collection BySize reports mallocs=%d frees=%d, want %d and %d", dropped, mal, fre, n, dropped)
	}
	reused := dropped / 2
	for i, k := 0, 0; i < reused; i, k = i+1, k+2 {
		keep[k] = new(blob)
	}
	runtime.ReadMemStats(&m3)
	if mal, fre := delta(sc, &m0, &m3); mal != uint64(n+reused) || fre != uint64(dropped) {
		return fmt.Errorf("after reusing %d slots BySize reports mallocs=%d frees=%d, want %d and %d", reused, mal, fre, n+reused, dropped)
	}
	for i, p := range keep {
		if p != nil && p.id != 0 && !intact(p, uintptr(i+1)) {
			return fmt.Errorf("survivor %d lost its contents", i)
		}
	}
	clear(keep)
	runtime.GC()
	runtime.ReadMemStats(&m4)
	if mal, fre := delta(sc, &m0, &m4); mal != uint64(n+reused) || fre != uint64(n+reused) {
		return fmt.Errorf("after dropping everything BySize reports mallocs=%d frees=%d, want %d for both", mal, fre, n+reused)
	}
	return nil
}

// group is a set of survivors that share one exclusion property.
type group struct {
	name   string
	ptrs   []*blob
	ids    []uintptr
	before []uintptr
}

func newGroup(name string, all []*blob, block int, keep func(k int) bool) *group {
	g := &group{name: name}
	for k := range perBlock {
		i := block*perBlock + k
		if keep(k) {
			g.ptrs = append(g.ptrs, all[i])
			g.ids = append(g.ids, uintptr(i+1))
			fill(all[i], uintptr(i+1))
		}
	}
	return g
}

// layout checks that block b of all holds perBlock objects one slot apart
// from a 32 KiB boundary, which is how immix hands out a fresh span.
func layout(all []*blob) error {
	for b := range len(all) / perBlock {
		base := addr(all[b*perBlock])
		if base%blockBytes != 0 {
			return fmt.Errorf("block %d starts at %#x, not at a 32 KiB boundary", b, base)
		}
		for k := range perBlock {
			if got, want := addr(all[b*perBlock+k]), base+uintptr(k)*blobSize; got != want {
				return fmt.Errorf("block %d object %d at %#x, want %#x", b, k, got, want)
			}
		}
	}
	return nil
}

// freshStart scans tmp for the first window of blocks consecutive fresh
// spans: every block starts at a 32 KiB boundary and its perBlock objects
// follow one slot apart. A reused swept span still holds survivors, so its
// free slots are never stride-contiguous from its base and any window
// overlapping it fails here; only a span that started empty serves perBlock
// consecutive slots from its base. The caller grows tmp until this window
// exists (bounded below), so the partial spans left by baseline class use
// drain first and the verified run is fresh spans back to back.
func freshStart(tmp []*blob, blocks int) (int, error) {
	need := blocks * perBlock
	for start := 0; start+need <= len(tmp); start++ {
		if addr(tmp[start])%blockBytes != 0 {
			continue
		}
		ok := true
		for b := 0; b < blocks && ok; b++ {
			base := addr(tmp[start+b*perBlock])
			if base%blockBytes != 0 {
				ok = false
				break
			}
			for k := range perBlock {
				if addr(tmp[start+b*perBlock+k]) != base+uintptr(k)*blobSize {
					ok = false
					break
				}
			}
		}
		if ok {
			return start, nil
		}
	}
	return -1, fmt.Errorf("no %d consecutive fresh spans in %d allocations", blocks, len(tmp))
}

func (g *group) record() {
	g.before = g.before[:0]
	for _, p := range g.ptrs {
		g.before = append(g.before, addr(p))
	}
}

func (g *group) moved() int {
	n := 0
	for i, p := range g.ptrs {
		if addr(p) != g.before[i] {
			n++
		}
	}
	return n
}

func (g *group) check(name string, wantMoved func(moved, total int) bool, why string) error {
	m := g.moved()
	if !wantMoved(m, len(g.ptrs)) {
		return fmt.Errorf("%s: %s group moved %d of %d survivors: %s", name, g.name, m, len(g.ptrs), why)
	}
	return nil
}

func none(moved, total int) bool { return moved == 0 }
func most(moved, total int) bool { return moved*2 >= total }

func holds(f func(moved, total int) bool) func(moved, total int) bool {
	if immix {
		return f
	}
	return none
}

// exclusions compares survivors of sparse blocks that nothing holds in place
// with survivors of blocks that a finalizer, a cleanup, a pin, or their size
// excludes from relocation, and with survivors of dense blocks. Under immix
// only the quiet group may move, and the others may move only once the pin
// is gone. Under the plain collector nothing ever moves.
func exclusions() error {
	debug.SetGCPercent(-1)
	const blocks = 8
	rootBigs = make([][]byte, 3)
	bigAddr := make([]uintptr, len(rootBigs))
	for i := range rootBigs {
		rootBigs[i] = make([]byte, 40000)
		rootBigs[i][0] = byte(i + 1)
		bigAddr[i] = uintptr(unsafe.Pointer(&rootBigs[i][0]))
	}
	// Bound the hunt for a fresh window: each baseline partial span
	// contributes fewer than perBlock allocations before the first fresh
	// span, so room for 16 drained spans plus slack fails fast instead of
	// growing without bound. GOMAXPROCS=1 with the GC disabled keeps
	// allocation order span-major, so the window, once present, is fresh
	// spans back to back.
	tmp := make([]*blob, 0, blocks*perBlock+16*perBlock+64)
	runtime.GC()
	runtime.GC()
	for len(tmp) < blocks*perBlock+4*perBlock+16 {
		p := new(blob)
		sink = unsafe.Pointer(p)
		tmp = append(tmp, p)
	}
	start := -1
	if immix {
		for {
			s, ferr := freshStart(tmp, blocks)
			if ferr == nil {
				start = s
				break
			}
			if len(tmp) >= cap(tmp) {
				return ferr
			}
			for i := 0; i < perBlock && len(tmp) < cap(tmp); i++ {
				p := new(blob)
				sink = unsafe.Pointer(p)
				tmp = append(tmp, p)
			}
		}
	} else {
		for i, p := range tmp {
			if addr(p)%startAlign() == 0 {
				start = i
				break
			}
		}
		if start < 0 {
			return fmt.Errorf("no blob at a span start in %d allocations", len(tmp))
		}
		if start+blocks*perBlock > len(tmp) {
			return fmt.Errorf("only %d slots after the first span start, want %d", len(tmp)-start, blocks*perBlock)
		}
	}
	all := tmp[start : start+blocks*perBlock]
	rootSkip = tmp[:start]
	clear(tmp[start+blocks*perBlock:])
	if immix {
		if err := layout(all); err != nil {
			return err
		}
	}
	sparse := func(k int) bool { return k%10 == 0 }
	grps := make([]*group, 0, 8)
	dense := func(k int) bool { return k%2 == 0 || k == perBlock-1 }
	host0 := newGroup("host0", all, 0, dense)
	host1 := newGroup("host1", all, 1, dense)
	quiet0 := newGroup("quiet0", all, 2, sparse)
	quiet1 := newGroup("quiet1", all, 3, sparse)
	fin := newGroup("finalizer", all, 4, sparse)
	cleanup := newGroup("cleanup", all, 5, sparse)
	pinned := newGroup("pinned", all, 6, sparse)
	grps = append(grps, host0, host1, quiet0, quiet1, fin, cleanup, pinned)
	rootGrps = grps
	clear(all)
	all = nil
	tmp = nil
	sink = nil
	for _, g := range grps {
		g.record()
	}
	runtime.SetFinalizer(fin.ptrs[1], func(*blob) {})
	cl := runtime.AddCleanup(cleanup.ptrs[1], func(int) {}, 0)
	var pin runtime.Pinner
	pin.Pin(pinned.ptrs[1])

	check := func(step string) error {
		for _, g := range grps {
			for i, p := range g.ptrs {
				if !intact(p, g.ids[i]) {
					return fmt.Errorf("%s: %s survivor %d lost its contents", step, g.name, i)
				}
			}
		}
		for i := range rootBigs {
			if addr2(rootBigs[i]) != bigAddr[i] || rootBigs[i][0] != byte(i+1) {
				return fmt.Errorf("%s: large object %d moved from %#x to %#x or lost its contents", step, i, bigAddr[i], addr2(rootBigs[i]))
			}
		}
		return nil
	}

	runtime.GC()
	if err := check("first collection"); err != nil {
		return err
	}
	for _, c := range []struct {
		g    *group
		want func(moved, total int) bool
		why  string
	}{
		{host0, none, "a dense block is not evacuated"},
		{host1, none, "a dense block is not evacuated"},
		{quiet0, holds(most), "a sparse block with no exclusion is evacuated"},
		{quiet1, holds(most), "a sparse block with no exclusion is evacuated"},
		{fin, none, "a span with a finalizer never relocates"},
		{cleanup, none, "a span with a cleanup never relocates"},
		{pinned, none, "a pinned object pins its block"},
	} {
		if err := c.g.check("first collection", c.want, c.why); err != nil {
			return err
		}
	}

	pin.Unpin()
	pinned.record()
	runtime.GC()
	if err := check("after unpin"); err != nil {
		return err
	}
	if err := pinned.check("after unpin", holds(most), "an unpinned sparse block is evacuated"); err != nil {
		return err
	}
	for _, g := range []*group{fin, cleanup} {
		if err := g.check("after unpin", none, "the exclusion of a finalizer or cleanup is sticky"); err != nil {
			return err
		}
	}

	runtime.SetFinalizer(fin.ptrs[1], nil)
	cl.Stop()
	runtime.GC()
	for _, g := range []*group{fin, cleanup} {
		if err := g.check("after removal", none, "removing a finalizer or stopping a cleanup does not clear the exclusion"); err != nil {
			return err
		}
	}
	if err := check("after removal"); err != nil {
		return err
	}
	runtime.KeepAlive(pin)
	return nil
}

func addr2(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

var scenarios = map[string]func() error{
	"aligned":    aligned,
	"reuse":      reuse,
	"counters":   counters,
	"exclusions": exclusions,
}

func main() {
	run := scenarios[os.Args[len(os.Args)-1]]
	if run == nil {
		fmt.Fprintln(os.Stderr, "usage: testlogosimmix scenario")
		os.Exit(2)
	}
	debug.SetGCPercent(-1)
	if err := run(); err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}
