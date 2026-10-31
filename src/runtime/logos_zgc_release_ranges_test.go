// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"
	"unsafe"

	. "runtime"
)

// Exact source-range release over real spans.
//
// Layout inside one 2 MB region, in address order:
//
//	head(manual) S0(heap) S1(heap) gap(heap, live) S2(heap) stk(manual)
//	ts(heap, to-space) S3(heap) tail(heap)
//
// S0+S1 are adjacent, so the prepared ranges coalesce to three:
// [S0,S1] [S2] [S3]. The test drives the runtime's own constructors and
// paths throughout: real spans from the page allocator, LogosZgcNewPage,
// LogosZgcRecordSources (the selection fill) + LogosZgcEligible (the vet,
// which prepares the ranges), LogosZgcRelocate (the commit), LogosZgcRelease
// (madvise) and LogosZgcRetire. No pin, goroutine status or syscall result
// is forged.
//
// Independent failure modes, each with its own assertion:
//   - gappy pages refused: Eligible must be true (fails while tiling is required)
//   - ranges not exact: PageSources must equal the three coalesced ranges
//   - whole-region release: boundary bytes of gap/stk/ts/head/tail survive
//   - under-release: every source byte is zero after release (Linux)
//   - off-by-one: last byte of each range zeroed, first byte past it intact
//   - leaking filter: PageFor/Remap see sources only, before and after release
//   - spilling commit: straddling and gap/ts relocates return 0, publish nothing
//   - gating: release refuses while one live object is unpublished
//   - identity too wide/narrow: gap and copy identities survive retire,
//     dead source identities are forgotten

const (
	logosZgcRHead = 0
	logosZgcRS0   = 1
	logosZgcRS1   = 2
	logosZgcRGap  = 3
	logosZgcRS2   = 4
	logosZgcRStk  = 5
	logosZgcRTs   = 6
	logosZgcRS3   = 7
	logosZgcRTail = 8
)

// logosZgcHolderRes is the stack-holder result. Package scope so the
// result channel and the holder signature name one identical type.
type logosZgcHolderRes struct {
	bad  int
	fill byte
}

func logosZgcRangeKinds() []int {
	return []int{
		LogosZgcRunManual,
		LogosZgcRunHeap, LogosZgcRunHeap, LogosZgcRunHeap, LogosZgcRunHeap,
		LogosZgcRunManual,
		LogosZgcRunHeap, LogosZgcRunHeap, LogosZgcRunHeap,
	}
}

func logosZgcCheckRangeBytes(t *testing.T, run *LogosZgcSpanRun, i int, b byte, what string) {
	t.Helper()
	if off := LogosZgcFirstMismatch(run.Base(i), run.Bytes(i), b); off >= 0 {
		t.Fatalf("%s byte %#x = %#x, want %#x (first mismatch at +%d)",
			what, run.Base(i)+uintptr(off), LogosZgcByteAt(run.Base(i)+uintptr(off)), b, off)
	}
}

