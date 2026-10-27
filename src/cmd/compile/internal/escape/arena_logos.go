// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package escape

import (
	"sync"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"internal/buildcfg"
)

// LogosArenaMaxBytes is the largest object an arena chunk can hold
// (one 64 KiB chunk minus its header allowance).
const LogosArenaMaxBytes = (64 << 10) - 1024

// LogosArenaEnabled reports whether the tier-0 arena experiment is on.
func LogosArenaEnabled() bool {
	return buildcfg.Experiment.LogosArena
}

// Arena placement reads the solve verdict, it never forms one. The
// foreign-escape rule is the precedence case and lives in batch.outlives
// (solve.go), which lets the foreignLoc root outlive a location only when
// foreignBacking (foreign_logos.go) recognizes a variable-capacity
// OMAKESLICE. Foreign wins over arena, so a make that rule forces onto the
// heap is never placed: as a local make it surfaces as EscHeap, as a
// returned make in its shadow's attrEscapes (EscHeap alone is also the
// expected result-only outcome there). sync.Pool storage reaches the same
// two marks as a hard heap sink (#138).
func logosArenaSolvedHeap(n *ir.MakeExpr) bool {
	return n != nil && n.Esc() == ir.EscHeap
}

// logosArenaShadowSolvedHeap reports that shadow escaped for a reason other
// than being returned, which no arena policy may treat as caller-region only.
func logosArenaShadowSolvedHeap(shadow *location) bool {
	return shadow != nil && shadow.hasAttr(attrEscapes)
}

// LogosArenaConsent says how a static call hands the callee the consent to
// allocate returned objects in the caller's arena region (SPEC Sec 8.3).
type LogosArenaConsent uint8

const (
	// LogosArenaConsentNone: the callee must allocate returned objects on
	// the heap.
	LogosArenaConsentNone LogosArenaConsent = iota

	// LogosArenaConsentSet: the result stays inside the calling frame (or
	// loop iteration), so the call always consents.
	LogosArenaConsentSet

	// LogosArenaConsentChain: the result flows only to the caller's own
	// results, so the call consents exactly when the caller received consent.
	LogosArenaConsentChain
)

type logosArenaCallKind uint8

const (
	logosArenaCallLocal logosArenaCallKind = iota + 1
	logosArenaCallChain
)

// A logosArenaBatch holds the shadow locations of one escape batch.
//
// Every returned slice allocation and every static call result gets a
// shadow location that the flow graph treats exactly like the real one
// except that a result parameter of another frame does not outlive it. The
// shadow escapes only for reasons other than being returned, so
// "real escapes and shadow does not" means "escapes only through a
// result": such an object may live in the caller's region.
type logosArenaBatch struct {
	makes []logosArenaMakeLoc
	calls []logosArenaCallLoc
}

type logosArenaMakeLoc struct {
	n      *ir.MakeExpr
	shadow *location
}

type logosArenaCallLoc struct {
	call   *ir.CallExpr
	caller *ir.Func
	callee *ir.Func
	// res is the call result modeled as a fresh allocation, resNoRet its
	// shadow that returning does not make escape.
	res      *location
	resNoRet *location
}

type logosArenaCallSite struct {
	caller *ir.Func
	callee *ir.Func
	kind   logosArenaCallKind
}

// logosArenaFnInfo records the body facts that decide whether fn may hold
// arena allocations.
type logosArenaFnInfo struct {
	hasDefer bool
	hasGoto  bool
}

// logosArenaRegistry is the package-wide record the walk phase reads. Every
// field is written only while an escape batch finishes; the walk phase starts
// after the last batch and only reads.
type logosArenaRegistry struct {
	mu sync.RWMutex

	fns     map[*ir.Func]logosArenaFnInfo
	returns map[ir.Node]bool

	// retSites maps each slice allocation that escapes only through a
	// result to its function.
	retSites map[*ir.MakeExpr]*ir.Func

	calls      []logosArenaCallSite
	callByNode map[*ir.CallExpr]int

	// producing holds the functions that hand a caller-region object to
	// their callers, directly or through a consenting chain; live holds the
	// producing functions with at least one consenting caller. Only live
	// functions take the consent token.
	producing map[*ir.Func]bool
	live      map[*ir.Func]bool
}

