// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"unsafe"
)

// cgoCanonicalPP resolves pp to the address the collector means by it so
// boundary verdicts judge the object named. A nested word healed to the
// good color fails the heap test and would skip the pin check, silently
// accepting an unpinned pointer; stale source words already resolve
// through forwarding. Read-only: slots and C frames are untouched, so an
// integer that alias heap in untyped memory is never rewritten.
func cgoCanonicalPP(pp unsafe.Pointer) unsafe.Pointer {
	return unsafe.Pointer(logosZgcCanonicalValue(uintptr(pp)))
}

// logosZgcCanonicalValue resolves v to the address the collector means by
// it: stale source words forward through the table and color bits come
// off, foreign words pass through. It reads only, allocates nothing, and
// runs in any preemptible context. This lives in shared code (not the
// tagged boundary file) so both collector configurations link it: the
// off-build zgcCanonical stub keeps it identity there.
func logosZgcCanonicalValue(v uintptr) uintptr {
	if !zgcCycleEnabled() {
		return v
	}
	return zgcCanonical(v)
}

// logosZgcCanonicalizeSlot rewrites *slot to its uncolored canonical
// address when it differs, and reports whether it stored. The store is
// an uncolored canonical word, the same shape every collector fixup
// writes; the replaced word names the same object (or its published
// copy, which is born marked), so no barrier is needed and concurrent
// marking stays sound.
//
// Call only from preemptible code before the values are copied into a
// foreign frame: cgocall and entersyscall themselves are nosplit and
// cannot walk types, and healing a slot after C copied the value cannot
// reach the copy. Generated cgo frames and syscall wrappers reload each
// pointer they hand out through this helper before the trap
// (parent-wired call sites, one slot at a time with the type metadata
// the walker already owns); the runtime's own top-level untag in cgocall
// stays the nosplit backstop for the frame pointer itself.
//
//go:nowritebarrierrec
func logosZgcCanonicalizeSlot(slot *uintptr) bool {
	if !zgcCycleEnabled() {
		return false
	}
	c := zgcCanonical(*slot)
	if c == *slot {
		return false
	}
	*slot = c
	return true
}

// cgoCanonicalizeSlot walks the value ptr names starting at the original
// variable slot, rewriting every pointer slot to its uncolored canonical
// address. It follows the same type-driven traversal as cgoCheckArg but
// never judges and never panics, so unpinned nesting is canonicalized
// rather than rejected. Generated code calls it pre-frame for each
// pointer argument, next to its _cgoCheckPointer emission, passing the
// address of its own argument variable: healing the walker's copies
// inside cgoCheckPointer could not reach what C reads, but healing the
// originals before the frame is built can. No-op unless the zgc cycle
// runs; preemptible context only.
//
//go:linkname cgoCanonicalizeSlot
func cgoCanonicalizeSlot(ptr any, slot unsafe.Pointer) {
	if slot == nil || !zgcCycleEnabled() {
		return
	}
	ep := efaceOf(&ptr)
	t := ep._type
	if t == nil || !t.Pointers() {
		return
	}
	if t.IsDirectIface() {
		// The slot holds the value itself. Canonicalize the top
		// word, then walk the referent for pointer kinds: a typed
		// pointer by its element type, which preserves the nested
		// layout, and unsafe.Pointer by heap type information, as
		// the checker does for a pointer whose element is unknown.
		// unsafe.Pointer's descriptor is a bare abi.Type with no
		// PtrType.Elem behind it, so it must never be read as a
		// ptrtype. Other direct kinds have no walkable referent,
		// matching the checker, which rejects them instead.
		logosZgcCanonicalizeSlot((*uintptr)(slot))
		switch t.Kind() {
		case abi.Pointer:
			p := *(*unsafe.Pointer)(slot)
			if p != nil {
				pt := (*ptrtype)(unsafe.Pointer(t))
				cgoCanonicalizeArg(pt.Elem, p, true)
			}
		case abi.UnsafePointer:
			p := *(*unsafe.Pointer)(slot)
			if p != nil {
				cgoCanonicalizeUnknown(p)
			}
		}
		return
	}
	cgoCanonicalizeArg(t, slot, true)
}

