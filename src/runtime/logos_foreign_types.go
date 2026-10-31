// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Shared foreign-call descriptor/token ABI (owner A: registry and ABI).
//
// This file is intentionally build-tag free: the moving collector and the
// off-experiment fallback share the exact descriptor, kind, and token
// layout, so generated cgo/syscall emission compiles identically on both
// shapes. Only the admission behaviour differs (registry versus no-op).
//
// A descriptor names Go memory the foreign call may observe, before any
// uintptr conversion erases provenance. The runtime consumes the array
// synchronously during admission and retains no descriptor, slot, or
// backing-array pointer afterwards: generated code may reuse or free the
// array as soon as Begin returns.
//
// Token protocol (both shapes):
//
//   - The invalid token (Frame == nil, Gen == 0) is the disabled-mode
//     sentinel: nothing relocates on this build, so there is no
//     ownership to take. Begin returns it only when the cycle is
//     disabled (or off-experiment), and the nested NoSplit path returns
//     it for trusted no-callback empty input. End and Extend on an
//     invalid token are no-ops; no pairing is required. Empty must not
//     be conflated with disabled: in the configured cycle, Begin
//     returns a VALID token even for an empty closure, because a
//     callback may still Extend it (e.g. a returned Pinner-pinned
//     pointer), and End is always required to recycle the frame.
//   - A valid token names exactly one admission on the calling goroutine.
//     The caller keeps it alive across the foreign call and passes it to
//     End exactly once, after foreign execution and result capture.
//     Skipping End leaks the pin; duplicating End throws.

// Per-Kind Type contract (settled; emission must follow it — no producer
// is wired yet, so this is a clean cutover):
//
//   - Value: Type is the *_type of the rooted VARIABLE. A nil Type
//     declares the variable is exactly one pointer-sized word. A
//     non-nil Type walks every pointer cell of one typ.Size_ value at
//     Slot using the type's GC bitmap, so aggregates (slice/string
//     headers, interfaces, structs) on stacks or in temporaries resolve
//     precisely while scalar cells stay bit-identical. Size must be 0.
//   - Whole: Type is the ELEMENT *_type tiled across the extent: Size
//     bytes hold Size/typ.Size_ consecutive elements (a single struct
//     is one element; a slice backing uses its element type, so spare
//     capacity is covered exactly). Type must be non-nil, typ.Size_
//     must be positive, and Size must be an exact multiple of it;
//     scalar backings use their scalar element type (zero cells found,
//     nothing rewritten). A typeless Whole is rejected fail-closed:
//     typeless discovery cannot justify canonical stores to C-observed
//     cells. Size is never inferred from Type: it stays the
//     authoritative extent bound.
//   - Unknown: Type must be nil and Size must be 0. Roots one
//     pointer-sized variable of unavailable layout (unsafe.Pointer
//     referent): like typeless Value, the single word resolves or
//     passes through as scalar.
//   - Prepared state for the restricted nested path (BeginNoSplit): it
//     must not allocate, lock, or spin, so it only reads GC masks. Begin
//     and Extend build every declared descriptor type's on-demand mask
//     while the world runs, and a built mask is permanent. A NoSplit
//     descriptor whose type was never declared through an earlier Begin
//     or Extend throws instead of building the mask; producers must
//     admit the type at a preemptible site first.

// logosForeignRootKind selects how Slot/Size describe the rooted memory.
type logosForeignRootKind uint32

