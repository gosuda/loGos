// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ctfe

// Statement-level semantic attacks on the M5-S2 interpreter, pinned
// through the public Eval seam with the fake host from ctfe_test.go:
//
//   - true, false, and nil are ordinary identifiers declared in the
//     universe block, so a parameter, short variable declaration, var,
//     or const of the same name hides the predeclared one for the whole
//     of its scope and no longer hides it once the scope ends;
//   - an expression statement may call a function with any number of
//     results and discards all of them, keeping every side effect and
//     every failure of the call.
//
// Every expected value is a literal derived by hand from Go's
// semantics, never read back from interpreter output, and every probe
// asserts through the public Int64 accessor: bool results go through
// Truth, which maps true to 1 and false to 0. Helper funcs are const
// funcs that mutate only slices handed to them, which share their
// backing store with the caller.

import (
	"strings"
	"testing"
)

var (
	stmtIntT   = BasicType("int", 8)
	stmtBoolT  = BasicType("bool", 8)
	stmtSliceT = &Type{Kind: KSlice, Elem: stmtIntT}
)

// stmtFunc is one const func declaration with its parameter and
// result types.
type stmtFunc struct {
	src     string
	params  []*Type
	results []*Type
}

func stmtIntFn(src string, params ...*Type) stmtFunc {
	return stmtFunc{src: src, params: params, results: []*Type{stmtIntT}}
}

func stmtBoolFn(src string, params ...*Type) stmtFunc {
	return stmtFunc{src: src, params: params, results: []*Type{stmtBoolT}}
}

var stmtTruth = stmtIntFn("package p\nfunc Truth(b bool) int {\n\tif b {\n\t\treturn 1\n\t}\n\treturn 0\n}", stmtBoolT)

// stmtHost registers every fn as a const func on a fresh 64-bit host.
func stmtHost(t *testing.T, fns ...stmtFunc) *fakeHost {
	t.Helper()
	h := newFakeHost(t, 8)
	for _, f := range fns {
		fn := parseFunc(t, f.src, true, f.params, f.results)
		h.funcs[fn.Name] = fn
	}
	return h
}

// stmtProbe is one int-valued call expression and the value Go
// evaluates it to.
type stmtProbe struct {
	call string
	want int64
}

// stmtWantInt evaluates call on h and compares it with want.
func stmtWantInt(t *testing.T, h *fakeHost, p stmtProbe) {
	t.Helper()
	v, _ := evalOne(t, h, DefaultConfig(), parseExpr(t, p.call))
	mustInt(t, v, p.want, p.call)
}

// TestSemanticStatementPredeclaredShadowing pins Go's scoping of true,
// false, and nil. Each group is one func probed by several calls: bool
// parameters read through a return, a negation, and a && operand; typed
// shadows (int, slice); each declaration form; a block-scoped shadow
// that must give way to the predeclared constant at scope exit; and
// unshadowed controls that must keep evaluating as the predeclared
// constants.
func TestSemanticStatementPredeclaredShadowing(t *testing.T) {
	for _, g := range []struct {
		name   string
		fn     stmtFunc
		probes []stmtProbe
	}{
		{"true parameter",
			stmtBoolFn("package p\nfunc T(true bool) bool { return true }", stmtBoolT),
			[]stmtProbe{{`Truth(T(false))`, 0}, {`Truth(T(true))`, 1}}},
		{"false parameter",
			stmtBoolFn("package p\nfunc F(false bool) bool { return false }", stmtBoolT),
			[]stmtProbe{{`Truth(F(true))`, 1}, {`Truth(F(false))`, 0}}},
		{"negated false parameter",
			stmtBoolFn("package p\nfunc N(false bool) bool { return !false }", stmtBoolT),
			[]stmtProbe{{`Truth(N(true))`, 0}, {`Truth(N(false))`, 1}}},
		{"true parameter as left operand of &&",
			stmtBoolFn("package p\nfunc A(true bool, x bool) bool { return true && x }", stmtBoolT, stmtBoolT),
			[]stmtProbe{{`Truth(A(false, true))`, 0}, {`Truth(A(true, true))`, 1}}},

		// Typed shadows: true as int, nil as []int. Each is valid Go and
		// only type-checks because the name is a variable.
		{"true parameter typed int",
			stmtIntFn("package p\nfunc P(true int) int { return true + 1 }", stmtIntT),
			[]stmtProbe{{`P(41)`, 42}}},
		{"nil parameter typed slice",
			stmtIntFn("package p\nfunc S(nil []int) int { return len(nil) }", stmtSliceT),
			[]stmtProbe{{`S([]int{1, 2, 3})`, 3}}},

		// Declaration forms. L binds x, x+1, x+2 to true, false, nil:
		// L(1) = 1*100 + 2*10 + 3; L(4) = 4*100 + 5*10 + 6.
		{"short declarations",
			stmtIntFn("package p\nfunc L(x int) int {\n\ttrue := x\n\tfalse := x + 1\n\tnil := x + 2\n\treturn true*100 + false*10 + nil\n}", stmtIntT),
			[]stmtProbe{{`L(1)`, 123}, {`L(4)`, 456}}},
		{"var declaration",
			stmtIntFn("package p\nfunc V() int {\n\tvar false = 7\n\treturn false + 1\n}"),
			[]stmtProbe{{`V()`, 8}}},
		{"const declaration",
			stmtIntFn("package p\nfunc C() int {\n\tconst nil = 40\n\treturn nil + 2\n}"),
			[]stmtProbe{{`C()`, 42}}},

		// The inner false is 5 only inside the if body; afterwards the
		// predeclared false is back: `if false` is skipped, `if !false`
		// runs. x true: 5 + 1000; x false: 1000.
		{"block shadow ends with its scope",
			stmtIntFn("package p\nfunc Scope(x bool) int {\n\tn := 0\n\tif x {\n\t\tfalse := 5\n\t\tn += false\n\t}\n\tif false {\n\t\tn += 100\n\t}\n\tif !false {\n\t\tn += 1000\n\t}\n\treturn n\n}", stmtBoolT),
			[]stmtProbe{{`Scope(true)`, 1005}, {`Scope(false)`, 1000}}},

		// Controls: nothing shadowed, so true, false, and nil keep their
		// predeclared meaning (1 + 10; 1 + 20 for the nil probes).
		{"unshadowed true and false",
			stmtIntFn("package p\nfunc Unshadowed() int {\n\tn := 0\n\tif true {\n\t\tn += 1\n\t}\n\tif !false {\n\t\tn += 10\n\t}\n\treturn n\n}"),
			[]stmtProbe{{`Unshadowed()`, 11}}},
		{"unshadowed nil",
			stmtIntFn("package p\nfunc NilControl() int {\n\tvar a []int\n\tb := []int{1}\n\tn := 0\n\tif a == nil {\n\t\tn += 1\n\t}\n\tif b != nil {\n\t\tn += 20\n\t}\n\treturn n\n}"),
			[]stmtProbe{{`NilControl()`, 21}}},
	} {
		t.Run(g.name, func(t *testing.T) {
			h := stmtHost(t, g.fn, stmtTruth)
			for _, p := range g.probes {
				t.Run(p.call, func(t *testing.T) {
					stmtWantInt(t, h, p)
				})
			}
		})
	}
}

