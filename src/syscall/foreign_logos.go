// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix && goexperiment.logosgcmoving && (amd64 || arm64)

package syscall

import "unsafe"

// Fork/exec foreign preflight for the moving collector (owner: fork lane).
//
// The moving collector relocates heap objects while the parent builds
// the argv/env C strings, mapping buffers, and fd tables that the fork
// child reads after clone. Between SlicePtrFromStrings conversion and
// rawVforkSyscall the child-visible backings can move, so the child
// would exec or print stale bytes. This file admits the exact cells the
// child reads, in the parent, before runtime_BeforeFork, and pairs the
// admission after runtime_AfterFork.
//
// Two nested frames cover the two cell sets. Copies made after the
// outer Begin inherit canonical values, so every frame the parent or
// the child reads is canonical.
//
//   - Outer: forkExec admits its conversion cells before calling
//     down, and ends after the err-pipe read and failure cleanup.
//     This canonicalizes the cells the parent path reuses.
//   - Inner: forkAndExecInChild1 admits every derived buffer plus
//     its own cells immediately before runtime_BeforeFork, and ends
//     in the parent after the mapPipe sync. The token is written
//     through a pointer into forkAndExecInChild's frame, not
//     returned in forkAndExecInChild1's results: under
//     CLONE_VM|CLONE_VFORK the child runs the callee's frame, so a
//     token stored there can be clobbered by the child's own spills
//     and the parent's End would then validate a foreign generation.
//     The inner frame never Value-roots a slot inside outer-exposed
//     heap (fail-loud); those cells are already canonical from the
//     outer frame.
//   - The child never Begins or Ends: it only issues RawSyscall-class
//     calls and plain reads of admitted cells.
//
// Canonical cells, not copies: every Value descriptor names a private
// stack cell of the admitting frame, so commit rewrites the cells the
// clone path reads. Helpers receive pointers to those cells (or the
// slot address plus the caller-computed *_type); they never root their
// own parameter copies. Whole descriptors pin each backing with the
// true extent in bytes and the element *_type. Heap-interior slots are
// named only by the frame that first exposes their object; nested
// frames read those cells through canonical pointers and pin backings
// with Whole descriptors, which never throw on heap.
//
// Raw integers (pipe fds, flags, signals, pids) need no root and stay
// bit-identical. Stack scalars (lim, mapPipe, pidfd local, counters)
// live in address-stable stack memory and need no root. Globals (none,
// slash tables) never relocate. doCheckClonePidfd's child reads no
// memory (immediate EXIT_GROUP), so it needs no admission.
//
// The preflight does not exempt the fork functions from compiler
// instrumentation: base.LogosNoLB suppresses the load barrier only for
// runtime-package files and a fixed list of runtime functions, so a
// -lb build of package syscall does instrument forkAndExecInChild1.
// The bracket must therefore hold under instrumented code, which is
// why the token lives outside the child-shared frame rather than
// relying on that frame surviving the child untouched.
//
// Root inventory. Outer (max 15: 7 Value + 8 Whole) names forkExec's
// cells: Value over argv0, argv, envv, chroot, dir, attr, sys; Whole
// over argv0 bytes, argv/envv pointer and byte backings, chroot/dir
// bytes, attr.Files backing. Inner (max 37) names
// forkAndExecInChild1's cells: Value over its params/locals without
// heap-interior slots; Whole over argv0/argv/envv/chroot/dir/Files
// backings, cred.Groups via the sys Credential, caps backing, uid/gid
// mapping extents, clone3 struct, fd backing, six derived buffers.
//
// Standalone Exec (exec_unix.go) is covered by logosExecBegin/End with
// the same cell discipline: argv0/argv/envv plus their backings,
// Begin after conversion before runtime_BeforeExec, End after
// runtime_AfterExec. A successful execve never returns, so End runs
// only on the failure path; the replaced image owns nothing afterwards.

// logosForeignRootKind selects how Slot/Size describe the rooted memory.
// Order and width mirror runtime.logosForeignRootKind exactly.
type logosForeignRootKind uint32

const (
	logosForeignRootValue logosForeignRootKind = iota
	logosForeignRootWhole
	logosForeignRootUnknown
)

