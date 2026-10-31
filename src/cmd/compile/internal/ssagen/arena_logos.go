// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/escape"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/sys"
	"go/constant"
	"internal/buildcfg"
)

// LogosArenaBumpMaxBytes caps the inline bump fast path: ssagen intrinsifies
// only rounded payloads at or below it, larger or non-constant sizes fall
// back to nil so walk's logosArenaAlloc slow call runs. Walk shares this
// single definition for its bump gate. It is distinct from
// escape.LogosArenaMaxBytes, which bounds chunk fit: keep the two limits
// separate.
const LogosArenaBumpMaxBytes = 512

// logosArenaBumpBuilder implements the runtime.logosArenaBump intrinsic on amd64.
// It emits OpLogosArenaBump with chunk and memory args fetched via OpGetG
// plus logosArenaGOffset, rounded size as auxint, returning nil when the
// watermark check fails.
func logosArenaBumpBuilder(s *state, n *ir.CallExpr, args []*ssa.Value) *ssa.Value {
	if !buildcfg.Experiment.LogosArena || Arch.LinkArch.Family != sys.AMD64 || base.Flag.Race {
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
	if size <= 0 || size > LogosArenaBumpMaxBytes {
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

// hasArenaBump reports whether fn's walked body contains a call to runtime.logosArenaBump,
// runtime.logosArenaAlloc, runtime.logosArenaMakeSlice, or runtime.logosArenaAllocScan.
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
				switch name.Sym().Name {
				case "logosArenaBump", "logosArenaAlloc", "logosArenaMakeSlice", "logosArenaAllocScan":
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
	if !buildcfg.Experiment.LogosArena || escape.LogosArenaFrameDeferred(s.curfn) || !hasArenaBump(s.curfn) {
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

// logosArenaLoopEligible mirrors the walk placement gate: experiment on,
// amd64 only. Walk lowers arena makes via runtime calls under race, so a
// race gate here would leak eligible loop allocations instead of restoring
// them. Cond/Post allocation safety is the escape owner's veto, not a
// second check here.
func logosArenaLoopEligible() bool {
	return buildcfg.Experiment.LogosArena && Arch.LinkArch.Family == sys.AMD64
}

// logosArenaLoopSave emits one mark-save at the top of bBody and records
// the three mark values under the loop node. The save dominates every
// in-body exit edge; the cond-false edge never passes through bBody, so it
// never observes an uninitialized mark. A second save for the same node is
// a no-op: the single static save already executes each dynamic iteration.
func (s *state) logosArenaLoopSave(loop ir.Node) {
	if !logosArenaLoopEligible() || loop == nil {
		return
	}
	if s.arenaLoopMarks == nil {
		s.arenaLoopMarks = make(map[ir.Node][3]*ssa.Value)
	} else if _, ok := s.arenaLoopMarks[loop]; ok {
		return
	}
	saveFn := typecheck.LookupRuntimeFunc("logosArenaMarkSave")
	res := s.rtcall(saveFn, true, []*types.Type{types.Types[types.TUINTPTR], types.Types[types.TUINTPTR], types.Types[types.TUINTPTR]})
	s.arenaLoopMarks[loop] = [3]*ssa.Value{res[0], res[1], res[2]}
}

// logosArenaLoopRestore emits a restore to the loop's saved mark. It never
// deletes the mark: multiple CFG edges (fallthrough, continue, break,
// labeled exits) share one lowering, and every edge needs the same
// dominating save values. Missing marks (ineligible loops in an exited
// suffix) restore nothing. Return and panic paths emit no loop restore;
// the synthetic frame defer restores the earlier frame mark last.
func (s *state) logosArenaLoopRestore(loop ir.Node) {
	if !logosArenaLoopEligible() || loop == nil {
		return
	}
	if s.arenaLoopMarks == nil {
		return
	}
	m, ok := s.arenaLoopMarks[loop]
	if !ok {
		return
	}
	restoreFn := typecheck.LookupRuntimeFunc("logosArenaMarkRestore")
	s.rtcall(restoreFn, true, nil, m[0], m[1], m[2])
}

// logosArenaLoopRestoreDepth restores the top-down suffix of the arena
// loop stack starting at depth, the index recorded when a labeled
// break/continue target was installed. Innermost first. Out-of-range
// depths restore nothing. Entries without a saved mark restore nothing
// via logosArenaLoopRestore. Callers emit this before the jump so the
// restore dominates the exit edge.
func (s *state) logosArenaLoopRestoreDepth(depth int) {
	if depth < 0 || depth > len(s.arenaLoopStack) {
		return
	}
	for i := len(s.arenaLoopStack) - 1; i >= depth; i-- {
		s.logosArenaLoopRestore(s.arenaLoopStack[i].loop)
	}
}
