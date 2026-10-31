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
	"unsafe"

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

// logosZgcRequireHeapWindow skips a test whose contract lives in the zgc
// collector's own machinery: the self-heal slow path, its accounting, and
// the release-drain-retire lifecycle. An immix-linked binary neither
// drives nor honours those paths, so the fixtures would plant coloured
// words in a build with no zgc barrier and take the whole binary down.
func logosZgcRequireHeapWindow(t *testing.T) {
	t.Helper()
	if LogosZgcCycleEnabled() {
		return
	}
	t.Skip("needs the zgc collector's low heap window: run with -ldflags=-X=runtime.logosGCMode=zgc")
}

func TestLogosZgcSoftwareBarrier(t *testing.T) {
	logosZgcRequireHeapWindow(t)
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
	logosZgcRequireHeapWindow(t)
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
	logosZgcRequireHeapWindow(t)
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

// Ordering pin: a live foreign hold keeps the source page unreleased,
// and the storage chain (release → heal → drain → remap → retire)
// respects ownership at every step. The hold is published through the
// real bounded hold protocol the foreign boundary emits —
// logosZgcForeignHoldEnter canonicalizes the root and claims a slot
// (logos_zgc_boundary.go:315), no direct slot manipulation — and the
// witnessed gate is zgcPageHasActiveHold (logos_zgc_boundary.go:427),
// the exact predicate the pause release loop consults before it may
// call zgcReleasePage (logos_zgc_cycle.go:95). Exit clears the hold,
// so every later gate proceeds on its own merits.
//
// A regression that drops the hold from the gate query fails the
// active-hold assertions while the hold is live; a regression that
// releases or zeroes before the drain completes fails the intact-bytes
// assertions; a regression that frees storage before the drain fails
// the retire gates; a regression that skips the remap pass fails the
// early retire.
func TestLogosZgcReleaseWaitsForForeignHold(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	if GOOS != "linux" {
		t.Skip("blocked: zero fill after release is a property of madvise(MADV_DONTNEED) on Linux")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()
	LogosZgcForeignInit()

	run := LogosZgcAllocRun([]int{LogosZgcRunManual, LogosZgcRunHeap, LogosZgcRunHeap, LogosZgcRunHeap})
	if run == nil {
		t.Fatal("no tight span run in one 2 MB region after 256 trials")
	}
	for i := 3; i >= 0; i-- {
		t.Cleanup(func() { run.Free(i) })
	}
	const head, s0, s1, ts = 0, 1, 2, 3
	elem := run.Elem(s0)
	if elem == 0 || run.Elem(s1) != elem {
		t.Fatal("source spans have no common object size")
	}
	if run.Nelems(s0) < 1 || run.Nelems(s1) < 1 {
		t.Fatal("source span holds no object")
	}
	// The two hand-built source objects are real objects of their spans:
	// hand their slots out through the allocator's own occupancy
	// accounting, so the claim path's published-copy decrement
	// (zgcCopyOneResult) moves a real count to zero instead of wrapping
	// an uncounted span below zero, and retire's live-allocation vet
	// sees the truth.
	run.Occupy(s0, 0)
	run.Occupy(s1, 0)

	region := run.Base(0) &^ (LogosZgcPageSize - 1)
	pg := LogosZgcNewPage(region, 2*elem, 2)
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
	})

	// Selection order is not address order; record reversed so the
	// prepare sort is exercised.
	LogosZgcRecordSources(pg, run, []int{s1, s0})
	var eligible, reserved bool
	LogosZgcWithWorldStopped(func() {
		eligible = LogosZgcEligible(pg)
		reserved = LogosZgcReserveToSpace(pg)
	})
	if !eligible {
		t.Fatal("page with adjacent sources is ineligible")
	}
	if !reserved {
		t.Fatal("page reserved no to-space span for the copies")
	}
	ranges, exact := LogosZgcPageSources(pg)
	if !exact || len(ranges) != 1 || ranges[0] != [2]uintptr{run.Base(s0), run.Base(s1) + run.Bytes(s1)} {
		t.Fatalf("source ranges = %v, want one coalesced range", ranges)
	}

	LogosZgcFill(run.Base(s0), run.Bytes(s0), 0xA1)
	LogosZgcFill(run.Base(s1), run.Bytes(s1), 0xA2)
	LogosZgcFill(run.Base(head), run.Bytes(head), 0xE0)
	LogosZgcFill(run.Base(ts), run.Bytes(ts), 0x5C)

	// Publish every live byte first, so the release below is decided by
	// the ownership gates alone, never by an unpublished byte. The
	// copies commit through the production claim-and-commit path, so
	// each published copy leaves its source count and the winners are
	// the addresses the heal and retire lifecycle actually names.
	src := []uintptr{run.Base(s0), run.Base(s1)}
	dst := make([]uintptr, 2)
	for i, si := range []int{s0, s1} {
		won, st := LogosZgcCopyOneResult(pg, run, si, src[i], elem)
		if st != LogosZgcRelocateDone || won == 0 {
			t.Fatalf("copy of %#x status %d winner %#x, want a committed copy", src[i], st, won)
		}
		dst[i] = won
	}
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()

	// Live foreign hold: the boundary thread publishes its hold at
	// entry, naming the source object as the direct root, and the page
	// the root names must answer the pause gate until the hold ends.
	h := LogosZgcForeignNewG(8)
	gpPtr := *(*unsafe.Pointer)(unsafe.Pointer(h))
	LogosZgcForeignGSetStatus(gpPtr, LogosZgcGRunning)
	holds := LogosZgcForeignHoldCount()
	if slot := LogosZgcForeignHoldEnter(gpPtr, src[0]); slot < 0 {
		t.Fatal("foreign hold enter found no free slot in an empty table")
	}
	defer LogosZgcForeignHoldExit(gpPtr)
	if d := LogosZgcForeignHoldDepth(gpPtr); d != 1 {
		t.Fatalf("hold depth = %d, want 1", d)
	}
	if c := LogosZgcForeignHoldCount(); c != holds+1 {
		t.Fatalf("hold count = %d, want %d", c, holds+1)
	}
	if !LogosZgcPageHasActiveHold(unsafe.Pointer(pg)) {
		t.Fatal("page of the held root reports no active hold")
	}
	for i, fill := range []byte{0xA1, 0xA2} {
		if off := LogosZgcFirstMismatch(run.Base(s0+i), run.Bytes(s0+i), fill); off >= 0 {
			t.Fatalf("source span %d byte %#x changed under the foreign hold", s0+i, run.Base(s0+i)+uintptr(off))
		}
	}
	if LogosZgcPageFor(src[0]) != pg {
		t.Fatal("page lost its registration under a live foreign hold")
	}
	LogosZgcForeignHoldExit(gpPtr)
	if d := LogosZgcForeignHoldDepth(gpPtr); d != 0 {
		t.Fatalf("post-exit hold depth = %d, want 0", d)
	}
	if c := LogosZgcForeignHoldCount(); c != holds {
		t.Fatalf("post-exit hold count = %d, want %d", c, holds)
	}
	if LogosZgcPageHasActiveHold(unsafe.Pointer(pg)) {
		t.Fatal("hold outlived its Exit")
	}

	// Hold ended: the source returns to the OS with DONTNEED, exactly
	// the prepared ranges zero, and every copy keeps its bytes.
	if !LogosZgcRelease(pg) || !LogosZgcPageReleased(pg) {
		t.Fatal("release refused after the foreign hold ended")
	}
	for i, fill := range []byte{0xA1, 0xA2} {
		if !LogosZgcAllZero(run.Base(s0+i), run.Bytes(s0+i)) {
			t.Fatalf("source span %d not zeroed after release", s0+i)
		}
		if off := LogosZgcFirstMismatch(dst[i], elem, fill); off >= 0 {
			t.Fatalf("copy %d lost its contents at +%d after release", i, off)
		}
	}
	if LogosZgcByteAt(run.Base(head)) != 0xE0 || LogosZgcByteAt(run.Base(head)+run.Bytes(head)-1) != 0xE0 {
		t.Fatal("head bytes touched by the release")
	}
	if off := LogosZgcFirstMismatch(run.Base(ts), run.Bytes(ts), 0x5C); off >= 0 {
		t.Fatalf("to-space span clobbered at +%d", run.Base(ts)+uintptr(off))
	}

	// Self-heal drains through the off-heap forwarding table after the
	// source is zeroed, one heal per stale word.
	before := LogosZgcHeals()
	heal := LogosZgcColored(src[1], LogosZgcMark0)
	if got := LogosZgcLoad(&heal); got != dst[1] {
		t.Fatalf("stale slot healed to %#x after release, want %#x", got, dst[1])
	}
	if n := LogosZgcHeals() - before; n != 1 {
		t.Fatalf("drain took %d heals, want 1", n)
	}

	// The filled to-space spans join the swept sets the way the
	// mark-termination pause publishes them, draining the page's
	// destination list; retirement still needs a remap pass first.
	LogosZgcWithWorldStopped(func() { LogosZgcPublishToSpaceSpans(pg) })
	if n := LogosZgcPageStateToSpaceLen(pg); n != 0 {
		t.Fatalf("publish left %d to-space spans", n)
	}

	// A remap pass is the first ownership retirement needs.
	var early bool
	LogosZgcWithWorldStopped(func() { early = LogosZgcRetire(pg) })
	if early || LogosZgcPageFor(src[0]) != pg {
		t.Fatal("page retired before a remap pass completed after release")
	}
	LogosZgcRemapDone()

	var late bool
	LogosZgcWithWorldStopped(func() { late = LogosZgcRetire(pg) })
	if !late || LogosZgcPageFor(src[0]) != nil {
		t.Fatal("page not retired after the hold ended and a remap pass")
	}
	retired = true
	run.MarkFreed(s0)
	run.MarkFreed(s1)
}

