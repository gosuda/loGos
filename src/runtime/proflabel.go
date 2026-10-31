// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/atomic"
	"unsafe"
)

var labelSync uintptr

// runtime_setProfLabel should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/localsession
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname runtime_setProfLabel runtime/pprof.runtime_setProfLabel
func runtime_setProfLabel(labels unsafe.Pointer) {
	// Introduce race edge for read-back via profile.
	// This would more properly use &getg().labels as the sync address,
	// but we do the read in a signal handler and can't call the race runtime then.
	//
	// This uses racereleasemerge rather than just racerelease so
	// the acquire in profBuf.read synchronizes with *all* prior
	// setProfLabel operations, not just the most recent one. This
	// is important because profBuf.read will observe different
	// labels set by different setProfLabel operations on
	// different goroutines, so it needs to synchronize with all
	// of them (this wouldn't be an issue if we could synchronize
	// on &getg().labels since we would synchronize with each
	// most-recent labels write separately.)
	//
	// racereleasemerge is like a full read-modify-write on
	// labelSync, rather than just a store-release, so it carries
	// a dependency on the previous racereleasemerge, which
	// ultimately carries forward to the acquire in profBuf.read.
	if raceenabled {
		racereleasemerge(unsafe.Pointer(&labelSync))
	}
	if goexperiment.LogosGCMoving {
		// Release between the label map initialization (done by the
		// caller before this function) and the publish below, so the
		// pin pass and the fenced inherit never observe a half built
		// map. Preemptible context only, no lock, no allocation. The
		// signal handler keeps its plain load.
		publicationBarrier()
	}
	getg().labels = labels
}

// logosAcquireProfLabel loads gp.labels for preemptible or STW contexts
// (inherit, pin pass). Under the moving experiment it is an acquire load
// pairing with the release in runtime_setProfLabel. The signal handler
// path keeps its plain load and is untouched.
func logosAcquireProfLabel(gp *g) unsafe.Pointer {
	if goexperiment.LogosGCMoving {
		return atomic.Loadp(unsafe.Pointer(&gp.labels))
	}
	return gp.labels
}

// runtime_getProfLabel should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/cloudwego/localsession
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname runtime_getProfLabel runtime/pprof.runtime_getProfLabel
func runtime_getProfLabel() unsafe.Pointer {
	return getg().labels
}
