// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/abi"
	"internal/runtime/atomic"
	"unsafe"
)

// Foreign-call registry core (owner A: registry and shared ABI).
//
// This file implements frame/token ownership on the calling goroutine,
// growable and reclaimable off-heap registry and work storage, the
// synchronized exposure gate, and the exact object and page queries.
// Closure traversal, copy/heal commit, and collector-lifecycle wiring
// build on these primitives (owners B/C/D); emission builds on their
// APIs (owners G/H/I).
//
// Ownership and lifetime:
//
//   - The token owns the frame. Frames are off-heap (sysAlloc); only
//     the chain top pointer lives on the owning g (g.logosForeignTop),
//     so migration across Ms after exitsyscall cannot detach it.
//     Goroutine structs recycle, hence identity is (owner, generation),
//     never the address alone. Fast-call diagnostics stay on the M.
//   - A frame's identity is (owner g address, generation). The address
//     alone is insufficient: goroutine structs recycle. End validates
//     owner, generation, and LIFO position; violations throw.
//   - Frames are off-heap (sysAlloc). Chunk size is an allocation unit,
//     never a capacity limit: chunks, tables, and the queue all grow on
//     demand. Allocation failure unwinds the unpublished partial frame
//     and then throws, so entry is prevented and the failure is never
//     confused with a legitimate empty (invalid) token; it never
//     truncates, substitutes a partial closure, or enters foreign code
//     untracked.
//   - Published records are borrowed only under the gate while the world
//     runs, or gateless with the world stopped (worldsema exclusion).
//     No reader retains a record pointer past unlock (or past startThe
//     World for STW sections). Refcounts keep shared exposures alive
//     until the last frame retires.
//
// Consistency (single-process, shared memory):
//
//   - End-retire vs world-running queries: linearizable under
//     zgcForeignGate (one mutex, blocking acquire on preemptible
//     paths). This is the gate's entire scope: unlink plus refcount
//     release on one side, locking exposure queries on the other.
//   - The registry's own world-stopped admission paths are gateless,
//     serialized by worldsema (only the stop-the-world holder runs):
//     traversal, copy-help, publication, and MarkRoots take no gate.
//     The shared locking queries are additionally valid inside pauses:
//     gate sections are short and never block, the same leaf-lock
//     discipline as the quarantine and page locks the pauses already
//     share with world-running paths.
//   - Queries (page/object) come in two forms: locking forms hold the
//     gate and are exact in any world state; STW forms assert the stop
//     and read lock-free for pause code (selector/release/fixup). The
//     exposure-then-heal race is closed by pairing the gate on both
//     sides for world-running paths (owners B/C use the Locked forms
//     around pure checks).
//   - Frame push/pop and the per-g spare are thread-confined to the
//     owning g: no consistency protocol needed, and none is taken
//     (safe in nosplit).
//   - Generation is a linearizable atomic counter; 0 is reserved for
//     the invalid token and skipped on wrap.
//
// Lock order: the gate is a plain leaf mutex (no lockrank.go or
// mklockrank.go change; leaf locks are debug-checker supported), and a
// leaf lock must be taken last: NO gate-held section acquires any other
// lock. Gate-held sections (End retire, locking queries) contain only
// table/list ops and sysAlloc/sysFree, which take no runtime locks.
// STW paths take no lock ranked at or below the gate's peers at all.
// Future copy/heal owners must keep this split: preparation outside,
// pure commit checks inside; STW commit paths gateless.
//
// Stop-the-world: ordinary admission stops the world with stwUnknown.
// A dedicated stw reason belongs to a future proc.go touch owned by the
// parent; this lane reuses the generic reason rather than editing files
// outside its five owned paths.
//
// Marking model: admission sets the mark bit of every admitted base
// directly (world stopped, non-atomic) in every GC phase, so sweep
// spares exposed objects and the next mark-start re-shade (MarkRoots,
// wired by owner D) is idempotent. Traversal completeness at admission,
// plus Extend for callback-added objects, substitutes for collector
// scanning of pre-marked objects: C never introduces unadmitted edges
// except by storing already-admitted pointers.

const (
	// zgcForeignObjsPerChunk is the per-chunk object-record count: an
	// allocation unit, not a capacity limit. Frames chain chunks.
	zgcForeignObjsPerChunk = 128
	// zgcForeignSetMinCap is the smallest exposure-table capacity.
	zgcForeignSetMinCap = 64
	// zgcForeignSetHashMul mixes heap/page keys whose low bits are
	// zero (page starts share 21 zero bits). The index uses high bits
	// of the product, which mix every input bit.
	zgcForeignSetHashMul = uintptr(0x9E3779B97F4A7C15)
)

// zgcForeignFrame is one admission frame. All link fields are plain
// uintptr (never Go pointers) so off-heap stores need no write barrier
// and stay safe in nosplit paths.
type zgcForeignFrame struct {
	owner     uintptr // owning *g as an address; 0 when on a free list
	gen       uint64  // generation from zgcForeignGen
	parent    uintptr // enclosing *zgcForeignFrame, 0 at the stack base
	chunks    uintptr // admitted-base chunk list head (*zgcForeignObjChunk)
	nObjs     uint64  // admitted distinct bases
	published uint32  // 1 after STW publication
	_         [4]byte
	link      uintptr       // published-list linkage (STW publish, gate-held End unlink)
	owned     zgcForeignSet // per-frame owned-membership hash: every base this
	// frame admitted (distinct bases; counts stay 0/1 and are never
	// used for release). Hash lookups prove self-coverage for the
	// NoSplit path without scanning chunks; release, rollback, and
	// MarkRoots iterate chunks. Lifetime == frame lifetime: freed at
	// frame disposal, never shared across goroutines (owning-g-local
	// plus STW-exclusive mutation, lock-free reads by the owner).
}

// zgcForeignObjChunk holds admitted canonical bases. Each entry pairs
// with one global refcount increment, so retire stays balanced. Entries
// are duplicate-free per frame: the per-admission seen set suppresses
// repeats within an admission, and the frame owned-set suppresses
// re-recording across Begin/Extend calls (already-owned bases re-queue
// for rescan without new entries). The frame owned-set records the same
// distinct bases for hash membership.
type zgcForeignObjChunk struct {
	next  uintptr // *zgcForeignObjChunk
	n     uint32
	_     [4]byte
	addrs [zgcForeignObjsPerChunk]uintptr
}

// zgcForeignSet is a growable off-heap key->refcount map with open
// addressing. Key 0 means empty (nil is never admitted); ^uintptr(0)
// is a tombstone (never a valid heap or page address inside 4 TB).
// The global exposure tables are gate-held (world running) or
// STW-exclusive (world stopped); per-frame owned-sets and the
// per-admission seen set are owning-g-local plus STW-exclusive. No
// atomics inside any of them.
type zgcForeignSet struct {
	keys uintptr // *[cap]uintptr
	cnts uintptr // *[cap]uint32
	mask uintptr // cap-1
	cap  uintptr
	live uintptr
	tomb uintptr
}

// zgcForeignQueue is the STW-exclusive admission work queue. Only one
// admission runs at a time (worldsema serializes STW holders).
type zgcForeignQueue struct {
	base uintptr // *[cap]uintptr
	head uintptr
	tail uintptr
	cap  uintptr
}

var (
	// zgcForeignGate serializes publication, retirement, queries, and
	// marking against each other. Leaf mutex: no rank registration.
	zgcForeignGate mutex
	// zgcForeignGen mints frame generations; 0 is reserved invalid.
	zgcForeignGen atomic.Uint64
	// zgcForeignActive counts published object-holding frames.
	zgcForeignActive atomic.Int32
	// zgcForeignObjects maps canonical object base -> refcount.
	zgcForeignObjects zgcForeignSet
	// zgcForeignPages maps exposed 2 MB region key -> pin count. The key
	// is pure address arithmetic (zgcForeignRegionKey), never the mutable
	// zgcPageFor registration identity, so acquire, release, rollback,
	// and query always agree for the lifetime of a pin.
	zgcForeignPages zgcForeignSet
	// zgcForeignPublished heads the published frame list. Mutated only
	// under worldsema exclusion (STW publish; STW-serialized retire is
	// impossible while running, so End's unlink takes the gate to pair
	// with world-running locking queries, never with another writer).
	zgcForeignPublished uintptr
	// zgcForeignAdmitQueue is the STW-exclusive admission scratch,
	// serialized by worldsema: only the stop-the-world holder runs the
	// admission core, so no two admissions (Begin or Extend, any g)
	// overlap. Concurrent foreign calls simply wait on worldsema;
	// throwing on contention would crash legitimate concurrency.
	zgcForeignAdmitQueue zgcForeignQueue
)

// zgcForeignNextGen returns a fresh nonzero generation, skipping 0 on
// wrap so the invalid token stays unforgeable.
//
//go:nosplit
func zgcForeignNextGen() uint64 {
	for {
		g := zgcForeignGen.Add(1)
		if g != 0 {
			return g
		}
	}
}

// zgcForeignGateLock acquires the exposure gate.
func zgcForeignGateLock() {
	lock(&zgcForeignGate)
}

// zgcForeignGateUnlock releases the exposure gate.
func zgcForeignGateUnlock() {
	unlock(&zgcForeignGate)
}

// ---- exposure tables (gate-held for world-running paths; STW-paired paths access them gateless) ----

//go:nosplit
func zgcForeignSetKeyAt(s *zgcForeignSet, i uintptr) uintptr {
	return *(*uintptr)(add(unsafe.Pointer(s.keys), i*unsafe.Sizeof(uintptr(0))))
}

func zgcForeignSetCntAt(s *zgcForeignSet, i uintptr) uint32 {
	return *(*uint32)(add(unsafe.Pointer(s.cnts), i*4))
}

func zgcForeignSetSetAt(s *zgcForeignSet, i uintptr, key uintptr, cnt uint32) {
	*(*uintptr)(add(unsafe.Pointer(s.keys), i*unsafe.Sizeof(uintptr(0)))) = key
	*(*uint32)(add(unsafe.Pointer(s.cnts), i*4)) = cnt
}

// zgcForeignSetAlloc allocates key/cnt storage for n entries in one
// off-heap block: keys occupy the first n words, counts follow.
func zgcForeignSetAlloc(n uintptr) (keys, cnts uintptr, ok bool) {
	total := n*unsafe.Sizeof(uintptr(0)) + n*4
	raw := sysAlloc(total, &memstats.other_sys, "logos zgc foreign set")
	if raw == nil {
		return 0, 0, false
	}
	return uintptr(raw), uintptr(add(raw, n*unsafe.Sizeof(uintptr(0)))), true
}

// zgcForeignSetRehashTo moves all live entries to a fresh table of
// newCap entries. The old block is freed. Counts are preserved.
func zgcForeignSetRehashTo(s *zgcForeignSet, newCap uintptr) bool {
	keys, cnts, ok := zgcForeignSetAlloc(newCap)
	if !ok {
		return false
	}
	old := *s
	*s = zgcForeignSet{keys: keys, cnts: cnts, mask: newCap - 1, cap: newCap}
	for i := uintptr(0); i < old.cap; i++ {
		k := *(*uintptr)(add(unsafe.Pointer(old.keys), i*unsafe.Sizeof(uintptr(0))))
		if k == 0 || k == ^uintptr(0) {
			continue
		}
		c := *(*uint32)(add(unsafe.Pointer(old.cnts), i*4))
		h := (k * zgcForeignSetHashMul) >> 16 & s.mask
		for {
			if zgcForeignSetKeyAt(s, h) == 0 {
				zgcForeignSetSetAt(s, h, k, c)
				s.live++
				break
			}
			h = (h + 1) & s.mask
		}
	}
	if old.cap != 0 {
		total := old.cap*unsafe.Sizeof(uintptr(0)) + old.cap*4
		sysFree(unsafe.Pointer(old.keys), total, &memstats.other_sys)
	}
	return true
}