func TestLogosZgcGappyRangeReleaseExact(t *testing.T) {
	// The page goes over real heap spans: only the zgc low heap window
	// keeps the granule registrable.
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

	run := LogosZgcAllocRun(logosZgcRangeKinds())
	if run == nil {
		t.Fatal("no tight span run in one 2 MB region after 256 trials")
	}
	for i := logosZgcRTail; i >= 0; i-- {
		t.Cleanup(func() { run.Free(i) })
	}

	elem := run.Elem(logosZgcRS0)
	if elem == 0 {
		t.Fatal("source span has no objects")
	}
	srcIdx := []int{logosZgcRS0, logosZgcRS1, logosZgcRS2, logosZgcRS3}
	type slot struct{ span, obj int }
	var live []slot
	for _, s := range srcIdx {
		for _, o := range []uintptr{0, 1} {
			if o >= run.Nelems(s) {
				t.Fatalf("span %d holds %d objects, want slot %d", s, run.Nelems(s), o)
			}
			live = append(live, slot{s, int(o)})
		}
	}

	region := run.Base(0) &^ (LogosZgcPageSize - 1)
	liveBytes := uintptr(len(live)) * elem
	pg := LogosZgcNewPage(region, liveBytes, len(live))
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
	})

	// Selection order is not address order; record reversed so the
	// prepare sort is exercised.
	LogosZgcRecordSources(pg, run, []int{logosZgcRS3, logosZgcRS2, logosZgcRS1, logosZgcRS0})
	// Selection-pause vet: zgcPageEligible asserts a stopped world under
	// staticlockranking (via zgcForeignPagePinnedSTW), and the collector
	// only vets inside zgcSelectPages in the mark-termination pause.
	var eligible bool
	LogosZgcWithWorldStopped(func() { eligible = LogosZgcEligible(pg) })
	if !eligible {
		t.Fatal("gappy page refused: sources around live gap/manual spans are ineligible")
	}
	got, exact := LogosZgcPageSources(pg)
	if !exact {
		t.Fatal("prepared page is not exact")
	}
	want := [][2]uintptr{
		{run.Base(logosZgcRS0), run.Base(logosZgcRS1) + run.Bytes(logosZgcRS1)},
		{run.Base(logosZgcRS2), run.Base(logosZgcRS2) + run.Bytes(logosZgcRS2)},
		{run.Base(logosZgcRS3), run.Base(logosZgcRS3) + run.Bytes(logosZgcRS3)},
	}
	if len(got) != len(want) {
		t.Fatalf("source ranges = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("source range %d = %#x-%#x, want %#x-%#x", i, got[i][0], got[i][1], want[i][0], want[i][1])
		}
	}

	// Fill: every source span a distinct nonzero pattern, gaps/manual/
	// to-space/head/tail their own sentinels.
	srcFill := map[int]byte{logosZgcRS0: 0xA1, logosZgcRS1: 0xA2, logosZgcRS2: 0xA3, logosZgcRS3: 0xA4}
	for _, s := range srcIdx {
		LogosZgcFill(run.Base(s), run.Bytes(s), srcFill[s])
	}
	LogosZgcFill(run.Base(logosZgcRGap), run.Bytes(logosZgcRGap), 0xC0)
	LogosZgcFill(run.Base(logosZgcRStk), run.Bytes(logosZgcRStk), 0xD0)
	LogosZgcFill(run.Base(logosZgcRTs), run.Bytes(logosZgcRTs), 0x5C)
	LogosZgcFill(run.Base(logosZgcRHead), run.Bytes(logosZgcRHead), 0xE0)
	LogosZgcFill(run.Base(logosZgcRTail), run.Bytes(logosZgcRTail), 0xE1)
	for _, s := range srcIdx {
		logosZgcCheckRangeBytes(t, run, s, srcFill[s], "source fill")
	}

	// Identities: live sources, first dead object of each source span,
	// and two gap objects.
	baseIDs := LogosZgcIdentityLen()
	liveID := make(map[uintptr]uint32)
	for _, l := range live {
		a := run.Base(l.span) + uintptr(l.obj)*elem
		liveID[a] = LogosZgcIdentityHash(a)
	}
	deadID := make(map[uintptr]uint32)
	for _, s := range srcIdx {
		if run.Nelems(s) > 2 {
			a := run.Base(s) + 2*elem
			deadID[a] = LogosZgcIdentityHash(a)
		}
	}
	gapAddrs := []uintptr{run.Base(logosZgcRGap), run.Base(logosZgcRGap) + elem}
	gapID := make(map[uintptr]uint32)
	for _, a := range gapAddrs {
		gapID[a] = LogosZgcIdentityHash(a)
	}

	// Pre-release filter: sources resolve to the page, everything else
	// (head, gap, manual, to-space, tail) resolves to nil, and nothing
	// remaps yet.
	for _, s := range srcIdx {
		for _, a := range []uintptr{run.Base(s), run.Base(s) + run.Bytes(s) - 1} {
			if LogosZgcPageFor(a) != pg {
				t.Fatalf("source edge %#x does not resolve to the page", a)
			}
			if LogosZgcRemap(a) != a {
				t.Fatalf("unpublished source %#x remaps to %#x", a, LogosZgcRemap(a))
			}
		}
	}
	for _, i := range []int{logosZgcRHead, logosZgcRGap, logosZgcRStk, logosZgcRTs, logosZgcRTail} {
		a := run.Base(i) + run.Bytes(i)/2
		if LogosZgcPageFor(a) != nil {
			t.Fatalf("gap/manual/to-space/head/tail %#x resolves to the page", a)
		}
		if LogosZgcRemap(a) != a {
			t.Fatalf("gap address %#x remaps to %#x", a, LogosZgcRemap(a))
		}
	}

	// Spilling commits are refused and publish nothing: a source object
	// straddling S1 into the gap, a gap object, and a to-space word.
	straddle := run.Base(logosZgcRS1) + run.Bytes(logosZgcRS1) - elem/2
	if got := LogosZgcRelocate(pg, straddle, elem, run.Base(logosZgcRTs)); got != 0 {
		t.Fatalf("straddling relocate = %#x, want 0", got)
	}
	if _, ok := LogosZgcForward(pg, straddle); ok {
		t.Fatal("straddling relocate published a forwarding entry")
	}
	if got := LogosZgcRelocate(pg, run.Base(logosZgcRGap), elem, run.Base(logosZgcRTs)); got != 0 {
		t.Fatalf("gap relocate = %#x, want 0", got)
	}
	if got := LogosZgcRelocate(pg, run.Base(logosZgcRTs), elem, run.Base(logosZgcRTs)+elem); got != 0 {
		t.Fatalf("to-space relocate = %#x, want 0", got)
	}
	logosZgcCheckRangeBytes(t, run, logosZgcRTs, 0x5C, "to-space after refused relocates")
	logosZgcCheckRangeBytes(t, run, logosZgcRGap, 0xC0, "gap after refused relocates")

	// Relocate all live objects but the last: release must refuse and
	// change nothing.
	dstCursor := uintptr(0)
	dstOf := make(map[uintptr]uintptr)
	for _, l := range live[:len(live)-1] {
		from := run.Base(l.span) + uintptr(l.obj)*elem
		dst := run.Base(logosZgcRTs) + dstCursor*elem
		if got := LogosZgcRelocate(pg, from, elem, dst); got != dst {
			t.Fatalf("relocate %#x = %#x, want %#x", from, got, dst)
		}
		dstOf[from] = dst
		dstCursor++
	}
	if LogosZgcRelease(pg) {
		t.Fatal("page released while a live object had no published copy")
	}
	for _, s := range srcIdx {
		logosZgcCheckRangeBytes(t, run, s, srcFill[s], "source after refused release")
	}
	logosZgcCheckRangeBytes(t, run, logosZgcRGap, 0xC0, "gap after refused release")

	// Publish the last live object; release must now take exactly the
	// three source ranges.
	last := live[len(live)-1]
	lastFrom := run.Base(last.span) + uintptr(last.obj)*elem
	lastDst := run.Base(logosZgcRTs) + dstCursor*elem
	if got := LogosZgcRelocate(pg, lastFrom, elem, lastDst); got != lastDst {
		t.Fatalf("relocate %#x = %#x, want %#x", lastFrom, got, lastDst)
	}
	dstOf[lastFrom] = lastDst
	for from, dst := range dstOf {
		if LogosZgcHasIdentity(from) {
			t.Fatalf("source %#x still holds an identity after the move", from)
		}
		if !LogosZgcHasIdentity(dst) {
			t.Fatalf("copy %#x holds no identity after the move", dst)
		}
		if LogosZgcIdentityHash(dst) != liveID[from] {
			t.Fatalf("copy %#x lost its identity", dst)
		}
	}
	if !LogosZgcRelease(pg) {
		t.Fatal("release refused a fully published gappy page")
	}

	if GOOS == "linux" {
		for _, s := range srcIdx {
			if !LogosZgcAllZero(run.Base(s), run.Bytes(s)) {
				t.Fatalf("source span %d not zeroed after release", s)
			}
		}
		// Off-by-one in both directions at every range edge.
		for _, r := range want {
			if LogosZgcByteAt(r[0]) != 0 || LogosZgcByteAt(r[1]-1) != 0 {
				t.Fatalf("range edge %#x-%#x not zeroed", r[0], r[1])
			}
		}
		for _, i := range []int{logosZgcRHead, logosZgcRGap, logosZgcRStk, logosZgcRTs, logosZgcRTail} {
			for _, a := range []uintptr{run.Base(i), run.Base(i) + run.Bytes(i) - 1} {
				if LogosZgcByteAt(a) == 0 {
					t.Fatalf("neighbour byte %#x zeroed by the release", a)
				}
			}
		}
	}
	logosZgcCheckRangeBytes(t, run, logosZgcRGap, 0xC0, "gap after release")
	logosZgcCheckRangeBytes(t, run, logosZgcRStk, 0xD0, "manual span after release")
	logosZgcCheckRangeBytes(t, run, logosZgcRHead, 0xE0, "head after release")
	logosZgcCheckRangeBytes(t, run, logosZgcRTail, 0xE1, "tail after release")

	// Copies kept their contents and the untouched to-space tail is
	// still the sentinel: the release touched ranges only.
	cursor := uintptr(0)
	for _, l := range live {
		dst := run.Base(logosZgcRTs) + cursor*elem
		if off := LogosZgcFirstMismatch(dst, elem, srcFill[l.span]); off >= 0 {
			t.Fatalf("copy %d of span %d lost its contents at +%d", l.obj, l.span, off)
		}
		cursor++
	}
	used := cursor * elem
	if tail := run.Bytes(logosZgcRTs) - used; tail > 0 {
		if off := LogosZgcFirstMismatch(run.Base(logosZgcRTs)+used, tail, 0x5C); off >= 0 {
			t.Fatalf("to-space tail clobbered at +%d", used+uintptr(off))
		}
	}

	// Stale source slots heal to their copies (interior offset included);
	// gap and to-space words are returned as-is.
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()
	first := live[0]
	firstFrom := run.Base(first.span) + uintptr(first.obj)*elem
	stale := LogosZgcColored(firstFrom, LogosZgcMark0)
	if got := LogosZgcLoad(&stale); got != dstOf[firstFrom] {
		t.Fatalf("stale slot healed to %#x, want %#x", got, dstOf[firstFrom])
	}
	interior := LogosZgcColored(firstFrom+40, LogosZgcMark1)
	if got := LogosZgcLoad(&interior); got != dstOf[firstFrom]+40 {
		t.Fatalf("stale interior slot healed to %#x, want %#x", got, dstOf[firstFrom]+40)
	}
	gapWord := LogosZgcColored(run.Base(logosZgcRGap)+16, LogosZgcMark0)
	if got := LogosZgcRemap(LogosZgcAddr(gapWord)); got != run.Base(logosZgcRGap)+16 {
		t.Fatalf("gap word remapped to %#x", got)
	}
	for _, s := range srcIdx {
		for _, a := range []uintptr{run.Base(s), run.Base(s) + run.Bytes(s) - 1} {
			if LogosZgcPageFor(a) != pg {
				t.Fatalf("released source edge %#x lost its page", a)
			}
		}
	}

	// Retire only after a remap pass; it frees exactly the quarantined
	// spans and forgets exactly the source-range identities. Retire runs
	// in the mark-termination pause (zgcRetirePendingPages asserts a
	// stopped world under staticlockranking), so both probes stop the
	// world the way the collector does; relocation and release above stay
	// outside because evacuators copy concurrently and the release seam
	// takes no STW assertion.
	var retiredEarly, retiredLate bool
	LogosZgcWithWorldStopped(func() { retiredEarly = LogosZgcRetire(pg) })
	if retiredEarly {
		t.Fatal("page retired before a remap pass completed after release")
	}
	LogosZgcRemapDone()
	LogosZgcWithWorldStopped(func() { retiredLate = LogosZgcRetire(pg) })
	if !retiredLate {
		t.Fatal("page not retired after a remap pass")
	}
	retired = true
	for _, s := range srcIdx {
		run.MarkFreed(s)
		if LogosZgcPageFor(run.Base(s)) != nil {
			t.Fatalf("retired source %#x still registered", run.Base(s))
		}
	}
	for _, a := range gapAddrs {
		if !LogosZgcHasIdentity(a) || LogosZgcIdentityHash(a) != gapID[a] {
			t.Fatalf("gap object %#x lost its identity at retire", a)
		}
	}
	for a := range deadID {
		if LogosZgcHasIdentity(a) {
			t.Fatalf("dead source object %#x kept its identity at retire", a)
		}
	}
	for from, dst := range dstOf {
		if !LogosZgcHasIdentity(dst) || LogosZgcIdentityHash(dst) != liveID[from] {
			t.Fatalf("copy %#x lost its identity at retire", dst)
		}
	}
	if n := LogosZgcIdentityLen() - baseIDs; n != len(dstOf)+len(gapID) {
		t.Fatalf("identity table holds %d new entries, want %d", n, len(dstOf)+len(gapID))
	}
	for _, dst := range dstOf {
		LogosZgcIdentityForget(dst)
	}
	for _, a := range gapAddrs {
		LogosZgcIdentityForget(a)
	}
	for a := range deadID {
		LogosZgcIdentityForget(a)
	}
}

