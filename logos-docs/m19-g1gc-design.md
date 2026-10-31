# G1 collector for plain Go and loGo/s

Anchor: David Parnas, Dieter Rams, Tony Hoare

Status: design only. No `g1` mode exists in the tree today. Every citation below
was read in this worktree. Anything not read is marked INFERENCE.

## 1. Decision

Base: regions plus ranked collection set plus the existing stop-the-world
evacuation. The pause-target policy is absorbed into it. A second metadata
system (own region table, per-region source bitmaps, SATB buffers) is rejected.

Why: the hard parts of G1 already exist here behind other names. Regions are
immix blocks (`src/runtime/logos_immix.go:19-29`). Cross-region remembered sets
are the logosgen card table (`src/runtime/logos_gen.go:73-75`). Intra-block
remembered sets are the immix dirty parity pair (`src/runtime/logos_immix.go:63-94`).
Evacuation with off-heap forwarding exists and is proven
(`src/runtime/logos_evac_immix.go:37-55`, `src/runtime/logos_evac_fix.go:25-69`).
Pause control exists as the elastic pacer (`src/runtime/logos_pacer.go`).
A faithful Java-style clone would rebuild all five and add a second source of
truth for "which region is dirty", which is the defect class this tree already
paid for once.

Rejected alternatives:

- Faithful clone with its own region table and SATB barriers. Duplicates five
  landed mechanisms, and its remembered sets would diverge from the card table.
- Policy layer alone. Without regions and evacuation it is a scheduler, not a
  collector, and cannot reclaim anything region-wise.

## 2. What G1 is here

Region: one 32 KiB immix block, 128 B lines, 256 lines per block
(`src/runtime/logos_immix.go:19-29`). Per-block metadata already carries every
field a region needs (`src/runtime/logos_immix.go:63-72`): base, the recomputed
line view, the occupancy plane, a two-generation dirty parity pair, the epoch
used for enqueue deduplication, intrusive next pointers, and a sticky pin byte.
The block table mirrors the heap arena map (`src/runtime/logos_immix.go:93-112`).

Roles: young, old, humongous, free, pinned. Phase 1 stores no role tag. It
derives young versus old from span age at selection time, using the thresholds
that already exist: mature after 2 cycles, full cycle every 4 minors
(`src/runtime/logos_gen.go:36-40`). INFERENCE: derivation cost per pause is
acceptable at 32 KiB granularity; if it is not, one byte per block is the
fallback.

Collection-set selection: rank evacuable regions by free-line ratio, descending,
then take the longest prefix that fits the pause budget in bytes and spans.
Today the code takes a binary cutoff instead: dense or pinned regions are
skipped and the rest are collected (`src/runtime/logos_evac_immix.go:190-228`).
The liveness input is already correct: the merge rule is occupied equals mark
coverage UNION the active dirty plane, so a dirty line can only over-pin, never
under-pin (`src/runtime/logos_evac_immix.go:209-223`).

Remembered sets, cross-region: one byte per 512 B of heap, allocated per arena
(`src/runtime/logos_gen.go:36-40`, `:73-75`, `:129-153`). Writers mark a card on
slot store, on bulk copy, and on type-bit copy
(`src/runtime/logos_gen.go:233-243`, `:331-343`). Drains run from the write
buffer, the per-P buffer, and the no-P scratch buffer, then settle under a
stopped world (`src/runtime/logos_gen.go:246-319`, `:388-399`). Scans walk a
whole arena, a single card, or a span range (`src/runtime/logos_gen.go:484-529`).
The partial trailing card belongs to exactly one span and only its owner may
clear it (`src/runtime/logos_gen.go:616-648`). G1 inherits that rule unchanged
and must state who clears the cards of an evacuated region: the evacuating span,
once, during the pause.

Remembered sets, intra-block: the dirty parity pair plus an epoch and intrusive
enqueue list (`src/runtime/logos_immix.go:63-94`). The store barrier sets the
line bit atomically and enlists the block once per epoch
(`src/runtime/logos_immix.go:196-220`). The table flips at a stopped point and
is cleared in bounded steps. G1 reuses this as the young-to-young and
intra-region input; it adds nothing.