// zgcForeignSetFind returns the table index holding key.
//
//go:nosplit
func zgcForeignSetFind(s *zgcForeignSet, key uintptr) (uintptr, bool) {
	if s.cap == 0 {
		return 0, false
	}
	h := (key * zgcForeignSetHashMul) >> 16 & s.mask
	for n := uintptr(0); n < s.cap; n++ {
		i := (h + n) & s.mask
		k := zgcForeignSetKeyAt(s, i)
		if k == key {
			return i, true
		}
		if k == 0 {
			return 0, false
		}
	}
	return 0, false
}

// zgcForeignSetAddRef increments key's refcount, inserting at 1 when
// absent. It reports whether the key is newly inserted and whether the
// operation succeeded (false only on off-heap exhaustion).
func zgcForeignSetAddRef(s *zgcForeignSet, key uintptr) (isNew, ok bool) {
	if key == 0 || key == ^uintptr(0) {
		return false, false
	}
	if s.cap == 0 {
		if !zgcForeignSetRehashTo(s, zgcForeignSetMinCap) {
			return false, false
		}
	}
	if (s.live+s.tomb)*4 >= s.cap*3 {
		if !zgcForeignSetRehashTo(s, s.cap*2) {
			return false, false
		}
	}
	h := (key * zgcForeignSetHashMul) >> 16 & s.mask
	firstTomb := s.cap // sentinel: no tombstone seen
	for n := uintptr(0); n < s.cap; n++ {
		i := (h + n) & s.mask
		k := zgcForeignSetKeyAt(s, i)
		if k == key {
			zgcForeignSetSetAt(s, i, k, zgcForeignSetCntAt(s, i)+1)
			return false, true
		}
		if k == 0 {
			if firstTomb != s.cap {
				i = firstTomb
				s.tomb--
			}
			zgcForeignSetSetAt(s, i, key, 1)
			s.live++
			return true, true
		}
		if k == ^uintptr(0) && firstTomb == s.cap {
			firstTomb = i
		}
	}
	if firstTomb != s.cap {
		// Table full of live entries plus a tombstone: reuse it.
		zgcForeignSetSetAt(s, firstTomb, key, 1)
		s.live++
		s.tomb--
		return true, true
	}
	return false, false
}

// zgcForeignSetRelease decrements key's refcount, removing it at zero.
// Unbalanced release (unknown key) throws: refcounts pair exactly with
// frame-chunk entries, so this signals runtime corruption.
func zgcForeignSetRelease(s *zgcForeignSet, key uintptr) {
	i, ok := zgcForeignSetFind(s, key)
	if !ok {
		throw("logos zgc: foreign exposure release without a matching acquire")
	}
	if c := zgcForeignSetCntAt(s, i); c <= 1 {
		zgcForeignSetSetAt(s, i, ^uintptr(0), 0)
		s.live--
		s.tomb++
	} else {
		zgcForeignSetSetAt(s, i, key, c-1)
	}
	// Reclaim table storage when mostly empty, keeping a floor.
	// Shrinking is best-effort: exhaustion keeps the larger table.
	if s.cap > zgcForeignSetMinCap && s.live*8 < s.cap {
		zgcForeignSetRehashTo(s, s.cap/2)
	}
}

// zgcForeignSetHas reports membership without mutating. Nosplit-safe:
// pure loads and arithmetic, so the NoSplit self-coverage check may use
// it while the world runs.
//
//go:nosplit
func zgcForeignSetHas(s *zgcForeignSet, key uintptr) bool {
	_, ok := zgcForeignSetFind(s, key)
	return ok
}

// ---- frame and chunk storage ----

// Storage discipline: frames come from the owning g's single spare slot
// or fresh off-heap memory; chunks are always fresh off-heap memory and
// return straight to the OS. There are no shared free lists, so no path
// needs synchronization for allocation: STW admission (exclusive by
// worldsema) and world-running teardown never share mutable allocator
// state. Steady-state reuse comes from the per-g spare frame.

// zgcForeignFrameAlloc returns a zeroed frame from fresh off-heap
// memory. Nil reports off-heap exhaustion. Sync-free (sysAlloc only),
// callable from any context including nosplit and STW.
func zgcForeignFrameAlloc() *zgcForeignFrame {
	raw := sysAlloc(unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys, "logos zgc foreign frame")
	if raw == nil {
		return nil
	}
	return (*zgcForeignFrame)(raw)
}

// zgcForeignChunkAlloc returns a zeroed chunk. Nil reports exhaustion.
// Sync-free like frame allocation.
func zgcForeignChunkAlloc() *zgcForeignObjChunk {
	raw := sysAlloc(unsafe.Sizeof(zgcForeignObjChunk{}), &memstats.other_sys, "logos zgc foreign chunk")
	if raw == nil {
		return nil
	}
	return (*zgcForeignObjChunk)(raw)
}

// zgcForeignFrameAppend records one admitted base in the frame. The
// global refcount increment must already be applied; on chunk-exhaustion
// the caller releases what it added.
func zgcForeignFrameAppend(f *zgcForeignFrame, base uintptr) bool {
	head := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks))
	if head == nil || head.n >= zgcForeignObjsPerChunk {
		nc := zgcForeignChunkAlloc()
		if nc == nil {
			return false
		}
		nc.next = f.chunks
		f.chunks = uintptr(unsafe.Pointer(nc))
		head = nc
	}
	head.addrs[head.n] = base
	head.n++
	f.nObjs++
	return true
}

// zgcForeignFrameDestroyChunks frees all frame chunks straight to the
// OS. Sync-free: chunks are frame-private at every call site (published
// frames are retired first; unpublished frames never shared), so no gate
// or STW pairing is needed.
func zgcForeignFrameDestroyChunks(f *zgcForeignFrame) {
	for f.chunks != 0 {
		c := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks))
		f.chunks = c.next
		zgcForeignChunkDestroy(c)
	}
	f.nObjs = 0
}

// ---- admission work queue (STW-exclusive scratch) ----

func zgcForeignQueueReset(q *zgcForeignQueue) {
	q.head, q.tail = 0, 0
}

func zgcForeignQueuePush(q *zgcForeignQueue, v uintptr) bool {
	if q.tail >= q.cap {
		newCap := q.cap * 2
		if newCap == 0 {
			newCap = 256
		}
		raw := sysAlloc(newCap*unsafe.Sizeof(uintptr(0)), &memstats.other_sys, "logos zgc foreign queue")
		if raw == nil {
			return false
		}
		for i := uintptr(0); i < q.tail; i++ {
			*(*uintptr)(add(raw, i*unsafe.Sizeof(uintptr(0)))) = *(*uintptr)(add(unsafe.Pointer(q.base), i*unsafe.Sizeof(uintptr(0))))
		}
		if q.cap != 0 {
			sysFree(unsafe.Pointer(q.base), q.cap*unsafe.Sizeof(uintptr(0)), &memstats.other_sys)
		}
		q.base = uintptr(raw)
		q.cap = newCap
	}
	*(*uintptr)(add(unsafe.Pointer(q.base), q.tail*unsafe.Sizeof(uintptr(0)))) = v
	q.tail++
	return true
}

// zgcForeignQueueShrink releases queue headroom after an admission that
// used far less than a previously grown peak. Best-effort.
func zgcForeignQueueShrink(q *zgcForeignQueue) {
	if q.cap <= 1024 || q.tail*4 >= q.cap {
		return
	}
	newCap := uintptr(1024)
	raw := sysAlloc(newCap*unsafe.Sizeof(uintptr(0)), &memstats.other_sys, "logos zgc foreign queue")
	if raw == nil {
		return
	}
	for i := uintptr(0); i < q.tail; i++ {
		*(*uintptr)(add(raw, i*unsafe.Sizeof(uintptr(0)))) = *(*uintptr)(add(unsafe.Pointer(q.base), i*unsafe.Sizeof(uintptr(0))))
	}
	sysFree(unsafe.Pointer(q.base), q.cap*unsafe.Sizeof(uintptr(0)), &memstats.other_sys)
	q.base = uintptr(raw)
	q.cap = newCap
}

// ---- heap resolution ----

// zgcForeignHeapBase resolves w to its heap object base, interior offset,
// and span, using allocation validity, not the previous cycle's mark
// bits. A small-span slot is allocated iff idx < freeindex or its alloc
// bit is set: bump-pointer allocation advances freeindex without setting
// alloc bits, so an alloc-bit-only test would reject freshly allocated
// ordinary objects as non-heap (the same composite the sweeper and the
// immix paths use). Non-heap words (scalars, stack, C/mmap addresses,
// arena chunks) and free slots report false. Never admit a slot this
// rejects: it names garbage or free memory.
//
// Stability premise: the world stopped freezes span metadata. With the
// world running, malloc and sweep mutate it concurrently and the foreign
// gate does not stop them (it serializes registry state only), so a
// world-running caller must keep the queried memory alive by other
// means: a live Go reference in its own frames or arguments, or a
// published frame's refcount on an already-exposed base. The gate alone
// is never a heap freeze.
func zgcForeignHeapBase(w uintptr) (base, off uintptr, s *mspan, ok bool) {
	if w == 0 {
		return 0, 0, nil, false
	}
	a := zgcCanonical(w)
	sp := spanOfHeap(a)
	if sp == nil || sp.isUserArenaChunk {
		return 0, 0, nil, false
	}
	if sp.spanclass.sizeclass() == 0 {
		if sp.allocCount == 0 {
			return 0, 0, nil, false
		}
		return sp.startAddr, a - sp.startAddr, sp, true
	}
	idx := sp.objIndex(a)
	if idx >= uintptr(sp.nelems) {
		return 0, 0, nil, false
	}
	if idx >= uintptr(sp.freeindex) && !sp.allocBitsForIndex(idx).isMarked() {
		return 0, 0, nil, false
	}
	b := sp.startAddr + idx*sp.elemsize
	if a < b || a >= b+sp.elemsize {
		return 0, 0, nil, false
	}
	return b, a - b, sp, true
}

// zgcForeignHeapBaseNoSplit is the nosplit subset of resolution: strip
// color bits with pure arithmetic and resolve the object base through
// nosplit span helpers only, with span bounds as the only validity
// check. It deliberately does NOT read freeindex or alloc bits: sweep
// is concurrent and another P may own the span, so allocator metadata
// can change under the read and a validity check could throw on a
// lawful covered pointer. Membership against the caller's own admitted
// frames is the exact admission test; resolution only maps a word to
// the base that membership decides on. A garbage word that resolves
// into a span either misses (throw) or hits an admitted base whose
// marker is then vacuous but harmless.
//
//go:nosplit
func zgcForeignHeapBaseNoSplit(w uintptr) (base uintptr, ok bool) {
	if w == 0 {
		return 0, false
	}
	// Strip only the active color bits and reject anything above the
	// heap range, exactly as zgcForeignQueryKey does. Masking with
	// zgcAddrMask instead would truncate a high C, stack, or mmap word
	// into the heap range and risk aliasing a live span.
	a := w &^ atomic.Loaduintptr(&zgcColorMaskBits)
	if a == 0 || a > zgcAddrMask {
		return 0, false
	}
	sp := spanOfHeap(a)
	if sp == nil || sp.isUserArenaChunk {
		return 0, false
	}
	if sp.spanclass.sizeclass() == 0 {
		if a < sp.startAddr || a >= sp.limit {
			return 0, false
		}
		return sp.startAddr, true
	}
	idx := sp.objIndex(a)
	if idx >= uintptr(sp.nelems) {
		return 0, false
	}
	b := sp.startAddr + idx*sp.elemsize
	if a < b || a >= b+sp.elemsize {
		return 0, false
	}
	return b, true
}