// logosForeignRoot is one admission descriptor. Field order and layout
// mirror runtime.logosForeignRoot exactly (Type, Slot, Size, Kind); the
// linkname call below depends on the word-for-word identity.
type logosForeignRoot struct {
	Type unsafe.Pointer
	Slot unsafe.Pointer
	Size uintptr
	Kind logosForeignRootKind
}

// logosForeignToken names one admission frame. Layout mirrors
// runtime.logosForeignToken exactly (Frame, Gen).
type logosForeignToken struct {
	Frame unsafe.Pointer
	Gen   uint64
}

// Layout pins against silent drift from the runtime twins. Offsets, not
// sizes: the word width differs between 32- and 64-bit builds.
var (
	_ [unsafe.Offsetof(logosForeignRoot{}.Slot) - unsafe.Sizeof(unsafe.Pointer(nil))]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Size) - 2*unsafe.Sizeof(unsafe.Pointer(nil))]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Kind) - 2*unsafe.Sizeof(unsafe.Pointer(nil)) - unsafe.Sizeof(uintptr(0))]struct{}
	_ [unsafe.Sizeof(logosForeignToken{}) - unsafe.Sizeof(unsafe.Pointer(nil)) - 8]struct{}
)

// logosForkExtent is one caller-decomposed backing for a type this file
// would otherwise have to name across build shapes: the extent base,
// its true size in bytes, and the element *_type. Empty (nil base or
// zero size) means nothing to pin and is skipped.
type logosForkExtent struct {
	Base unsafe.Pointer
	Size uintptr
	Elem unsafe.Pointer
}

// logosForkCells points at the actual cells the fork child reads. Every
// field is a pointer to the caller's cell (or a slot address plus the
// caller-computed type where the referent type is linux-only), so
// admission rewrites the cells the clone path reads.
type logosForkCells struct {
	Argv0, Chroot, Dir                                **byte
	Argv, Envv                                        *[]*byte
	Attr                                              **ProcAttr
	SysSlot                                           unsafe.Pointer // &sys cell
	SysType                                           unsafe.Pointer // *_type of *SysProcAttr
	SysCred                                           **Credential   // &sys.Credential field
	Cred                                              **Credential   // &cred local
	Caps                                              *[]uintptr     // &sys.AmbientCaps header
	UidMap                                            logosForkExtent
	GidMap                                            logosForkExtent
	CloneSlot                                         unsafe.Pointer // &clone3 cell
	CloneType                                         unsafe.Pointer // *_type of *cloneArgs
	CloneObj                                          logosForkExtent
	Rlim                                              **Rlimit
	Fd                                                *[]int
	Puid, Psetgroups, Pgid, Uidmap, Setgroups, Gidmap *[]byte
}

// logosEface mirrors the interface header to read a value's *_type word
// without importing reflect into this low-level package.
type logosEface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// logosTypeOf returns the *_type word of v's dynamic type: the
// authoritative layout word the registry walks. Safe on typed-nil
// pointers (the type word is still present); never dereferences v.
func logosTypeOf(v any) unsafe.Pointer {
	return (*logosEface)(unsafe.Pointer(&v)).typ
}

var logosByteType = logosTypeOf(byte(0))

// logosMaxCStrLen bounds the NUL scan. Conversions always append the
// terminator, so reaching the bound is impossible; hitting it throws
// rather than pinning a truncated extent.
const logosMaxCStrLen = 1 << 30

// logosCStrLen returns the length of the NUL-terminated string at p,
// not counting the terminator. The caller guarantees p != nil.
func logosCStrLen(p *byte) uintptr {
	// One byte at a time: the terminator always lies within the
	// referent allocation (conversions append it), so every load stays
	// in bounds. A single unsafe.Slice over logosMaxCStrLen would span
	// many allocations and trip checkptr.
	for i := uintptr(0); i < logosMaxCStrLen; i++ {
		if *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + i)) == 0 {
			return i
		}
	}
	panic("syscall: logos fork preflight reached unterminated string")
}

// logosCStrBytes returns the Whole extent (base, strlen+1) of the
// NUL-terminated string at p, or (nil, 0) for a nil pointer.
func logosCStrBytes(p *byte) (unsafe.Pointer, uintptr) {
	if p == nil {
		return nil, 0
	}
	return unsafe.Pointer(p), logosCStrLen(p) + 1
}

