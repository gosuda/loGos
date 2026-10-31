// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && unix

package testlogos

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// Mandatory-ZGC foreign-boundary attacks. Every case drives real C: C
// holds Go memory across a condition-variable barrier while Go forces
// moving collections, C calls back into Go, or a C-created thread calls
// into Go. Nothing here reads or seeds runtime state; the only observations
// are public: runtime.GC, runtime.MemStats.NumGC, runtime.Pinner, and the
// address cgo hands to C (frCanon, frEntryAddr).
//
// Observation contract (LOGOS_EXPECT_MOVE, the variable testlogosmove
// already uses):
//
//	LOGOS_EXPECT_MOVE=1  the unrelated-movement witness is mandatory: at
//	                     least a quarter of witness objects (and of any
//	                     object a case releases) must have moved, else the
//	                     case fails. Run zgc builds this way.
//	LOGOS_EXPECT_MOVE=0  nothing may move (non-moving collectors).
//	unset                identity and data assertions still run, and every
//	                     witness reports its count through t.Log, but the
//	                     run says it cannot distinguish relocation that was
//	                     suppressed from relocation that never happened.
//
// Public observation gaps, stated rather than skipped:
//
//   - No public API names the collector or forces relocation of a given
//     page; relocation is observed by the canonical address C sees before
//     and after forced cycles.
//   - No public control starts a foreign call at a chosen phase of a
//     running cycle. Every case therefore stages entry, forced cycles, and
//     release on fixed barrier phases and claims nothing about admission
//     racing a concurrent cycle.
//   - Relocation selection is the collector's choice, so "became movable
//     again" is asserted on populations (at least a quarter of a released
//     set), against a witness that proves unrelated pages do move.

const (
	frPoison      uint64 = 0xDEADBEEFDEADBEEF
	frRegionShift        = 21
	frWaitTimeout        = frWaitSeconds * time.Second
	frChildEnv           = "LOGOS_FOREIGN_ROOTS_CHILD"
	frLargeWords         = 5000

	// frInteriorSkip is the word offset of the interior callback result the
	// interior cases hand C, matching the interior case of
	// TestForeignRootsFreshPointerHeld. Objects have at least 6 words, so
	// the skip-aware window never empties.
	frInteriorSkip = 3
)

// frMix is splitmix64; foreign_roots_c.c carries the same function.
func frMix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// frWord is the data contract: word j of the object with tag holds
// frWord(tag, j).
func frWord(tag uint32, j uint64) uint64 {
	return frMix(uint64(tag)*0x9E3779B97F4A7C15 + j + 1)
}

func frFill(w []uint64, tag uint32) {
	for j := range w {
		w[j] = frWord(tag, uint64(j))
	}
}

// frDataErr reports the first word of w that departs from the contract, or
// "". frStampedDataErr expects C's stamp in the last word.
func frDataErr(w []uint64, tag uint32) string { return frDataErrMode(w, tag, false) }

func frStampedDataErr(w []uint64, tag uint32) string { return frDataErrMode(w, tag, true) }

func frDataErrMode(w []uint64, tag uint32, stamped bool) string {
	for j := range w {
		want := frWord(tag, uint64(j))
		if stamped && j == len(w)-1 {
			want ^= frStampWord
		}
		if w[j] != want {
			return fmt.Sprintf("word %d of %d = %#x, want %#x", j, len(w), w[j], want)
		}
	}
	return ""
}

// frStatus decodes a C status word.
func frStatus(st int) string {
	if st == 0 {
		return "ok"
	}
	codes := []string{
		"?",
		"nil pointer",
		"referent corrupted at entry",
		"referent corrupted while held (relocated, freed, or overwritten under C)",
		"bad argument",
		"barrier timeout",
		"nested word changed under C",
	}
	name := "unknown code"
	if c := st & 15; c < len(codes) {
		name = codes[c]
	}
	return fmt.Sprintf("status %#x: %s at index %d", st, name, st>>4-1)
}

// frSurv is one surviving object of the fragmented heap.
type frSurv struct {
	w   []uint64
	tag uint32
}

// frEnt is a survivor with the address C observed before any collection.
type frEnt struct {
	s    frSurv
	addr uintptr
}

func (e frEnt) base() unsafe.Pointer { return unsafe.Pointer(&e.s.w[0]) }

func (e frEnt) last() uintptr { return e.addr + uintptr(len(e.s.w))*8 - 1 }

// frHeap is a deliberately fragmented heap: about 24 MiB of small objects
// of five size classes, interleaved across the address space, of which one
// in eight survives, plus a few large-object-span survivors. Every
// survivor is fresh: no collection has run since its allocation. Sparse
// pages are exactly what a relocating collector evacuates.
type frHeap struct {
	small []frEnt // sorted by address
	large []frEnt
	regs  []uintptr // distinct 2 MiB regions holding small survivors, ascending
}

type frClass struct{ words, churn, every int }

var frClasses = []frClass{
	{6, 80000, 8},  // 48 B
	{8, 70000, 8},  // 64 B
	{32, 20000, 8}, // 256 B
	{128, 5000, 8}, // 1 KiB
	{1024, 640, 8}, // 8 KiB
	{5000, 90, 6},  // 40000 B: above 32 KiB, a large-object span
}