var logosArenaReg = logosArenaRegistry{
	fns:        make(map[*ir.Func]logosArenaFnInfo),
	returns:    make(map[ir.Node]bool),
	retSites:   make(map[*ir.MakeExpr]*ir.Func),
	callByNode: make(map[*ir.CallExpr]int),
	producing:  make(map[*ir.Func]bool),
	live:       make(map[*ir.Func]bool),
}

// logosArenaInfo returns the cached body facts of fn, scanning it on first
// use. The scan also records which slice allocations sit under a return
// statement.
func logosArenaInfo(fn *ir.Func) logosArenaFnInfo {
	logosArenaReg.mu.RLock()
	info, ok := logosArenaReg.fns[fn]
	logosArenaReg.mu.RUnlock()
	if ok {
		return info
	}

	var returned []ir.Node
	var scan func(n ir.Node)
	scan = func(n ir.Node) {
		if n == nil {
			return
		}
		switch n := n.(type) {
		case *ir.ClosureExpr:
			return
		case *ir.ReturnStmt:
			for _, res := range n.Results {
				ir.Visit(res, func(m ir.Node) {
					if m, ok := m.(*ir.MakeExpr); ok && m.Op() == ir.OMAKESLICE {
						returned = append(returned, m)
					}
				})
			}
		case *ir.GoDeferStmt:
			if n.Op() == ir.ODEFER {
				info.hasDefer = true
			}
		case *ir.BranchStmt:
			if n.Op() == ir.OGOTO && (n.Label == nil || len(n.Label.Name) == 0 || n.Label.Name[0] != '.') {
				info.hasGoto = true
			}
		}
		ir.DoChildren(n, func(c ir.Node) bool { scan(c); return false })
	}
	scan(fn)
	logosArenaReg.store(fn, info, returned)
	return info
}

// store records the scan of fn.
func (r *logosArenaRegistry) store(fn *ir.Func, info logosArenaFnInfo, returned []ir.Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fns[fn] = info
	for _, m := range returned {
		r.returns[m] = true
	}
}

// LogosArenaPlaceable reports whether fn may hold arena allocations: the
// experiment is on, fn is not part of the runtime, and its body has no
// defer (the frame restore is an emitted defer there, SPEC #63) and no goto
// (unstructured loops have no back-edge to restore on).
func LogosArenaPlaceable(fn *ir.Func) bool {
	if !LogosArenaEnabled() || base.Flag.CompilingRuntime {
		return false
	}
	if fn.Pragma&ir.Nosplit != 0 || fn.Wrapper() {
		return false
	}
	info := logosArenaInfo(fn)
	return !info.hasDefer && !info.hasGoto
}

// LogosArenaVetoed reports whether fn must not use arena placement because
// its body has a goto.
func LogosArenaVetoed(fn *ir.Func) bool {
	return logosArenaInfo(fn).hasGoto
}

func logosArenaReturned(n ir.Node) bool {
	logosArenaReg.mu.RLock()
	defer logosArenaReg.mu.RUnlock()
	return logosArenaReg.returns[n]
}

// logosArenaRetCapable reports whether fn may allocate returned objects in
// its caller's region: it holds arena allocations and is not a closure,
// because only static calls to named functions carry the consent.
func logosArenaRetCapable(fn *ir.Func) bool {
	return fn.OClosure == nil && !fn.Dupok() && LogosArenaPlaceable(fn)
}

// logosArenaRetOutlives is outlives for a result parameter root and a
// shadow location. A value returned by the function that holds the
// allocation lives in that function's caller region, so its own result does
// not outlive the shadow, but a result variable declared outside the loop
// that holds the allocation still does (the next iteration's restore would
// free the object while the variable keeps it). A result of any other
// function reaches consumers this analysis does not see (a closure whose
// results are lost, an in-batch callee with callers outside the batch), so
// it outlives the shadow exactly as it outlives the real location.
func (b *batch) logosArenaRetOutlives(l, other *location) bool {
	if l.curfn != other.curfn {
		return true
	}
	return l.loopDepth < other.loopDepth
}