// logosByteBacking returns the packed byte backing extent of a
// SlicePtrFromStrings result: the first non-nil element base and the
// exact summed size. SlicePtrFromStrings packs elements contiguously
// in order, so the sum is the true extent. All-nil input yields
// (nil, 0): the child only reads the (separately rooted) pointer
// backing then.
func logosByteBacking(arr []*byte) (unsafe.Pointer, uintptr) {
	var base unsafe.Pointer
	var size uintptr
	for _, p := range arr {
		if p == nil {
			continue
		}
		if base == nil {
			base = unsafe.Pointer(p)
		}
		size += logosCStrLen(p) + 1
	}
	return base, size
}

// logosRootSet accumulates one admission's descriptors on the caller
// stack. The array fits every frame here (inner max 37, outer max 15).
const logosForkMaxRoots = 40

type logosRootSet struct {
	arr [logosForkMaxRoots]logosForeignRoot
	n   uintptr
}

// value roots one pointer variable cell. A nil slot is a caller bug
// and is skipped; a nil *slot* (typed-nil referent) is lawful and
// resolves to nothing.
func (s *logosRootSet) value(slot, typ unsafe.Pointer) {
	if slot == nil {
		return
	}
	s.arr[s.n].Type = typ
	s.arr[s.n].Slot = slot
	s.arr[s.n].Kind = logosForeignRootValue
	s.n++
}

// whole pins one extent with its true byte size and element type.
// Empty extents are skipped; Size must already be an exact multiple of
// the element size (callers pass len*elemSize).
func (s *logosRootSet) whole(base unsafe.Pointer, size uintptr, elem unsafe.Pointer) {
	if base == nil || size == 0 || elem == nil {
		return
	}
	s.arr[s.n].Type = elem
	s.arr[s.n].Slot = base
	s.arr[s.n].Size = size
	s.arr[s.n].Kind = logosForeignRootWhole
	s.n++
}

// byteSlice roots a []byte cell and its backing.
func (s *logosRootSet) byteSlice(sp *[]byte) {
	b := *sp
	s.value(unsafe.Pointer(sp), logosTypeOf(b))
	if len(b) > 0 {
		s.whole(unsafe.Pointer(&b[0]), uintptr(len(b)), logosTypeOf(b[0]))
	}
}

func (s *logosRootSet) cstrRoot(sp **byte) {
	s.value(unsafe.Pointer(sp), logosTypeOf(*sp))
	if b, sz := logosCStrBytes(*sp); b != nil {
		s.whole(b, sz, logosByteType)
	}
}

func (s *logosRootSet) strSlice(sp *[]*byte) {
	h := *sp
	s.value(unsafe.Pointer(sp), logosTypeOf(h))
	if len(h) == 0 {
		return
	}
	s.whole(unsafe.Pointer(&h[0]), uintptr(len(h))*unsafe.Sizeof(h[0]), logosTypeOf(h[0]))
	if b, sz := logosByteBacking(h); b != nil {
		s.whole(b, sz, logosByteType)
	}
}

func (s *logosRootSet) attrRoots(attr **ProcAttr) {
	at := *attr
	s.value(unsafe.Pointer(attr), logosTypeOf(at))
	if at != nil && len(at.Files) > 0 {
		s.whole(unsafe.Pointer(&at.Files[0]), uintptr(len(at.Files))*unsafe.Sizeof(at.Files[0]), logosTypeOf(at.Files[0]))
	}
}

//go:linkname logosZgcForeignBegin runtime.logosZgcForeignBegin
//go:noescape
func logosZgcForeignBegin(roots unsafe.Pointer, n uintptr) logosForeignToken

//go:linkname logosZgcForeignEnd runtime.logosZgcForeignEnd
func logosZgcForeignEnd(t logosForeignToken)

