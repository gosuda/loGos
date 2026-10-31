// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license for the specific language governing permissions and limitations.

package runtime_test

import (
	"fmt"
	"internal/testenv"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// Deterministic cleanup-evacuation coverage for the immix collector.
//
// The scenario binary can only shape the heap from user space, so its
// per-object assertions are best-effort. This test covers the same
// acceptance row from inside the test binary using the production
// fixture's deterministic mechanism
// (logos_immix_production_test.go:immixProdExclusions): eligible-class
// spans are whole 32 KiB blocks, so a span owns its block exclusively and
// region blocks are pure by construction. Cleanup arguments are
// pointer-bearing 1152-byte nodes in fillBlocks-verified sparse
// block-spans (4 of 28 slots live, poisoned spans skipped), so relocation
// must rewrite their pointer masks as well as move bytes; closures stay
// 64-bit class and cluster in fresh spans topped with dead filler,
// verified free of sticky exclusions. A bounded retry loop absorbs
// transient refusals (conservative pins, sweep timing, destination
// pressure); the loop exits only once every argument and closure has
// relocated, so the per-object assertions below are exact. Exhausting the
// loop fails honestly with per-object span and block state.
//
// It needs the same immix test binary as TestLogosImmixProduction and runs
// the scenario in a child copy with GOMAXPROCS(1) and GC percent -1, so no
// other goroutine allocates in the classes under test and no background
// cycle starts. Waits use wall-clock deadlines, never timers, so no timer
// goroutine perturbs the pin set.

const immixCleanupChildEnv = "TEST_LOGOS_IMMIX_CLEANUP_CHILD"

type immixCleanupNode struct {
	next *immixCleanupNode
	v    int
	pad  [5]uintptr
}

// immixCleanupArg is a pointer-bearing 1152-byte cleanup argument: the
// same size class as immixProdBlob, so its spans are whole 32 KiB blocks,
// but scan, so relocation must rewrite its pointer mask as well as move
// its bytes. Arguments form a ring through next; the drain checks every
// link observes the current object.
type immixCleanupArg struct {
	next *immixCleanupArg
	id   uintptr
	// Sized so the object plus its malloc header fills exactly the
	// 1152-byte size class.
	pad [1152 - 3*unsafe.Sizeof(uintptr(0))]byte
}

func immixCleanupArgFill(a *immixCleanupArg, i int) {
	a.id = uintptr(1000000 + i)
	for j := range a.pad {
		a.pad[j] = byte(i) + byte(j)
	}
}

func immixCleanupArgIntact(a *immixCleanupArg, i int, next *immixCleanupArg) bool {
	if a.id != uintptr(1000000+i) || a.next != next {
		return false
	}
	return immixCleanupArgPayload(a, i)
}

func immixCleanupArgPayload(a *immixCleanupArg, i int) bool {
	if a.id != uintptr(1000000+i) {
		return false
	}
	for j := range a.pad {
		if a.pad[j] != byte(i)+byte(j) {
			return false
		}
	}
	return true
}

type immixCleanupHolder struct {
	child *immixCleanupArg
	pad   [100]byte
}

// TestLogosImmixCleanup runs the cleanup-evacuation scenario in a child
// copy of the test binary.
func TestLogosImmixCleanup(t *testing.T) {
	if os.Getenv(immixCleanupChildEnv) != "" {
		immixCleanupChild(t)
		return
	}
	if runtime.GOOS == "js" || runtime.GOOS == "wasip1" {
		t.Skip("no subprocesses")
	}
	immixProdRequireMode(t)
	testenv.MustHaveExec(t)
	cmd := testenv.Command(t, testenv.Executable(t), "-test.run=^TestLogosImmixCleanup$", "-test.v")
	cmd.Env = append(os.Environ(), immixCleanupChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestLogosImmixCleanup (") || strings.Contains(string(out), "--- SKIP") {
		t.Fatalf("%v\n%s", out, err)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

func immixCleanupChild(t *testing.T) {
	immixProdRequireMode(t)
	// Explicit restores, not defers: defer records pin their funcvals
	// for the life of the child, and the pin pass retains a granule
	// naming one every cycle.
	prevMaxProcs := runtime.GOMAXPROCS(1)
	prevGCPercent := debug.SetGCPercent(-1)
	immixCleanupEvacuation(t)
	runtime.GOMAXPROCS(prevMaxProcs)
	debug.SetGCPercent(prevGCPercent)
}

func immixCleanupLogBlock(t *testing.T, bb uintptr, mine map[uintptr]int) {
	t.Helper()
	for pg := uintptr(0); pg < immixProdBlock; pg += immixProdPage {
		st, ok := runtime.LogosImmixSpanOf(unsafe.Pointer(bb + pg))
		if !ok {
			t.Logf("block %#x page +%#x: no in-use span", bb, pg)
			continue
		}
		t.Logf("block %#x page +%#x: span base=%#x size=%d alloc=%d/%d special=%v noreloc=%v pinned=%v cached=%v active=%v mine=%d",
			bb, pg, st.Base, st.ElemSize, st.AllocCount, st.NElems, st.Specials, st.NoReloc, st.Pinned, st.Cached, st.Active, mine[st.Base])
	}
}

func immixCleanupBlockState(addr uintptr) (free int, pinned bool) {
	occ, _, _, pinned, ok := runtime.LogosImmixPlanesAt(addr)
	if !ok {
		return -1, pinned
	}
	for _, l := range occ {
		if l == 0 {
			free++
		}
	}
	return free, pinned
}

func immixCleanupWait(t *testing.T, ch <-chan struct{}, what string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		runtime.GC()
		select {
		case <-ch:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not run", what)
		}
	}
}

func immixCleanupEvacuation(t *testing.T) {
	const n = 32

	gate, blocked := make(chan struct{}), make(chan struct{})
	blocker := new(immixCleanupHolder)
	runtime.AddCleanup(blocker, func(int) {
		close(blocked)
		<-gate
	}, 0)
	blocker = nil
	immixCleanupWait(t, blocked, "blocking cleanup", 30*time.Second)

	// Owners carry the cleanup specials, which exclude their spans from
	// relocation. Holder spans are small and can never share a block
	// owned by a block-span, but they are still kept out of closure
	// blocks below.
	owners := make([]*immixCleanupHolder, n)
	for i := range owners {
		owners[i] = new(immixCleanupHolder)
	}
	// Pad each owner block full with dead same-class holders, so later
	// closure spans cannot land in a block a cleanup special fixes.
	// The mask matches blockOf below; the reservation must precede any
	// 64-bit allocation that could otherwise share these blocks.
	ownerBlocks := map[uintptr]bool{}
	for i := range owners {
		ownerBlocks[uintptr(unsafe.Pointer(owners[i]))&^(immixProdBlock-1)] = true
	}
	var ownerPad []*immixCleanupHolder
	for b := range ownerBlocks {
		for k := 0; k < 10*70; k++ {
			p := new(immixCleanupHolder)
			ownerPad = append(ownerPad, p)
			if uintptr(unsafe.Pointer(p))&^(immixProdBlock-1) != b {
				break
			}
		}
	}

	// Destination reservation: free slots are punched after the region
	// is placed, so the region never lands in destination holes.
	destBig := make([]*immixCleanupArg, 0, 140)
	for range cap(destBig) {
		destBig = append(destBig, new(immixCleanupArg))
	}
	destSmall := make([]*immixCleanupNode, 0, 256)
	for range cap(destSmall) {
		destSmall = append(destSmall, new(immixCleanupNode))
	}
	// Gap pushes closure allocation past the dense dest spans. The
	// pages stay live until the filler is dropped, then go free.
	gap := make([]*[8192]byte, 0, 8)
	for range cap(gap) {
		gap = append(gap, new([8192]byte))
	}

	// Region arguments: exact fresh block-spans via the production
	// helper. Every span is a whole 32 KiB block, so region blocks are
	// pure by construction; the sparse keep leaves them fragmented.
	// Spans carrying a live special from before the fixture are skipped:
	// fillBlocks verifies freshness, not exclusion state.
	blocks := immixProdFillBlocks(t, func() unsafe.Pointer { return unsafe.Pointer(new(immixCleanupArg)) }, immixProdHeader, 12, 28)
	args := make([]*immixCleanupArg, 0, n)
outer:
	for _, block := range blocks {
		if st := immixProdSpan(t, block[0]); st.Specials || st.NoReloc {
			continue
		}
		for k, p := range block {
			if k%7 != 0 {
				continue
			}
			args = append(args, (*immixCleanupArg)(p))
			if len(args) == n {
				break outer
			}
		}
	}
	if len(args) != n {
		t.Fatalf("sparse keep selected %d arguments from clean spans, want %d", len(args), n)
	}
	for i := range args {
		a := args[i]
		immixCleanupArgFill(a, i)
		a.next = args[(i+1)%len(args)]
	}
	for i := range args {
		st := immixProdSpan(t, unsafe.Pointer(args[i]))
		if !st.Active || !st.HasBlock || !st.BlockRegistered || st.ElemSize != 1152 || st.NoReloc || st.Specials {
			t.Fatalf("region argument %d not in an evacuable block-span: %+v", i, st)
		}
	}

	// Closures stay 64-bit class: they cluster in fresh spans topped up
	// with dead filler. Their spans carry no block metadata, so only
	// the sticky exclusions fail the layout fast; density and pins go
	// through the retry loop.
	got := make(chan *immixCleanupArg, n)
	kept := make([]func(*immixCleanupArg), 0, n)
	argAt := make([]uintptr, 0, n)
	clAt := make([]uintptr, 0, n)
	ownerAt := make([]uintptr, 0, n)
	// Dead filler goes first so reused spans are full before any closure
	// allocates: closures land on fresh spans instead of inheriting live
	// init-garbage specials. Dropped before the evacuation GCs; ~40 dead
	// slots per live closure keep closure blocks fragmented.
	var topup []*immixCleanupNode
	for range 40 * n {
		topup = append(topup, new(immixCleanupNode))
	}
	for i := range args {
		a := args[i]
		owners[i].child = a
		ownerAt = append(ownerAt, uintptr(unsafe.Pointer(owners[i])))
		a0, a1, a2, a3, a4 := i, i+1, i+2, i+3, i+4
		cl := func(arg *immixCleanupArg) {
			if a1 != a0+1 || a2 != a0+2 || a3 != a0+3 || a4 != a0+4 || arg != a || !immixCleanupArgPayload(arg, a0) {
				panic("cleanup closure lost a captured value")
			}
			got <- arg
		}
		runtime.AddCleanup(owners[i], cl, a)
		kept = append(kept, cl)
		argAt = append(argAt, uintptr(unsafe.Pointer(a)))
		clAt = append(clAt, *(*uintptr)(unsafe.Pointer(&cl)))
	}
	for i := range kept {
		p := *(*unsafe.Pointer)(unsafe.Pointer(&kept[i]))
		st := immixProdSpan(t, p)
		if st.NoReloc || st.Specials {
			t.Fatalf("region closure %d (%p) not evacuable: %+v", i, p, st)
		}
	}
	closureBlocks := map[uintptr]bool{}
	blockOf := func(p unsafe.Pointer) uintptr {
		return uintptr(p) &^ (immixProdBlock - 1)
	}
	for i := range kept {
		closureBlocks[blockOf(*(*unsafe.Pointer)(unsafe.Pointer(&kept[i])))] = true
	}
	for i := range owners {
		if b := blockOf(unsafe.Pointer(owners[i])); closureBlocks[b] {
			t.Fatalf("owner %d shares closure block %#x: candidates would fix the block", i, b)
		}
	}
	sharedDest := 0
	for i := range destSmall {
		if b := blockOf(unsafe.Pointer(destSmall[i])); closureBlocks[b] {
			sharedDest++
		}
	}
	if sharedDest > 0 {
		t.Logf("%d dest nodes share closure blocks: dense reservation may dilute those blocks", sharedDest)
	}

	clear(owners)
	clear(ownerPad)
	runtime.GC()
	for i := range args {
		if uintptr(unsafe.Pointer(args[i])) != argAt[i] || *(*uintptr)(unsafe.Pointer(&kept[i])) != clAt[i] {
			t.Fatalf("cleanup %d moved before it was queued", i)
		}
	}
	runtime.KeepAlive(blocks)
	blocks = nil
	clear(topup)
	clear(gap)
	for i := range destBig {
		if i%3 == 2 {
			destBig[i] = nil
		}
	}
	for i := range destSmall {
		if i%8 != 0 {
			destSmall[i] = nil
		}
	}

	// Budget discriminator: live objects per size class versus free
	// destination slots. If free slots fall below live-to-move, the
	// stall is destination budget; otherwise it is selection/density.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	freeSmall, freeBig := 0, 0
	for i := range destSmall {
		if destSmall[i] == nil {
			freeSmall++
		}
	}
	for i := range destBig {
		if destBig[i] == nil {
			freeBig++
		}
	}
	for _, bs := range ms.BySize {
		switch bs.Size {
		case 64, 80, 1152:
			live := int64(bs.Mallocs) - int64(bs.Frees)
			t.Logf("class %d: mallocs=%d frees=%d live~%d", bs.Size, bs.Mallocs, bs.Frees, live)
		}
	}
	t.Logf("dest free slots: 64B=%d 1152B=%d; live to move: 32 closures + 32 args", freeSmall, freeBig)

	// Drive the evacuation until every argument and closure relocated.
	allMoved := func() bool {
		for i := range args {
			if uintptr(unsafe.Pointer(args[i])) == argAt[i] || *(*uintptr)(unsafe.Pointer(&kept[i])) == clAt[i] {
				return false
			}
		}
		return true
	}
	countMoved := func() int {
		m := 0
		for i := range args {
			if uintptr(unsafe.Pointer(args[i])) != argAt[i] {
				m++
			}
			if *(*uintptr)(unsafe.Pointer(&kept[i])) != clAt[i] {
				m++
			}
		}
		return m
	}
	const maxAttempts = 8
	attempts := 0
	prevMoved := 0
	for ; attempts < maxAttempts && !allMoved(); attempts++ {
		c0, m0 := runtime.LogosEvacTotals()
		runtime.GC()
		c1, m1 := runtime.LogosEvacTotals()
		m := countMoved()
		st := immixProdSpan(t, unsafe.Pointer(args[0]))
		_, _, _, pinned, _ := runtime.LogosImmixPlanesAt(argAt[0])
		t.Logf("attempt %d: evac cycles=%d objects=%d moved=%d (+%d) spanAlloc=%d/%d blockPinned=%v",
			attempts+1, c1-c0, m1-m0, m, m-prevMoved, st.AllocCount, st.NElems, pinned)
		prevMoved = m
	}
	if !allMoved() {
		probe := new(immixCleanupNode)
		probeBase := immixProdSpan(t, unsafe.Pointer(probe)).Base
		t.Logf("probe 64B span base=%#x", probeBase)
		census := func(bb uintptr) (int, string) {
			var argN, clN, bigN, smallN int
			for i := range args {
				if blockOf(unsafe.Pointer(args[i])) == bb {
					argN++
				}
				if blockOf(*(*unsafe.Pointer)(unsafe.Pointer(&kept[i]))) == bb {
					clN++
				}
			}
			for i := range destBig {
				if destBig[i] != nil && blockOf(unsafe.Pointer(destBig[i])) == bb {
					bigN++
				}
			}
			for i := range destSmall {
				if destSmall[i] != nil && blockOf(unsafe.Pointer(destSmall[i])) == bb {
					smallN++
				}
			}
			lines := argN*9 + clN + bigN*9 + smallN
			return lines, fmt.Sprintf("args=%d closures=%d destBig=%d destSmall=%d", argN, clN, bigN, smallN)
		}
		mine := map[uintptr]int{}
		tally := func(p unsafe.Pointer) {
			if st, ok := runtime.LogosImmixSpanOf(p); ok {
				mine[st.Base]++
			}
		}
		for i := range args {
			tally(unsafe.Pointer(args[i]))
			tally(*(*unsafe.Pointer)(unsafe.Pointer(&kept[i])))
		}
		for i := range destBig {
			if destBig[i] != nil {
				tally(unsafe.Pointer(destBig[i]))
			}
		}
		for i := range destSmall {
			if destSmall[i] != nil {
				tally(unsafe.Pointer(destSmall[i]))
			}
		}
		for _, a := range ownerAt {
			tally(unsafe.Pointer(a))
		}
		stuckBlocks := map[uintptr]bool{}
		for i := range args {
			clPtr := *(*unsafe.Pointer)(unsafe.Pointer(&kept[i]))
			if uintptr(unsafe.Pointer(args[i])) == argAt[i] {
				st := immixProdSpan(t, unsafe.Pointer(args[i]))
				free, pinned := immixCleanupBlockState(argAt[i])
				lines, who := census(blockOf(unsafe.Pointer(args[i])))
				t.Logf("cleanup %d arg stayed: span=%+v blockPin=%v blockOccFree=%d/256 cached=%v myLive~%d[%s] probeHere=%v", i, st, pinned, free, st.Cached, lines, who, st.Base == probeBase)
				stuckBlocks[blockOf(unsafe.Pointer(args[i]))] = true
			}
			if uintptr(clPtr) == clAt[i] {
				st := immixProdSpan(t, clPtr)
				free, pinned := immixCleanupBlockState(clAt[i])
				lines, who := census(blockOf(clPtr))
				t.Logf("cleanup %d closure stayed: span=%+v blockPin=%v blockOccFree=%d/256 cached=%v myLive~%d[%s] probeHere=%v", i, st, pinned, free, st.Cached, lines, who, st.Base == probeBase)
				stuckBlocks[blockOf(clPtr)] = true
			}
		}
		for bb := range stuckBlocks {
			immixCleanupLogBlock(t, bb, mine)
		}
		t.Fatalf("region did not fully evacuate in %d collections", maxAttempts)
	}

	// Per-object mask proof part 1: every relocated argument carries
	// its payload and observes the current next object. Part 2 runs
	// with the cleanups below.
	for i := range args {
		if !immixCleanupArgIntact(args[i], i, args[(i+1)%len(args)]) {
			t.Fatalf("moved argument %d lost its payload or next link", i)
		}
	}

	close(gate)
	deadline := time.Now().Add(30 * time.Second)
	seen := make([]bool, n)
	sweeps := 0
	for ran := 0; ran < n; {
		select {
		case a := <-got:
			k := int(a.id - 1000000)
			if k < 0 || k >= n || args[k] != a || seen[k] {
				t.Fatalf("cleanup received a stale argument")
			}
			seen[k] = true
			ran++
		default:
			sweeps++
			if sweeps%128 == 0 {
				runtime.GC()
			} else {
				runtime.Gosched()
			}
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d queued cleanups ran", ran, n)
			}
		}
	}

	// Post-move stability: further collections must not corrupt the
	// relocated graph.
	runtime.GC()
	for i := range args {
		if !immixCleanupArgIntact(args[i], i, args[(i+1)%len(args)]) {
			t.Fatalf("argument %d lost its payload or next link after later collections", i)
		}
	}
	runtime.KeepAlive(destBig)
	runtime.KeepAlive(destSmall)
	runtime.KeepAlive(args)
	runtime.KeepAlive(kept)
	t.Logf("arguments moved=%d/%d closures moved=%d/%d in %d collections", n, n, n, n, attempts+1)
}
