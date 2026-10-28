// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"bytes"
	"math/bits"
	"runtime/debug"
	"sync"
	"testing"

	. "runtime"
)

var logosZgcColors = []uint8{LogosZgcMark0, LogosZgcMark1, LogosZgcRemapped, LogosZgcFinal}

const logosZgcReserved = uint64(1<<47 | 1<<43 | 0xF<<60 | 0xFF<<48)

func logosZgcBit(n uint) uint64 { return 1 << n }

func TestLogosZgcLayout(t *testing.T) {
	if LogosZgcAddrBits != 42 || LogosZgcAddrMask != 1<<42-1 {
		t.Fatalf("address field = %d bits, mask %#x; want 42 bits, mask 1<<42-1", LogosZgcAddrBits, LogosZgcAddrMask)
	}
	if LogosZgcAddrMask+1 != 4<<40 {
		t.Fatalf("usable range = %#x bytes, want 4 TB", LogosZgcAddrMask+1)
	}
	if LogosZgcTagRegion != 0xFFFF<<48 {
		t.Fatalf("MMU-ignored region = %#x, want bits 63..48", LogosZgcTagRegion)
	}
	if LogosZgcColorMask != 0xF<<56 {
		t.Fatalf("hardware color field = %#x, want bits 59..56", LogosZgcColorMask)
	}
	soft := map[uint8]uint{LogosZgcMark0: 42, LogosZgcMark1: 44, LogosZgcRemapped: 45, LogosZgcFinal: 46}
	tag := map[uint8]uint{LogosZgcMark0: 56, LogosZgcMark1: 57, LogosZgcRemapped: 58, LogosZgcFinal: 59}
	for c, pos := range soft {
		if got := LogosZgcSoftBits(c); got != logosZgcBit(pos) {
			t.Errorf("software bits of color %#x = %#x, want bit %d", c, got, pos)
		}
	}
	for c, pos := range tag {
		if got := LogosZgcTagBits(c); got != logosZgcBit(pos) {
			t.Errorf("hardware bits of color %#x = %#x, want bit %d", c, got, pos)
		}
	}
}

func TestLogosZgcLayoutReservedBitsStayZero(t *testing.T) {
	softMask := logosZgcBit(42) | logosZgcBit(44) | logosZgcBit(45) | logosZgcBit(46)
	for c := uint8(0); c <= LogosZgcColorNibble; c++ {
		s, h := LogosZgcSoftBits(c), LogosZgcTagBits(c)
		if s&(logosZgcReserved|LogosZgcAddrMask|LogosZgcColorMask) != 0 {
			t.Errorf("software encoding of %#x = %#x leaves bits 42,44,45,46", c, s)
		}
		if h&(logosZgcReserved|LogosZgcAddrMask|softMask) != 0 {
			t.Errorf("hardware encoding of %#x = %#x leaves bits 59..56", c, h)
		}
		if bits.OnesCount64(s) != bits.OnesCount64(uint64(c)) || bits.OnesCount64(h) != bits.OnesCount64(uint64(c)) {
			t.Errorf("encodings of %#x are not bit-for-bit: soft %#x hard %#x", c, s, h)
		}
	}
}

func TestLogosZgcUnmaskRoundTrip(t *testing.T) {
	addrs := []uintptr{0, 8, 1 << 41, uintptr(LogosZgcAddrMask) &^ 7, uintptr(LogosZgcAddrMask), 0xc000123458}
	selects := map[string]func(uint8){"software": LogosZgcSelectSoftware, "hardware": LogosZgcSelectHardware}
	for name, sel := range selects {
		sel(LogosZgcRemapped)
		for _, a := range addrs {
			for c := uint8(0); c <= LogosZgcColorNibble; c++ {
				if got := LogosZgcAddr(LogosZgcColored(a, c)); got != a {
					t.Fatalf("%s: unmask(%#x colored %#x) = %#x", name, a, c, got)
				}
			}
			noisy := a + uintptr(LogosZgcTagRegion|logosZgcBit(46)|logosZgcBit(44))
			if LogosZgcAddr(noisy) != a {
				t.Fatalf("%s: unmask of %#x with stray tag bits kept them", name, noisy)
			}
		}
	}
}

