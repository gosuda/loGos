// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/escape"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/sys"
	"go/constant"
	"internal/buildcfg"
)

// logosArenaBumpBuilder implements the runtime.logosArenaBump intrinsic on amd64.
// It emits OpLogosArenaBump with chunk and memory args fetched via OpGetG
// plus logosArenaGOffset, rounded size as auxint, returning nil when the
// watermark check fails.
func logosArenaBumpBuilder(s *state, n *ir.CallExpr, args []*ssa.Value) *ssa.Value {
	if !buildcfg.Experiment.LogosArena || Arch.LinkArch.Family != sys.AMD64 {
		return s.constNil(types.Types[types.TUNSAFEPTR])
	}
	var size int64
	if len(args) > 0 && (args[0].Op == ssa.OpConst64 || args[0].Op == ssa.OpConst32) {
		size = args[0].AuxInt
	} else if len(n.Args) > 0 && ir.IsConst(n.Args[0], constant.Int) {
		size = ir.Int64Val(n.Args[0])
	} else {
		return s.constNil(types.Types[types.TUNSAFEPTR])
	}
	if size <= 0 || size > escape.LogosArenaMaxBytes {
		return s.constNil(types.Types[types.TUNSAFEPTR])
	}
	rnd := (size + 7) &^ 7

	mem := s.mem()
	g := s.newValue1(ssa.OpGetG, s.f.Config.Types.BytePtr, mem)
	offset := int64(ssa.LogosArenaGOffset)
	chunkAddr := s.newValue1I(ssa.OpOffPtr, types.NewPtr(s.f.Config.Types.BytePtr), offset, g)
	chunk := s.newValue2(ssa.OpLoad, s.f.Config.Types.BytePtr, chunkAddr, mem)

	bumpTyp := types.NewTuple(types.Types[types.TUNSAFEPTR], types.TypeMem)
	bump := s.newValue2I(ssa.OpLogosArenaBump, bumpTyp, rnd, chunk, mem)
	ptr := s.newValue1(ssa.OpSelect0, types.Types[types.TUNSAFEPTR], bump)
	newMem := s.newValue1(ssa.OpSelect1, types.TypeMem, bump)
	s.vars[memVar] = newMem
	return ptr
}

// hasArenaBump reports whether fn's walked body contains a call to runtime.logosArenaBump
// or runtime.logosArenaAlloc.
func hasArenaBump(fn *ir.Func) bool {
	if fn == nil {
		return false
	}
	found := false
	ir.VisitList(fn.Body, func(n ir.Node) {
		if found {
			return
		}
		if call, ok := n.(*ir.CallExpr); ok {
			if name, ok := call.Fun.(*ir.Name); ok && name.Sym() != nil && name.Sym().Pkg == ir.Pkgs.Runtime {
				if name.Sym().Name == "logosArenaBump" || name.Sym().Name == "logosArenaAlloc" {
					found = true
				}
			}
		}
	})
	return found
}

// logosRegisterArena registers the logosArenaBump intrinsic.
func logosRegisterArena(add func(string, string, intrinsicBuilder, ...*sys.Arch)) {
	add("runtime", "logosArenaBump", logosArenaBumpBuilder, sys.ArchAMD64)
}

func (s *state) logosArenaFrameEnter() {
	if !buildcfg.Experiment.LogosArena || !hasArenaBump(s.curfn) {
		return
	}
	saveFn := typecheck.LookupRuntimeFunc("logosArenaMarkSave")
	res := s.rtcall(saveFn, true, []*types.Type{types.Types[types.TUINTPTR], types.Types[types.TUINTPTR], types.Types[types.TUINTPTR]})
	s.arenaMarkChunk = res[0]
	s.arenaMarkNoscan = res[1]
	s.arenaMarkScan = res[2]
	s.hasArenaSave = true
}

func (s *state) logosArenaFrameExit() {
	if !s.hasArenaSave {
		return
	}
	restoreFn := typecheck.LookupRuntimeFunc("logosArenaMarkRestore")
	s.rtcall(restoreFn, true, nil, s.arenaMarkChunk, s.arenaMarkNoscan, s.arenaMarkScan)
}
