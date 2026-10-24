// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package atomic

import "unsafe"

// Canonical ZGC pointer atomics (W1a).
//
// Under GOEXPERIMENT=logosgcmoving a heap slot can carry color bits that
// the compiler's load barrier (-lb) strips on every barriered load, but
// which a raw compare-and-swap or store would treat as part of the
// pointer. The NoWB helpers below pull their bodies from the runtime,
// which canonicalizes both operands, compares canonical identities,
// retries on a changed raw word, and writes only canonical replacement
// words. With colors inert the helpers are the identity, preserving the
// upstream compare and store semantics exactly.
//
// The user-facing Store and CompareAndSwap (write-barrier variants)
// canonicalize in the runtime before atomicwb and keep calling the
// runtime forms, so race interception and write-barrier behavior are
// unchanged. These NoWB forms are for write-barrier-free runtime
// callers that hold slots the barriered loads have already healed or
// that only canonical words reach.

// provided by runtime
//
//go:linkname casPointerNoWB
func casPointerNoWB(ptr *unsafe.Pointer, old, new unsafe.Pointer) bool

// provided by runtime
//
//go:linkname storePointerNoWB
func storePointerNoWB(ptr *unsafe.Pointer, new unsafe.Pointer)
