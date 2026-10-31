# M19 GC modes

Normative sources: `logos-docs/SPEC.md` Sec 19 and Sec 3.1 plus Sec 8.4,
`logos-docs/IMPLEMENTATION.md` M19-S1 to S8. This page closes the M19 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

Selection: `logos build -gc=MODE`, `go build -gc=MODE` (a flag without help
text under `go`), or `logos.toml [build] gc` under `logos`; cmd/go passes
`-X runtime.logosGCMode=MODE` to the linker. Unknown mode reports
`logos build: unknown -gc mode "x"
(want greentea, zgc, immix, wasm, batch, epsilon)` with the `go build:`
prefix under plain `go`. Default under `logos` is `greentea`, or `wasm` when
GOARCH is wasm. An empty production controller mode uses elastic pacing;
empty test controller modes keep upstream pacing.

- S1 batch and epsilon: `gcPercent = -1` at `gcinit`; batch triggers at 90
  percent of `GOMEMLIMIT` (or physical memory when unset) then runs a stop
  the world mark; cmd/go adds `-wb=false` to every compile except `runtime`
  and its internal dependencies for these two modes. Pointer stores emit
  plain `MOVQ` with no barrier instructions. Pointer keyed maps keep standard
  `memhash` (batch and epsilon never move objects, so address hashing stays
  valid).
- S2 greentea with elastic workers: `revise()` computes `u = heapLive /
  heapGoal` and sets fractional utilization so total background utilization
  equals `elastic(u)`; worker assignment picks it up. Assist debt accounting
  is unchanged.
- S3 wasm idle marking: on GOARCH=wasm the collector stays single threaded
  on plain atomics; pointer stores keep the write barrier while marking runs
  incrementally. The heap splits into a low permanent region and a high
  scratch region (`runtime/logos_wasmheap.go`); `memory.grow` extends the
  high region only. The js and wasm scheduler idle path drains mark work for
  up to 2 ms before returning to the event loop when a mark phase is active.
- S4 generational card table (`GOEXPERIMENT=logosgen`): 1 byte per 512 bytes
  of heap arena set by the write barrier slow path for pointer stores into
  mature spans (maturity is survival through at least 2 cycles); minor cycle
  at 50 percent heap growth; mature clean spans skip scanning without reading
  `gcmarkBits` (sticky bit skip); full cycle every 4 minor cycles.
- S5 moving modes (`GOEXPERIMENT=logosgcmoving`): phase 1 immix evacuates
  fragmented spans during mark termination with precise type info, off heap
  forwarding table, and a pin set (conservatively scanned frames,
  `Pinner` pinned objects, cgo passed objects, pointer keyed map keys, plus
  `logosNoReloc` spans with attached finalizers or cleanups, cleared only in
  `span.init`). Cleanup fixup rewrites attached and queued cleanup functions
  with the existing pointer masks. Phase 1 substrate landed; the mode gate at
  `cmd/go/internal/work/logos_gc.go` stays closed pending full std immix
  (red evidence recorded, error names the missing phase). Phase 2 zgc adds
  concurrent relocation with load barrier pass, self healing slow path,
  software unmasking on CPUs without top byte ignore, relocation targets over
  35 percent fragmented, off heap forwarding in page metadata, and
  `madvise(MADV_DONTNEED)` of evacuated source pages before self heal drains.
- S6 green tea span deck: per span dirty bit set at mark time without
  dereference; per P span queues drained in monotonic address order; inline
  `PREFETCHT0` in the sequential span scan on amd64. Selection is the compile
  time constant `goexperiment.LogosSpanDeck` baselined true; every GOARCH=wasm
  target keeps the deck off and `nologosspandeck` retains the FIFO path.
- S7 zgc colored pointers: 42 bit canonical address in bits 41 to 0 (4 TB
  range), MMU ignored region in bits 63 to 48, color in bits 59 to 56 with
  bits 63 to 60 and 55 to 48 zero, bit 47 reserved zero, bit 46 final, bit 45
  remapped, bit 44 mark1, bit 43 reserved zero, bit 42 mark0. Bad color loads
  route into the self heal CAS path (`TESTQ` plus `JNZ`) and unmask with
  `ANDQ`. Every syscall and cgo entry untags pointers.
