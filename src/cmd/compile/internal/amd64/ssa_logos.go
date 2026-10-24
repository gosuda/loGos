// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package amd64

import (
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssagen"
	"cmd/internal/obj"
)

func ssaGenRORX(s *ssagen.State, v *ssa.Value) {
	maxRotate := int64(63)
	if v.Op == ssa.OpAMD64RORXL {
		maxRotate = 31
	}
	if v.AuxInt < 0 || v.AuxInt > maxRotate {
		v.Fatalf("rotate amount out of range [0,%d]: %d", maxRotate, v.AuxInt)
	}
	p := s.Prog(v.Op.Asm())
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = v.AuxInt
	p.AddRestSourceReg(v.Args[0].Reg())
	p.To.Type = obj.TYPE_REG
	p.To.Reg = v.Reg()
}