Evacuation: stop the world, require marking complete, pick candidates, assign
per-span off-heap forwarding tables, build a destination pool, evacuate each
candidate, rewrite every root through forwarding, verify under `gccheckmark`,
then free the forwarding tables (`src/runtime/logos_evac_immix.go:37-55`,
`:68-81`, `:247-264`, `:266-303`; `src/runtime/logos_evac_fix.go:25-69`,
`:284-333`). Pin granule is one block (`src/runtime/logos_evac_immix.go:52-53`),
and the pin pass refuses a running, idle, or copy-stack goroutine, which aborts
evacuation entirely (`src/runtime/logos_evac_pin.go:154-168`).

Barriers: stores mark the 512 B card and set the 128 B line bit; loads are
plain. Two new SPEC Section 8.4 lines state exactly that, alongside the existing
per-mode lines (`logos-docs/SPEC.md:1237-1248`).

Pointer-keyed maps: a block whose address is used as a hash is pinned and never
evacuates. That is the immix rule today
(`src/runtime/logos_move_moving.go:71`, `logos-docs/SPEC.md:1246-1248`).

Humongous: an object at or above half a region, and any span that crosses a
block boundary, never enters the collection set and is swept in place
(INFERENCE: follows the candidate triage in `src/runtime/logos_evac_immix.go:190-228`,
which excludes spans whose layout the block-granular machinery cannot move).

## 3. Mode wiring

A new `-gc=g1` must touch exactly these sites. All are read, not inferred.

1. Mode list: `src/cmd/go/internal/logos/config/config.go:83`, with order pinned
   by `config_gc_test.go:55`.
2. Pending-phase map: `src/cmd/go/internal/work/logos_gc.go:22`, today mapping
   immix to phase 1 and zgc to phase 2. Add `g1` to phase 3 and delete the entry
   only when the gate passes.
3. Flag and precedence: `logos_gc.go:27-36` registers `-gc`;
   `:57-71` resolves flag over `logos.toml` over default.
4. Selection side effects: `logos_gc.go:79-97` rejects the nongreentea tool tag,
   rejects an unknown mode with the exact message, enforces the phase gate,
   sets `cfg.LogosGC`, injects `-X=runtime.logosGCMode=g1` at `:94`, and decides
   the elastic pacer tag at `:95-97`.
5. Runtime dispatch: `src/runtime/logos_gcmode.go:12` is the injected variable;
   `:37-50` is the switch that must learn `g1` (concurrent marking on, no forced
   STW, no forced MaxInt64 limit, no new refusal); `:65-70` and `:77-89` are the
   STW and refusal predicates that must return false for g1.
6. String switches that must be taught g1 only where its semantics differ:
   `src/runtime/logos_gen.go:118-121`, `logos_move.go:12,20`,
   `logos_move_moving.go:21,71`, `logos_zgc_mode.go:13`.
7. Pacer tag: `src/cmd/go/internal/load/logos_pacer.go:19`, `:32-38`. G1 keeps
   elastic marking, so it must not take the nongreentea tag.
8. Documentation: the M19 row and the `-gc` mode list, plus a new subsection.
9. SPEC: Section 19 mode list and its exact error string
   (`logos-docs/SPEC.md:1798-1804`), the `gc` key in Section 10.3
   (`logos-docs/SPEC.md:1317-1318`), the phase-gate sentence shaped on the
   immix and zgc templates, and the two barrier lines in Section 8.4.

Acceptance gate: until phase 1 passes `go test -short std` with the experiment
on, `-gc=g1` must fail with a phase-3 message shaped like the immix and zgc
errors. A `-run` subset does not satisfy the gate.

Plain `go` is untouched: it keeps the upstream collector and pacing until
`./run.bash` passes with a changed default (`logos-docs/SPEC.md:1803`). The
defect to avoid is the hook you cannot revisit without a prod incident: every
G1 branch sits behind the mode predicate, so the no-flag path compiles out.

## 4. Cycle

States: `Idle`, `Mark`, `Select`, `Evacuate`, `Cleanup`, back to `Idle`.
Phase 2 inserts `ConcurrentMark` with SATB and `Remark` before `Select`.

Young versus mixed, decided at `Select`:

- Young-only when the trigger is heap growth, at least one minor is due, fewer
  than 4 minors have run since the last full cycle, and no forced, timed, leak,
  or checkmark trigger is present.
