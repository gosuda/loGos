// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssagen

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"internal/buildcfg"
	"strings"
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
	for _, arg := range call.Args {
		if logosSyscallIsUintptrConv(arg) {
			return true
		}
	}
	return false
}

// logosSyscallIsUintptrConv unwraps one unsafe.Pointer to uintptr
// conversion, including elements of a variadic slice literal.
func logosSyscallIsUintptrConv(n ir.Node) bool {
	switch n := n.(type) {
	case *ir.ConvExpr:
		if n.Op() != ir.OCONVNOP {
			return false
		}
		if n.Type() == nil || !n.Type().IsUintptr() {
			return false
		}
		return n.X != nil && n.X.Type() != nil && n.X.Type().IsUnsafePtr()
	case *ir.CompLitExpr:
		for _, e := range n.List {
			v := e
			if k, ok := v.(*ir.KeyExpr); ok {
				v = k.Value
			}
			if logosSyscallIsUintptrConv(v) {
				return true
			}
		}
	}
	return false
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
// Wiring stage: the emitted rows are empty (nil, 0). The runtime no-ops
// empty admissions, so this is behavior-preserving bring-up wiring whose
// CALL presence the codegen fixture verifies. Materializing real rows off
// the KeepAlive temps is follow-up work; it must reuse this same bracket
// (not a second emission site) so every site keeps exactly one pair.

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
func logosSyscallAdmissionBoundaryPackage() bool {
	p := base.Ctxt.Pkgpath
	if p == "syscall" || p == "internal/syscall" {
		return true
	}
	return strings.HasPrefix(p, "internal/syscall/")
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
	case "zgcSyscallAdmit", "zgcSyscallAdmitIndirect", "zgcSyscallEnd",
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
	case nosplit:
		// No indirect NoSplit entry point exists; with empty rows the
		// lane no-ops, so the NoSplit pair keeps nosplit callers safe
		// while preserving token pairing.
		name = "zgcSyscallAdmitNoSplit"
	case indirect:
		name = "zgcSyscallAdmitIndirect"
	}
	uptr := types.Types[types.TUNSAFEPTR]
	rows := s.constNil(uptr)
	count := s.constInt(types.Types[types.TUINTPTR], 0)
	results := []*types.Type{types.Types[types.TUNSAFEPTR], types.Types[types.TUINT64]}
	var token []*ssa.Value
	if name == "zgcSyscallAdmitIndirect" {
		code := s.constNil(uptr)
		token = s.rtcall(typecheck.LookupRuntimeFunc(name), true, results, code, rows, count)
	} else {
		token = s.rtcall(typecheck.LookupRuntimeFunc(name), true, results, rows, count)
	}
	return logosSyscallAdmission{frame: token[0], gen: token[1], nosplit: nosplit, ok: true}
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