// cgoCanonicalizeArg rewrites every pointer slot of the value of type t
// at p to its uncolored canonical address. The traversal mirrors
// cgoCheckArg case for case (same typed metadata, same scoping, same
// structural recursion profile) minus verdicts: panics become returns,
// pin tests are skipped, and each descended slot is canonicalized first
// so the walk continues into the canonical subgraph. Stores are plain
// uncolored canonical words naming the same object or its born-marked
// copy, so concurrent marking stays sound without a barrier.
func cgoCanonicalizeArg(t *_type, p unsafe.Pointer, indir bool) {
	if !t.Pointers() || p == nil {
		return
	}
	switch t.Kind() {
	default:
		throw("can't happen")
	case abi.Array:
		at := (*arraytype)(unsafe.Pointer(t))
		if !indir {
			if at.Len != 1 {
				throw("can't happen")
			}
			cgoCanonicalizeArg(at.Elem, p, !at.Elem.IsDirectIface())
			return
		}
		for range at.Len {
			cgoCanonicalizeArg(at.Elem, p, true)
			p = add(p, at.Elem.Size_)
		}
	case abi.Chan, abi.Map, abi.Func:
		// No walkable referent under the checker either. The top
		// word is canonicalized by the caller.
		if indir {
			logosZgcCanonicalizeSlot((*uintptr)(p))
		}
	case abi.Interface:
		it := *(**_type)(p)
		if it == nil || inheap(uintptr(unsafe.Pointer(it))) {
			return
		}
		dp := add(p, goarch.PtrSize)
		logosZgcCanonicalizeSlot((*uintptr)(unsafe.Pointer(dp)))
		p = *(*unsafe.Pointer)(dp)
		if p == nil {
			return
		}
		cgoCanonicalizeArg(it, p, !it.IsDirectIface())
	case abi.Slice:
		st := (*slicetype)(unsafe.Pointer(t))
		s := (*slice)(p)
		logosZgcCanonicalizeSlot((*uintptr)(unsafe.Pointer(&s.array)))
		p = s.array
		if p == nil {
			return
		}
		if !st.Elem.Pointers() {
			return
		}
		for range s.cap {
			cgoCanonicalizeArg(st.Elem, p, true)
			p = add(p, st.Elem.Size_)
		}
	case abi.String:
		ss := (*stringStruct)(p)
		logosZgcCanonicalizeSlot((*uintptr)(unsafe.Pointer(&ss.str)))
	case abi.Struct:
		st := (*structtype)(unsafe.Pointer(t))
		if !indir {
			if len(st.Fields) != 1 {
				throw("can't happen")
			}
			cgoCanonicalizeArg(st.Fields[0].Typ, p, !st.Fields[0].Typ.IsDirectIface())
			return
		}
		for _, f := range st.Fields {
			if !f.Typ.Pointers() {
				continue
			}
			cgoCanonicalizeArg(f.Typ, add(p, f.Offset), true)
		}
	case abi.Pointer, abi.UnsafePointer:
		if indir {
			logosZgcCanonicalizeSlot((*uintptr)(p))
			p = *(*unsafe.Pointer)(p)
			if p == nil {
				return
			}
		}
		cgoCanonicalizeUnknown(p)
	}
}

// cgoCanonicalizeUnknown rewrites the pointer slots of the heap object at
// p using heap type information, the same metadata
// cgoCheckUnknownPointer judges with, minus verdicts. One level, like the
// checker: pinned interiors stay a documented residual in both.
func cgoCanonicalizeUnknown(p unsafe.Pointer) {
	p = cgoCanonicalPP(p)
	if !inheap(uintptr(p)) {
		return
	}
	base, span, _ := findObject(uintptr(p), 0, 0)
	if base == 0 {
		return
	}
	tp := span.typePointersOfUnchecked(base)
	for {
		var addr uintptr
		if tp, addr = tp.next(base + span.elemsize); addr == 0 {
			break
		}
		logosZgcCanonicalizeSlot((*uintptr)(unsafe.Pointer(addr)))
	}
}
