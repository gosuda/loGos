// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"internal/buildcfg"
	"strings"

	"go/constant"
)

// Order-time syscall provenance for the moving collector (SSA walk order).
//
// The runtime foreign-admission lane (runtime/zgcSyscallAdmit) needs to
// know, for every syscall-like call site, which target the call names.
// That fact is recorded in SSA walk order, which replays the order-pass
// output order statement by statement, before per-call SSA lowering:
//
//   - Inlined calls disappear during inlining: by SSA call time the
//     InlinedCallExpr is gone, so only an order-time note still names
//     the original target.
//   - go/defer statements lower to runtime scheduling calls
//     (newproc/deferproc): by SSA call time the site names the
//     scheduler, not the syscall target. Only an order-time note still
//     names the deferred/goroutine target.
//
// Production admission does not consume recorded notes: the single
// call-lowering site brackets each qualifying synchronous call with an
// Admit/End pair gated directly on logosSyscallForeignCall. The recorder
// below is the tested ordering contract for those six shapes.
// Kinds: direct (static same-package PFUNC), imported (static
// cross-package PFUNC), indirect (closure value or interface method),
// go (OGO), defer (ODEFER), inlined (already-inline-expanded body still
// attributed to its origin). The recorder is append-only: notes arrive
// in walk/program order and flush in the same order. No existing call
// emission order is changed; this record rides alongside it.
//
// Lifetime (advisor constraint): go/defer notes are Async. A short
// Admit/End pair around the scheduling call (newproc/deferproc) would
// release retained cells before the syscall runs in the new goroutine
// or at function return, permitting a moved kernel buffer. Async notes
// therefore require admission that lives until execution finishes: the
// child/deferred execution performs its own synchronous pair when it
// runs, keyed by the recorded target. Only direct, imported, indirect,
// and inlined (synchronous) notes admit around the call itself.

// LogosSyscallCallKind names the six call shapes that carry syscall
// provenance.
type LogosSyscallCallKind int

const (
	// LogosSyscallDirect is a static call to a same-package function.
	LogosSyscallDirect LogosSyscallCallKind = iota
	// LogosSyscallImported is a static call to another package.
	LogosSyscallImported
	// LogosSyscallIndirect is a closure-value or interface-method call.
	LogosSyscallIndirect
	// LogosSyscallGo is a go statement. Async: admission must outlive
	// the scheduling call until the goroutine executes.
	LogosSyscallGo
	// LogosSyscallDefer is a defer statement. Async: admission must
	// outlive the scheduling call until the deferred call executes.
	LogosSyscallDefer
	// LogosSyscallInlined is a call whose body was inline-expanded.
	// The note is attributed to the origin target at order time.
	LogosSyscallInlined
)

// LogosSyscallProvenance is one order-time note.
type LogosSyscallProvenance struct {
	Kind   LogosSyscallCallKind
	Target string
	Async  bool
	Order  int
}

// LogosSyscallOrderRecorder accumulates provenance in program order.
type LogosSyscallOrderRecorder struct {
	recs []LogosSyscallProvenance
}

// LogosSyscallNote appends one note in walk/program order. Async is
// derived: go/defer notes outlive their scheduling call.
func (r *LogosSyscallOrderRecorder) LogosSyscallNote(kind LogosSyscallCallKind, target string) {
	r.recs = append(r.recs, LogosSyscallProvenance{
		Kind:   kind,
		Target: target,
		Async:  kind == LogosSyscallGo || kind == LogosSyscallDefer,
		Order:  len(r.recs),
	})
}

// LogosSyscallOrdered returns notes in the order they were recorded.
// The returned slice is a copy; mutating it does not disturb the
// recorder, and recording never reorders existing entries.
func (r *LogosSyscallOrderRecorder) LogosSyscallOrdered() []LogosSyscallProvenance {
	out := make([]LogosSyscallProvenance, len(r.recs))
	copy(out, r.recs)
	return out
}

// LogosSyscallClassify maps order-time site properties to a kind.
// inlined wins: an inline-expanded call is attributed to its origin
// even when the expanded form looks direct. go/defer win next: the
// scheduling call shape determines the extended lifetime. Remaining
// static calls split into direct/imported by package; dynamic calls
// (non-static fun or interface dispatch) are indirect.
func LogosSyscallClassify(inlined bool, op string, isStatic, isSamePackage, isInterface bool) LogosSyscallCallKind {
	if inlined {
		return LogosSyscallInlined
	}
	switch op {
	case "go":
		return LogosSyscallGo
	case "defer":
		return LogosSyscallDefer
	}
	if !isStatic || isInterface {
		return LogosSyscallIndirect
	}
	if isSamePackage {
		return LogosSyscallDirect
	}
	return LogosSyscallImported
}

