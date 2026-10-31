// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(goexperiment.logosgcmoving && (amd64 || arm64))

package runtime

import "unsafe"

// Stubs for the zgc cycle outside the moving-collector experiment. Every
// hook in shared files resolves in all builds; the cycle runs only with
// GOEXPERIMENT=logosgcmoving on amd64 or arm64 under -gc=zgc.

type zgcSlotVisitor func(slot *uintptr)

// zgcPage is opaque outside the experiment: off-build stubs only carry
// it, never inspect it. The real header lives in logos_zgc_page.go.
type zgcPage struct {
	_ [0]func()
}

func zgcGCStartHook() {}

func zgcMarkTerminationHook() {}

func zgcInit() {}

func zgcAsmBoundaryComplete() bool { return false }

func zgcFlipMark() {}

func zgcMarkStartRoots() {}

func zgcScanSelf(gp *g, gcw *gcWork) {}

func zgcRemapPassComplete() {}

func zgcFinishRelocation() {}

func zgcWakeWorkers() {}

func zgcNextUnfinishedPage() *zgcPage { return nil }

func zgcRelocationWorker() {}

func zgcBarrierMark(target uintptr) {}

func zgcRelocateLargeLocked(pg *zgcPage, s *mspan) (uintptr, bool) { return 0, false }

func zgcReservePageToSpace(pg *zgcPage) bool { return false }

func zgcAbandonPage(pg *zgcPage) {}

func zgcCanRelocateDeep() bool { return false }

func zgcSelectPages(budget uintptr) int { return 0 }

func zgcEvacuatePage(pg *zgcPage) bool { return false }

func zgcEvacuateObject(pg *zgcPage, s *mspan, i uintptr) uintptr { return 0 }

func zgcCopyOne(pg *zgcPage, s *mspan, src, size uintptr) uintptr { return 0 }

func zgcPublishToSpace(pg *zgcPage) {}

func zgcReserveToSpace(sc spanClass, need uintptr) *mspan { return nil }

func zgcRetirePendingPages() {}

func zgcRefreshDeferredSpans() {}

func zgcPublishToSpaceSpans(pg *zgcPage) {}

func zgcRetirePage(pg *zgcPage) bool { return false }

func zgcQuarantineSpan(s *mspan, pg *zgcPage) {}

func zgcQuarantineBypass(s *mspan) bool { return false }

func zgcPublishReadyToSpace() {}

func zgcUnquarantineSpan(s *mspan) {}

func zgcFixSlot(slot *uintptr) {}

func zgcVerifySlot(slot *uintptr) {}

func zgcWalkAll(v zgcSlotVisitor) {}

func zgcFixRoots(v zgcSlotVisitor) {}

func zgcWalkHeapLive(v zgcSlotVisitor) {}

func zgcWalkAllLiveForCheck() {}

func logosZgcUntagArg(p unsafe.Pointer) unsafe.Pointer { return p }

// logosZgcCanonicalStoreValue is the off experiment twin of the boundary
// bridge: no slot ever carries color bits here, so the value is stored
// unchanged. It still returns its pointer argument directly, preserving
// the escape edge the uintptr round trip of logosZgcUntagArg hides.
func logosZgcCanonicalStoreValue(p unsafe.Pointer) unsafe.Pointer { return p }

func zgcBoundaryWrappersLanded() bool { return false }
