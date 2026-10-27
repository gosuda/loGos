---
name: logos-performance
description: Optimize build and run-time performance with the logos toolchain, for .lgo code and for Go code built by it. Use when a logos.toml exists, the files are .lgo, or the user names the logos command, and the user says optimize, speed up, -O levels, PGO, binary size, allocation, arena, GC mode, SIMD, or zero-alloc.
license: BSD-3-Clause
compatibility: Needs the logos command from github.com/gosuda/loGos on PATH.
metadata:
  version: "0.22.0"
  milestone: "M13"
---

# Performance optimization

The logos toolchain provides build-time and run-time optimization controls for .lgo packages and Go packages built by it.

## Procedure

1. Measure baseline performance first. Save benchmark output with `go test -bench=. -count=6 > baseline.txt`.
2. Change exactly one flag or configuration key in `logos.toml` or the build command.
3. Verify against the baseline with the same benchmark and inputs using `benchstat baseline.txt new.txt`.
4. Ensure tests and race checks pass under the selected mode before keeping the configuration.

## Optimization tiers (M13)

The `-O` build flag is accepted by both `logos build` and `go build`. In `logos.toml`, `[build] opt_level` sets the default for `logos` builds. Plain `go` reads no `logos.toml` and requires the explicit `-O` flag.

| Level | Inlining budget | Stack buffer threshold | Specialization | Flags emitted |
|---|---|---|---|---|
| `O0` | none | none | off | `-N -l` |
| `O1` | upstream default | 32 B | off | upstream compiler defaults |
| `O2` | 120 | 256 B | on (cost <= 200) | `-d=inlbudget=120,variablemakethreshold=256` |
| `O3` | 400 | 4096 B | on (unlimited) | `-d=inlbudget=400,inlrecursivebudget=80,variablemakethreshold=4096,logosspecialize=1` |
| `Os` | 15 | 32 B | off | `-d=inlbudget=15`, `-logos.icf` |
| `Oz` | 15 | 32 B | off | `-d=inlbudget=15`, `-logos.icf` |

Rules:

* `logos.toml [build] opt_level` defaults to `"O2"`. Command-line `-O` overrides the file setting (`src/cmd/go/testdata/script/logos_opt_levels.txt`).
* `synthetic_pgo` in `logos.toml [build]` controls branch likelihood hints. Modes are `off`, `standard`, and `aggressive`. O2 defaults to `standard` (marks `?` error blocks and `os.Exit` calls cold). O3 defaults to `aggressive` (also marks explicit `panic` calls cold).
* `target_cpu` in `logos.toml [build]` accepts `"deterministic"` (`GOAMD64=v3`, `GOARM64=v8.2`) or `"native"` (selects host architecture features from `internal/cpu`).
* Clone specialization generates static copies (`·logosspec<N>`) for concrete types at call sites when `-d=logosspecialize=1` is set (`src/cmd/go/testdata/script/logos_opt_levels.txt`). Pclntab strips the suffix so stack traces display the original function name.
* Identical code folding at `Os` and `Oz` (`-logos.icf`) deduplicates text symbols with matching machine code, relocation targets, and metadata (`src/cmd/link/internal/ld/logos_icf_test.go`).
* Improvements apply to Go packages built under `logos` and under plain `go` with `-O`.

## Fixed-array SIMD (M14)

loGo/s supports element-wise vector operators and reductions on qualifying fixed arrays.

Rules:

* Element-wise operators `+`, `-`, `*`, `/`, `&`, `|`, `^` and reductions `.sum()`, `.min()`, `.max()`, `.dot()` are supported on `[N]T` where `N` is a power of two, `T` is an integer or floating-point kind other than complex (`int`, `uint`, and `uintptr` use 64-bit lanes), and total array size is at most 64 bytes (`SPEC.md` Sec 2.4, `src/internal/types/testdata/logos/simd_types.lgo`, `test/logos/run/simd.lgo`).
* Array sizes exceeding 64 bytes fail with `correctness::simd_array_too_large`. Non-power-of-two lengths fail with `correctness::simd_array_shape`.
* Lowering emits vector instructions via `simd/archsimd` when built with `GOEXPERIMENT=simd` on amd64 or arm64 for fixed-width numeric kinds and aliases (`byte`, `rune`, `float32`, `type f32 = float32`), subject to CPU feature gates on amd64 (`AVX`, `AVX2`, `AVX512`) elided at matching `GOAMD64` levels.
* Defined element types, `int`, `uint`, and `uintptr` retain unrolled scalar lowering.
* When `GOEXPERIMENT=simd` is absent, the target is not amd64 or arm64, or CPU feature checks fail, lowering falls back to unrolled scalar code.
* Reductions use a fixed pairwise order across both vector and scalar forms, guaranteeing bit-identical floating-point output.
* Go packages have no array operators or reductions; they use `simd/archsimd` with `GOEXPERIMENT=simd` directly.