var (
	frSink     []uint64
	frNodeSink *frNode
)

func frRegion(a uintptr) uintptr { return a >> frRegionShift }

func frBuildHeap(t *testing.T) *frHeap {
	t.Helper()
	const chunks = 40
	h := new(frHeap)
	tag := uint32(0)
	for ch := range chunks {
		for _, c := range frClasses {
			for i := c.churn * ch / chunks; i < c.churn*(ch+1)/chunks; i++ {
				w := make([]uint64, c.words)
				frSink = w
				if i%c.every != 0 {
					continue
				}
				tag++
				frFill(w, tag)
				e := frEnt{s: frSurv{w: w, tag: tag}}
				e.addr = frCanon(unsafe.Pointer(&w[0]))
				if c.words >= frLargeWords {
					h.large = append(h.large, e)
				} else {
					h.small = append(h.small, e)
				}
			}
		}
	}
	slices.SortFunc(h.small, func(a, b frEnt) int { return cmp.Compare(a.addr, b.addr) })
	for _, e := range h.small {
		if r := frRegion(e.addr); len(h.regs) == 0 || h.regs[len(h.regs)-1] != r {
			h.regs = append(h.regs, r)
		}
	}
	return h
}

func frNeedRegions(t *testing.T, h *frHeap, n int) {
	t.Helper()
	if len(h.regs) < n {
		t.Fatalf("the fragmented heap spans only %d distinct %d-byte regions but this case needs %d disjoint ones: the page-disjoint referent observation is unavailable on this heap layout", len(h.regs), 1<<frRegionShift, n)
	}
}

// inRegions returns the small survivors whose address lies in one of regs.
func (h *frHeap) inRegions(regs []uintptr) []frEnt {
	var out []frEnt
	for _, e := range h.small {
		if slices.Contains(regs, frRegion(e.addr)) {
			out = append(out, e)
		}
	}
	return out
}

// frSpread returns n distinct entries spaced evenly through ents, or nil
// when there are fewer than n.
func frSpread(ents []frEnt, n int) []frEnt {
	if n <= 0 || len(ents) < n {
		return nil
	}
	out := make([]frEnt, n)
	for i := range out {
		out[i] = ents[(2*i+1)*len(ents)/(2*n)]
	}
	return out
}

func frPick(t *testing.T, what string, ents []frEnt, n int) []frEnt {
	t.Helper()
	out := frSpread(ents, n)
	if out == nil {
		t.Fatalf("%s: the fragmented heap offers %d candidates, need %d", what, len(ents), n)
	}
	return out
}

// frHeldRegions is the set of 2 MiB regions that the given entries touch.
func frHeldRegions(sets ...[]frEnt) map[uintptr]bool {
	m := make(map[uintptr]bool)
	for _, set := range sets {
		for _, e := range set {
			m[frRegion(e.addr)] = true
			m[frRegion(e.last())] = true
		}
	}
	return m
}

// witness returns up to max small survivors that share no region with
// held: unrelated objects on unrelated pages.
func (h *frHeap) witness(held map[uintptr]bool, max int) []frEnt {
	var cand []frEnt
	for _, e := range h.small {
		if !held[frRegion(e.addr)] && !held[frRegion(e.last())] {
			cand = append(cand, e)
		}
	}
	if len(cand) > max {
		cand = frSpread(cand, max)
	}
	return cand
}

// frMoved counts entries whose canonical address changed since the heap
// was built.
func frMoved(ents []frEnt) int {
	n := 0
	for _, e := range ents {
		if frCanon(e.base()) != e.addr {
			n++
		}
	}
	return n
}

// frRequireMovement applies the LOGOS_EXPECT_MOVE contract to a count of
// moved objects.
func frRequireMovement(t *testing.T, what string, moved, total int) {
	t.Helper()
	want := max(1, total/4)
	switch os.Getenv("LOGOS_EXPECT_MOVE") {
	case "1":
		if total == 0 {
			t.Errorf("%s: no objects are available to observe, so relocation cannot be observed", what)
		} else if moved < want {
			t.Errorf("%s: %d of %d moved, want at least %d; LOGOS_EXPECT_MOVE=1 requires a moving collector (GOEXPERIMENT=logosgcmoving, -ldflags=-X=runtime.logosGCMode=zgc)", what, moved, total, want)
		}
	case "0":
		if moved != 0 {
			t.Errorf("%s: %d of %d moved under LOGOS_EXPECT_MOVE=0", what, moved, total)
		}
	default:
		t.Logf("%s: %d of %d moved. LOGOS_EXPECT_MOVE is unset, so this run cannot tell suppressed relocation from relocation that never happens; its identity assertions are not evidence of safety. Set LOGOS_EXPECT_MOVE=1 on a zgc build.", what, moved, total)
	}
}

// frMovingRounds forces rounds collections, each followed by a churn that
// overwrites storage a collection released, and checks through NumGC that
// every forced collection ran.
func frMovingRounds(t *testing.T, rounds int) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range rounds {
		runtime.GC()
		frChurn()
	}
	runtime.ReadMemStats(&after)
	if got := after.NumGC - before.NumGC; got < uint32(rounds) {
		t.Fatalf("forced %d collections but NumGC advanced by %d", rounds, got)
	}
}