// zgcForeignEnsureCopy returns the canonical base for an admitted
// object: the published copy when its page already relocated the object
// (Done), otherwise a synchronous copy-help through the single typed
// relocation implementation, or the base itself for unregistered pages
// and pinned large objects. Only Done adopts a copy; NoMemory alone
// reports (0, false) so the admission rolls back fail-closed (never a
// partial frame, never user-slot writes); NotHeap returns (base, true)
// as real foreign/ordinary identity under proper lifetime (world stopped
// freezes span metadata, so the source is stable for the STW admit);
// Refused returns (base, true) for the in-place exposure below (a source
// the gate veto freezes at commit: already-exposed shared bases and
// pinned first-exposed sources), never failing the admission and never
// waiting for unexposure, so same-G callback Extend and re-admission of
// an active exposure stay independent and complete. It takes only
// page-state, identity, and gate leaf locks, never speciallock: the
// caller runs it STW-paired (worldsema exclusive, no retire contends)
// and records the winner afterwards under the existing token, closure,
// generation, NoP, and refcount invariants. The copy/commit owner (B)
// will tighten this into the indivisible commit without changing its
// contract. The old boolean shape stays: true means admit the returned
// base (copy or in-place source), false means fail the admission.
func zgcForeignEnsureCopy(base uintptr) (uintptr, bool) {
	pg := zgcPageAcquire(base)
	if pg == nil {
		return base, true
	}
	if to, ok := pg.forward(base); ok {
		zgcPageRelease(pg)
		return to, true
	}
	to, st := zgcRelocateOneResult(pg, base)
	zgcPageRelease(pg)
	switch st {
	case zgcRelocateDone:
		return to, true
	case zgcRelocateNoMemory:
		return 0, false
	case zgcRelocateNotHeap:
		return base, true
	default:
		return base, true
	}
}

// zgcForeignMarkAdmitted shades the mark bit of an admitted base
// directly (world stopped, non-atomic). Admission-time marking in every
// phase keeps sweep from freeing exposed objects before the next
// mark-start re-shade; traversal completeness at admission plus Extend
// for callback additions substitutes for collector scanning of these
// pre-marked objects.
func zgcForeignMarkAdmitted(s *mspan, base uintptr) {
	idx := uintptr(0)
	if s.spanclass.sizeclass() != 0 {
		idx = s.objIndex(base)
	}
	mbits := s.markBitsForIndex(idx)
	if useCheckmark {
		setCheckmark(base, 0, 0, mbits)
		return
	}
	if !mbits.isMarked() {
		mbits.setMarkedNonAtomic()
	}
}

// ---- admission core ----

// zgcForeignRegionKey returns the stable pin key of a canonical object
// address: its 2 MB-aligned region. Registration state of the page
// directory changes over a pin's lifetime (selection registers, release
// unregisters), so the key must be a pure function of the address. One
// key per object base suffices: a large object's span never moves, and
// selection excludes every page a non-contained span overlaps.
//
//go:nosplit
func zgcForeignRegionKey(addr uintptr) uintptr {
	return addr &^ (zgcPageSize - 1)
}

// zgcForeignAdmitFrame and zgcForeignAdmitQueueRef serve the
// STW-exclusive traversal visitor below: pause paths must not capture
// closures, so named package state replaces them.
//
// zgcForeignAdmitSeen is the per-admission visited set: canonical bases
// already owned by THIS admission (Begin or Extend call), independent of
// the global exposure tables. Traversal queues on first sight in this
// admission, never on global isNew: an object already exposed by another
// live frame is still enqueued and fully expanded for this frame, so
// every frame owns its complete transitive closure and no frame's End
// releases descendants another live frame still needs (global refcounts
// keep them alive). Deduplication is hash-based (this set), never a
// quadratic chunk scan.
//
// zgcForeignAdmitPending defers caller-slot canonicalization to commit:
// Value/Unknown resolutions record (slot, canonical+off) here during
// traversal and apply them only after the queue drains successfully. A
// failed admission therefore leaves no partly rewritten descriptors:
// user memory is untouched (mark bits excepted, conservatively) and the
// partial frame rolls back before Begin returns invalid or Extend
// throws. Both scratch areas hold only canonical bases and transient
// slot addresses; they are freed before startTheWorld in every path, so
// no descriptor, slot, or backing-array pointer is retained after
// admission.
var (
	zgcForeignAdmitFrame    *zgcForeignFrame
	zgcForeignAdmitQueueRef *zgcForeignQueue
	zgcForeignAdmitFailed   bool
	zgcForeignAdmitSeen     zgcForeignSet
)

// zgcForeignPendingWrite is one deferred caller-slot canonicalization:
// *slot := val, applied only at commit.
type zgcForeignPendingWrite struct {
	slot uintptr
	val  uintptr
}

var (
	zgcForeignAdmitPendingBase uintptr // *[cap]zgcForeignPendingWrite, off-heap STW scratch
	zgcForeignAdmitPendingN    uintptr
	zgcForeignAdmitPendingCap  uintptr
)

// zgcForeignSetDestroy frees a set's off-heap block and zeroes it, so a
// burst's peak is reclaimed rather than retained. Safe on an empty set.
func zgcForeignSetDestroy(s *zgcForeignSet) {
	if s.cap != 0 {
		total := s.cap*unsafe.Sizeof(uintptr(0)) + s.cap*4
		sysFree(unsafe.Pointer(s.keys), total, &memstats.other_sys)
	}
	*s = zgcForeignSet{}
}

// zgcForeignQueueReserve pre-reserves room for extra queue entries before
// any global mutation, so the post-commit push below cannot fail after
// refcounts and frame entries are already published to the partial
// admission. False reports off-heap exhaustion with nothing mutated.
func zgcForeignQueueReserve(q *zgcForeignQueue, extra uintptr) bool {
	// Overflow fails closed before any allocation or mutation: a
	// wrapped need or capacity must never size a copy or a push.
	if extra > ^uintptr(0)-q.tail {
		return false
	}
	need := q.tail + extra
	if need <= q.cap {
		return true
	}
	newCap := q.cap * 2
	if newCap == 0 {
		newCap = 256
	}
	for newCap < need {
		// Absurd growth (256 Gi entries) is exhaustion, not a
		// size to allocate: fail before the doubling can wrap.
		if newCap >= 1<<38 {
			return false
		}
		newCap *= 2
	}
	raw := sysAlloc(newCap*unsafe.Sizeof(uintptr(0)), &memstats.other_sys, "logos zgc foreign queue")
	if raw == nil {
		return false
	}
	for i := uintptr(0); i < q.tail; i++ {
		*(*uintptr)(add(raw, i*unsafe.Sizeof(uintptr(0)))) = *(*uintptr)(add(unsafe.Pointer(q.base), i*unsafe.Sizeof(uintptr(0))))
	}
	if q.cap != 0 {
		sysFree(unsafe.Pointer(q.base), q.cap*unsafe.Sizeof(uintptr(0)), &memstats.other_sys)
	}
	q.base = uintptr(raw)
	q.cap = newCap
	return true
}

// zgcForeignFramePop drops the most recently appended frame entry. The
// atomic-fallback path in AdmitBaseSTW calls it immediately after its
// own Append, so the head chunk is known non-empty.
func zgcForeignFramePop(f *zgcForeignFrame) {
	head := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks))
	head.n--
	head.addrs[head.n] = 0
	f.nObjs--
}

// zgcForeignSlotExposed reports whether the caller-slot address itself
// lies in a published foreign-exposed heap object. Such slots are
// C-visible memory: admission must neither rewrite them (racing live C
// readers) nor silently skip them (handing C a stale or colored word).
// The caller fails loud instead. Stack, global, and foreign addresses
// report false and remain canonicalization targets. The resolution
// canonicalizes first, so a slot in a relocated source object still
// matches its recorded copy base.
func zgcForeignSlotExposed(slot unsafe.Pointer) bool {
	base, _, _, ok := zgcForeignHeapBase(uintptr(slot))
	if !ok {
		return false
	}
	return zgcForeignSetHas(&zgcForeignObjects, base)
}

// zgcForeignAdmitPendingReserve allocates room for up to n deferred
// slot writes as the initial cap. Typed walks emit one entry per
// resolved pointer cell, unbounded by the descriptor count; Push grows
// the vector, so this is only a seed. False reports exhaustion with
// nothing mutated.
func zgcForeignAdmitPendingReserve(n uintptr) bool {
	zgcForeignAdmitPendingBase = 0
	zgcForeignAdmitPendingN = 0
	zgcForeignAdmitPendingCap = 0
	if n == 0 {
		return true
	}
	elem := unsafe.Sizeof(zgcForeignPendingWrite{})
	if n > ^uintptr(0)/elem {
		return false
	}
	raw := sysAlloc(n*elem, &memstats.other_sys, "logos zgc foreign pending")
	if raw == nil {
		return false
	}
	zgcForeignAdmitPendingBase = uintptr(raw)
	zgcForeignAdmitPendingCap = n
	return true
}

// zgcForeignAdmitPendingPush records one deferred canonicalization,
// growing the off-heap vector as needed: typed walks emit one entry per
// resolved pointer cell, unbounded by the descriptor count. False
// reports off-heap exhaustion with nothing else mutated.
func zgcForeignAdmitPendingPush(slot unsafe.Pointer, val uintptr) bool {
	// Idempotent: a cell that already holds the canonical word needs
	// no store. This keeps re-admission of an already-canonical cell
	// (nested Begin/Extend over a slot an outer call published) from
	// writing into memory C may be reading.
	if *(*uintptr)(slot) == val {
		return true
	}
	if zgcForeignAdmitPendingN >= zgcForeignAdmitPendingCap {
		newCap := zgcForeignAdmitPendingCap * 2
		if newCap == 0 {
			newCap = 64
		}
		if newCap >= 1<<38 {
			return false
		}
		raw := sysAlloc(newCap*unsafe.Sizeof(zgcForeignPendingWrite{}), &memstats.other_sys, "logos zgc foreign pending")
		if raw == nil {
			return false
		}
		for i := uintptr(0); i < zgcForeignAdmitPendingN; i++ {
			*(*zgcForeignPendingWrite)(add(raw, i*unsafe.Sizeof(zgcForeignPendingWrite{}))) = *(*zgcForeignPendingWrite)(add(unsafe.Pointer(zgcForeignAdmitPendingBase), i*unsafe.Sizeof(zgcForeignPendingWrite{})))
		}
		if zgcForeignAdmitPendingCap != 0 {
			sysFree(unsafe.Pointer(zgcForeignAdmitPendingBase), zgcForeignAdmitPendingCap*unsafe.Sizeof(zgcForeignPendingWrite{}), &memstats.other_sys)
		}
		zgcForeignAdmitPendingBase = uintptr(raw)
		zgcForeignAdmitPendingCap = newCap
	}
	w := (*zgcForeignPendingWrite)(add(unsafe.Pointer(zgcForeignAdmitPendingBase), zgcForeignAdmitPendingN*unsafe.Sizeof(zgcForeignPendingWrite{})))
	w.slot = uintptr(slot)
	w.val = val
	zgcForeignAdmitPendingN++
	return true
}

