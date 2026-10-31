// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gc

import (
	"testing"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"internal/abi"
)

// The foreign-target flag is emitted only on amd64 and arm64, where the
// moving collector and the ABIInternal-wrapper calling convention are
// supported. Every other architecture is excluded by design: no flag,
// no wrapper marking, no runtime consult, byte-identical objects.

func TestLogosForeignFlagArchEnabled(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		if !logosForeignFlagArchEnabled(arch) {
			t.Errorf("arch %q must carry the foreign-target flag", arch)
		}
	}
}

func TestLogosForeignFlagArchSkipsUnsupported(t *testing.T) {
	for _, arch := range []string{"386", "riscv64", "wasm", "loong64", "ppc64", "s390x"} {
		if logosForeignFlagArchEnabled(arch) {
			t.Errorf("arch %q must stay unmarked (unsupported moving-collector wrapper shape)", arch)
		} else {
			t.Logf("arch %q skipped: unsupported moving-collector wrapper shape", arch)
		}
	}
}

// Propagation decision: a pragma on any function in a symbol group
// authorizes the group, so a generated wrapper sharing its bodyless
// declaration's Sym inherits the flag even though the wrapper itself
// carries no pragma. Groups with no pragma anywhere stay unmarked.

func TestLogosForeignMarkedSyms(t *testing.T) {
	decl := &types.Sym{Name: "bodylessDecl"}
	plain := &types.Sym{Name: "ordinary"}
	views := []logosForeignFuncView{
		{Sym: decl, Pragma: ir.UintptrKeepAlive},
		{Sym: decl, Pragma: 0},
		{Sym: plain, Pragma: 0},
	}
	got := logosForeignMarkedSyms(views)
	if !got[decl] {
		t.Fatal("shared Sym with foreign declaration pragma must be marked (wrapper inherits)")
	}
	if got[plain] {
		t.Fatal("Sym with no foreign pragma must stay unmarked")
	}
	if got[nil] {
		t.Fatal("nil Sym must never be marked")
	}
}

// Flag-level propagation: a bodyless declaration's pragma authorizes the
// shared symbol group, and the production write lands
// FuncFlagUintptrForeign on the ABIInternal wrapper symbol — the one
// function values reference — while the raw ABI0 body stays unmarked.
// Runs on a scratch Link so no test pollutes the compiler context; the
// production test binary only runs amd64/arm64 moving builds through
// the arch gate above.

func TestLogosForeignApplyFlagBodylessWrapper(t *testing.T) {
	old := base.Ctxt
	base.Ctxt = &obj.Link{}
	defer func() { base.Ctxt = old }()
	pkg := types.NewPkg("example.com/foreign", "foreign")
	sym := &types.Sym{Name: "Syscall", Pkg: pkg}
	// Mirror generation: declaration carries the pragma, the ABI wrapper
	// shares the Sym but carries none.
	views := []logosForeignFuncView{
		{Sym: sym, Pragma: ir.UintptrKeepAlive},
		{Sym: sym, Pragma: 0},
	}
	if !logosForeignMarkedSyms(views)[sym] {
		t.Fatal("shared Sym with foreign declaration pragma must be authorized")
	}
	// Simulate the assembled wrapper TEXT (ABIInternal flavor only).
	sym.LinksymABI(obj.ABIInternal).NewFuncInfo()
	logosForeignApplyFlag(sym)
	if sym.LinksymABI(obj.ABIInternal).Func().FuncFlag&abi.FuncFlagUintptrForeign == 0 {
		t.Fatal("ABIInternal wrapper symbol must carry FuncFlagUintptrForeign")
	}
	if sym.LinksymABI(obj.ABI0).Func() != nil && sym.LinksymABI(obj.ABI0).Func().FuncFlag&abi.FuncFlagUintptrForeign != 0 {
		t.Fatal("raw ABI0 body must stay unmarked")
	}
}