// LogosSyscallKindString names kinds for tests and diagnostics.
func LogosSyscallKindString(k LogosSyscallCallKind) string {
	switch k {
	case LogosSyscallDirect:
		return "direct"
	case LogosSyscallImported:
		return "imported"
	case LogosSyscallIndirect:
		return "indirect"
	case LogosSyscallGo:
		return "go"
	case LogosSyscallDefer:
		return "defer"
	case LogosSyscallInlined:
		return "inlined"
	}
	return "unknown"
}

// LogosSyscallEmission is the consumer-side plan for one ordered note.
// Sync notes lower to an Admit/End pair around the call itself.
// Async notes (go/defer) must not lower to a short pair around the
// scheduling call: admission lives until the child/deferred execution
// finishes, so the consumer keys the execution-time pair off Target
// instead of pairing with newproc/deferproc.
type LogosSyscallEmission struct {
	Target string
	Async  bool
	Order  int
}

// LogosSyscallFlush consumes notes in recorded order and returns the
// emission plan in the same order. It never reorders: output[i].Order
// is strictly increasing. It emits no code itself, so existing call
// emission order is unchanged; it is the tested ordering contract, not
// a production emission input (production admission emits directly via
// logosSyscallForeignCall at the call-lowering site).
func (r *LogosSyscallOrderRecorder) LogosSyscallFlush() []LogosSyscallEmission {
	out := make([]LogosSyscallEmission, len(r.recs))
	for i, n := range r.recs {
		out[i] = LogosSyscallEmission{Target: n.Target, Async: n.Async, Order: n.Order}
	}
	return out
}

// Note: the former per-function SSA side table (state.logosSyscallOrder
// plus logosSyscallOrderNote/Observe/ObserveCall and the KindOfNode,
// InlinedForeign, CallTarget, TargetOf helpers) was write-only in
// production — notes were appended during lowering but never read by
// emitted code — so it was deleted. The standalone recorder above
// (Note/Ordered/Flush/Classify) remains as the tested ordering contract,
// and admission emission below consumes logosSyscallForeignCall directly
// at the single call-lowering site.

// logosSyscallForeignCall reports whether a call site may pass
// uintptr-erased pointers: escape kept an argument alive for the call,
// or an argument converts unsafe.Pointer to uintptr at a site whose
// callee may treat it as a pointer. A callee pragma alone never
// qualifies: scalar-only calls to //go:uintptrkeepalive targets (such as
// syscall.RawSyscall with constant arguments) erase no pointer and gain
// no provenance. Static callees additionally need the pragma; dynamic
// dispatch has no static callee to check, so conversion-shaped arguments
// alone route to the indirect lane, where the runtime no-ops unflagged
// targets. Ordinary calls gain no provenance, so normal code emits
// nothing.
func logosSyscallForeignCall(call *ir.CallExpr) bool {
	if call == nil {
		return false
	}
	if len(call.KeepAlive) > 0 {
		return true
	}
	if !logosSyscallHasUintptrConv(call) {
		return false
	}
	if call.Fun != nil {
		if callee := ir.StaticCalleeName(call.Fun); callee != nil && callee.Func != nil {
			const mask = ir.UintptrKeepAlive | ir.UintptrEscapes
			if callee.Func.Pragma&mask != 0 {
				return true
			}
			return false
		}
	}
	// No static callee: closure value or interface dispatch. A real
	// unsafe.Pointer to uintptr conversion is present (checked above),
	// so the indirect lane applies.
	return true
}

// logosSyscallHasUintptrConv reports whether any call argument converts
// unsafe.Pointer to uintptr, the escape-rewritten shape for syscall-like
// targets.
func logosSyscallHasUintptrConv(call *ir.CallExpr) bool {
	return ir.HasUintptrConv(call)
}

func logosSyscallIsUintptrConv(n ir.Node) bool {
	return ir.IsUintptrConv(n)
}

