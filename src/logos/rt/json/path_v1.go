// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

// indexedFieldPaths is false when encoding/json reports the Field of an
// *UnmarshalTypeError inside an array without the element index, as it does
// without goexperiment.jsonv2: "A" for any element of A.
const indexedFieldPaths = false
