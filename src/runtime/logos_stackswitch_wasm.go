// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// This file holds the Go side of the stack-switching mode of the wasm port
// (loGo/s M16-S1, --wasm-features=stack-switching). The assembly in
// asm_wasm.s (GOWASM_stackswitch) runs every goroutine on its own
// WebAssembly continuation, kept in table 1 of the module. Only that
// assembly references these declarations, so other builds drop them.
//
// The port runs one thread, so none of this state needs a lock.

// wasmContCur is the continuation slot plus 1 that is running, 0 on the
// root stack. wasmContNext is the slot plus 1 that the root loop resumes
// next, 0 when no switch is pending. wasmContPaused is the slot plus 1 of
// the continuation that paused for JavaScript (GOOS=js).
var wasmContCur, wasmContNext, wasmContPaused int64

// wasmConts maps a g to its continuation slot. A slot belongs to the g
// struct, not to one goroutine: g structs are never freed (allgs keeps
// them), and a goroutine that reuses a g resumes the continuation its
// predecessor left. That continuation then unwinds completely, because
// no frame on it matches the entry of the new goroutine, and its root
// enters the new goroutine. The table holds uintptr keys in
// persistentalloc memory, so it has no pointers for the garbage collector.
var wasmConts struct {
	tab  uintptr // [size]wasmContEntry
	size uintptr // power of 2
	n    uintptr // used entries
}

type wasmContEntry struct {
	g    uintptr
	slot int64 // plus 1
}

// wasmContSlot returns the continuation slot plus 1 of gp, assigning the
// next free slot to a g seen for the first time. runtime·gogo calls it on
// the g0 stack.
//
//go:nosplit
func wasmContSlot(gp *g) int64 {
	if wasmConts.n*2 >= wasmConts.size {
		wasmContGrow()
	}
	key := uintptr(unsafe.Pointer(gp))
	e := wasmContFind(wasmConts.tab, wasmConts.size, key)
	if e.g == 0 {
		e.g = key
		wasmConts.n++
		e.slot = int64(wasmConts.n)
	}
	return e.slot
}

// wasmContFind returns the entry of key in tab or the empty entry where
// key belongs.
//
//go:nosplit
func wasmContFind(tab, size, key uintptr) *wasmContEntry {
	i := (key >> 3) * 0x9e3779b97f4a7c15 & (size - 1)
	for {
		e := (*wasmContEntry)(unsafe.Pointer(tab + i*unsafe.Sizeof(wasmContEntry{})))
		if e.g == key || e.g == 0 {
			return e
		}
		i = (i + 1) & (size - 1)
	}
}

// wasmContGrow doubles the table and rehashes it. The old memory stays
// allocated; it is at most as large as the final table.
func wasmContGrow() {
	size := max(wasmConts.size*2, 64)
	tab := uintptr(persistentalloc(size*unsafe.Sizeof(wasmContEntry{}), 8, &memstats.other_sys))
	for i := uintptr(0); i < wasmConts.size; i++ {
		old := (*wasmContEntry)(unsafe.Pointer(wasmConts.tab + i*unsafe.Sizeof(wasmContEntry{})))
		if old.g != 0 {
			*wasmContFind(tab, size, old.g) = *old
		}
	}
	wasmConts.tab, wasmConts.size = tab, size
}
