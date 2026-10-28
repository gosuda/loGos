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

	isConst := escape.LogosArenaConstMake(n, ir.CurFunc)
	isDynamic := escape.LogosArenaMake(n, ir.CurFunc)
	if !isConst && !isDynamic {
		return nil
	}

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

	elemSize := n.Type().Elem().Size()
	rtyp := reflectdata.MakeSliceElemRType(base.Pos, n)

	if isConst {
		capVal := ir.Int64Val(capExpr)
		totalSize := capVal * elemSize
		if totalSize <= 0 || totalSize > escape.LogosArenaMaxBytes {
			return nil
		}
		rndSize := (totalSize + 7) &^ 7

		// Call runtime.logosArenaBump(rndSize).
		fn := typecheck.LookupRuntime("logosArenaBump")
		callBump := mkcall1(fn, types.Types[types.TUNSAFEPTR], init, ir.NewInt(base.Pos, rndSize))
		ptrTemp := typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TUNSAFEPTR])
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

		sh := ir.NewSliceHeaderExpr(base.Pos, n.Type(), ptrTemp, typecheck.Conv(lenNode, types.Types[types.TINT]), typecheck.Conv(capExpr, types.Types[types.TINT]))
		sh.SetTypecheck(1)
		slice := typecheck.TempAt(base.Pos, ir.CurFunc, n.Type())
		appendWalkStmt(init, ir.NewAssignStmt(base.Pos, slice, sh))
		return slice
	}

	// Dynamic make: call runtime.logosArenaAlloc(rtyp, size).
	allocFn := typecheck.LookupRuntime("logosArenaAlloc")
	capConv := typecheck.Conv(capExpr, types.Types[types.TUINTPTR])
	var sizeNode ir.Node
	if elemSize == 1 {
		sizeNode = capConv
	} else {
		sizeNode = typecheck.Expr(ir.NewBinaryExpr(base.Pos, ir.OMUL, capConv, ir.NewInt(base.Pos, elemSize)))
	}
	ptrTemp := typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TUNSAFEPTR])
	callAlloc := mkcall1(allocFn, types.Types[types.TUNSAFEPTR], init, rtyp, sizeNode)
	appendWalkStmt(init, ir.NewAssignStmt(base.Pos, ptrTemp, callAlloc))

	sh := ir.NewSliceHeaderExpr(base.Pos, n.Type(), ptrTemp, typecheck.Conv(lenNode, types.Types[types.TINT]), typecheck.Conv(capExpr, types.Types[types.TINT]))
	sh.SetTypecheck(1)
	slice := typecheck.TempAt(base.Pos, ir.CurFunc, n.Type())
	appendWalkStmt(init, ir.NewAssignStmt(base.Pos, slice, sh))
	return slice
}