// TestSemanticStatementDiscardedCallResults pins Go's expression
// statement: a call yielding zero or several results may stand alone,
// its results are dropped, and its side effects and failures stay.
// Zero- and multi-result helpers reach the caller only through
// statements, so a statement path that demands exactly one value
// cannot run them at all.
func TestSemanticStatementDiscardedCallResults(t *testing.T) {
	h := stmtHost(t,
		stmtFunc{
			src:    "package p\nfunc Set(s []int, i int, v int) { s[i] = v }",
			params: []*Type{stmtSliceT, stmtIntT, stmtIntT},
		},
		// Two results of different types.
		stmtFunc{
			src:     "package p\nfunc Tick(s []int) (int, bool) {\n\ts[0]++\n\treturn s[0], true\n}",
			params:  []*Type{stmtSliceT},
			results: []*Type{stmtIntT, stmtBoolT},
		},
		// Index 5 of a 1-element slice.
		stmtFunc{
			src:     "package p\nfunc Boom(s []int) (int, int) { return s[5], 0 }",
			params:  []*Type{stmtSliceT},
			results: []*Type{stmtIntT, stmtIntT},
		},

		// s ends as [4 0 5]: 4*100 + 0*10 + 5.
		stmtIntFn("package p\nfunc ZeroResult() int {\n\ts := []int{0, 0, 0}\n\tSet(s, 0, 4)\n\tSet(s, 2, 5)\n\treturn s[0]*100 + s[1]*10 + s[2]\n}"),
		// One call, one tick: a dropped call that never ran leaves 0 and
		// one that ran twice leaves 2.
		stmtIntFn("package p\nfunc MultiResult() int {\n\ts := []int{0}\n\tTick(s)\n\treturn s[0]\n}"),
		// Go allows parenthesized call statements: two ticks.
		stmtIntFn("package p\nfunc Parenthesized() int {\n\ts := []int{0}\n\t(Tick(s))\n\t((Tick(s)))\n\treturn s[0]\n}"),
		stmtIntFn("package p\nfunc Failing() int {\n\ts := []int{1}\n\tBoom(s)\n\treturn 1\n}"),
	)

	for _, p := range []stmtProbe{
		{`ZeroResult()`, 405},
		{`MultiResult()`, 1},
		{`Parenthesized()`, 2},
	} {
		t.Run(p.call, func(t *testing.T) {
			stmtWantInt(t, h, p)
		})
	}

	// The call body's own failure surfaces unchanged: it is neither
	// swallowed with the dropped results nor replaced by a statement
	// level error.
	t.Run("failure inside a discarded call surfaces", func(t *testing.T) {
		cerr := evalErr(t, h, DefaultConfig(), parseExpr(t, `Failing()`))
		if cerr.Code != CodeEval || !strings.Contains(cerr.Detail, "index") {
			t.Fatalf("error = %v (code %v), want CodeEval index failure", cerr, cerr.Code)
		}
	})
}