// Consumer: syscall foreign-admission emission (SSA call lowering).
//
// The order-time recorder above stays the authority on provenance: it is
// append-only, never reorders, and its API (Note/Ordered/Flush/Classify)
// is unchanged. This section is the emission consumer. The single
// call-lowering site (state.call in ssa.go) brackets each qualifying
// synchronous call with an Admit/End pair, gated on
// logosSyscallForeignCall, so only qualifying sites emit and existing
// call emission order is unchanged (the pair rides alongside the call).
//
// Lifetime: only callNormal (synchronous) calls admit around themselves.
// go/defer scheduling calls (callGo/callDefer/callDeferStack) are Async:
// a short pair around newproc/deferproc would release retained cells
// before the syscall runs, so they are skipped here. The deferred or
// spawned bodies are separate functions whose own inner synchronous
// sites emit when those bodies are compiled. Tail calls have no
// post-call point and are skipped.
//
// Rows are materialized here, in this same bracket (not a second emission
// site), so every site keeps exactly one pair: each pair names the address
// of an unsafe.Pointer cell holding a converted pointer plus a nil type
// word (single opaque word). The runtime retains the whole backing through
// its heap closure, so spare capacity is covered and the codegen fixture's
// CALL presence plus the behavioral storms agree. Iovec arrays add one
// sextuple each (array cell, payload element type, Base/Len offsets,
// stride, proven count) in the same buffer with the version bit set in
// n; the runtime expands each to an Unknown root over the Base field
// plus a Whole root over the payload extent, all released by the one
// End after the blocking call completes.
//
// Producer limit: only call-site-provable extents emit sextuples (a
// single struct, a fixed array, or a slice recovered from &s[k] with a
// constant k). An opaque pointer whose count lives in a sibling scalar
// argument, or an element pointer stored through an unsafe.Pointer name,
// has no IR link between count and array here; correlating it needs a
// counted loop at the lowering site, owned by state.call in ssa.go, so
// those sites keep today's Unknown-only behavior, unchanged.

// logosSyscallAdmissionEnabled gates admission emission. The runtime lane
// exists only under GOEXPERIMENT=logosgcmoving on amd64/arm64; everywhere
// else emission stays off and codegen is byte-identical to today.
func logosSyscallAdmissionEnabled() bool {
	return buildcfg.Experiment.LogosGCMoving && (buildcfg.GOARCH == "amd64" || buildcfg.GOARCH == "arm64")
}

// logosSyscallAdmissionCallerEligible reports whether the enclosing
// function may legally take an admission pair. The admission lane may
// allocate or store, so it is illegal wherever an allocating runtime
// call is illegal: anywhere inside the runtime package itself (panic and
// syscall-printing paths reachable from nowritebarrierrec callers reject
// it at build time), and in any function carrying nowritebarrierrec,
// nowritebarrier, or systemstack semantics. A plain //go:nosplit caller
// in an ordinary package stays eligible: it takes the NoSplit pair whose
// callees are themselves nosplit (the fixture's nosplit case pins this).
// There is no //go:yeswritebarrierrec-off pragma; that spelling matches
// nothing and grants nothing.

// logosSyscallAdmissionBoundaryPackage reports whether the package being
// compiled implements the fork/exec boundary itself. The fork child must
// not grow its stack or allocate, so the admission bracket belongs at
// the outermost caller and is never emitted inside the boundary
// implementation (e.g. syscall.forkAndExecInChild1's RawSyscall sites,
// which carry real unsafe.Pointer conversions and would otherwise wrap).
// The support code under internal/runtime/syscall (notably linux) is part
// of the same boundary: the fork child path there cannot allocate or grow
// the stack either, and building admission temps there is restricted, so it
// stays excluded like syscall and internal/syscall.
func logosSyscallAdmissionBoundaryPackage() bool {
	p := base.Ctxt.Pkgpath
	if p == "syscall" || p == "internal/syscall" {
		return true
	}
	if strings.HasPrefix(p, "internal/syscall/") {
		return true
	}
	if p == "internal/runtime/syscall" || strings.HasPrefix(p, "internal/runtime/syscall/") {
		return true
	}
	return false
}

func logosSyscallAdmissionCallerEligible(fn *ir.Func) bool {
	if fn == nil {
		return false
	}
	if base.Ctxt.Pkgpath == "runtime" {
		return false
	}
	if logosSyscallAdmissionBoundaryPackage() {
		return false
	}
	p := fn.Pragma
	if p&(ir.Nowritebarrierrec|ir.Nowritebarrier) != 0 {
		return false
	}
	if p&ir.Systemstack != 0 {
		return false
	}
	return true
}

// logosSyscallIsAdmissionCall reports whether n is itself an
// admission-lane call, which must never gain a wrapper: double admission
// would invalidate the token pairing.
func logosSyscallIsAdmissionCall(n *ir.CallExpr) bool {
	if n == nil || n.Fun == nil {
		return false
	}
	name, ok := n.Fun.(*ir.Name)
	if !ok || name.Sym() == nil || name.Sym().Pkg != ir.Pkgs.Runtime {
		return false
	}
	switch name.Sym().Name {
	case "zgcSyscallAdmit", "zgcSyscallAdmitIndirect", "zgcSyscallAdmitIndirectNoSplit", "zgcSyscallEnd",
		"zgcSyscallAdmitNoSplit", "zgcSyscallEndNoSplit":
		return true
	}
	return false
}

