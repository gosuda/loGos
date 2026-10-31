// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.logosgcmoving && (amd64 || arm64)

package runtime

import (
	"internal/runtime/atomic"
)

// Background relocation workers.
//
// A dedicated goroutine set copies pages concurrently with mutators after
// the mark-termination pause registers them. The load barrier never
// relocates: it heals already-published copies and otherwise returns the
// source address, so workers own every copy. Workers share the page mutex
// and the zgcRelocateObject CAS protocol with the stop-the-world finish
// hook, which helps any remainder to completion before the rewrite, so
// workers are best-effort and need no shutdown protocol.

// zgcWorkerCount bounds the background relocation pool.
const zgcWorkerCount = 4

const (
	zgcWorkerRunning = 0
	zgcWorkerParking = 1
	zgcWorkerParked  = 2
	zgcWorkerWaking  = 3
)

type zgcWorker struct {
	state atomic.Uint32
	note  note
}

var zgcWorkers [zgcWorkerCount]zgcWorker

// zgcWorkGen counts relocation wakes: zgcWakeWorkers bumps it once per
// registration while the world is stopped, before waking parked workers.
// A worker records the generation of its last completed scan and re-scans
// while its observed generation lags, so racing wakes are guaranteed to be
// observed even if a wake arrives while the worker is actively scanning.
// One bump per stop-the-world pause, so wrap-around is not a concern.
var zgcWorkGen atomic.Uint64

var zgcWorkersStarted atomic.Uint32

func init() {
	if logosZgcMode() {
		zgcInitWorkers()
	}
}

// zgcInitWorkers spawns the dedicated relocation pool.
// Worker spawning is deferred until after procresize (via package init)
// so runtime bootstrap (schedinit/gcinit) remains goroutine- and allocation-free.
func zgcInitWorkers() {
	if !zgcWorkersStarted.CompareAndSwap(0, 1) {
		return
	}
	for i := range zgcWorkers {
		w := &zgcWorkers[i]
		noteclear(&w.note)
		go zgcRelocationWorker(w)
	}
}

// zgcWakeWorkers wakes the pool for the newly registered pages. The
// generation bump is the delivery guarantee every worker observes;
// parked workers are readied via notewakeup.
// No channel or mutex operations occur while the world is stopped: each worker
// uses a lock-free 4-state machine (Running, Parking, Parked, Waking) and runtime notes.
// notewakeup uses atomic xchg and futexwakeup without acquiring any scheduler
// or runtime mutex, making it completely pause-safe during STW.
func zgcWakeWorkers() {
	if !zgcCycleEnabled() || zgcWorkersStarted.Load() == 0 {
		return
	}
	zgcWorkGen.Add(1)
	for i := range zgcWorkers {
		w := &zgcWorkers[i]
		for {
			st := w.state.Load()
			if st == zgcWorkerRunning || st == zgcWorkerWaking {
				break
			}
			if st == zgcWorkerParking {
				if w.state.CompareAndSwap(zgcWorkerParking, zgcWorkerRunning) {
					break
				}
				continue
			}
			if st == zgcWorkerParked {
				if w.state.CompareAndSwap(zgcWorkerParked, zgcWorkerWaking) {
					notewakeup(&w.note)
					w.state.Store(zgcWorkerRunning)
					break
				}
				continue
			}
			break
		}
	}
}

// zgcRelocationWorker copies registered pages until every live byte has
// a published copy, then parks until the next registration. A refused
// page (pinned or foreign-exposed, zgcEvacuatePage reports false with
// published still below live) parks instead of spinning: reselecting it
// immediately would burn a P for the whole cycle, so the remainder waits
// for barrier relocation, unexposure, or the stop-the-world finish hook.
// Parking discipline: the worker uses a 4-state machine (Running, Parking, Parked, Waking)
// and runtime notes only while fully caught up (seen == zgcWorkGen).
// If a wake races while Parking, the wake CASes Parking -> Running and avoids
// notewakeup. If the worker is already Parked, the wake CASes Parked -> Waking,
// calls notewakeup, and stores Running. The generation counter ensures no work is lost.
func zgcRelocationWorker(w *zgcWorker) {
	seen := zgcWorkGen.Load() - 1
	for {
		for seen != zgcWorkGen.Load() {
			seen = zgcWorkGen.Load()
			if !zgcCycleEnabled() {
				break
			}
			for {
				pg := zgcNextUnfinishedPage()
				if pg == nil {
					break
				}
				// One release per selection, on every exit refusals
				// included: the lease is the worker's own, gained under
				// the registry lock, and it covers the whole attempt —
				// claims, refills, NoMemory and the final publication
				// state. zgcEvacuatePage itself neither gains nor drops
				// the ref: the finish hook and test seams call it under
				// the world stop without a lease.
				if pg.published.Load() != pg.live && !zgcEvacuatePage(pg) {
					zgcPageRelease(pg)
					break
				}
				zgcPageRelease(pg)
			}
		}

		if !w.state.CompareAndSwap(zgcWorkerRunning, zgcWorkerParking) {
			continue
		}
		if seen != zgcWorkGen.Load() {
			w.state.Store(zgcWorkerRunning)
			continue
		}
		if !w.state.CompareAndSwap(zgcWorkerParking, zgcWorkerParked) {
			continue
		}
		notetsleepg(&w.note, -1)
		noteclear(&w.note)
		w.state.Store(zgcWorkerRunning)
	}
}

// zgcNextUnfinishedPage returns a registered page with unpublished live
// bytes, or nil. The returned page carries a lease: the ref is gained
// under the same lock that protects the list, before the header can
// escape, so a worker suspended between attempts still keeps the header
// and its state alive across any pause. The caller owes exactly one
// zgcPageRelease per non-nil result, on every exit.
//
// Discipline: the holder cannot be async-preempted
// mid-section (holding a runtime mutex sets m.locks, which the
// preemption predicate requires to be zero), and the section parks
// nothing: atomic loads and a range loop only, no channel ops, no nested
// locks, no syscalls, no allocation.
func zgcNextUnfinishedPage() *zgcPage {
	zgcPagesLock()
	defer zgcPagesUnlock()
	for _, pg := range zgcAllPages {
		if pg.published.Load() != pg.live {
			pg.refs.Add(1)
			return pg
		}
	}
	return nil
}
