// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build cgo && unix

// Real foreign-boundary fixtures behind foreign_roots_test.go: C functions
// that hold Go memory across a deterministic condition-variable barrier,
// callbacks that hand Pinner-pinned (or deliberately unpinned) pointers
// back to C, and a C-created thread that calls into Go with no Go caller
// below it. Every call here is an ordinary plain-path cgo call: no
// #cgo noescape or nocallback directive, so the calls take
// runtime.cgocall. No fixture touches runtime internals; C observes
// addresses only through the pointer values cgo hands it.
//
// Pinning rationale (cgo pointer rules, runtime/cgocall.go cgoCheckArg):
//
//   - A Go pointer passed as an argument ("top level") may point at Go
//     memory that holds no unpinned Go pointers, and C may use it only
//     while the call runs. Every referent the hold calls receive is
//     pointer-free memory, so no Pinner is needed or used: the foreign
//     frame is the only thing that can keep the referent in place.
//   - A pointer a callback returns to C is checked with top == false:
//     it must be pinned. C may keep a pinned pointer after the call that
//     received it until the matching Unpin. The callbacks pin inside the
//     callback, before returning, and the tests keep the Pinners until
//     the lifetime under test ends.
//   - Go memory passed to C may contain pointers to pinned objects (the
//     nested node case); an unpinned pointee is rejected with the
//     upstream message.

package testlogos

/*
#include <stdint.h>

#define FR_MAX_REFS 2048
#define FR_MAX_GATES 64
#define FR_MAX_KEPT 64

// One top-level pointer argument: C verifies words uint64 values starting
// at p. The value at p[m] must be fr_word(tag, skip+m): skip is the word
// offset of p inside its object, so interior pointers are covered.
typedef struct fr_ref {
	void *p;
	unsigned int tag;
	unsigned short words;
	unsigned short skip;
} fr_ref;

// Passed by value: every pointer is a distinct top-level argument cell of
// the call frame, never a pointer stored in Go memory the callee reads.
typedef struct fr_refs {
	int n;
	fr_ref r[FR_MAX_REFS];
} fr_refs;

void fr_gate_reset(int g);
int fr_gate_wait_entered(int g, int want);
void fr_gate_release(int g);
void fr_meta_set(int g, int i, unsigned int tag, unsigned int words, unsigned int skip);
uintptr_t fr_entry_addr(int g, int i);
uintptr_t fr_addr(void *p);
int fr_hold_refs(int g, fr_refs refs, int stamp);
int fr_try_refs(fr_refs refs);
int fr_cb_hold(int g, int n, int stamp);
int fr_kept_check(int g, int from, int to);
int fr_thread_start(int g, int n, int stamp, int unpinned);
int fr_thread_join(int g);
int fr_hold_node(int g, void *node);
int fr_hold_indexed(int g, void *p, int start, int n, unsigned int tag0, unsigned int tagV0, int stamp);
*/
import "C"