// zgcForeignAdmitValueCell admits one Value variable cell: resolve,
// canonicalize at commit, retain the referent. Cells inside published
// heap exposure throw fail-loud — a Value slot must be private, and
// skipping would hand C a stale or colored argument word. Stack
// C-visibility is not tracked here: emission must name only fresh
// private cells per admission and never reuse a stack cell already
// observed by an active outer C call; nested Extend descriptors must be
// callback-private. False reports off-heap exhaustion.
func zgcForeignAdmitValueCell(f *zgcForeignFrame, q *zgcForeignQueue, cell uintptr) bool {
	base, off, _, ok := zgcForeignHeapBase(*(*uintptr)(unsafe.Pointer(cell)))
	if !ok {
		return true
	}
	if zgcForeignSlotExposed(unsafe.Pointer(cell)) {
		throw("logos zgc: foreign root cell inside published exposure")
	}
	return zgcForeignAdmitCanonCell(f, q, cell, base, off)
}

// zgcForeignAdmitWholeCell admits one Whole extent cell: resolve and
// retain the referent. Only non-heap cells (stack, global) are
// canonicalized at commit; a heap-resident cell is never written here,
// whatever its exposure state. The extent's heap backing is admitted by
// its paired Value descriptor in the same admission, and the result is
// order-independent: a backing that gets a copy hands C the copy (a
// store into the source cell would land on a dead object), and a
// backing first exposed in place has its interior cells canonicalized
// by the queue-phase rewrite pass. Already-exposed backings are
// canonical from first exposure. False reports off-heap exhaustion.
func zgcForeignAdmitWholeCell(f *zgcForeignFrame, q *zgcForeignQueue, cell uintptr) bool {
	base, off, _, ok := zgcForeignHeapBase(*(*uintptr)(unsafe.Pointer(cell)))
	if !ok {
		return true
	}
	if _, _, _, inHeap := zgcForeignHeapBase(cell); inHeap {
		canon, ok := zgcForeignEnsureCopy(base)
		if !ok {
			return false
		}
		return zgcForeignAdmitBaseSTW(f, q, base, canon)
	}
	return zgcForeignAdmitCanonCell(f, q, cell, base, off)
}

// zgcForeignAdmitCanonCell is the write half shared by the Value and
// Whole cell paths once each has decided the cell is writable: copy-help
// the referent, defer the cell's canonical store to commit, and retain
// the referent. base and off come from the cell's current word. False
// reports off-heap exhaustion with nothing else mutated.
func zgcForeignAdmitCanonCell(f *zgcForeignFrame, q *zgcForeignQueue, cell, base, off uintptr) bool {
	canon, ok := zgcForeignEnsureCopy(base)
	if !ok {
		return false
	}
	if !zgcForeignAdmitPendingPush(unsafe.Pointer(cell), canon+off) {
		return false
	}
	return zgcForeignAdmitBaseSTW(f, q, base, canon)
}

// zgcForeignTypeCellsNoSplit is the nosplit-safe form of
// zgcForeignTypeCells for the restricted admission path. The iterator
// (and typePointers.next on every later advance) reads the type's GC
// mask through getGCMask; for an on-demand type (PtrBytes beyond the
// inline mask) an unbuilt mask would be built there with persistentalloc,
// systemstack, or an osyield spin, none of which the restricted path may
// do. This form only reads the mask slot: when the mask is built the
// walk is pure loads, otherwise it throws. Prepared-state contract:
// Begin and Extend build every declared descriptor type's mask while the
// world runs (zgcForeignPrebuildMasks), and a built mask is permanent,
// so a nested NoSplit descriptor is lawful exactly when its type was
// declared by an earlier Begin or Extend; the producer must admit it
// through one first.
//
//go:nosplit
func zgcForeignTypeCellsNoSplit(typ *_type, addr uintptr) typePointers {
	if typ.PtrBytes != 0 && typ.TFlag&abi.TFlagGCMaskOnDemand != 0 {
		p := atomic.Loadp(unsafe.Pointer(typ.GCData))
		if p == nil || p == unsafe.Pointer(&inProgress) {
			throw("logos zgc: foreign nosplit type mask unbuilt; declare the type through Begin or Extend first")
		}
	}
	return zgcForeignTypeCells(typ, addr)
}

// zgcForeignAdmitPendingApply commits every deferred canonicalization,
// then frees the scratch: caller slots now hold canonical copy
// addresses (offset preserved), uncolored per the store rule, so C reads
// plain-dereferenceable words. No descriptor, slot, or backing pointer
// is retained afterwards.
func zgcForeignAdmitPendingApply() {
	for i := uintptr(0); i < zgcForeignAdmitPendingN; i++ {
		w := (*zgcForeignPendingWrite)(add(unsafe.Pointer(zgcForeignAdmitPendingBase), i*unsafe.Sizeof(zgcForeignPendingWrite{})))
		*(*uintptr)(unsafe.Pointer(w.slot)) = w.val
	}
	zgcForeignAdmitPendingFree()
}

// zgcForeignTypeCells returns a pointer-cell iterator over one value of
// type typ at addr, mirroring mspan.typePointersOfType without a span:
// admission extents may lie on stacks, which have no span, and the
// running caller's own frames cannot be walked precisely while it runs
// (logosWalkStack skips running goroutines), so the emission-declared
// static layout is the authoritative precise path. Empty when typ
// carries no pointers; tiling across Whole elements is automatic in
// typePointers.next. Callers bound iteration with the authoritative
// extent end; Whole additionally requires an exact multiple.
//
//go:nosplit
func zgcForeignTypeCells(typ *_type, addr uintptr) typePointers {
	if typ == nil || typ.PtrBytes == 0 {
		return typePointers{}
	}
	gcmask := getGCMask(typ)
	if gcmask == nil {
		// Impossible for a valid pointer-carrying *_type (write
		// barriers rely on it unconditionally): fail loud rather
		// than silently under-discover. On-demand masks build on
		// first use like any barrier path.
		throw("logos zgc: foreign type without GC bitmap")
	}
	return typePointers{elem: addr, addr: addr, mask: readUintptr(gcmask), typ: typ}
}

var (
	// zgcForeignAdmitRewrite holds canonical bases this admission first
	// exposed in place (canonical address equals source: no copy was
	// taken, so C observes the object where it sits). Their pointer
	// fields are canonicalized during queue expansion; all other reached
	// objects expand read-only (already-exposed memory whose fields are
	// canonical from first exposure, or copy sources whose interiors
	// belong to the copy-commit owner). Membership only; destroyed with
	// the other per-admission scratch on every exit.
	zgcForeignAdmitRewrite zgcForeignSet
)

// zgcForeignAdmitScratchDestroy releases all per-admission scratch:
// the visited work-set, the in-place rewrite set, and any unapplied
// pending writes. Burst peaks are reclaimed, never retained.
func zgcForeignAdmitScratchDestroy() {
	zgcForeignSetDestroy(&zgcForeignAdmitSeen)
	zgcForeignSetDestroy(&zgcForeignAdmitRewrite)
	zgcForeignAdmitPendingFree()
}

// zgcForeignPrebuildMasks builds the on-demand GC bitmaps for every
// declared descriptor type while the world still runs. The admission
// stop must never spin on another thread's in-progress mask build
// (getGCMaskOnDemand parks on inProgress, and a stopped builder would
// park it forever) nor allocate inside the stop: after this returns,
// getGCMask on these types is a pure load. Call after shape validation,
// before stopTheWorld, in Begin and Extend (both preemptible with a P).
// Transitive heap-object masks stay the collector's own accepted
// profile — every stop already walks span type information.
func zgcForeignPrebuildMasks(roots *logosForeignRoot, n uintptr) {
	rootSize := unsafe.Sizeof(logosForeignRoot{})
	for i := uintptr(0); i < n; i++ {
		desc := (*logosForeignRoot)(add(unsafe.Pointer(roots), i*rootSize))
		if desc.Type != nil {
			getGCMask((*_type)(desc.Type))
		}
	}
}

// zgcForeignAdmitPendingFree discards deferred writes without applying
// them: failed admissions leave user memory untouched.
func zgcForeignAdmitPendingFree() {
	if zgcForeignAdmitPendingCap != 0 {
		sysFree(unsafe.Pointer(zgcForeignAdmitPendingBase), zgcForeignAdmitPendingCap*unsafe.Sizeof(zgcForeignPendingWrite{}), &memstats.other_sys)
	}
	zgcForeignAdmitPendingBase = 0
	zgcForeignAdmitPendingN = 0
	zgcForeignAdmitPendingCap = 0
}

// zgcForeignAdmitVisit expands one heap slot into the admission closure:
// resolve, copy-help, refcount, record, enqueue on first sight in this
// admission. Read-only on the heap slot — heap object memory is never
// rewritten while C may access it; only deferred caller-slot writes
// canonicalize, and those target private caller variables (verified by
// the descriptor path, which throws on C-visible slots). Allocation
// failure sets the failure flag instead of throwing, so the admission
// unwinds its unpublished partial frame and fails the entry closed.
func zgcForeignAdmitVisit(slot *uintptr) {
	if zgcForeignAdmitFailed {
		return
	}
	base, _, _, ok := zgcForeignHeapBase(*slot)
	if !ok {
		return
	}
	canon, ok := zgcForeignEnsureCopy(base)
	if !ok {
		zgcForeignAdmitFailed = true
		return
	}
	if !zgcForeignAdmitBaseSTW(zgcForeignAdmitFrame, zgcForeignAdmitQueueRef, base, canon) {
		zgcForeignAdmitFailed = true
	}
}

// zgcForeignAdmitVisitRewrite canonicalizes one pointer cell of an
// object this admission first exposed in place (member of the rewrite
// set): the container is unpublished and globally new. The cell is
// always rewritten to the canonical address — including when the
// referent was copied: C follows this in-place container directly, so
// leaving the source address would bypass the copy and reopen the
// copy-vs-C-write divergence for the nested edge. Single-word stores
// are atomic, so a concurrent heap-traversing C thread of another call
// sees old (retained) or new (canonical), never a tear. Already-exposed
// objects and copy interiors never reach this visitor; they expand
// through the read-only visitor above (copy interiors belong to the
// copy-commit owner).
func zgcForeignAdmitVisitRewrite(slot *uintptr) {
	// Dedicated write path, not the WholeCell helper: the container is
	// a rewrite-set member (globally new, in place, unpublished), so it
	// already sits in the global object set by construction and the
	// shared exposed-cell check would false-positive on this
	// admission's own refs and leave every field stale. No check is
	// needed instead of skipped: no published frame owns the container,
	// so no C-visible memory is rewritten; single-word stores keep a
	// concurrent heap-traversing C thread tear-free (old retained or
	// new canonical).
	if zgcForeignAdmitFailed {
		return
	}
	base, off, _, ok := zgcForeignHeapBase(*slot)
	if !ok {
		return
	}
	canon, ok := zgcForeignEnsureCopy(base)
	if !ok {
		zgcForeignAdmitFailed = true
		return
	}
	if !zgcForeignAdmitPendingPush(unsafe.Pointer(slot), canon+off) {
		zgcForeignAdmitFailed = true
		return
	}
	if !zgcForeignAdmitBaseSTW(zgcForeignAdmitFrame, zgcForeignAdmitQueueRef, base, canon) {
		zgcForeignAdmitFailed = true
	}
}