// frChurn allocates 4 MiB of poison so memory returned by a collection is
// reused and overwritten before C reads it again.
func frChurn() {
	for range 1024 {
		w := make([]uint64, 512)
		for j := range w {
			w[j] = frPoison
		}
		frSink = w
	}
}

func frRefOf(e frEnt, skip int) frRef {
	return frRef{
		P:     unsafe.Pointer(&e.s.w[skip]),
		Tag:   e.s.tag,
		Words: uint16(len(e.s.w) - skip),
		Skip:  uint16(skip),
	}
}

func frRefsOf(ents []frEnt) []frRef {
	out := make([]frRef, len(ents))
	for i, e := range ents {
		out[i] = frRefOf(e, 0)
	}
	return out
}

func frRefPtrs(refs []frRef) []unsafe.Pointer {
	out := make([]unsafe.Pointer, len(refs))
	for i, r := range refs {
		out[i] = r.P
	}
	return out
}

func frPtrsOf(ents []frEnt) []unsafe.Pointer {
	out := make([]unsafe.Pointer, len(ents))
	for i, e := range ents {
		out[i] = e.base()
	}
	return out
}

func frWords(ents []frEnt) [][]uint64 {
	out := make([][]uint64, len(ents))
	for i, e := range ents {
		out[i] = e.s.w
	}
	return out
}