- Mixed otherwise. Forced, timed, and checkmark cycles are always full.

Pause steps:

1. Read the trigger from the controller, clamped by the existing 50 percent
   growth rule (`src/runtime/logos_gen.go:139-153`).
2. Choose young or mixed.
3. Rank candidate regions by free-line ratio and truncate to the pause budget.
4. Assign forwarding, build the destination pool, evacuate each span in the set,
   fix every root through forwarding, verify under checkmark.
5. Clear evacuated regions' cards once, rebuild survivor cards, clear the
   dirtied-block list that the barrier produced.
6. Account moved bytes, matured bytes, and minors since full, then feed the
   measured pause into the next commit.

Concurrent: marking and sweeping. Stopped: step 4 in full, plus the forwarding
lifetime and the pin pass. Region release: forwarding is pause-local and freed
at pause end; evacuated regions return to the free pool; candidates not chosen
keep their age and re-enter the next selection.

Still binding from the existing collector: the whole evacuate step requires the
world stopped and marking complete, and the pin pass vetoes evacuation when any
goroutine is running, idle, or copying its stack
(`src/runtime/logos_evac_immix.go:37-55`, `src/runtime/logos_evac_pin.go:154-168`).

## 5. Pause target

One knob, `logosPauseTargetNs`, default 0 meaning off. Three pure functions do
the work, and the pacer keeps authority over worker count:

- `logosPauseSelect(target, live, goal)` returns young or full.
- `logosPauseBudget(target, markRate)` returns the byte and span cap for the
  collection set.
- `logosPauseObserve(pause, reclaimed)` updates the cost estimate for the next
  commit.

They attach at the existing seams: the elastic utilization curve
(`logosElasticUtilization`, floor 0.25 below 0.80 utilization, ceiling 0.75 at
0.95) still sets background workers; `logosElasticStart` clamps the dedicated
count to the budget; `logosElasticRevise` may ask for an early young cycle when
live over goal predicts overrun and never raises workers mid-cycle;
`logosElasticEnd` keeps its utilization formula and only records pause against
target; `gcControllerState.commit` keeps the goal and trigger math and adds the
per-cycle scan allowance.

Measurable today: the goal and trigger math, the mark rate, live and goal, card
density, free-line counts per block, and evacuate totals. Must be added: the
target knob, the three functions, a pause cost model calibrated by EWMA over
prior pauses, and moved-byte and abort-rate counters.

What a target cannot guarantee in phase 1: bounded pause duration. Every
collection still stops for the root walk, the pin pass, forwarding setup, the
copy of live bytes in the set, and the scan of the set. The budget bounds the
copy prefix only; the fixed walk cost applies even to a one-region set. Without
concurrent marking and an incremental root walk there is no preemption point,
and skipping old-to-old edges on young cards alone is unsound.

## 6. New code

Phase 1 (reuse plus policy; all sizes are estimates):

| file | symbol | purpose | size |
|---|---|---|---|
| `src/runtime/logos_g1.go` | `logosG1Mode` | g1 mode predicate | 10 |
| `src/runtime/logos_g1.go` | `logosG1SelectCandidates` | rank by free-line ratio, truncate by budget | 80 |
| `src/runtime/logos_g1.go` | `logosG1YoungOnly` | young versus mixed using age and minors-since-full | 40 |
| `src/runtime/logos_g1.go` | `logosPauseSelect`, `logosPauseBudget`, `logosPauseObserve` | pause policy | 90 |
| `src/runtime/logos_g1_barrier.go` | combined store path | card bit plus line bit | 60 |
| `src/runtime/logos_gcmode.go` | `g1` case | dispatch, predicates | 20 |
| `src/cmd/go/internal/logos/config/config.go` | mode list entry | add `g1` | 5 |
| `src/cmd/go/internal/work/logos_gc.go` | phase map, gate text, ldflags | wiring | 20 |
| `logos-docs/SPEC.md`, `logos-docs/IMPLEMENTATION.md` | Section 19, 10.3, 8.4, M19 | normative rows | 20 |
| tests | ranking, card-clear ownership, humongous exclusion, mode text, gate text | pin the policy | 250 |

Phase 2 (concurrent evacuation, only after the phase 1 gate):