func TestLogosZgcHardwareTagging(t *testing.T) {
	ok, errno := LogosZgcEnableHardwareTagging()
	if !ok {
		t.Skipf("blocked: no hardware pointer tagging on %s/%s (errno %d); the #29 and #168 hardware attack did not run", GOOS, GOARCH, errno)
	}
	base, free := logosZgcMapPages(1)
	defer free()
	LogosZgcStore(base, 0x1234)
	old := debug.SetPanicOnFault(true)
	defer debug.SetPanicOnFault(old)
	var lost []uint
	for pos := uint(56); pos <= 59; pos++ {
		if !logosZgcReadThrough(base + uintptr(logosZgcBit(pos))) {
			lost = append(lost, pos)
		}
	}
	if len(lost) != 0 {
		t.Fatalf("color bits %v did not survive addressing on %s (Linux LAM_U57 ignores 62..57 and AMD UAI ignores 63..57, so SPEC 19.2 bit 56 needs a layout decision)", lost, GOARCH)
	}
}

func logosZgcReadThrough(addr uintptr) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return LogosZgcWord(addr) == 0x1234
}

func logosZgcMapPages(n int) (base uintptr, free func()) {
	base, raw := LogosZgcMapPagesRaw(n)
	return base, func() { LogosZgcUnmapPages(raw, n) }
}

func logosZgcPlace(t *testing.T) (base uintptr, free func()) {
	t.Helper()
	return logosZgcMapPages(2)
}

func TestLogosZgcGoodColorFollowsFlip(t *testing.T) {
	all := LogosZgcSoftBits(LogosZgcColorNibble)
	for _, good := range []uint8{LogosZgcMark0, LogosZgcMark1, LogosZgcRemapped} {
		LogosZgcSelectSoftware(good)
		if g, m := LogosZgcGoodBits(), LogosZgcColorMaskBits(); uint64(g) != LogosZgcSoftBits(good) || uint64(m) != all {
			t.Errorf("good %#x: good bits %#x, color mask %#x, want %#x and %#x", good, g, m, LogosZgcSoftBits(good), all)
		}
	}
	LogosZgcSelectSoftware(LogosZgcRemapped)
	LogosZgcFlip(LogosZgcMark1)
	if LogosZgcGoodBits() != uintptr(LogosZgcSoftBits(LogosZgcMark1)) {
		t.Fatalf("flip to mark1 left good bits %#x", LogosZgcGoodBits())
	}
}

func TestLogosZgcSoftwareBarrier(t *testing.T) {
	for _, sel := range []func(uint8){LogosZgcSelectSoftware, LogosZgcSelectHardware} {
		sel(LogosZgcRemapped)
		base, free := logosZgcPlace(t)
		pg := LogosZgcNewPage(base, 0, 4)
		oldA, newA := base+0x100, base+LogosZgcPageSize+0x200
		LogosZgcForwardInsert(pg, oldA, newA)
		logosZgcCheckBarrier(t, oldA, newA)
		LogosZgcFreePage(pg)
		free()
	}
}

func logosZgcCheckBarrier(t *testing.T, oldA, newA uintptr) {
	t.Helper()
	slot := new(uintptr)
	*slot = LogosZgcColored(newA, LogosZgcRemapped)
	before := LogosZgcHeals()
	if got := LogosZgcLoad(slot); got != newA || LogosZgcHeals() != before {
		t.Fatalf("good load = %#x heals %d, want %#x and no heal", got, LogosZgcHeals()-before, newA)
	}
	for _, c := range []uint8{LogosZgcMark0, LogosZgcMark1, LogosZgcFinal, LogosZgcRemapped | LogosZgcFinal} {
		*slot = LogosZgcColored(oldA, c)
		before = LogosZgcHeals()
		if got := LogosZgcLoad(slot); got != newA {
			t.Fatalf("bad color %#x: load = %#x, want forwarded %#x", c, got, newA)
		}
		if *slot != LogosZgcColored(newA, LogosZgcRemapped) || LogosZgcHeals() != before+1 {
			t.Fatalf("bad color %#x: slot %#x heals %d, want healed word and one heal", c, *slot, LogosZgcHeals()-before)
		}
		before = LogosZgcHeals()
		if LogosZgcLoad(slot) != newA || LogosZgcHeals() != before {
			t.Fatalf("second load of healed slot took the slow path")
		}
	}
	logosZgcCheckUncolored(t, slot, oldA, newA)
	logosZgcCheckRecolorAndFlip(t, slot, oldA)
}

