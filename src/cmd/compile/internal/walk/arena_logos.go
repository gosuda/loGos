// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/escape"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"internal/buildcfg"
)

// walkLogosArenaMake attempts to lower an OMAKESLICE into an arena allocation:
// for constant sizes via OpLogosArenaBump (with logosArenaAlloc fallback),
// and for dynamic sizes via runtime.logosArenaAlloc.
func walkLogosArenaMake(n *ir.MakeExpr, init *ir.Nodes) ir.Node {
	if !buildcfg.Experiment.LogosArena {
		return nil
	}
	if base.Ctxt.Arch.Name != "amd64" {
		return nil
	}
	if ir.CurFunc == nil {
		return nil
	}
	if n.Type().Elem().HasPointers() {
		return nil
	}

	if escape.LogosArenaConstMake(n, ir.CurFunc) {
		// The size is positive and fits one chunk, proven by division
		// before any product of capacity and element size exists.
		if total, ok := escape.LogosArenaConstBytes(n); ok {
			return walkLogosArenaConstMake(n, init, total)
		}
		return nil
	}
	if escape.LogosArenaMake(n, ir.CurFunc) {
		return walkLogosArenaDynamicMake(n, init)
	}
	return nil
}

// walkLogosArenaConstMake lowers a make of a small constant capacity whose
// total size, totalSize bytes, is already known to fit one arena chunk.
func walkLogosArenaConstMake(n *ir.MakeExpr, init *ir.Nodes, totalSize int64) ir.Node {
	lenNode := safeExpr(n.Len, init)
	capExpr := n.Cap
	if capExpr != nil {
		capExpr = safeExpr(capExpr, init)
	} else {
		capExpr = lenNode
	}

	// Standard makeslice validation: len >= 0, cap >= 0, len <= cap.
	nifLen := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OLT, lenNode, ir.NewInt(base.Pos, 0)), []ir.Node{mkcall("panicmakeslicelen", nil, init)}, nil)
	appendWalkStmt(init, nifLen)

	nifCap := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OLT, capExpr, ir.NewInt(base.Pos, 0)), []ir.Node{mkcall("panicmakeslicecap", nil, init)}, nil)
	appendWalkStmt(init, nifCap)

	lenCap := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OGT, typecheck.Conv(lenNode, types.Types[types.TUINT64]), typecheck.Conv(capExpr, types.Types[types.TUINT64])), []ir.Node{mkcall("panicmakeslicecap", nil, init)}, nil)
	appendWalkStmt(init, lenCap)

	rtyp := reflectdata.MakeSliceElemRType(base.Pos, n)
	rndSize := (totalSize + 7) &^ 7

	var ptrTemp *ir.Name
	if totalSize <= 512 && !base.Flag.Race {
		// Fast path: call runtime.logosArenaBump(rndSize) with fallback.
		fn := typecheck.LookupRuntime("logosArenaBump")
		callBump := mkcall1(fn, types.Types[types.TUNSAFEPTR], init, ir.NewInt(base.Pos, rndSize))
		ptrTemp = typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TUNSAFEPTR])
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, ptrTemp, callBump))

		// If ptrTemp == nil, fallback to runtime allocator logosArenaAlloc.
		var ifBody ir.Nodes
		allocFn := typecheck.LookupRuntime("logosArenaAlloc")
		callAlloc := mkcall1(allocFn, types.Types[types.TUNSAFEPTR], &ifBody, rtyp, ir.NewInt(base.Pos, rndSize))
		appendWalkStmt(&ifBody, ir.NewAssignStmt(base.Pos, ptrTemp, callAlloc))

		// Else: bump succeeded; zero noscan memory before exposing to user.
		var elseBody ir.Nodes
		appendWalkStmt(&elseBody, mkcall("memclrNoHeapPointers", nil, &elseBody, ptrTemp, ir.NewInt(base.Pos, rndSize)))

		nif := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OEQ, ptrTemp, typecheck.NodNil()), ifBody, elseBody)
		appendWalkStmt(init, typecheck.Stmt(nif))
	} else {
		// Noscan sizes > 512 or under -race take the runtime allocator directly.
		allocFn := typecheck.LookupRuntime("logosArenaAlloc")
		callAlloc := mkcall1(allocFn, types.Types[types.TUNSAFEPTR], init, rtyp, ir.NewInt(base.Pos, rndSize))
		ptrTemp = typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TUNSAFEPTR])
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, ptrTemp, callAlloc))
	}

	return walkLogosArenaSlice(n, init, ptrTemp, lenNode, capExpr)
}

// walkLogosArenaDynamicMake lowers a make of dynamic capacity. It emits no
// checks and no size arithmetic of its own: runtime.logosArenaMakeSlice takes
// len and cap as int64, as makeslice64 does, and applies the makeslice
// truncation, overflow, maxAlloc and len-before-cap verdicts before it
// allocates, then dispatches to logosArenaAllocScan for pointer-bearing
// element types and logosArenaAlloc otherwise. The stack-buffer and heap
// fallbacks of walkMakeSlice are not involved, so the call has no else branch.
func walkLogosArenaDynamicMake(n *ir.MakeExpr, init *ir.Nodes) ir.Node {
	lenNode := safeExpr(n.Len, init)
	capExpr := n.Cap
	if capExpr != nil {
		capExpr = safeExpr(capExpr, init)
	} else {
		capExpr = lenNode
	}

	rtyp := reflectdata.MakeSliceElemRType(base.Pos, n)
	allocFn := typecheck.LookupRuntime("logosArenaMakeSlice")
	int64Typ := types.Types[types.TINT64]
	ptrTemp := typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TUNSAFEPTR])
	callAlloc := mkcall1(allocFn, types.Types[types.TUNSAFEPTR], init, rtyp, typecheck.Conv(lenNode, int64Typ), typecheck.Conv(capExpr, int64Typ))
	appendWalkStmt(init, ir.NewAssignStmt(base.Pos, ptrTemp, callAlloc))

	return walkLogosArenaSlice(n, init, ptrTemp, lenNode, capExpr)
}

// walkLogosArenaSlice assembles the slice header of n over the arena block
// at ptr and returns the temporary that holds it.
func walkLogosArenaSlice(n *ir.MakeExpr, init *ir.Nodes, ptr *ir.Name, lenNode, capExpr ir.Node) ir.Node {
	sh := ir.NewSliceHeaderExpr(base.Pos, n.Type(), ptr, typecheck.Conv(lenNode, types.Types[types.TINT]), typecheck.Conv(capExpr, types.Types[types.TINT]))
	sh.SetTypecheck(1)
	slice := typecheck.TempAt(base.Pos, ir.CurFunc, n.Type())
	appendWalkStmt(init, ir.NewAssignStmt(base.Pos, slice, sh))
	return slice
}
