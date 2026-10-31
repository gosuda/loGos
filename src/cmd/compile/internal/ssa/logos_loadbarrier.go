// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
)

// logosLoadBarrier inserts the zgc load barrier (SPEC Sec 8.4, Sec 19.2)
// on every pointer load under -lb. A load
//
//	p = Load <*T> slot mem
//
// becomes
//
//	w    = Load <uintptr> slot mem
//	if (w ^ runtime.zgcGoodMask) & runtime.zgcColorMaskBits != 0 {
//		r, mem' = runtime.zgcLoadBarrierSlow(slot, w)
//	}
//	p    = Phi <*T> (w & runtime.zgcUnmaskBits, r)
//	mem" = Phi (mem, mem')
//
// The fast path tests the exact good color and unmasks the color bits
// with one AND; the slow path heals the slot and returns the canonical
// address. While zgc is not active the color mask is zero and the unmask
// word is all ones, so the barrier never calls out and returns the loaded
// word unchanged. The raw word is typed uintptr until the Phi: a colored
// word is not a heap address and must not enter a stack map.
//
// Pointer-typed results of atomic loads (Select0 of AtomicLoadPtr) take
// the same barrier. Loads from read-only data hold no colored words and
// stay plain.
func logosLoadBarrier(f *Func) {
	if !base.LogosLoadBarrier {
		return
	}
	if f.LogosNoLB {
		return
	}
	lb := &loadBarrier{f: f}
	var work []*Block
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if lb.slotOf(v) != nil {
				work = append(work, b)
				break
			}
		}
	}
	if len(work) == 0 {
		return
	}
	if !(f.ABIDefault == f.ABI1 && len(f.Config.intParamRegs) >= 2) || f.Config.PtrSize != 8 {
		f.Fatalf("-lb needs the register ABI on a 64-bit target")
	}
	lb.init()
	for len(work) > 0 {
		b := work[0]
		work = work[1:]
		if tail := lb.split(b); tail != nil {
			work = append(work, b, tail)
		}
	}
	f.invalidateCFG()
	repairMemory(f)
}

// loadBarrier holds the state of one logosLoadBarrier run.
type loadBarrier struct {
	f                    *Func
	slow                 *obj.LSym
	good, colors, unmask *obj.LSym
	sb                   *Value
}

func (lb *loadBarrier) init() {
	lb.slow = base.PkgLinksym("runtime", "zgcLoadBarrierSlow", obj.ABIInternal)
	lb.good = base.PkgLinksym("runtime", "zgcGoodMask", obj.ABI0)
	lb.colors = base.PkgLinksym("runtime", "zgcColorMaskBits", obj.ABI0)
	lb.unmask = base.PkgLinksym("runtime", "zgcUnmaskBits", obj.ABI0)
	_, lb.sb = lb.f.spSb()
}

// slotOf returns the address v loads a pointer from when v needs a load
// barrier, or nil.
func (lb *loadBarrier) slotOf(v *Value) *Value {
	var slot *Value
	switch v.Op {
	case OpLoad:
		slot = v.Args[0]
	case OpSelect0:
		if v.Args[0].Op != OpAtomicLoadPtr {
			return nil
		}
		slot = v.Args[0].Args[0]
	default:
		return nil
	}
	if !v.Type.IsPtrShaped() || !v.Type.HasPointers() || IsReadOnlyGlobalAddr(slot) {
		return nil
	}
	return slot
}

// memOf returns the memory state the pointer load v reads: the memory
// argument of a Load, or the memory result of the atomic load, which it
// creates when no value uses it yet.
func (lb *loadBarrier) memOf(v *Value) *Value {
	if v.Op == OpLoad {
		return v.Args[1]
	}
	a := v.Args[0]
	for _, w := range v.Block.Values {
		if w.Op == OpSelect1 && w.Args[0] == a {
			return w
		}
	}
	return v.Block.NewValue1(a.Pos, OpSelect1, types.TypeMem, a)
}

// memPrev returns the memory state memory value w follows, or nil.
func memPrev(w *Value) *Value {
	if m := w.MemoryArg(); m != nil {
		return m
	}
	if (w.Op == OpSelect1 || w.Op == OpSelectN) && len(w.Args) == 1 {
		return w.Args[0].MemoryArg()
	}
	return nil
}

