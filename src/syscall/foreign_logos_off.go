// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix && !(goexperiment.logosgcmoving && (amd64 || arm64))

package syscall

import "unsafe"

// Off-experiment fork/exec preflight: genuine no-op. Nothing relocates
// on this build, so no admission is ever taken and every token is
// invalid. Signatures mirror foreign_logos.go exactly so callers in
// exec_linux.go and exec_unix.go compile on both shapes.

type logosForeignRootKind uint32

const (
	logosForeignRootValue logosForeignRootKind = iota
	logosForeignRootWhole
	logosForeignRootUnknown
)

type logosForeignRoot struct {
	Type unsafe.Pointer
	Slot unsafe.Pointer
	Size uintptr
	Kind logosForeignRootKind
}

type logosForeignToken struct {
	Frame unsafe.Pointer
	Gen   uint64
}

var (
	_ [unsafe.Offsetof(logosForeignRoot{}.Slot) - unsafe.Sizeof(unsafe.Pointer(nil))]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Size) - 2*unsafe.Sizeof(unsafe.Pointer(nil))]struct{}
	_ [unsafe.Offsetof(logosForeignRoot{}.Kind) - 2*unsafe.Sizeof(unsafe.Pointer(nil)) - unsafe.Sizeof(uintptr(0))]struct{}
	_ [unsafe.Sizeof(logosForeignToken{}) - unsafe.Sizeof(unsafe.Pointer(nil)) - 8]struct{}
)

type logosForkExtent struct {
	Base unsafe.Pointer
	Size uintptr
	Elem unsafe.Pointer
}

type logosForkCells struct {
	Argv0, Chroot, Dir                                **byte
	Argv, Envv                                        *[]*byte
	Attr                                              **ProcAttr
	SysSlot                                           unsafe.Pointer
	SysType                                           unsafe.Pointer
	SysCred                                           **Credential
	Cred                                              **Credential
	Caps                                              *[]uintptr
	UidMap                                            logosForkExtent
	GidMap                                            logosForkExtent
	CloneSlot                                         unsafe.Pointer
	CloneType                                         unsafe.Pointer
	CloneObj                                          logosForkExtent
	Rlim                                              **Rlimit
	Fd                                                *[]int
	Puid, Psetgroups, Pgid, Uidmap, Setgroups, Gidmap *[]byte
}

type logosEface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

func logosTypeOf(v any) unsafe.Pointer {
	return (*logosEface)(unsafe.Pointer(&v)).typ
}

func logosForkBegin(c *logosForkCells) logosForeignToken {
	return logosForeignToken{}
}

func logosForkEnd(t logosForeignToken) {
}

type logosForkOuterCells struct {
	Argv0, Chroot, Dir **byte
	Argv, Envv         *[]*byte
	Attr               **ProcAttr
	SysSlot            unsafe.Pointer
	SysType            unsafe.Pointer
}

func logosForkOuterBegin(c *logosForkOuterCells) logosForeignToken {
	return logosForeignToken{}
}

func logosForkOuterEnd(t logosForeignToken) {
}

func logosExecBegin(argv0 **byte, argv, envv *[]*byte) logosForeignToken {
	return logosForeignToken{}
}

func logosExecEnd(t logosForeignToken) {
}
