// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/abi"
	"unsafe"
)

// Private cgo emission ABI for the moving collector (owner G).
//
// cmd/cgo emits these entry points only when it generates for the moving
// experiment on amd64 or arm64, the exact build constraint of this file;
// logos_zgc_cgo_off.go provides the same four symbols with inert bodies
// for every other runtime shape so generated code links identically.
// Ordinary (non-moving) cgo output carries no reference to any of them.
//
// Generated code cannot name runtime types and must not guess reflection
// layouts, so each foreign descriptor travels as one row of the generated
// _cgoForeignRow type: an interface value (the existing type-info
// transport _cgoCheckPointer already uses) plus a shape tag. The
// interface carries the compiler-authoritative static type of the thing
// being described and, where one exists, its address, without copying
// aggregates or spelling any type in generated source:
//
//   - cgoForeignShapeVar: V is any(&v) for a variable v the C call reads
//     (an argument local _cgoN, or an exported function's result slot).
//     The runtime roots the variable itself; admission rewrites it to the
//     canonical copy address, and the caller reloads it right before C.
//   - cgoForeignShapeExtent: V is any(x) for a pointer or slice value x
//     whose pointee or backing C may read as a whole: a pointer names its
//     pointee (single element, or an array tiled by its element type), a
//     slice names its whole backing up to cap. Only the pointer cells the
//     element type's GC bitmap names are walked.
//   - cgoForeignShapeContainer: V is any(&a) for the indexed operand a of
//     &a[i]; a may be a slice, an array (rooted in place, so a stack array
//     resolves exactly), or a pointer to an array (the array it names is
//     the extent, not the pointer variable).
//
// Rows are consumed in place: the runtime rewrites the caller's row array
// into []logosForeignRoot (same size and word layout, asserted below),
// compacts away rows that cannot name a pointer cell, and retains nothing
// after the call returns. Generated code builds a fresh array per call and
// never reuses it, so there is no row-count limit and no allocation.
const (
	cgoForeignShapeVar       = 0
	cgoForeignShapeExtent    = 1
	cgoForeignShapeContainer = 2
)

// cgoForeignRow mirrors the generated _cgoForeignRow word for word: the
// two interface words, an unused word that becomes logosForeignRoot.Size,
// and the shape tag that becomes logosForeignRoot.Kind.
type cgoForeignRow struct {
	typ   unsafe.Pointer // interface type word of V
	data  unsafe.Pointer // interface data word of V
	_     uintptr
	shape uintptr
}

// The in-place conversion needs identical size and field offsets; a
// negative array length fails the build if either layout drifts.
var (
	_ [unsafe.Sizeof(logosForeignRoot{}) - unsafe.Sizeof(cgoForeignRow{})]struct{}
	_ [unsafe.Sizeof(cgoForeignRow{}) - unsafe.Sizeof(logosForeignRoot{})]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Kind) - unsafe.Offsetof(cgoForeignRow{}.shape)]struct{}
	_ [unsafe.Offsetof(cgoForeignRow{}.shape) - unsafe.Offsetof(logosForeignRoot{}.Kind)]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Slot) - unsafe.Offsetof(cgoForeignRow{}.data)]struct{}
	_ [unsafe.Offsetof(cgoForeignRow{}.data) - unsafe.Offsetof(logosForeignRoot{}.Slot)]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Size) - 2*unsafe.Sizeof(uintptr(0))]struct{}
	_ [2*unsafe.Sizeof(uintptr(0)) - unsafe.Offsetof(logosForeignRoot{}.Size)]struct{}
)

// cgoForeignBegin admits the Go memory a generated cgo call is about to
// hand to C and returns the token the caller must pair with
// cgoForeignEnd. Generated code reloads every admitted variable after
// this returns and before entering C. With the zgc cycle disabled nothing
// relocates, rows are not even converted, and the invalid token (End is a
// no-op) comes back; in the configured cycle the token is always valid,
// including for an empty closure, because a callback may extend it.
//
// Call from preemptible Go code before the foreign call: admission
// stops the world.
//
//go:linkname cgoForeignBegin
func cgoForeignBegin(rows unsafe.Pointer, n uintptr) logosForeignToken {
	if !zgcCycleEnabled() {
		return logosForeignInvalidToken
	}
	n = cgoForeignRoots(rows, n)
	return logosZgcForeignBegin((*logosForeignRoot)(rows), n)
}

// cgoForeignEnd releases the admission cgoForeignBegin or cgoForeignEnter
// returned. Generated code defers it immediately after the paired Begin,
// so normal return, panic unwinding through C callbacks, and Goexit all
// release the frame exactly once. The invalid token is a no-op.
//
//go:linkname cgoForeignEnd
func cgoForeignEnd(t logosForeignToken) {
	logosZgcForeignEnd(t)
}

