# M17 smart stack

Normative sources: `logos-docs/SPEC.md` Sec 3.2 (RFC-302),
`logos-docs/IMPLEMENTATION.md` M17-S1 to S2. This page closes the M17 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

- Stack first dynamic slices (S1): M13-S1 already sets
  `variablemakethreshold` per level; defaults, thresholds, and allocation
  budgets stay unchanged here. The change narrows only the terminal backing
  allocation predicate in `escape/foreign_logos.go`: reject nil and
  non `OMAKESLICE` nodes; for a slice make, take `Cap` when present else
  `Len`, and classify effective capacity as variable only when
  `ir.IsSmallIntConst` is false after literal rewriting. Only variable
  effective capacity makes flowing into a cgo call argument or an
  `unsafe.Pointer` conversion are forced to heap by this rule.
  Fixed effective capacity makes retain ordinary escape and size behavior;
  nonforeign dynamic makes stay unchanged. Walk emits a fixed `[K]E` frame
  buffer; the function prologue `morestack` check covers it.
- Per call site initial goroutine stack size (S2): `runtime/stack_logos.go`
  holds a fixed 1024 entry open addressing table keyed by `gopc` storing an
  EMA (weight 1/8) of final stack sizes, updated in `goexit0` (size is
  `gp.stack.hi - gp.stack.lo`) and read in `newproc1` to choose the power of
  two start size bounded by the maximum stack. The experiment field is
  `LogosStack` (added with the standard experiment procedure: field in
  `src/internal/goexperiment`, generated `exp_*_on` and `off` files,
  runtime gate by build tag or generated constant, compiler gate by
  `buildcfg.Experiment`).

## How to enable or observe it

- S1 is automatic at O2 and O3 through the tier threshold. S2 selects with
  `GOEXPERIMENT=logosstack`.
- Observe with the specified O2 stack buffer behavior (at O2,
  `make([]byte, n)` with `n` up to 256 in a nonescaping use takes the stack
  buffer path with `runtime.makeslice` on the slow branch only; parent
  requires the 255, 256, 257 boundary checks with deep recursion forcing
  growth, and the O3 4096 byte exercise; the plan fixture paths
  `test/logos/codegen/stack_make.lgo` and `test/logos/run/stack_make.lgo`
  do not exist in this checkout),
  `test/escape_foreign.go` with `escape_foreign_import.dir` (effective
  capacity boundary through unsafe conversion or imported foreign helpers),
  real cgo `TestFastcallSlicePlacement`, and
  `src/runtime/logos_stack_test.go` (after 1000 goroutines from one `go`
  statement each grow to 64 KB, later goroutines from that statement start at
  64 KB through the `runtime.LogosStartSize(pc)` hook while another statement
  keeps the global size; spawn allocates nothing; 10,000 distinct `go` sites
  stay bounded by table eviction with replacement).
- Parent to run: codegen and testdir checks including the foreign harness
  plus real cgo placement, and
  `GOEXPERIMENT=logosstack go test -short runtime`.

## Budgets and defaults

- Variable make stack path bounds: 256 bytes at O2, 4096 bytes at O3 (the
  upstream 32 byte behavior is retained for functions in a recursive call
  graph cycle). At O3 a directly recursive function with its own variable
  size make, a recursive function inlining three helpers, and a two function
  cycle keep frames of at most three digits while nonrecursive functions with
  the same helpers keep full tier buffers.
- Table capacity is 1024 entries with replacement eviction.

## Known limits

- No per allocation SP guard exists; the spec SP minus n guarantee is not
  implemented as stated and the prologue check is the cover.
- Only the terminal backing allocation predicate is narrowed; the existing
  foreign flow machinery is otherwise preserved.
