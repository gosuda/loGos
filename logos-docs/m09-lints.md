# M9 lints and JSON diagnostics

Normative sources: `logos-docs/SPEC.md` Sec 1.2 (RFC-102) and Sec 16,
`logos-docs/IMPLEMENTATION.md` M9-S1 to S2. This page closes the M9 P1-S3 docs
row. It describes the behavior the tree implements; parent runs the gates
below to confirm the checkout.

## What the mechanism is

Package `cmd/compile/internal/logos/lint` runs on `.lgo` packages (every
rule) and, under `logos lint` only, on the Go packages of the main module
(the language neutral rules). It runs after the first types2 pass (syntax and
types rules) and inside escape analysis (rule
`perf::large_backing_array_pin`). Rules defined on loGo/s syntax or types
never run on Go packages.

Severities come from `logos.toml [lints]` passed to the compiler as
`-logos.cfg=FILE` (cmd/go writes the parsed policy as JSON into the action
work directory):

| Severity | Meaning |
|---|---|
| forbid | cannot be lowered and ignores `//logos:allow` |
| deny | error unless `//logos:allow(<rule>)` precedes the statement or declaration |
| warn | prints a warning |
| allow | silent |

Group defaults: `correctness` forbid, `concurrency` forbid, `slop` deny,
`complexity` deny, `perf` warn, `style` warn, `suspicious` warn, `portability`
warn, `pedantic` allow. The three early rules resolve the same way: types2
hands `suspicious::unapplied_struct_default` and
`suspicious::side_effect_before_sort` to the lint pass, and cmd/go applies
the `portability` group severity with a `//logos:allow` directive above the
package clause for alias extension files.

JSON output: `--json-diagnostics` prints one JSON object per diagnostic on
stderr, one per line, with schema version `1.0`, severity, rule id, file,
span (start and end line and column, byte offset, byte length), message,
provenance, and autofix candidates.
A Go type error under `logos build --json-diagnostics` prints one valid
record with `file` ending in `.go` and an empty rule id.

## How to enable or observe it

- `logos build` runs lints on `.lgo` packages. `logos lint ./...` reports
  without building binaries and also covers main module Go packages with the
  language neutral subset. Plain `go` has no lint step.
  `logos lint --pedantic` enables the pedantic group; default builds stay
  silent for it.
- Observe with `src/cmd/go/testdata/script/logos_lint_config.txt`
  (deny fails the build, allow silences, forbid plus allow is itself an
  error, config `slop = "warn"` downgrades),
  `logos_lint_go.txt` (Go module: `slop::discarded_error` at deny and no
  loGo/s only rule; build prints no diagnostics; config and allow behavior;
  dependency modules get no diagnostics), `logos_pedantic_lint.txt` (flag
  leg), `test/logos/lint/<rule>.lgo` per rule with ERROR comments plus a
  passing negative case and N-1, N, N+1 boundary cases, and the `.go` twins
  for language neutral rules.
- Parent to run: `go test cmd/go -run 'Script/^logos_lint_config$'`,
  `go test cmd/go -run 'Script/^logos_lint_go$'`,
  `go test cmd/go -run 'Script/^logos_pedantic_lint$'` for the lint leg,
  `go test -short cmd/compile/internal/logos/lint`, and
  `go test cmd/internal/testdir -run 'Test/logos/lint'`; the lint overhead
  benchmark `BenchmarkLintOverhead` reports the 2 percent target.

## Budgets and defaults

| Key | Default | Notes |
|---|---|---|
| max_nesting_depth | 3 | selected limit testing uses N-1, N, N+1 |
| max_cognitive_complexity | 15 | score sums branch constructs plus boolean operator changes |
| max_function_lines | 60 | per function |
| max_function_params | 4 | per function |
| max_comptime_ast_nodes | 4096 | shared with M7 |
| max_ctfe_steps | 100000 | shared with M7 |

Language neutral rules (20, run at deny on Go when the default is forbid):
`arena_pointer_escape`, `infinite_recursion`, `typed_nil_return` (only the
`(*T)(nil)` form is active in Go), `unclosed_io_closer`,
`unbounded_channel_op`, `concurrent_multiword_race` (go statement leg),
`reentrant_withlock_deadlock` (`WithLock` literal leg), `err_chain_reset`,
`discarded_error`, `todo_placeholder`, `cognitive_overflow`,
`large_backing_array_pin`, `time_after_in_loop`, `printf_misuse`,
`copy_lock`, `structtag_malformed`, `string_intconv`, `self_assignment`,
`defer_in_loop`, `httpresponse`. The other 8 rules are `.lgo` only.

## Known limits

- `logos build` runs no lints on Go packages (R-U1). Third party modules of
  either kind get no code lints, only capabilities (M12).
- A `deny` violation fails the build; a `forbid` violation fails even with an
  allow directive (`correctness::allow_forbidden_rule`).
- Autofix candidates naming `?` are emitted for `.lgo` files only.
- `perf::time_after_in_loop` stays a warning: since Go 1.23 an unreferenced
  timer is collected before firing, so hoisting would make behavior depend on
  the optimization level.