| file | symbol | purpose | size |
|---|---|---|---|
| `src/runtime/logos_g1_conc.go` | concurrent mark state, SATB drain, remark, cleanup | concurrent marking with a stopped remark | 300-500 |
| `src/runtime/logos_g1_conc.go` | load barrier slow path | heal or remap during concurrent evacuation | 100-200 |
| `src/cmd/compile/internal/ssa/logos_g1_barrier.go` | load barrier emitter | reuse the zgc pass shape | 50 |

REJECTED, because each duplicates a landed mechanism:

| symbol | reason |
|---|---|
| private region table | duplicates the immix block table |
| per-region source bitmap remembered set | duplicates the 512 B card table plus the immix dirty pair |
| SATB buffer type at phase 1 | there is no concurrent mark at phase 1 |
| second trigger, worker, or assist formula | duplicates `src/runtime/mgcpacer.go` |

## 7. Invariants

1. Mode coherence: mode value, gate, and docs agree. Enforced by the closed list
   and the phase fatal; tested by the unknown-mode and gate messages.
2. The barrier writes nothing that selection recomputes: it may only set the
   card byte and the line bit. Enforced at the code boundary; tested by a
   store-only run leaving the recomputed planes unchanged.
3. Selection reads post-scan state only. Enforced by state order; tested by a
   ranking unit test that feeds pre-scan input.
4. Forwarding lifetime is pause-local. Enforced by the world-stopped assertion
   at entry and the free at pause end; tested by a checkmark run whose verify
   pass fails on a stale forwarding read.
5. Pinned regions never enter the set. Enforced by the candidate filter; tested
   by the pin matrix (cgo pointer, conservative frame, pointer-keyed block).
6. Humongous spans never evacuate. Enforced by the size and shape check; tested
   by an address-stability assertion across a mixed cycle.
7. Card clear ownership: the owning span clears once; evacuated cards read clean
   and survivor cards read re-dirtied after the pause.
8. Plain `go` is unaffected. Enforced by the mode predicate on every branch;
   tested by `go test -short std` with no flags.
9. The budget is an upper bound only: it never skips a root and never overrides
   a forced full cycle.

## 8. Failure modes

| failure | detection |
|---|---|
| missed old-to-young card, live young survivor collected | old-to-young store matrix across GC with checkmark verify |
| missed pin, stale copy retained after a move | pin matrix plus abort-rate counter |
| address-hashed block moved, map lookup returns a zero value | pointer-key map test under evacuation; fails loudly in `go test -short std` |
| card clear and line-dirty clear disagree | post-cycle audit comparing card scan against the dirty union |
| raw `-X` bypass produces an unreproducible g1 binary | build-metadata check that runtime mode and cmd/go selection agree |
| destination pool exhausted mid-set | pool-shortfall counter plus an abort-or-compact path test |
| pin veto masks heap growth: short pauses, zero movement | moved bytes per pause against heap growth |
| phase declared complete on a subset run | require the full `go test -short std` with the experiment on |

## 9. Honest limits

- Phase 1 cannot promise a bounded pause. It bounds the copy prefix. A real
  pause bound needs concurrent marking, an incremental root walk, and phase 2.
- SPEC changes are required before the mode can be selected: Section 19 mode
  list and error string, Section 10.3 `gc` key, a phase-3 gate sentence, and two
  Section 8.4 barrier lines plus the pointer-keyed-map line.
- It differs from Java G1 in four fixed ways: regions are 32 KiB blocks, not
  multi-megabyte regions; remembered sets are card bytes plus line bits, not
  per-region source bitmaps; phase 1 evacuation is stop-the-world per set; pause
  control is a controller cap, not a separate predictor with concurrent
  refinement.
- Both languages share one collector. loGo/s packages get `-gc=g1` from the same
  flag, the same `logos.toml [build] gc` key, and the same runtime. No new
  directive or syntax is added. Arena objects keep the Section 8.3 invariants:
  referents stay alive across `runtime.GC`, and the pin rules that already
  exclude arena-referenced blocks continue to apply.
- Plain `go` keeps the upstream collector and pacing until `./run.bash` passes
  with a changed default. Any leak of g1 behavior into the no-flag path is a
  defect even when tests pass.
