// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rt

// PR3 feedback regressions for ViewAs POD enforcement.
// Reviewer prose is untrusted; expectations derive from the published
// comptime check and the runtime cast shape.
//
// RED: ViewAs of a struct carrying a slice header must panic with a
// distinct logos: ViewAs: message. Controls pin legitimate POD views.

import (
	"strings"
	"testing"
)

type viewasBadSlice struct {
	Bytes []byte
}

func TestPR3ViewAsRejectsSliceField(t *testing.T) {
	b := make([]byte, 32)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("ViewAs[slice-field] did not panic")
		}
		msg, _ := r.(string)
		if !strings.HasPrefix(msg, "logos: ViewAs:") {
			t.Fatalf("panic = %q, want logos: ViewAs: prefix", msg)
		}
	}()
	_ = ViewAs[viewasBadSlice](b)
}

func TestPR3ViewAsLegitHeaderControl(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	h := ViewAs[viewasHeader](b)
	if h.Magic != 67305985 {
		t.Fatalf("legit header Magic = %d", h.Magic)
	}
}
