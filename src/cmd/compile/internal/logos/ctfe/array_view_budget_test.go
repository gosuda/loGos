// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ctfe

// Array-view host sharing (public seam only).
//
// Two slice headers cut from one array (`a[0:4]` twice) carry distinct
// backing-header identities over one underlying array storage. This
// test proves through the public Eval+Host seam that a host value
// holding both views preserves Go aliasing after ingress (a write
// through one view is visible through the other in a single read)
// while two reads of the host value stay isolated (preserving the
// approved fresh per-read isolation rule). It uses generous budgets
// so the verdict is aliasing, not steps or bytes.
//
// Budget ambiguity is intentionally not asserted here. Notes:
//   - valueSize keys slices by backing-header pointer
//     (seen[v.slice.backing]); hostCopy dedups storage by storeKey
//     ({first *Value, length}).
//   - Two `a[0:4]` headers over one [4]int thus share a storeKey but
//     not a header identity: hostCopy bills 32B + 6 slots once, while
//     the post-ingest checkSize/valueSize scan bills 2 x 32B = 64B.
//   - Config says "distinct bytes" but the suite only observes visible
//     values; overlapping-window policy is unclear, so no 40-byte
//     control or CodeTooLarge/detail wording is pinned here. Full note
//     also saved to local://array-view-budget-finding.md.

import (
	"testing"
)

// TestArrayViewHostSharingAliasIsolation installs a host value holding
// two full views over one [4]int storage, then checks single-read
// sharing and two-read isolation through Eval only.
func TestArrayViewHostSharingAliasIsolation(t *testing.T) {
	intT := BasicType("int", 8)
	arr2T := &Type{Kind: KArray, Len: 2, Elem: &Type{Kind: KSlice, Elem: intT}}
	big := Config{MaxSteps: 1 << 20, MaxValueBytes: 1 << 20, WordSize: 8}

	build := newFakeHost(t, 8)
	buildFn := parseFunc(t, `package p
func Build() [2][]int {
	a := [4]int{1, 2, 3, 4}
	s1 := a[0:4]
	s2 := a[0:4]
	return [2][]int{s1, s2}
}`, true, nil, []*Type{arr2T})
	build.funcs[buildFn.Name] = buildFn
	shared, _, err := Eval(build, big, parseExpr(t, `Build()`))
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	h := newFakeHost(t, 8)
	h.vars["H"] = shared
	cfg := DefaultConfig()

	if v, _ := evalOne(t, h, cfg, parseExpr(t, `len(H)`)); v.Int64() != 2 {
		t.Fatalf("len(H) = %d, want 2", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `len(H[0])`)); v.Int64() != 4 {
		t.Fatalf("len(H[0]) = %d, want 4", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `H[0][0]`)); v.Int64() != 1 {
		t.Fatalf("H[0][0] = %d, want 1", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `H[1][3]`)); v.Int64() != 4 {
		t.Fatalf("H[1][3] = %d, want 4", v.Int64())
	}

	aliasFn := parseFunc(t, `package p
func CheckAlias() int {
	h := H
	h[0][0] = 99
	return h[1][0]
}`, true, nil, []*Type{intT})
	h.funcs[aliasFn.Name] = aliasFn
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `CheckAlias()`)); v.Int64() != 99 {
		t.Fatalf("CheckAlias() = %d, want 99 (two array views share one backing)", v.Int64())
	}

	isoFn := parseFunc(t, `package p
func CheckIso() int {
	x := H
	y := H
	x[0][0] = 99
	return y[0][0]
}`, true, nil, []*Type{intT})
	h.funcs[isoFn.Name] = isoFn
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `CheckIso()`)); v.Int64() != 1 {
		t.Fatalf("CheckIso() = %d, want 1 (two reads isolated)", v.Int64())
	}
	if v, _ := evalOne(t, h, cfg, parseExpr(t, `H[0][0]`)); v.Int64() != 1 {
		t.Fatalf("host H[0][0] after mutation = %d, want 1", v.Int64())
	}
}
