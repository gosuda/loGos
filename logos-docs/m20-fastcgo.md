# M20 cgo in loGo/s packages and fast cgo

Normative sources: `logos-docs/SPEC.md` Sec 3.4 (RFC-304),
`logos-docs/IMPLEMENTATION.md` M20-S0 to S1. This page closes the M20 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

- S0 cgo in `.lgo` packages: a file importing `"C"` must use Go syntax only,
  else `correctness::cgo_file_logos_syntax`. Pointers returned by C are
  platform pointers (stored into a nonnil location only with a check). The
  cgo outputs `_cgo_gotypes.go` and `*.cgo1.go` compile with the other `.lgo`
  files; per file mode is keyed on the file name (`isLogosFile`), so the user
  facing no mix rule does not apply to generated files. Go packages use the
  same cmd/cgo pipeline unchanged with the same layout and cache key format.
- S1 fast path: a C function carrying both `#cgo noescape F` and
  `#cgo nocallback F` (Go spelling) or `//logos:cgo_fastcall` (`.lgo`
  spelling, mapped by cmd/cgo to the same pair) takes the fast path: cmd/cgo
  emits a call to `runtime.logosCgocallFast` instead of `runtime.cgocall`.
  The PLAN Sec 8.5 trampoline skips trace bookkeeping, keeps profiling state
  (`gp.syscallpc`, `gp.syscallsp`, `m.ncgo` raised, so `sigprof` attributes a
  signal in C to the Go call site), performs the status change sysmon needs
  to retake the P (same rule and threshold as syscalls), switches stacks
  like `asmcgocall`, and returns on the `exitsyscall` fast path.
- Command defaults: `LogosFastCgo` is false in the shared baseline. Managed
  `logos` commands enable it through `cmd/go/internal/cfg` unless the user
  explicitly selects its state. Plain `go` and raw `logos tool` invocations
  stay opt in. Explicit process and `go env -w` settings keep their ordering
  and override the default, with arena and wasm preemption settings and
  subprocess propagation preserved.
- Foreign contract: descriptors are consumed synchronously during admission;
  the runtime retains no descriptor, slot, or backing array pointer after
  return. The invalid token (`Frame` nil, `Gen` 0) is the disabled mode
  sentinel only (`End` and `Extend` on it are no ops with no pairing). In the
  configured cycle `Begin` returns a valid token even for an empty closure
  and every `Begin` pairs with exactly one `End`; skipping `End` leaks the
  pin and duplicating it throws.

## How to enable or observe it

- Write Go cgo with the directive pair or `.lgo` cgo with
  `//logos:cgo_fastcall`; select with `GOEXPERIMENT=logosfastcgo` (managed
  `logos` on by default, plain `go` opt in, `nologosfastcgo` forces off).
- Observe with `src/cmd/go/testdata/script/logos_cgo_lgo.txt` (Go syntax
  gate, C call result use, checked stores at `.lgo` locations),
  `src/cmd/cgo/internal/testlogos` (`BenchmarkFastcall` with and without
  the experiment; `go tool objdump` shows `logosCgocallFast` for `fast_add`
  and `cgocall` for the single directive twins; `go tool nm` lists exactly
  one fast entry in a mixed binary; generated file names equal the baseline;
  blocking parity with the same C sleep under `GOMAXPROCS=1`; `nocallback`
  panic naming the runtime message with the test finishing inside its
  deadline and later calls still running; CPU profile attribution of C time
  to the call site within 5 percentage points of the plain path).
- Parent to run: cgo tests with the experiment on under both `go` and
  `logos`, the `nocallback`, profile attribution, and blocking parity legs,
  `-race` on `cmd/cgo/internal/testlogos`, and the command default script
  (Logos default on, Go default off, explicit overrides, GOENV precedence,
  cache separation, raw tool control).

## Budgets and defaults

- `BenchmarkFastcall` reports against a plain baseline (PLAN target near
  2 ns; reported, never a pass condition). Blocking parity requires fast
  median at most 1.05 times plain median with standard deviation at most 20
  percent of each median; when noise exceeds the bound the check is blocked,
  not passed.
- Profile attribution bound is 5 percentage points between paths.
- The C sleep parity shape is 100 microseconds under `GOMAXPROCS=1` with
  `N` frozen across both paths.

## No-clock rule and measured result

- The trampoline records no start time. Slot 0 of `m.fastCgoFrame` holds
  the DI/arg copy, `m.fastCallEnterNano` stays zero, slots 1 to 3 stay
  zero, and all are cleared to zero on return. No consumer reads the
  timestamp and the clock read alone cost about 30 ns of the 49 ns
  measured per fast call, so recording a start time would lose the ratio.
- Paired measurement from the same tree with one frozen `N`:
  `BenchmarkFastcall` on the same empty C function gave plain 57.77 ns
  per call and fast 49.24 ns per call at 33,559,636 iterations, ratio
  1.1733 as plain divided by fast, against the 1.05 acceptance ratio.
  This matches T-304 and M20-S1 in `logos-docs/IMPLEMENTATION.md`.
- Method: two builds of the same benchmark, baseline with
  `GOEXPERIMENT=nologosfastcgo` and candidate with
  `GOEXPERIMENT=logosfastcgo`, same host and flags; the ratio is baseline
  median over candidate median.

## Known limits

- Bridge only packages must preserve nilable C struct fields and checked
  stores with `.lgo` locations; ordinary Go syntax errors keep their parser
  diagnostics.
- Callback through a `nocallback` function panics and never hangs; Go
  pointer validity under moving modes stays a gate requirement.
- The fixed cap legacy hold consumers stay until the cgo typed plus raw
  syscall producers are both live and parent gated.