## WASM profile (M16)

The WASM profile activates automatically for `GOOS=js GOARCH=wasm` and `GOOS=wasip1 GOARCH=wasm` in `logos` and `go`.

Rules:

* Pure non-yielding functions (`MayYield=0`) skip resume-point `br_table` dispatch in the function prologue and use native WASM calls with `local.get` and `local.set` (`test/logos/codegen/wasm_noyield.lgo`).
* Functions with `defer`, channel operations, select, or transitive calls to `runtime.gopark` retain `MayYield=1` and keep resume dispatch.
* The `--wasm-features=stack-switching` build flag compiles `MayYield=1` functions to `cont.new` and `resume` instructions.
* The `--wasm-loader` flag emits `app.loader.mjs` alongside the WASM binary. `logos build` enables this by default; plain `go` requires the explicit flag (`logos_wasm_loader.txt`).
* Directives `//logos:wasmexport` and `//logos:wasmimport` lower to `//go:wasmexport` and `//go:wasmimport`.
* 64-bit integer and floating-point fields maintain natural 8-byte alignment in structs (`test/logos/codegen/wasm_align.lgo`).

## Stack sizing (M17)

Smart stack allocations keep dynamic allocations on the goroutine stack when escape analysis proves they do not escape.

Rules:

* Non-escaping dynamic slices created with `make([]byte, n)` use a variable-make stack buffer sized by tier: default 32 bytes (O1 / plain), 256 bytes at O2, and 4096 bytes at O3 (`base.Debug.VariableMakeThreshold`, `src/cmd/compile/internal/inline/stackbuf_logos_test.go`).
* Functions in recursive call-graph cycles keep the upstream variable-make threshold (capped at 32 bytes) at every level to prevent frame multiplication across recursion (row #134, `test/logos/codegen/recursive_stack.go`, `test/logos/codegen/recursive_stack.lgo`).
* Dynamic slice sizes exceeding the tier threshold fall back to heap allocation on the slow branch.
* Slices with variable effective capacity flowing into cgo calls or `unsafe.Pointer` conversions allocate backing storage on the heap (row #144, `src/cmd/cgo/internal/testlogos/fastcall_test.go`).
* Fixed-effective-capacity makes retain ordinary escape and size behavior, not a guarantee of stack allocation, and are not forced to the heap by foreign flow (`SPEC.md` Sec 3.2).
* Goroutine initial stack sizing with `GOEXPERIMENT=logosstack` maintains a 1024-entry exponential moving average table keyed by call-site PC to allocate right-sized stacks on spawn (`src/runtime/logos_stack_test.go`).

## Tier-0 arena and bump allocation (M18)

Goroutine-local arena allocation is controlled by `GOEXPERIMENT=logosarena` and `logos.toml [build] arena = true`.

Rules:

* Non-escaping allocations exceeding stack buffer thresholds allocate into goroutine arena chunks instead of the heap when arena mode is active (`src/runtime/logos_arena_test.go`, `src/cmd/go/internal/cfg/logos_arena_test.go`).
* Small noscan objects up to 512 bytes use an inline bump-allocation path marked as an async preemption unsafe point (`src/runtime/logos_arena_test.go`).
* Watermarks follow LIFO ordering and are restored at function returns and loop back-edges.
* Memory holding pointers is scanned by the garbage collector during stack scanning.
* Calling `runtime.SetFinalizer` on an arena object panics (`src/runtime/logos_arena_test.go`).

## GC modes (M19)

Collector selection is controlled by `-gc=MODE` or `logos.toml [build] gc`. Proven by `src/cmd/go/testdata/script/logos_gc_mode.txt` and `src/runtime/logos_gcmode_test.go`.

| Mode | Collector behavior |
|---|---|
| `greentea` | Default under `logos` on native architectures. Elastic worker pacing based on heap utilization; elastic pacing is also the default for an empty production-controller mode. Generational cards use GOEXPERIMENT=logosgen on amd64; their stable-default migration remains blocked. Span deck is default on except on wasm; `nologosspandeck` retains the FIFO comparison path. |
| `batch` | Stops the world for collections; triggers collection at 90% of `GOMEMLIMIT`. Packages compile with `-wb=false`. |
| `epsilon` | No automatic collection. Only explicit `runtime.GC()` collects. |
| `wasm` | Default under `logos` on `GOARCH=wasm`. Runs incremental mark work during browser idle periods. |
| `immix` | Mark-region moving collector requiring `GOEXPERIMENT=logosgcmoving` phase 1. |
| `zgc` | Concurrent colored-pointer collector requiring `GOEXPERIMENT=logosgcmoving` phase 2. |

Rules:

* Plain `go` without `-gc` keeps the upstream Go collector; an empty production-controller mode still uses elastic pacing.
* `batch` and `epsilon` ignore environment `GOGC` settings and disable write barriers outside package `runtime`.
* Generational collection uses GOEXPERIMENT=logosgen on amd64: card-table tracking (1 byte per 512 bytes of heap arena) with mature span skips. Its stable-default migration remains blocked.

## Fast cgo (M20)

Fast-cgo bypasses standard cgo trampoline overhead for eligible non-blocking C functions.

| Language | Annotation |
|---|---|
| Go | `#cgo noescape F` and `#cgo nocallback F` |
| loGo/s | `//logos:cgo_fastcall F` in a `.lgo` file |

Rules:

* The fast path requires both `noescape` and `nocallback`. A function with only one directive stays on the plain cgo path (`src/cmd/cgo/internal/testlogos/fastcall_test.go`).
* `logos` enables `GOEXPERIMENT=logosfastcgo` by default; plain `go` requires explicit opt-in.
* If a `nocallback` function calls back into Go, the runtime panics immediately instead of deadlocking (`TestFastcallNoCallbackViolation`).
* CPU profile samples during fast C calls attribute to the calling Go site (`TestFastcallProfileAttribution`).
* Measured performance status (measured 2026-10-24): BenchmarkFastcall with timestamp records 114.700 ns/op (sd 1.975) versus plain cgo 85.455 ns/op (re-measured 86.360), yielding ratio 0.745. The performance gate requires at least 1.05 and is currently FAIL. Dominant overhead is the per-call `nanotime()` entry timestamp (~36 ns). Do not claim a speedup. Blocking parity, callback parity, and profile attribution pass.

## Zero-allocation enforcement (M22)

The `//logos:zeroalloc` directive asserts that a function causes zero heap allocations.

Rules:

* Place `//logos:zeroalloc` directly above a function declaration in `.lgo` or `.go` files (`test/logos/codegen/zeroalloc.lgo`).
* If escape analysis detects any heap allocation, the build fails with `[perf::zeroalloc_violation] <func> allocates: <reason>`.
* Violations include escaping allocations from `new`, struct literals, map operations, slice growths, string operations, and method values (`test/logos/codegen/zeroalloc_sites.lgo`).
* Empty slice literals and zero-size interface values are exempt and allocate zero heap bytes.

## Diagnostics

| Rule | Fix |
|---|---|
| `correctness::simd_array_too_large` | Reduce array length or element type size so the fixed array does not exceed 64 bytes. |
| `correctness::simd_array_shape` | Use a power-of-two array length for element-wise SIMD operations. |
| `perf::zeroalloc_violation` | Eliminate heap allocations, use stack buffers, or remove the `//logos:zeroalloc` directive. |
| `correctness::cgo_file_logos_syntax` | Use standard Go syntax in files importing `"C"`. |
| `comptime::node_limit` | Reduce comptime AST unrolling or raise `max_comptime_ast_nodes` in `logos.toml`. |
