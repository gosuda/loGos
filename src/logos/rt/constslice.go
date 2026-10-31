// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rt

// This file holds the runtime half of M13-S3 #121 slice covariance
// (SPEC T-203.4). In a loGo/s call, a []C argument may pass to a
// `const []I` parameter when C implements the interface I. Lowering
// (cmd/compile/internal/logos/constslice.go) wraps that argument in a
// call to CovSlice below, so the callee's plain []I parameter receives
// a real slice. Specialized clones at -O=2 and -O=3 drop the call and
// take the []C directly; every other call, at every level, keeps it.

// CovSlice converts a []E to a []I element by element. The checker
// proves that E implements I for every conversion it lowers and for
// every direct call in loGo/s source; the clone specializer proves it
// again before it removes a call.
//
// A nil source returns nil, and a non-nil source of length zero returns
// a non-nil empty slice, so the result says which of the two the caller
// passed. Any other source returns a fresh slice of the same length that
// shares no backing array with s: writes to the result never reach s,
// and later writes to s never reach the result. Pointer elements still
// point at the same objects.
//
// An element converts through its dynamic value. A nil interface element
// stays a nil I rather than failing the assertion, and a nil pointer
// element stays a nil pointer inside a non-nil I, exactly as the
// language converts one value to an interface. An element whose dynamic
// type does not implement I panics with the ordinary type assertion
// message.
func CovSlice[I any, E any](s []E) []I {
	if s == nil {
		return nil
	}
	r := make([]I, len(s))
	for i, e := range s {
		var a any = e
		if a == nil {
			continue
		}
		r[i] = a.(I)
	}
	return r
}
