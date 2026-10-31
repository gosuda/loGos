# M16 wasm profile

Normative sources: `logos-docs/SPEC.md` Sec 14,
`logos-docs/IMPLEMENTATION.md` M16-S1 to S2. This page closes the M16 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

Automatic profile for `GOOS=js GOARCH=wasm` and `GOOS=wasip1 GOARCH=wasm`
with no external tool (no TinyGo), for Go and loGo/s packages alike under
either command, using the installed toolchain only. Under plain `go` it
activates after the gate below passes (G3). Go packages emit `MayYield`
facts on GOARCH=wasm under either command.

- Per function fact `MayYield`: 0 when the function has no `defer`, makes no
  indirect call, makes no call to a function with `MayYield` 1 or unknown
  facts, has no transitive call to `runtime.gopark`, and makes no channel,
  select, `time.Sleep`, `runtime.Gosched`, or blocking `wasmimport` call.
  Blocking `wasmimport` means `poll_oneoff` and `sched_yield` on wasip1; js
  `wasmimport` calls never block by themselves (the js sleep path parks
  through `gopark` after scheduling work).
- S1: functions with `MayYield` 0 skip resume point dispatch in the prologue
  (no `br_table`) and use `local.get`, `local.set` with native `call`
  (`ACALLNORESUME`); functions calling channel operations, with `defer`,
  transitively calling `gopark`, or otherwise yielding keep dispatch. A
  misclassified `MayYield` 0 call that unwinds traps (`UNDEF`).
- S2: `logos build -o app.wasm` writes `app.loader.mjs`; `go build -o
  app.wasm` writes no loader by default and writes one with `--wasm-loader`.
  The loader template `cmd/go/internal/logos/wasmloader.mjs.tmpl` wraps
  `wasm_exec.js` startup and exports `instantiateLogos`. `logos/rt` gains
  wasm only helpers `_logos_scratch_alloc`, `_logos_scratch_reset`,
  `_logos_errmsg_ptr`, and `_logos_errmsg_len`; the wrapper checks the
  `int32` errCode return and throws the read error string. Directives
  `//logos:wasmexport` and `//logos:wasmimport` lower to their `//go:` forms.

## How to enable or observe it

- Cross compile with the stated GOOS and GOARCH values; no flag is needed
  for the profile itself. `logos build --wasm-features=stack-switching`
  exercises the stack switching leg.
- Observe with `test/logos/codegen/wasm_noyield.lgo` (leaf arithmetic has no
  dispatch; channel, defer, and transitive `gopark` cases keep dispatch),
  `src/cmd/go/testdata/script/logos_wasm_loader.txt` (loader emission, Go
  main package with `//go:wasmexport` parity, identical loader bytes, 1 MB
  input forcing `memory.grow`), and
  `src/cmd/go/testdata/script/logos_wasm_stackswitch.txt`.
- Parent to run: the codegen test, `GOOS=wasip1 GOARCH=wasm logos build`
  plus `GOOS=js GOARCH=wasm logos test -short ./...` of `test/logos/wasm`,
  and without `node` record the run leg as blocked (A-NODE). The three
  benchmarks report dispatch free size (target 3x), speedup (target 4x), and
  `MayYield` shares (targets 90 percent and 10 percent) as reported numbers,
  never as pass conditions.

## Budgets and defaults

- No size or speed numbers are gates; all three benchmarks report.
- The flag adds no standard library rebuild (action IDs of `std` stay equal)
  and `go help build` stays byte identical.
- Plain `go` flips its default only after `run.bash` passes with it (G3).

## Known limits

- Without `node`, the execution leg is blocked, not passed.
- Go wasm ports use 8 byte pointers (spec correction recorded by P0).
- Only the installed toolchain is used; TinyGo is never required.
