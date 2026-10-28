// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.jsonv2

package json

// invalidUTF8Escape is the text an invalid UTF-8 byte becomes inside an
// escaped string. Without goexperiment.jsonv2 encoding/json writes the six
// character escape sequence \ufffd.
const invalidUTF8Escape = `\ufffd`
