// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/types"
	"internal/goarch"
	"testing"
)

// TestLogosArenaBumpOpContract pins contract C2 (M18-S3): the generic bump
// op exists with an Int64 aux and a tuple result, and it is an async
// preemption unsafe point so no goroutine switch can observe the
// half-updated chunk watermark (plive.go records v.Op.UnsafePoint();
// preempt.go rejects unsafe points).
func TestLogosArenaBumpOpContract(t *testing.T) {
	if !OpLogosArenaBump.UnsafePoint() {
		t.Fatal("OpLogosArenaBump.UnsafePoint() = false, want true")
	}
	info := opcodeTable[OpLogosArenaBump]
	if info.argLen != 2 {
		t.Errorf("OpLogosArenaBump argLen = %d, want 2 (chunk, mem)", info.argLen)
	}
	if info.auxType != auxInt64 {
		t.Errorf("OpLogosArenaBump auxType = %v, want auxInt64", info.auxType)
	}
	if !info.hasSideEffects {
		t.Error("OpLogosArenaBump hasSideEffects = false, want true")
	}
}

// TestLogosArenaBumpLowersAMD64 proves the AMD64 rewrite lowers the generic
// bump to LoweredLogosArenaBump, the op amd64/ssa.go emits as the inline
// add-compare sequence.
func TestLogosArenaBumpLowersAMD64(t *testing.T) {
	c := testConfig(t) // amd64
	bumpTyp := types.NewTuple(types.Types[types.TUNSAFEPTR], types.TypeMem)
	fun := c.Fun("entry",
		Bloc("entry",
			Valu("mem", OpInitMem, types.TypeMem, 0, nil),
			Valu("sp", OpSP, c.config.Types.Uintptr, 0, nil),
			Valu("chunk", OpOffPtr, c.config.Types.BytePtr, 0, nil, "sp"),
			Valu("bump", OpLogosArenaBump, bumpTyp, 16, nil, "chunk", "mem"),
			Valu("ptr", OpSelect0, types.Types[types.TUNSAFEPTR], 0, nil, "bump"),
			Valu("mem2", OpSelect1, types.TypeMem, 0, nil, "bump"),
			Ret("mem2")))
	CheckFunc(fun.f)
	lower(fun.f)
	CheckFunc(fun.f)
	checkOpcodeCounts(t, fun.f, map[Op]int{OpLogosArenaBump: 0, OpAMD64LoweredLogosArenaBump: 1})
	if !OpAMD64LoweredLogosArenaBump.UnsafePoint() {
		t.Fatal("OpAMD64LoweredLogosArenaBump.UnsafePoint() = false, want true")
	}
}

// TestLogosArenaGOffset pins the single named compiler-side offset of
// g.logosArena. The compiler has no g type when building user code and
// reaches g only through OpGetG, so the offset cannot be derived here;
// TestLogosArenaFieldOffset in src/runtime (S1) asserts
// unsafe.Offsetof(g{}.logosArena) equals this value and is the drift guard.
func TestLogosArenaGOffset(t *testing.T) {
	if logosArenaGOffset != 4*goarch.PtrSize {
		t.Fatalf("logosArenaGOffset = %d, want 4*goarch.PtrSize = %d", logosArenaGOffset, 4*goarch.PtrSize)
	}
}