// memRank numbers the memory values of b along its memory chain, from 1;
// a memory value of another block ranks 0.
func memRank(b *Block) map[*Value]int {
	rank := make(map[*Value]int)
	var of func(m *Value) int
	of = func(m *Value) int {
		if m == nil || m.Block != b {
			return 0
		}
		if r, ok := rank[m]; ok {
			return r
		}
		r := 1
		if m.Op != OpPhi && m.Op != OpInitMem {
			r = of(memPrev(m)) + 1
		}
		rank[m] = r
		return r
	}
	for _, v := range b.Values {
		if v.Type.IsMemory() {
			of(v)
		}
	}
	return rank
}

// firstLoad returns the pointer load of b that reads the earliest memory
// state of b and that memory state, or nil.
func (lb *loadBarrier) firstLoad(b *Block) (load, mem *Value) {
	rank := memRank(b)
	best := -1
	for _, v := range b.Values {
		if lb.slotOf(v) == nil {
			continue
		}
		m := lb.memOf(v)
		if r := rank[m]; best < 0 || r < best {
			load, mem, best = v, m, r
		}
	}
	for load != nil {
		up := lb.loadAbove(load, mem)
		if up == nil {
			return load, mem
		}
		load = up
	}
	return nil, nil
}

// loadAbove returns a pointer load of mem in the block of v that v
// transitively depends on, as *(*pp) depends on *pp, or nil. Its barrier
// must come first, because the loads of mem all move after the barrier.
func (lb *loadBarrier) loadAbove(v, mem *Value) *Value {
	seen := make(map[*Value]bool)
	var walk func(w *Value) *Value
	walk = func(w *Value) *Value {
		for _, a := range w.Args {
			if a.Block != v.Block || a.Op == OpPhi || seen[a] {
				continue
			}
			seen[a] = true
			if lb.slotOf(a) != nil && lb.memOf(a) == mem {
				return a
			}
			if up := walk(a); up != nil {
				return up
			}
		}
		return nil
	}
	return walk(v)
}

// startsAfter reports whether v must run after the barrier of a load of
// mem: v is a pointer load of mem, or v consumes mem to make the next
// memory state (a store, a call, an atomic).
func (lb *loadBarrier) startsAfter(v, mem *Value) bool {
	if v.Op == OpPhi || v == mem {
		return false
	}
	if lb.slotOf(v) != nil {
		return lb.memOf(v) == mem
	}
	t := v.Type
	if t.IsMemory() {
		return memPrev(v) == mem
	}
	return (t.IsTuple() || t.IsResults()) && v.MemoryArg() == mem
}

// partition splits the values of b other than load into those that run
// before its barrier and those that run after it: the values that
// transitively depend on a value that starts after it.
func (lb *loadBarrier) partition(b *Block, load, mem *Value) (pre, post []*Value) {
	isAfter := make(map[*Value]bool)
	var visit func(v *Value) bool
	visit = func(v *Value) bool {
		if v.Block != b || v.Op == OpPhi {
			return false
		}
		if res, ok := isAfter[v]; ok {
			return res
		}
		isAfter[v] = false
		res := lb.startsAfter(v, mem)
		for _, a := range v.Args {
			res = visit(a) || res
		}
		isAfter[v] = res
		return res
	}
	for _, v := range b.Values {
		switch {
		case v == load:
		case visit(v):
			post = append(post, v)
		default:
			pre = append(pre, v)
		}
	}
	return pre, post
}

