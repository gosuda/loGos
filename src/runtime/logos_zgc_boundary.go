// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Go-side syscall, cgo, and string boundaries (W2).
//
// Every stub taking a pointer holds the memory by reference while the
// thread traps or blocks, so the kernel and C must never see a colored
// word. Untagging happens at the Go caller, never in asm: the untag is
// pure arithmetic, nosplit-safe, and allocation-free. Foreign lifetimes
// are bounded by an entry-time hold publication protocol (zgcForeignHold)
// synchronized with gcStart's release gate; the pause pass expands the
// transitive closure across conservative frames. Until the Z2-asm
// wrappers land, the remaining assembly stub rows stay an audited
// residual.
//
// Untag validation mirrors the tree-standard canonical pattern
// (zgcCanonical, logosZgcPinnerCanonicalize, zgcForeignHeapBaseNoSplit):
// validate first that no bit sits above the non-address field (flag
// bits and the colour nibble), without applying the full address mask,
// so a high C, stack, or mmap word keeps its bits instead of truncating
// into the heap range and aliasing a live span; then, only for a word
// accepted as heap (a registered relocation source or any heap span),
// return the pure address field, which clears every color, flag, and
// tag bit above it in both encodings. Colored words into ordinary
// non-relocating heap therefore still canonicalize; only genuinely
// foreign words pass through untouched.

// zgcPageRegisteredNoSplit is the nosplit subset of zgcPageFor used on
// the syscall/cgo boundary: an announced, lock-free directory probe plus
// the nosplit source-range check, with no splitting call. Misses fall back
// to the source address at the caller, which stays valid under quarantine
// while release waits on full publication.
//
// logosZgcFindObjectPtr is the upstream-hook form of the nosplit
// findObject canonicalization chain: heap-checked untag through atomic
// loads only, then remap through atomic loads only. Upstream mbitmap.go
// calls only this single hook so plain builds link without the
// experiment-only symbols. Misses keep the source, valid under
// quarantine while release waits on full publication.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcFindObjectPtr(p uintptr) uintptr {
	return zgcRemapNoSplit(zgcUntagFast(p))
}

//go:nosplit
//go:nowritebarrierrec
func zgcPageRegisteredNoSplit(addr uintptr) bool {
	zgcPageTransientEnter()
	registered := zgcPageLookupTransient(addr) != nil
	zgcPageTransientLeave()
	return registered
}

// zgcUntagPtrSize is the stride of one entry in the raw arena-map loads
// below: every entry is a single pointer.
const zgcUntagPtrSize = unsafe.Sizeof(uintptr(0))

// zgcUntagHeapInUse reports whether a names a live heap span. It is the
// spanOfHeap membership test without slice indexing: -N -l builds inline
// nothing, and every bounds-checked index on a nosplit path adds a
// panicBounds/panicBounds64 frame chain that overflows the 792-byte
// nosplit stack budget for deep callers (typedslicecopy via getGCMask).
// Every address is validated in software before it is dereferenced
// (arena live, L1/L2 bounds, table and span non-nil), so a foreign word
// returns false without faulting or panicking. Span state still loads
// through s.state.get and the base/limit comparison mirrors spanOfHeap;
// only the validated-address table reads are raw. The index arithmetic
// duplicates arenaIndex plus arenaIdx.l1/l2: keep in sync with mheap.go.
//
//go:nosplit
//go:nowritebarrierrec
func zgcUntagHeapInUse(a uintptr) bool {
	if !zgcHeapArenaLive() {
		return false
	}
	ri := (a - arenaBaseOffset) / heapArenaBytes
	var l1, l2 uint
	if arenaL1Bits == 0 {
		l2 = uint(ri)
	} else {
		l1 = uint(ri) >> arenaL1Shift
		l2 = uint(ri) & (1<<arenaL2Bits - 1)
	}
	var ha *heapArena
	if arenaL1Bits == 0 {
		// Single level: arenas[0] is the L2 table itself (constant
		// index, so no bounds check). The l2 index below is variable
		// and stays raw.
		if l2 >= uint(len(mheap_.arenas[0])) {
			return false
		}
		l2t := mheap_.arenas[0]
		if l2t == nil {
			return false
		}
		ha = *(**heapArena)(unsafe.Pointer(uintptr(unsafe.Pointer(&l2t[0])) + uintptr(l2)*zgcUntagPtrSize))
	} else {
		if l1 >= uint(len(mheap_.arenas)) {
			return false
		}
		l2t := *(*unsafe.Pointer)(unsafe.Pointer(uintptr(unsafe.Pointer(&mheap_.arenas[0])) + uintptr(l1)*zgcUntagPtrSize))
		if l2t == nil {
			return false
		}
		ha = *(**heapArena)(unsafe.Pointer(uintptr(l2t) + uintptr(l2)*zgcUntagPtrSize))
	}
	if ha == nil {
		return false
	}
	s := *(**mspan)(unsafe.Pointer(uintptr(unsafe.Pointer(&ha.spans[0])) + uintptr((a/pageSize)%pagesPerArena)*zgcUntagPtrSize))
	if s == nil || s.state.get() != mSpanInUse || a < s.base() || a >= s.limit {
		return false
	}
	return true
}