// logosSyscallAdmission is one pending Admit/End pair: the token halves
// returned by the Admit half plus the shape that selects the End half.
type logosSyscallAdmission struct {
	frame, gen *ssa.Value
	nosplit    bool
	ok         bool
}

// logosSyscallWholeDesc is one iovec-array sextuple for the Whole
// lane: the temp cell holding the array pointer, the payload element
// type, the Base/Len field offsets and struct stride, and the proven
// element count (a constant for single structs and fixed arrays, a
// slice-length value for recovered slices).
type logosSyscallWholeDesc struct {
	slot    *ssa.Value
	elem    *types.Type
	baseOff int64
	lenOff  int64
	iovSize int64
	count   *ssa.Value
}

// logosSyscallIovecShape reports whether t is an iovec struct: exactly
// two fields, a typed-pointer Base first and an 8-byte integer Len
// second. The payload element must be byte-like so Len counts bytes and
// the Whole Size is exact; anything else keeps today's Unknown-only
// behavior.
func logosSyscallIovecShape(t *types.Type) (elem *types.Type, baseOff, lenOff int64, ok bool) {
	if t == nil || !t.IsStruct() || t.NumFields() != 2 {
		return nil, 0, 0, false
	}
	bf, lf := t.Field(0), t.Field(1)
	if bf == nil || lf == nil || bf.Type == nil || lf.Type == nil {
		return nil, 0, 0, false
	}
	if !bf.Type.IsPtr() {
		return nil, 0, 0, false
	}
	e := bf.Type.Elem()
	if e != types.Types[types.TUINT8] {
		return nil, 0, 0, false
	}
	if !lf.Type.IsInteger() || lf.Type.Size() != 8 {
		return nil, 0, 0, false
	}
	return e, bf.Offset, lf.Offset, true
}

// logosSyscallWholeCell roots one conversion source exactly like the
// spill path (same temp discipline, same Unknown row) and returns the
// temp cell address for the Whole sextuple. holder is the conversion
// whose operand is rewritten when a fresh temp is needed; src is the
// pointer-valued operand itself, already unwrapped through the inner
// unsafe.Pointer conversion of the normal uintptr(unsafe.Pointer(...))
// form. Consumed operands are kept so the spill pass skips them without
// dropping the array row.
func (s *state) logosSyscallWholeCell(call *ir.CallExpr, holder *ir.ConvExpr, src ir.Node, kept map[ir.Node]bool, slotAddrs *[]*ssa.Value) *ssa.Value {
	if xn, ok := src.(*ir.Name); ok && xn != nil {
		if kept[xn] {
			return s.addr(xn)
		}
		if xn.Class == ir.PAUTO || xn.Class == ir.PPARAM || xn.Class == ir.PPARAMOUT {
			if xn.Addrtaken() || !s.canSSA(xn) {
				kept[xn] = true
				a := s.addr(xn)
				*slotAddrs = append(*slotAddrs, a)
				return a
			}
			optyp := src.Type()
			tmp, _ := s.temp(call.Pos(), optyp)
			tmp.SetAddrtaken(true)
			tmpAddr := s.addr(tmp)
			v := s.expr(xn)
			if v.Type != optyp {
				v = s.newValue1(ssa.OpCopy, optyp, v)
			}
			s.store(optyp, tmpAddr, v)
			kept[xn] = true
			kept[tmp] = true
			*slotAddrs = append(*slotAddrs, tmpAddr)
			return tmpAddr
		}
	}
	optyp := src.Type()
	tmp, _ := s.temp(call.Pos(), optyp)
	tmp.SetAddrtaken(true)
	tmpAddr := s.addr(tmp)
	v := s.expr(src)
	if v.Type != optyp {
		v = s.newValue1(ssa.OpCopy, optyp, v)
	}
	s.store(optyp, tmpAddr, v)
	holder.X = tmp
	kept[tmp] = true
	*slotAddrs = append(*slotAddrs, tmpAddr)
	return tmpAddr
}

