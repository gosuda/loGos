// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.jsonv2

package json

// indexedFieldPaths is true when encoding/json reports the Field of an
// *UnmarshalTypeError inside an array with the element index in the path,
// as it does with goexperiment.jsonv2: "A.0" for the first element of A.
const indexedFieldPaths = true