// zgcForeignWholeRange bounds a Whole-extent scan to bytes the
// descriptor actually owns. Overflow fails closed (a near-2^64 size is
// never honest emission). The loop below reads only whole pointer words
// fully inside [slot, slot+size): pointers are word-aligned and fit
// only in whole words, so no out-of-range byte is ever read and no
// contained pointer is ever skipped. The align-up adds at most 7 to a
// base bounded by end, so it cannot wrap. Pure arithmetic,
// nosplit-safe.
//
//go:nosplit
func zgcForeignWholeRange(slot unsafe.Pointer, size uintptr) (lo, end uintptr, ok bool) {
	ptrSize := unsafe.Sizeof(uintptr(0))
	base := uintptr(slot)
	if size > ^uintptr(0)-base {
		return 0, 0, false
	}
	end = base + size
	off := (^base + 1) & (ptrSize - 1)
	if off > size {
		return end, end, true
	}
	return base + off, end, true
}

// zgcForeignAdmitBaseSTW admits one canonical base: per-admission
// dedupe, refcount, record, page pin, mark bit, enqueue on first sight
// in this admission. base is the source object base, canon the admitted
// (possibly copied) base. The unit is atomic: queue room is pre-reserved
// before any global mutation, and the retained push-failure fallback
// releases what this call added and pops the frame entry, so a failed
// call leaves no partial refcount, page credit, chunk entry, rewrite
// record, or unowned queue slot behind. STW-paired and gateless:
// worldsema exclusion (linearizable stop-the-world section; no
// concurrent admission overlaps) covers the global sets while the world
// is stopped.
func zgcForeignAdmitBaseSTW(f *zgcForeignFrame, q *zgcForeignQueue, base, canon uintptr) bool {
	// Per-admission visited work-set: hash-deduplicated, independent of
	// the global tables. An object already exposed by another live
	// frame is still fully owned and expanded for THIS frame, so this
	// frame's closure is complete and its End cannot strand descendants
	// another frame needs (global refcounts arbitrate lifetime:
	// linearizable acquire/release pairing under worldsema here, under
	// the gate for world-running retire).
	if zgcForeignSetHas(&zgcForeignAdmitSeen, canon) {
		return true
	}
	if _, ok := zgcForeignSetAddRef(&zgcForeignAdmitSeen, canon); !ok {
		return false
	}
	// Frame ownership is checked before any global refcount, page
	// credit, or chunk entry is taken: a base this frame already owns
	// (admitted by an earlier call on the same frame) needs no new
	// ownership record. It is still queued so this admission rescans
	// its current fields for callback-added descendants.
	if zgcForeignSetHas(&f.owned, canon) {
		// Queue room is reserved before the push below, so no
		// partial state exists to unwind on this path.
		if !zgcForeignQueueReserve(q, 1) {
			zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
			return false
		}
		if !zgcForeignQueuePush(q, canon) {
			zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
			return false
		}
		return true
	}
	// Pre-reserve the queue slot before publication: a post-mutation
	// QueuePush failure must be impossible, not merely unwound.
	if !zgcForeignQueueReserve(q, 1) {
		zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
		return false
	}
	globalNew, ok := zgcForeignSetAddRef(&zgcForeignObjects, canon)
	if !ok {
		zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
		return false
	}
	if _, ok := zgcForeignSetAddRef(&zgcForeignPages, zgcForeignRegionKey(canon)); !ok {
		zgcForeignSetRelease(&zgcForeignObjects, canon)
		zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
		return false
	}
	if !zgcForeignFrameAppend(f, canon) {
		zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(canon))
		zgcForeignSetRelease(&zgcForeignObjects, canon)
		zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
		return false
	}
	// Per-frame independent ownership, hash-recorded: distinct bases
	// this frame admitted. This admission reached here only for bases
	// the frame does not yet own, so the insert always adds; release
	// iterates chunks, never this set. Counts stay 0/1.
	if _, ok := zgcForeignSetAddRef(&f.owned, canon); !ok {
		zgcForeignFramePop(f)
		zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(canon))
		zgcForeignSetRelease(&zgcForeignObjects, canon)
		zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
		return false
	}
	// First exposure in place (no copy taken: C observes the object
	// where it sits) records the base for field canonicalization during
	// queue expansion. Copies (C observes the copy) and already-exposed
	// objects (fields canonical from first exposure) expand read-only.
	if canon == base && globalNew {
		if _, ok := zgcForeignSetAddRef(&zgcForeignAdmitRewrite, canon); !ok {
			zgcForeignSetRelease(&f.owned, canon)
			zgcForeignFramePop(f)
			zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(canon))
			zgcForeignSetRelease(&zgcForeignObjects, canon)
			zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
			return false
		}
	}
	s := spanOfHeap(canon)
	if s != nil {
		zgcForeignMarkAdmitted(s, canon)
	}
	if !zgcForeignQueuePush(q, canon) {
		if zgcForeignSetHas(&zgcForeignAdmitRewrite, canon) {
			zgcForeignSetRelease(&zgcForeignAdmitRewrite, canon)
		}
		zgcForeignSetRelease(&f.owned, canon)
		zgcForeignFramePop(f)
		zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(canon))
		zgcForeignSetRelease(&zgcForeignObjects, canon)
		zgcForeignSetRelease(&zgcForeignAdmitSeen, canon)
		return false
	}
	return true
}

// zgcForeignFrameDestroyOwned frees a frame's owned-membership scratch.
// Membership only (release iterates chunks), so counts are irrelevant at
// destroy. Safe on empty frames: markers never own. Sync-free
// (sysFree only), callable gateless and world-running: the set is
// frame-private at every disposal site.
func zgcForeignFrameDestroyOwned(f *zgcForeignFrame) {
	zgcForeignSetDestroy(&f.owned)
}

// zgcForeignAdmitDescriptorSTW admits one descriptor under the settled
// per-Kind Type contract: Value walks one variable (one word typeless,
// full GC bitmap typed), Whole tiles its element bitmap across the
// authoritative Size bound, Unknown is one typeless word. Resolved cells
// canonicalize at commit; scalar cells stay bit-identical; cells inside
// published exposure throw fail-loud. STW-paired and gateless;
// copy-help takes only leaf locks, never the gate.
//
// Store/color discipline (verified against zgcUntagFast,
// logosZgcCanonicalStoreValue, and the logosWalkObject/Stack walk core):
// stack, scalar-lookalike, and foreign words pass untagging through
// unchanged, so scalar integer words stay bit-identical; only canonical
// uncolored copy words are ever stored, and only into caller-owned or
// first-exposed-in-place cells. Typeless running-stack extents are never
// walked from dynamic frame maps (logosWalkStack skips running
// goroutines, and this admission's caller is running): static Type
// layout is the authoritative precise path, typeless Whole is rejected.
func zgcForeignAdmitDescriptorSTW(f *zgcForeignFrame, q *zgcForeignQueue, desc *logosForeignRoot) bool {
	switch desc.Kind {
	case logosForeignRootValue, logosForeignRootUnknown:
		if desc.Slot == nil {
			throw("logos zgc: foreign descriptor with nil slot")
		}
		if desc.Kind == logosForeignRootUnknown && desc.Type != nil {
			throw("logos zgc: foreign unknown root with type")
		}
		if desc.Size != 0 {
			throw("logos zgc: foreign value root with size")
		}
		if desc.Type == nil {
			// Exactly one pointer-sized word (contract): the
			// precise case for pointer variables and unsafe
			// referents. Scalars pass through bit-identical;
			// stack referents stay covered by the collector's
			// own stack scan.
			return zgcForeignAdmitValueCell(f, q, uintptr(desc.Slot))
		}
		// Typed aggregate variable: walk every pointer cell of one
		// value using the type's GC bitmap; scalar cells pass through
		// untouched by construction (the mask never names them, so a
		// heap-looking integer is never rewritten).
		typ := (*_type)(desc.Type)
		slot := uintptr(desc.Slot)
		if typ.Size_ > ^uintptr(0)-slot {
			return false
		}
		limit := slot + typ.Size_
		tp := zgcForeignTypeCells(typ, slot)
		for {
			var cell uintptr
			tp, cell = tp.next(limit)
			if cell == 0 {
				break
			}
			if !zgcForeignAdmitValueCell(f, q, cell) {
				return false
			}
		}
		return true
	case logosForeignRootWhole:
		// Valid-empty: a zero extent admits nothing regardless of
		// Slot/Type, so Begin/Extend stay End-paired for empty
		// closures and NoSplit stays token-free.
		if desc.Size == 0 {
			return true
		}
		if desc.Slot == nil {
			throw("logos zgc: foreign whole descriptor with nil slot")
		}
		typ := (*_type)(desc.Type)
		if typ == nil {
			// No layout, no justified stores: typeless discovery
			// over a multi-word C-observed extent is rejected
			// fail-closed rather than silently handing C stale
			// cells. Scalar backings pass their scalar element
			// type (zero cells, nothing rewritten).
			return false
		}
		elemSize := typ.Size_
		if elemSize == 0 || desc.Size%elemSize != 0 {
			return false
		}
		// Size is authoritative and never inferred from Type (a
		// slice type's Size_ is its header, not its backing:
		// inferring it would silently under-pin). The extent covers
		// the typed backing including spare capacity, exactly as
		// emission sized it.
		base := uintptr(desc.Slot)
		if desc.Size > ^uintptr(0)-base {
			return false
		}
		end := base + desc.Size
		// Pointer offsets are relative to Slot: tiling from an
		// align-up address would shift every field. Misaligned
		// extents with pointer cells fail closed instead of
		// reinterpreting; scalar backings (zero pointer cells) may
		// start unaligned and need no walk.
		if typ.PtrBytes != 0 && base&(unsafe.Sizeof(uintptr(0))-1) != 0 {
			return false
		}
		tp := zgcForeignTypeCells(typ, base)
		for {
			var cell uintptr
			tp, cell = tp.next(end)
			if cell == 0 {
				break
			}
			if !zgcForeignAdmitWholeCell(f, q, cell) {
				return false
			}
		}
		return true
	}
	throw("logos zgc: foreign descriptor with unknown kind")
	return false
}

// zgcForeignChunkDestroy frees one chunk directly to the OS. Sync-free
// (no cache, no gate): used to unwind unpublished admissions, whose
// chunks are still frame-private.
func zgcForeignChunkDestroy(c *zgcForeignObjChunk) {
	sysFree(unsafe.Pointer(c), unsafe.Sizeof(zgcForeignObjChunk{}), &memstats.other_sys)
}

// zgcForeignRollbackSTW undoes an unpublished partial admission:
// every frame-chunk entry pairs with one object refcount and, for
// registered pages, one page pin. STW-paired and gateless; chunks go
// straight back to the OS so no shared cache is touched.
func zgcForeignRollbackSTW(f *zgcForeignFrame) {
	for f.chunks != 0 {
		c := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks))
		f.chunks = c.next
		for i := uint32(0); i < c.n; i++ {
			base := c.addrs[i]
			zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(base))
			zgcForeignSetRelease(&zgcForeignObjects, base)
			// Membership removal is exact: every entry appended
			// by an admission inserted ownership exactly once
			// (already-owned bases take no new entries), so the
			// guard below always hits; it stays as a safety net.
			if zgcForeignSetHas(&f.owned, base) {
				zgcForeignSetRelease(&f.owned, base)
			}
		}
		zgcForeignChunkDestroy(c)
	}
	f.nObjs = 0
}