//go:nosplit
//go:nowritebarrierrec
func zgcUntagFast(p uintptr) uintptr {
	colors := atomic.Loaduintptr(&zgcColorMaskBits)
	if colors == 0 {
		return p
	}
	// Validate first, before any masking: the word is strippable only
	// when every bit above the address field lies in the non-address
	// region (flag bits or the colour nibble), so a foreign high word
	// returns bit-identical instead of folding into the heap range.
	// Only a validated heap word reaches the mask below.
	if p&^(zgcAddrMask|zgcFlagMask|zgcColorMask) != 0 {
		return p
	}
	a := p & zgcAddrMask
	if a == p {
		// Uncolored word: already canonical (even if heap or foreign).
		return p
	}
	// Colored or flagged candidate: accept only when the stripped word
	// names heap. Before the arena map is live no word can name heap;
	// zgcUntagHeapInUse reports false there (as for any foreign word),
	// so the word passes through unchanged. The probe performs the
	// spanOfHeap membership test without slice indexing: -N -l builds
	// inline nothing, and bounds-checked indexes would add
	// panicBounds/panicBounds64 frames that overflow deep nosplit
	// callers (typedslicecopy via getGCMask).
	if !zgcPageRegisteredNoSplit(a) && !zgcUntagHeapInUse(a) {
		return p
	}
	// Validated heap word: keep the address field alone, so every color,
	// flag, and tag bit above it is gone in both encodings.
	return a
}

// logosZgcUntagArg strips every color, flag, and tag bit of a heap
// pointer argument crossing into a syscall or cgo call, so the kernel
// and C see the canonical word. Words that do not name heap after
// stripping (C, mmap, stack, globals) pass through with every high bit
// intact; with colors inert everything is the identity.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcUntagArg(p unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(zgcUntagFast(uintptr(p)))
}

// logosZgcCanonicalStoreValue strips the active color bits of a heap
// value being stored into a pointer slot. It must stay a direct function
// of its unsafe.Pointer argument, never the uintptr round trip of
// logosZgcUntagArg: upstream noescape hides escapes exactly through that
// uintptr conversion (stubs.go noescape/noEscapePtr), so a stored value
// routed through logosZgcUntagArg loses the escape edge that makes
// modulesinit heap-allocate the modules slice, and the global then names
// a dead stack frame (GDB 6114: modulesSlice == 0x7fffffffd5e0, the
// caller's C stack). With colors inert this returns p unchanged; the
// escape edge is structural, not arithmetic.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcCanonicalStoreValue(p unsafe.Pointer) unsafe.Pointer {
	q := unsafe.Pointer(zgcUntagFast(uintptr(p)))
	if q == p {
		return p
	}
	return q
}

// zgcCanonicalStringData returns the canonical data pointer of the string
// at sp. Map string fast paths compare through it so assembly memhash
// reads canonical bytes.
func zgcCanonicalStringData(sp *string) unsafe.Pointer {
	return unsafe.Pointer(zgcUntagFast(*(*uintptr)(unsafe.Pointer(sp))))
}

// maps_logosZgcCanonicalString is the maps package hook for canonical
// string data pointers (W2/W3 caller-canonicalized pattern).
//
//go:linkname maps_logosZgcCanonicalString internal/runtime/maps.logosZgcCanonicalString
func maps_logosZgcCanonicalString(sp *string) unsafe.Pointer {
	return zgcCanonicalStringData(sp)
}