// cgoForeignEnter opens the callback scope of an exported Go function. A
// callback that runs inside a Go to C call (a published frame already
// sits on this goroutine's chain) extends that real outer frame, which
// the outer C call's own End releases: no scope is opened and the invalid
// token comes back. A callback with no outer, such as a C-created
// thread, opens a self-bounded frame: Begin with an empty closure (valid
// even though empty, so results can extend it) that the wrapper's
// deferred cgoForeignEnd retires. With the zgc cycle disabled it is a
// no-op returning the invalid token.
//
//go:linkname cgoForeignEnter
func cgoForeignEnter() logosForeignToken {
	if !zgcCycleEnabled() {
		return logosForeignInvalidToken
	}
	if _, ok := cgoForeignOuter(getg()); ok {
		return logosForeignInvalidToken
	}
	return logosZgcForeignBegin(nil, 0)
}

// cgoForeignResults canonicalizes and admits the result words an exported
// Go function returns to C. Generated code calls it only after the
// existing _cgoCheckResult verdicts accepted the raw result words, so the
// checker never sees a rewritten word. The rows extend the innermost real
// outer frame (the scope cgoForeignEnter opened, or the Go to C frame the
// callback is nested in); explicit Pinner pins survive that frame's End
// by their own protocol. With nothing to extend (the cycle was configured
// off between Enter and here) it falls back to a self-bounded
// Begin/End so a result word is never left uncanonicalized in the
// configured cycle.
//
//go:linkname cgoForeignResults
func cgoForeignResults(rows unsafe.Pointer, n uintptr) {
	if !zgcCycleEnabled() {
		return
	}
	n = cgoForeignRoots(rows, n)
	roots := (*logosForeignRoot)(rows)
	if outer, ok := cgoForeignOuter(getg()); ok {
		logosZgcForeignExtend(outer, roots, n)
		return
	}
	logosZgcForeignEnd(logosZgcForeignBegin(roots, n))
}

// cgoForeignOuter returns the token of the innermost published frame on
// gp's chain: the real outer a callback extends. Unpublished NoSplit
// markers own nothing and cannot be extended, so they are skipped. The
// walk is bounded by the recorded depth; a longer chain is corrupt and
// fails loud.
func cgoForeignOuter(gp *g) (logosForeignToken, bool) {
	steps := gp.logosForeignDepth
	for top := gp.logosForeignTop; top != 0; {
		f := (*zgcForeignFrame)(unsafe.Pointer(top))
		if f.published == 1 {
			return logosForeignToken{Frame: unsafe.Pointer(f), Gen: f.gen}, true
		}
		if steps == 0 {
			throw("logos zgc: foreign frame chain longer than its depth")
		}
		steps--
		top = f.parent
	}
	return logosForeignToken{}, false
}

// cgoForeignRoots converts the n generated rows at rows into
// logosForeignRoot descriptors in place and returns the compacted count.
// A row that cannot name a pointer cell (pointer-free element type, nil
// or empty extent, unknown referent layout) is dropped: admission would
// find nothing to resolve or rewrite, and scalar bytes are never named.
// Reads row i before writing slot out <= i, so compaction is safe.
func cgoForeignRoots(rows unsafe.Pointer, n uintptr) uintptr {
	if n == 0 {
		return 0
	}
	if rows == nil {
		throw("logos zgc: cgo foreign rows missing")
	}
	const rootSize = unsafe.Sizeof(logosForeignRoot{})
	if n > ^uintptr(0)/rootSize {
		throw("logos zgc: cgo foreign row count overflows the address space")
	}
	out := uintptr(0)
	for i := uintptr(0); i < n; i++ {
		var root logosForeignRoot
		if !cgoForeignRowRoot((*cgoForeignRow)(add(rows, i*rootSize)), &root) {
			continue
		}
		*(*logosForeignRoot)(add(rows, out*rootSize)) = root
		out++
	}
	return out
}

// cgoForeignRowRoot resolves one generated row to its descriptor. It
// reports false for rows that name no pointer cell.
func cgoForeignRowRoot(row *cgoForeignRow, root *logosForeignRoot) bool {
	t := (*_type)(row.typ)
	if t == nil {
		throw("logos zgc: cgo foreign row without a type word")
	}
	switch row.shape {
	case cgoForeignShapeVar:
		return cgoForeignVarRoot(t, row.data, root)
	case cgoForeignShapeExtent:
		return cgoForeignExtentRoot(t, row.data, root)
	case cgoForeignShapeContainer:
		return cgoForeignContainerRoot(t, row.data, root)
	}
	throw("logos zgc: cgo foreign row with unknown shape")
	return false
}

