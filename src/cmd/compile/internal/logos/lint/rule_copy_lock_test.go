// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"testing"
)

const copyLockLgoSrc = `package p

import "sync"

type State struct {
	mu    sync.Mutex
	count int
}

func (s State) BadRecv() int { // want
	return s.count
}

func (s *State) GoodRecv() int {
	return s.count
}

func passByVal(s State) int { // want
	return s.count
}

func passByPtr(s *State) int {
	return s.count
}

func copyAssign() {
	var s1 State
	var s2 State
	s2 = s1 // want
	var s3 = s1 // want
	_ = &s2
	_ = &s3
}

func copyReturn() State {
	var s State
	return s // want
}

func cleanForms() {
	ptr := &State{}
	_ = ptr
	passByPtr(ptr)
}
`

const copyLockGoSrc = `package p

import "sync"

type State struct {
	mu    sync.Mutex
	count int
}

func (s State) BadRecv() int { // want
	return s.count
}

func (s *State) GoodRecv() int {
	return s.count
}

func passByVal(s State) int { // want
	return s.count
}

func passByPtr(s *State) int {
	return s.count
}

func copyAssign() {
	var s1 State
	var s2 State
	s2 = s1 // want
	var s3 = s1 // want
	_ = &s2
	_ = &s3
}

func copyReturn() State {
	var s State
	return s // want
}

func cleanForms() {
	ptr := &State{}
	_ = ptr
	passByPtr(ptr)
}
`

func TestCopyLock(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", copyLockLgoSrc}, finding{"suspicious::copy_lock", Warn})
	expect(t, KindGo, nil, testSource{"x.go", copyLockGoSrc}, finding{"suspicious::copy_lock", Warn})
}

func TestCopyLockAllow(t *testing.T) {
	const src = `package p

import "sync"

type State struct {
	mu sync.Mutex
}

//logos:allow(suspicious::copy_lock)
func allowed(s State) {}
`
	if got := summarize(Run(check(t, KindGo, "x.go", src))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	if got := summarize(Run(check(t, KindLgo, "x.lgo", src))); len(got) != 0 {
		t.Errorf("loGo/s package results %q, want none", got)
	}
}
