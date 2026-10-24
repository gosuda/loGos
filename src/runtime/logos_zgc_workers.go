// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

// Background relocation workers.
//
// A dedicated goroutine set copies pages concurrently with mutators after
// the mark-termination pause registers them. Workers and the mutator deep
// path share the page mutex and the zgcRelocateObject CAS protocol: two
// threads racing one unpublished object keep one copy. Under
// GOMAXPROCS=1 a mutator deep-path relocation makes progress without
// yielding, so no spin livelock exists. When the next GC starts, the
// stop-the-world hook helps any remainder to completion before the
// rewrite, so workers are best-effort and need no shutdown protocol.

// zgcWorkerCount bounds the background relocation pool.
var zgcWorkerCount = 4

// zgcWorkCh wakes relocation workers after registration.
var zgcWorkCh chan struct{}

// zgcInitWorkers creates the worker channel and parks the dedicated pool
// while the world runs: spawning goroutines inside a pause is avoided.
func zgcInitWorkers() {
	zgcWorkCh = make(chan struct{}, 16)
	for range zgcWorkerCount {
		go zgcRelocationWorker(zgcWorkCh)
	}
}

// zgcWakeWorkers wakes the pool for the newly registered pages. Workers
// are best-effort: the next gcStart hook helps any remainder to
// completion before the rewrite, so a full channel simply sheds load.
func zgcWakeWorkers() {
	if !zgcCycleEnabled() || zgcWorkCh == nil {
		return
	}
	for range zgcWorkerCount {
		select {
		case zgcWorkCh <- struct{}{}:
		default:
		}
	}
}

// zgcRelocationWorker copies registered pages until every live byte has
// a published copy, then parks until the next registration.
func zgcRelocationWorker(ch chan struct{}) {
	for range ch {
		if !zgcCycleEnabled() {
			continue
		}
		for {
			pg := zgcNextUnfinishedPage()
			if pg == nil {
				break
			}
			if pg.published.Load() != pg.live {
				zgcEvacuatePage(pg)
			}
		}
	}
}

// zgcNextUnfinishedPage returns a registered page with unpublished live
// bytes, or nil.
func zgcNextUnfinishedPage() *zgcPage {
	zgcPagesLock()
	defer zgcPagesUnlock()
	for _, pg := range zgcAllPages {
		if pg.published.Load() != pg.live {
			return pg
		}
	}
	return nil
}
