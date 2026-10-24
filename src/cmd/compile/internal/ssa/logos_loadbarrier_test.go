// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"cmd/compile/internal/base"
	"cmd/compile/internal/types"
)

// A zgc load barrier splits a block and adds a memory state, the Phi after
// the slow call. Every memory operation that follows the load must read that
// state, on every path, through the memory Phis of the blocks it merges
// with. These tests pin that chain at the level where logosLoadBarrier and
// repairMemory work, on SSA built with the Fun fixtures.
//
// No test compares the result with an expected block layout. The memory
// check runs the function: memoryChainProblems walks every path from the
// entry, keeps the memory state in effect, and requires each memory
// operation to read exactly that state and each memory Phi to take, over
// each edge, the state the predecessor ended with. A use of a state that a
// barrier has superseded fails that rule in whatever order the pass builds
// its blocks. The dependency checks (memoryAlways, memoryAfter) then state
// which barrier a later operation must follow.

// lbCase is a function the tests build fresh for every run, with the check
// of the memory dependencies that must hold once the pass has run.
type lbCase struct {
	name  string
	build func(c *Conf) fun
	check func(t *testing.T, fn fun)
}

// lbCases are valid functions: before the pass each one has a single memory
// chain. Those with pointer loads gain barriers; the others must pass
// through unchanged.
var lbCases = []lbCase{
	{"straight", lbStraight, checkStraight},
	{"entry diamond", lbEntryDiamond, checkEntryDiamond},
	{"loop header", lbLoopHeader, checkLoopHeader},
	{"loop body", lbLoopBody, checkLoopBody},
	{"join one arm", lbJoinOneArm, checkJoin("lp")},
	{"join both arms", lbJoinBothArms, checkJoin("lp", "rp")},
	{"join dead predecessor", lbJoinDeadPred, checkJoinDeadPred},
	{"chain", lbChain, checkChain},
	{"atomic", lbAtomic, checkAtomic},
	{"dead block", lbDead, checkDead},
	{"stores only", lbStraightStores, nil},
	{"quiet diamond", lbQuietDiamond, nil},
	{"diamond phi", lbDiamondPhi, nil},
	{"quiet loop", lbQuietLoop, nil},
	{"quiet nested loops", lbQuietNested, nil},
	{"plain loads", lbPlainLoads, nil},
}

// lbStaleCases read a memory state that an earlier block has superseded, as
// the blocks after a split do until repairMemory renames their uses.
var lbStaleCases = []lbCase{
	{"diamond", lbStaleDiamond, checkStaleDiamond},
	{"diamond dead predecessor", lbStaleDiamondDeadPred, checkStaleDiamond},
	{"loop", lbStaleLoop, checkStaleLoop},
	{"nested loops", lbStaleNested, checkStaleNested},
}

// TestLogosLoadBarrierMemory runs the whole pass. Afterwards the function
// must be well formed, keep one memory chain on every path, hold no pointer
// load without its barrier, and keep every later memory operation behind the
// barrier of the load it followed.
func TestLogosLoadBarrierMemory(t *testing.T) {
	for _, tc := range lbCases {
		t.Run(tc.name, func(t *testing.T) {
			useLoadBarrier(t, true)
			fn := tc.build(testConfig(t))
			f := fn.f
			CheckFunc(f)
			if probs := memoryChainProblems(f); len(probs) != 0 {
				t.Fatalf("the fixture is not a valid function: %s", strings.Join(probs, "; "))
			}
			want := len(pointerLoads(f))
			if !tryRun(t, "logosLoadBarrier", func() { logosLoadBarrier(f) }) {
				return
			}
			CheckFunc(f)
			for _, p := range memoryChainProblems(f) {
				t.Errorf("memory chain after the barriers: %s", p)
			}
			if left := pointerLoads(f); len(left) != 0 {
				t.Errorf("%d pointer loads keep no barrier: %v", len(left), left)
			}
			if got := slowCalls(f); got != want {
				t.Errorf("%d slow-path calls for %d pointer loads", got, want)
			}
			if tc.check != nil {
				tc.check(t, fn)
			}
		})
	}
}

// TestLogosLoadBarrierRepairKeepsValid runs repairMemory on valid functions
// under every block order it can meet: the postorder, no postorder at all,
// reversed blocks, and a postorder that lacks one live block. A function
// that already has one memory chain must come out unchanged, whatever block
// the walk meets first.
func TestLogosLoadBarrierRepairKeepsValid(t *testing.T) {
	for _, tc := range lbCases {
		t.Run(tc.name, func(t *testing.T) {
			for mode := 0; ; mode++ {
				f := tc.build(testConfig(t)).f
				CheckFunc(f)
				desc := repairOrder(f, mode)
				if desc == "" {
					return
				}
				before := dumpFunc(f)
				if !tryRun(t, desc+": repairMemory", func() { repairMemory(f) }) {
					return
				}
				if after := dumpFunc(f); after != before {
					t.Errorf("%s: repairMemory changed a valid function\nbefore:\n%s\nafter:\n%s", desc, before, after)
				}
			}
		})
	}
}