- S8 immix blocks and lines: 32 KiB blocks split into 256 128 byte lines with
  the 256 byte line mark header stored off heap in block metadata. Occupancy
  is the `freeindex` OR `allocBits` composite set on every served slot and
  rebuilt at sweep; dirty is the parity pair barrier with STW epoch flip and
  bounded clear; hole hunting reads occupancy; no allocation derives from
  line marks.

## How to enable or observe it

- Select per build (`-gc=batch`) or per module (`[build] gc`). Plain `go`
  accepts the same flag values.
- Observe with `src/runtime/logos_gcmode_test.go` (epsilon `NumGC` 0 after
  100 MB with `GOMEMLIMIT` unset; batch idle until 90 percent then STW mark
  with `NumGC` increase; `gccheckmark` silent without barriers),
  `src/runtime/logos_pacer_test.go` (utilization goals at u 0.79, 0.80,
  0.875, 0.95, 0.96 equal 0.25, 0.25, 0.50, 0.75, 0.75 with assists running
  at or above 0.95; `BenchmarkLogosAssistShare` reported),
  the specified wasm GC shape (`i64` loads and stores with barrier call,
  split heap growth bound; the plan paths `test/logos/run/wasm_gc_idle.lgo`
  and `test/logos/codegen/logos_wasm.lgo` do not exist in this checkout;
  observe via `test/logos/run/wasm_loop_preempt.lgo` and
  `test/logos/codegen/wasm_noyield.lgo` plus the runtime unit gates),
  `src/runtime/logos_gen_test.go` (card marking, 49, 50, 51 percent trigger,
  maturity at 1, 2, 3 cycles, full cycle every 4),
  `src/runtime/logos_move_test.go` plus finalizer and cleanup attacks
  (no move of pinned and excluded spans, handles stay valid, resurrection
  and reregistration preserved), `src/runtime/logos_spandeck_test.go`
  (bitmap set without dereference, address order pops, prefetch sites, L1
  report), `src/runtime/logos_zgc_test.go` (layout, hardware round trip on
  TBI and LAM or UAI, software self heal, untag at entries, 34, 35, 36
  percent evacuation boundary), and `src/runtime/logos_immix_test.go`.
- Parent to run: the runtime unit gates above (generational and moving legs
  with their experiments on), `logos test -short std` and
  `go test -short std` under `-gc=batch` (all packages except `runtime`
  compiled without barriers), `logos test -short std` with the default mode,
  wasm legs with `node` present (record as blocked without it), and the mode
  gate check that `-gc=immix` and `-gc=zgc` stay unavailable until their
  phases complete.

## Budgets and defaults

- Batch trigger is 90 percent of the memory limit; epsilon never collects.
  Minor cycle trigger is 50 percent heap growth; full cycle every 4 minor
  cycles; maturity is 2 surviving cycles; evacuation picks pages over 35
  percent fragmented and skips pages at or below it.
- Wasm idle drain is bounded at 2 ms per idle entry; mark slices on wasm run
  on one thread.
- Benchmarks for pause (`BenchmarkLogosPause` p99) and assist share are
  recorded, not pass conditions.

## Known limits

- The `-wb=false` mode is sound only because marking never runs concurrently
  with mutators; the flag is still marked TODO upstream.
- Stable default migration for the generational experiment is blocked
  (compiler crashes during `make.bash` even with full cycles); no default
  flip is claimed.
- New finalizer or cleanup attachment must synchronize with concurrent
  relocation before the object can move; `-gc=zgc` stays unavailable until
  that handshake plus cleanup root fixup pass concurrent attach and relocate
  tests (recorded as blocked, not complete).
- Upstream finalizers may resurrect or reregister and need not run before
  exit; preserving that is required and the exact GC count is not a
  scheduling guarantee.