// zgcForeignAdmitSTW runs the full admission with the world stopped:
// resolve, copy-help, traverse the complete object closure, mark,
// commit. It is gateless (worldsema exclusion covers the global sets)
// and acquires only leaf locks via copy-help, so the debug lock order
// holds. The admission is a transaction: descriptor slots are rewritten
// only at commit, after the queue drains. On failure the partial frame
// is rolled back, deferred writes are discarded (user memory untouched),
// scratch is freed, and false is returned; the caller unwinds without
// entering foreign code. Publication is a separate gateless step in the
// caller (STW-held worldsema).
func zgcForeignAdmitSTW(f *zgcForeignFrame, roots *logosForeignRoot, n uintptr) bool {
	zgcForeignAdmitScratchDestroy()
	q := &zgcForeignAdmitQueue
	zgcForeignQueueReset(q)
	if !zgcForeignAdmitPendingReserve(n) {
		zgcForeignAdmitScratchDestroy()
		zgcForeignQueueShrink(q)
		return false
	}
	rootSize := unsafe.Sizeof(logosForeignRoot{})
	for i := uintptr(0); i < n; i++ {
		desc := (*logosForeignRoot)(add(unsafe.Pointer(roots), i*rootSize))
		if !zgcForeignAdmitDescriptorSTW(f, q, desc) {
			// Atomicity: discard deferred writes (no partly
			// rewritten descriptors), roll back every refcount,
			// page credit, and chunk entry this call added, and
			// free scratch before the failed Begin returns.
			zgcForeignAdmitPendingFree()
			zgcForeignRollbackSTW(f)
			zgcForeignAdmitScratchDestroy()
			zgcForeignQueueShrink(q)
			return false
		}
	}
	zgcForeignAdmitFrame = f
	zgcForeignAdmitQueueRef = q
	zgcForeignAdmitFailed = false
	for q.head < q.tail && !zgcForeignAdmitFailed {
		base := *(*uintptr)(add(unsafe.Pointer(q.base), q.head*unsafe.Sizeof(uintptr(0))))
		q.head++
		s := spanOfHeap(base)
		if s == nil || s.spanclass.noscan() {
			continue
		}
		// Actual object layout, not a page pass: the collector's
		// own pointer-slot walker enumerates this object's precise
		// slots; first sight in THIS admission (seen set) enqueues,
		// so shared/exposed descendants are fully owned here too.
		// First-exposed-in-place objects rewrite their fields to
		// canonical (C observes them where they sit); everything
		// else expands read-only.
		if zgcForeignSetHas(&zgcForeignAdmitRewrite, base) {
			logosWalkObject(s, base, zgcForeignAdmitVisitRewrite)
		} else {
			logosWalkObject(s, base, zgcForeignAdmitVisit)
		}
	}
	zgcForeignAdmitFrame = nil
	zgcForeignAdmitQueueRef = nil
	if zgcForeignAdmitFailed {
		zgcForeignAdmitPendingFree()
		zgcForeignRollbackSTW(f)
		zgcForeignAdmitScratchDestroy()
		zgcForeignQueueShrink(q)
		return false
	}
	// Commit: canonicalize caller slots only now that the complete
	// closure is proven, then reclaim burst scratch.
	zgcForeignAdmitPendingApply()
	zgcForeignQueueShrink(q)
	zgcForeignAdmitScratchDestroy()
	return true
}

// zgcForeignPublish links a fully admitted frame into the published set.
// Gateless: called once per admission, at the end of the STW holder's
// exclusive section, when no other thread can link, unlink, or iterate.
// Pure pointer ops and a counter; no lock of any rank is acquired.
func zgcForeignPublish(f *zgcForeignFrame) {
	f.published = 1
	f.link = zgcForeignPublished
	zgcForeignPublished = uintptr(unsafe.Pointer(f))
	zgcForeignActive.Add(1)
}

// zgcForeignBeginLocked sets up one frame (g-local spare first, then
// fresh off-heap memory), pushes it on the caller's stack unpublished,
// and runs the STW admission. No gate is taken anywhere on this path:
// worldsema exclusion covers every shared structure touched here, so
// the gate pairs only world-running retire against world-running
// queries. On failure the frame is already recycled before return, so
// the caller holds nothing to unwind.
func zgcForeignBeginLocked(gp *g, roots *logosForeignRoot, n uintptr) logosForeignToken {
	var f *zgcForeignFrame
	if gp.logosForeignSpare != 0 {
		f = (*zgcForeignFrame)(unsafe.Pointer(gp.logosForeignSpare))
		gp.logosForeignSpare = 0
		*f = zgcForeignFrame{}
	} else {
		f = zgcForeignFrameAlloc()
		if f == nil {
			return logosForeignInvalidToken
		}
	}
	f.owner = uintptr(unsafe.Pointer(gp))
	f.gen = zgcForeignNextGen()
	f.parent = gp.logosForeignTop
	gp.logosForeignTop = uintptr(unsafe.Pointer(f))
	gp.logosForeignDepth++
	if !zgcForeignAdmitSTW(f, roots, n) {
		gp.logosForeignTop = f.parent
		gp.logosForeignDepth--
		// Rollback already released every partial entry; free the
		// owned-membership block before recycling the frame so no
		// burst peak is retained or leaked.
		zgcForeignFrameDestroyOwned(f)
		if gp.logosForeignSpare == 0 {
			*f = zgcForeignFrame{}
			gp.logosForeignSpare = uintptr(unsafe.Pointer(f))
		} else {
			sysFree(unsafe.Pointer(f), unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys)
		}
		return logosForeignInvalidToken
	}
	zgcForeignPublish(f)
	return logosForeignToken{Frame: unsafe.Pointer(f), Gen: f.gen}
}

// logosZgcForeignBegin performs the complete preemptible admission for
// one foreign call: off-heap frame, stop-the-world closure, gate-free
// publication. In the configured cycle it ALWAYS returns a valid token,
// even when the initial closure is empty: a legal C call with zero
// Go-pointer inputs may still callback and return a Pinner-pinned
// pointer, and Extend upgrades that same frame. End is always required
// and recycles even an empty frame. The invalid token is returned only
// when the cycle is disabled (identity stable, no ownership possible);
// empty must not be conflated with disabled. Off-heap exhaustion throws
// after unwinding every partial unpublished structure; it never returns
// the invalid token. Descriptors are consumed synchronously;
// Value/Unknown slots are rewritten to canonical copies.
//
// Must be called from a preemptible Go context with a P, before
// entersyscall: it stops the world and allocates off-heap metadata. It
// must never be called from a nosplit region, a signal handler, or
// without a P; the restricted nested path is BeginNoSplit.
//
//go:linkname logosZgcForeignBegin
func logosZgcForeignBegin(roots *logosForeignRoot, n uintptr) logosForeignToken {
	if !zgcCycleEnabled() {
		// Nothing relocates on this configuration: identity is
		// stable, no ownership needed.
		return logosForeignInvalidToken
	}
	gp := getg()
	// Shape contract first, fail-loud on both build shapes: kinds,
	// slots, Value/Size, Unknown Type/Size, Whole element type and Size
	// multiple. Single source of truth (shared helper); admission below
	// enforces the semantic half (coverage, canonicalization,
	// atomicity).
	logosForeignValidateRoots(roots, n)
	// Build every declared type bitmap while the world runs, so the
	// stop below never spins on an on-demand mask build.
	zgcForeignPrebuildMasks(roots, n)
	// No token-free path: even an empty closure publishes a valid,
	// End-paired frame so callbacks can Extend it.
	// Serialized with concurrent admissions (and GC pauses) by
	// worldsema inside stopTheWorld: the STW holder is exclusive, and
	// nothing under STW takes the gate.
	w := stopTheWorld(stwUnknown)
	tok := zgcForeignBeginLocked(gp, roots, n)
	startTheWorld(w)
	if !logosForeignTokenValid(tok) {
		// BeginLocked has already unwound every partial, unpublished
		// structure. An invalid token here can only mean off-heap
		// exhaustion, for empty closures as well as non-empty ones:
		// never hand the caller a token indistinguishable from the
		// disabled sentinel, which would let foreign code run untracked.
		throw("logos zgc: foreign admission exhausted off-heap storage")
	}
	return tok
}

// logosZgcForeignBeginNoSplit admits a nested foreign call from a
// restricted (nosplit, no-P, systemstack) context. It takes no locks,
// allocates at most one off-heap frame (spare first), stops no world,
// and touches only the caller's own frame chain.
//
// It succeeds only when every heap referent in roots is already covered
// by the caller's own active frames (admitted by an enclosing Begin or
// Extend, whose slots were rewritten to canonical copies). An uncovered
// heap referent, a malformed descriptor, or off-heap exhaustion throws:
// the typed caller must have preflighted at a preemptible site, and an
// invalid token returned here would be indistinguishable from a
// legitimate empty closure and let foreign code run untracked. Stale
// duplicate slots holding a pre-copy source address are uncovered and
// throw rather than aliasing. The invalid token means only an empty
// descriptor set (trusted no-callback nesting) or a disabled cycle.
// A configured Begin never returns invalid for an empty closure, so an
// invalid token here never aliases a live frame.
//
// Callers on the systemstack or in ordinary code may also use it for
// pure LIFO nesting when they can prove coverage; otherwise use Begin.
//
//go:nosplit
func logosZgcForeignBeginNoSplit(roots *logosForeignRoot, n uintptr) logosForeignToken {
	gp := getg()
	if !zgcCycleEnabled() {
		// Cycle disabled: nothing relocates, identity stable. This is
		// the same configuration predicate Begin gates on, now with a
		// //go:nosplit contract (logos_zgc_mode.go), so the check is
		// exact even before the first color flip: covered heap roots
		// must not silently skip.
		return logosForeignInvalidToken
	}
	if n != 0 && roots == nil {
		throw("logos zgc: foreign nosplit descriptors missing")
	}
	// An empty descriptor set is token-free, but only for trusted
	// no-callback nesting: with no roots there is nothing to cover and
	// no frame to Extend. Covered heap roots always take the marker
	// path below and never skip silently.
	if n == 0 {
		return logosForeignInvalidToken
	}
	rootSize := unsafe.Sizeof(logosForeignRoot{})
	for i := uintptr(0); i < n; i++ {
		desc := (*logosForeignRoot)(add(unsafe.Pointer(roots), i*rootSize))
		switch desc.Kind {
		case logosForeignRootValue, logosForeignRootUnknown:
			if desc.Slot == nil {
				throw("logos zgc: foreign nosplit descriptor with nil slot")
			}
			if desc.Kind == logosForeignRootUnknown && desc.Type != nil {
				throw("logos zgc: foreign nosplit unknown root with type")
			}
			if desc.Size != 0 {
				throw("logos zgc: foreign nosplit value root with size")
			}
			if desc.Type == nil {
				val := *(*uintptr)(desc.Slot)
				base, ok := zgcForeignHeapBaseNoSplit(val)
				if !ok {
					continue
				}
				if !zgcForeignCoveredBySelf(gp, base) {
					throw("logos zgc: foreign nosplit admission of an uncovered heap referent")
				}
				continue
			}
			typ := (*_type)(desc.Type)
			slot := uintptr(desc.Slot)
			if typ.Size_ > ^uintptr(0)-slot {
				throw("logos zgc: foreign nosplit value extent overflows the address space")
			}
			limit := slot + typ.Size_
			tp := zgcForeignTypeCellsNoSplit(typ, slot)
			for {
				var cell uintptr
				tp, cell = tp.next(limit)
				if cell == 0 {
					break
				}
				base, ok := zgcForeignHeapBaseNoSplit(*(*uintptr)(unsafe.Pointer(cell)))
				if !ok {
					continue
				}
				if !zgcForeignCoveredBySelf(gp, base) {
					throw("logos zgc: foreign nosplit admission of an uncovered heap referent")
				}
			}
		case logosForeignRootWhole:
			if desc.Size == 0 {
				continue
			}
			if desc.Slot == nil {
				throw("logos zgc: foreign nosplit whole descriptor with nil slot")
			}
			typ := (*_type)(desc.Type)
			if typ == nil || typ.Size_ == 0 || desc.Size%typ.Size_ != 0 {
				throw("logos zgc: foreign nosplit whole root with bad element type")
			}
			base := uintptr(desc.Slot)
			if desc.Size > ^uintptr(0)-base {
				throw("logos zgc: foreign nosplit whole extent overflows the address space")
			}
			end := base + desc.Size
			if typ.PtrBytes != 0 && base&(unsafe.Sizeof(uintptr(0))-1) != 0 {
				throw("logos zgc: foreign nosplit whole extent misaligned")
			}
			tp := zgcForeignTypeCellsNoSplit(typ, base)
			for {
				var cell uintptr
				tp, cell = tp.next(end)
				if cell == 0 {
					break
				}
				cbase, ok := zgcForeignHeapBaseNoSplit(*(*uintptr)(unsafe.Pointer(cell)))
				if !ok {
					continue
				}
				if !zgcForeignCoveredBySelf(gp, cbase) {
					throw("logos zgc: foreign nosplit admission of an uncovered heap referent")
				}
			}
		default:
			throw("logos zgc: foreign nosplit descriptor with unknown kind")
		}
	}
	var f *zgcForeignFrame
	if gp.logosForeignSpare != 0 {
		f = (*zgcForeignFrame)(unsafe.Pointer(gp.logosForeignSpare))
		gp.logosForeignSpare = 0
		*f = zgcForeignFrame{}
	} else {
		raw := sysAlloc(unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys, "logos zgc foreign frame")
		if raw == nil {
			throw("logos zgc: foreign nosplit marker exhausted off-heap storage")
		}
		f = (*zgcForeignFrame)(raw)
		*f = zgcForeignFrame{}
	}
	f.owner = uintptr(unsafe.Pointer(gp))
	f.gen = zgcForeignNextGen()
	f.parent = gp.logosForeignTop
	// Nested marker: no objects of its own, unpublished, invisible to
	// MarkRoots and the exposure tables. Lifetime pairs with End.
	gp.logosForeignTop = uintptr(unsafe.Pointer(f))
	gp.logosForeignDepth++
	return logosForeignToken{Frame: unsafe.Pointer(f), Gen: f.gen}
}