// TestLogosLoadBarrierRepairStale runs repairMemory on functions whose uses
// name a superseded state. Under every block order the result must be well
// formed, keep one memory chain on every path, and carry the new state to
// the blocks that merge it, loop heads included. The memory check must
// reject the input, or a repair could not be told from no repair.
func TestLogosLoadBarrierRepairStale(t *testing.T) {
	for _, tc := range lbStaleCases {
		t.Run(tc.name, func(t *testing.T) {
			if probs := memoryChainProblems(tc.build(testConfig(t)).f); len(probs) == 0 {
				t.Fatal("the memory check accepts the stale function")
			}
			for mode := 0; ; mode++ {
				fn := tc.build(testConfig(t))
				f := fn.f
				desc := repairOrder(f, mode)
				if desc == "" {
					return
				}
				if !tryRun(t, desc+": repairMemory", func() { repairMemory(f) }) {
					return
				}
				f.invalidateCFG()
				CheckFunc(f)
				for _, p := range memoryChainProblems(f) {
					t.Errorf("%s: %s", desc, p)
				}
				t.Logf("block order: %s", desc)
				tc.check(t, fn)
				repaired := dumpFunc(f)
				repairMemory(f)
				if again := dumpFunc(f); again != repaired {
					t.Errorf("%s: a second repair changed the repaired function\nfirst:\n%s\nsecond:\n%s", desc, repaired, again)
				}
			}
		})
	}
}

