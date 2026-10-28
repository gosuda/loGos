// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package json

import "testing"

func TestAllocKeepsExistingPointee(t *testing.T) {
	keep := new(int)
	*keep = 7
	p := keep
	if got := Alloc(&p); got != keep || *got != 7 {
		t.Fatalf("Alloc(non-nil) = %p (%d), want the existing pointee %p (7)", got, *got, keep)
	}
}

func TestAllocFillsNil(t *testing.T) {
	var p *string
	got := Alloc(&p)
	if got == nil || p != got || *got != "" {
		t.Fatalf("Alloc(nil) = %v, field %v; want one new zero pointee stored in the field", got, p)
	}
}