// logosSyscallWholeConv attempts one uintptr conversion as an iovec
// array. Provable shapes: &s and &a (single struct / whole fixed
// array), &x[k] with a constant k over a slice or fixed array (count
// from the slice length or the static bound minus k), and an opaque
// Name of pointer-to-fixed-array type (count from the type). Anything
// else — untyped operands, opaque element pointers, non-constant
// indices — is left for the spill pass, unchanged.
func (s *state) logosSyscallWholeConv(call *ir.CallExpr, conv *ir.ConvExpr, kept map[ir.Node]bool, slotAddrs *[]*ssa.Value, descs *[]logosSyscallWholeDesc) {
	holder := conv
	src := conv.X
	if inner, ok := src.(*ir.ConvExpr); ok && inner != nil && inner.X != nil &&
		inner.Op() == ir.OCONVNOP && inner.Type() != nil && inner.Type().IsUnsafePtr() {
		holder, src = inner, inner.X
	}
	pt := src.Type()
	if pt == nil || pt.IsUnsafePtr() || !pt.IsPtr() {
		return
	}
	uintptrType := types.Types[types.TUINTPTR]
	tint := types.Types[types.TINT]
	if addr, ok := src.(*ir.AddrExpr); ok && addr != nil {
		switch x := addr.X.(type) {
		case *ir.Name:
			if x == nil {
				return
			}
			xt := x.Type()
			if elem, bo, lo, ok := logosSyscallIovecShape(xt); ok {
				slot := s.logosSyscallWholeCell(call, holder, src, kept, slotAddrs)
				if slot == nil {
					return
				}
				*descs = append(*descs, logosSyscallWholeDesc{slot: slot, elem: elem, baseOff: bo, lenOff: lo, iovSize: xt.Size(), count: s.constInt(uintptrType, 1)})
				return
			}
			if xt.IsArray() {
				if elem, bo, lo, ok := logosSyscallIovecShape(xt.Elem()); ok && xt.NumElem() != 0 {
					slot := s.logosSyscallWholeCell(call, holder, src, kept, slotAddrs)
					if slot == nil {
						return
					}
					*descs = append(*descs, logosSyscallWholeDesc{slot: slot, elem: elem, baseOff: bo, lenOff: lo, iovSize: xt.Elem().Size(), count: s.constInt(uintptrType, xt.NumElem())})
				}
			}
			return
		case *ir.IndexExpr:
			if x == nil {
				return
			}
			cname, ok := x.X.(*ir.Name)
			if !ok || cname == nil {
				return
			}
			if !ir.IsConst(x.Index, constant.Int) {
				return
			}
			ct := cname.Type()
			var elem *types.Type
			var bo, lo, stride int64
			var lenVal *ssa.Value
			if ct.IsSlice() {
				e, b, l, ok := logosSyscallIovecShape(ct.Elem())
				if !ok {
					return
				}
				elem, bo, lo, stride = e, b, l, ct.Elem().Size()
				sv := s.expr(cname)
				lenVal = s.newValue1(ssa.OpSliceLen, tint, sv)
			} else if ct.IsArray() {
				e, b, l, ok := logosSyscallIovecShape(ct.Elem())
				if !ok || ct.NumElem() == 0 {
					return
				}
				elem, bo, lo, stride = e, b, l, ct.Elem().Size()
				lenVal = s.constInt(tint, ct.NumElem())
			} else {
				return
			}
			kv := s.expr(x.Index)
			if kv.Type == nil || kv.Type.Size() != 8 {
				return
			}
			if kv.Type != tint {
				kv = s.newValue1(ssa.OpCopy, tint, kv)
			}
			slot := s.logosSyscallWholeCell(call, holder, src, kept, slotAddrs)
			if slot == nil {
				return
			}
			count := s.newValue2(ssa.OpSub64, tint, lenVal, kv)
			*descs = append(*descs, logosSyscallWholeDesc{slot: slot, elem: elem, baseOff: bo, lenOff: lo, iovSize: stride, count: count})
			return
		}
		return
	}
	if srcName, ok := src.(*ir.Name); ok && srcName != nil {
		if t := pt.Elem(); t.IsArray() {
			if elem, bo, lo, ok := logosSyscallIovecShape(t.Elem()); ok && t.NumElem() != 0 {
				slot := s.logosSyscallWholeCell(call, holder, srcName, kept, slotAddrs)
				if slot == nil {
					return
				}
				*descs = append(*descs, logosSyscallWholeDesc{slot: slot, elem: elem, baseOff: bo, lenOff: lo, iovSize: t.Elem().Size(), count: s.constInt(uintptrType, t.NumElem())})
			}
		}
		return
	}
}

// logosSyscallWholeArg attempts each conversion of one argument as an
// iovec array, mirroring the spill argument walk (including variadic
// slice literals). Unconsumed conversions fall through to the spill
// pass unchanged.
func (s *state) logosSyscallWholeArg(call *ir.CallExpr, arg ir.Node, kept map[ir.Node]bool, slotAddrs *[]*ssa.Value, descs *[]logosSyscallWholeDesc) {
	var cands []ir.Node
	if lit, ok := arg.(*ir.CompLitExpr); ok && lit != nil {
		for _, e := range lit.List {
			v := e
			if k, ok := v.(*ir.KeyExpr); ok && k != nil {
				v = k.Value
			}
			cands = append(cands, v)
		}
	} else {
		cands = append(cands, arg)
	}
	for _, c := range cands {
		conv, ok := c.(*ir.ConvExpr)
		if !ok || conv == nil || conv.X == nil || !logosSyscallIsUintptrConv(conv) {
			continue
		}
		s.logosSyscallWholeConv(call, conv, kept, slotAddrs, descs)
	}
}