// TestLogosLoadBarrierIneligible checks that the pass leaves a function
// alone when -lb is off, and when no load needs a barrier: a load of a
// non-pointer word, and a pointer load from read-only data.
func TestLogosLoadBarrierIneligible(t *testing.T) {
	t.Run("flag off", func(t *testing.T) {
		useLoadBarrier(t, false)
		f := lbStraight(testConfig(t)).f
		before := dumpFunc(f)
		logosLoadBarrier(f)
		if after := dumpFunc(f); after != before {
			t.Errorf("the pass changed the function with -lb off\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
	t.Run("no eligible load", func(t *testing.T) {
		useLoadBarrier(t, true)
		f := lbPlainLoads(testConfig(t)).f
		if n := len(pointerLoads(f)); n != 0 {
			t.Fatalf("the fixture has %d pointer loads that need a barrier", n)
		}
		before := dumpFunc(f)
		logosLoadBarrier(f)
		if after := dumpFunc(f); after != before {
			t.Errorf("the pass changed a function without eligible loads\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
}

// useLoadBarrier sets the -lb flag until the test ends.
func useLoadBarrier(t *testing.T, on bool) {
	t.Helper()
	was := base.LogosLoadBarrier
	base.LogosLoadBarrier = on
	t.Cleanup(func() { base.LogosLoadBarrier = was })
}

// tryRun calls run and reports a panic as a test failure. It reports
// whether run returned normally.
func tryRun(t *testing.T, what string, run func()) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", what, r)
			ok = false
		}
	}()
	run()
	return true
}

// repairOrder arranges f so that repairMemory meets its blocks in the order
// of the given mode, and describes it. Mode 0 keeps the postorder, mode 1
// leaves the cached postorder empty so that every block is missing, mode 2
// also reverses the blocks of f, and each later mode drops one block from
// the postorder. It returns "" when mode is past the last order.
func repairOrder(f *Func, mode int) string {
	po := slices.Clone(f.postorder())
	switch {
	case mode == 0:
		return "postorder"
	case mode == 1:
		f.cachedPostorder = []*Block{}
		return "no postorder"
	case mode == 2:
		f.cachedPostorder = []*Block{}
		slices.Reverse(f.Blocks)
		return "no postorder, blocks reversed"
	case mode-3 < len(po):
		k := mode - 3
		desc := fmt.Sprintf("postorder without %v", po[k])
		f.cachedPostorder = slices.Delete(po, k, k+1)
		return desc
	}
	return ""
}

// dumpFunc lists every block and value of f with its arguments and use
// count, in the order f holds them.
func dumpFunc(f *Func) string {
	var sb strings.Builder
	for _, b := range f.Blocks {
		fmt.Fprintf(&sb, "%v %s preds=%v\n", b, b.LongString(), b.Preds)
		for _, v := range b.Values {
			fmt.Fprintf(&sb, "\t%s uses=%d\n", v.LongString(), v.Uses)
		}
	}
	return sb.String()
}

// memIn returns the memory state v reads, or nil. A projection of a tuple
// reads the tuple, not a state.
func memIn(v *Value) *Value {
	if n := len(v.Args); n > 0 && v.Args[n-1].Type == types.TypeMem {
		return v.Args[n-1]
	}
	return nil
}

// memProjection returns the tuple or results value whose memory result v
// selects, or nil.
func memProjection(v *Value) *Value {
	if v.Type == types.TypeMem && len(v.Args) == 1 && v.Args[0].Type.IsMemory() && v.Args[0].Type != types.TypeMem {
		return v.Args[0]
	}
	return nil
}

// memPhiOf returns the memory Phi of b, or nil.
func memPhiOf(b *Block) *Value {
	for _, v := range b.Values {
		if v.Op == OpPhi && v.Type == types.TypeMem {
			return v
		}
	}
	return nil
}

// memWalk is the state of memoryChainProblems.
type memWalk struct {
	problems []string
	seen     map[string]bool
	visits   map[*Block]int
	steps    int
}

func (w *memWalk) report(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !w.seen[msg] {
		w.seen[msg] = true
		w.problems = append(w.problems, msg)
	}
}

// memoryChainProblems executes the memory of f along every path from the
// entry and returns what breaks the single chain:
//
//   - a memory Phi that takes, over an edge, a state other than the one the
//     predecessor ends with;
//   - a memory operation that reads a state other than the one in effect
//     where it runs, which is how a stale use shows;
//   - two operations that both write over the same state;
//   - a block control that is not the state the block ends with.
//
// A block runs at most twice on a path, so each loop edge is taken once.
// Dead blocks are not on any path and are not checked.
func memoryChainProblems(f *Func) []string {
	var initMem *Value
	for _, v := range f.Entry.Values {
		if v.Op == OpInitMem {
			initMem = v
		}
	}
	if initMem == nil {
		return []string{"the entry block has no InitMem"}
	}
	w := &memWalk{seen: make(map[string]bool), visits: make(map[*Block]int)}
	w.enter(f.Entry, -1, initMem)
	return w.problems
}

// enter runs b after control came over the edge that is argument number edge
// of its Phis, with cur the state the predecessor ended with. The entry
// block has no such edge: edge is -1.
func (w *memWalk) enter(b *Block, edge int, cur *Value) {
	if w.visits[b] >= 2 {
		return
	}
	if w.steps++; w.steps > 100000 {
		w.report("the walk exceeds 100000 steps")
		return
	}
	w.visits[b]++
	defer func() { w.visits[b]-- }()

	if edge >= 0 {
		var phi *Value
		for _, v := range b.Values {
			if v.Op != OpPhi || v.Type != types.TypeMem {
				continue
			}
			if phi != nil {
				w.report("%v has two memory phis, %v and %v", b, phi, v)
			}
			phi = v
		}
		if phi != nil {
			switch {
			case edge >= len(phi.Args):
				w.report("%v has no argument for edge %d of %v", phi, edge, b)
				return
			case phi.Args[edge] != cur:
				w.report("%v takes %v over edge %d, but the predecessor ends with %v", phi, phi.Args[edge], edge, cur)
			}
			cur = phi
		}
	}
	end, ok := w.run(b, cur)
	if !ok {
		return
	}
	for _, c := range b.ControlValues() {
		if c.Type == types.TypeMem && c != end {
			w.report("%v ends with %v, but its control is %v", b, end, c)
		}
	}
	for _, e := range b.Succs {
		w.enter(e.b, e.i, end)
	}
}

// run executes the memory operations of b from the state cur and returns
// the state b ends with. Operations that only read the state run before the
// one that writes over it, the order the scheduler gives them. A tuple or
// results value writes through its memory projection.
func (w *memWalk) run(b *Block, cur *Value) (*Value, bool) {
	projection := make(map[*Value]*Value)
	var pending []*Value
	for _, v := range b.Values {
		if tuple := memProjection(v); tuple != nil && tuple.Block == b {
			projection[tuple] = v
		}
		if v.Op != OpPhi && memIn(v) != nil {
			pending = append(pending, v)
		}
	}
	writes := func(v *Value) bool {
		return v.Type == types.TypeMem || (v.Type.IsMemory() && projection[v] != nil)
	}
	for len(pending) > 0 {
		var rest, writers []*Value
		progress := false
		for _, v := range pending {
			switch {
			case memIn(v) != cur:
				rest = append(rest, v)
			case writes(v):
				writers = append(writers, v)
			default:
				progress = true
			}
		}
		if len(writers) > 1 {
			w.report("%v and %v both write over %v in %v", writers[0], writers[1], cur, b)
			return cur, false
		}
		if len(writers) == 1 {
			cur = writers[0]
			if cur.Type != types.TypeMem {
				cur = projection[cur]
			}
			progress = true
		}
		pending = rest
		if !progress {
			break
		}
	}
	for _, v := range pending {
		w.report("%v reads %v in %v, but the memory in effect is %v", v, memIn(v), b, cur)
	}
	return cur, len(pending) == 0
}

// memoryAlways reports whether every chain of memory states that ends in m
// passes through base: m is built on base on every path. A cycle through a
// loop adds no route of its own.
func memoryAlways(m, base *Value) bool {
	onPath := make(map[*Value]bool)
	var walk func(x *Value) bool
	walk = func(x *Value) bool {
		switch {
		case x == base:
			return true
		case onPath[x]:
			return true
		}
		onPath[x] = true
		defer delete(onPath, x)
		if x.Op == OpPhi {
			for _, a := range x.Args {
				if !walk(a) {
					return false
				}
			}
			return true
		}
		if tuple := memProjection(x); tuple != nil {
			x = tuple
		}
		in := memIn(x)
		return in != nil && walk(in)
	}
	return walk(m)
}

// memoryAfter reports whether some chain of memory states that ends in m
// passes through base.
func memoryAfter(m, base *Value) bool {
	seen := make(map[*Value]bool)
	var walk func(x *Value) bool
	walk = func(x *Value) bool {
		if x == base {
			return true
		}
		if seen[x] {
			return false
		}
		seen[x] = true
		if x.Op == OpPhi {
			for _, a := range x.Args {
				if walk(a) {
					return true
				}
			}
			return false
		}
		if tuple := memProjection(x); tuple != nil {
			x = tuple
		}
		in := memIn(x)
		return in != nil && walk(in)
	}
	return walk(m)
}

// pointerLoads returns the loads of f that need a barrier: pointer-shaped
// results of a load, or of an atomic pointer load, from memory that is not
// read-only data.
func pointerLoads(f *Func) []*Value {
	var loads []*Value
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			var slot *Value
			switch {
			case v.Op == OpLoad:
				slot = v.Args[0]
			case v.Op == OpSelect0 && v.Args[0].Op == OpAtomicLoadPtr:
				slot = v.Args[0].Args[0]
			default:
				continue
			}
			if v.Type.IsPtrShaped() && !IsReadOnlyGlobalAddr(slot) {
				loads = append(loads, v)
			}
		}
	}
	return loads
}

// slowCalls counts the calls of f to the slow path of the barrier.
func slowCalls(f *Func) int {
	n := 0
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if aux, ok := v.Aux.(*AuxCall); ok && v.Op == OpStaticCall && aux.Fn != nil && aux.Fn.Name == "runtime.zgcLoadBarrierSlow" {
				n++
			}
		}
	}
	return n
}

// barrierState returns the memory state after the barrier of the pointer
// load p, which the pass turned into the result Phi of the block after the
// slow call. It reports an error and returns nil when p is no such Phi.
func barrierState(t *testing.T, p *Value) *Value {
	t.Helper()
	if p.Op != OpPhi || !p.Type.IsPtrShaped() {
		t.Errorf("%s is not the result of a barrier", p.LongString())
		return nil
	}
	m := memPhiOf(p.Block)
	if m == nil {
		t.Errorf("the barrier of %v leaves no memory phi in %v", p, p.Block)
	}
	return m
}

// requireAlways reports an error unless the memory state m is built on the
// barrier state base on every path. A nil argument skips the check; the
// error is reported where it arose.
func requireAlways(t *testing.T, what string, m, base *Value) {
	t.Helper()
	if m != nil && base != nil && !memoryAlways(m, base) {
		t.Errorf("%s: memory state %v is not built on %v on every path", what, m, base)
	}
}

// checkPhiArgs reports an error unless the memory Phi takes, from each
// predecessor in want, the state want names, and a memory state from every
// other predecessor.
func checkPhiArgs(t *testing.T, phi *Value, want map[*Block]*Value) {
	t.Helper()
	for i, e := range phi.Block.Preds {
		w, ok := want[e.Block()]
		switch {
		case phi.Args[i] == nil:
			t.Errorf("%v has no argument for the edge from %v", phi, e.Block())
		case ok && phi.Args[i] != w:
			t.Errorf("%v takes %v from %v, want %v", phi, phi.Args[i], e.Block(), w)
		case !ok && phi.Args[i].Type != types.TypeMem:
			t.Errorf("%v takes %v, not a memory state, from %v", phi, phi.Args[i], e.Block())
		}
	}
}

// lbFun builds a function the way the compiler hands it to the pass: the
// register ABI is the default ABI.
func lbFun(c *Conf, entry string, blocs ...bloc) fun {
	fn := c.Fun(entry, blocs...)
	fn.f.ABI0, fn.f.ABI1 = c.config.ABI0, c.config.ABI1
	fn.f.ABIDefault = fn.f.ABI1
	return fn
}

// lbEntry returns the entry block of a fixture: the initial memory, two
// pointer slots in static data, a nil pointer to store and a constant
// condition, followed by the extra entries.
func lbEntry(c *Conf, extra ...any) bloc {
	ty := c.config.Types
	entries := append([]any{
		Valu("mem0", OpInitMem, types.TypeMem, 0, nil),
		Valu("sb", OpSB, ty.Uintptr, 0, nil),
		Valu("slot", OpAddr, ty.BytePtrPtr, 0, nil, "sb"),
		Valu("slot2", OpOffPtr, ty.BytePtrPtr, 8, nil, "slot"),
		Valu("nilp", OpConstNil, ty.BytePtr, 0, nil),
		Valu("c", OpConstBool, ty.Bool, 1, nil),
	}, extra...)
	return Bloc("entry", entries...)
}

// lbLoad is a pointer load of slot at memory state mem.
func lbLoad(c *Conf, name, slot, mem string) valu {
	return Valu(name, OpLoad, c.config.Types.BytePtr, 0, nil, slot, mem)
}

// lbStore stores the pointer val into slot at memory state mem.
func lbStore(c *Conf, name, slot, val, mem string) valu {
	return Valu(name, OpStore, types.TypeMem, 0, c.config.Types.BytePtr, slot, val, mem)
}

// lbStraight loads a pointer and stores it back in one block.
func lbStraight(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c,
			lbLoad(c, "p", "slot", "mem0"),
			lbStore(c, "st", "slot2", "p", "mem0"),
			Exit("st")))
}

