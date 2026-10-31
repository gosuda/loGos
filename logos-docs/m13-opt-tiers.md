# M13 optimization tiers, synthetic PGO, clone specialization, ICF

Normative sources: `logos-docs/SPEC.md` Sec 1.5 (RFC-105),
`logos-docs/IMPLEMENTATION.md` M13-S1 to S4. This page closes the M13 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

Backend rule for M13 to M20 (R-U1): every improvement below applies to Go
packages as well as loGo/s packages. Flags and `GOEXPERIMENT` select it under
both commands; plain `go` reads no `logos.toml` and keeps its upstream
defaults until `run.bash` passes with the changed default (G3). Only the
named language rules stay `.lgo` only.

## What the mechanism is

- Levels: `logos build -O` and `go build -O` accept `0, 1, 2, 3, s, z`.
  `-O` applies to all build-flag commands (`build`, `test`, `run`), not only `logos build`/`go build`.
  `logos` reads the default from `logos.toml [build] opt_level` (default
  `O2`); an explicit `-O` overrides the file; `target_cpu` is `logos` only.
  Invalid level reports `logos build: invalid -O level "4"
  (want 0, 1, 2, 3, s, z)` with the `go build:` prefix under plain `go`.
  `go help build` stays byte identical (the flag carries no help text).
- Synthetic PGO: `[build] synthetic_pgo` (`off`, `standard`, `aggressive`)
  passed as `-d=logossynthpgo=`; under `go`, `-O=2` and `-O=3` select the
  same defaults and `-gcflags=-d=logossynthpgo=<level>` overrides them.
  `off` emits no `Cold()` calls. `standard` emits in `?` error blocks (`.lgo`
  only) and before `os.Exit`. `aggressive` also emits before explicit `panic`
  calls. Go packages get the `os.Exit` marks (standard and aggressive) and
  the `panic` marks (aggressive). Emission runs only at O2 and O3.
- Clone specialization: at O2 (callee cost at most 200 nodes) and O3 (no cost
  limit) when `-d=logosspecialize=1`, a same package call with concrete
  arguments produces a clone (`Copy·logosspec1`) with interface calls folded
  per concrete type. Linker function name writing strips `·logosspec<N>` and
  `·logosct<N>` suffixes in `pcln` output.
- ICF at Os and Oz: `ld/icf_logos.go` hashes each eligible text symbol by
  content bytes plus relocation offsets, types, targets, addends, ABI
  version, FuncInfo sizes, identity and flag values, and table contents by
  value, confirms equality, redirects relocations to the kept symbol, drops
  the duplicate, and repeats until nothing merges. Only name and line table
  stay uncompared. Runtime and assembly functions never fold; cgo exports
  keep entries under internal linking.

## How to enable or observe it

- Set the tier per build (`-O=3`) or per module (`opt_level = "O3"`).
  Observe with `src/cmd/go/testdata/script/logos_opt_levels.txt`
  (flag mapping, Go parity, file override, invalid level),
  `test/logos/codegen/property_inline.lgo` at O2,
  `interp_stack.lgo` at O3 (4095, 4096, 4097 byte bounds),
  `recursive_stack` twins at O3 (frame width classes), `cold*.lgo`
  (`Cold()` emission and absence), `specialize.lgo` (clone symbol plus static
  calls), and `ld/logos_icf_test.go` (identical bytes fold, relocation and
  pointer map negatives, caller cascade, cgo entries, `gccheckmark` clean).
- Parent to run: the `logos_opt_levels.txt` script, the codegen twin set
  with `-all_codegen` where triplets apply, the SSA `likelyadjust` unit test,
  `go test -short -O=3 encoding/asn1`, `go test -short -O=3 std` under both
  commands, and the `logos_opt_levels.txt` script, whose embedded `stackbuf`
  fixture owns the level-size pairs.

## Budgets and defaults

- Defaults: plain `go` without `-O` uses upstream O1; `logos` without a flag
  uses file default `O2`. Without a key, O2 selects `standard` synthetic PGO
  and O3 selects `aggressive`; O0, O1, Os, Oz emit nothing. O2 shows
  `-d=inlbudget=120,variablemakethreshold=256,logossynthpgo=standard`; O3 shows
  `-d=inlbudget=400,inlrecursivebudget=80,variablemakethreshold=4096,logosspecialize=1,logossynthpgo=aggressive`;
  Os and Oz show `-d=inlbudget=15` plus link flag `-logos.icf`.

| Level | Variable-make buffer (non-std) | Variable-make buffer (std) | Emitted compiler flags | Emitted linker flags |
|---|---|---|---|---|
| O0 | 32 B | 32 B | `-N -l` | - |
| O1 | 32 B | 32 B | (upstream baseline) | - |
| O2 | 256 B | 32 B | `-d=inlbudget=120,variablemakethreshold=256,logossynthpgo=standard` | - |
| O3 | 4096 B | 32 B | `-d=inlbudget=400,inlrecursivebudget=80,variablemakethreshold=4096,logosspecialize=1,logossynthpgo=aggressive` | - |
| Os | 32 B | 32 B | `-d=inlbudget=15` | `-logos.icf` |
| Oz | 32 B | 32 B | `-d=inlbudget=15` | `-logos.icf` |

- Standard-library exception: packages compiling with standard-library provenance (`p.Standard` or `ForTest` with `search.IsStandardImportPath`) never receive `variablemakethreshold=` in forced gcflags and keep the upstream 32-byte variable-make stack buffer at every opt level (`src/cmd/go/internal/work/logos_stdlib_gate.go`). Upstream Go tests pin exact slice capacity and allocation counts, which fail when the threshold is raised; non-standard packages receive the tier-configured buffer (256 bytes at O2, 4096 bytes at O3). Functions in recursive call-graph cycles also keep the upstream 32-byte threshold at every opt level.
- Escape and prove add no work budgets in this tree (only recursion and
  capacity guards); the O3 spec row states this and the levels add nothing
  for them.
- Specialization blocks on parameters compared with `nil`, reassigned, or
  stored in a struct field; nesting deeper than 4; stack traces still print
  the uncloned name.

## Known limits

- The compiler guesses no Go error block; Go gains only the `os.Exit` and
  `panic` marks.
- Generic dictionaries need no ICF work: shaped paths share code, generic
  functions carry `Dupok`, and the linker picks one duplicate.
- This milestone adds the first README improvements sentence once the Go
  twins pass (standard library short gate at O3 under both commands).
