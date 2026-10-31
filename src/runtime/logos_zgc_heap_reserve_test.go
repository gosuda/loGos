// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime_test

import (
	"testing"
	"unsafe"

	. "runtime"
)

// Heap-reserve placement + true-move admission (owner H-runtime proof,
// allocator-owned placement).
//
// Placement binds the allocator owner's low-window contract
// (logos_zgc_heap_reserve.go: whole [1,1<<42) interval, prune + exact
// search, never a fixed budget, never a high publish): every public heap
// allocation below must name a REAL live heap span (spanOfHeap, InUse,
// bounds-checked, user-arena rejected) with containment, never a spanless
// mmap page, and — in zgc mode only — must lie at or below the 42-bit
// mask. The default-mode control (cycle disabled: greentea/immix/plain)
// asserts spans only, imposing no new range constraint.
//
// Evacuation uses the true collector cycle (runtime GC selection +
// production copy commit), never invented spans or forwarding records and
// never raw color on an unmanaged address. A stale source word with a
// published copy must canonicalize to a valid Go span whose whole
// transitive closure stays retained and healed through sweep until End.

// Live global sink: every allocation below escapes to the heap through a
// real consumer. No stack-only fixture may pass as placement proof.
var logosHeapReserveSink []any

//go:noinline
func logosHeapReserveTiny() *[16]byte { return new([16]byte) }

//go:noinline
func logosHeapReserveSmall() *[256]byte { return new([256]byte) }

//go:noinline
func logosHeapReserveLarge() *[80 << 10]byte { return new([80 << 10]byte) }

//go:noinline
func logosHeapReservePost() *[32]byte { return new([32]byte) }

type logosHeapReserveNode struct {
	childA  *logosHeapReserveNode
	childB  *logosHeapReserveNode
	payload [32]byte
}

//go:noinline
func logosHeapReserveNewNode() *logosHeapReserveNode { return new(logosHeapReserveNode) }

func logosHeapReserveCheck(t *testing.T, addr uintptr, what string) {
	t.Helper()
	base, size, ok := LogosZgcHeapReserveSpan(addr)
	if !ok {
		t.Fatalf("%s %#x: no live heap span (want real spanOfHeap, not mmap)", what, addr)
	}
	if addr < base || addr >= base+size {
		t.Fatalf("%s %#x outside span [%#x,%#x)", what, addr, base, base+size)
	}
	if LogosZgcCycleEnabled() {
		if uint64(addr) > LogosZgcAddrMask {
			t.Fatalf("%s %#x above 4 TB field (mask %#x)", what, addr, LogosZgcAddrMask)
		}
		if uint64(base+size-1) > LogosZgcAddrMask {
			t.Fatalf("%s span [%#x,%#x) straddles 4 TB field (mask %#x)", what, base, base+size, LogosZgcAddrMask)
		}
	}
}

// TestLogosZgcHeapReservePlacement proves real low placement across size
// classes: tiny, small, large (>64 KiB, forcing a second span/grow), and
// one object allocated after a GC. Every address names a live heap span
// with containment; in zgc mode every address is also in the 42-bit
// field (predicted red while the heap sits high, green after the
// allocator fix). Disabled mode is the control: spans only, no range
// constraint.
func TestLogosZgcHeapReservePlacement(t *testing.T) {
	LogosZgcForeignInit()
	tiny := logosHeapReserveTiny()
	for i := range tiny {
		tiny[i] = byte(i + 1)
	}
	logosHeapReserveSink = append(logosHeapReserveSink, tiny)
	tinyAddr := uintptr(unsafe.Pointer(tiny))
	logosHeapReserveCheck(t, tinyAddr, "tiny")

	small := logosHeapReserveSmall()
	for i := range small {
		small[i] = byte(i + 3)
	}
	logosHeapReserveSink = append(logosHeapReserveSink, small)
	smallAddr := uintptr(unsafe.Pointer(small))
	logosHeapReserveCheck(t, smallAddr, "small")

	large := logosHeapReserveLarge()
	for i := range 64 {
		large[i] = byte(i + 7)
	}
	large[len(large)-1] = 0x5a
	logosHeapReserveSink = append(logosHeapReserveSink, large)
	largeAddr := uintptr(unsafe.Pointer(&large[0]))
	logosHeapReserveCheck(t, largeAddr, "large")
	logosHeapReserveCheck(t, largeAddr+uintptr(len(large))-1, "large tail")

	GC()
	post := logosHeapReservePost()
	for i := range post {
		post[i] = byte(0xc0 + i)
	}
	logosHeapReserveSink = append(logosHeapReserveSink, post)
	postAddr := uintptr(unsafe.Pointer(post))
	logosHeapReserveCheck(t, postAddr, "postGC")

	for i := range tiny {
		if tiny[i] != byte(i+1) {
			t.Fatalf("tiny payload corrupted at %d", i)
		}
	}
	if large[len(large)-1] != 0x5a {
		t.Fatal("large payload corrupted")
	}
	KeepAlive(tiny)
	KeepAlive(small)
	KeepAlive(large)
	KeepAlive(post)
}

