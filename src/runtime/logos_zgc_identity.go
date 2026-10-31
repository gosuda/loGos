// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

const (
	zgcIdentTomb    = 1
	zgcIdentMinCap  = 64
	zgcIdentIDMul   = 0x9E3779B1
	zgcIdentHashMul = 0x9E3779B97F4A7C15
)

type zgcIdentEntry struct {
	_    sys.NotInHeap
	addr uintptr
	id   uint32
}

// zgcIdentityTable maps an object address to its 32-bit identity hash. The
// table is off-heap and leaf-locked. Pointer-keyed maps hash the identity
// instead of the address, so a relocated key keeps its hash once the
// collector moves its entry with zgcIdentityMove.
type zgcIdentityTable struct {
	lock    mutex
	entries unsafe.Pointer
	mask    uintptr
	shift   uint8
	live    int
	used    int
	counter uint32
}

var zgcIdentity zgcIdentityTable

// init registers the static rank for the identity table lock once before
// its first acquisition. The lock is held across the shared admission
// gate (leaf) in zgcRelocateObject, so it must be a real rank while the
// gate stays the true final leaf. lockInit is a no-op without
// staticlockranking and allocates nothing. It runs in
// doInit(runtime_inittasks) before gcenable, user package inits, sweep,
// and cycles: bootstrap maps before gcinit use no pointer-keyed identity
// hashes (modulesinit appends, itabsinit under itabLock), so no identity
// acquisition precedes it. Single execution, never re-mutated.
func init() {
	lockInit(&zgcIdentity.lock, lockRankZgcIdentity)
}

func (t *zgcIdentityTable) slot(i uintptr) *zgcIdentEntry {
	return (*zgcIdentEntry)(add(t.entries, i*unsafe.Sizeof(zgcIdentEntry{})))
}

func (t *zgcIdentityTable) home(addr uintptr) uintptr {
	return uintptr(uint64(addr>>3) * zgcIdentHashMul >> t.shift)
}

// find returns the entry of addr, or nil. Callers hold t.lock.
func (t *zgcIdentityTable) find(addr uintptr) *zgcIdentEntry {
	if t.entries == nil {
		return nil
	}
	i := t.home(addr)
	for range t.mask + 1 {
		e := t.slot(i)
		if e.addr == 0 {
			return nil
		}
		if e.addr == addr {
			return e
		}
		i = (i + 1) & t.mask
	}
	return nil
}

// resize rebuilds the table for at least four slots per live entry, which
// also drops tombstones. Callers hold t.lock.
func (t *zgcIdentityTable) resize() {
	slots := uintptr(zgcIdentMinCap)
	for slots < uintptr(t.live+1)*4 {
		slots <<= 1
	}
	n := alignUp(slots*unsafe.Sizeof(zgcIdentEntry{}), physPageSize)
	raw := sysAlloc(n, &memstats.other_sys, "logos zgc identity table")
	if raw == nil {
		throw("zgc: out of memory growing the identity table")
	}
	old := *t
	t.entries, t.mask = raw, slots-1
	t.shift = 64 - uint8(sys.TrailingZeros64(uint64(slots)))
	t.used = 0
	if old.entries != nil {
		t.rehash(&old)
	}
}

// rehash moves the live entries of old into t and frees old's storage.
func (t *zgcIdentityTable) rehash(old *zgcIdentityTable) {
	for i := range old.mask + 1 {
		if e := old.slot(i); e.addr > zgcIdentTomb {
			t.place(e.addr, e.id)
		}
	}
	sysFree(old.entries, alignUp((old.mask+1)*unsafe.Sizeof(zgcIdentEntry{}), physPageSize), &memstats.other_sys)
}

// place stores an entry in the first free slot of its probe sequence.
func (t *zgcIdentityTable) place(addr uintptr, id uint32) *zgcIdentEntry {
	i := t.home(addr)
	for t.slot(i).addr > zgcIdentTomb {
		i = (i + 1) & t.mask
	}
	e := t.slot(i)
	if e.addr == 0 {
		t.used++
	}
	e.addr, e.id = addr, id
	return e
}

func (t *zgcIdentityTable) insert(addr uintptr, id uint32) *zgcIdentEntry {
	if t.entries == nil || (t.used+1)*2 > int(t.mask+1) {
		t.resize()
	}
	t.live++
	return t.place(addr, id)
}

func (t *zgcIdentityTable) remove(e *zgcIdentEntry) {
	e.addr = zgcIdentTomb
	t.live--
}