func checkStraight(t *testing.T, fn fun) {
	b := barrierState(t, fn.values["p"])
	requireAlways(t, "store after the load", fn.values["st"], b)
}

// lbEntryDiamond loads in the entry block, so that the barrier state must
// reach a store in one arm, and a Phi and a store at the join.
func lbEntryDiamond(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c,
			lbLoad(c, "lp", "slot", "mem0"),
			Valu("nn", OpIsNonNil, c.config.Types.Bool, 0, nil, "lp"),
			If("nn", "left", "right")),
		Bloc("left",
			lbStore(c, "ml", "slot2", "lp", "mem0"),
			Goto("join")),
		Bloc("right",
			Goto("join")),
		Bloc("join",
			Valu("mj", OpPhi, types.TypeMem, 0, nil, "ml", "mem0"),
			lbStore(c, "mt", "slot", "nilp", "mj"),
			Exit("mt")))
}

func checkEntryDiamond(t *testing.T, fn fun) {
	b := barrierState(t, fn.values["lp"])
	requireAlways(t, "store in the arm", fn.values["ml"], b)
	requireAlways(t, "phi at the join", fn.values["mj"], b)
	requireAlways(t, "store after the join", fn.values["mt"], b)
}

// lbLoopHeader loads in a loop head that has a memory Phi, and stores in the
// body; the exit reads the Phi.
func lbLoopHeader(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, Goto("head")),
		Bloc("head",
			Valu("mh", OpPhi, types.TypeMem, 0, nil, "mem0", "ms"),
			lbLoad(c, "lp", "slot", "mh"),
			Valu("nn", OpIsNonNil, c.config.Types.Bool, 0, nil, "lp"),
			If("nn", "body", "exit")),
		Bloc("body",
			lbStore(c, "ms", "slot2", "lp", "mh"),
			Goto("head")),
		Bloc("exit",
			Exit("mh")))
}