// Canonical raw pointer atomics (W1b).
//
// A barriered load heals a heap slot to the good color, so a slot can
// hold a colored word when a raw atomic CAS or store touches it. The
// raw primitives below never heal: they canonicalize both operands,
// compare canonical identities against the raw word, and write only
// canonical replacement words, so a CAS succeeds across a color change
// and fails only on a true identity mismatch. With colors inert the
// untag is the identity and the CAS reduces to load, compare, swap.
//
// casPointerNoWB and storePointerNoWB are pulled by
// internal/runtime/atomic under the experiment (types.go
// CompareAndSwapNoWB and StoreNoWB); the same two runtime names serve
// the unconditional raw callers in stack.go and type.go, so the
// inverted-tag fallback file mirrors them with raw primitives.

// zgcAtomicStorepNoWB stores the canonical form of new at *ptr without
// a write barrier. Only canonical words are ever written; healing
// stays with the barriered-load slow path.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname zgcAtomicStorepNoWB internal/runtime/atomic.storePointerNoWB
func zgcAtomicStorepNoWB(ptr *unsafe.Pointer, new unsafe.Pointer) {
	atomic.StorepNoWB(noescape(unsafe.Pointer(ptr)), logosZgcCanonicalStoreValue(new))
}

// zgcAtomicCasp1 compares the canonical identity of old with the
// canonical identity of the raw word at *ptr. On identity match it
// retries until the raw word is unchanged, then swaps in the canonical
// form of new; a changed raw word (recolor, heal, relocation) retries
// rather than failing, a different canonical identity fails with the
// slot untouched. Never heals, never allocates, never locks.
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname zgcAtomicCasp1 internal/runtime/atomic.casPointerNoWB
func zgcAtomicCasp1(ptr *unsafe.Pointer, old, new unsafe.Pointer) bool {
	want := zgcUntagFast(uintptr(old))
	repl := uintptr(logosZgcUntagArg(new))
	slot := (*uintptr)(unsafe.Pointer(ptr))
	for {
		raw := atomic.Loaduintptr(slot)
		if zgcUntagFast(raw) != want {
			return false
		}
		if atomic.Casuintptr(slot, raw, repl) {
			return true
		}
	}
}

// zgcBoundaryWrappersLanded reports whether every pointer-typed assembly
// path reads and writes only untagged words.
//
// Contract: true means no asm consumer can observe a colored word —
// every listed bypass site either receives an untagged word from its Go
// caller or carries its own barrier — so the collector may rely on asm
// never faulting on, leaking, or persisting a color. False means at
// least one named residual below still needs a caller-side untag or an
// asm wrapper outside this slice (the .s files and their out-of-slice
// Go callers are frozen here), and the collector must keep the
// mitigations those residuals document.
//
// Per-site status (surveyed bypass list):
//   - W1b raw atomics (StorepNoWB, Casp1): CLOSED on the Go side.
//     zgcAtomicStorepNoWB and zgcAtomicCasp1 canonicalize both operands
//     through zgcUntagFast and write only canonical replacements; the
//     atomic_pointer.go wrappers untag before storing. Raw asm never
//     sees a color through this path.
//   - W1d arm64 out-of-line Loadp: CLOSED on the Go side.
//     zgcAtomicLoadpSlow mirrors the compiled barrier fast path,
//     strips ordinary-heap stale color (heap span, no registered
//     source) to the canonical address without touching the slot, and
//     delegates everything else (relocating pages with the original
//     word for the heal CAS, foreign words) to zgcLoadBarrierSlow.
//   - Syscall/cgo/fastcall pointer args: CLOSED on the Go side.
//     cgocall, logos_fastcall, and the foreign registry admit,
//     canonicalize (logosZgcForeignArg/zgcCanonical), and hold every
//     argument before the trap, so asmcgocall and crosscall2 carry only
//     canonical words into C; cgocallback's fn is a code pointer and its
//     frame is such a canonicalized argument, merely forwarded by asm.
//   - reflectcall fn dereference (asm MOVQ (DX)): REMAINING.
//     fn arrives from out-of-slice Go callers (mfinal, syscall_windows,
//     reflect) with no untag in this slice, and asm dereferences the
//     funcval word raw. Closing it needs a caller-side untag or an asm
//     wrapper; both are outside this slice's .s/caller ban, so the flag
//     stays false.
//   - Defer records read by asm: MITIGATED, REMAINING.
//     Heap-allocated defer closures are read without a barrier; the
//     evacuation pins every referent (logos_evac_pin.go pinGoroutine P8)
//     so nothing asm names moves. Pinning is the mitigation, not a
//     barrier, until the wrappers land.
func zgcBoundaryWrappersLanded() bool {
	return false
}