// logosSyscallEmitAdmit emits the Admit half immediately before a
// qualifying synchronous call and returns the pending token. It creates
// SSA (and threads memory) only for qualifying sites; every other site
// returns the zero admission and emits nothing. Only k == callNormal
// (synchronous) admits: go/defer scheduling calls (callGo/callDefer/
// callDeferStack) and tail calls never admit around the scheduler.
func (s *state) logosSyscallEmitAdmit(n *ir.CallExpr, k callKind) logosSyscallAdmission {
	var none logosSyscallAdmission
	if s == nil || s.curfn == nil || n == nil {
		return none
	}
	if !logosSyscallAdmissionEnabled() {
		return none
	}
	if !logosSyscallAdmissionCallerEligible(s.curfn) {
		return none
	}
	if k != callNormal {
		return none
	}
	if n.Op() != ir.OCALLFUNC && n.Op() != ir.OCALLINTER {
		return none
	}
	if logosSyscallIsAdmissionCall(n) {
		return none
	}
	if !logosSyscallForeignCall(n) {
		return none
	}
	indirect := n.Op() == ir.OCALLINTER || ir.StaticCalleeName(n.Fun) == nil
	nosplit := s.curfn.Pragma&ir.Nosplit != 0
	name := "zgcSyscallAdmit"
	switch {
	case nosplit && indirect && logosSyscallIndirectCodeAvailable(n):
		name = "zgcSyscallAdmitIndirectNoSplit"
	case nosplit:
		// The NoSplit pair keeps nosplit callers safe while preserving
		// token pairing. It mints no ownership: covered cells pair with
		// nothing, uncovered cells throw as bad producer protocol.
		name = "zgcSyscallAdmitNoSplit"
	case indirect:
		name = "zgcSyscallAdmitIndirect"
	}
	// Stale-row skip: the indirect lanes (zgcSyscallAdmitIndirect and
	// zgcSyscallAdmitIndirectNoSplit) discard rows on a nil code word
	// (runtime nil check), so emit nothing — not even the rows array —
	// when the code word is unavailable (interface dispatch or a complex
	// callee with no cheap pure PC). Direct, imported, inlined,
	// direct-nosplit, and computed-closure (func value via *ir.Name)
	// lanes are unchanged.
	if (name == "zgcSyscallAdmitIndirect" || name == "zgcSyscallAdmitIndirectNoSplit") && !logosSyscallIndirectCodeAvailable(n) {
		return none
	}
	uptr := types.Types[types.TUNSAFEPTR]
	uintptrType := types.Types[types.TUINTPTR]
	var slotAddrs []*ssa.Value
	kept := make(map[ir.Node]bool, len(n.KeepAlive))
	for _, kn := range n.KeepAlive {
		if kn == nil {
			continue
		}
		kept[kn] = true
		slotAddrs = append(slotAddrs, s.addr(kn))
	}
	// Iovec recovery runs before the spill pass so consumed operands
	// carry both their Unknown row and their sextuple; the spill pass
	// then skips them via kept. Nosplit callers keep today's Unknown
	// rows only: sextuple expansion stops the world.
	var wdescs []logosSyscallWholeDesc
	if !nosplit {
		for _, arg := range n.Args {
			s.logosSyscallWholeArg(n, arg, kept, &slotAddrs, &wdescs)
		}
	}
	for _, arg := range n.Args {
		s.logosSyscallSpillConvArg(n, arg, kept, &slotAddrs)
	}
	if len(slotAddrs) == 0 {
		return none
	}
	nPairs := len(slotAddrs)
	nWdescs := int64(len(wdescs))
	totalRows := int64(2 * nPairs)
	secWords := int64(0)
	if nWdescs > 0 {
		totalRows += 1 + 2*nWdescs
		secWords = 1 + 4*nWdescs
	}
	arrType := types.NewArray(uptr, totalRows)
	types.CalcArraySize(arrType)
	_, rowsAddr := s.temp(n.Pos(), arrType)
	elemPtr := types.NewPtr(uptr)
	ptrSize := int64(types.PtrSize)
	nilTypeWord := s.constNil(uptr)
	for i, slot := range slotAddrs {
		slotVal := slot
		if !slotVal.Type.IsUnsafePtr() {
			slotVal = s.newValue1(ssa.OpCopy, uptr, slotVal)
		}
		off0 := s.newValue1I(ssa.OpOffPtr, elemPtr, int64(2*i)*ptrSize, rowsAddr)
		s.store(uptr, off0, slotVal)
		off1 := s.newValue1I(ssa.OpOffPtr, elemPtr, (int64(2*i)+1)*ptrSize, rowsAddr)
		s.store(uptr, off1, nilTypeWord)
	}
	if nWdescs > 0 {
		secType := types.NewArray(uintptrType, secWords)
		types.CalcArraySize(secType)
		_, secAddr := s.temp(n.Pos(), secType)
		secPtr := secAddr
		if !secPtr.Type.IsUnsafePtr() {
			secPtr = s.newValue1(ssa.OpCopy, uptr, secPtr)
		}
		secOff := s.newValue1I(ssa.OpOffPtr, elemPtr, int64(2*nPairs)*ptrSize, rowsAddr)
		s.store(uptr, secOff, secPtr)
		secElem := types.NewPtr(uintptrType)
		s.store(uintptrType, s.newValue1I(ssa.OpOffPtr, secElem, 0, secAddr), s.constInt(uintptrType, nWdescs))
		for i, d := range wdescs {
			pbase := int64(2*nPairs + 1 + 2*i)
			slotVal := d.slot
			if !slotVal.Type.IsUnsafePtr() {
				slotVal = s.newValue1(ssa.OpCopy, uptr, slotVal)
			}
			s.store(uptr, s.newValue1I(ssa.OpOffPtr, elemPtr, (pbase+0)*ptrSize, rowsAddr), slotVal)
			ev := s.entryNewValue1A(ssa.OpAddr, types.NewPtr(types.Types[types.TUINT8]), reflectdata.TypeLinksym(d.elem), s.sb)
			if !ev.Type.IsUnsafePtr() {
				ev = s.newValue1(ssa.OpCopy, uptr, ev)
			}
			s.store(uptr, s.newValue1I(ssa.OpOffPtr, elemPtr, (pbase+1)*ptrSize, rowsAddr), ev)
			sbase := int64(1 + 4*i)
			s.store(uintptrType, s.newValue1I(ssa.OpOffPtr, secElem, (sbase+0)*ptrSize, secAddr), s.constInt(uintptrType, d.baseOff))
			s.store(uintptrType, s.newValue1I(ssa.OpOffPtr, secElem, (sbase+1)*ptrSize, secAddr), s.constInt(uintptrType, d.lenOff))
			s.store(uintptrType, s.newValue1I(ssa.OpOffPtr, secElem, (sbase+2)*ptrSize, secAddr), s.constInt(uintptrType, d.iovSize))
			countVal := d.count
			if countVal.Type != uintptrType {
				countVal = s.newValue2(ssa.OpConvert, uintptrType, countVal, s.mem())
			}
			s.store(uintptrType, s.newValue1I(ssa.OpOffPtr, secElem, (sbase+3)*ptrSize, secAddr), countVal)
		}
	}
	rowsVal := rowsAddr
	if !rowsVal.Type.IsUnsafePtr() {
		rowsVal = s.newValue1(ssa.OpCopy, uptr, rowsVal)
	}
	count := s.constInt(uintptrType, int64(nPairs))
	if len(wdescs) > 0 {
		// Top bit set at runtime (a constant shift would overflow
		// int64 at compile time); the bit pattern survives the
		// uintptr encoding and marks the iovec section.
		verBit := int64(1)
		verBit <<= 63
		count = s.constInt(uintptrType, int64(nPairs)|verBit)
	}
	results := []*types.Type{types.Types[types.TUNSAFEPTR], types.Types[types.TUINT64]}
	var token []*ssa.Value
	if name == "zgcSyscallAdmitIndirect" || name == "zgcSyscallAdmitIndirectNoSplit" {
		code := s.logosSyscallIndirectCode(n)
		if code == nil {
			// Unreachable after the early available-check above, which
			// skips emission before any SSA (including the rows array).
			// Never synthesise a nil code word the runtime would discard.
			return none
		} else if !code.Type.IsUnsafePtr() {
			code = s.newValue2(ssa.OpConvert, uptr, code, s.mem())
		}
		token = s.rtcall(typecheck.LookupRuntimeFunc(name), true, results, code, rowsVal, count)
	} else {
		token = s.rtcall(typecheck.LookupRuntimeFunc(name), true, results, rowsVal, count)
	}
	return logosSyscallAdmission{frame: token[0], gen: token[1], nosplit: nosplit, ok: true}
}