func checkLoopHeader(t *testing.T, fn fun) {
	b := barrierState(t, fn.values["lp"])
	requireAlways(t, "store in the body", fn.values["ms"], b)
	requireAlways(t, "memory after the loop", fn.blocks["exit"].Controls[0], b)
}

// lbLoopBody loads in a loop body that makes no other memory state: the
// head has no memory Phi, and gains one for the state the barrier adds on
// the back edge, as the exit does for its two predecessors.
func lbLoopBody(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, Goto("head")),
		Bloc("head",
			If("c", "body", "exit")),
		Bloc("body",
			lbLoad(c, "lp", "slot", "mem0"),
			Valu("nn", OpIsNonNil, c.config.Types.Bool, 0, nil, "lp"),
			If("nn", "head", "exit")),
		Bloc("exit",
			Exit("mem0")))
}

func checkLoopBody(t *testing.T, fn fun) {
	b := barrierState(t, fn.values["lp"])
	if memPhiOf(fn.blocks["head"]) == nil {
		t.Errorf("loop head %v has no memory phi for the state of the back edge", fn.blocks["head"])
	}
	if ctl := fn.blocks["exit"].Controls[0]; b != nil && !memoryAfter(ctl, b) {
		t.Errorf("memory after the loop, %v, does not follow the barrier %v", ctl, b)
	}
}