// logosHeapReserveFragment holds bounded fragmentation pressure in the
// same size class as the graph nodes, so the true cycle can select the
// graph's own spans. Fixed count per round (2048 nodes, ~96 KiB of the
// same 48-byte class), at most three rounds. The caller drops the
// returned slice before GC so every fragment is dead while the graph
// stays live in the global sink.
func logosHeapReserveFragment(t *testing.T, n int) []*logosHeapReserveNode {
	t.Helper()
	g := make([]*logosHeapReserveNode, 0, n)
	for range n {
		g = append(g, logosHeapReserveNewNode())
	}
	return g
}

// TestLogosZgcHeapReserveMoveAdmit proves the full honest path: a real
// heap graph moves under the true cycle, a foreign Begin on the stale
// source canonicalizes to a valid Go span, every transitive child stays
// retained and healed through sweep, and End releases. Movement is
// required: the test fails (never skips) when no copy publishes within
// the bounded GC budget. Disabled mode is the control: valid spans, no
// range constraint, no evacuation claim.
func TestLogosZgcHeapReserveMoveAdmit(t *testing.T) {
	if !LogosZgcCycleEnabled() {
		LogosZgcForeignInit()
		n := logosHeapReserveNewNode()
		logosHeapReserveSink = append(logosHeapReserveSink, n)
		addr := uintptr(unsafe.Pointer(n))
		if _, _, ok := LogosZgcHeapReserveSpan(addr); !ok {
			t.Fatalf("control node %#x: no live heap span", addr)
		}
		KeepAlive(n)
		return
	}
	for _, sel := range []struct {
		name string
		fn   func(uint8)
	}{
		{"software", LogosZgcSelectSoftware},
		{"hardware", LogosZgcSelectHardware},
	} {
		sel.fn(LogosZgcRemapped)
		func() {
			defer LogosZgcReset()
			LogosZgcForeignInit()
			parent := logosHeapReserveNewNode()
			c1 := logosHeapReserveNewNode()
			c2 := logosHeapReserveNewNode()
			parent.childA = c1
			parent.childB = c2
			for i := range parent.payload {
				parent.payload[i] = byte(0xa0 + i)
				c1.payload[i] = byte(0xb0 + i)
				c2.payload[i] = byte(0xc0 + i)
			}
			logosHeapReserveSink = append(logosHeapReserveSink, parent, c1, c2)
			srcParent := uintptr(unsafe.Pointer(parent))
			srcC1 := uintptr(unsafe.Pointer(c1))
			srcC2 := uintptr(unsafe.Pointer(c2))
			logosHeapReserveCheck(t, srcParent, sel.name+" parent source")
			logosHeapReserveCheck(t, srcC1, sel.name+" child1 source")
			logosHeapReserveCheck(t, srcC2, sel.name+" child2 source")

			payOff := unsafe.Offsetof(logosHeapReserveNode{}.payload)
			var dstParent uintptr
			moved := false
			for range 3 {
				func() {
					g := logosHeapReserveFragment(t, 2048)
					_ = g
				}()
				GC()
				GC()
				dstParent = LogosZgcRemap(srcParent)
				if dstParent != srcParent {
					moved = true
					break
				}
			}
			if !moved {
				t.Fatalf("%s: parent never moved after bounded GC budget (retain-only is not proof)", sel.name)
			}
			dstC1 := LogosZgcRemap(srcC1)
			dstC2 := LogosZgcRemap(srcC2)
			logosHeapReserveCheck(t, dstParent, sel.name+" parent copy")
			logosHeapReserveCheck(t, dstC1, sel.name+" child1 canonical")
			logosHeapReserveCheck(t, dstC2, sel.name+" child2 canonical")

			stale := unsafe.Pointer(LogosZgcColored(srcParent, 1))
			if uintptr(stale) == srcParent {
				t.Fatal("color fixture did not alter the word")
			}
			var temp unsafe.Pointer = stale
			slot := unsafe.Pointer(&temp)
			rows, rp, n := logosSyscallRows(slot, nil)
			frame, gen := LogosZgcSyscallAdmit(rp, n)
			if !LogosZgcSyscallTokenValid(frame, gen) {
				t.Fatal("admit returned invalid token with cycle enabled")
			}
			if uintptr(temp) != dstParent {
				t.Fatalf("temp not canonicalized to to-space: got %#x want %#x", uintptr(temp), dstParent)
			}
			if !LogosZgcSyscallExposed(dstParent) {
				t.Fatal("to-space parent not retained while frame live")
			}
			if !LogosZgcSyscallExposed(dstC1) || !LogosZgcSyscallExposed(dstC2) {
				t.Fatal("transitive children not retained")
			}
			for i := range parent.payload {
				if LogosZgcByteAt(dstParent+payOff+uintptr(i)) != byte(0xa0+i) {
					t.Fatalf("parent copy payload corrupted at %d", i)
				}
				if LogosZgcByteAt(dstC1+payOff+uintptr(i)) != byte(0xb0+i) {
					t.Fatalf("child1 canonical payload corrupted at %d", i)
				}
				if LogosZgcByteAt(dstC2+payOff+uintptr(i)) != byte(0xc0+i) {
					t.Fatalf("child2 canonical payload corrupted at %d", i)
				}
			}
			childAOff := unsafe.Offsetof(logosHeapReserveNode{}.childA)
			childBOff := unsafe.Offsetof(logosHeapReserveNode{}.childB)
			slotA := (*uintptr)(unsafe.Pointer(dstParent + childAOff))
			slotB := (*uintptr)(unsafe.Pointer(dstParent + childBOff))
			if got := LogosZgcLoad(slotA); got != dstC1 {
				t.Fatalf("parent copy childA healed to %#x, want canonical %#x", got, dstC1)
			}
			if got := LogosZgcLoad(slotB); got != dstC2 {
				t.Fatalf("parent copy childB healed to %#x, want canonical %#x", got, dstC2)
			}
			if got := LogosZgcLoad(slotA); got != dstC1 {
				t.Fatalf("parent copy childA not fixpoint: got %#x want %#x", got, dstC1)
			}
			GC()
			if !LogosZgcSyscallExposed(dstParent) || !LogosZgcSyscallExposed(dstC1) || !LogosZgcSyscallExposed(dstC2) {
				t.Fatal("closure not retained through sweep")
			}
			if uintptr(temp) != dstParent {
				t.Fatalf("sweep moved temp: got %#x want %#x", uintptr(temp), dstParent)
			}
			frame2, gen2 := LogosZgcSyscallAdmit(rp, n)
			if !LogosZgcSyscallTokenValid(frame2, gen2) {
				t.Fatal("second admit returned invalid token")
			}
			if uintptr(temp) != dstParent {
				t.Fatalf("fixpoint moved temp: got %#x want %#x", uintptr(temp), dstParent)
			}
			LogosZgcSyscallEnd(frame2, gen2)
			LogosZgcSyscallEnd(frame, gen)
			if LogosZgcSyscallExposed(dstParent) {
				t.Fatal("to-space parent still exposed after balanced End")
			}
			KeepAlive(parent)
			KeepAlive(c1)
			KeepAlive(c2)
			KeepAlive(rows)
		}()
	}
}