// zgcForeignCoveredBySelf reports whether base was admitted by one of
// gp's own active frames. G-local chain walk with one hash lookup per
// frame (O(depth), never a chunk scan): the frame owned-set records
// every base the frame admitted. No locks: only gp mutates its chain
// and sets, and a running goroutine implies no STW admission is
// mutating them. Nosplit-safe: SetHas is plain reads plus arithmetic.
// The walk is bounded by the frame depth: a chain longer than the
// pushes that built it is corrupt and fails closed.
//
//go:nosplit
func zgcForeignCoveredBySelf(gp *g, base uintptr) bool {
	steps := gp.logosForeignDepth
	for top := gp.logosForeignTop; top != 0; {
		f := (*zgcForeignFrame)(unsafe.Pointer(top))
		if zgcForeignSetHas(&f.owned, base) {
			return true
		}
		top = f.parent
		if steps == 0 {
			return false
		}
		steps--
	}
	return false
}

// zgcForeignRetireLocked removes one published frame's ownership: unlink
// from the published list and release every object refcount and page
// pin. Gate-held.
func zgcForeignRetireLocked(f *zgcForeignFrame) {
	prev := uintptr(0)
	for cur := zgcForeignPublished; cur != 0; {
		cf := (*zgcForeignFrame)(unsafe.Pointer(cur))
		next := cf.link
		if cur == uintptr(unsafe.Pointer(f)) {
			if prev == 0 {
				zgcForeignPublished = next
			} else {
				(*zgcForeignFrame)(unsafe.Pointer(prev)).link = next
			}
			break
		}
		prev = cur
		cur = next
	}
	zgcForeignActive.Add(-1)
	for c := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks)); c != nil; c = (*zgcForeignObjChunk)(unsafe.Pointer(c.next)) {
		for i := uint32(0); i < c.n; i++ {
			base := c.addrs[i]
			zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(base))
			zgcForeignSetRelease(&zgcForeignObjects, base)
		}
	}
}

// logosZgcForeignEnd releases exactly the invocation named by t: owner,
// generation, and LIFO position are all validated. Invalid tokens are a
// no-op (disabled-mode sentinel, or trusted-empty NoSplit markers, pair
// with nothing). Published frames retire under the gate (unlink plus
// refcount release only); chunk destruction, the g-local pop, and the
// spare stash are gate-free past unlock.
// Markers (unpublished, object-free) never take the gate.
//
//go:linkname logosZgcForeignEnd
func logosZgcForeignEnd(t logosForeignToken) {
	if !logosForeignTokenValid(t) {
		return
	}
	gp := getg()
	f := (*zgcForeignFrame)(t.Frame)
	if gp.logosForeignTop != uintptr(unsafe.Pointer(f)) {
		throw("logos zgc: foreign End without matching Begin (LIFO violation)")
	}
	// Two independent checks share this throw. An owner mismatch
	// means the frame belongs to another g (or was recycled); it says
	// nothing about generations.
	//
	// The generation check is separate: f.gen is write-once, set by
	// the Begin path that created this frame (zgcForeignBeginLocked,
	// and logosZgcForeignBeginNoSplit for unpublished markers), and
	// the token carries that same word, so for a live frame in LIFO
	// position the two must agree. End and Extend never write it.
	// So when f.gen != t.Gen, the token's Gen word changed after
	// Begin returned while the frame's did not: the producer stored
	// the token where something else could rewrite it. Under vfork
	// that is any slot of the child-shared frame; keep the token in
	// the caller's frame (see syscall.forkAndExecInChild1).
	if f.owner != uintptr(unsafe.Pointer(gp)) || f.gen != t.Gen {
		println("logos zgc: foreign End mismatch: gp=", unsafe.Pointer(gp), " top=", gp.logosForeignTop, " depth=", gp.logosForeignDepth, " spare=", gp.logosForeignSpare, " f=", unsafe.Pointer(f), " owner=", f.owner, " fgen=", f.gen, " tgen=", t.Gen)
		throw("logos zgc: foreign End owner/generation mismatch")
	}
	if f.published == 0 {
		if f.nObjs != 0 || f.chunks != 0 {
			throw("logos zgc: foreign End of unpublished frame holding objects")
		}
		gp.logosForeignTop = f.parent
		gp.logosForeignDepth--
		// Markers never own (no objects, no owned inserts); destroy
		// is a no-op that keeps every disposal path uniform.
		zgcForeignFrameDestroyOwned(f)
		if gp.logosForeignSpare == 0 {
			*f = zgcForeignFrame{}
			gp.logosForeignSpare = uintptr(unsafe.Pointer(f))
			return
		}
		sysFree(unsafe.Pointer(f), unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys)
		return
	}
	// Retire under the gate: unlink plus refcount release is the only
	// world-running mutation shared with locking queries, and the
	// cycle pause itself consults the locking query form. Gate sections
	// are short and never block: pure table ops and sysAlloc/sysFree,
	// the same leaf-lock discipline as the quarantine and page locks
	// the pauses already share with world-running paths. Everything
	// after unlock is g-local (pop, spare) or sync-free (sysFree).
	zgcForeignGateLock()
	zgcForeignRetireLocked(f)
	zgcForeignGateUnlock()
	zgcForeignFrameDestroyChunks(f)
	// Chunks are gone (refcounts released under the gate); free the
	// frame-private owned-membership block past unlock, sync-free.
	zgcForeignFrameDestroyOwned(f)
	gp.logosForeignTop = f.parent
	gp.logosForeignDepth--
	if gp.logosForeignSpare == 0 && gp.logosForeignTop == 0 {
		*f = zgcForeignFrame{}
		gp.logosForeignSpare = uintptr(unsafe.Pointer(f))
		return
	}
	sysFree(unsafe.Pointer(f), unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys)
}

// logosZgcForeignEndNoSplit pairs BeginNoSplit markers inside restricted
// contexts. It takes no locks and stops no world: the marker must be
// unpublished (use End for published frames), so teardown is g-local
// plus at most one sync-free sysFree. Throws on any ownership, LIFO, or
// published-frame violation.
//
//go:nosplit
func logosZgcForeignEndNoSplit(t logosForeignToken) {
	if !logosForeignTokenValid(t) {
		return
	}
	gp := getg()
	f := (*zgcForeignFrame)(t.Frame)
	if gp.logosForeignTop != uintptr(unsafe.Pointer(f)) {
		throw("logos zgc: foreign End without matching Begin (LIFO violation)")
	}
	if f.owner != uintptr(unsafe.Pointer(gp)) || f.gen != t.Gen {
		println("logos zgc: foreign EndNoSplit mismatch: gp=", unsafe.Pointer(gp), " top=", gp.logosForeignTop, " depth=", gp.logosForeignDepth, " spare=", gp.logosForeignSpare, " f=", unsafe.Pointer(f), " owner=", f.owner, " fgen=", f.gen, " tgen=", t.Gen)
		throw("logos zgc: foreign End owner/generation mismatch")
	}
	if f.published != 0 || f.nObjs != 0 || f.chunks != 0 {
		throw("logos zgc: foreign NoSplit End of publishing frame")
	}
	gp.logosForeignTop = f.parent
	gp.logosForeignDepth--
	// Markers never own; destroy is a uniform no-op.
	zgcForeignFrameDestroyOwned(f)
	if gp.logosForeignSpare == 0 {
		*f = zgcForeignFrame{}
		gp.logosForeignSpare = uintptr(unsafe.Pointer(f))
		return
	}
	sysFree(unsafe.Pointer(f), unsafe.Sizeof(zgcForeignFrame{}), &memstats.other_sys)
}

// logosZgcForeignExtend admits additional roots into an already active
// outer invocation (Go->C->Go callbacks): the new closure joins the
// outer frame's lifetime and survives callback return until the outer C
// call ends. The invalid outer token is a no-op (disabled-mode sentinel:
// nothing relocates, so there is no closure to extend and End pairs with
// nothing). A valid outer frame must be owned by the caller, published,
// and present in the caller's chain; it need not be the top (a nested
// call may be active). Runs the same STW admission as Begin against the
// outer frame. Off-heap exhaustion throws: entry already happened, so
// failing closed means failing loud.
func logosZgcForeignExtend(outer logosForeignToken, roots *logosForeignRoot, n uintptr) {
	if !logosForeignTokenValid(outer) {
		return
	}
	gp := getg()
	f := (*zgcForeignFrame)(outer.Frame)
	if f.owner != uintptr(unsafe.Pointer(gp)) || f.gen != outer.Gen {
		throw("logos zgc: foreign Extend owner/generation mismatch")
	}
	if f.published != 1 {
		throw("logos zgc: foreign Extend of unpublished frame")
	}
	found := false
	for top := gp.logosForeignTop; top != 0; {
		if top == uintptr(unsafe.Pointer(f)) {
			found = true
			break
		}
		top = (*zgcForeignFrame)(unsafe.Pointer(top)).parent
	}
	if !found {
		throw("logos zgc: foreign Extend of inactive outer frame")
	}
	if n == 0 {
		return
	}
	if roots == nil {
		throw("logos zgc: nil foreign Extend roots")
	}
	if !zgcCycleEnabled() {
		return
	}
	// Same shape contract as Begin: fail-loud before stopping the
	// world, so a malformed callback descriptor never reaches the
	// transactional core.
	logosForeignValidateRoots(roots, n)
	// Build every declared type bitmap while the world runs (see Begin).
	zgcForeignPrebuildMasks(roots, n)
	// Same worldsema serialization as Begin: callbacks extending
	// concurrently simply wait their turn. Gateless: the world is
	// stopped, so the frame chain and global sets are exclusive.
	w := stopTheWorld(stwUnknown)
	// Extend reuses the admission core with the outer frame as target:
	// new bases append to its chunks and refcounts. Only entries
	// appended in this call roll back on failure; pre-existing entries
	// stay published.
	ok := zgcForeignExtendSTW(f, roots, n)
	startTheWorld(w)
	if !ok {
		throw("logos zgc: foreign Extend admission exhausted off-heap storage")
	}
}

