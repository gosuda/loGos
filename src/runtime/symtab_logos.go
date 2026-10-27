// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/math"
	"internal/runtime/sys"
	"unsafe"
)

// logosCloneBaseName returns the base name with every terminal loGo/s
// clone suffix stripped (SPEC T-203.4, T-205, red-team #9, #74): the
// middle-dot infixes ·logosspec and ·logosct and the source-level
// comptime infix _logosct, each with a nonempty ASCII decimal sequence
// number, applied repeatedly to a fixpoint (a clone of a comptime copy
// reads <f>_logosct<M>·logosspec<N>). It mirrors logosCloneBaseName in
// cmd/link/internal/ld/pcln_logos.go and the capSinkName normalization in
// cmd/link/internal/ld/capabilities_logos.go, with values from the single
// source of truth in internal/abi (Logos*Infix).
//
// Naming rule: the link symbol and go tool nm keep the full canonical
// suffix so clones stay stable, unique, and resolvable; pclntab
// funcnametab stores the stripped base so traces print <f> and FuncForPC
// resolves to the base (row #74 fix). The base is also the capability
// sink identity, so a cloned sink matches its base across shared, plugin,
// and internal links. Runtime has no strings import; the strip is manual
// byte matching over the same table.
func logosCloneBaseName(name string) string {
	orig := name
	for {
		i := len(name)
		for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
			i--
		}
		if i == len(name) {
			return name
		}
		base, ok := logosCutCloneInfix(name[:i])
		if !ok {
			return name
		}
		if base == "" {
			return orig
		}
		name = base
	}
}

// logosCutCloneInfix cuts one terminal clone infix from s, reporting the
// base. One table-driven path handles every spelling.
func logosCutCloneInfix(s string) (string, bool) {
	infixes := [3]string{abi.LogosSpecDisplayInfix, abi.LogosCtDisplayInfix, abi.LogosCtASCIIInfix}
	for _, infix := range infixes {
		if len(s) >= len(infix) && s[len(s)-len(infix):] == infix {
			return s[:len(s)-len(infix)], true
		}
	}
	return "", false
}

// logosModuleTable is the active module list that GOEXPERIMENT=logosgcmoving
// publishes instead of the heap-resident *[]*moduledata. capacity
// *moduledata words follow the header, the way an itab trails its method
// table; the first n of them are published.
//
// Every element is a *moduledata, and moduledata is sys.NotInHeap, so the
// list names no Go heap object. Upstream keeps the slice header and backing
// array on the Go heap, where a moving collector may relocate them, so
// each read of the list is a pointer load that -lb must barrier, and the
// barrier's slow path relocates. cgoIsGoPointer reads the list under
// //go:nowritebarrierrec, where that call must not appear. This table is
// persistentalloc memory: it never moves, the collector never scans it, and
// its type is sys.NotInHeap, so the compiler's NotInHeap rule
// (types.PtrDataSize) keeps every load of it plain.
//
// Published entries [0, n) are immutable. n changes only by a release store
// after the entries it admits are written.
type logosModuleTable struct {
	sys.NotInHeap
	n        uintptr
	capacity uintptr
}

const logosModuleTableHdr = unsafe.Sizeof(logosModuleTable{})

// logosModuleTableAddr is the address of the current logosModuleTable, or 0
// before the first modulesinit. It is a uintptr read and written with
// atomics, like zgcDirAddr, so reading it is not a pointer load.
var logosModuleTableAddr uintptr

// logosModulePublishing is 1 while logosPublishModules runs, so a second
// concurrent caller fails loudly instead of corrupting the list.
var logosModulePublishing atomic.Uint32

//go:nosplit
func logosModuleTableCurrent() *logosModuleTable {
	addr := atomic.Loaduintptr(&logosModuleTableAddr)
	if addr == 0 {
		return nil
	}
	return (*logosModuleTable)(unsafe.Pointer(addr))
}

//go:nosplit
func (t *logosModuleTable) entries() **moduledata {
	return (**moduledata)(add(unsafe.Pointer(t), logosModuleTableHdr))
}

// logosActiveModules is activeModules under the experiment. It allocates
// nothing and loads no heap pointer. The slice has len == cap == n and
// aliases entries that are never modified after publication, so a caller
// holding it across a later publication keeps reading the list it saw, and
// an append by the caller copies.
//
//go:nosplit
//go:nowritebarrierrec
func logosActiveModules() []*moduledata {
	t := logosModuleTableCurrent()
	if t == nil {
		return nil
	}
	return unsafe.Slice(t.entries(), int(atomic.Loaduintptr(&t.n)))
}

// logosNewModuleTable allocates an unpublished table of the given capacity.
func logosNewModuleTable(capacity uintptr) *logosModuleTable {
	words, overflow := math.MulUintptr(capacity, goarch.PtrSize)
	if overflow || words > maxAlloc-logosModuleTableHdr {
		throw("modulesinit: module table size overflows")
	}
	t := (*logosModuleTable)(persistentalloc(logosModuleTableHdr+words, 0, &memstats.other_sys))
	t.capacity = capacity
	return t
}

// logosPublishModules makes mods the active module list. mods must extend
// the published list: the module list only grows, because addmoduledata
// appends at lastmoduledatap, modules are never unlinked or unloaded, and a
// module is marked bad only by plugin_lastmoduleinit before the module is
// first passed to modulesinit. The hasmain swap acts on the same prefix
// every time, because only the main executable carries hasmain. A mods that
// shrinks or rewrites the published prefix breaks that invariant and throws.
//
// New entries go into the unused tail of the current table, and one release
// store of n publishes them. A table with no room is replaced by one of at
// least twice its capacity, holding a copy of the published prefix, and the
// old table is retained because a nosplit reader may still hold it. The
// retained tables have geometrically growing capacities, so their total
// size is linear in the module count.
//
// The callers are schedinit, which runs on one M before any goroutine, and
// plugin_lastmoduleinit, which plugin.Open calls with pluginsMu held; the
// modulesinit contract allows one caller at a time. logosModulePublishing
// turns a violation into a throw. persistentalloc is legal in both
// contexts: schedinit calls modulesinit after mallocinit, and persistentalloc
// switches to the system stack itself.
func logosPublishModules(mods []*moduledata) {
	if !logosModulePublishing.CompareAndSwap(0, 1) {
		throw("modulesinit: concurrent call")
	}
	t := logosModuleTableCurrent()
	want := uintptr(len(mods))
	var have uintptr
	if t != nil {
		have = atomic.Loaduintptr(&t.n)
		if want < have {
			throw("modulesinit: active module list shrank")
		}
		for i, md := range unsafe.Slice(t.entries(), int(have)) {
			if md != mods[i] {
				throw("modulesinit: active module list changed")
			}
		}
	}
	grown := t == nil || want > t.capacity
	if grown {
		capacity := want
		if t != nil && 2*t.capacity > capacity {
			capacity = 2 * t.capacity
		}
		nt := logosNewModuleTable(capacity)
		if have != 0 {
			copy(unsafe.Slice(nt.entries(), int(have)), unsafe.Slice(t.entries(), int(have)))
		}
		t = nt
	}
	copy(unsafe.Slice((**moduledata)(add(unsafe.Pointer(t.entries()), have*goarch.PtrSize)), int(want-have)), mods[have:])
	atomic.Storeuintptr(&t.n, want)
	if grown {
		atomic.Storeuintptr(&logosModuleTableAddr, uintptr(unsafe.Pointer(t)))
	}
	logosModulePublishing.Store(0)
}
