// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

import (
	"bytes"
	"cmd/internal/obj"
	"cmd/internal/objabi"
)

// This file holds the code generation of the stack-switching mode of the
// WASM profile (loGo/s M16-S1, SPEC Sec 14.1), which the build selects with
// --wasm-features=stack-switching and the tools see as ctxt.WasmStackSwitch.
//
// Every goroutine runs on its own WebAssembly continuation (the
// stack-switching proposal: cont.new, resume, suspend). runtime.gogo
// suspends the running continuation and the root loop resumes the one of
// the target goroutine, so a goroutine switch leaves the WebAssembly frames
// of the goroutine in place instead of unwinding them. Three cases still
// move control inside one continuation, and this file compiles them:
//
//   - Stack growth: runtime.morestack switches back with the target set to
//     the entry of the function that grew the stack. The stack check runs in
//     a loop, so that function repeats the check when morestack reports an
//     unwind instead of returning through its callers.
//   - A switch back to a parked goroutine targets the frame that called
//     mcall, and recovery targets the frame that deferred the call. The
//     frames above the target return 1 as before; the target frame stops
//     the unwind when the Go stack depth and the PC_F that gogo wrote at
//     -8(SP) match its own frame, and it re-enters its br_table at the PC_B
//     found there. Each compiled function with a resume point saves the
//     depth of its frame in the local MYSP for that comparison. The depth
//     is the high end of the stack of g minus SP, not SP itself: a stack
//     copy (growth, or a shrink during a collection) moves every frame of
//     the goroutine and its stack bounds by the same distance, so a depth
//     saved in a frame that stays on the WebAssembly stack remains valid,
//     and a raw SP would not.
//   - A call that may unwind but never resumes in its caller is ACALLCONT:
//     it propagates the unwind and adds no resume point. The compiler uses
//     it outside the runtime for calls of functions that may yield, so
//     those callers drop their resume points. One call is never ACALLCONT:
//     runtime.mcall, whose caller is the target of the switch back. A frame
//     that is re-entered without a br_table restarts at its first
//     instruction, so the emitter gives every mcall call its resume point
//     (keepSwitchTargets), also in a function outside the runtime that
//     inlined Gosched.
//
// The linker gives the module the types and the tag these instructions
// name: the type index ContFuncType is func(), ContType is cont
// ContFuncType, tag ContTag has type ContFuncType, and table ContTable
// holds (ref null ContType), one entry per goroutine.
const (
	ContFuncType = 1
	ContType     = 2
	ContTag      = 0
	ContTable    = 1

	// BlockContRef is the block type operand of a block whose result is
	// one (ref ContType): Block $BlockContRef. It is the handler label of
	// a resume.
	BlockContRef = -1
)

// stackSwitch is the per-function state of the stack-switching code
// generation. A nil *stackSwitch means the mode is off; every method
// then leaves the code as upstream generates it.
type stackSwitch struct {
	ctxt    *obj.Link
	s       *obj.LSym
	frame   *obj.Prog // last Prog of the frame setup, MYSP is saved after it
	restart *obj.Prog // loop around the stack check
	outer   *obj.Prog // loop around the dispatch, target of a matched unwind
	target  bool      // the function checks unwind targets
}

func init() {
	for _, as := range []obj.As{ATableGet, ATableSet, ATableGrow, ATableSize, ARefFunc, AResume, ACALLCONT} {
		unaryDst[as] = true
	}
}

func newStackSwitch(ctxt *obj.Link, s *obj.LSym) *stackSwitch {
	if !ctxt.WasmStackSwitch {
		return nil
	}
	ss := &stackSwitch{ctxt: ctxt, s: s, frame: s.Func().Text}
	if p := s.Func().Text; p.Link != nil {
		for q, i := p.Link, 0; q != nil && i < 4; q, i = q.Link, i+1 {
			if q.Spadj != 0 {
				ss.frame = q
				break
			}
		}
	}
	ss.keepSwitchTargets()
	return ss
}

// switchesBack reports whether a call of sym can resume its own caller:
// runtime.mcall records the PC and the SP of its caller in g.sched, and the
// runtime.gogo that later runs the goroutine again unwinds the continuation
// to that PC and re-enters the caller there.
func switchesBack(sym *obj.LSym) bool {
	return sym != nil && sym.Name == "runtime.mcall"
}