// zgcForeignExtendSTW admits roots into an already-published frame.
// Only entries appended in this call may be rolled back; pre-existing
// entries stay published even on failure. The per-admission visited set
// starts empty for every Extend: re-admitting an already-owned base
// takes no new ownership record but re-queues a rescan of its current
// fields, so callback-added descendants are discovered rather than
// skipped. Descriptor slots rewrite only at commit; on
// failure deferred writes are discarded, so callback descriptors are
// never partly canonicalized. STW-paired and gateless.
func zgcForeignExtendSTW(f *zgcForeignFrame, roots *logosForeignRoot, n uintptr) bool {
	// Mark the append frontier: chunks pushed beyond this point, and
	// the count within the frontier chunk, belong to this call.
	frontChunk := f.chunks
	var frontN uint32
	if frontChunk != 0 {
		frontN = (*zgcForeignObjChunk)(unsafe.Pointer(frontChunk)).n
	}
	frontObjs := f.nObjs
	zgcForeignAdmitScratchDestroy()
	q := &zgcForeignAdmitQueue
	zgcForeignQueueReset(q)
	if !zgcForeignAdmitPendingReserve(n) {
		zgcForeignAdmitPendingFree()
		zgcForeignAdmitScratchDestroy()
		zgcForeignQueueShrink(q)
		return false
	}
	rootSize := unsafe.Sizeof(logosForeignRoot{})
	zgcForeignAdmitFrame = f
	zgcForeignAdmitQueueRef = q
	zgcForeignAdmitFailed = false
	for i := uintptr(0); i < n && !zgcForeignAdmitFailed; i++ {
		desc := (*logosForeignRoot)(add(unsafe.Pointer(roots), i*rootSize))
		if !zgcForeignAdmitDescriptorSTW(f, q, desc) {
			zgcForeignAdmitFailed = true
		}
	}
	for q.head < q.tail && !zgcForeignAdmitFailed {
		base := *(*uintptr)(add(unsafe.Pointer(q.base), q.head*unsafe.Sizeof(uintptr(0))))
		q.head++
		s := spanOfHeap(base)
		if s == nil || s.spanclass.noscan() {
			continue
		}
		if zgcForeignSetHas(&zgcForeignAdmitRewrite, base) {
			logosWalkObject(s, base, zgcForeignAdmitVisitRewrite)
		} else {
			logosWalkObject(s, base, zgcForeignAdmitVisit)
		}
	}
	zgcForeignAdmitFrame = nil
	zgcForeignAdmitQueueRef = nil
	if zgcForeignAdmitFailed {
		zgcForeignAdmitPendingFree()
		zgcForeignExtendRollbackSTW(f, frontChunk, frontN, frontObjs)
		zgcForeignAdmitScratchDestroy()
		zgcForeignQueueShrink(q)
		return false
	}
	zgcForeignAdmitPendingApply()
	zgcForeignQueueShrink(q)
	zgcForeignAdmitScratchDestroy()
	return true
}

// zgcForeignExtendRollbackSTW drops only the entries appended after the
// recorded frontier, releasing their refcounts. Appended chunks are
// unlinked and freed straight to the OS (frame-private, sync-free); the
// pre-existing frontier chunk is truncated in place. STW-paired and
// gateless.
func zgcForeignExtendRollbackSTW(f *zgcForeignFrame, frontChunk uintptr, frontN uint32, frontObjs uint64) {
	for f.chunks != frontChunk {
		c := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks))
		for i := uint32(0); i < c.n; i++ {
			base := c.addrs[i]
			zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(base))
			zgcForeignSetRelease(&zgcForeignObjects, base)
			if zgcForeignSetHas(&f.owned, base) {
				zgcForeignSetRelease(&f.owned, base)
			}
			f.nObjs--
		}
		f.chunks = c.next
		zgcForeignChunkDestroy(c)
	}
	if frontChunk != 0 {
		c := (*zgcForeignObjChunk)(unsafe.Pointer(frontChunk))
		for c.n > frontN {
			c.n--
			base := c.addrs[c.n]
			zgcForeignSetRelease(&zgcForeignPages, zgcForeignRegionKey(base))
			zgcForeignSetRelease(&zgcForeignObjects, base)
			if zgcForeignSetHas(&f.owned, base) {
				zgcForeignSetRelease(&f.owned, base)
			}
			f.nObjs--
		}
	}
	if f.nObjs != frontObjs {
		throw("logos zgc: foreign Extend rollback imbalance")
	}
}

// ---- scheduler transport bookkeeping ----

// zgcForeignTransportEnter records a scheduler foreign transition
// (reentersyscall and friends) without touching frame ownership.
// Nosplit-safe: one g-local counter.
//
//go:nosplit
func zgcForeignTransportEnter(gp *g) {
	if gp == nil {
		return
	}
	gp.logosForeignTrans++
}

// zgcForeignTransportExit pairs TransportEnter on every exitsyscall
// path. It never releases frames: callbacks and blocking syscalls
// inside them cannot pop the outer invocation.
//
//go:nosplit
func zgcForeignTransportExit(gp *g) {
	if gp == nil {
		return
	}
	if gp.logosForeignTrans > 0 {
		gp.logosForeignTrans--
	}
}

// ---- exposure queries ----

// zgcForeignQueryKey maps an arbitrary queried address, possibly colored
// or foreign, to its pin key. It strips only the active color bits and
// rejects any address above the 4 TB heap range, exactly as zgcCanonical
// does: masking with zgcAddrMask instead would alias a C, stack, or mmap
// address onto a heap region that shares its low 42 bits. It does not
// follow forwarding: the key names an address region, not an object.
func zgcForeignQueryKey(addr uintptr) (uintptr, bool) {
	a := addr &^ atomic.Loaduintptr(&zgcColorMaskBits)
	if a == 0 || a > zgcAddrMask {
		return 0, false
	}
	return zgcForeignRegionKey(a), true
}

// zgcForeignPagePinnedSTW reports whether addr names memory on a page
// pinned by published foreign exposure, with the world stopped. No lock
// is taken: worldsema exclusion covers the tables, and a stopped world
// cannot retire. Pause code may use this lock-free form; the locking
// form is equally exact and cannot deadlock a pause. Asserts the world
// stop only when static lock ranking is built in.
//
// No allocation.
func zgcForeignPagePinnedSTW(addr uintptr) bool {
	assertWorldStopped()
	if zgcForeignActive.Load() == 0 {
		return false
	}
	key, ok := zgcForeignQueryKey(addr)
	return ok && zgcForeignSetHas(&zgcForeignPages, key)
}

// zgcForeignPagePinnedLocked reports whether addr names memory on a
// page pinned by published foreign exposure. Exact page-membership
// check for selector, copy-eligibility, and release paths. The caller
// holds the gate (world running) or the world stopped. No allocation.
//
// The check is distinct from object exposure: a whole page stays put
// while any of its objects is exposed, but fixup suppression needs the
// exact object set, not the page.
func zgcForeignPagePinnedLocked(addr uintptr) bool {
	if zgcForeignActive.Load() == 0 {
		return false
	}
	key, ok := zgcForeignQueryKey(addr)
	return ok && zgcForeignSetHas(&zgcForeignPages, key)
}

// zgcForeignPagePinned is the locking form of the page query.
func zgcForeignPagePinned(addr uintptr) bool {
	if zgcForeignActive.Load() == 0 {
		return false
	}
	zgcForeignGateLock()
	ok := zgcForeignPagePinnedLocked(addr)
	zgcForeignGateUnlock()
	return ok
}

// zgcForeignObjectExposedLocked reports whether addr lies in a canonical
// object base currently exposed by published frames. Exact object
// membership for collector fixup suppression: skipping an entire page's
// fixups would leave unrelated objects' stale references unhealed, so
// the page check is only a fast prefilter and this exact check decides.
// Pass the slot address (or any interior pointer); membership is on the
// containing base. No forwarding resolution: recorded bases are exactly
// the admission-canonical addresses that exposed slots hold (rewrites
// happen only at admission, and exposed slots are never healed), so a
// Resolution uses zgcCanonical (color strip plus forwarding to the
// current copy), and membership is on the resolved base. This is exact
// under the exposure contract: admission canonicalizes slots to the
// copy, and exposed objects are pinned against further relocation, so
// the canonical base is stable; stale alias slots holding a source
// address still resolve to the recorded copy and hit. The caller holds
// the gate (world running) or the world stopped. No allocation and no
// borrowed record lifetime past return.
func zgcForeignObjectExposedLocked(addr uintptr) bool {
	if zgcForeignActive.Load() == 0 {
		return false
	}
	base, _, _, ok := zgcForeignHeapBase(addr)
	if !ok {
		return false
	}
	return zgcForeignSetHas(&zgcForeignObjects, base)
}

// zgcForeignObjectExposed is the locking form of the exact query.
func zgcForeignObjectExposed(addr uintptr) bool {
	if zgcForeignActive.Load() == 0 {
		return false
	}
	zgcForeignGateLock()
	ok := zgcForeignObjectExposedLocked(addr)
	zgcForeignGateUnlock()
	return ok
}

// zgcForeignMarkRoots shades every canonical object retained by
// published frames, including callback additions. Called at mark start
// with the world stopped; worldsema exclusion (not the gate) covers the
// published list and chunks, which are immutable post-publish and cannot
// be retired while the world is stopped. gcw is the caller's mark work
// buffer; greying enqueues objects for scanning so edges stored before
// the scan are discovered. Frames retired before this runs are already
// unlinked and contribute nothing. No gate is taken so greyobject's own
// lock order stays undisturbed (the gate is a leaf and must be last).
func zgcForeignMarkRoots(gcw *gcWork) {
	if zgcForeignActive.Load() == 0 || gcw == nil {
		return
	}
	for cur := zgcForeignPublished; cur != 0; cur = (*zgcForeignFrame)(unsafe.Pointer(cur)).link {
		f := (*zgcForeignFrame)(unsafe.Pointer(cur))
		if f.published != 1 {
			continue
		}
		for c := (*zgcForeignObjChunk)(unsafe.Pointer(f.chunks)); c != nil; c = (*zgcForeignObjChunk)(unsafe.Pointer(c.next)) {
			for i := uint32(0); i < c.n; i++ {
				base := c.addrs[i]
				s := spanOfHeap(base)
				if s == nil {
					continue
				}
				idx := uintptr(0)
				if s.spanclass.sizeclass() != 0 {
					idx = s.objIndex(base)
					if idx >= uintptr(s.nelems) {
						continue
					}
				}
				greyobject(base, 0, 0, s, gcw, idx)
			}
		}
	}
}