// The vet rejects what selection must never own: an empty list, a manual
// span, and spans outside the page region. None of them may prepare
// ranges (a rejected page owns no byte).
func TestLogosZgcGappyRangeVetRejectsNonSources(t *testing.T) {
	// AllocRun only accepts a tight real-span run inside a registrable
	// granule; above the field zgcTestRunTight rejects every trial and
	// the run comes back nil. Only the zgc low heap window can satisfy it.
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()

	run := LogosZgcAllocRun([]int{LogosZgcRunHeap, LogosZgcRunManual})
	if run == nil {
		t.Fatal("no tight span run after 256 trials")
	}
	t.Cleanup(func() { run.Free(0); run.Free(1) })
	region := run.Base(0) &^ (LogosZgcPageSize - 1)

	pg := LogosZgcNewDetachedPageAt(region, LogosZgcPageSize)
	t.Cleanup(func() { LogosZgcForgetPage(pg); LogosZgcFreePage(pg) })

	// Selection-pause vet under a real world stop (see above): Eligible
	// asserts a stopped world under staticlockranking, and the collector
	// only vets in the selection pause.
	LogosZgcRecordSources(pg, run, nil)
	var vetEmpty bool
	LogosZgcWithWorldStopped(func() { vetEmpty = LogosZgcEligible(pg) })
	if vetEmpty {
		t.Fatal("page with no source spans is eligible")
	}
	if _, exact := LogosZgcPageSources(pg); exact {
		t.Fatal("rejected page prepared ranges")
	}

	LogosZgcRecordSources(pg, run, []int{1})
	var vetManual bool
	LogosZgcWithWorldStopped(func() { vetManual = LogosZgcEligible(pg) })
	if vetManual {
		t.Fatal("page owning a manual span is eligible")
	}

	if next := region + LogosZgcPageSize; next <= uintptr(LogosZgcAddrMask)-(LogosZgcPageSize-1) {
		away := LogosZgcNewDetachedPageAt(next, LogosZgcPageSize)
		t.Cleanup(func() { LogosZgcForgetPage(away); LogosZgcFreePage(away) })
		LogosZgcRecordSources(away, run, []int{0})
		var vetAway bool
		LogosZgcWithWorldStopped(func() { vetAway = LogosZgcEligible(away) })
		if vetAway {
			t.Fatal("page owning a span outside its region is eligible")
		}
	}
}

