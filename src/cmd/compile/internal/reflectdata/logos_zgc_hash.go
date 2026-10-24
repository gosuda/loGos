// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflectdata

import (
	"fmt"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

// identitySignature is eqSignature for the hash and equality functions of
// a type descriptor, which maps and interface comparisons call. Under -lb
// (-gc=zgc) objects move (SPEC Sec 19, M19-S7), and the runtime reads map
// keys and interface data words without load barriers, so a stored word
// may still name the source of a relocated object. Every pointer,
// unsafe.Pointer and channel word therefore becomes sigPointer: it hashes
// through runtime.zgcptrhash (side-table identity of its object plus the
// offset) and compares through runtime.zgcptrequal (current canonical
// addresses). Without -lb it is eqSignature. EqFor, which lowers ==
// between values the compiled code loaded through barriers, keeps
// eqSignature.
func identitySignature(t *types.Type) string {
	if !base.LogosLoadBarrier {
		return eqSignature(t)
	}
	e := eqSigBuilder{ptrIdentity: true}
	if !base.Ctxt.Arch.CanMergeLoads {
		if a := t.Alignment(); a != int64(types.PtrSize) {
			fmt.Fprintf(&e.r, "%c%d", sigAlign, a)
		}
	}
	e.build(t)
	e.flush(true)
	return e.r.String()
}

// zgcRuntimeFunc returns runtime.name of type
// func(unsafe.Pointer, X) result, where X is unsafe.Pointer for
// zgcptrequal and uintptr for zgcptrhash. It is declared here rather than
// in the generated builtin table: only functions compiled under -lb call
// these, and the runtime defines both in every configuration.
func zgcRuntimeFunc(name string, result *types.Type) *ir.Name {
	sym := ir.Pkgs.Runtime.Lookup(name)
	if sym.Def == nil {
		second := types.Types[types.TUINTPTR]
		if name == "zgcptrequal" {
			second = types.Types[types.TUNSAFEPTR]
		}
		sig := types.NewSignature(nil,
			[]*types.Field{
				types.NewField(src.NoXPos, nil, types.Types[types.TUNSAFEPTR]),
				types.NewField(src.NoXPos, nil, second),
			},
			[]*types.Field{
				types.NewField(src.NoXPos, nil, result),
			})
		sym.Def = ir.NewFunc(src.NoXPos, src.NoXPos, sym, sig).Nname
	}
	return sym.Def.(*ir.Name)
}