// keepSwitchTargets turns the ACALLCONT calls of runtime.mcall back into
// calls with a resume point. The compiler makes every call of a function
// outside the runtime an ACALLCONT, on the ground that a goroutine switch
// suspends the continuation and keeps the frame, so the frame is never
// re-entered. A function that calls mcall itself, which happens when it
// inlines Gosched, breaks that ground: it is the frame the switch back
// targets. Without a resume point it has no br_table, so the caller of its
// unwound frame (wasm_cont_entry) calls it again with the PC_B that gogo
// wrote at -8(SP), and it restarts at its first instruction with the SP of
// its frame: the body before the call runs again, one frame lower each
// time. With the resume point the function stops the unwind itself (see
// targetCheck) and continues after the call in the frame that is still on
// the continuation.
func (ss *stackSwitch) keepSwitchTargets() {
	for p := ss.s.Func().Text; p != nil; p = p.Link {
		if p.As == ACALLCONT && p.To.Type == obj.TYPE_MEM && switchesBack(p.To.Sym) {
			p.As = obj.ACALL
		}
	}
}

// restartLoop opens the loop around the stack check.
func (ss *stackSwitch) restartLoop(p *obj.Prog, appendp appendFunc) *obj.Prog {
	if ss == nil {
		return p
	}
	p = appendp(p, ALoop)
	ss.restart = p
	return p
}

// restartEnd closes the loop around the stack check.
func (ss *stackSwitch) restartEnd(p *obj.Prog, appendp appendFunc) *obj.Prog {
	if ss == nil || ss.restart == nil {
		return p
	}
	return appendp(p, AEnd)
}

// isRestart reports whether call is the morestack call of the stack check.
func (ss *stackSwitch) isRestart(call *obj.Prog) bool {
	return ss != nil && ss.restart != nil && (call.To.Sym == morestack || call.To.Sym == morestackNoCtxt)
}

// restartBranch repeats the stack check when morestack unwinds: the runtime
// switched back to this function's entry on a grown stack. morestack saved
// g.sched.sp as the SP of the function at its entry, without the
// return-address slot that the call pushed, and runtime.gogo set SP to that
// value on the new stack before it unwound the WebAssembly stack. SP is
// therefore already the entry SP of this function when the call reports the
// unwind, as it is when upstream re-enters the function at PC_B 0 from
// wasm_pc_f_loop, so the check repeats without touching SP. When the call
// reports no unwind the code falls through with the SP the call produced.
func (ss *stackSwitch) restartBranch(p *obj.Prog, appendp appendFunc) *obj.Prog {
	p = appendp(p, AIf)
	p = appendp(p, ABr)
	p.To = obj.Addr{Type: obj.TYPE_BRANCH, Val: ss.restart}
	return appendp(p, AEnd)
}

// depth leaves the depth of the Go stack at SP on the WebAssembly stack:
// the high end of the stack of g (g.stack.hi, the second word of g) minus
// SP. It is the quantity that a stack copy preserves.
func (ss *stackSwitch) depth(p *obj.Prog, appendp appendFunc) *obj.Prog {
	p = appendp(p, AGet, regAddr(REGG))
	p = appendp(p, AI32WrapI64)
	p = appendp(p, AI32Load, constAddr(int64(ss.ctxt.Arch.PtrSize))) // g.stack.hi
	p = appendp(p, AGet, regAddr(REG_SP))
	return appendp(p, AI32Sub)
}

// planTarget decides whether the function checks unwind targets: a
// compiled function with a resume point that can see an unwind. It saves
// the depth of the frame in MYSP right after the frame setup. The stack
// check, which repeats until the stack is large enough, precedes the frame
// setup, so the save runs once the stack is final for this activation.
func (ss *stackSwitch) planTarget(numResumePoints, numUnwindExits int, appendp appendFunc) {
	if ss == nil || ss.ctxt.IsAsm || numResumePoints == 0 || numUnwindExits == 0 {
		return
	}
	if ss.s.Func().WasmImport != nil || ss.s.Func().WasmExport != nil {
		return
	}
	ss.target = true
	last := appendp(ss.depth(ss.frame, appendp), ASet, regAddr(REG_MYSP))
	if ss.frame.Spadj != 0 {
		// The frame setup ends the pc at which SP is still the entry SP
		// (preprocess advanced pc after it), so the save belongs to the
		// next pc; at the setup's pc it would make the pcsp table report
		// the frame as allocated at the entry.
		for q := ss.frame.Link; ; q = q.Link {
			q.Pc = ss.frame.Pc + 1
			if q == last {
				break
			}
		}
	}
}

