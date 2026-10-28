// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/abi"
	"internal/goexperiment"
	"unsafe"
)

// logosMovingMode reports whether a moving collector is selected: the
// experiment is on and the linker chose a moving -gc mode (M19-S5 for
// immix; the zgc lane sets -gc=zgc). Pin helpers are shared because an
// object that must not move under one moving collector must not move
// under any.
func logosMovingMode() bool {
	return goexperiment.LogosGCMoving && (logosGCMode == "immix" || logosGCMode == "zgc")
}

// logosExcludeRelocation records that a special of the given kind was
// attached to span. Finalizers and cleanups run against the object they
// were attached to, so attaching one immoblizes the span: an object with
// a finalizer or cleanup is a root and cannot keep an object alive.
// Guarded by speciallock.
func (span *mspan) logosExcludeRelocation(kind uint8) {
	if kind == _KindSpecialFinalizer || kind == _KindSpecialCleanup {
		span.logosNoReloc = true
	}
}

// logosPinObject keeps the object containing p in place until its span is
// reused: the object's address feeds an address hash or an untraced root.
func logosPinObject(p unsafe.Pointer) {
	if s := spanOfHeap(uintptr(p)); s != nil {
		s.logosPinned = true
	}
}

// logosPinSudog keeps s in place: wait queues of channels that hold no
// pointers for the collector still point at it.
func logosPinSudog(s *sudog) *sudog {
	if s != nil && logosMovingMode() {
		logosPinObject(unsafe.Pointer(s))
	}
	return s
}

// logosPinChan keeps a channel whose collector type has no pointers in
// place, with the synctest bubble it names. Such a channel holds its
// buffer, queues, and lock inline: the collector does not walk them, so
// they must not move.
func logosPinChan(c *hchan, mem uintptr, elem *_type) {
	if mem != 0 && elem.Pointers() {
		return
	}
	logosPinObject(unsafe.Pointer(c))
}

// maps_logosPinMapKey pins every heap object whose address the hash of the
// map key at p mixes in (#159). The map code calls it before it hashes the
// key. Holding the M keeps a stop-the-world evacuation from moving an object
// between the read of its address and the pin. Shared by both moving
// collectors; the gate selects the mode, not the lane.
//
//go:linkname maps_logosPinMapKey internal/runtime/maps.logosPinMapKey
func maps_logosPinMapKey(key *abi.Type, p unsafe.Pointer) {
	if !logosMovingMode() {
		return
	}
	mp := acquirem()
	logosPinKeyValue(key, p)
	releasem(mp)
}

// sync_logosPinMapKey is maps_logosPinMapKey for internal/sync.HashTrieMap,
// which hashes keys through the map type's hasher on its own.
//
//go:linkname sync_logosPinMapKey internal/sync.logosPinMapKey
func sync_logosPinMapKey(key *abi.Type, p unsafe.Pointer) {
	maps_logosPinMapKey(key, p)
}

// logosPinKeyValue walks a value of type t at p the way the type's hash does
// and pins each object hashed by address.
func logosPinKeyValue(t *abi.Type, p unsafe.Pointer) {
	if !t.Pointers() {
		return
	}
	switch t.Kind() {
	case abi.Pointer, abi.UnsafePointer, abi.Chan:
		logosPinObject(*(*unsafe.Pointer)(p))
	case abi.Interface:
		logosPinInterfaceKey(t, p)
	case abi.Array:
		at := (*abi.ArrayType)(unsafe.Pointer(t))
		for i := uintptr(0); i < at.Len; i++ {
			logosPinKeyValue(at.Elem, add(p, i*at.Elem.Size_))
		}
	case abi.Struct:
		st := (*abi.StructType)(unsafe.Pointer(t))
		for i := range st.Fields {
			f := &st.Fields[i]
			logosPinKeyValue(f.Typ, add(p, f.Offset))
		}
	}
}

// logosPinInterfaceKey pins the pointers inside the dynamic value of the
// interface key at p.
func logosPinInterfaceKey(t *abi.Type, p unsafe.Pointer) {
	var dyn *abi.Type
	if len((*abi.InterfaceType)(unsafe.Pointer(t)).Methods) == 0 {
		dyn = (*eface)(p)._type
	} else if tab := (*iface)(p).tab; tab != nil {
		dyn = tab.Type
	}
	if dyn == nil {
		return
	}
	data := (*[2]unsafe.Pointer)(p)[1]
	if dyn.IsDirectIface() {
		logosPinObject(data)
		return
	}
	logosPinKeyValue(dyn, data)
}

// sync_logosPinObject pins the object holding p for sync.Cond, which records
// its own address as a uintptr. Shared by both moving collectors; the gate
// selects the mode, not the lane.
//
//go:linkname sync_logosPinObject sync.runtime_logosPinObject
func sync_logosPinObject(p unsafe.Pointer) {
	if !logosMovingMode() {
		return
	}
	mp := acquirem()
	logosPinObject(p)
	releasem(mp)
}