const (
	// logosForeignRootValue roots one variable: Slot is the address of
	// the variable. Each pointer cell the per-Kind contract names is
	// resolved to a heap object (or accepted as a scalar/stack word
	// needing no pin). On successful admission every resolved cell is
	// rewritten to its canonical copy address at commit, preserving any
	// interior offset, so the caller reloads C arguments from admitted
	// variables. Cells inside published heap exposure are rejected
	// fail-loud instead of raced or skipped. Stack cells must be fresh
	// per admission: never reuse a stack slot already observed by an
	// active outer C call.
	logosForeignRootValue logosForeignRootKind = iota
	// logosForeignRootWhole pins a whole multi-word extent (array/slice
	// backing, including spare capacity, or a stack container): Slot is
	// the first byte of the extent and Size its length in bytes. Pointer
	// cells named by the tiled element bitmap resolve into the heap
	// closure; a cell is canonicalized exactly when its storage is
	// caller-owned (stack cells) or heap memory this admission first
	// exposes in place, and left read-only otherwise (already-exposed
	// memory, copy sources). Scalar cells are never rewritten.
	// Pair it with a Value descriptor for the argument pointer itself.
	logosForeignRootWhole
	// logosForeignRootUnknown roots a pointer-sized variable whose type
	// descriptor is unavailable (unsafe.Pointer referent): the single
	// word resolves like typeless Value, and expansion of the resolved
	// object always uses span type information. The slot is still
	// rewritten to the canonical copy at commit.
	logosForeignRootUnknown
)

// logosForeignRoot is one admission descriptor. Type carries the
// per-Kind contract above (nil where the contract allows unknown
// layout). Heap object expansion always uses span type information,
// which is authoritative for heap; Value aggregates and Whole extents
// additionally walk the declared type bitmaps with Size as the exact
// bound. Size is never inferred from Type: a slice type's Size_ is its
// header, not its backing, so inference would silently under-pin
// backings. Emission must always pass the true extent bytes for
// dynamically sized extents. All fields are plain values so the array
// is safe to build on a Go stack, including for nosplit callers.
type logosForeignRoot struct {
	Type unsafe.Pointer // *_type of the rooted variable, nil if unknown
	Slot unsafe.Pointer // variable address (Value/Unknown) or extent base (Whole)
	Size uintptr        // extent bytes (Whole only); authoritative, never inferred
	Kind logosForeignRootKind
}

// logosForeignToken names one admission frame. Frame is the off-heap
// frame address; Gen is the frame generation and distinguishes a live
// frame from a recycled address.
type logosForeignToken struct {
	Frame unsafe.Pointer // *zgcForeignFrame (off-heap), nil when token-free
	Gen   uint64         // frame generation, 0 reserved for invalid
}

// logosForeignInvalidToken is the disabled-mode sentinel: no ownership,
// and End/Extend on it are no-ops. Returned only when the cycle is
// disabled (or off-experiment). A configured Begin never returns it,
// not even for an empty closure, so emission must always pair a
// configured Begin with End.
var logosForeignInvalidToken = logosForeignToken{Frame: nil, Gen: 0}

// logosForeignTokenValid reports whether t carries admission ownership.
// Pure arithmetic, safe in any context including nosplit.
//
//go:nosplit
func logosForeignTokenValid(t logosForeignToken) bool {
	return t.Frame != nil && t.Gen != 0
}

// logosForeignValidateRoots enforces descriptor shape per the per-Kind
// Type contract on both build shapes (fail-loud producer contract):
// non-nil roots for n != 0; known kinds; non-nil slots (Whole allows nil
// only with zero Size); Value carries no Size; Unknown carries no Type;
// Whole carries a non-nil element Type whose positive typ.Size_ divides
// Size. Nosplit-safe: plain loads and arithmetic, no allocation.
//
//go:nosplit
func logosForeignValidateRoots(roots *logosForeignRoot, n uintptr) {
	if n == 0 {
		return
	}
	if roots == nil {
		throw("logos zgc: nil foreign root descriptors")
	}
	rootSize := unsafe.Sizeof(logosForeignRoot{})
	for i := uintptr(0); i < n; i++ {
		desc := (*logosForeignRoot)(add(unsafe.Pointer(roots), i*rootSize))
		switch desc.Kind {
		case logosForeignRootValue:
			if desc.Slot == nil {
				throw("logos zgc: nil foreign root slot")
			}
			if desc.Size != 0 {
				throw("logos zgc: foreign value root with size")
			}
		case logosForeignRootUnknown:
			if desc.Slot == nil {
				throw("logos zgc: nil foreign root slot")
			}
			if desc.Type != nil {
				throw("logos zgc: foreign unknown root with type")
			}
			if desc.Size != 0 {
				throw("logos zgc: foreign unknown root with size")
			}
		case logosForeignRootWhole:
			if desc.Slot == nil && desc.Size != 0 {
				throw("logos zgc: nil foreign root region")
			}
			if desc.Type == nil {
				throw("logos zgc: foreign whole root without element type")
			}
			elemSize := (*_type)(desc.Type).Size_
			if elemSize == 0 || desc.Size%elemSize != 0 {
				throw("logos zgc: foreign whole root with misaligned size")
			}
		default:
			throw("logos zgc: unknown foreign root kind")
		}
	}
}

