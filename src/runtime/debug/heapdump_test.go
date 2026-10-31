// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package debug_test

import (
	"os"
	"runtime"
	. "runtime/debug"
	"testing"
	"unsafe"
	"weak"
)

func TestWriteHeapDumpNonempty(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skipf("WriteHeapDump is not available on %s.", runtime.GOOS)
	}
	f, err := os.CreateTemp("", "heapdumptest")
	if err != nil {
		t.Fatalf("TempFile failed: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	WriteHeapDump(f.Fd())
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	const minSize = 1
	if size := fi.Size(); size < minSize {
		t.Fatalf("Heap dump size %d bytes, expected at least %d bytes", size, minSize)
	}
}

type Obj struct {
	x, y int
}

func objfin(x *Obj) {
	//println("finalized", x)
}

func TestWriteHeapDumpFinalizers(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skipf("WriteHeapDump is not available on %s.", runtime.GOOS)
	}
	f, err := os.CreateTemp("", "heapdumptest")
	if err != nil {
		t.Fatalf("TempFile failed: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	// bug 9172: WriteHeapDump couldn't handle more than one finalizer
	println("allocating objects")
	x := &Obj{}
	runtime.SetFinalizer(x, objfin)
	y := &Obj{}
	runtime.SetFinalizer(y, objfin)

	// Trigger collection of x and y, queueing of their finalizers.
	println("starting gc")
	runtime.GC()

	// Make sure WriteHeapDump doesn't fail with multiple queued finalizers.
	println("starting dump")
	WriteHeapDump(f.Fd())
	println("done dump")
}

type G[T any] struct{}
type I interface {
	M()
}

//go:noinline
func (g G[T]) M() {}

var dummy I = G[int]{}
var dummy2 I = G[G[int]]{}

func TestWriteHeapDumpTypeName(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skipf("WriteHeapDump is not available on %s.", runtime.GOOS)
	}
	f, err := os.CreateTemp("", "heapdumptest")
	if err != nil {
		t.Fatalf("TempFile failed: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	WriteHeapDump(f.Fd())
	dummy.M()
	dummy2.M()
}

const (
	logosRetentionChildBytes = 1152
	logosRetentionChurn      = 4096
	logosRetentionParents    = 4
)

// The parents and their weak handles are package-level, so the collector
// scans them with their type's pointer map. Heap slices of these shapes would
// be small scan objects of the class under test, and the bitmap defect could
// hide the parents themselves: a four-pointer backing array is a 32-byte scan
// object whose pointer words would be dropped like the children.
var (
	logosRetentionParentSet [logosRetentionParents]*logosRetentionParent
	logosRetentionWeakSet   [logosRetentionParents]weak.Pointer[logosRetentionChild]
)

// logosRetentionChild is a pointer-free leaf of 1152 bytes: churn of this
// class writes no pointer bitmap, so only the parents exercise the parent
// class's bitmap.
type logosRetentionChild struct {
	id  uint64
	sum uint64
	pad [logosRetentionChildBytes - 2*unsafe.Sizeof(uint64(0))]byte
}

// logosRetentionParent is exactly one 24-byte slot: the pointer word is
// first, so a correct pointer bitmap is one bit and the bitmap lives in the
// span rather than with the type.
type logosRetentionParent struct {
	child *logosRetentionChild
	id    uint64
	sum   uint64
}

func logosRetentionSum(id uint64) uint64 {
	return id*0x9e3779b97f4a7c15 ^ 0x2545f4914f6cdd1d
}

func logosRetentionChildPut(c *logosRetentionChild, id uint64) {
	c.id, c.sum = id, logosRetentionSum(id)
	for i := range c.pad {
		c.pad[i] = byte(c.sum>>(uint(i&7)*8)) + byte(i)
	}
}

func logosRetentionChildOK(c *logosRetentionChild, id uint64) bool {
	if c.id != id || c.sum != logosRetentionSum(id) {
		return false
	}
	for i := range c.pad {
		if c.pad[i] != byte(c.sum>>(uint(i&7)*8))+byte(i) {
			return false
		}
	}
	return true
}

func logosRetentionParentOK(p *logosRetentionParent, id uint64) bool {
	return p.id == id && p.sum == logosRetentionSum(id)^0x6a09e667f3bcc909
}

// newLogosRetentionChild is not inlined so that the caller never holds the
// only strong reference in its frame.
//
//go:noinline
func newLogosRetentionChild(id uint64) *logosRetentionChild {
	c := new(logosRetentionChild)
	logosRetentionChildPut(c, id)
	return c
}

// newLogosRetentionParent returns a parent and a weak handle to its child.
// Once it returns, the parent's field is the child's only strong reference.
//
//go:noinline
func newLogosRetentionParent(id uint64) (*logosRetentionParent, weak.Pointer[logosRetentionChild]) {
	c := newLogosRetentionChild(id)
	p := &logosRetentionParent{child: c, id: id, sum: logosRetentionSum(id) ^ 0x6a09e667f3bcc909}
	return p, weak.Make(c)
}

//go:noinline
func newLogosRetentionChildWeak(id uint64) weak.Pointer[logosRetentionChild] {
	return weak.Make(newLogosRetentionChild(id))
}

// TestLogosPointerRetention checks that a child reachable only through a live
// 24-byte pointer-bearing parent survives a forced collection and allocation
// churn with its contents intact.
//
// The parent is small enough to carry no malloc header, so the collector
// finds its pointer through the heap bitmap at the end of the parent's span.
// A build that serves the class from multi-page spans can write that bitmap
// relative to one page and read it relative to the whole span; the child is
// then never marked, the sweep frees it, and churn re-serves its slot with a
// different payload. The weak handle observes reclamation without keeping the
// child alive, and the child is dereferenced only while that handle is
// non-nil, so the test never follows a reclaimed pointer.
//
// This lives in package debug_test rather than runtime_test because it needs
// no runtime test exports: weak handles and the public runtime API observe
// everything.
func TestLogosPointerRetention(t *testing.T) {
	if unsafe.Sizeof(logosRetentionParent{}) != 24 {
		t.Skipf("parent is %d bytes; a 24-byte slot needs 64-bit pointers", unsafe.Sizeof(logosRetentionParent{}))
	}

	// Keep other goroutines out of the 24-byte size class and collect only at
	// the explicit points below.
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer SetGCPercent(SetGCPercent(-1))

	// Settle the heap: the two cycles also finish any sweep the previous
	// cycle left outstanding, so no earlier content is reclaimed below.
	runtime.GC()
	runtime.GC()

	logosRetentionParentSet = [logosRetentionParents]*logosRetentionParent{}
	logosRetentionWeakSet = [logosRetentionParents]weak.Pointer[logosRetentionChild]{}
	defer func() {
		logosRetentionParentSet = [logosRetentionParents]*logosRetentionParent{}
		logosRetentionWeakSet = [logosRetentionParents]weak.Pointer[logosRetentionChild]{}
	}()
	var ids [logosRetentionParents]uint64
	for i := range logosRetentionParents {
		id := uint64(0x5100 + i*0x111)
		p, w := newLogosRetentionParent(id)
		if p.child == nil || !logosRetentionParentOK(p, id) {
			t.Fatalf("parent %d is not initialized", i)
		}
		logosRetentionParentSet[i] = p
		logosRetentionWeakSet[i] = w
		ids[i] = id
	}

	// The control has a weak handle and no strong reference. A completed
	// collection clears its handle, so a live handle below is retention and
	// not a collection that never ran.
	control := newLogosRetentionChildWeak(0xdead)

	runtime.GC()

	// Churn the child class so that any slot freed by the collection above is
	// swept and served again with a different payload.
	churn := make([]*logosRetentionChild, 0, logosRetentionChurn)
	for i := range logosRetentionChurn {
		churn = append(churn, newLogosRetentionChild(uint64(0x9000+i)))
	}

	// Starting this cycle finishes the sweep of the one above, which is where
	// weak handles to unmarked objects are cleared.
	runtime.GC()

	if control.Value() != nil {
		t.Fatal("an unrooted child survived the collection: the control is not observing reclamation")
	}
	for i := range logosRetentionParents {
		p := logosRetentionParentSet[i]
		if !logosRetentionParentOK(p, ids[i]) {
			t.Errorf("parent %d lost its contents across the collection and churn", i)
			continue
		}
		c := logosRetentionWeakSet[i].Value()
		if c == nil {
			t.Errorf("child %d was reclaimed although its only strong reference was live 24-byte parent %d", i, i)
			continue
		}
		if p.child != c {
			t.Errorf("parent %d points at %p but its weak handle names %p", i, p.child, c)
			continue
		}
		if !logosRetentionChildOK(c, ids[i]) {
			t.Errorf("child %d lost its contents: got id=%#x sum=%#x, want %#x/%#x", i, c.id, c.sum, ids[i], logosRetentionSum(ids[i]))
		}
	}
	runtime.KeepAlive(churn)
}
