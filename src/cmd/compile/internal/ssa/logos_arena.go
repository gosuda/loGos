// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import "internal/goarch"

// logosArenaGOffset is the byte offset of g.logosArena. The compiler has no
// g type when building user code and reaches g only through OpGetG, so the
// offset cannot be derived here; TestLogosArenaFieldOffset in src/runtime
// asserts unsafe.Offsetof(g{}.logosArena) equals this value.
const logosArenaGOffset = 4 * goarch.PtrSize
const LogosArenaGOffset = logosArenaGOffset
