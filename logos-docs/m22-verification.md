# M22 verification suites and end to end example

Normative sources: `logos-docs/SPEC.md` Sec 11 and Sec 22,
`logos-docs/IMPLEMENTATION.md` M22. This page closes the M22 P1-S3 docs row.
It describes the behavior the tree implements; parent runs the gates below
to confirm the checkout.

## What the mechanism is

- S1 upstream regression (PLAN Sec 22.1): `logos test -O=1 std cmd`, `-O=2`,
  `-O=3` without `-short`, and the same three under plain `go`; no outcome
  is recorded in the `logos-docs/REDTEAM.md` header today (title,
  resolutions line, and table header only), and the six-command matrix has
  not been run green, so parent records S1 as blocked and runs no -O gate
  here.
- S2 eject round trip fuzzing (Sec 22.2) is not shipped in this checkout:
  `test/logos/eject/gen` and `TestEjectRoundTrip` do not exist, so parent
  records S2 as blocked and runs no fuzz counts. The specified shape, when it
  lands, is: generate random well typed `.lgo` programs from a grammar over
  every shipped construct (unions, `const switch T`, `errdefer`, `$"..."`,
  chains, options, groups); run each with `logos run` and, after eject, with
  `go run`, and compare stdout, exit code, and stderr with goroutine stack
  frames removed (lines matching `goroutine`, `.go`, and `.lgo` frame
  patterns are dropped). Specified counts are 1,000 programs slice gate and
  10,000 milestone gate (`-count` flag).
- S3 red team completeness (Sec 22.3): every row of `logos-docs/REDTEAM.md`
  is `tested` or `satisfied upstream` with the proving test; gate
  `grep -c '| planned |' logos-docs/REDTEAM.md` prints 0. Runtime attack
  tests (files under `test/logos/redteam` whose header line is `// runtime`)
  run under `-race` and under each `-gc` mode whose M19 gate has passed
  (greentea always, immix after phase 1, zgc after phase 2); a mode whose
  gate has not passed is recorded as `blocked` with the gate name, never as
  `tested`.
- S4 codegen and `//logos:zeroalloc` (Sec 22.4): the directive makes the
  compiler fail with `perf::zeroalloc_violation` naming the function and the
  escape reason when the function performs a heap allocation.
  `test/logos/codegen/zeroalloc.lgo` covers a passing and a failing function
  plus the SIMD and fast call assertions from M14 and M20.
  `test/logos/codegen/zeroalloc_sites.lgo` requires reports for escaping
  allocations from `new`, maps, string operations, and method values; empty
  slice literals and zero size interface values compile without a diagnostic.
  `zeroalloc_run.lgo` observes zero allocations for those two exemptions.
  `BenchmarkConstMapLookup` reports lookup latency with zero allocations and
  asserts the table symbol is read only; `BenchmarkHotLoad` reports
  `Hot.Load` against a plain atomic pointer load.
- S5 reproducibility (Sec 22.5): building `test/logos/example` twice in
  clean caches yields identical SHA-256 for linux amd64, linux arm64, and js
  wasm (cross compiled on one host, plus the same digests on a second host
  when available); the wasm stripped size is recorded in
  `test/logos/size/wasm_size.txt` and the gate fails on growth above 1
  percent.
- S6 end to end example (Sec 11, tailored per T-Sec11):
  `test/logos/example/order_service.lgo` with a fake database implementation
  and a `main` that runs `ProcessBatch`, plus the expected output file;
  `logos_eject_example.txt` ejects and runs it with the same output.

## How to enable or observe it

- Layout: codegen fixtures, size record, and example service live under
  `test/logos` with the paths named above; the red team ledger lives at
  `logos-docs/REDTEAM.md`; the S2 generator and round trip test are absent
  (see above).
- Parent to run: all of the above in slice then milestone counts, then
  `./run.bash --no-rebuild` as the final milestone gate. Paid or host
  dependent legs (10,000 case fuzz, second host digests, wasm runs without
  `node`) are recorded as blocked when the host cannot run them, never as
  passed.

## Budgets and defaults

- Fuzz counts: 1,000 programs slice gate, 10,000 milestone gate.
- Reproducibility bound: wasm size growth above 1 percent fails.
- Zero alloc: one shared rule (any heap allocation fails the annotated
  function) with exactly two exemptions (empty slice literals, zero size
  interface values).

## Known limits

- Stack frame lines are dropped in the eject comparison by design; a diff
  outside those patterns is a failure.
- Red team rows for unpassed GC modes stay `blocked` with the mode gate name.
- The directive is per function; it does not change allocation behavior,
  only reports it.