// outerLoop opens the loop that a matched unwind branches to.
func (ss *stackSwitch) outerLoop(p *obj.Prog, appendp appendFunc) *obj.Prog {
	if ss == nil || !ss.target {
		return p
	}
	p = appendp(p, ALoop)
	ss.outer = p
	return p
}

// targetCheck follows the unwind exit. When the unwind targets this
// frame (no pause, the depth of SP equals MYSP, the PC_F at -8(SP) is this
// function and its PC_B is not the entry) it re-enters the dispatch at that
// PC_B; otherwise it falls out of the outer loop and returns 1. gogo has
// set g to the goroutine it switched to, so the depth counts from the stack
// that SP points into.
func (ss *stackSwitch) targetCheck(p *obj.Prog, appendp appendFunc) *obj.Prog {
	if ss == nil || !ss.target {
		return p
	}
	p = appendp(p, AGet, regAddr(REG_PAUSE))
	p = appendp(p, AI32Eqz)
	p = ss.depth(p, appendp)
	p = appendp(p, AGet, regAddr(REG_MYSP))
	p = appendp(p, AI32Eq)
	p = appendp(p, AI32And)
	p = appendp(p, AIf)
	p = appendp(p, AGet, regAddr(REG_SP))
	p = appendp(p, AI32Const, constAddr(8))
	p = appendp(p, AI32Sub)
	p = appendp(p, AI64Load, constAddr(0))
	p = appendp(p, AI64Const, constAddr(16))
	p = appendp(p, AI64ShrU)
	p = appendp(p, AI64Const, obj.Addr{Type: obj.TYPE_ADDR, Name: obj.NAME_EXTERN, Sym: ss.s})
	p = appendp(p, AI64Const, constAddr(16))
	p = appendp(p, AI64ShrU)
	p = appendp(p, AI64Eq)
	p = appendp(p, AIf)
	p = appendp(p, AGet, regAddr(REG_SP))
	p = appendp(p, AI32Const, constAddr(8))
	p = appendp(p, AI32Sub)
	p = appendp(p, AI32Load16U, constAddr(0))
	p = appendp(p, ASet, regAddr(REG_PC_B))
	p = appendp(p, AGet, regAddr(REG_PC_B))
	p = appendp(p, ABrIf)
	p.To = obj.Addr{Type: obj.TYPE_BRANCH, Val: ss.outer}
	p = appendp(p, AEnd)
	p = appendp(p, AEnd)
	return appendp(p, AEnd) // end of the outer loop
}

type appendFunc = func(p *obj.Prog, as obj.As, args ...obj.Addr) *obj.Prog

// encodeStackSwitch writes the instructions of this file. It reports
// false for a Prog it does not encode (a plain Block).
func encodeStackSwitch(ctxt *obj.Link, s *obj.LSym, w *bytes.Buffer, p *obj.Prog) bool {
	switch p.As {
	case ABlock:
		if p.From.Type != obj.TYPE_CONST || p.From.Offset != BlockContRef {
			return false
		}
		writeOpcode(w, ABlock)
		w.WriteByte(0x64) // (ref ContType)
		writeUleb128(w, ContType)
	case ATableGet, ATableSet, ATableGrow, ATableSize:
		writeOpcode(w, p.As)
		writeUleb128(w, uint64(p.To.Offset))
	case ARefNullCont:
		w.WriteByte(0xd0)
		writeUleb128(w, ContType)
	case ARefIsNull:
		w.WriteByte(0xd1)
	case ARefFunc:
		if p.To.Type != obj.TYPE_MEM || p.To.Sym == nil {
			ctxt.Diag("RefFunc needs a function symbol: %v", p)
			return true
		}
		w.WriteByte(0xd2)
		s.AddRel(ctxt, obj.Reloc{Type: objabi.R_CALL, Off: int32(w.Len()), Siz: 1, Sym: p.To.Sym})
	case AContNew:
		w.WriteByte(0xe0)
		writeUleb128(w, ContType)
	case ASuspend:
		w.WriteByte(0xe2)
		writeUleb128(w, ContTag)
	case AResume:
		// resume ContType (on ContTag label)
		w.WriteByte(0xe3)
		writeUleb128(w, ContType)
		writeUleb128(w, 1)
		w.WriteByte(0x00)
		writeUleb128(w, ContTag)
		writeUleb128(w, uint64(p.To.Offset))
	default:
		return false
	}
	return true
}
