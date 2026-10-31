// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// logosCgoCanonSink keeps every fixture object reachable, so escape
// analysis cannot leave it on a goroutine stack. zgcCanonical strips a
// color only from a word whose unmasked form names heap; a stack word is
// foreign and must pass through, so a stack fixture would measure only
// that refusal.
var logosCgoCanonSink []any

func logosCgoCanonHeap[T any](p *T) *T {
	logosCgoCanonSink = append(logosCgoCanonSink, p)
	return p
}

func LogosCgoCanonHeapInt(v int) *int {
	p := logosCgoCanonHeap(new(int))
	*p = v
	return p
}

func LogosCgoCanonHeapSlice(n, spare int) []*int {
	s := make([]*int, n, n+spare)
	for i := range s {
		s[i] = LogosCgoCanonHeapInt(i + 1)
	}
	logosCgoCanonSink = append(logosCgoCanonSink, s)
	return s
}

func LogosCgoCanonHeapString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	s := string(b)
	logosCgoCanonSink = append(logosCgoCanonSink, s)
	return s
}

// LogosCgoCanonPin keeps v reachable from the sink, so an object only the
// fixture names stays on the heap.
func LogosCgoCanonPin(v any) { logosCgoCanonSink = append(logosCgoCanonSink, v) }

func LogosCgoCanonDropSink() { logosCgoCanonSink = nil }

// LogosCgoCanonHeapAboveField reports whether v names allocated heap
// outside the 42-bit canonical address field zgcCanonical can strip. A
// heap word above the field is foreign to the canonical helper in every
// collector mode, so a fixture that needs colors on ordinary heap must
// skip on it instead of asserting: no mode flip can move the
// already-placed heap. A non-heap word above the field is not covered:
// it falls through to the strippable assertion and fails loud.
func LogosCgoCanonHeapAboveField(v uintptr) bool { return v > zgcAddrMask && inheap(v) }

// LogosCgoCanonStrippable reports whether the canonical helper will strip
// a color from v. It repeats zgcCanonical's acceptance test exactly: the
// unmasked word must name a registered zgc page or an allocated heap
// span. Fixtures assert it, so a fixture that escaped to the stack fails
// loud instead of silently measuring the foreign-word pass-through.
func LogosCgoCanonStrippable(v uintptr) bool {
	if v == 0 {
		return false
	}
	a := v &^ atomic.Loaduintptr(&zgcColorMaskBits)
	return a <= zgcAddrMask && (zgcPageFor(a) != nil || inheap(a))
}

var logosCgoCanonSaved struct {
	mode   string
	pct    int32
	hw     bool
	good   uintptr
	mask   uintptr
	unmask uintptr
	active bool
}

// LogosCgoCanonTestMode selects zgc with software colors and suspends the
// collector, saving every field it changes so the restore leaves no color
// behind for later tests.
func LogosCgoCanonTestMode() func() {
	if logosCgoCanonSaved.active {
		throw("logos: overlapping cgo canonicalization test modes")
	}
	logosCgoCanonSaved.active = true
	logosCgoCanonSaved.mode = logosGCMode
	logosCgoCanonSaved.pct = setGCPercent(-1)
	logosCgoCanonSaved.hw = zgcHardwareTagging
	logosCgoCanonSaved.good = atomic.Loaduintptr(&zgcGoodMask)
	logosCgoCanonSaved.mask = atomic.Loaduintptr(&zgcColorMaskBits)
	logosCgoCanonSaved.unmask = atomic.Loaduintptr(&zgcUnmaskBits)
	logosGCMode = "zgc"
	// The page registry is created by zgcInit at startup only for the
	// linked collector. Under any other -X runtime.logosGCMode the
	// directory has zero capacity and the first fixture page throws
	// "page registry overflow", so create it here through the existing
	// test export before any fixture registers a page. It probes
	// hardware tagging and configures colors, which the next line
	// overrides for determinism; every step is nil-guarded and safe to
	// repeat on binaries where zgcInit already ran.
	LogosZgcInit()
	zgcConfigure(zgcEncodingSoftware, zgcColor(zgcColorRemapped))
	return LogosCgoCanonTestModeRestore
}

func LogosCgoCanonTestModeRestore() {
	logosCgoCanonSaved.active = false
	logosGCMode = logosCgoCanonSaved.mode
	setGCPercent(logosCgoCanonSaved.pct)
	zgcHardwareTagging = logosCgoCanonSaved.hw
	atomic.Storeuintptr(&zgcGoodMask, logosCgoCanonSaved.good)
	atomic.Storeuintptr(&zgcColorMaskBits, logosCgoCanonSaved.mask)
	atomic.Storeuintptr(&zgcUnmaskBits, logosCgoCanonSaved.unmask)
}

func LogosCgoCanonBad(v uintptr) uintptr {
	return v + uintptr(zgcColorBits(zgcColor(zgcColorMark0)))
}

// LogosCgoCanonStoreWord seeds a color or stale address into the typed
// pointer slot at slot from runtime code, which checkptr does not
// instrument; a test-side unsafe.Pointer(uintptr) round-trip is the
// bare conversion -race rejects. It returns nothing, so no invalid
// pointer value escapes to test code.
//
//go:nosplit
func LogosCgoCanonStoreWord(slot unsafe.Pointer, w uintptr) {
	*(*uintptr)(slot) = w
}

func LogosCgoCanonicalizeSlot(ptr any, slot unsafe.Pointer) {
	cgoCanonicalizeSlot(ptr, slot)
}

func LogosCgoCanonicalizeUnknown(p unsafe.Pointer) {
	cgoCanonicalizeUnknown(p)
}

func LogosCgoMapCheckPanics(m any) (panicked bool) {
	ep := efaceOf(&m)
	defer func() {
		panicked = recover() != nil
	}()
	cgoCheckArg(ep._type, ep.data, !ep._type.IsDirectIface(), true, cgoCheckPointerFail)
	return false
}