// zgcWalkAllLiveForCheck verifies no stale reference remains after the
// gcStart rewrite when checkmark debugging is on.
func zgcWalkAllLiveForCheck() {
	logosWalkAllWith(zgcVerifySlot, logosWalkPolicy{skipSpan: zgcSpanQuarantined, useAllocBits: true})
}

// Bounded foreign hold protocol.
//
// Holds protect heap storage referenced by foreign code (cgo, syscalls,
// and raw syscalls) across concurrent GC cycles and pause-driven release.
// Publication occurs at entry before entering foreign execution; exit
// clears the hold upon return. Bounded capacity prevents runaway growth,
// and overflow fails closed rather than running untracked.

const zgcMaxForeignHolds = 256

type zgcForeignHoldSlot struct {
	gp     atomic.Uintptr // *g address of holder, or 0 if free
	root   atomic.Uintptr // direct foreign argument root, canonicalized
	depth  atomic.Uint32  // nesting depth for reentrant foreign calls
	epoch  atomic.Uint32  // remap epoch at entry
	active atomic.Uint32  // 1 when hold is fully published, 0 otherwise
}

var (
	zgcForeignHoldSlots [zgcMaxForeignHolds]zgcForeignHoldSlot
	zgcForeignHoldCount atomic.Int32
	// zgcForeignHoldOverflow counts hold-table overflows. Entry is
	// nosplit on a user stack, so it cannot throw there; in-scope
	// callers discard the -1, so the flag carries the failure to the
	// release gate (system stack in the pause, preemptible in the test
	// export), where throwing is legal. Sticky by design: a drained
	// table does not un-run the untracked entry.
	zgcForeignHoldOverflow atomic.Uint32
)

// logosZgcForeignArg canonicalizes and untags a pointer crossing a foreign
// boundary, ensuring that if the argument names an object on a relocated
// page, the foreign caller receives the canonical to-space copy address.
//
// The entry is nosplit (it runs on the syscall/cgo path where the stack
// must not split), so it uses only the nosplit chain: zgcUntagFast strips
// the active color bits without touching high foreign bits, and
// zgcRemapNoSplit resolves a published copy through atomic loads only. A
// miss returns the untagged source, which stays valid under quarantine
// while release waits on full publication; the next preemptible load
// heals and converges. It never calls the splitting zgcCanonical here:
// that path locks and may grow the stack, which is exactly the user-stack
// system-stack fatal this boundary must not produce.
//
//go:nosplit
//go:nowritebarrierrec
func logosZgcForeignArg(p unsafe.Pointer) unsafe.Pointer {
	if p == nil {
		return nil
	}
	return unsafe.Pointer(zgcRemapNoSplit(zgcUntagFast(uintptr(p))))
}

// logosZgcForeignHoldEnter publishes a hold for gp before entering foreign
// execution. If root is non-zero, it names an argument passed across the
// boundary, which is canonicalized before recording.
//
// Reentrant calls on the same gp increment depth. Nosplit-safe and allocation-free:
// the root resolves through the nosplit untag/remap chain only, and overflow
// publishes nothing (never a throw: throwing needs the system stack, which
// the nosplit caller does not have). The -1 alone cannot fail closed — the
// entry callers discard it — so overflow also sets zgcForeignHoldOverflow,
// and the release gate throws there instead of freeing memory out from
// under the untracked entry.
//
//go:nosplit
func logosZgcForeignHoldEnter(gp *g, root uintptr) int {
	if gp == nil {
		return -1
	}
	canonicalRoot := uintptr(0)
	if root != 0 {
		canonicalRoot = zgcRemapNoSplit(zgcUntagFast(root))
	}
	gVal := uintptr(unsafe.Pointer(gp))
	// 1. Nested call on the same goroutine.
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		if slot.gp.Load() == gVal && slot.active.Load() != 0 {
			slot.depth.Add(1)
			if canonicalRoot != 0 && slot.root.Load() == 0 {
				slot.root.Store(canonicalRoot)
			}
			return i
		}
	}
	// 2. Claim empty slot. Claim gp ownership, write payload, then publish active=1.
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		if slot.gp.Load() == 0 && slot.active.Load() == 0 {
			if slot.gp.CompareAndSwap(0, gVal) {
				slot.root.Store(canonicalRoot)
				slot.epoch.Store(zgcRemapEpoch.Load())
				slot.depth.Store(1)
				slot.active.Store(1)
				zgcForeignHoldCount.Add(1)
				return i
			}
		}
	}
	// 3. Overflow: no hold published. No throw here: this entry is
	// nosplit on a user stack, and throwing needs the system stack.
	// The -1 alone is not fail-closed (callers discard it and still
	// enter foreign code), so record the overflow for the release gate
	// below, which throws on the system stack instead of releasing
	// memory out from under the untracked entry.
	zgcForeignHoldOverflow.Add(1)
	return -1
}