// split inserts the barrier of one pointer load of b. It picks the load
// that reads the earliest memory state of b, so that b keeps no other
// load that needs a barrier: every value that depends on the chosen load,
// on a load of the same memory, or on a later memory state of b moves to
// a new block after the barrier. It returns that block, or nil when b has
// no load left.
//
// The raw word is typed uintptr until the Phi, because a colored word is
// not a heap address and must not enter a stack map. The slot argument of
// the slow call stays pointer-typed, so a stack slot is adjusted when the
// callee grows the stack. The load itself becomes the value Phi, so every
// use of it, in this block or in a dominated one, reads the result.
func (lb *loadBarrier) split(b *Block) *Block {
	f := lb.f
	load, mem := lb.firstLoad(b)
	if load == nil {
		return nil
	}
	pre, post := lb.partition(b, load, mem)

	pos := load.Pos
	typ := &f.Config.Types
	slot := lb.slotOf(load)

	bThen := f.NewBlock(BlockPlain)
	bEnd := f.NewBlock(b.Kind)
	bThen.Pos = pos
	bEnd.Pos = b.Pos
	bEnd.CopyControls(b)
	bEnd.Likely = b.Likely
	bEnd.Aux, b.Aux = b.Aux, nil
	for _, e := range b.Succs {
		bEnd.Succs = append(bEnd.Succs, e)
		e.b.Preds[e.i].b = bEnd
	}
	b.Values = pre

	var word *Value
	if load.Op == OpLoad {
		word = b.NewValue2(pos, OpLoad, typ.Uintptr, slot, mem)
	} else {
		word = b.NewValue1(pos, OpSelect0, typ.Uintptr, load.Args[0])
	}
	global := func(s *obj.LSym) *Value {
		addr := b.NewValue1A(pos, OpAddr, typ.UintptrPtr, s, lb.sb)
		return b.NewValue2(pos, OpLoad, typ.Uintptr, addr, mem)
	}
	diff := b.NewValue2(pos, OpXor64, typ.Uintptr, word, global(lb.good))
	bad := b.NewValue2(pos, OpAnd64, typ.Uintptr, diff, global(lb.colors))
	cond := b.NewValue2(pos, OpNeq64, typ.Bool, bad, f.ConstInt64(typ.UInt64, 0))
	fast := b.NewValue2(pos, OpAnd64, typ.Uintptr, word, global(lb.unmask))
	b.Kind = BlockIf
	b.ResetControls()
	b.SetControl(cond)
	b.Likely = BranchUnlikely
	b.Succs = b.Succs[:0]
	b.AddEdgeTo(bThen)
	b.AddEdgeTo(bEnd)
	bThen.AddEdgeTo(bEnd)

	argTypes := []*types.Type{typ.UintptrPtr, typ.Uintptr}
	resTypes := []*types.Type{typ.Uintptr}
	aux := StaticAuxCall(lb.slow, f.ABIDefault.ABIAnalyzeTypes(argTypes, resTypes))
	call := bThen.NewValue0A(pos, OpStaticCall, types.NewResults([]*types.Type{typ.Uintptr, types.TypeMem}), aux)
	call.AddArgs(slot, word, mem)
	call.AuxInt = aux.ArgWidth()
	healed := bThen.NewValue1I(pos, OpSelectN, typ.Uintptr, 0, call)
	callMem := bThen.NewValue1I(pos, OpSelectN, types.TypeMem, 1, call)

	bEnd.NewValue2(pos, OpPhi, types.TypeMem, mem, callMem)
	load.reset(OpPhi)
	load.AddArg2(fast, healed)
	load.Block = bEnd
	bEnd.Values = append(bEnd.Values, load)
	bEnd.Values = append(bEnd.Values, post...)
	for _, v := range post {
		v.Block = bEnd
	}
	return bEnd
}

// repairMemory restores the single memory chain after the splits: a
// block reads, at entry, the memory its predecessors end with, through a
// memory Phi when they end with different states. A split adds such a
// state, the Phi after the slow call, so blocks that merge a path through
// it with another path gain a Phi, and every use of the old state that
// the new one now reaches is renamed. The fixed point runs over blocks in
// reverse postorder, those the postorder misses first. A block gains a Phi
// only when predecessors that already have a state disagree, so a block
// visited before its predecessors, or one no state reaches, never gains
// one. It leaves a correct function unchanged.
func repairMemory(f *Func) {
	n := f.NumBlocks()
	start, end, last, phi := make([]*Value, n), make([]*Value, n), make([]*Value, n), make([]*Value, n)
	var initMem *Value
	for _, b := range f.Blocks {
		last[b.ID] = lastMemory(b)
		for _, v := range b.Values {
			switch {
			case v.Op == OpInitMem:
				initMem = v
			case v.Op == OpPhi && v.Type == types.TypeMem:
				phi[b.ID] = v
			}
		}
	}
	po := f.postorder()
	order := po
	seen := make([]bool, n)
	for _, b := range po {
		seen[b.ID] = true
	}
	for _, b := range f.Blocks {
		if !seen[b.ID] {
			order = append(order, b)
		}
	}
	for changed := true; changed; {
		changed = false
		for i := len(order) - 1; i >= 0; i-- {
			b := order[i]
			s := entryMemory(b, initMem, phi, end)
			if s == nil && somePredEnd(b, end) {
				s = b.NewValue0(b.Pos, OpPhi, types.TypeMem)
				phi[b.ID] = s
			}
			e := last[b.ID]
			if e == nil {
				e = s
			}
			if s != start[b.ID] || e != end[b.ID] {
				start[b.ID], end[b.ID], changed = s, e, true
			}
		}
	}
	for _, b := range order {
		renameMemory(b, start[b.ID], end, initMem)
	}
}