// lbJoinOneArm has a pointer load in one arm of a diamond whose join has no
// memory Phi, since no arm changes the memory before the pass. The join
// stores a pointer.
func lbJoinOneArm(c *Conf) fun {
	return lbJoinFun(c, []any{Goto("join")}, []string{"lp", "nilp"})
}

// lbJoinBothArms has a pointer load in both arms of the diamond.
func lbJoinBothArms(c *Conf) fun {
	right := []any{lbLoad(c, "rp", "slot2", "mem0"), Goto("join")}
	return lbJoinFun(c, right, []string{"lp", "rp"})
}

// lbJoinDeadPred adds a block that no path reaches and that jumps to the
// join, a third predecessor.
func lbJoinDeadPred(c *Conf) fun {
	return lbJoinFun(c, []any{Goto("join")}, []string{"lp", "nilp", "nilp"}, Bloc("dead", Goto("join")))
}

// lbJoinFun builds the diamond of the lbJoin fixtures: right holds the
// entries of the right arm, phiArgs the pointer Phi of the join in
// predecessor order, and extra the blocks that also jump to the join.
func lbJoinFun(c *Conf, right []any, phiArgs []string, extra ...bloc) fun {
	blocs := []bloc{
		lbEntry(c, If("c", "left", "right")),
		Bloc("left", lbLoad(c, "lp", "slot", "mem0"), Goto("join")),
		Bloc("right", right...),
	}
	blocs = append(blocs, extra...)
	blocs = append(blocs, Bloc("join",
		Valu("pp", OpPhi, c.config.Types.BytePtr, 0, nil, phiArgs...),
		lbStore(c, "ms", "slot2", "pp", "mem0"),
		Exit("ms")))
	return lbFun(c, "entry", blocs...)
}

// checkJoin returns the check of a join fixture: the join must have a memory
// Phi that its store reads, and the store must follow the barrier of each
// load named in loads.
func checkJoin(loads ...string) func(*testing.T, fun) {
	return func(t *testing.T, fn fun) {
		ms := fn.values["ms"]
		join := ms.Block
		phi := memPhiOf(join)
		if phi == nil {
			t.Fatalf("%v merges states that differ after the barrier but has no memory phi", join)
		}
		if len(phi.Args) != len(join.Preds) {
			t.Fatalf("%s has %d arguments for %d predecessors", phi.LongString(), len(phi.Args), len(join.Preds))
		}
		if got := ms.Args[len(ms.Args)-1]; got != phi {
			t.Errorf("the store after the join reads %v, not the memory phi %v", got, phi)
		}
		for _, name := range loads {
			if b := barrierState(t, fn.values[name]); b != nil && !memoryAfter(ms, b) {
				t.Errorf("the store after the join does not follow the barrier of %s", name)
			}
		}
	}
}

// checkJoinDeadPred checks the join of lbJoinDeadPred; its Phi takes a memory
// state over the edge of the dead block as over the others.
func checkJoinDeadPred(t *testing.T, fn fun) {
	checkJoin("lp")(t, fn)
	phi := memPhiOf(fn.values["ms"].Block)
	if a := phi.Args[2]; a.Type != types.TypeMem {
		t.Errorf("the phi takes %v over the edge from the dead block", a)
	}
}

// lbChain has three pointer loads and three stores in one block. The first
// two load the first state; the store after them must follow both barriers,
// and the third load, of the state that store makes, must follow it.
func lbChain(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c,
			lbLoad(c, "l1", "slot", "mem0"),
			lbLoad(c, "l2", "slot2", "mem0"),
			lbStore(c, "m1", "slot2", "l1", "mem0"),
			lbLoad(c, "l3", "slot", "m1"),
			lbStore(c, "m2", "slot", "l3", "m1"),
			lbStore(c, "m3", "slot2", "l2", "m2"),
			Exit("m3")))
}