// Releasing a page that was never prepared over heap memory throws
// instead of zeroing memory the page cannot account for. Runs in a
// subprocess: a throw cannot be recovered in-process.
func TestLogosZgcUnpreparedHeapPageReleaseThrows(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") != "" {
		defer debug.SetGCPercent(debug.SetGCPercent(-1))
		obj := new([64]byte)
		base := uintptr(unsafe.Pointer(obj)) &^ (LogosZgcPageSize - 1)
		pg := LogosZgcNewDetachedPageAt(base, LogosZgcPageSize)
		defer LogosZgcDropPage(pg)
		t.Log("ARMED")
		if LogosZgcRelease(pg) {
			t.Log("RELEASED")
		}
		return
	}
	testenv.MustHaveGoBuild(t)
	exp := os.Getenv("GOEXPERIMENT")
	if !strings.Contains(","+exp+",", ",logosgcmoving,") {
		exp = strings.TrimPrefix(exp+",logosgcmoving", ",")
	}
	cmd := exec.Command(testenv.GoToolPath(t), "test", "-count=1", "-v",
		"-run=^TestLogosZgcUnpreparedHeapPageReleaseThrows$", "runtime")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "LOGOS_ZGC_INNER=1", "GOGC=off", "GOEXPERIMENT="+exp)
	// A throw exits the test binary nonzero; quiet refusal would exit
	// zero and a wrongful release would log RELEASED. No diagnostics
	// text is matched: any wording of the fail-closed path passes.
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("inner run succeeded, want a throw\n%s", out)
	}
	// ARMED proves the inner binary built and reached the release
	// call, so a nonzero exit cannot be a build break or an early
	// t.Fatal. No throw wording is matched.
	if !strings.Contains(string(out), "ARMED") {
		t.Fatalf("inner run died before reaching the release\n%s", out)
	}
	if strings.Contains(string(out), "--- FAIL") {
		t.Fatalf("inner run failed without throwing\n%s", out)
	}
	if strings.Contains(string(out), "RELEASED") {
		t.Fatalf("unprepared page released heap memory\n%s", out)
	}
}

