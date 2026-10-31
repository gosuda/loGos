// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"unsafe"
)

// A global root naming a heap object must survive collections with the
// object intact AND relocated: the marker has to mark through the global,
// the evacuator has to publish a forwarding entry, and the gcStart
// rewrite has to heal the slot to the copy. Survival alone proves
// nothing, so the inner test places the victims on a contract-conforming
// page, runs the collections the policy needs, and asserts the globals
// name intact objects at new addresses. All victims are larger than the
// inline mark-bit ceiling, so only the greyobject path can mark them;
// if that path drops words outside the registered relocation pages,
// nothing is ever selected or moved and the move assertion fails. The
// noscan victim is the itabTable shape (pointer-free heap memory reachable
// only from a runtime global).
//
// Placement is the contract: selection admits a 2 MB page only when every
// overlapping span sits fully inside it
// (src/runtime/logos_zgc_select.go:11-16,46-48), and prepareSources
// rejects a page whose span leaves the region
// (src/runtime/logos_zgc_page.go:586-589), so a heap page owns exactly
// its quarantined source spans and nothing else
// (src/runtime/logos_zgc_page.go:113-127). Production-placed spans cannot
// promise that: a span straddling the granule edge excludes its whole
// page, and ambient neighbours veto the rest, so flooding ambient pages
// leaves selection at zero and the move assertion below passes vacuous.
// LogosZgcAllocGranule instead reserves one whole-granule 1024-byte noscan
// span, granule-aligned on a granule no page owns, by construction: the
// victims' page is then ~0.4% live, far past the 35 percent threshold
// (logos-docs/SPEC.md:1991), with no flood needed. The globals stay real
// globals and every step under test stays production code: root scan
// through the global, greyobject mark (1024 still exceeds the 512-byte
// inline ceiling), production copy, forwarding publication, and gcStart
// healing.

var logosZgcGlobalFixNoscan [8]*[1024]byte

type logosZgcGlobalFixScanned struct {
	p, q  *byte
	magic [1016]byte
}

var logosZgcGlobalFixScan [8]*logosZgcGlobalFixScanned

// TestLogosZgcGlobalFix builds this package with the zgc mode linked and
// runs TestLogosZgcGlobalFixInner in it, so a real collection exercises
// the production mark, select, and sweep path instead of the test-only
// color machinery.
func TestLogosZgcGlobalFix(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") != "" {
		t.Skip("inner run")
	}
	testenv.MustHaveGoBuild(t)
	exp := os.Getenv("GOEXPERIMENT")
	if !slices.Contains(strings.Split(exp, ","), "logosgcmoving") {
		exp = strings.TrimPrefix(exp+",logosgcmoving", ",")
	}
	// -ldflags=-X=runtime.logosGCMode=zgc is required: it links the zgc
	// cycle and restricts the heap to the low window the collector
	// manages. No GOGC=off here: the inner test needs a real collection.
	// -v is required: the assertion below matches the inner test's PASS
	// line, which the testing package prints only in verbose mode.
	cmd := exec.Command(testenv.GoToolPath(t), "test", "-count=1", "-v",
		"-ldflags=-X=runtime.logosGCMode=zgc",
		"-run=^TestLogosZgcGlobalFixInner$", "runtime")
	cmd = testenv.CleanCmdEnv(cmd)
	cmd.Env = append(cmd.Env, "LOGOS_ZGC_INNER=1", "GOEXPERIMENT="+exp)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestLogosZgcGlobalFixInner") {
		t.Fatalf("inner run under zgc mode: %v\n%s", err, out)
	}
}

// TestLogosZgcGlobalFixInner runs only inside TestLogosZgcGlobalFix.
func TestLogosZgcGlobalFixInner(t *testing.T) {
	if os.Getenv("LOGOS_ZGC_INNER") == "" {
		t.Skip("runs inside TestLogosZgcGlobalFix")
	}
	// A synthetic page from an earlier test covers real heap memory and
	// cannot be told apart by the release pass, which needs prepared
	// source spans. Free every test page before this test collects.
	runtime.LogosZgcDropTestPages()

	// The victims' page, by construction: one whole-granule 1024-byte
	// noscan span, granule-aligned on a granule no page owns. GC stays
	// off across the acquisition and hand-out, as the seam requires; the
	// rounds below re-enable it. Every occupied slot is named by a real
	// global before the first collection, so the sweeper keeps what the
	// marker marks through those globals.
	prevGC := debug.SetGCPercent(-1)
	run := runtime.LogosZgcAllocGranule()
	if run == nil {
		t.Fatal("no whole-granule heap span after 8 trials")
	}
	if elem := run.Elem(0); elem != 1024 {
		t.Fatalf("granule elem = %d, want 1024-byte noscan slots", elem)
	}
	for i := range logosZgcGlobalFixNoscan {
		run.Occupy(0, uintptr(i))
		v := (*[1024]byte)(unsafe.Pointer(run.Base(0) + uintptr(i)*run.Elem(0)))
		for j := range v {
			v[j] = byte(j*7 + 3)
		}
		logosZgcGlobalFixNoscan[i] = v
	}
	debug.SetGCPercent(prevGC)
	// The run handle is intentionally not freed: after release and retire
	// the collector owns the source span, so freeing here could double
	// free; the subprocess exits when the test does.
	for i := range logosZgcGlobalFixScan {
		s := &logosZgcGlobalFixScanned{}
		for j := range s.magic {
			s.magic[j] = byte(j*13 + 5)
		}
		logosZgcGlobalFixScan[i] = s
	}
	before := make([]uintptr, len(logosZgcGlobalFixNoscan))
	for i, v := range logosZgcGlobalFixNoscan {
		before[i] = uintptr(unsafe.Pointer(v))
	}

	// The first collection marks (and sweeps the span into a movable
	// state); a later one selects and evacuates the granule page, and the
	// gcStart rewrite heals the globals from the published forwarding
	// entries. Retirement ordering (a registered page waits for release
	// plus a remap pass) likewise needs more than one pause to settle.
	moved := 0
	for round := 0; round < 3 && moved == 0; round++ {
		runtime.GC()
		logosZgcGlobalFixCheckPatterns(t)
		runtime.GC()
		logosZgcGlobalFixCheckPatterns(t)
		for i, v := range logosZgcGlobalFixNoscan {
			if uintptr(unsafe.Pointer(v)) != before[i] {
				moved++
			}
		}
	}
	if moved == 0 {
		t.Fatalf("no global-rooted noscan object relocated across two collections: selection/evacuation proof missing, test passes vacuous")
	}
}

func logosZgcGlobalFixCheckPatterns(t *testing.T) {
	t.Helper()
	for k, v := range logosZgcGlobalFixNoscan {
		for i := range v {
			if want := byte(i*7 + 3); v[i] != want {
				t.Fatalf("global noscan object %d lost byte %d across GC: %#x, want %#x (object swept while reachable)", k, i, v[i], want)
			}
		}
	}
	for k, w := range logosZgcGlobalFixScan {
		for i := range w.magic {
			if want := byte(i*13 + 5); w.magic[i] != want {
				t.Fatalf("global scan object %d lost byte %d across GC: %#x, want %#x (object swept while reachable)", k, i, w.magic[i], want)
			}
		}
	}
}