func checkChain(t *testing.T, fn fun) {
	v := fn.values
	p1 := barrierState(t, v["l1"])
	p2 := barrierState(t, v["l2"])
	p3 := barrierState(t, v["l3"])
	requireAlways(t, "first store after the barrier of the first load", v["m1"], p1)
	requireAlways(t, "first store after the barrier of the second load", v["m1"], p2)
	requireAlways(t, "last store after the barrier of the third load", v["m3"], p3)
	requireAlways(t, "barrier of the third load after the store it reads", p3, v["m1"])
}

// lbAtomic loads a pointer atomically and stores it. The barrier follows the
// memory result of the atomic, and the store follows the barrier.
func lbAtomic(c *Conf) fun {
	ty := c.config.Types
	return lbFun(c, "entry",
		lbEntry(c,
			Valu("a", OpAtomicLoadPtr, types.NewTuple(ty.BytePtr, types.TypeMem), 0, nil, "slot", "mem0"),
			Valu("ap", OpSelect0, ty.BytePtr, 0, nil, "a"),
			Valu("am", OpSelect1, types.TypeMem, 0, nil, "a"),
			lbStore(c, "st", "slot2", "ap", "am"),
			Exit("st")))
}

func checkAtomic(t *testing.T, fn fun) {
	v := fn.values
	b := barrierState(t, v["ap"])
	requireAlways(t, "store after the atomic load", v["st"], b)
	requireAlways(t, "barrier after the memory the atomic load makes", b, v["am"])
}

// lbDead has a block that no path reaches. It loads a pointer and stores it,
// and jumps to a join that has a Phi argument for it.
func lbDead(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, If("c", "left", "join")),
		Bloc("left",
			lbStore(c, "ml", "slot2", "nilp", "mem0"),
			Goto("join")),
		Bloc("dead",
			lbLoad(c, "dl", "slot", "mem0"),
			lbStore(c, "dm", "slot2", "dl", "mem0"),
			Goto("join")),
		Bloc("join",
			Valu("mj", OpPhi, types.TypeMem, 0, nil, "mem0", "ml", "dm"),
			Exit("mj")))
}

func checkDead(t *testing.T, fn fun) {
	if dl := fn.values["dl"]; dl.Op != OpPhi {
		t.Errorf("the load in the dead block keeps no barrier: %s", dl.LongString())
	}
	for i, a := range fn.values["mj"].Args {
		if a == nil || a.Type != types.TypeMem {
			t.Errorf("argument %d of the join phi is %v, not a memory state", i, a)
		}
	}
}

// lbStraightStores has two stores and no load.
func lbStraightStores(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c,
			lbStore(c, "m1", "slot", "nilp", "mem0"),
			lbStore(c, "m2", "slot2", "nilp", "m1"),
			Exit("m2")))
}

// lbQuietDiamond has arms that make no memory state and a join that stores.
// The join needs no Phi, whichever block a walk meets first.
func lbQuietDiamond(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, If("c", "left", "right")),
		Bloc("left", Goto("join")),
		Bloc("right", Goto("join")),
		Bloc("join",
			lbStore(c, "ms", "slot", "nilp", "mem0"),
			Exit("ms")))
}

// lbDiamondPhi stores in one arm; the join has the Phi.
func lbDiamondPhi(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, If("c", "left", "right")),
		Bloc("left",
			lbStore(c, "ml", "slot2", "nilp", "mem0"),
			Goto("join")),
		Bloc("right", Goto("join")),
		Bloc("join",
			Valu("mj", OpPhi, types.TypeMem, 0, nil, "ml", "mem0"),
			lbStore(c, "ms", "slot", "nilp", "mj"),
			Exit("ms")))
}

// lbQuietLoop has a loop that makes no memory state, and an exit with two
// predecessors that stores.
func lbQuietLoop(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, Goto("head")),
		Bloc("head", If("c", "body", "exit")),
		Bloc("body", If("c", "head", "exit")),
		Bloc("exit",
			lbStore(c, "ms", "slot", "nilp", "mem0"),
			Exit("ms")))
}

// lbQuietNested has two nested loops that make no memory state.
func lbQuietNested(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, Goto("outer")),
		Bloc("outer", If("c", "inner", "exit")),
		Bloc("inner", If("c", "ibody", "latch")),
		Bloc("ibody", Goto("inner")),
		Bloc("latch", Goto("outer")),
		Bloc("exit",
			lbStore(c, "ms", "slot", "nilp", "mem0"),
			Exit("ms")))
}