// logosArenaMake records the shadow location of an OMAKESLICE. The caller
// passes the hole of the real allocation location so that the real flow is
// built first.
func (e *escape) logosArenaMake(k hole, n *ir.MakeExpr, _ hole) {
	if !LogosArenaEnabled() {
		return
	}
	if k.dst == &e.blankLoc || !logosArenaRetCapable(e.curfn) {
		return
	}
	shadow := &location{n: n, curfn: e.curfn, loopDepth: e.loopDepth, logosNoRet: true}
	e.flow(k.addr(n, "spill"), shadow)
	e.logosArena.makes = append(e.logosArena.makes, logosArenaMakeLoc{n: n, shadow: shadow})
}

// logosArenaCall models the result of a direct static call as a fresh
// allocation at the call site, when the callee may hand back caller-region
// objects. Whether that allocation escapes tells the walk phase whether the
// call may consent.
func (e *escape) logosArenaCall(ks []hole, call *ir.CallExpr, fns []*ir.Name) {
	if ks == nil || len(fns) != 1 || !LogosArenaEnabled() {
		return
	}
	if call.Op() != ir.OCALLFUNC || !LogosArenaPlaceable(e.curfn) {
		return
	}
	callee := logosArenaStaticCallee(call.Fun)
	if callee == nil || callee.Nname != fns[0] {
		return
	}
	if !e.inMutualBatch(fns[0]) && !logosArenaProducing(callee) {
		return
	}
	results := callee.Type().Results()
	for i, k := range ks {
		if i < len(results) && results[i].Type.HasPointers() && k.dst != &e.blankLoc && k.derefs < 0 {
			return
		}
	}
	var res, resNoRet *location
	for i, k := range ks {
		if i >= len(results) || !results[i].Type.HasPointers() || k.dst == &e.blankLoc {
			continue
		}
		if res == nil {
			res = &location{n: call, curfn: e.curfn, loopDepth: e.loopDepth}
			resNoRet = &location{n: call, curfn: e.curfn, loopDepth: e.loopDepth, logosNoRet: true}
		}
		e.flow(k.addr(call, "arena-return"), res)
		e.flow(k.addr(call, "arena-return"), resNoRet)
	}
	if res == nil {
		return
	}
	e.logosArena.calls = append(e.logosArena.calls, logosArenaCallLoc{call: call, caller: e.curfn, callee: callee, res: res, resNoRet: resNoRet})
}

// logosArenaStaticCallee returns the named function that a call through fun
// reaches directly, or nil for closures, function values, interface
// methods, and duplicate-ok functions (generic instantiations): the linker
// keeps one copy of those, so the copy compiled here may not be the one
// that takes the consent token.
func logosArenaStaticCallee(fun ir.Node) *ir.Func {
	switch fun.Op() {
	case ir.ONAME, ir.OMETHEXPR:
	default:
		return nil
	}
	name := ir.StaticCalleeName(fun)
	if name == nil || name.Class != ir.PFUNC || name.Func == nil || name.Func.OClosure != nil || name.Func.Dupok() {
		return nil
	}
	return name.Func
}

func logosArenaProducing(fn *ir.Func) bool {
	logosArenaReg.mu.RLock()
	defer logosArenaReg.mu.RUnlock()
	return logosArenaReg.producing[fn]
}

