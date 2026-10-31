# M7 comptime (`const if`, `const for`, generics, `const switch`, `ViewAs`)

Normative sources: `logos-docs/SPEC.md` Sec 2.5 (RFC-205), `logos-docs/IMPLEMENTATION.md`
M7-S1 to S5. This page closes the M7 P1-S3 docs row. It describes the behavior
the tree implements; parent runs the gates below to confirm the checkout.

## What the mechanism is

A compile-time-only package plus four syntax forms, all Logos-mode (`.lgo`)
only:

- Virtual package `comptime` (no source; types2 provides it in Logos mode;
  eject removes the import). API: `Fields[T]()`, `Field` struct
  (`Name/Index/Type/Tag/Exported`), `IsStruct[T]()`, `TypeOf[T]()`,
  `Error(msg)`. Using a value of these types at run time is
  `comptime::runtime_use`.
- `const if` (S1): condition must be constant; both branches are
  type-checked, so an undefined name in the dead branch is an error; only the
  live branch block is kept.
- `const for <idx>, <f> := range <expr>` over concrete types (S2) with
  `val.[f]` field projection. The body is unrolled once per field with `f`
  substituted by constants and `val.[f]` by `val.<Name>`; every unrolled copy
  is re-checked by the normal re-check pass (`types2.LogosCheckCopy`) so inner
  lowering reads per-copy types. Each copy lowers against its own virtual
  text region; the host splice composes the finished region texts. After
  lowering every copy node retakes the `const for` position, so DWARF still
  names the `.lgo` line.
- Comptime generics (S3): a generic function containing `const for`,
  `val.[f]`, or `comptime.*` calls is copied per instantiation site as
  `<f>·logosct<N>` in the instantiating package (type arguments substituted in
  syntax, then checked), memoized per type-argument list. Exported comptime
  generics travel in `__.LOGOS` `comptime` records (function source plus
  referenced declarations with qualified names). An instantiation stack
  detects cycles (`comptime::circular_instantiation`). There is no
  `noder/stencil.go` hook in this tree; lowering generates the copies.
  Transitive built packages the root does not import directly are appended to
  the root importcfg (sorted by import path) for build, lint, and content-ID
  hash; plain-Go roots and `BuildN` runs add nothing.
- `const switch T` (S4): exact, tilde, and interface cases with the PLAN
  Sec 7.2 runtime form (`switch any((*T)(nil)).(type)` with `unsafe` rebinding
  for exact cases, `any(v).(I)` for interface cases) plus trailing
  `return buf, nil` when no case matches. Lowering and the infinite-recursion
  lint use the same checked case selector. Types live in
  `cmd/compile/internal/types2/subst_logos.go`.
- `comptime.ViewAs[T](b)` (S5): length check then
  `(*T)(unsafe.Pointer(unsafe.SliceData(b)))` on amd64, arm64, 386, ppc64le,
  s390x, riscv64; a copy into an aligned temporary elsewhere.

## How to enable or observe it

- Write `.lgo` in Logos mode; `comptime` needs no import setup beyond normal
  use. Eject removes the import; eject output must equal native lowering
  (oracle `TestEjectMatchesLowering`).
- Observe with the fixtures named in M7-S1–S5 (`testdata/logos/const_if.lgo`,
  `test/logos/run/const_for.lgo`, `const_for_lines.lgo`,
  `const_for_inner_lowering.lgo`, `comptime_generic.lgo`,
  `const_switch*.lgo`, `viewas.lgo`) and the eject scripts
  `logos_eject_const_for*.txt`, `logos_comptime_*.txt`,
  `logos_eject_comptime.txt`, `logos_eject_const_switch.txt`.
- Parent to run (acceptance, not an executed claim): the S2/S3 18-fixture
  selector
  `go test -count=1 ./cmd/internal/testdir -run '^Test$/^logos$/^(run|compile)$/^(const_for|comptime)'`,
  `go test -race -count=1 cmd/go -run 'TestScript/^logos_eject_const_for$'`
  and the `(cross|foreign|imports)` variants, the `logos_comptime_*` and
  `logos_eject_comptime` scripts, the oracle
  `go test -count=1 ./cmd/compile/internal/logos -run TestEjectMatchesLowering`,
  plus `go vet` over `logos`, `types2`, `work`, `runtime` and scoped `gofmt`.

## Budgets and defaults

- Expanded-node limit uses the selected limit, default 4096; configuration
  MAY raise it. The specified boundary is rejection above the selected N with
  acceptance at N; parent runs the N-1, N, N+1 checks in the S3 gate to confirm.
- `pclntab` name stripping for `·logosct<N>` is owned by M13-S3; S3 asserts
  instantiation, memoization, and cycle behavior, not the stripped name.
- `comptime.ViewAs` length failure panics with
  `logos: ViewAs: need <want> bytes, have <got>`; views keep `b` alive.

## Known limits

- Dead branches are still checked: undefined names, unapplied narrowing
  disqualifications, and `comptime.Error` in a live branch fail compilation.
- Infinite recursion through a selected concrete case is rejected; recursive
  dead cases are ignored.
- `ViewAs` allows only padding-free structs with no `bool`, pointers,
  interfaces, slices, maps, or strings; otherwise
  `comptime::viewas_not_pod` naming the offset. Odd-offset views read correct
  values on amd64/arm64; `GOARCH=mips` compiles via the aligned copy.
- Shadowed cases after a broader case are `comptime::unreachable_case`;
  a `T` matching no case returns the input unchanged.