// Public live-heap half of the observation limit in the export seam: with
// the real cycle running, fragmented heaps interleaved with goroutine
// stacks keep their neighbours and their identities across collections.
// It logs how many gappy pages the cycle actually registered, and skips
// when this heap shape produced none, since the deterministic range
// assertions above own the must-cover part.
func TestLogosZgcLiveHeapGappyReleaseKeepsNeighbours(t *testing.T) {
	if !LogosZgcCycleEnabled() {
		t.Skip("needs the zgc cycle; run under -gc=zgc")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))

	const holders = 4
	resCh := make(chan logosZgcHolderRes, holders)
	startCh := make(chan struct{})
	doneCh := make(chan struct{})
	for range holders {
		go logosZgcHoldDeepStack(0x70, startCh, doneCh, resCh)
	}
	close(startCh)

	type node struct {
		val  int
		next *node
		pad  [104]byte
	}
	var kept []*node
	for b := range 20 {
		for i := range 10000 {
			n := &node{val: b*100000 + i}
			for j := range n.pad {
				n.pad[j] = byte(i + j)
			}
			if i%5 == 0 {
				kept = append(kept, n)
			}
		}
	}
	sawGappy := 0
	t.Cleanup(func() { logosZgcSink = nil })
	for r := range 6 {
		GC()
		// Touch the heap between cycles so stacks and heap spans
		// interleave differently each round. Pinned through the
		// shared sink so the churn really lands on the heap.
		var trash []any
		for i := range 20000 {
			trash = append(trash, &node{val: -r*100000 - i})
		}
		logosZgcSink = trash
		if n := LogosZgcGappyPages(); n > sawGappy {
			sawGappy = n
		}
		for _, n := range kept {
			want := n.val % 100000
			if want < 0 {
				want = -want
			}
			for j := range n.pad {
				if n.pad[j] != byte(want+j) {
					t.Fatalf("kept node %d pad byte %d = %#x", n.val, j, n.pad[j])
				}
			}
		}
	}
	close(doneCh)
	timeout := time.After(30 * time.Second)
	for range holders {
		select {
		case r := <-resCh:
			if r.bad != 0 {
				t.Fatalf("stack holder (fill %#x) lost %d bytes across gappy releases", r.fill, r.bad)
			}
		case <-timeout:
			t.Fatal("stack holder did not return")
		}
	}
	kept = nil
	GC()
	t.Logf("saw %d gappy registered pages", sawGappy)
	if sawGappy == 0 {
		t.Skip("this heap shape registered no gappy page; deterministic coverage is in TestLogosZgcGappyRangeReleaseExact")
	}
}