// nextID returns a nonzero id. The odd multiplier makes ids unique within
// each 2^32-assignment window; a collision after that only costs a hash
// collision.
func (t *zgcIdentityTable) nextID() uint32 {
	t.counter++
	if t.counter == 0 {
		t.counter++
	}
	return t.counter * zgcIdentIDMul
}

// zgcIdentityHash returns the identity hash of the object at p, assigning
// one on first use.
func zgcIdentityHash(p unsafe.Pointer) uint32 {
	t := &zgcIdentity
	lock(&t.lock)
	id := t.idLocked(uintptr(p))
	unlock(&t.lock)
	return id
}

// idLocked returns the identity of the object starting at addr, assigning
// one on first use. Callers hold t.lock. addr is first resolved through
// the forwarding table: relocation publishes under t.lock
// (zgcRelocateObject), so a stale source address can never mint a second
// identity for an object whose identity already moved to its copy.
func (t *zgcIdentityTable) idLocked(addr uintptr) uint32 {
	pg := zgcPageAcquire(addr)
	if pg != nil {
		if to, ok := pg.forward(addr); ok {
			addr = to
		}
		zgcPageRelease(pg)
	}
	e := t.find(addr)
	if e == nil {
		e = t.insert(addr, t.nextID())
		zgcIdentityInUse.Store(true)
	}
	return e.id
}

// zgcIdentityInUse turns true at the first identity assigned. Until then
// the sweeper has no identity to forget for freed objects.
var zgcIdentityInUse atomic.Bool

// zgcIdentityTracking reports whether freed objects may hold identities.
func zgcIdentityTracking() bool {
	return zgcIdentityInUse.Load()
}

// zgcIdentitySweepFree drops the identity of the freed object at x, so
// a later object at the reused address starts with a fresh identity and
// the table does not grow with dead objects.
func zgcIdentitySweepFree(x uintptr) {
	t := &zgcIdentity
	lock(&t.lock)
	if e := t.find(x); e != nil {
		t.remove(e)
	}
	unlock(&t.lock)
}

// moveLocked carries the identity of the object at from to to. Callers
// hold t.lock. zgcRelocateObject calls it in the same critical section
// that publishes the forwarding entry, so no hasher sees the entry
// published while the identity still sits at from (idLocked).
func (t *zgcIdentityTable) moveLocked(from, to uintptr) {
	if e := t.find(from); e != nil {
		id := e.id
		t.remove(e)
		if dst := t.find(to); dst != nil {
			dst.id = id
		} else {
			t.insert(to, id)
		}
	}
}

// reserveMoveLocked ensures a subsequent moveLockedNoGrow for from->to
// cannot resize. Callers hold t.lock outside the foreign gate (identity
// first, gate last); it may sysAlloc/sysFree via resize in this
// reservation phase, never under the gate. Fail-closed via resize throw
// on off-heap exhaustion; no fixed cap, no identity loss.
func (t *zgcIdentityTable) reserveMoveLocked(from, to uintptr) {
	if t.entries == nil {
		return
	}
	if e := t.find(from); e == nil {
		return
	} else if dst := t.find(to); dst != nil {
		return
	}
	if (t.used+1)*2 > int(t.mask+1) {
		t.resize()
	}
}

// moveLockedNoGrow is the gate-held half of moveLocked: identical
// semantics, but never resizes. It carries one identity from source to
// copy without touching mark or alloc bits, so the quarantine
// marks-subset-allocs invariant is unaffected. Capacity must be pre-reserved by
// reserveMoveLocked under the same identity lock hold before the gate.
// Violation fails closed; no fallback identity loss.
func (t *zgcIdentityTable) moveLockedNoGrow(from, to uintptr) {
	if e := t.find(from); e != nil {
		id := e.id
		t.remove(e)
		if dst := t.find(to); dst != nil {
			dst.id = id
		} else {
			if t.entries == nil || (t.used+1)*2 > int(t.mask+1) {
				throw("zgc: identity table grew under gate")
			}
			t.live++
			t.place(to, id)
		}
	}
}

// zgcIdentityMove carries the identity hash of the object that moved from
// from to to. An object that never had an identity needs no entry.
func zgcIdentityMove(from, to unsafe.Pointer) {
	t := &zgcIdentity
	lock(&t.lock)
	t.moveLocked(uintptr(from), uintptr(to))
	unlock(&t.lock)
}

// zgcIdentityForget drops the identity hash of a dead object.
func zgcIdentityForget(p unsafe.Pointer) {
	t := &zgcIdentity
	lock(&t.lock)
	if e := t.find(uintptr(p)); e != nil {
		t.remove(e)
	}
	unlock(&t.lock)
}