import (
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

const (
	frMaxRefs  = 2048
	frMaxGates = 64
	frMaxKept  = 64

	// frWaitSeconds bounds every C wait. It only turns a deadlock into a
	// failure: no passing path depends on it.
	frWaitSeconds = 120

	// frStampWord is XORed by C into the last word of a referent it was
	// told to stamp, so Go can prove C and Go name the same object.
	frStampWord uint64 = 0x5A5A5A5A5A5A5A5A
)

func init() {
	if int(C.FR_MAX_REFS) != frMaxRefs || int(C.FR_MAX_GATES) != frMaxGates || int(C.FR_MAX_KEPT) != frMaxKept {
		panic("testlogos: foreign-roots fixture constants differ between Go and C")
	}
}

// frRef is one top-level pointer argument of a hold call.
type frRef struct {
	P     unsafe.Pointer
	Tag   uint32
	Words uint16
	Skip  uint16
}

// frNode is Go memory that carries a pointer for C to follow. Its layout
// matches struct fr_node in foreign_roots_c.c.
type frNode struct {
	Next  unsafe.Pointer
	Tag   uint32
	Words uint32
}

func frBool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// The hold, callback-hold, and thread entry points below come in named
// pairs (plain and stamped, pinned and unpinned) over one private
// implementation each, so no call site passes a bare boolean.

func frFillRefs(a *C.fr_refs, refs []frRef) {
	if len(refs) > frMaxRefs {
		panic("testlogos: too many foreign refs")
	}
	a.n = C.int(len(refs))
	for i := range refs {
		a.r[i].p = refs[i].P
		a.r[i].tag = C.uint(refs[i].Tag)
		a.r[i].words = C.ushort(refs[i].Words)
		a.r[i].skip = C.ushort(refs[i].Skip)
	}
}

var frGateSeq atomic.Uint32

// frNewGate returns a freshly reset barrier gate. Gates are recycled
// round-robin; every test releases and joins its holders before the
// recycle distance of frMaxGates gates is reached.
func frNewGate() int {
	g := int(frGateSeq.Add(1)-1) % frMaxGates
	C.fr_gate_reset(C.int(g))
	return g
}

// frGateWaitEntered blocks in C until want holders reached the barrier of
// gate g. It reports false on the bounded timeout.
func frGateWaitEntered(g, want int) bool {
	return C.fr_gate_wait_entered(C.int(g), C.int(want)) == 0
}

// frGateRelease lets every holder of gate g leave its barrier. It is
// idempotent, and a gate released before its holder arrives never blocks.
func frGateRelease(g int) { C.fr_gate_release(C.int(g)) }

// frMetaSet records the data contract of the i'th pointer a callback hands
// to the holder of gate g.
func frMetaSet(g, i int, tag uint32, words, skip int) {
	C.fr_meta_set(C.int(g), C.int(i), C.uint(tag), C.uint(words), C.uint(skip))
}

// frEntryAddr is the address C received for slot i of gate g at entry.
func frEntryAddr(g, i int) uintptr { return uintptr(C.fr_entry_addr(C.int(g), C.int(i))) }

// frCanon returns the address C observes for p across a plain cgo call: the
// canonical address the foreign boundary hands to C.
func frCanon(p unsafe.Pointer) uintptr { return uintptr(C.fr_addr(p)) }

// frHold calls C with refs as distinct top-level pointer arguments. C
// records each entry address, verifies each referent, waits at the barrier
// of gate g, and verifies again. The result is a status word, 0 for
// success. frHoldStamped also XORs frStampWord into the last word of every
// referent after the barrier, so Go can prove C wrote the object Go holds.
func frHold(g int, refs []frRef) int { return frHoldMode(g, refs, false) }

// frHoldStamped is frHold plus the stamp write.
func frHoldStamped(g int, refs []frRef) int { return frHoldMode(g, refs, true) }

func frHoldMode(g int, refs []frRef, stamp bool) int {
	var a C.fr_refs
	frFillRefs(&a, refs)
	return int(C.fr_hold_refs(C.int(g), a, frBool(stamp)))
}

// frTryRefs exposes refs, then C calls a Go callback that returns an
// unpinned pointer. The result check must panic; the panic unwinds the C
// frame. A normal return is the defect: it yields 0.
func frTryRefs(refs []frRef) int {
	var a C.fr_refs
	frFillRefs(&a, refs)
	return int(C.fr_try_refs(a))
}

// frCBHold is a call with no pointer input at all: C obtains n pointers
// from FrProvide callbacks, keeps them, verifies them against frMetaSet
// data, waits at the barrier of gate g, and verifies again. Kept pointers
// stay valid exactly as long as their pins. frCBHoldStamped also stamps
// every object after the barrier.
func frCBHold(g, n int) int { return frCBHoldMode(g, n, false) }

// frCBHoldStamped is frCBHold plus the stamp write.
func frCBHoldStamped(g, n int) int { return frCBHoldMode(g, n, true) }

func frCBHoldMode(g, n int, stamp bool) int {
	return int(C.fr_cb_hold(C.int(g), C.int(n), frBool(stamp)))
}

// frKeptCheck has C re-read kept[from:to] of gate g in a later, pointer-free
// call and verify each referent under the contract its holder left behind:
// pristine data words, plus the stamp in each window's last word when that
// hold stamped them.
func frKeptCheck(g, from, to int) int {
	return int(C.fr_kept_check(C.int(g), C.int(from), C.int(to)))
}

// frThreadStartStamped creates a C thread that behaves like frCBHoldStamped:
// its callbacks run with no Go caller below them. It returns the
// pthread_create result.
func frThreadStartStamped(g, n int) int { return frThreadStartMode(g, n, true, false) }

// frThreadStartUnpinned creates a C thread that calls FrUnpinned once,
// again with no Go caller below the callback. It returns the
// pthread_create result.
func frThreadStartUnpinned(g int) int { return frThreadStartMode(g, 0, false, true) }

func frThreadStartMode(g, n int, stamp, unpinned bool) int {
	return int(C.fr_thread_start(C.int(g), C.int(n), frBool(stamp), frBool(unpinned)))
}

// frThreadJoin joins the thread of gate g and returns its status.
func frThreadJoin(g int) int { return int(C.fr_thread_join(C.int(g))) }

// frHoldNode passes one top-level pointer to Go memory that carries a
// nested pointer. C records both addresses, follows the nested one, waits
// at the barrier of gate g, and checks that the nested word did not change.
func frHoldNode(g int, n *frNode) int {
	return int(C.fr_hold_node(C.int(g), unsafe.Pointer(n)))
}

// frIdxElem is one element of the fixed-array tile the non-regression guard
// exercises. Its layout matches struct fr_idx_elem in foreign_roots_c.c: a Go
// pointer word C follows plus a scalar word C verifies and stamps. C sees
// the caller's tile itself, never a boxed copy.
type frIdxElem struct {
	P unsafe.Pointer
	V uint64
}

// frHoldIndexed exercises the fixed-array tile *pa across gate g, handing C
// the interior pointer &(*pa)[start]. The call shape dereferences a pointer
// to an array, so cgo roots pa itself in _cgoIndex and indexes through it:
// C points into the caller's tile. Every P must point to Pinner-pinned
// memory (legal, so leaves never relocate); C verifies every P/V cell in
// the tile, holds across moving collections, and stamps leaves plus tile V
// words. The Container row names the array pa points at, so the whole tile
// stays admitted while C walks it by pointer arithmetic.
func frHoldIndexed(g int, pa *[8]frIdxElem, start int, tag0, tagV0 uint32) int {
	return frHoldIndexedMode(g, pa, start, tag0, tagV0, false)
}

func frHoldIndexedStamped(g int, pa *[8]frIdxElem, start int, tag0, tagV0 uint32) int {
	return frHoldIndexedMode(g, pa, start, tag0, tagV0, true)
}

func frHoldIndexedMode(g int, pa *[8]frIdxElem, start int, tag0, tagV0 uint32, stamp bool) int {
	return int(C.fr_hold_indexed(C.int(g), unsafe.Pointer(&(*pa)[start]), C.int(start), C.int(8), C.uint(tag0), C.uint(tagV0), frBool(stamp)))
}

// frPlan scripts what the exported callbacks hand to C for one gate.
type frPlan struct {
	mu     sync.Mutex
	objs   [][]uint64
	pinner []*runtime.Pinner

	// interior is the word offset of the pointer handed to C instead of
	// the base word; the pin still covers the whole object. Required so a
	// callback can return an interior result (meta skip > 0) on pinned
	// memory, which no fixture exercised.
	interior int
	calls    int
}

// frNewPinnedPlan returns a plan handing &objs[i][0] to C for callback i,
// pinning that object with its own Pinner before returning it.
func frNewPinnedPlan(objs [][]uint64) *frPlan {
	p := &frPlan{objs: objs, pinner: make([]*runtime.Pinner, len(objs))}
	for i := range p.pinner {
		p.pinner[i] = new(runtime.Pinner)
	}
	return p
}

// frNewInteriorPinnedPlan is frNewPinnedPlan with the word handed to C
// taken at interior offset at of objs[i]: the callback pins the whole
// object and returns &objs[i][at], so C observes an interior callback
// result. at == 0 reproduces frNewPinnedPlan. Required because the C
// holder verifies and stamps with meta skip at, and no provider returned
// an interior word.
func frNewInteriorPinnedPlan(objs [][]uint64, at int) *frPlan {
	p := frNewPinnedPlan(objs)
	p.interior = at
	return p
}

func (p *frPlan) provide(i int) unsafe.Pointer {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	obj := p.objs[i]
	if pn := p.pinner[i]; pn != nil {
		pn.Pin(&obj[0])
	}
	return unsafe.Pointer(&obj[p.interior])
}

// Calls reports how many callbacks the plan served.
func (p *frPlan) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// UnpinAll drops the plan's own pin of every object and gives up ownership
// of the Pinners, so a second call has nothing left to unpin.
func (p *frPlan) UnpinAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, pn := range p.pinner {
		if pn != nil {
			pn.Unpin()
			p.pinner[i] = nil
		}
	}
}

var (
	frPlans    [frMaxGates]atomic.Pointer[frPlan]
	frUnpinned atomic.Pointer[[8]uint64]
)

func frSetPlan(g int, p *frPlan) { frPlans[g].Store(p) }

// frSetUnpinned makes FrUnpinned return a pointer to a fresh heap object
// that nothing pins.
func frSetUnpinned() { frUnpinned.Store(new([8]uint64)) }

// FrProvide is called from C. It returns the object scripted for (gate, i),
// pinned first when the plan says so.
//
//export FrProvide
func FrProvide(gate, i C.int) unsafe.Pointer {
	return frPlans[gate].Load().provide(int(i))
}

// FrUnpinned is called from C. It returns an unpinned Go heap pointer,
// which the generated result check must reject.
//
//export FrUnpinned
func FrUnpinned() unsafe.Pointer {
	return unsafe.Pointer(frUnpinned.Load())
}