//go:noinline
func logosZgcHoldDeepStack(fill byte, start <-chan struct{}, done <-chan struct{}, res chan<- logosZgcHolderRes) {
	<-start
	bad := logosZgcCheckDeepStack(fill, 0, done)
	res <- logosZgcHolderRes{bad, fill}
}

//go:noinline
func logosZgcCheckDeepStack(fill byte, depth int, done <-chan struct{}) int {
	var frame [4096]byte
	for i := range frame {
		frame[i] = fill
	}
	if depth < 24 {
		return logosZgcCheckDeepStack(fill, depth+1, done)
	}
	<-done
	bad := 0
	for _, b := range frame {
		if b != fill {
			bad++
		}
	}
	return bad
}

// Ordering pin: madvise(MADV_DONTNEED) runs inside zgcReleasePage only
// after the evacuation drain completes (every live byte has a published
// copy: the published == live gate at logos_zgc_page.go:625) and before
// the self-heal drain completes (the remap pass that heals every stale
// slot, zgcRemapPassComplete at logos_zgc_cycle.go:336, runs after the
// release: the page retires only once zgcRemapEpoch advances past the
// release epoch, logos_zgc_quarantine.go:620-623). The madvise itself is
// zgcReleasePages at logos_zgc_release_linux.go:13-14, reached through
// releaseSources (logos_zgc_page.go:637) after the foreign-pin gate
// (logos_zgc_page.go:631-635) refuses a pinned region.
//
// A regression that returns the source to the OS before the drain (for
// example, releasing with a live object still unpublished) fails the
// refusal and intact-bytes assertions below; a regression that frees the
// storage before the drain (retiring without a remap pass) fails the
// retire gate.
func TestLogosZgcReleaseNeedsPublishedDrain(t *testing.T) {
	// The page goes over real heap spans: only the zgc low heap window
	// keeps the granule registrable.
	logosZgcRequireHeapWindow(t)
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	GC()
	GC()

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

	region := run.Base(0) &^ (LogosZgcPageSize - 1)
	from := []uintptr{run.Base(s0), run.Base(s1)}
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
	var eligible bool
	LogosZgcWithWorldStopped(func() { eligible = LogosZgcEligible(pg) })
	if !eligible {
		t.Fatal("page with adjacent sources is ineligible")
	}
	got, exact := LogosZgcPageSources(pg)
	if !exact || len(got) != 1 || got[0] != [2]uintptr{run.Base(s0), run.Base(s1) + run.Bytes(s1)} {
		t.Fatalf("source ranges = %v, want one coalesced range", got)
	}

	LogosZgcFill(run.Base(s0), run.Bytes(s0), 0xA1)
	LogosZgcFill(run.Base(s1), run.Bytes(s1), 0xA2)
	LogosZgcFill(run.Base(head), run.Bytes(head), 0xE0)
	LogosZgcFill(run.Base(ts), run.Bytes(ts), 0x5C)

	// Evacuation drain incomplete: one live object has no published
	// copy, so its source bytes are the only copy and the page must
	// not return to the OS.
	dst := []uintptr{run.Base(ts), run.Base(ts) + elem}
	if got := LogosZgcRelocate(pg, from[0], elem, dst[0]); got != dst[0] {
		t.Fatalf("relocate %#x = %#x, want %#x", from[0], got, dst[0])
	}
	if LogosZgcRelease(pg) || LogosZgcPageReleased(pg) {
		t.Fatal("page released before the evacuation drain completed")
	}
	if off := LogosZgcFirstMismatch(run.Base(s0), run.Bytes(s0), 0xA1); off >= 0 {
		t.Fatalf("source span s0 byte %#x changed before release", run.Base(s0)+uintptr(off))
	}
	if off := LogosZgcFirstMismatch(run.Base(s1), run.Bytes(s1), 0xA2); off >= 0 {
		t.Fatalf("source span s1 byte %#x changed before release", run.Base(s1)+uintptr(off))
	}

	// Drain complete: the last copy publishes, and only now may the
	// exact source range return to the OS.
	if got := LogosZgcRelocate(pg, from[1], elem, dst[1]); got != dst[1] {
		t.Fatalf("relocate %#x = %#x, want %#x", from[1], got, dst[1])
	}
	if !LogosZgcRelease(pg) || !LogosZgcPageReleased(pg) {
		t.Fatal("release refused a fully published page")
	}
	if GOOS == "linux" {
		for _, s := range []int{s0, s1} {
			if !LogosZgcAllZero(run.Base(s), run.Bytes(s)) {
				t.Fatalf("source span %d not zeroed after release", s)
			}
		}
		if LogosZgcByteAt(run.Base(head)) != 0xE0 || LogosZgcByteAt(run.Base(head)+run.Bytes(head)-1) != 0xE0 {
			t.Fatal("head bytes touched by the release")
		}
		if used := 2 * elem; run.Bytes(ts) > used {
			if off := LogosZgcFirstMismatch(run.Base(ts)+used, run.Bytes(ts)-used, 0x5C); off >= 0 {
				t.Fatalf("to-space tail clobbered at +%d", used+uintptr(off))
			}
		}
	}

	// Self-heal drain completes through the off-heap forwarding table
	// after the source is zeroed: stale slots still heal to their copies.
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()
	for i := range 2 {
		stale := LogosZgcColored(from[i], LogosZgcMark0)
		if got := LogosZgcLoad(&stale); got != dst[i] {
			t.Fatalf("stale slot %d healed to %#x after release, want %#x", i, got, dst[i])
		}
	}

	// Storage is freed only after the drain: retire refuses before a
	// remap pass completes after the release.
	var early, late bool
	LogosZgcWithWorldStopped(func() { early = LogosZgcRetire(pg) })
	if early || LogosZgcPageFor(from[0]) != pg {
		t.Fatal("page retired before a remap pass completed after release")
	}
	LogosZgcRemapDone()
	LogosZgcWithWorldStopped(func() { late = LogosZgcRetire(pg) })
	if !late {
		t.Fatal("page not retired after a remap pass")
	}
	retired = true
	run.MarkFreed(s0)
	run.MarkFreed(s1)
	for _, d := range dst {
		LogosZgcIdentityForget(d)
	}
}