func logosZgcCheckUncolored(t *testing.T, slot *uintptr, oldA, newA uintptr) {
	t.Helper()
	*slot = oldA
	before := LogosZgcHeals()
	if got := LogosZgcLoad(slot); got != newA {
		t.Fatalf("uncolored word of a forwarded object: load = %#x, want %#x (plain stores and allocator results carry no color)", got, newA)
	}
	if *slot != LogosZgcColored(newA, LogosZgcRemapped) || LogosZgcHeals() != before+1 {
		t.Fatalf("uncolored word: slot %#x heals %d, want healed word and one heal", *slot, LogosZgcHeals()-before)
	}
	*slot = 0
	before = LogosZgcHeals()
	if LogosZgcLoad(slot) != 0 || *slot != 0 || LogosZgcHeals() != before {
		t.Fatalf("nil load changed the slot or counted a heal")
	}
}

func logosZgcCheckRecolorAndFlip(t *testing.T, slot *uintptr, oldA uintptr) {
	t.Helper()
	unforwarded := oldA + 0x200
	*slot = LogosZgcColored(unforwarded, LogosZgcMark0)
	if got := LogosZgcLoad(slot); got != unforwarded || *slot != LogosZgcColored(unforwarded, LogosZgcRemapped) {
		t.Fatalf("unforwarded bad load = %#x slot %#x, want recolor only", got, *slot)
	}
	LogosZgcFlip(LogosZgcMark1)
	before := LogosZgcHeals()
	if got := LogosZgcLoad(slot); got != unforwarded || LogosZgcHeals() != before+1 {
		t.Fatalf("pointer of the previous good color after flip: load %#x heals %d", got, LogosZgcHeals()-before)
	}
	if *slot != LogosZgcColored(unforwarded, LogosZgcMark1) {
		t.Fatalf("flip heal left slot %#x", *slot)
	}
	LogosZgcFlip(LogosZgcRemapped)
}

func TestLogosZgcSelfHealConcurrentCAS(t *testing.T) {
	LogosZgcSelectSoftware(LogosZgcRemapped)
	base, free := logosZgcPlace(t)
	defer free()
	pg := LogosZgcNewPage(base, 0, 4)
	defer LogosZgcFreePage(pg)
	oldA, newA := base+0x40, base+LogosZgcPageSize+0x80
	LogosZgcForwardInsert(pg, oldA, newA)
	const workers = 8
	slot := new(uintptr)
	*slot = LogosZgcColored(oldA, LogosZgcMark0)
	before := LogosZgcHeals()
	results := make([]uintptr, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Go(func() {
			<-start
			results[i] = LogosZgcLoad(slot)
		})
	}
	close(start)
	wg.Wait()
	for i, r := range results {
		if r != newA {
			t.Fatalf("worker %d loaded %#x, want %#x", i, r, newA)
		}
	}
	if h := LogosZgcHeals() - before; h < 1 || h > workers {
		t.Fatalf("heals = %d, want between 1 and %d", h, workers)
	}
	if *slot != LogosZgcColored(newA, LogosZgcRemapped) {
		t.Fatalf("final slot %#x lost the healed word", *slot)
	}
}

func logosZgcIsREXW(b byte) bool { return b&0xF8 == 0x48 }