// logosSyscallSpillConvArg roots one argument's unsafe.Pointer->uintptr
// conversion when escape kept no KeepAlive temp (dynamic dispatch or an
// escapes-only pragma). A memory-resident name (address-taken or
// non-SSA-able) roots by address with no extra evaluation; a
// register-held name (SSA-able, never address-taken) materialises an
// addressable temp of the operand type holding the same value the call
// passes, and the slot names the temp while the call keeps its original
// argument; any other shape evaluates once into a new addressable temp
// that the call reuses, so the pointer evaluates a single time and the
// Slot names the cell the call reads.
func (s *state) logosSyscallSpillConvArg(call *ir.CallExpr, arg ir.Node, kept map[ir.Node]bool, slots *[]*ssa.Value) {
	spill := func(conv *ir.ConvExpr) {
		if conv == nil || conv.X == nil || !logosSyscallIsUintptrConv(conv) {
			return
		}
		if xn, ok := conv.X.(*ir.Name); ok && xn != nil && kept[xn] {
			return
		}
		if xn, ok := conv.X.(*ir.Name); ok && xn != nil {
			switch xn.Class {
			case ir.PAUTO, ir.PPARAM, ir.PPARAMOUT:
				if xn.Addrtaken() || !s.canSSA(xn) {
					kept[xn] = true
					*slots = append(*slots, s.addr(xn))
					return
				}
				// Register-held: the home cell holds garbage while the
				// call passes the OpArg/SSA-var value. Root a temp copy
				// of that same value; leave conv.X alone so the call
				// still passes the original.
				optyp := conv.X.Type()
				tmp, _ := s.temp(call.Pos(), optyp)
				tmp.SetAddrtaken(true)
				tmpAddr := s.addr(tmp)
				v := s.expr(xn)
				if v.Type != optyp {
					v = s.newValue1(ssa.OpCopy, optyp, v)
				}
				s.store(optyp, tmpAddr, v)
				kept[xn] = true
				kept[tmp] = true
				*slots = append(*slots, tmpAddr)
				return
			}
		}
		optyp := conv.X.Type()
		tmp, _ := s.temp(call.Pos(), optyp)
		tmp.SetAddrtaken(true)
		tmpAddr := s.addr(tmp)
		v := s.expr(conv.X)
		if v.Type != optyp {
			v = s.newValue1(ssa.OpCopy, optyp, v)
		}
		s.store(optyp, tmpAddr, v)
		conv.X = tmp
		kept[tmp] = true
		*slots = append(*slots, tmpAddr)
	}
	var cands []ir.Node
	if lit, ok := arg.(*ir.CompLitExpr); ok && lit != nil {
		for _, e := range lit.List {
			v := e
			if k, ok := v.(*ir.KeyExpr); ok && k != nil {
				v = k.Value
			}
			cands = append(cands, v)
		}
	} else {
		cands = append(cands, arg)
	}
	for _, c := range cands {
		if conv, ok := c.(*ir.ConvExpr); ok {
			spill(conv)
		}
	}
}