// Ordering pin: a live foreign root pins its whole 2 MB region against
// release. Admission records the canonical base in zgcForeignObjects and
// its region in zgcForeignPages (logos_zgc_foreign_registry.go:1163-1170),
// and zgcReleasePage refuses a pinned region under the gate
// (logos_zgc_page.go:631-635) before reaching madvise
// (logos_zgc_release_linux.go:13-14 via logos_zgc_page.go:637). The pin
// is region-wide, so a root naming the to-space copy still protects the
// source range in the same region.
//
// A regression that releases under a live foreign root (for example,
// checking the pin after the madvise, or dropping the gate) fails the
// refusal and intact-bytes assertions while the frame is live; the
// post-End half proves the gate is the pin and not the publish count.
func TestLogosZgcReleaseRefusesLiveForeignRoot(t *testing.T) {
	if !LogosZgcSyscallMode() {
		t.Skip("requires -gc=zgc cycle for foreign admission")
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

	region := run.Base(0) &^ (LogosZgcPageSize - 1)
	from := []uintptr{run.Base(s0), run.Base(s1)}
	pg := LogosZgcNewPage(region, 2*elem, 2)
	retired := false
	t.Cleanup(func() {
		if !retired {
			LogosZgcForgetPage(pg)
			LogosZgcDropPage(pg)
		}
	})

	LogosZgcRecordSources(pg, run, []int{s1, s0})
	var eligible bool
	LogosZgcWithWorldStopped(func() { eligible = LogosZgcEligible(pg) })
	if !eligible {
		t.Fatal("page with adjacent sources is ineligible")
	}
	if _, exact := LogosZgcPageSources(pg); !exact {
		t.Fatal("prepared page is not exact")
	}

	LogosZgcFill(run.Base(s0), run.Bytes(s0), 0xA1)
	LogosZgcFill(run.Base(s1), run.Bytes(s1), 0xA2)
	LogosZgcFill(run.Base(head), run.Bytes(head), 0xE0)
	LogosZgcFill(run.Base(ts), run.Bytes(ts), 0x5C)

	// Fully publish first, so the foreign pin is the only reason to refuse.
	dst := []uintptr{run.Base(ts), run.Base(ts) + elem}
	for i := range 2 {
		if got := LogosZgcRelocate(pg, from[i], elem, dst[i]); got != dst[i] {
			t.Fatalf("relocate %#x = %#x, want %#x", from[i], got, dst[i])
		}
	}

	// Live foreign root: a stale colored source word admitted through
	// the syscall lane canonicalizes to the to-space copy and pins the
	// region for the frame's lifetime.
	LogosZgcSelectSoftware(LogosZgcRemapped)
	defer LogosZgcReset()
	stale := unsafe.Pointer(LogosZgcColored(from[0], LogosZgcMark0))
	var temp unsafe.Pointer = stale
	slot := unsafe.Pointer(&temp)
	rows := []unsafe.Pointer{slot, nil}
	frame, gen := LogosZgcSyscallAdmit(unsafe.Pointer(&rows[0]), 1)
	if !LogosZgcSyscallTokenValid(frame, gen) {
		t.Fatal("admit returned invalid token with cycle enabled")
	}
	if uintptr(temp) != dst[0] {
		t.Fatalf("admitted root = %#x, want copy %#x", uintptr(temp), dst[0])
	}
	if !LogosZgcSyscallExposed(dst[0]) {
		t.Fatal("admitted root not retained while frame live")
	}
	if LogosZgcRelease(pg) || LogosZgcPageReleased(pg) {
		t.Fatal("page released while a live foreign root names its region")
	}
	if LogosZgcPageFor(from[0]) != pg {
		t.Fatal("released page lost its registration under a live foreign root")
	}
	if off := LogosZgcFirstMismatch(run.Base(s0), run.Bytes(s0), 0xA1); off >= 0 {
		t.Fatalf("source byte %#x zeroed under a live foreign root", run.Base(s0)+uintptr(off))
	}
	LogosZgcSyscallEnd(frame, gen)
	if LogosZgcSyscallExposed(dst[0]) {
		t.Fatal("root still exposed after End")
	}
	KeepAlive(rows)

	// Pin gone: release proceeds, zeroes exactly the source range, and
	// the page still heals and retires through the drain.
	if !LogosZgcRelease(pg) || !LogosZgcPageReleased(pg) {
		t.Fatal("release refused after the foreign root left")
	}
	if GOOS == "linux" {
		for _, s := range []int{s0, s1} {
			if !LogosZgcAllZero(run.Base(s), run.Bytes(s)) {
				t.Fatalf("source span %d not zeroed after release", s)
			}
		}
		if LogosZgcByteAt(run.Base(head)) != 0xE0 || LogosZgcByteAt(run.Base(head)+run.Bytes(head)-1) != 0xE0 {
			t.Fatal("head bytes touched by the release")
		}
	}
	heal := LogosZgcColored(from[1], LogosZgcMark0)
	if got := LogosZgcLoad(&heal); got != dst[1] {
		t.Fatalf("stale slot healed to %#x after release, want %#x", got, dst[1])
	}
	var early, late bool
	LogosZgcWithWorldStopped(func() { early = LogosZgcRetire(pg) })
	if early {
		t.Fatal("page retired before a remap pass completed after release")
	}
	LogosZgcRemapDone()
	LogosZgcWithWorldStopped(func() { late = LogosZgcRetire(pg) })
	if !late {
		t.Fatal("page not retired after a remap pass")
	}
	retired = true
	run.MarkFreed(s0)
	run.MarkFreed(s1)
	for _, d := range dst {
		LogosZgcIdentityForget(d)
	}
}