// logosForkBegin admits every cell the fork child reads. Call in
// forkAndExecInChild1 after all derived buffers exist and immediately
// before runtime_BeforeFork; pair the token with logosForkEnd after
// runtime_AfterFork in the parent. Never call in the child.
//
// The caller must store the returned token outside the vfork child's
// frame (forkAndExecInChild1 writes it through a pointer into
// forkAndExecInChild). A token kept in the child-shared frame can be
// overwritten by the child's own spills, leaving the parent's End with
// a Frame/Gen pair that never matched its Begin.
func logosForkBegin(c *logosForkCells) logosForeignToken {
	if c == nil {
		return logosForeignToken{}
	}
	var s logosRootSet

	s.cstrRoot(c.Argv0)
	s.strSlice(c.Argv)
	s.strSlice(c.Envv)
	s.cstrRoot(c.Chroot)
	s.cstrRoot(c.Dir)
	s.attrRoots(c.Attr)

	s.value(c.SysSlot, c.SysType)
	sc := *c.SysCred
	if sc != nil && len(sc.Groups) > 0 {
		s.whole(unsafe.Pointer(&sc.Groups[0]), uintptr(len(sc.Groups))*unsafe.Sizeof(sc.Groups[0]), logosTypeOf(sc.Groups[0]))
	}
	cr := *c.Cred
	s.value(unsafe.Pointer(c.Cred), logosTypeOf(cr))

	cp := *c.Caps
	if len(cp) > 0 {
		s.whole(unsafe.Pointer(&cp[0]), uintptr(len(cp))*unsafe.Sizeof(cp[0]), logosTypeOf(cp[0]))
	}

	s.whole(c.UidMap.Base, c.UidMap.Size, c.UidMap.Elem)
	s.whole(c.GidMap.Base, c.GidMap.Size, c.GidMap.Elem)

	s.value(c.CloneSlot, c.CloneType)
	s.whole(c.CloneObj.Base, c.CloneObj.Size, c.CloneObj.Elem)

	s.value(unsafe.Pointer(c.Rlim), logosTypeOf(*c.Rlim))

	fd := *c.Fd
	s.value(unsafe.Pointer(c.Fd), logosTypeOf(fd))
	if len(fd) > 0 {
		s.whole(unsafe.Pointer(&fd[0]), uintptr(len(fd))*unsafe.Sizeof(fd[0]), logosTypeOf(fd[0]))
	}

	s.byteSlice(c.Puid)
	s.byteSlice(c.Psetgroups)
	s.byteSlice(c.Pgid)
	s.byteSlice(c.Uidmap)
	s.byteSlice(c.Setgroups)
	s.byteSlice(c.Gidmap)

	return logosZgcForeignBegin(unsafe.Pointer(&s.arr[0]), s.n)
}

// logosForkEnd releases the fork admission. Call only in the parent,
// after runtime_AfterFork. An invalid token (cycle disabled) is a
// no-op.
func logosForkEnd(t logosForeignToken) {
	logosZgcForeignEnd(t)
}

type logosForkOuterCells struct {
	Argv0, Chroot, Dir **byte
	Argv, Envv         *[]*byte
	Attr               **ProcAttr
	SysSlot            unsafe.Pointer
	SysType            unsafe.Pointer
}

func logosForkOuterBegin(c *logosForkOuterCells) logosForeignToken {
	if c == nil {
		return logosForeignToken{}
	}
	var s logosRootSet

	s.cstrRoot(c.Argv0)
	s.strSlice(c.Argv)
	s.strSlice(c.Envv)
	s.cstrRoot(c.Chroot)
	s.cstrRoot(c.Dir)
	s.attrRoots(c.Attr)
	s.value(c.SysSlot, c.SysType)
	return logosZgcForeignBegin(unsafe.Pointer(&s.arr[0]), s.n)
}

func logosForkOuterEnd(t logosForeignToken) {
	logosZgcForeignEnd(t)
}

// logosExecBegin admits argv0/argv/envv and their backings for the
// standalone Exec path. Call after the string conversions and before
// runtime_BeforeExec; pair with logosExecEnd after runtime_AfterExec.
// On a successful execve the image is replaced and End never runs.
func logosExecBegin(argv0 **byte, argv, envv *[]*byte) logosForeignToken {
	var s logosRootSet

	s.cstrRoot(argv0)
	s.strSlice(argv)
	s.strSlice(envv)
	return logosZgcForeignBegin(unsafe.Pointer(&s.arr[0]), s.n)
}

// logosExecEnd releases the Exec admission after runtime_AfterExec.
func logosExecEnd(t logosForeignToken) {
	logosZgcForeignEnd(t)
}