// logosArenaMark finishes a batch: it records the slice allocations that
// escape only through a result and the consent each call site may give, then
// recomputes which functions take the consent token.
func (b *batch) logosArenaMark(fns []*ir.Func) {
	if !LogosArenaEnabled() {
		return
	}
	for _, fn := range fns {
		logosArenaInfo(fn)
	}

	logosArenaReg.mu.Lock()
	defer logosArenaReg.mu.Unlock()

	for _, m := range b.logosArena.makes {
		fn := m.shadow.curfn
		// Foreign precedence seam for returned makes: a foreign-forced
		// (or otherwise non-result-escaping) make has its shadow marked
		// by walkAll, so it is never recorded as a caller-region site.
		if !logosArenaSolvedHeap(m.n) || logosArenaShadowSolvedHeap(m.shadow) || !logosArenaRetShape(m.n) {
			continue
		}
		logosArenaReg.retSites[m.n] = fn
	}
	for _, c := range b.logosArena.calls {
		if logosArenaShadowSolvedHeap(c.resNoRet) {
			continue
		}
		kind := logosArenaCallLocal
		if c.res.hasAttr(attrEscapes) {
			if !logosArenaRetCapableLocked(c.caller) {
				continue
			}
			kind = logosArenaCallChain
		}
		logosArenaReg.callByNode[c.call] = len(logosArenaReg.calls)
		logosArenaReg.calls = append(logosArenaReg.calls, logosArenaCallSite{caller: c.caller, callee: c.callee, kind: kind})
	}
	logosArenaReg.recompute()
}

// logosArenaRetCapableLocked is logosArenaRetCapable for a function the
// caller already scanned, for use while the registry lock is held.
func logosArenaRetCapableLocked(fn *ir.Func) bool {
	if fn.OClosure != nil || fn.Dupok() || !LogosArenaEnabled() || base.Flag.CompilingRuntime {
		return false
	}
	if fn.Pragma&ir.Nosplit != 0 || fn.Wrapper() {
		return false
	}
	info := logosArenaReg.fns[fn]
	return !info.hasDefer && !info.hasGoto
}

// logosArenaRetShape reports whether the slice allocation n can live in an
// arena chunk: a heap-allocation reason such as a huge constant size would
// have made the real location escape without the shadow noticing.
func logosArenaRetShape(n *ir.MakeExpr) bool {
	if HeapAllocReason(n) != "" {
		return false
	}
	elem := n.Type().Elem()
	if elem.Size() <= 0 || elem.Alignment() > 8 {
		return false
	}
	capExpr := n.Cap
	if capExpr == nil {
		capExpr = n.Len
	}
	if capExpr != nil && ir.IsSmallIntConst(capExpr) && !logosArenaConstFits(ir.Int64Val(capExpr), elem.Size()) {
		return false
	}
	return true
}

// logosArenaConstFits reports whether n elements of elemSize bytes
// (elemSize > 0) fit one arena object. It divides before it multiplies, so
// a huge elemSize cannot wrap the product into a size that looks small.
func logosArenaConstFits(n, elemSize int64) bool {
	return n <= LogosArenaMaxBytes/elemSize
}

// LogosArenaConstBytes returns the byte size of n when n is a make of a
// small constant capacity and that size is positive and fits one arena
// object. The fit is tested by division before the product is formed, and
// the walk phase calls this before it evaluates or appends anything, so a
// refusal leaves the standard lowering a clean start.
func LogosArenaConstBytes(n *ir.MakeExpr) (int64, bool) {
	capExpr := n.Cap
	if capExpr == nil {
		capExpr = n.Len
	}
	if capExpr == nil || !ir.IsSmallIntConst(capExpr) {
		return 0, false
	}
	elemSize := n.Type().Elem().Size()
	capVal := ir.Int64Val(capExpr)
	if elemSize <= 0 || capVal <= 0 || !logosArenaConstFits(capVal, elemSize) {
		return 0, false
	}
	return capVal * elemSize, true
}

