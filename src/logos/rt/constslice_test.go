// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rt

import "testing"

type covGetter interface{ Get() int }

type covWide interface {
	Get() int
	Extra() int
}

type covValue struct{ n int }

func (v covValue) Get() int { return v.n }

type covPointer struct{ n int }

func (p *covPointer) Get() int {
	if p == nil {
		return -1
	}
	return p.n
}

type covBoth struct{ n int }

func (b covBoth) Get() int   { return b.n }
func (b covBoth) Extra() int { return b.n * 2 }

func TestCovSliceNilAndEmpty(t *testing.T) {
	var nilSrc []covValue
	if got := CovSlice[covGetter](nilSrc); got != nil {
		t.Errorf("nil source gave %v, want nil", got)
	}
	got := CovSlice[covGetter]([]covValue{})
	if got == nil || len(got) != 0 {
		t.Errorf("empty source gave %#v, want a non-nil empty slice", got)
	}
}

func TestCovSliceElements(t *testing.T) {
	got := CovSlice[covGetter]([]covValue{{1}, {2}, {3}})
	if len(got) != 3 || cap(got) != 3 {
		t.Fatalf("len %d cap %d, want 3 and 3", len(got), cap(got))
	}
	for i, g := range got {
		if g.Get() != i+1 {
			t.Errorf("element %d is %d, want %d", i, g.Get(), i+1)
		}
	}
}

func TestCovSliceOwnsItsBackingArray(t *testing.T) {
	src := []*covPointer{{1}, {2}}
	got := CovSlice[covGetter](src)
	got[0] = nil
	src[1] = &covPointer{9}
	if src[0] == nil || src[0].Get() != 1 {
		t.Errorf("writing the result reached the source: %v", src)
	}
	if got[1].Get() != 2 {
		t.Errorf("writing the source reached the result: %d", got[1].Get())
	}
	src[0].n = 5
	src[1] = nil
	if got[1] == nil || got[1].Get() != 2 {
		t.Errorf("result element lost its pointee: %v", got[1])
	}
	shared := CovSlice[covGetter]([]*covPointer{src[0]})
	src[0].n = 7
	if shared[0].Get() != 7 {
		t.Errorf("pointer element did not share its pointee: %d", shared[0].Get())
	}
}

func TestCovSliceNilPointerElement(t *testing.T) {
	got := CovSlice[covGetter]([]*covPointer{nil, {3}})
	if got[0] == nil {
		t.Fatal("nil pointer element became a nil interface")
	}
	if got[0].Get() != -1 || got[1].Get() != 3 {
		t.Errorf("elements %d and %d, want -1 and 3", got[0].Get(), got[1].Get())
	}
}

func TestCovSliceInterfaceElements(t *testing.T) {
	got := CovSlice[covGetter]([]covWide{nil, covBoth{7}})
	if got[0] != nil {
		t.Errorf("nil interface element became %v, want nil", got[0])
	}
	if got[1].Get() != 7 {
		t.Errorf("element is %d, want 7", got[1].Get())
	}
}

func TestCovSliceRejectsNonImplementer(t *testing.T) {
	type closer interface{ Close() }
	defer func() {
		if recover() == nil {
			t.Error("converting an element that lacks the interface did not panic")
		}
	}()
	CovSlice[closer]([]covValue{{1}})
}