// Copy hold: the meaningful foreign hold on a canonical copy survives
// the source page's retire. The admission names the copy itself — the
// exact address production handed the foreign caller — never a stale
// source word: the copy commit refuses to re-copy a non-source object
// (zgcRelocateObject returns 0 for anything outside the prepared
// source ranges, logos_zgc_page.go:745-748), so zgcForeignAdmitBaseSTW
// owns the copy in place, refcounts zgcForeignObjects[canon] and pins
// zgcForeignRegionKey(canon) (logos_zgc_foreign_registry.go:1166-1177).
// End releases exactly those canon-keyed records
// (zgcForeignRetireLocked, logos_zgc_foreign_registry.go:1758-1765);
// the forwarding table of the old source base is never consulted.
//
// With release and a remap pass done, the source page may retire under
// the live hold: the caller's word was rewritten to the copy before
// admission published (zgcForeignAdmitPendingApply), and once the
// table is gone no functional consumer resolves the old base — the
// load barrier's unregistered-page arm strips color with no heal
// (logos_zgc.go:264-281). What must hold instead is the copy: the
// token stays valid and the copy stays exposed across the retire, the
// payload is intact after End, and End leaves nothing exposed. The
// retire return is captured exactly once: when the to-space copy
// shares the page's region the region pin refuses the drain while the
// frame is live and only End clears it; when the copy landed elsewhere
// the source retires under the hold. Both outcomes carry the same
// contract, so the fixture asserts both without depending on
// allocator placement.
func TestLogosZgcCopyHoldSurvivesSourceRetire(t *testing.T) {
	logosZgcRequireHeapWindow(t)
	if GOOS != "linux" {
		t.Skip("blocked: zero fill after release is a property of madvise(MADV_DONTNEED) on Linux")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()
	LogosZgcForeignInit()

	run := LogosZgcAllocRun([]int{LogosZgcRunManual, LogosZgcRunHeap, LogosZgcRunHeap, LogosZgcRunHeap})
	if run == nil {
		t.Fatal("no tight span run in one 2 MB region after 256 trials")
	}
	for i := 3; i >= 0; i-- {
		t.Cleanup(func() { run.Free(i) })
	}
	const head, s0, s1, ts = 0, 1, 2, 3
	elem := run.Elem(s0)
	if elem == 0 || run.Elem(s1) != elem {
		t.Fatal("source spans have no common object size")
	}
	if run.Nelems(s0) < 1 || run.Nelems(s1) < 1 {
		t.Fatal("source span holds no object")
	}
	// Real objects of their spans, so the published-copy decrement
	// moves a real count to zero and retire's live-allocation vet sees
	// the truth.
	run.Occupy(s0, 0)
	run.Occupy(s1, 0)

	region := run.Base(0) &^ (LogosZgcPageSize - 1)
	pg := LogosZgcNewPage(region, 2*elem, 2)
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
	})

	LogosZgcRecordSources(pg, run, []int{s1, s0})
	var eligible, reserved bool
	LogosZgcWithWorldStopped(func() {
		eligible = LogosZgcEligible(pg)
		reserved = LogosZgcReserveToSpace(pg)
	})
	if !eligible {
		t.Fatal("page with adjacent sources is ineligible")
	}
	if !reserved {
		t.Fatal("page reserved no to-space span for the copies")
	}
	ranges, exact := LogosZgcPageSources(pg)
	if !exact || len(ranges) != 1 || ranges[0] != [2]uintptr{run.Base(s0), run.Base(s1) + run.Bytes(s1)} {
		t.Fatalf("source ranges = %v, want one coalesced range", ranges)
	}

	LogosZgcFill(run.Base(s0), run.Bytes(s0), 0xA1)
	LogosZgcFill(run.Base(s1), run.Bytes(s1), 0xA2)
	LogosZgcFill(run.Base(head), run.Bytes(head), 0xE0)
	LogosZgcFill(run.Base(ts), run.Bytes(ts), 0x5C)

	// Publish every live byte first, so the release below is decided by
	// the ownership gates alone, never by an unpublished byte.
	src := []uintptr{run.Base(s0), run.Base(s1)}
	dst := make([]uintptr, 2)
	for i, si := range []int{s0, s1} {
		won, st := LogosZgcCopyOneResult(pg, run, si, src[i], elem)
		if st != LogosZgcRelocateDone || won == 0 {
			t.Fatalf("copy of %#x status %d winner %#x, want a committed copy", src[i], st, won)
		}
		dst[i] = won
	}
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()

	// frame1 owns the canonical copy before the source release. When
	// the copy shares the page's region the pin refuses the release
	// while the frame is live — the real source-release criterion —
	// and only End clears it; when the copy landed elsewhere the
	// release proceeds under the hold, the same copy-hold survival the
	// retire half asserts below. Both branches converge on the released
	// page with the copy intact.
	// The slot is a plain word temp admitted with Type nil (one opaque
	// word): the runtime reads and rewrites the cell exactly as before,
	// but no uintptr->unsafe.Pointer conversion exists for checkptr to
	// reject, and &temp1 is a real typed address.
	var temp1 uintptr = dst[0]
	rows1 := []unsafe.Pointer{unsafe.Pointer(&temp1), nil}
	frame1, gen1 := LogosZgcSyscallAdmit(unsafe.Pointer(&rows1[0]), 1)
	if !LogosZgcSyscallTokenValid(frame1, gen1) {
		t.Fatal("first admit returned invalid token with cycle enabled")
	}
	if temp1 != dst[0] {
		t.Fatalf("first admitted root = %#x, want the canonical copy owned in place", temp1)
	}
	if !LogosZgcSyscallExposed(dst[0]) {
		t.Fatal("canonical copy not exposed while the first frame is live")
	}
	if LogosZgcRelease(pg) {
		// Out-of-region geometry: the source releases under the hold.
		LogosZgcSyscallEnd(frame1, gen1)
		if LogosZgcSyscallExposed(dst[0]) {
			t.Fatal("canonical copy still exposed after End")
		}
	} else {
		// In-region geometry: the region pin refused the release.
		if LogosZgcPageReleased(pg) {
			t.Fatal("page released while the canonical copy's pin named its region")
		}
		if off := LogosZgcFirstMismatch(run.Base(s0), run.Bytes(s0), 0xA1); off >= 0 {
			t.Fatalf("source span byte %#x changed under the foreign hold", run.Base(s0)+uintptr(off))
		}
		LogosZgcSyscallEnd(frame1, gen1)
		if LogosZgcSyscallExposed(dst[0]) {
			t.Fatal("canonical copy still exposed after End")
		}
		if !LogosZgcRelease(pg) || !LogosZgcPageReleased(pg) {
			t.Fatal("release refused after the pin ended")
		}
	}
	// The release returned exactly the prepared ranges to the OS,
	// zeroed them, and left every copy and every byte outside the
	// ranges untouched.
	for i, fill := range []byte{0xA1, 0xA2} {
		if !LogosZgcAllZero(run.Base(s0+i), run.Bytes(s0+i)) {
			t.Fatalf("source span %d not zeroed after release", s0+i)
		}
		if off := LogosZgcFirstMismatch(dst[i], elem, fill); off >= 0 {
			t.Fatalf("copy %d lost its contents at +%d after release", i, off)
		}
	}
	if LogosZgcByteAt(run.Base(head)) != 0xE0 || LogosZgcByteAt(run.Base(head)+run.Bytes(head)-1) != 0xE0 {
		t.Fatal("head bytes touched by the release")
	}
	if off := LogosZgcFirstMismatch(run.Base(ts), run.Bytes(ts), 0x5C); off >= 0 {
		t.Fatalf("to-space span clobbered at +%d", run.Base(ts)+uintptr(off))
	}

	// The filled to-space spans join the swept sets the way the
	// mark-termination pause publishes them, draining the page's
	// destination list; a remap pass is the last ownership gate before
	// retirement.
	LogosZgcWithWorldStopped(func() { LogosZgcPublishToSpaceSpans(pg) })
	if n := LogosZgcPageStateToSpaceLen(pg); n != 0 {
		t.Fatalf("publish left %d to-space spans", n)
	}
	LogosZgcRemapDone()

	// frame2 owns the real canonical copy in place: valid token, the
	// slot keeps the copy address, the copy is exposed.
	// Same plain-word slot contract as frame1: Type nil, real typed
	// address, no fabricated conversion.
	var temp2 uintptr = dst[0]
	rows2 := []unsafe.Pointer{unsafe.Pointer(&temp2), nil}
	frame2, gen2 := LogosZgcSyscallAdmit(unsafe.Pointer(&rows2[0]), 1)
	if !LogosZgcSyscallTokenValid(frame2, gen2) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if temp2 != dst[0] {
		t.Fatalf("admitted root = %#x, want the canonical copy owned in place", temp2)
	}
	if !LogosZgcSyscallExposed(dst[0]) {
		t.Fatal("canonical copy not exposed while the frame is live")
	}

	// Capture the retire exactly once. In-region geometry: the pin
	// refuses the drain while frame2 is live. Elsewhere: the source
	// retires under the hold. Either way the freed page must never be
	// retired or dropped again, so the retired flag moves before any
	// later assertion can reach the cleanup.
	var retiredNow bool
	LogosZgcWithWorldStopped(func() { retiredNow = LogosZgcRetire(pg) })
	if retiredNow {
		retired = true
		run.MarkFreed(s0)
		run.MarkFreed(s1)
	}

	// The copy hold survives its source: the token is live and the
	// canonical copy is still exposed, whatever the source page's fate.
	if !LogosZgcSyscallTokenValid(frame2, gen2) {
		t.Fatal("frame lost its token across the source retire")
	}
	if !LogosZgcSyscallExposed(dst[0]) {
		t.Fatal("canonical copy not exposed across the source retire")
	}

	// End releases the canon-keyed records only, and the held payload
	// is intact afterwards.
	LogosZgcSyscallEnd(frame2, gen2)
	if LogosZgcSyscallExposed(dst[0]) {
		t.Fatal("canonical copy still exposed after End")
	}
	if !retiredNow && LogosZgcPageHasActiveHold(unsafe.Pointer(pg)) {
		t.Fatal("hold outlived its End")
	}
	if off := LogosZgcFirstMismatch(dst[0], elem, 0xA1); off >= 0 {
		t.Fatalf("held copy lost its payload at +%d", off)
	}

	if !retiredNow {
		// The pin named this page's region and refused the drain while
		// the frame was live; with the frame ended it proceeds.
		var late bool
		LogosZgcWithWorldStopped(func() { late = LogosZgcRetire(pg) })
		if !late || LogosZgcPageFor(src[0]) != nil {
			t.Fatal("page not retired after the pin ended and a remap pass")
		}
		retired = true
		run.MarkFreed(s0)
		run.MarkFreed(s1)
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
