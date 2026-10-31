# M14 fixed array SIMD

Normative sources: `logos-docs/SPEC.md` Sec 2.4 (RFC-204),
`logos-docs/IMPLEMENTATION.md` M14-S1. This page closes the M14 P1-S3 docs
row. It describes the behavior the tree implements; parent runs the gates
below to confirm the checkout.

## What the mechanism is

Elementwise operators and the `sum`, `min`, `max`, `dot` reduction family on
qualifying fixed array types (for example `[4]float64`, `[8]float32`,
`[16]uint8`, `[32]byte`, `[64]byte`). Types2 allows the operators in Logos
mode (`simd_logos.go`); lowering emits `simd/archsimd` calls when the build
has `GOEXPERIMENT=simd` and GOARCH is amd64 or arm64, else unrolled scalar
statements. Reductions use the fixed pairwise order in both lowerings, so
float results are bit identical with and without the experiment. Reductions
over slices use the vector form with masked tail loads (M15-S2).

## How to enable or observe it

- Write `.lgo` array expressions; build with `GOEXPERIMENT=simd` on amd64 or
  arm64 for the vector form. Observe with `testdata/logos/simd_types.lgo`,
  `test/logos/run/simd.lgo` (every operator and reduction against a scalar
  loop oracle), and `test/logos/codegen/simd.lgo` (amd64 `VADDPS` for
  `[8]float32` addition, arm64 `FADD` vector form).
- Parent to run: types2, testdir, and codegen tests with `GOEXPERIMENT=simd`;
  the M15 eject script for the slice reduction leg.

## Budgets and defaults

- Allowed shapes are power of two lengths whose storage fits the 64 byte cap;
  `[1024]float64` (8192 bytes) reports
  `correctness::simd_array_too_large`, and non power of two lengths are
  rejected. Parent runs the N-1, N, N+1 checks around the cap in the slice
  gate.
- Default without the experiment is unrolled scalar statements with identical
  numeric results.

## Known limits

- These operators are loGo/s syntax and type rules because Go gives `[N]T +
  [N]T` no meaning (R-U1). M14 adds no Go package syntax. Go packages keep
  `simd/archsimd` (upstream, `GOEXPERIMENT=simd`) as their SIMD surface, and
  the M15 backend gains reach them.
- Slice reductions need the M15 masked loaders to avoid page crossing reads.
- Scalar form limits: integer `/` has no archsimd method on either arch, and
  `*` has none on amd64 8 bit lanes or arm64 64 bit integer lanes; those
  operators take the scalar form.