// logosZgcFindTestQ returns the index of the first TESTQ r64, r/m64
// (REX.W 85 /r) in code, or -1.
func logosZgcFindTestQ(code []byte) int {
	for i := 0; i+2 < len(code); i++ {
		if logosZgcIsREXW(code[i]) && code[i+1] == 0x85 && code[i+2]&0xC0 == 0xC0 {
			return i
		}
	}
	return -1
}

// logosZgcBranch decodes the JE or JNE at code[at]. It returns the taken
// target, the fall-through offset, and whether the branch is taken when
// the TESTQ result is nonzero (JNE), which is the bad-color polarity.
func logosZgcBranch(code []byte, at int) (target, next int, takenWhenNonzero, ok bool) {
	rel32 := func(i int) int {
		return int(int32(code[i]) | int32(code[i+1])<<8 | int32(code[i+2])<<16 | int32(code[i+3])<<24)
	}
	switch {
	case code[at] == 0x75 || code[at] == 0x74:
		next = at + 2
		return next + int(int8(code[at+1])), next, code[at] == 0x75, true
	case code[at] == 0x0F && (code[at+1] == 0x85 || code[at+1] == 0x84):
		next = at + 6
		return next + rel32(at+2), next, code[at+1] == 0x85, true
	}
	return 0, 0, false, false
}

func TestLogosZgcBarrierInstructions(t *testing.T) {
	if GOARCH != "amd64" {
		t.Skip("blocked: the TESTQ/JNZ/ANDQ shape is asserted with amd64 encodings")
	}
	code := LogosZgcBarrierCode()
	testq := logosZgcFindTestQ(code)
	if testq < 0 {
		t.Fatalf("barrier has no TESTQ: % x", code)
	}
	target, next, nonzero, ok := logosZgcBranch(code, testq+3)
	if !ok || target < 0 || target+8 > len(code) {
		t.Fatalf("TESTQ is not followed by a conditional branch: % x", code)
	}
	heal := next
	if nonzero {
		heal = target
	}
	if !bytes.Contains(code[heal:min(len(code), heal+8)], []byte{0xE8}) {
		t.Fatalf("bad-color path at %#x does not reach the CALL to the self-heal path: % x", heal, code)
	}
	// The unmask is one ANDQ of a register with the loaded zgcUnmaskBits
	// word (REX.W 21 or REX.W 23), not of a 42-bit immediate: before zgc is
	// configured the word is all ones and the AND must keep every bit.
	andq := false
	for i := 0; i+1 < len(code); i++ {
		if code[i]&0xF8 == 0x48 && (code[i+1] == 0x21 || code[i+1] == 0x23) {
			andq = true
			break
		}
	}
	if !andq {
		t.Fatalf("barrier does not ANDQ the loaded word with the unmask word: % x", code)
	}
}

// TestLogosZgcUnconfiguredLoadKeepsForeignWords: before zgcConfigure the
// runtime barrier must return every word unchanged, as the compiled one
// does. A pointer to C, mmap or stack memory above the 4 TB field keeps
// its high address bits; a 42-bit mask here would truncate it.
func TestLogosZgcUnconfiguredLoadKeepsForeignWords(t *testing.T) {
	defer LogosZgcReset()
	LogosZgcReset()
	heap := new([4]uintptr)
	for _, w := range []uintptr{
		0,
		logosZgcAddrOf(heap),
		0x5555_0000_1000,
		0x7f12_3456_7890,
		0x7ffd_dead_bee8,
		uintptr(LogosZgcAddrMask) + 8,
	} {
		slot := new(uintptr)
		*slot = w
		before := LogosZgcHeals()
		if got := LogosZgcLoad(slot); got != w || *slot != w || LogosZgcHeals() != before {
			t.Errorf("unconfigured load of %#x = %#x, slot %#x, heals %d; want it unchanged", w, got, *slot, LogosZgcHeals()-before)
		}
	}
	KeepAlive(heap)
}

