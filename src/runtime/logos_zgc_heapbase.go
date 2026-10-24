// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/goexperiment"

// logosFixedHeapBase turns off heap base randomization under
// GOEXPERIMENT=logosgcmoving. A zgc pointer word keeps its canonical
// address in bits 41..0 (SPEC 19.2, M19-S7), so the heap must stay below
// 4 TB, and the software color bits 42..46 must not collide with address
// bits. A randomized base puts arenas anywhere below 2^47, which the
// fixed arena hints starting at 0x00c0<<32 avoid.
const logosFixedHeapBase = goexperiment.LogosGCMoving
