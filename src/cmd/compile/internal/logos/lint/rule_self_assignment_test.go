// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"testing"
)

const selfAssignmentLgoSrc = `package p

type S struct {
	f int
}

func badAssignments(x int, s S, other S) {
	x = x // want
	s.f = s.f // want
	var a, b int
	a, b = a, b // want
	a, b = a, 1 // want
	_ = a
	_ = b
}

func cleanAssignments(x, y int, s, other S) {
	x = y
	s.f = other.f
}

func cleanMap(m map[string]int, y int) {
	m["k"] = y
}
`

const selfAssignmentGoSrc = `package p

type S struct {
	f int
}

func badAssignments(x int, s S, other S) {
	x = x // want
	s.f = s.f // want
	var a, b int
	a, b = a, b // want
	a, b = a, 1 // want
	_ = a
	_ = b
}

func cleanAssignments(x, y int, s, other S) {
	x = y
	s.f = other.f
}

func cleanMap(m map[string]int) {
	m["k"] = m["k"]
}
`

func TestSelfAssignment(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", selfAssignmentLgoSrc}, finding{"suspicious::self_assignment", Warn})
	expect(t, KindGo, nil, testSource{"x.go", selfAssignmentGoSrc}, finding{"suspicious::self_assignment", Warn})
}

func TestSelfAssignmentAllow(t *testing.T) {
	const src = `package p

//logos:allow(suspicious::self_assignment)
func allowed(x int) {
	x = x
}
`
	if got := summarize(Run(check(t, KindGo, "x.go", src))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	if got := summarize(Run(check(t, KindLgo, "x.lgo", src))); len(got) != 0 {
		t.Errorf("loGo/s package results %q, want none", got)
	}
}
