// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lint

import (
	"testing"
)

const deferInLoopLgoSrc = `package p

func cleanup() {}

func badLoop(files []string) {
	for range files {
		defer cleanup() // want
	}
	for i := 0; i < len(files); i++ {
		if i%2 == 0 {
			defer cleanup() // want
		}
	}
}

func cleanLoop(files []string) {
	defer cleanup()
	for range files {
		func() {
			defer cleanup()
		}()
	}
}
`

const deferInLoopGoSrc = `package p

func cleanup() {}

func badLoop(files []string) {
	for range files {
		defer cleanup() // want
	}
	for i := 0; i < len(files); i++ {
		if i%2 == 0 {
			defer cleanup() // want
		}
	}
}

func cleanLoop(files []string) {
	defer cleanup()
	for range files {
		func() {
			defer cleanup()
		}()
	}
}
`

func TestDeferInLoop(t *testing.T) {
	expect(t, KindLgo, nil, testSource{"x.lgo", deferInLoopLgoSrc}, finding{"suspicious::defer_in_loop", Warn})
	expect(t, KindGo, nil, testSource{"x.go", deferInLoopGoSrc}, finding{"suspicious::defer_in_loop", Warn})
}

func TestDeferInLoopAllow(t *testing.T) {
	const src = `package p

func cleanup() {}

//logos:allow(suspicious::defer_in_loop)
func allowed(n int) {
	for i := range n {
		_ = i
		defer cleanup()
	}
}
`
	if got := summarize(Run(check(t, KindGo, "x.go", src))); len(got) != 0 {
		t.Errorf("Go package results %q, want none", got)
	}
	if got := summarize(Run(check(t, KindLgo, "x.lgo", src))); len(got) != 0 {
		t.Errorf("loGo/s package results %q, want none", got)
	}
}