// somePredEnd reports whether any predecessor of b ends with a memory state.
func somePredEnd(b *Block, end []*Value) bool {
	for _, e := range b.Preds {
		if end[e.b.ID] != nil {
			return true
		}
	}
	return false
}

// entryMemory returns the memory state b starts with: InitMem in the
// entry block, the memory Phi of b, or the one state its predecessors
// end with. It returns nil when the predecessors disagree, so b needs a
// Phi.
func entryMemory(b *Block, initMem *Value, phi, end []*Value) *Value {
	if b == b.Func.Entry {
		return initMem
	}
	if p := phi[b.ID]; p != nil {
		return p
	}
	var s *Value
	for _, e := range b.Preds {
		m := end[e.b.ID]
		switch {
		case m == nil:
		case s == nil:
			s = m
		case s != m:
			return nil
		}
	}
	return s
}

// lastMemory returns the memory state b ends with when b makes one, or nil:
// the state no other memory operation of b consumes, else the memory Phi
// of b. The Phi does not count as consumed while the first operation of b
// still names the state of a predecessor, which renameMemory fixes later.
func lastMemory(b *Block) *Value {
	consumed := make(map[*Value]bool)
	for _, v := range b.Values {
		if v.Op != OpPhi && v.Type.IsMemory() {
			if m := v.MemoryArg(); m != nil {
				consumed[m] = true
			}
		}
	}
	var phi *Value
	for _, v := range b.Values {
		switch {
		case v.Type != types.TypeMem || consumed[v]:
		case v.Op == OpPhi:
			phi = v
		default:
			return v
		}
	}
	return phi
}

// renameMemory makes the values and controls of b that read a memory state
// of another block read start, the state b begins with, and sets the
// arguments of the memory Phi of b to the states its predecessors end with.
// A nil start means b is dead: its values already name valid states, so it
// is left alone.
func renameMemory(b *Block, start *Value, end []*Value, initMem *Value) {
	if start == nil {
		return
	}
	for _, v := range b.Values {
		switch {
		case v.Op != OpPhi:
			renameReads(v, start)
		case v.Type == types.TypeMem:
			setMemoryPhi(v, end, initMem)
		}
	}
	for i, c := range b.ControlValues() {
		if c.Type == types.TypeMem && c.Block != b && c != start {
			b.ReplaceControl(i, start)
		}
	}
}

// renameReads makes v read start instead of a memory state of another block.
func renameReads(v *Value, start *Value) {
	for i, a := range v.Args {
		if a.Type == types.TypeMem && a.Block != v.Block && a != start {
			v.SetArg(i, start)
		}
	}
}

// setMemoryPhi sets the arguments of the memory Phi v to the states the
// predecessors of its block end with. A predecessor that ends with no
// state is dead and its edge never runs, so v keeps its argument, or takes
// initMem when v is new and has none.
func setMemoryPhi(v *Value, end []*Value, initMem *Value) {
	for i, e := range v.Block.Preds {
		switch m := end[e.b.ID]; {
		case m != nil && i < len(v.Args):
			v.SetArg(i, m)
		case m != nil:
			v.AddArg(m)
		case i >= len(v.Args):
			v.AddArg(initMem)
		}
	}
}