// frCheckAddrs asserts the C-observed identity: the address C received at
// entry for slot first+i equals the canonical address of ptrs[i] now.
func frCheckAddrs(t *testing.T, what string, g int, ptrs []unsafe.Pointer, first int) {
	t.Helper()
	bad, msg := 0, ""
	for i, p := range ptrs {
		entry, now := frEntryAddr(g, first+i), frCanon(p)
		if entry == 0 || entry != now {
			bad++
			if msg == "" {
				msg = fmt.Sprintf("slot %d: C saw %#x at entry, canonical address now %#x", first+i, entry, now)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%s: %d of %d referents moved or were never seen by C; first: %s", what, bad, len(ptrs), msg)
	}
}

// frCheckSet asserts every referent still holds its contract data;
// frCheckSetStamped also expects C's stamp in the last word.
func frCheckSet(t *testing.T, what string, ents []frEnt) {
	t.Helper()
	frCheckSetMode(t, what, ents, frDataErr)
}

func frCheckSetStamped(t *testing.T, what string, ents []frEnt) {
	t.Helper()
	frCheckSetMode(t, what, ents, frStampedDataErr)
}

func frCheckSetMode(t *testing.T, what string, ents []frEnt, check func([]uint64, uint32) string) {
	t.Helper()
	bad, first := 0, ""
	for i, e := range ents {
		if msg := check(e.s.w, e.s.tag); msg != "" {
			bad++
			if first == "" {
				first = fmt.Sprintf("referent %d (tag %d): %s", i, e.s.tag, msg)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%s: %d of %d referents hold wrong data; first: %s", what, bad, len(ents), first)
	}
}

func frWaitEntered(t *testing.T, g, want int) {
	t.Helper()
	if !frGateWaitEntered(g, want) {
		t.Fatalf("gate %d: %d holder(s) did not reach the C barrier within %v", g, want, frWaitTimeout)
	}
}

// frGoCall runs call on its own goroutine, so the controller can force
// collections while the call is in flight in C.
func frGoCall(call func() int) <-chan int {
	ch := make(chan int, 1)
	go func() { ch <- call() }()
	return ch
}

func frAwait(t *testing.T, ch <-chan int, what string) int {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(frWaitTimeout):
		t.Fatalf("%s: the C call did not return within %v", what, frWaitTimeout)
		return -1
	}
}

// TestForeignRootsMovementWitness calibrates the observation itself: a
// fragmented heap with no foreign call in flight. Under LOGOS_EXPECT_MOVE=1
// a quarter of the survivors must move, so a later "did not move" verdict
// cannot come from a collector that never relocates.
func TestForeignRootsMovementWitness(t *testing.T) {
	requireCgo(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := frBuildHeap(t)
	wit := h.witness(nil, 4000)
	if len(wit) == 0 {
		t.Fatal("the fragmented heap offers no witness objects")
	}
	frMovingRounds(t, 3)
	frRequireMovement(t, "survivors of a fragmented heap", frMoved(wit), len(wit))
	frCheckSet(t, "small survivors", h.small)
	frCheckSet(t, "large survivors", h.large)
}

// frJustAllocated allocates and fills one pointer-free object and returns
// it with its canonical address. Nothing but the C call that observes the
// address runs between its allocation and its use.
func frJustAllocated(words int, tag uint32) frEnt {
	w := make([]uint64, words)
	frSink = w
	frFill(w, tag)
	e := frEnt{s: frSurv{w: w, tag: tag}}
	e.addr = frCanon(e.base())
	return e
}

// TestForeignRootsFreshPointerHeld: a fresh heap pointer (never marked) is
// the only argument of a real C call. C blocks at a barrier while Go
// forces four moving collections with churn, then re-reads the object and
// stamps it. The object must keep the address C received, keep its data,
// and show C's stamp through Go's own pointer. The survivor cases take an
// object allocated during the fragmentation churn, on a page that a
// relocating collector evacuates; the justAllocated case allocates the
// object right before the call, so nothing separates its allocation from
// its admission. The interior case passes a pointer three words into the
// object.
func TestForeignRootsFreshPointerHeld(t *testing.T) {
	requireCgo(t)
	for _, tc := range []struct {
		name          string
		skip          int
		justAllocated bool
	}{{"base", 0, false}, {"interior", 3, false}, {"justAllocated", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			h := frBuildHeap(t)
			frNeedRegions(t, h, 4)
			var held []frEnt
			if tc.justAllocated {
				held = []frEnt{frJustAllocated(8, 0xFFFF0001)}
			} else {
				held = frPick(t, "fresh referent", h.inRegions(h.regs[:2]), 1)
			}
			refs := []frRef{frRefOf(held[0], tc.skip)}
			wit := h.witness(frHeldRegions(held), 2000)

			g := frNewGate()
			defer frGateRelease(g)
			res := frGoCall(func() int { return frHoldStamped(g, refs) })
			frWaitEntered(t, g, 1)
			frMovingRounds(t, 4)
			frGateRelease(g)
			if st := frAwait(t, res, "hold"); st != 0 {
				t.Fatalf("C reported %s", frStatus(st))
			}
			frCheckAddrs(t, "fresh referent", g, frRefPtrs(refs), 0)
			frCheckSetStamped(t, "fresh referent", held)
			frRequireMovement(t, "unrelated survivors", frMoved(wit), len(wit))
		})
	}
}

// TestForeignRootsIndexedArrayTile guards the indexed form &(*pa)[start]:
// cgo must admit the array pa points at, not a boxed array copy. C derives
// the tile base and verifies every element's pointer leaf and scalar word,
// holds across four moving collections, and stamps leaves plus tile V words.
// Entry must equal the caller's element address and the stamp must reach
// the caller's array; either fails if C pointed into a copy.
func TestForeignRootsIndexedArrayTile(t *testing.T) {
	requireCgo(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := frBuildHeap(t)
	frNeedRegions(t, h, 4)
	const nIdx = 8
	const startIdx = 2
	const tag0 = uint32(0x1D0000)
	const tagV0 = uint32(0x1E0000)
	leaves := make([][]uint64, nIdx)
	for k := range leaves {
		leaves[k] = make([]uint64, 8)
		frSink = leaves[k]
		frFill(leaves[k], tag0+uint32(k))
	}
	pins := newFrPinSet(nIdx)
	defer pins.Release()
	for k := range leaves {
		pins.Pin(k, unsafe.Pointer(&leaves[k][0]))
	}
	pa := new([8]frIdxElem)
	for k := range nIdx {
		pa[k].P = unsafe.Pointer(&leaves[k][0])
		pa[k].V = frWord(tagV0, uint64(k))
	}
	heldRegs := map[uintptr]bool{
		frRegion(uintptr(unsafe.Pointer(&pa[0]))): true,
	}
	for k := range leaves {
		heldRegs[frRegion(uintptr(unsafe.Pointer(&leaves[k][0])))] = true
	}
	wit := h.witness(heldRegs, 2000)

	g := frNewGate()
	defer frGateRelease(g)
	res := frGoCall(func() int { return frHoldIndexedStamped(g, pa, startIdx, tag0, tagV0) })
	frWaitEntered(t, g, 1)
	if got, want := frEntryAddr(g, 0), frCanon(unsafe.Pointer(&pa[startIdx])); got != want {
		t.Fatalf("indexed entry = %#x, want caller's element %#x: C points into a boxed copy, not &pa[%d]", got, want, startIdx)
	}
	frMovingRounds(t, 4)
	frGateRelease(g)
	if st := frAwait(t, res, "indexed hold"); st != 0 {
		t.Fatalf("C reported %s", frStatus(st))
	}
	for k := range leaves {
		if err := frStampedDataErr(leaves[k], tag0+uint32(k)); err != "" {
			t.Fatalf("leaf %d: %s", k, err)
		}
	}
	for k := range nIdx {
		want := frWord(tagV0, uint64(k)) ^ frStampWord
		if pa[k].V != want {
			t.Fatalf("pa[%d].V = %#x, want stamped %#x: C stamped a boxed copy, not the caller's tile", k, pa[k].V, want)
		}
	}
	frRequireMovement(t, "unrelated survivors", frMoved(wit), len(wit))
}

// TestForeignRootsManyDistinctReferents: one real C call receives n
// distinct top-level heap referents as separate argument cells of one
// by-value struct, n above 256 and 1024, across five size classes and
// four 2 MiB regions, four of them large-object spans, every fifth an
// interior pointer. No referent is pinned, so the foreign frame is the only
// retention. Distinctness is asserted on the objects' own bases.
func TestForeignRootsManyDistinctReferents(t *testing.T) {
	requireCgo(t)
	for _, n := range []int{257, 600, 1500} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			h := frBuildHeap(t)
			frNeedRegions(t, h, 6)
			const nLarge = 4
			if len(h.large) < nLarge {
				t.Fatalf("the heap offers %d large objects, need %d", len(h.large), nLarge)
			}
			held := frPick(t, "distinct small referents", h.inRegions(h.regs[:4]), n-nLarge)
			held = append(held, h.large[:nLarge]...)
			bases := make(map[uintptr]bool, n)
			for _, e := range held {
				bases[e.addr] = true
			}
			if len(bases) != n {
				t.Fatalf("%d referents name only %d distinct objects", n, len(bases))
			}
			refs := make([]frRef, n)
			for i, e := range held {
				skip := 0
				if i%5 == 4 {
					skip = 2
				}
				refs[i] = frRefOf(e, skip)
			}
			wit := h.witness(frHeldRegions(held), 2000)

			g := frNewGate()
			defer frGateRelease(g)
			res := frGoCall(func() int { return frHoldStamped(g, refs) })
			frWaitEntered(t, g, 1)
			frMovingRounds(t, 4)
			frGateRelease(g)
			if st := frAwait(t, res, "hold"); st != 0 {
				t.Fatalf("C reported %s", frStatus(st))
			}
			frCheckAddrs(t, "distinct referents", g, frRefPtrs(refs), 0)
			frCheckSetStamped(t, "distinct referents", held)
			frRequireMovement(t, "unrelated survivors", frMoved(wit), len(wit))
		})
	}
}

// TestForeignRootsOverlappingSharedReferents: calls A and B are in flight
// together. Both receive the same eight shared referents; each also holds
// eight referents of its own, all four sets on disjoint regions. One call
// ends while the other continues, in both orders. The ended call's
// exclusive referents must become movable again, the shared referents must
// stay exactly where both calls saw them for as long as the other call
// holds them, and the surviving call must find all its data intact.
func TestForeignRootsOverlappingSharedReferents(t *testing.T) {
	requireCgo(t)
	for _, endFirst := range []string{"A", "B"} {
		t.Run("end"+endFirst+"First", func(t *testing.T) {
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			h := frBuildHeap(t)
			frNeedRegions(t, h, 8)
			xa := frPick(t, "A exclusive", h.inRegions(h.regs[0:2]), 8)
			shared := frPick(t, "shared", h.inRegions(h.regs[2:4]), 8)
			xb := frPick(t, "B exclusive", h.inRegions(h.regs[4:6]), 8)
			wit := h.witness(frHeldRegions(xa, shared, xb), 2000)
			refsA := append(frRefsOf(xa), frRefsOf(shared)...)
			refsB := append(frRefsOf(xb), frRefsOf(shared)...)

			gA, gB := frNewGate(), frNewGate()
			defer frGateRelease(gA)
			defer frGateRelease(gB)
			resA := frGoCall(func() int { return frHold(gA, refsA) })
			frWaitEntered(t, gA, 1)
			resB := frGoCall(func() int { return frHold(gB, refsB) })
			frWaitEntered(t, gB, 1)
			frMovingRounds(t, 2)

			firstG, firstRes, firstX := gA, resA, xa
			secondG, secondRes, secondX := gB, resB, xb
			if endFirst == "B" {
				firstG, firstRes, firstX = gB, resB, xb
				secondG, secondRes, secondX = gA, resA, xa
			}
			frGateRelease(firstG)
			if st := frAwait(t, firstRes, "first call"); st != 0 {
				t.Fatalf("first call: C reported %s", frStatus(st))
			}
			frMovingRounds(t, 3)

			sharedPtrs := frPtrsOf(shared)
			frCheckAddrs(t, "shared referents after the first call ended (A's view)", gA, sharedPtrs, len(xa))
			frCheckAddrs(t, "shared referents after the first call ended (B's view)", gB, sharedPtrs, len(xb))
			frCheckAddrs(t, "exclusive referents of the surviving call while it holds", secondG, frPtrsOf(secondX), 0)
			frRequireMovement(t, "exclusive referents of the call that ended", frMoved(firstX), len(firstX))

			frGateRelease(secondG)
			if st := frAwait(t, secondRes, "second call"); st != 0 {
				t.Fatalf("second call (data re-read after the first call ended): C reported %s", frStatus(st))
			}
			frCheckAddrs(t, "shared referents after both calls ended", gA, sharedPtrs, len(xa))
			frCheckAddrs(t, "exclusive referents of the surviving call", secondG, frPtrsOf(secondX), 0)
			frCheckSet(t, "A exclusive", xa)
			frCheckSet(t, "B exclusive", xb)
			frCheckSet(t, "shared", shared)
			frRequireMovement(t, "unrelated survivors", frMoved(wit), len(wit))
		})
	}
}

// frPinSet owns one Pinner per slot. Release unpins each Pinner exactly
// once and gives up ownership, so deferred cleanup after an explicit
// Release has nothing left to unpin.
type frPinSet struct{ pinners []*runtime.Pinner }

func newFrPinSet(n int) *frPinSet {
	s := &frPinSet{pinners: make([]*runtime.Pinner, n)}
	for i := range s.pinners {
		s.pinners[i] = new(runtime.Pinner)
	}
	return s
}

func (s *frPinSet) Pin(i int, p unsafe.Pointer) { s.pinners[i].Pin(p) }

func (s *frPinSet) Release() {
	for i, pn := range s.pinners {
		if pn != nil {
			pn.Unpin()
			s.pinners[i] = nil
		}
	}
}

// frLauncher starts a callback holder on gate g for n callbacks and returns
// the function that waits for its status after release.
type frLauncher func(g, n int) (finish func() int)

// frGoCallLauncher holds from an ordinary Go-to-C call that has no pointer
// input: the only pointers come from callbacks.
func frGoCallLauncher(t *testing.T) frLauncher {
	return func(g, n int) func() int {
		res := frGoCall(func() int { return frCBHoldStamped(g, n) })
		return func() int { return frAwait(t, res, "callback hold") }
	}
}

// frThreadLauncher holds from a C-created thread: its callbacks run with no
// Go-origin outer call and no foreign frame below them.
func frThreadLauncher(t *testing.T) frLauncher {
	return func(g, n int) func() int {
		if rc := frThreadStartStamped(g, n); rc != 0 {
			t.Fatalf("pthread_create failed: %d", rc)
		}
		return func() int { return frThreadJoin(g) }
	}
}

// frPinnedCallbacks: C obtains twelve pointers from callbacks, each to a
// fresh object that the callback pins before returning it, holds them
// across four forced moving collections, re-reads and stamps them. Each
// object must keep the address the callback result gave C, keep its data,
// show the stamp, and keep that address after the call ended while the
// pins live. skip is the word offset of the pointer the callback hands C
// inside its pinned object: 0 is the base result, any other value makes C
// observe an interior result whose meta carries words len-skip and skip,
// so C verifies and stamps the window that starts skip words in.
func frPinnedCallbacks(t *testing.T, launch frLauncher, skip int) {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := frBuildHeap(t)
	frNeedRegions(t, h, 4)
	objs := frPick(t, "callback referents", h.inRegions(h.regs[:2]), 12)
	wit := h.witness(frHeldRegions(objs), 2000)

	g := frNewGate()
	defer frGateRelease(g)
	plan := frNewInteriorPinnedPlan(frWords(objs), skip)
	defer plan.UnpinAll()
	frSetPlan(g, plan)
	for i, e := range objs {
		frMetaSet(g, i, e.s.tag, len(e.s.w)-skip, skip)
	}

	finish := launch(g, len(objs))
	frWaitEntered(t, g, 1)
	frMovingRounds(t, 4)
	frGateRelease(g)
	if st := finish(); st != 0 {
		t.Fatalf("C reported %s", frStatus(st))
	}
	if got := plan.Calls(); got != len(objs) {
		t.Fatalf("callbacks served = %d, want %d", got, len(objs))
	}
	ptrs := make([]unsafe.Pointer, len(objs))
	for i, e := range objs {
		ptrs[i] = unsafe.Pointer(&e.s.w[skip])
	}
	frCheckAddrs(t, "pinned callback results", g, ptrs, 0)
	frCheckSetStamped(t, "pinned callback results", objs)

	frMovingRounds(t, 2)
	// The kept windows carry C's stamp in their last word (frCheckSetStamped
	// just proved it), so this re-read verifies the post-stamp contract:
	// pristine words plus the designed tail stamp, after the frame ended,
	// while the pins live.
	if st := frKeptCheck(g, 0, len(objs)); st != 0 {
		t.Fatalf("C re-reading its kept pointers after the frame ended: %s", frStatus(st))
	}
	frCheckAddrs(t, "pinned callback results after the call ended", g, ptrs, 0)
	plan.UnpinAll()
	frMovingRounds(t, 2)
	frRequireMovement(t, "callback referents after their pin was dropped", frMoved(objs), len(objs))
	frRequireMovement(t, "unrelated survivors", frMoved(wit), len(wit))
}

// TestForeignRootsZeroPointerCallPinnedCallbackResults: a Go-to-C call with
// no pointer input whose callbacks return Pinner-pinned pointers.
func TestForeignRootsZeroPointerCallPinnedCallbackResults(t *testing.T) {
	requireCgo(t)
	frPinnedCallbacks(t, frGoCallLauncher(t), 0)
}

// TestForeignRootsCThreadCallbackPinnedResults: the same results returned
// to a C-created thread, with no Go-origin outer call below the callbacks.
func TestForeignRootsCThreadCallbackPinnedResults(t *testing.T) {
	requireCgo(t)
	frPinnedCallbacks(t, frThreadLauncher(t), 0)
}

// TestForeignRootsZeroPointerCallInteriorCallbackResults: the same pinned
// lifetime with an interior callback result. The callback pins the whole
// object and hands C the word frInteriorSkip words in, C verifies and
// stamps with meta skip frInteriorSkip, and the entry address must be the
// canonical address of that interior word, not of the base. Required
// because offset-preserving result canonicalization had no consumer.
func TestForeignRootsZeroPointerCallInteriorCallbackResults(t *testing.T) {
	requireCgo(t)
	frPinnedCallbacks(t, frGoCallLauncher(t), frInteriorSkip)
}

// TestForeignRootsCThreadInteriorCallbackResults: the same interior result
// returned to a C-created thread, with no Go-origin outer call below the
// callbacks, so each self-bounded per-callback frame must carry it too.
func TestForeignRootsCThreadInteriorCallbackResults(t *testing.T) {
	requireCgo(t)
	frPinnedCallbacks(t, frThreadLauncher(t), frInteriorSkip)
}

// TestForeignRootsPinnedLifetimeAndUnpin: explicit pin lifetime. C keeps
// sixteen pinned callback results after the call that received them ended.
// The foreign frame is gone, so only the pins keep them in place: C re-reads
// them in a later pointer-free call after forced collections. The last
// eight get a second Pinner, then every first pin is dropped: the first
// eight must become movable while the last eight stay put for C. Dropping
// the second pin makes those movable too. A pin must end exactly with its
// own Unpin: neither the end of a foreign call nor an unrelated Unpin may
// release it, and an Unpin must really release it.
func TestForeignRootsPinnedLifetimeAndUnpin(t *testing.T) {
	requireCgo(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := frBuildHeap(t)
	frNeedRegions(t, h, 6)
	once := frPick(t, "singly pinned referents", h.inRegions(h.regs[0:2]), 8)
	twice := frPick(t, "doubly pinned referents", h.inRegions(h.regs[2:4]), 8)
	all := slices.Concat(once, twice)
	wit := h.witness(frHeldRegions(all), 2000)

	g := frNewGate()
	defer frGateRelease(g)
	plan := frNewPinnedPlan(frWords(all))
	defer plan.UnpinAll()
	frSetPlan(g, plan)
	for i, e := range all {
		frMetaSet(g, i, e.s.tag, len(e.s.w), 0)
	}
	second := newFrPinSet(len(twice))
	defer second.Release()

	res := frGoCall(func() int { return frCBHold(g, len(all)) })
	frWaitEntered(t, g, 1)
	frGateRelease(g)
	if st := frAwait(t, res, "callback hold"); st != 0 {
		t.Fatalf("C reported %s", frStatus(st))
	}
	frCheckAddrs(t, "pinned referents right after the call", g, frPtrsOf(all), 0)

	frMovingRounds(t, 3)
	if st := frKeptCheck(g, 0, len(all)); st != 0 {
		t.Fatalf("C re-reading its kept pinned pointers after the frame ended: %s", frStatus(st))
	}
	frCheckAddrs(t, "pinned referents after the frame ended", g, frPtrsOf(all), 0)

	for i, e := range twice {
		second.Pin(i, e.base())
	}
	plan.UnpinAll()
	frMovingRounds(t, 3)
	if st := frKeptCheck(g, len(once), len(all)); st != 0 {
		t.Fatalf("C re-reading doubly pinned pointers after the first Unpin: %s", frStatus(st))
	}
	frCheckAddrs(t, "doubly pinned referents after the first Unpin", g, frPtrsOf(twice), len(once))
	frRequireMovement(t, "referents after their only pin was dropped", frMoved(once), len(once))

	second.Release()
	frMovingRounds(t, 3)
	frRequireMovement(t, "referents after their last pin was dropped", frMoved(twice), len(twice))
	frCheckSet(t, "singly pinned", once)
	frCheckSet(t, "doubly pinned", twice)
	frRequireMovement(t, "unrelated survivors", frMoved(wit), len(wit))
}

// frRecovered runs f and returns the text of the panic it raised, or "".
func frRecovered(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	f()
	return ""
}

func frPoint(n *frNode, leaf frEnt) {
	n.Next = leaf.base()
	n.Tag = leaf.s.tag
	n.Words = uint32(len(leaf.s.w))
}

// frExpectNestedRejected asserts the upstream pointer-check verdict for a
// top-level pointer to Go memory that holds an unpinned Go pointer.
func frExpectNestedRejected(t *testing.T, what string, n *frNode) {
	t.Helper()
	g := frNewGate()
	frGateRelease(g) // a wrongly accepted call must not wait at the barrier
	var st int
	msg := frRecovered(func() { st = frHoldNode(g, n) })
	if msg == "" {
		t.Fatalf("%s: the unpinned nested pointer was accepted (C %s)", what, frStatus(st))
	}
	if !strings.Contains(msg, "has Go pointer to unpinned Go pointer") {
		t.Fatalf("%s: rejection message changed: %q", what, msg)
	}
}

// frChildNested runs in a child process because rejection panics belong in
// an isolated process. A pinned nested pointee is accepted and the nested
// word C reads is the pinned address; unpinning it flips the verdict back to
// upstream's rejection, as does a pointee that was never pinned and has
// been through forced collections.
func frChildNested(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := frBuildHeap(t)
	frNeedRegions(t, h, 4)
	nodes := make([]*frNode, 0, 8400)
	for i := range 100000 {
		n := &frNode{Tag: uint32(i)}
		frNodeSink = n
		if i%12 == 0 {
			nodes = append(nodes, n)
		}
	}
	leaves := frPick(t, "nested leaves", h.inRegions(h.regs[:2]), 2)
	pinnedLeaf, plainLeaf := leaves[0], leaves[1]
	node := nodes[len(nodes)/2]
	frPoint(node, pinnedLeaf)
	pin := newFrPinSet(1)
	defer pin.Release()
	pin.Pin(0, pinnedLeaf.base())

	g := frNewGate()
	defer frGateRelease(g)
	res := frGoCall(func() int { return frHoldNode(g, node) })
	frWaitEntered(t, g, 1)
	frMovingRounds(t, 3)
	frGateRelease(g)
	if st := frAwait(t, res, "node hold"); st != 0 {
		t.Fatalf("C reported %s", frStatus(st))
	}
	frCheckAddrs(t, "node and its pinned nested pointee", g, []unsafe.Pointer{unsafe.Pointer(node), node.Next}, 0)
	if now := frCanon(pinnedLeaf.base()); now != pinnedLeaf.addr {
		t.Fatalf("pinned pointee moved: %#x, was %#x", now, pinnedLeaf.addr)
	}
	frCheckSetStamped(t, "pinned nested pointee", []frEnt{pinnedLeaf})

	pin.Release()
	frMovingRounds(t, 2)
	frExpectNestedRejected(t, "pointee after its Unpin", node)

	node2 := nodes[len(nodes)/3]
	frPoint(node2, plainLeaf)
	frMovingRounds(t, 3)
	frExpectNestedRejected(t, "never-pinned pointee after forced collections", node2)
}

// frChildGoOrigin: an exported callback returns an unpinned pointer to a C
// function the Go caller entered with eight top-level referents. The result
// check must reject it with upstream's message and the panic must unwind
// the C frame into the recovering Go caller. Afterwards nothing may stay
// held: the aborted call's referents become movable again, and the same
// goroutine can make ordinary foreign calls.
func frChildGoOrigin(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := frBuildHeap(t)
	frNeedRegions(t, h, 4)
	x := frPick(t, "aborted call's referents", h.inRegions(h.regs[:2]), 8)
	refs := frRefsOf(x)
	frSetUnpinned()

	var res int
	msg := frRecovered(func() { res = frTryRefs(refs) })
	if msg == "" {
		t.Fatalf("an unpinned callback result was accepted (C returned %d)", res)
	}
	if !strings.Contains(msg, "called from cgo is unpinned Go") {
		t.Fatalf("rejection message changed: %q", msg)
	}

	frMovingRounds(t, 3)
	frRequireMovement(t, "referents of a call aborted by a rejected result", frMoved(x), len(x))
	frCheckSet(t, "aborted call's referents", x)

	g := frNewGate()
	frGateRelease(g) // pre-released: the follow-up call must not wait
	if st := frHoldStamped(g, refs[:4]); st != 0 {
		t.Fatalf("ordinary call on the same goroutine after the abort: C reported %s", frStatus(st))
	}
	frCheckSetStamped(t, "follow-up call referents", x[:4])
}

// frChildThread: a C-created thread's callback returns an unpinned pointer.
// There is no Go caller to recover, so upstream's rejection is a fatal
// panic: this process must die with the upstream message. Surviving is the
// defect.
func frChildThread(t *testing.T) {
	frSetUnpinned()
	g := frNewGate()
	if rc := frThreadStartUnpinned(g); rc != 0 {
		t.Fatalf("pthread_create failed: %d", rc)
	}
	st := frThreadJoin(g)
	t.Fatalf("an unpinned result returned to a C-created thread was accepted (thread status %d)", st)
}

// TestForeignRootsChild is the subprocess driver of the rejection cases. It
// re-executes this test binary, so the child keeps the parent's collector
// mode, and the parent reads its output.
func TestForeignRootsChild(t *testing.T) {
	name := os.Getenv(frChildEnv)
	if name == "" {
		t.Skip("subprocess driver: runs only when a parent test sets " + frChildEnv)
	}
	requireCgo(t)
	switch name {
	case "nested":
		frChildNested(t)
	case "goorigin":
		frChildGoOrigin(t)
	case "thread":
		frChildThread(t)
	default:
		t.Fatalf("unknown child case %q", name)
	}
	fmt.Println("FOREIGN_ROOTS_CHILD_OK " + name)
}

func frRunChild(t *testing.T, name string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestForeignRootsChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), frChildEnv+"="+name)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

func frExpectChildOK(t *testing.T, name string) {
	t.Helper()
	requireCgo(t)
	out, err := frRunChild(t, name)
	if err != nil {
		t.Fatalf("child %s: %v\n%s", name, err, out)
	}
	if !strings.Contains(out, "FOREIGN_ROOTS_CHILD_OK "+name) {
		t.Fatalf("child %s did not finish its case:\n%s", name, out)
	}
}

// TestForeignRootsNestedPinnedVerdicts: pointer-check verdicts and Pinner
// obligations for a pointer nested in the memory a call receives.
func TestForeignRootsNestedPinnedVerdicts(t *testing.T) { frExpectChildOK(t, "nested") }

// TestForeignRootsUnpinnedResultRejectedGoOrigin: unpinned callback result
// rejected, panic recovered by the Go caller, nothing left held.
func TestForeignRootsUnpinnedResultRejectedGoOrigin(t *testing.T) {
	frExpectChildOK(t, "goorigin")
}

// TestForeignRootsUnpinnedResultRejectedCThread: unpinned callback result
// on a C-created thread kills the process with upstream's message.
func TestForeignRootsUnpinnedResultRejectedCThread(t *testing.T) {
	requireCgo(t)
	out, err := frRunChild(t, "thread")
	if err == nil {
		t.Fatalf("child survived an unpinned result returned to a C-created thread:\n%s", out)
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 2 {
		t.Fatalf("child did not die with an unrecovered panic (exit 2): %v\n%s", err, out)
	}
	if !strings.Contains(out, "called from cgo is unpinned Go") {
		t.Fatalf("child died without upstream's rejection message:\n%s", out)
	}
}