// zgcIdentityLen returns the number of live identity entries.
func zgcIdentityLen() int {
	t := &zgcIdentity
	lock(&t.lock)
	n := t.live
	unlock(&t.lock)
	return n
}

// zgcPointerHash is the hash a pointer-keyed map takes for the object
// starting at p under zgc. It depends on the identity, never on the
// address.
func zgcPointerHash(p unsafe.Pointer, seed uintptr) uintptr {
	return zgcIdentityMix(zgcIdentityHash(p), 0, seed)
}

func zgcIdentityMix(id uint32, off, seed uintptr) uintptr {
	h := (uint64(id) ^ uint64(off)<<32 ^ uint64(seed)) * zgcIdentHashMul
	return uintptr(h ^ h>>32)
}

// zgcKeyHash is the hash of the pointer word v as (part of) a map key
// under zgc. A word into a heap span hashes the identity of its object
// and its offset into it, so a key stays put when the collector moves
// the object, whether it points at the start or inside. Any other word
// (nil, globals, read-only data, C or mmap memory) never moves and hashes
// its address. v may come straight from memory the runtime reads without
// a barrier, so a colored or stale word is first resolved to its current
// canonical address; the slot itself is not healed.
func zgcKeyHash(v, seed uintptr) uintptr {
	addr := zgcCanonical(v)
	s := spanOfHeap(addr)
	if s == nil {
		return memhash(noescape(unsafe.Pointer(&addr)), seed, goarch.PtrSize)
	}
	base := s.base() + s.objIndex(addr)*s.elemsize
	// idLocked re-resolves base under the lock: the object may have been
	// published to a new address since zgcCanonical looked. The offset
	// is the same in the copy.
	return zgcIdentityMix(zgcIdentityHash(add(nil, base)), addr-base, seed)
}

// zgcCanonical returns the current canonical address of the pointer word
// v, or v itself when v is no colored zgc heap word.
func zgcCanonical(v uintptr) uintptr {
	colors := atomic.Loaduintptr(&zgcColorMaskBits)
	if colors == 0 {
		// Nothing relocates before zgc is configured.
		return v
	}
	// Validate before truncating: the word is strippable only when every
	// bit above the address field lies in the non-address region (flag
	// bits or the colour nibble), so a foreign high word returns
	// bit-identical instead of folding into the heap range. Only a word
	// accepted here is canonicalized below, and the canonical result
	// strips the whole non-address field.
	if v&^(zgcAddrMask|zgcFlagMask|zgcColorMask) != 0 {
		return v
	}
	addr := v & zgcAddrMask
	if addr != v {
		// Colored or flagged candidate: accept only when the stripped
		// word names heap. Before the arena map is live no word can name
		// heap, and the lookup would fault on the nil map, so keep the
		// word unchanged.
		if !zgcPageRegisteredNoSplit(addr) && (!zgcHeapArenaLive() || spanOfHeap(addr) == nil) {
			return v
		}
	} else if !zgcHeapArenaLive() || spanOfHeap(v) == nil {
		return v
	}
	// Validated heap word: keep the address field alone, so every color,
	// flag, and tag bit above it is gone in both encodings.
	if zgcPageRegisteredNoSplit(addr) {
		return zgcRemap(addr)
	}
	return addr
}

// zgcptrhash is the hasher of a pointer-sized key word at p. The compiler
// calls it for every pointer word of a key under -lb
// (cmd/compile/internal/reflectdata.hashSignature), and typehash calls it
// when zgc owns the heap.
func zgcptrhash(p unsafe.Pointer, h uintptr) uintptr {
	return zgcKeyHash(*(*uintptr)(p), h)
}

// zgcptrequal reports whether the pointer words at p and q name the same
// location now. The compiler calls it for every pointer word of a type's
// equality function under -lb, because map and interface code in the
// runtime reads stored words without a barrier: a stored source address
// of a relocated object equals the copy's address it now forwards to.
func zgcptrequal(p, q unsafe.Pointer) bool {
	x, y := *(*uintptr)(p), *(*uintptr)(q)
	return x == y || zgcCanonical(x) == zgcCanonical(y)
}

// zgcIdentityForced makes typehash hash pointer words by identity without
// -X runtime.logosGCMode=zgc. Tests set it with no map alive across the
// change.
var zgcIdentityForced bool

// zgcIdentityKeys reports whether typehash must hash the pointer words of
// a value of type t by identity: t holds pointers and zgc owns the heap.
func zgcIdentityKeys(t *_type) bool {
	return t.Pointers() && (zgcIdentityForced || logosGCMode == "zgc")
}