// logosZgcForeignHoldExit releases one hold level for gp upon return from
// foreign execution. Clears the slot when depth reaches zero.
//
//go:nosplit
func logosZgcForeignHoldExit(gp *g) {
	if gp == nil {
		return
	}
	gVal := uintptr(unsafe.Pointer(gp))
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		if slot.gp.Load() == gVal && slot.active.Load() != 0 {
			d := slot.depth.Add(-1)
			if d == 0 {
				slot.active.Store(0)
				slot.root.Store(0)
				slot.epoch.Store(0)
				zgcForeignHoldCount.Add(-1)
				slot.gp.Store(0)
			}
			return
		}
	}
}

// logosZgcForeignHoldDepth returns the nesting depth of gp's active hold.
//
//go:nosplit
func logosZgcForeignHoldDepth(gp *g) uint32 {
	if gp == nil {
		return 0
	}
	gVal := uintptr(unsafe.Pointer(gp))
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		if slot.gp.Load() == gVal && slot.active.Load() != 0 {
			return slot.depth.Load()
		}
	}
	return 0
}

// zgcPageHasActiveHold reports whether pg is retained by the foreign closure
// or directly addressed by any active foreign hold root. It consults both
// the legacy bounded-hold closure (populated by the entry hooks preserved
// below until emission migration) and the new-registry exact page pin set,
// so pages exposed through either path stay unreleased.
//
// Behavior is unchanged from before: this gate keeps directly named pages
// alive (linearizable hold publication; locking pin queries), nothing more.
// Payloads reachable only through a heap iovec array are intentionally not
// chased here: following that chain needs heap-object resolution
// (zgcForeignHeapBase) and slot walks, whose stability premise is snapshot
// isolation under a stopped world (logos_zgc_foreign_registry.go), while
// this gate is also callable with the world running (test export) and no
// portable stopped-world probe exists outside lockrank builds. Reads here
// are causal only (atomics plus the leaf gate), which is sufficient to
// retain directly named pages and insufficient to chase the heap. The crash
// fix therefore lives in the parent gate run: payload extents must be
// admitted at entry by the emitter rows, which run under stop-the-world
// exclusion.
//
// The new-registry consult always uses the locking form, so it is exact
// both inside a pause (the cycle caller) and with the world running (the
// test export), and it needs no world-state probe, which does not exist
// in builds without static lock ranking. Gate sections on both sides are
// short and never block, matching the quarantine and page leaf locks the
// pauses already share with world-running paths.
func zgcPageHasActiveHold(pg *zgcPage) bool {
	// Fail closed on a past hold-table overflow: some entry ran foreign
	// code with no hold published, so no page here is provably safe to
	// release. Throwing is legal at this gate — system stack in the
	// pause, preemptible in the test export, never a nosplit window —
	// while the entry-side throw was the user-stack fatal. Sticky: the
	// check precedes the count==0 early-out on purpose, so a drained
	// table does not clear the failure.
	if zgcForeignHoldOverflow.Load() != 0 {
		throw("logos zgc: foreign hold table overflowed")
	}
	if wi, bit, ok := zgcForeignPageBit(pg.start); ok {
		if zgcForeignVisited != nil && zgcForeignVisited[wi]&bit != 0 {
			return true
		}
	}
	if zgcForeignPagePinned(pg.start) {
		return true
	}
	if zgcForeignHoldCount.Load() == 0 {
		return false
	}
	for i := range zgcForeignHoldSlots {
		slot := &zgcForeignHoldSlots[i]
		if slot.active.Load() == 0 {
			continue
		}
		if slot.depth.Load() == 0 {
			continue
		}
		root := slot.root.Load()
		if root != 0 && root >= pg.start && root < pg.start+pg.size {
			return true
		}
	}
	return false
}