// recompute derives producing and live from the recorded sites. The caller
// holds the write lock.
func (r *logosArenaRegistry) recompute() {
	producing := make(map[*ir.Func]bool)
	for _, fn := range r.retSites {
		producing[fn] = true
	}
	for changed := true; changed; {
		changed = false
		for _, c := range r.calls {
			if c.kind == logosArenaCallChain && producing[c.callee] && !producing[c.caller] {
				producing[c.caller] = true
				changed = true
			}
		}
	}
	live := make(map[*ir.Func]bool)
	for changed := true; changed; {
		changed = false
		for _, c := range r.calls {
			if live[c.callee] || !producing[c.callee] {
				continue
			}
			if c.kind == logosArenaCallLocal || live[c.caller] {
				live[c.callee] = true
				changed = true
			}
		}
	}
	r.producing = producing
	r.live = live
}

// LogosArenaCandidate reports whether fn takes the consent token at entry:
// it hands caller-region objects to its callers and at least one static
// call in this package consents.
func LogosArenaCandidate(fn *ir.Func) bool {
	logosArenaReg.mu.RLock()
	defer logosArenaReg.mu.RUnlock()
	return logosArenaReg.live[fn]
}

// LogosArenaCallConsent reports the consent that the walk phase emits right
// before call.
func LogosArenaCallConsent(call *ir.CallExpr) LogosArenaConsent {
	logosArenaReg.mu.RLock()
	defer logosArenaReg.mu.RUnlock()
	i, ok := logosArenaReg.callByNode[call]
	if !ok {
		return LogosArenaConsentNone
	}
	c := logosArenaReg.calls[i]
	if !logosArenaReg.live[c.callee] {
		return LogosArenaConsentNone
	}
	switch c.kind {
	case logosArenaCallLocal:
		return LogosArenaConsentSet
	case logosArenaCallChain:
		if logosArenaReg.live[c.caller] {
			return LogosArenaConsentChain
		}
	}
	return LogosArenaConsentNone
}

// LogosArenaRetMake reports whether n escapes only through a result of fn
// and fn takes the consent token, so n allocates in the caller's region when
// the token is set and on the heap otherwise.
func LogosArenaRetMake(n *ir.MakeExpr, fn *ir.Func) bool {
	if !LogosArenaPlaceable(fn) || n.Op() != ir.OMAKESLICE {
		return false
	}
	logosArenaReg.mu.RLock()
	defer logosArenaReg.mu.RUnlock()
	return logosArenaReg.retSites[n] == fn && logosArenaReg.live[fn]
}

// logosArenaLocalShape reports whether n is a make of an element type the
// arena can hold, with no constant-size bound check applied. EscNone is the
// solve's verdict that nothing, foreign flow and sync.Pool included, forced
// n onto the heap; anything else is a place the arena must not take.
func logosArenaLocalShape(n *ir.MakeExpr) bool {
	if n.Op() != ir.OMAKESLICE || n.Esc() != ir.EscNone {
		return false
	}
	elem := n.Type().Elem()
	return elem.Size() > 0 && elem.Alignment() <= 8
}

// LogosArenaConstMake reports whether n is a constant-size make that the
// walk const path routes to the arena. The threshold matches the stack
// buffer check in walk and the chunk fit matches LogosArenaMaxBytes.
func LogosArenaConstMake(n *ir.MakeExpr, fn *ir.Func) bool {
	if !LogosArenaPlaceable(fn) || !logosArenaLocalShape(n) {
		return false
	}
	capExpr := n.Cap
	if capExpr == nil {
		capExpr = n.Len
	}
	if capExpr == nil || !ir.IsSmallIntConst(capExpr) {
		return false
	}
	total, ok := LogosArenaConstBytes(n)
	if !ok || total <= int64(base.Debug.VariableMakeThreshold) {
		return false
	}
	return !logosArenaReturned(n)
}

// LogosArenaMake reports whether n is an arena-eligible dynamic make.
func LogosArenaMake(n *ir.MakeExpr, fn *ir.Func) bool {
	if !LogosArenaPlaceable(fn) || !logosArenaLocalShape(n) {
		return false
	}
	capExpr := n.Cap
	if capExpr == nil {
		capExpr = n.Len
	}
	if capExpr == nil || ir.IsSmallIntConst(capExpr) {
		return false
	}
	return !logosArenaReturned(n)
}
