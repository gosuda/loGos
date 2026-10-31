# M15 backend peepholes and long loop preemption

Normative sources: `logos-docs/SPEC.md` Sec 15,
`logos-docs/IMPLEMENTATION.md` M15-S1 to S3. This page closes the M15 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

- Arm64 load and store pairing (S1): the existing SSA `pair` pass pairs
  adjacent loads and stores before `regalloc`; no post regalloc pairing pass
  exists, so the PLAN `ldpstp_logos.go` file is not built. The encodable
  offset check lives in the assembler (128 bit `-1024` to `+1008` multiples
  of 16, 64 bit `-512` to `+504` multiples of 8). 128 bit pairing is shipped
  (`OpARM64FMOVQload` and `OpARM64FMOVQstore` to `FLDPQ` and `FSTPQ`).
  Barriers hold by construction; candidates split by a `CALL`, a join label,
  or an LSE atomic between them stay separate.
- Amd64 masked tails, BMI2, CMOV, checked `make` (S2): shifts by variable
  amounts use `SHLX` and `SHRX` at v3 and v4; immediate 32 and 64 bit rotates
  are `ROLQ` and `ROLL` at v1 and v2 and the width complement `RORXQ` and
  `RORXL` at v3 and v4; variable count rotates keep the existing path at
  every level; 16 and 8 bit immediate rotates keep `ROLW` and `ROLB` at every
  level; ternary shaped branches become `CMOV` through `branchelim`. M14
  slice reductions use masked loads for the tail with no page crossing read
  (a slice ending at a page boundary followed by a `PROT_NONE` page is the
  attack shape). The `.lgo` lowering rewrites `*` inside `make` length and
  capacity arguments to `_logos_rt.MulLen(a, b)` (`bits.Mul64` with overflow
  panic naming `logos: make length overflow: w * h`).
- Long loop preemption on wasm (S3): the SSA back edge check pass
  (`compile.go` position 490, gated by `buildcfg.Experiment.PreemptibleLoops`)
  gains a per loop counter under `-d=logosloopstride=4096`, so the check fires
  every 4096 iterations. For GOARCH=wasm, `logos` sets that experiment by
  default; plain `go` accepts it through `GOEXPERIMENT=preemptibleloops` and
  sets it by default only after parent confirms the wasm run tests pass with
  it on (G3).

## How to enable or observe it

- Automatic under the stated GOEXPERIMENT and CPU level selections.
  A nonintrinsic archsimd call stays a `CALL` in lowered `.lgo` output.
- Observe with the `test/logos/codegen/arm64_pair.go` and `arm64_pair.lgo` twins (offsets 504 and 512
  pair at 504, 992 and 1008 pair at 1008, 1024 and 1040 stay separate,
  alignment and barrier negatives), `amd64_v3` twins (per level rotate and
  shift pins plus upstream `rotate.go`, `mathbits.go`, `arithmetic.go` pins),
  `test/logos/run/make_overflow.lgo` plus its Go twin, and
  `test/logos/run/wasm_loop_preempt.lgo` under `GOOS=js GOARCH=wasm`
  (a call free spinning goroutine does not starve a second goroutine; checks
  fire at iterations 4096 and 8192 with none in the first 4095).
- Parent to run: the `arm64_pair` and `amd64_v3` twin codegen tests with
  `-all_codegen`, the `logosAsmcheckEnv` propagation pin, the eject script
  for the SIMD slice leg, and the wasm loop test with `node` present (record
  as blocked without `node`).

## Budgets and defaults

- Pairing offsets are bounded by the assembler ranges above; masked tail
  loads are ten bodyless `Load<T>SliceMasked` loaders over the existing
  masked SSA operations (256 bit VPMASKMOVD and VPMASKMOVQ behind AVX2,
  zeroing K masked moves behind AVX512).
- Loop stride default is 4096 iterations; checks at N-1, N, N+1 around 4096
  are the specified boundary. `BenchmarkLogosLoopStride` reports check
  overhead with a target below 0.01 percent (reported, not a pass condition).

## Known limits

- Loads into the base register itself are not paired; 8 aligned but not 16
  aligned 16 byte accesses stay two loads.
- Async preemption does not exist on wasm (`preemptMSupported = false`);
  the stride counter is the mechanism, not a scheduler preemption.
- Masked loads regressed from the removed `LoadMasked<T>` form are covered
  by the new loaders; any nonintrinsic call remains a call.
