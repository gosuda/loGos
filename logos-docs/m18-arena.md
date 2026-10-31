# M18 tier 0 arena and bump allocation

Normative sources: `logos-docs/SPEC.md` Sec 3.1 (RFC-301) and Sec 8.2 to 8.3,
`logos-docs/IMPLEMENTATION.md` M18-S1 to S4. This page closes the M18 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

Experiment `logosarena` (field `LogosArena`, added with the M17-S2
procedure). `logos build` passes it through `GOEXPERIMENT` when
`logos.toml [build] arena = true` (key added here, default `false`).
`GOEXPERIMENT` is fixed per build, so `logos` rebuilds the standard library
with it. Plain `go` selects it with `GOEXPERIMENT=logosarena` and reads no
`logos.toml`; placement applies to Go and loGo/s packages alike, and the
plain `go` default stays off until `run.bash` passes with it on (G3).

Invariants AR1 to AR4: arena objects are reachable only from the allocating
frame and frames it calls (escape proof; no store to heap, global, channel,
or another stack); watermarks are LIFO with popped scan memory zeroed after
the watermark rises and before reuse; the GC scans each chunk
`[scanCur, scanBase)` as part of the goroutine stack scan with type words;
on park every chunk whose noscan and scan regions are both empty is released
(including empties behind a live chunk) while live chunks and addresses are
retained.

- S1 chunks, lease, release: `runtime/arena_logos.go`, `sideStackChunk`,
  per P free list `p.logosArenaPool`, `g.logosArena`, chunks from
  `mheap_.allocManual` with a distinct arena span type. `gopark` releases
  every fully empty chunk even behind a live chunk. Finalizer on an arena
  object panics (`runtime: SetFinalizer on loGo/s arena object`); cgo
  pointer checks do not panic on arena addresses. `noscanCur <= scanCur`
  holds; on breach link an overflow chunk or fall back to `mallocgc`.
- S2 compiler placement: escape analysis marks nonescaping, non stack
  allocatable allocations as arena eligible; walk lowers constant size
  allocations to `runtime.logosArenaAlloc`, dynamic size to
  `runtime.logosArenaMakeSlice` (len and cap as int64 with truncation,
  overflow, `maxAlloc`, and len before cap panics inside), and pointerful
  ones to `runtime.logosArenaAllocScan`. `ssagen` saves the watermark at
  function entry and restores at every exit and at loop back edges for
  allocations at that loop depth. `EscReturn` results allocate in the caller
  region.
- S3 inline noscan bump and GC scanning: new SSA op for the add compare fast
  path with slow path call, marked as an async preemption unsafe point
  (`PCDATA_UnsafePoint`); `scanstack` scans goroutine scan regions by type
  word; race builds call `racemalloc` and `racefree` on watermark moves. The
  SSA bump op is gated on noscan and size up to 512.
- S4: cancellation stays in `logos/rt.Group` (M8); no `cancelChan` field is
  added to `g`. The spec `cancelChan` duplicates that mechanism with the same
  observable behavior.

## How to enable or observe it

- Set `[build] arena = true` under `logos`, or
  `GOEXPERIMENT=logosarena` under either command.
- Observe with `src/runtime/logos_arena_test.go` (100,000 parked goroutines
  return chunks, mixed live and empty parking releases only empties with
  addresses preserved, exact fit and 1 byte overflow and empty pool fallback,
  finalizer panic, cgo checks),
  `test/logos/codegen/arena_place.lgo` (nonescaping dynamic `make` calls the
  arena entry, escaping keeps `makeslice`, `sync.Pool` stores stay on heap),
  `test/logos/run/arena_loop_carry.lgo` and `arena_loop_edges.lgo` (the plan
  `arena_loop.lgo` 1M iteration shape does not exist in this checkout;
  iteration local scratch and recursion return behavior via the existing
  carry and edges fixtures),
  `test/logos/codegen/arena_bump.lgo` (inline fast path shape plus PCDATA
  marker), `src/runtime/logos_arena_test.go` GC leg (referents survive `GC()`,
  no stale pointers after frame exit; parent requires the `-race` leg to
  report no data races), and
  `src/logos/rt/group_test.go` (shared group cancel with no `g` field read).
- Parent to run: `GOEXPERIMENT=logosarena go test -short -race runtime -run
  LogosArena`, the codegen and testdir checks with the experiment on, and
  `GOEXPERIMENT=logosarena go test -short std`. The bump benchmark reports
  the 1.5 ns target (reported, never a pass condition).

## Budgets and defaults

- Chunk size is 64 KiB (`logosArenaChunkBytes`); the largest object an arena
  chunk can hold is 64 KiB minus 1024 bytes (`LogosArenaMaxBytes`). The
  inline bump fast path covers rounded payloads at or below 512 bytes
  (`LogosArenaBumpMaxBytes`; 511 and 512 inline) while larger or non-constant
  sizes take the slow path (513 slow). The bump limit is distinct from the
  chunk fit limit.
- Default is off under both commands until the G3 flip.

## Known limits

- Larger than 512 byte noscan allocations call the arena slow path.
- `sync.Pool` stores never take the arena path.