func TestLogosZgcEvacuationThreshold(t *testing.T) {
	if LogosZgcEvacPercent != 35 {
		t.Fatalf("evacuation threshold = %d%%, want 35%%", LogosZgcEvacPercent)
	}
	cases := []struct {
		size, garbage uintptr
		want          bool
	}{
		{100, 34, false}, {100, 35, false}, {100, 36, true},
		{1000, 340, false}, {1000, 350, false}, {1000, 360, true},
		{LogosZgcPageSize, 734003, false}, {LogosZgcPageSize, 734004, true},
		{LogosZgcPageSize, 0, false}, {LogosZgcPageSize, LogosZgcPageSize, true},
	}
	for _, c := range cases {
		if got := LogosZgcFragmentationExceeds(c.garbage, c.size); got != c.want {
			t.Errorf("garbage %d of %d: exceeds = %v, want %v", c.garbage, c.size, got, c.want)
		}
		pg := LogosZgcNewDetachedPage(c.size, c.size-c.garbage)
		if got := LogosZgcPageEvacuable(pg); got != c.want {
			t.Errorf("page of %d with %d live: evacuable = %v, want %v", c.size, c.size-c.garbage, got, c.want)
		}
		LogosZgcFreePage(pg)
	}
}

func TestLogosZgcForwardingTableOffHeap(t *testing.T) {
	base, free := logosZgcPlace(t)
	defer free()
	const n = 4096
	pg := LogosZgcNewPage(base, n*16, n)
	tableAddr, tableLen := LogosZgcTableAddr(pg)
	if !LogosZgcOffHeap(LogosZgcPageAddr(pg)) || !LogosZgcOffHeap(tableAddr) {
		t.Fatalf("page header %#x or forwarding table %#x lies in the Go heap", LogosZgcPageAddr(pg), tableAddr)
	}
	if tableAddr+tableLen > base && tableAddr < base+LogosZgcPageSize {
		t.Fatalf("forwarding table %#x+%d overlaps the source page, so releasing the page would drop it", tableAddr, tableLen)
	}
	if tableLen < 2*n*8 {
		t.Fatalf("forwarding table holds %d bytes, want at least %d for load factor 1/2", tableLen, 2*n*8)
	}
	for i := range uintptr(n) {
		from, to := base+i*24, base+LogosZgcPageSize+i*16
		if got := LogosZgcForwardInsert(pg, from, to); got != to {
			t.Fatalf("insert %d returned %#x, want %#x", i, got, to)
		}
	}
	for i := range uintptr(n) {
		if to, ok := LogosZgcForward(pg, base+i*24); !ok || to != base+LogosZgcPageSize+i*16 {
			t.Fatalf("lookup %d = %#x, %v", i, to, ok)
		}
	}
	if _, ok := LogosZgcForward(pg, base+8); ok {
		t.Fatal("lookup of an address never inserted found an entry")
	}
	if got := LogosZgcForwardInsert(pg, base, 0xdead0000); got != base+LogosZgcPageSize {
		t.Fatalf("duplicate insert returned %#x, want the first writer's %#x", got, base+LogosZgcPageSize)
	}
	if LogosZgcPageFor(base+100) != pg || LogosZgcPageFor(base+LogosZgcPageSize) != nil {
		t.Fatal("page directory does not map exactly the registered page")
	}
	LogosZgcFreePage(pg)
	if LogosZgcPageFor(base) != nil {
		t.Fatal("freed page is still registered")
	}
}