// logosSyscallIndirectCodeAvailable reports whether IndirectCode can load
// a code pointer without synthesising nil: true only for a func-value
// call through an *ir.Name. Interface dispatch (OCALLINTER) and complex
// callees (non-Name fun) stay false, and the caller skips emission
// entirely — no rows array, no Admit/End pair — instead of emitting rows
// the runtime would discard on a nil code word.
func logosSyscallIndirectCodeAvailable(n *ir.CallExpr) bool {
	if n == nil || n.Op() != ir.OCALLFUNC {
		return false
	}
	fn, ok := n.Fun.(*ir.Name)
	return ok && fn != nil
}

// logosSyscallIndirectCode loads the target code pointer for a dynamic
// function-value call so the indirect lane can check the foreign flag.
// It runs before Admit, so a nil closure faults before any token is
// minted and nothing leaks. Interface dispatch and complex callees have
// no cheap pure PC here and stay nil; the caller skips those sites
// entirely, so nil never reaches an Admit call.
func (s *state) logosSyscallIndirectCode(n *ir.CallExpr) *ssa.Value {
	if n == nil || n.Op() != ir.OCALLFUNC {
		return nil
	}
	fn, ok := n.Fun.(*ir.Name)
	if !ok || fn == nil {
		return nil
	}
	fv := s.expr(fn)
	return s.rawLoad(types.Types[types.TUINTPTR], fv)
}

// logosSyscallEmitEnd emits the End half immediately after the wrapped
// call, pairing with the token from logosSyscallEmitAdmit. The zero
// admission emits nothing. s.rtcall clears prevCall, but the wrapped
// call's ORESULTs lower after this hook and still need it, so prevCall
// is preserved.
func (s *state) logosSyscallEmitEnd(a logosSyscallAdmission) {
	if !a.ok {
		return
	}
	name := "zgcSyscallEnd"
	if a.nosplit {
		name = "zgcSyscallEndNoSplit"
	}
	prev := s.prevCall
	s.rtcall(typecheck.LookupRuntimeFunc(name), true, nil, a.frame, a.gen)
	s.prevCall = prev
}