// lbPlainLoads loads a non-pointer word, and a pointer from the read-only
// nil slot; neither takes a barrier.
func lbPlainLoads(c *Conf) fun {
	ty := c.config.Types
	return lbFun(c, "entry",
		lbEntry(c,
			Valu("wslot", OpAddr, ty.UintptrPtr, 0, nil, "sb"),
			Valu("w", OpLoad, ty.Uintptr, 0, nil, "wslot", "mem0"),
			Valu("nilslot", OpConstNil, ty.BytePtrPtr, 0, nil),
			Valu("rp", OpLoad, ty.BytePtr, 0, nil, "nilslot", "mem0"),
			Valu("sw", OpStore, types.TypeMem, 0, ty.Uintptr, "wslot", "w", "mem0"),
			lbStore(c, "sp", "slot", "rp", "sw"),
			Exit("sp")))
}

// lbStaleDiamond stores in the left arm and again at the join, where the
// second store still reads the state before the arm.
func lbStaleDiamond(c *Conf) fun {
	return lbStaleDiamondFun(c)
}

// lbStaleDiamondDeadPred adds a block that no path reaches and that jumps to
// the join, a third predecessor.
func lbStaleDiamondDeadPred(c *Conf) fun {
	return lbStaleDiamondFun(c, Bloc("dead", Goto("join")))
}

// lbStaleDiamondFun builds the diamond of the lbStaleDiamond fixtures; extra
// are the blocks that also jump to the join.
func lbStaleDiamondFun(c *Conf, extra ...bloc) fun {
	blocs := []bloc{
		lbEntry(c, If("c", "left", "right")),
		Bloc("left",
			lbStore(c, "ml", "slot2", "nilp", "mem0"),
			Goto("join")),
		Bloc("right", Goto("join")),
	}
	blocs = append(blocs, extra...)
	blocs = append(blocs, Bloc("join",
		lbStore(c, "ms", "slot", "nilp", "mem0"),
		Exit("ms")))
	return lbFun(c, "entry", blocs...)
}

func checkStaleDiamond(t *testing.T, fn fun) {
	ms := fn.values["ms"]
	join := ms.Block
	phi := memPhiOf(join)
	if phi == nil {
		t.Fatalf("%v merges the state of the arm with the entry state but has no memory phi", join)
	}
	if got := ms.Args[len(ms.Args)-1]; got != phi {
		t.Errorf("the store at the join reads %v, not the memory phi %v", got, phi)
	}
	checkPhiArgs(t, phi, map[*Block]*Value{
		fn.blocks["left"]:  fn.values["ml"],
		fn.blocks["right"]: fn.values["mem0"],
	})
}

// lbStaleLoop stores in a loop body that reads the state before the loop,
// and the exit reads it too.
func lbStaleLoop(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, Goto("head")),
		Bloc("head", If("c", "body", "exit")),
		Bloc("body",
			lbStore(c, "ms", "slot2", "nilp", "mem0"),
			Goto("head")),
		Bloc("exit", Exit("mem0")))
}

func checkStaleLoop(t *testing.T, fn fun) {
	ms, head := fn.values["ms"], fn.blocks["head"]
	phi := memPhiOf(head)
	if phi == nil {
		t.Fatalf("loop head %v merges the entry state with %v but has no memory phi", head, ms)
	}
	checkPhiArgs(t, phi, map[*Block]*Value{
		fn.blocks["entry"]: fn.values["mem0"],
		fn.blocks["body"]:  ms,
	})
	if got := ms.Args[len(ms.Args)-1]; got != phi {
		t.Errorf("the store in the body reads %v, not the memory phi of the head %v", got, phi)
	}
	if ctl := fn.blocks["exit"].Controls[0]; !memoryAfter(ctl, ms) {
		t.Errorf("memory after the loop, %v, does not include the store %v", ctl, ms)
	}
}

// lbStaleNested stores in the outer loop after an inner loop, and reads the
// state before both loops everywhere.
func lbStaleNested(c *Conf) fun {
	return lbFun(c, "entry",
		lbEntry(c, Goto("outer")),
		Bloc("outer", If("c", "inner", "exit")),
		Bloc("inner", If("c", "ibody", "latch")),
		Bloc("ibody", Goto("inner")),
		Bloc("latch",
			lbStore(c, "ms", "slot2", "nilp", "mem0"),
			Goto("outer")),
		Bloc("exit", Exit("mem0")))
}

func checkStaleNested(t *testing.T, fn fun) {
	ms, outer := fn.values["ms"], fn.blocks["outer"]
	phi := memPhiOf(outer)
	if phi == nil {
		t.Fatalf("loop head %v merges the entry state with %v but has no memory phi", outer, ms)
	}
	checkPhiArgs(t, phi, map[*Block]*Value{
		fn.blocks["entry"]: fn.values["mem0"],
		fn.blocks["latch"]: ms,
	})
	if ctl := fn.blocks["exit"].Controls[0]; !memoryAfter(ctl, ms) {
		t.Errorf("memory after the loops, %v, does not include the store %v", ctl, ms)
	}
}
