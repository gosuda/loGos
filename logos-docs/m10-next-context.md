# M10 next and context

Normative sources: `logos-docs/SPEC.md` Sec 1.3 (RFC-103),
`logos-docs/IMPLEMENTATION.md` M10-S1 to S2. This page closes the M10 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

Two compiler modes driven by cmd/go, because cmd/go cannot import
cmd/compile internals:

- `logos next`: compile mode `-logos.mode=next`. It type checks each Go
  package of the module, finds rewrite candidates (assignment of a call whose
  last result is `error` followed immediately by
  `if err != nil { return <zero values>, err }` where `err` is unused later
  and the function returns `error` last; `if err := f(); err != nil`
  variants; `fmt.Sprintf` with only `%s %d %v` and interpolation compatible
  operand types; pure index range append loops), emits byte span edits, and
  output depends on flags: default prints the diff only; `--fix` writes
  `next_tasks.json` (`version logos-next-1` with id, category, file, span,
  rule, function signature, AI instruction, and verification command) and
  applies the edits to every package, renaming files; `--ai-patch` writes the
  plan and prints it. `--fix` and `--ai-patch` are exclusive.
- `logos context`: compile mode `-logos.mode=context` (parse only, no
  imports) driven by `cmd/go/internal/logos/contextcmd`. It prints the
  declaration of a symbol plus signatures of its methods and of types they
  mention (one level), no function bodies, at most 100 lines.

## How to enable or observe it

- Run in the module root: `logos next` prints a unified diff and changes
  nothing; `logos next --fix` renames every Go file to `.lgo` and rewrites
  the candidate patterns to `?`, keeping every comment and build line byte
  identical; `logos next --ai-patch` writes `next_tasks.json` and prints the
  plan without applying it. A pattern whose `if` body does more than return
  (logs, wraps) is left unchanged and listed in `next_tasks.json`. Untouched
  code is byte identical. `logos context <pkg>#<Symbol>` prints the scoped
  declaration; an unknown symbol reports `logos context: p has no symbol
  Symbol`.
- Observe with `src/cmd/go/testdata/script/logos_next.txt` (comments and
  `//go:build` lines preserved; parent runs the script and requires the
  converted module to build and its tests to pass; loop case
  `for i := 0; i < len(xs); i++ { ys = append(ys, f(xs[i])) }` becomes
  `ys := xs.map(x => f(x))` while loops with `break`, `continue`, or side
  effects stay unchanged and appear in `next_tasks.json`; a module importing
  a nonstandard Go package outside itself through a local `replace` converts
  and builds), and `logos_context.txt` (one level signatures, 100 line cap).
- Parent to run: the `logos_next.txt` and `logos_context.txt` script tests;
  `BenchmarkContext` reports the 50 ms target.

## Budgets and defaults

- `logos next` is the only rewriting command. Formatting of `.lgo` files is
  not changed by any command (`logos fmt` does not exist).
- Context output is capped at 100 lines with one level of mentioned types.
- Conversion keeps imports of Go packages outside the module as written
  (R-U2); imported declarations appear as platform declarations.

## Known limits

- Hand conversion stays as fallback: `logos next --fix` output is a starting
  point, and `Convert` step review still applies.
- Impure loops, logging or wrapping error branches, and patterns outside the
  candidate shapes are listed, not rewritten.
- A module with a nonstandard outside dependency needs that dependency
  present through the existing `replace`; `next` does not fetch it.