// cgoForeignVarRoot resolves any(&v): t is *V and slot is &v. A pointer
// or unsafe.Pointer variable is exactly one pointer word and takes the
// typeless forms; every other pointer-carrying variable (a C struct, a
// string header, a slice header) walks its static GC bitmap, so aggregate
// cells resolve precisely while scalar cells stay bit-identical.
func cgoForeignVarRoot(t *_type, slot unsafe.Pointer, root *logosForeignRoot) bool {
	if t.Kind() != abi.Pointer || slot == nil {
		throw("logos zgc: cgo foreign variable row without a variable address")
	}
	v := (*ptrtype)(unsafe.Pointer(t)).Elem
	if !v.Pointers() {
		return false
	}
	switch v.Kind() {
	case abi.UnsafePointer:
		*root = logosForeignRoot{Slot: slot, Kind: logosForeignRootUnknown}
	case abi.Pointer:
		*root = logosForeignRoot{Slot: slot, Kind: logosForeignRootValue}
	default:
		*root = logosForeignRoot{Type: unsafe.Pointer(v), Slot: slot, Kind: logosForeignRootValue}
	}
	return true
}

// cgoForeignContainerRoot resolves any(&a) for the operand a of &a[i].
// The container is what C may index, not the variable that names it: a
// slice variable resolves to its whole backing, a pointer to an array to
// the array it names (the typed [N]T traversal the pointer check uses,
// not the heap-only unknown-pointer path), and an array variable is
// rooted in place so a stack array resolves exactly. A non-variable
// operand arrives as the slice or *[N]T value itself and resolves as in
// cgoForeignExtentRoot.
func cgoForeignContainerRoot(t *_type, data unsafe.Pointer, root *logosForeignRoot) bool {
	if t.Kind() != abi.Pointer || data == nil {
		return cgoForeignExtentRoot(t, data, root)
	}
	pe := (*ptrtype)(unsafe.Pointer(t)).Elem
	switch pe.Kind() {
	case abi.Slice:
		s := (*slice)(data)
		elem := (*slicetype)(unsafe.Pointer(pe)).Elem
		return cgoForeignWholeRoot(elem, s.array, cgoForeignBackingSize(s, elem), root)
	case abi.Pointer:
		if (*ptrtype)(unsafe.Pointer(pe)).Elem.Kind() == abi.Array {
			return cgoForeignExtentRoot(pe, *(*unsafe.Pointer)(data), root)
		}
	}
	return cgoForeignExtentRoot(t, data, root)
}

// cgoForeignBackingSize returns the byte length of s's whole backing up
// to cap. A product that overflows the address space cannot belong to a
// valid slice and fails loud rather than under-pinning a wrapped extent.
func cgoForeignBackingSize(s *slice, elem *_type) uintptr {
	if elem.Size_ != 0 && uintptr(s.cap) > ^uintptr(0)/elem.Size_ {
		throw("logos zgc: cgo foreign slice backing overflows the address space")
	}
	return uintptr(s.cap) * elem.Size_
}

// cgoForeignExtentRoot resolves any(x) to the whole extent C may read: a
// slice value's entire backing up to cap, or what a pointer names, which
// is one element of its pointee type (an array pointee tiles by its
// element type). A pointer to a slice variable names the slice header,
// one element like any other pointee; only the container shape looks
// through it.
func cgoForeignExtentRoot(t *_type, data unsafe.Pointer, root *logosForeignRoot) bool {
	if data == nil {
		return false
	}
	switch t.Kind() {
	case abi.Slice:
		// any(slice): data points at the boxed header.
		s := (*slice)(data)
		elem := (*slicetype)(unsafe.Pointer(t)).Elem
		return cgoForeignWholeRoot(elem, s.array, cgoForeignBackingSize(s, elem), root)
	case abi.Pointer:
		// A pointer is direct in the interface: data is the pointer.
		pe := (*ptrtype)(unsafe.Pointer(t)).Elem
		if pe.Kind() == abi.Array {
			return cgoForeignWholeRoot((*arraytype)(unsafe.Pointer(pe)).Elem, data, pe.Size_, root)
		}
		return cgoForeignWholeRoot(pe, data, pe.Size_, root)
	}
	// unsafe.Pointer and everything else: no known layout to tile.
	return false
}

// cgoForeignWholeRoot builds the Whole descriptor tiling elem over size
// bytes at base. The extent is the exact byte length, never inferred
// from a type: a slice type's size is its header, not its backing. It
// reports false when nothing pointer-carrying lies in it: a nil base, an
// empty or zero-sized extent, or a pointer-free element type.
func cgoForeignWholeRoot(elem *_type, base unsafe.Pointer, size uintptr, root *logosForeignRoot) bool {
	if base == nil || size == 0 || elem.Size_ == 0 || !elem.Pointers() {
		return false
	}
	*root = logosForeignRoot{Type: unsafe.Pointer(elem), Slot: cgoForeignCanonicalAddr(base), Size: size, Kind: logosForeignRootWhole}
	return true
}

// cgoForeignCanonicalAddr resolves an extent base the generated code
// captured into a row to the address the collector means by it. Rows are
// built before the stop-the-world admission, so a backing that relocated
// in between must be scanned where it lives now, not at its retired
// source. Variable slots are never resolved: they are the caller's own
// writable storage, exactly as emitted. Read-only: stack, global, and C
// addresses pass through unchanged.
func cgoForeignCanonicalAddr(p unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(zgcCanonical(uintptr(p)))
}