func TestLogosZgcForwardingConcurrentInsert(t *testing.T) {
	base, free := logosZgcPlace(t)
	defer free()
	const keys, workers = 512, 8
	pg := LogosZgcNewPage(base, 0, keys)
	defer LogosZgcFreePage(pg)
	winners := make([][]uintptr, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range winners {
		winners[w] = make([]uintptr, keys)
		wg.Go(func() {
			<-start
			for k := range uintptr(keys) {
				to := base + LogosZgcPageSize + uintptr(w)*0x10000 + k*16
				winners[w][k] = LogosZgcForwardInsert(pg, base+k*16, to)
			}
		})
	}
	close(start)
	wg.Wait()
	for k := range uintptr(keys) {
		want, ok := LogosZgcForward(pg, base+k*16)
		if !ok {
			t.Fatalf("key %d missing after racing inserts", k)
		}
		for w := range winners {
			if winners[w][k] != want {
				t.Fatalf("key %d: worker %d saw winner %#x, table holds %#x", k, w, winners[w][k], want)
			}
		}
	}
}

func TestLogosZgcReleaseBeforeSelfHealDrains(t *testing.T) {
	if GOOS != "linux" {
		t.Skip("blocked: zero fill after release is a property of madvise(MADV_DONTNEED) on Linux")
	}
	LogosZgcSelectSoftware(LogosZgcRemapped)
	base, free := logosZgcPlace(t)
	defer free()
	src := base
	const objects, size, workers = 64, 4096, 8
	pg := LogosZgcNewPage(src, objects*size, objects)
	retired := false
	defer func() {
		if !retired {
			LogosZgcDropPage(pg)
		}
	}()
	slots := make([]uintptr, objects)
	for i := range uintptr(objects) {
		LogosZgcFill(src+i*size, size, byte(i+1))
		slots[i] = LogosZgcColored(src+i*size, LogosZgcMark0)
	}

	// Racing relocators copy every object but the last to their own
	// targets; exactly one copy of each wins.
	target := func(w int, i uintptr) uintptr {
		return base + LogosZgcPageSize + uintptr(w)*objects*size + i*size
	}
	won := make([][]uintptr, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		won[w] = make([]uintptr, objects-1)
		wg.Go(func() {
			<-start
			for i := range uintptr(objects - 1) {
				won[w][i] = LogosZgcRelocate(pg, src+i*size, size, target(w, i))
			}
		})
	}
	close(start)
	wg.Wait()
	dst := make([]uintptr, objects)
	for i := range objects - 1 {
		dst[i] = won[0][i]
		for w := range workers {
			if won[w][i] != dst[i] {
				t.Fatalf("object %d: relocators disagree on the winner %#x vs %#x", i, won[w][i], dst[i])
			}
		}
	}

	// Publication before DONTNEED: one live object has no published copy,
	// so the source is its only copy and must not return to the OS, even
	// though every relocator accounted its bytes (duplicates must not
	// count twice).
	if LogosZgcRelease(pg) || LogosZgcPageReleased(pg) {
		t.Fatal("page released while a live object had no published copy")
	}
	last := uintptr(objects - 1)
	if LogosZgcWord(src+last*size) != uintptr(0x0101010101010101)*uintptr(objects) {
		t.Fatal("unpublished object lost its source bytes")
	}
	dst[last] = LogosZgcRelocate(pg, src+last*size, size, target(0, last))
	if !LogosZgcRelease(pg) || !LogosZgcPageReleased(pg) || !LogosZgcAllZero(src, LogosZgcPageSize) {
		t.Fatal("source page still holds data after every object was published")
	}

	before := LogosZgcHeals()
	for i := range uintptr(objects) {
		if got := LogosZgcLoad(&slots[i]); got != dst[i] {
			t.Fatalf("stale slot %d healed to %#x after release, want %#x", i, got, dst[i])
		}
		if LogosZgcWord(dst[i]) != uintptr(0x0101010101010101)*uintptr(i+1) {
			t.Fatalf("object %d lost its contents in the copy", i)
		}
	}
	if LogosZgcHeals()-before != objects {
		t.Fatalf("drain took %d heals, want %d", LogosZgcHeals()-before, objects)
	}

	// Address reuse: until a remap pass completes after the release, a
	// slot may still hold a source address, so the page stays registered
	// and resolves it.
	stale := LogosZgcColored(src+3*size, LogosZgcMark1)
	if LogosZgcRetire(pg) || LogosZgcPageFor(src) != pg {
		t.Fatal("page retired before a remap pass")
	}
	if got := LogosZgcLoad(&stale); got != dst[3] {
		t.Fatalf("stale word resolved to %#x before retirement, want %#x", got, dst[3])
	}
	LogosZgcRemapDone()
	if !LogosZgcRetire(pg) || LogosZgcPageFor(src) != nil {
		t.Fatal("page not retired after a remap pass")
	}
	retired = true
	// The range is free for a new page now, and a fresh word into it
	// names the new occupant, not the old copy.
	LogosZgcFill(src, size, 0x5a)
	reuse := LogosZgcNewPage(src, 0, 4)
	defer LogosZgcFreePage(reuse)
	fresh := src
	if got := LogosZgcLoad(&fresh); got != src || LogosZgcWord(got) != 0x5a5a5a5a5a5a5a5a {
		t.Fatalf("fresh word into the reused range resolved to %#x, want %#x", got, src)
	}
}

func TestLogosZgcIdentityHashSurvivesRelocation(t *testing.T) {
	const n = 5000
	const stride, moved = 16, 0x4000_0000
	base := uintptr(0x1000_0000)
	held := LogosZgcIdentityLen()
	seen := make(map[uint32]uintptr, n)
	for i := range uintptr(n) {
		a := base + i*stride
		id := LogosZgcIdentityHash(a)
		if id == 0 || seen[id] != 0 {
			t.Fatalf("address %#x got id %#x (zero or duplicate of %#x)", a, id, seen[id])
		}
		seen[id] = a
		if LogosZgcIdentityHash(a) != id {
			t.Fatalf("address %#x changed id between calls", a)
		}
	}
	h0 := LogosZgcPointerHash(base, 7)
	table := map[uint32]string{LogosZgcIdentityHash(base): "value"}
	for i := range uintptr(n) {
		LogosZgcIdentityMove(base+i*stride, base+moved+i*stride)
	}
	if got := table[LogosZgcIdentityHash(base+moved)]; got != "value" {
		t.Fatalf("lookup by identity after the move = %q", got)
	}
	if LogosZgcPointerHash(base+moved, 7) != h0 {
		t.Fatal("pointer-keyed map hash changed when the object moved")
	}
	if LogosZgcIdentityLen() < held+n {
		t.Fatalf("identity table lost entries: %d < %d", LogosZgcIdentityLen(), held+n)
	}
	for i := range uintptr(n) {
		LogosZgcIdentityForget(base + moved + i*stride)
	}
	if LogosZgcIdentityLen() != held {
		t.Fatalf("identity table holds %d entries after forgetting all, want %d", LogosZgcIdentityLen(), held)
	}
}

func TestLogosZgcIdentityHashConcurrent(t *testing.T) {
	const addrs, workers = 2000, 8
	base := uintptr(0x2000_0000)
	got := make([][]uint32, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range got {
		got[w] = make([]uint32, addrs)
		wg.Go(func() {
			<-start
			for i := range got[w] {
				got[w][i] = LogosZgcIdentityHash(base + uintptr(i)*16)
			}
		})
	}
	close(start)
	wg.Wait()
	unique := make(map[uint32]bool, addrs)
	for i := range addrs {
		unique[got[0][i]] = true
		for w := 1; w < workers; w++ {
			if got[w][i] != got[0][i] {
				t.Fatalf("address %d: workers disagree on identity %#x vs %#x", i, got[w][i], got[0][i])
			}
		}
	}
	if len(unique) != addrs {
		t.Fatalf("%d distinct ids for %d addresses", len(unique), addrs)
	}
	for i := range addrs {
		LogosZgcIdentityForget(base + uintptr(i)*16)
	}
}

func BenchmarkLogosZgcUnmask(b *testing.B) {
	LogosZgcSelectSoftware(LogosZgcRemapped)
	const addr = 0xc0_0012_3000
	v := LogosZgcColored(addr, LogosZgcMark0)
	flag := uintptr(LogosZgcSoftBits(LogosZgcMark0))
	for b.Loop() {
		v = LogosZgcAddr(v | flag)
	}
	if v != addr {
		b.Fatalf("unmask chain ended at %#x", v)
	}
	logosZgcReportRSS(b)
}
