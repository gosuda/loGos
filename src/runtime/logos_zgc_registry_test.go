// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import unsafePkg "unsafe"

// testZgcPageHeader returns a zeroed page header for registry probes. Page
// metadata is NotInHeap, so it cannot be allocated on the Go heap.
func testZgcPageHeader() *zgcPage {
	p := sysAlloc(unsafePkg.Sizeof(zgcPage{}), &memstats.other_sys, "test zgc page header")
	if p == nil {
		panic("test: out of memory")
	}
	return (*zgcPage)(p)
}

// LogosZgcRegistryProbe checks that a page keeps its state index for as
// long as it owns a state, and returns "" on success or a description of
// the first failure. Package runtime cannot import testing, so the check
// lives here and an external test reports it.
func LogosZgcRegistryProbe() string {
	zgcInitPageRegistry()

	var pages []*zgcPage
	release := func() {
		for _, pg := range pages {
			zgcPageForget(pg)
		}
		for _, pg := range pages {
			sysFree(unsafePkg.Pointer(pg), unsafePkg.Sizeof(zgcPage{}), &memstats.other_sys)
		}
	}
	defer release()

	a, b := testZgcPageHeader(), testZgcPageHeader()
	pages = append(pages, a, b)
	sa, sb := zgcPageState(a), zgcPageState(b)
	if sa == nil || sb == nil || sa == sb {
		return "page states are not distinct"
	}
	ia, ib := a.stateIdx, b.stateIdx
	if ia == 0 || ib == 0 || ia == ib {
		return "state indexes are not distinct"
	}
	sa.spans = append(sa.spans, nil)
	if len(zgcPageSourceSpans(a)) != 1 {
		return "recorded source span is not visible"
	}

	zgcPageForget(a)
	if a.stateIdx != 0 {
		return "forgotten page keeps a state index"
	}
	if b.stateIdx != ib {
		return "forgetting a page moved a live page's state index"
	}
	if zgcPageState(b) != sb {
		return "lookup after forget returned a foreign state"
	}
	if len(zgcPageSourceSpans(a)) != 0 {
		return "forgotten page still reports source spans"
	}
	if len(zgcPageSourceSpans(b)) != 0 {
		return "live page reports another page's source spans"
	}

	// The state a released is pooled, so a new page may legitimately take
	// it back. What must not happen is b's live state being handed out.
	c := testZgcPageHeader()
	pages = append(pages, c)
	sc := zgcPageState(c)
	if sc == nil || sc == sb {
		return "hole reuse returned the live page's state"
	}
	if c.stateIdx != ia {
		return "forgotten index was not reused"
	}
	return ""
}