// Syscall iovec-payload Whole wire (owner H-runtime, producer ssagen).
//
// A blocking vectored syscall (readv/writev style) names its kernel
// buffers indirectly: the call's uintptr arguments name the iovec array
// while each payload is reachable only through the array's Base words.
// The boundary chase in logos_zgc_boundary.go defers a release but cannot
// exclude a payload page from relocation selection, so a kernel writing
// through the source address while the collector heals Go edges to the
// copy is a real hazard. Each payload extent is therefore admitted as a
// Whole descriptor (Slot = extent base, Size = authoritative bytes,
// Type = element *_type), pinned in the registry by AdmitWholeCell and
// never rewritten, for the whole blocking window: End pairs with the
// blocking completion (the post-call point), never with the scheduler
// return. Async sites (go/defer/tail) admit nothing, so no frame is ever
// released at a scheduler return while the child still runs.
//
// Wire (bound with the compiler lane; versioned in place, no new callee):
//
//   - The existing rows buffer carries both sections. When the top bit of
//     n is clear, the buffer holds exactly 2*n Unknown pairs and nothing
//     else: every existing producer and test behaves byte-identically.
//     When the top bit is set, n0 = n with the bit cleared counts the
//     Unknown pairs, one word at rows[2*n0] names the integer section,
//     and 2*m words follow holding one [ArraySlot, ElemType] pair per
//     iovec array. The integer section is a [*]uintptr array holding m
//     followed by one [BaseOff, LenOff, IovSize, Count] quad per array.
//     The top bit is
//     free because lawful pair counts stay far below it
//     (zgcSyscallMaxPairs); the nosplit leaf rejects the bit outright.
//     Pointers live only in unsafe.Pointer-typed memory (scanned) and
//     integers only in uintptr-typed memory (unscanned), so the
//     collector never chases a metadata integer as a pointer.
//   - ArraySlot is the address of the unsafe.Pointer temp whose uintptr
//     conversion the call reads (the same cell the Unknown pair names:
//     the array row is always emitted alongside). ElemType is the payload
//     element *_type (byte for Iovec-style payloads, so Len counts bytes
//     and Size is exact). BaseOff/LenOff are the byte offsets of the Base
//     and Len fields within one iovec struct, IovSize its stride, Count
//     the proven element count. The runtime loads each (Base, Len) pair
//     from the frozen array at admission time and builds, per element,
//     one Unknown root over the Base field (retaining the payload object)
//     plus one Whole root with Size = Len, so Size stays authoritative
//     for dynamically sized extents and is never inferred from Type.
//   - A nil array value or a zero Len element admits nothing for that
//     element (the kernel reports the error itself); unreadable addresses
//     are skipped element-wise so unmapped user pointers can neither
//     fault the stopped world nor reach the registry; malformed
//     descriptors throw fail-loud like every other lane.
//
// Only provable extents are emitted: a single iovec struct, a fixed-size
// array of them, or a slice recovered from the conversion operand's own
// address expression. An opaque pointer whose count lives only in a
// sibling scalar argument cannot be correlated inside the syscall lane;
// correlating it needs producer metadata from outside these files (see
// the producer note in logos_syscall_order.go) and is deliberately not
// emitted: those sites keep today's Unknown-only behavior, unchanged.
const logosSyscallWholeVersion = uintptr(1) << (8*unsafe.Sizeof(uintptr(0)) - 1)

// logosSyscallWholeSextuples reports whether n carries an iovec section.
// Pure arithmetic, safe in any context including nosplit.
//
//go:nosplit
func logosSyscallWholeSextuples(n uintptr) bool {
	return n&logosSyscallWholeVersion != 0
}
