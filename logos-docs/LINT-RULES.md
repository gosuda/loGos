# Lint rules for Go and loGo/s

Status: detailed per-rule reference. This file expands the
ruleset design in [LINT-RULESET.md](LINT-RULESET.md) into one entry per
rule. Grounded in the code of this worktree
(`.outline/worktree/integ-gcmode`, commit `305b55c73b`); every existing-rule
entry cites its source file. Claimed-but-unsurveyed details are marked
[UNVERIFIED]. Companions: [LINT-RULESET.md](LINT-RULESET.md) (normative
design), [SPEC.md](SPEC.md) Sec 1.2 (rule prose), [SPEC.md](SPEC.md) Sec 16
(JSON diagnostics).

## Engines and severity ladder

Two engines, one policy.

- **The in-compiler lint** (`cmd/compile/internal/logos/lint`) owns
  depth for both language scopes. It sees lowering, escape facts, and
  ownership that no external tool can recover. Per scope:
  - **Go scope**, for plain Go packages. Deep in-compiler rules run only
    under `logos lint`, never under `logos build` or plain `go`
    (T-102; `logos_lint.go:24-54` dedicated lint actor). Rule severity is
    capped: `forbid` in Go lowers to `deny` so `//logos:allow` can
    silence a false positive (`policy.go:158-171`).
  - **loGo/s scope**, for `.lgo` packages. All 21 registered rules run on
    every compile that names a policy, findings print inline, `deny` and
    `forbid` stop the compile at the lint hook
    (`noder/logos_lint.go:80-84`, `logosLintHook`).
- **golangci-lint** (`logos-docs/golangci.yml`) covers repository Go
  tooling code only: the compiler and cmd/go packages the fork adds.
  It never touches `.lgo` sources or lowered output (LINT-RULESET.md
  Verdict). Its severities live in its own config, not in
  `logos.toml`.

Severity ladder, lowest to highest (`severity.go:12-31`).

| Severity | Meaning |
|---|---|
| `allow` | Silent. The rule does not run at all. |
| `warn` | Prints `warning: [<rule>] <message>`, never fails the build or `logos lint`. |
| `deny` | Error unless a `//logos:allow(<group::name>)` directive precedes the statement or declaration. Fails builds and `logos lint`. |
| `forbid` | Error that configuration cannot lower and, in a loGo/s package, a directive cannot silence; the directive itself is reported as `correctness::allow_forbidden_rule`. |

Build behavior by severity: `LogosReport` counts everything but `"warn"`
as an error (`base/logos_diag.go:54-59`), so `warn` never fails a build
and the other three do. Under `-logos.lintonly` the compile continues
through escape analysis so syntax-phase and escape-phase findings print
together, then exits nonzero iff any queued diagnostic is an error
(`work/logos_lint.go:24-54`).

## Rule groups

Nine groups (`severity.go:41-57`). Group defaults
(`DefaultSeverity`, `severity.go:73-87`): `correctness` and `concurrency`
default to `forbid`, `slop` and `complexity` to `deny`, `perf`, `style`,
`suspicious`, and `portability` to `warn`, `pedantic` to `allow`.

| Group | Default severity | Unlowerable? |
|---|---|---|
| `correctness` | `forbid` | Yes: `setGroup` rejects lowering (`policy.go`) |
| `concurrency` | `forbid` | Yes: `setGroup` rejects lowering |
| `slop` | `deny` | No |
| `complexity` | `deny` | No |
| `perf` | `warn` | No |
| `style` | `warn` | No; zero registered rules, an accepted key that currently governs nothing |
| `suspicious` | `warn` | No |
| `portability` | `warn` | No |
| `pedantic` | `allow` | No; raised to `warn` by `--pedantic` |

The forbid groups are `correctness` and `concurrency`
(`config/lints.go:31-32` `forbidGroups`); `[configuration::forbid_lowered]`
rejects any lower value at parse time. Two rules in `correctness` carry a
per-rule `Default: Deny` carve-out (SPEC T-102: they are deny rules that
live in a forbid group, so they do not fail every build) and are detailed
below.

## Existing rules

### correctness::arena_pointer_escape

Source: `src/cmd/compile/internal/logos/lint/rule_arena_pointer_escape.go:13-36`.

- Severity: Forbid (group default, `correctness`).
- Scope: Go and .lgo (`Go: true`).
- Checks: the address of a frame object kept past the frame through an
  unsafe address. A frame object is a local variable, a field of a local
  struct, an element of a local array, the result of `new` or of an
  address-of composite literal; a local that every assignment sets to such
  an address stands for it. Slice elements, map entries, and anything
  reached through a pointer are not frame storage.
- Forms: the integer form `uintptr(unsafe.Pointer(&x))` is reported
  wherever the value is stored, including arithmetic on it (an integer
  keeps nothing alive and does not follow the object when its frame moves
  or dies). The pointer form `unsafe.Pointer(&x)` is reported only when it
  leaves the frame (returned, sent, assigned to a named result, or stored
  in non-frame storage).
- Exemptions: a pointer form held in a local variable stays in the frame
  and is exempt, because the pointer dies with the frame; a call argument,
  a comparison, and an assignment to the blank identifier store nothing.
- Enforcement: `Check` (`checkArenaPointerEscape`), reads the syntax tree
  and checker objects only, so it holds for both languages.
- Fixture: `test/logos/lint/arena_pointer_escape.lgo` plus
  `arena_pointer_escape_go.go`.
- Config keys and thresholds: none.

### correctness::infinite_recursion

Source: `src/cmd/compile/internal/logos/lint/rule_infinite_recursion.go:12-25`.

- Severity: Forbid (group default).
- Scope: Go and .lgo.
- Checks: a function whose every entry path reaches a self-call with
  identical-by-object arguments before any return, branch, panic, exit,
  `?`, or yield. A recursion that can leave another way, or that never
  reaches the call, is silent; a shadowing or renamed variable, or a
  parameter reassigned on the path, defeats the match because the
  arguments then carry other values.
- Exemptions: none beyond the fail-safe paths above; the rule fails safe
  on any path it cannot prove.
- Enforcement: `Check` (`checkInfiniteRecursion`), syntax plus checker
  objects.
- Fixtures: `test/logos/lint/infinite_recursion.lgo`,
  `infinite_recursion_go.go`, and
  `infinite_recursion_const_switch.lgo` (const-switch shape).
- Config keys and thresholds: none.

### correctness::typed_nil_return

Source: `src/cmd/compile/internal/logos/lint/rule_typed_nil_return.go:12-25`.

- Severity: Forbid (group default) in .lgo; effectively Deny in Go,
  where only the `(*T)(nil)` form is active (Go has no options or
  platform pointers) and the Go cap lowers forbid to deny.
- Scope: Go and .lgo.
- Checks: a `return` statement handing a possibly nil `(*T)(nil)`
  conversion, an un-narrowed option, or a platform pointer to an
  interface or error result without a dominating nil check. An interface
  that holds a nil pointer is not nil, so the caller's `err != nil` test
  passes while `err` is nil inside.
- Exemptions: none listed; the return value must reach the interface or
  error result directly.
- Enforcement: `Check` (`checkTypedNilReturn`) with the nil-flow analysis
  in `nilflow.go`.
- Fixture: `test/logos/lint/typed_nil_return.lgo` plus
  `typed_nil_return_go.go`.
- Config keys and thresholds: none.

### correctness::unclosed_io_closer

Source: `src/cmd/compile/internal/logos/lint/rule_unclosed_io_closer.go:12-28`.

- Severity: **Default: Deny** (SPEC carve-out from the group default
  Forbid: a deny rule placed in the forbid group so ordinary code with a
  dropped `Close` does not fail every build).
- Scope: Go and .lgo.
- Checks: a local variable that holds an `io.Closer` returned by a call,
  in a function that neither defers its `Close` nor hands the value to
  another owner. A variable is held when an assignment or declaration
  inside the function sets it from a call, directly or through a loGo/s
  `?`, and its type has a `Close() error` method. One finding per
  variable, at its first assignment from a call.
- Exemptions: parameters, named results, and variables of another
  function are not held; a conversion and a call of a builtin are not
  calls that return an owner. The function counts as closing the variable
  when a `defer` statement, or a loGo/s scope defer, calls its `Close`,
  either directly (with or without `?`) or inside a deferred function
  literal. An errdefer does not close it: it runs only on failure. The
  function counts as handing the variable on when the variable appears
  anywhere except as the receiver of a method call, a field selection, an
  operand of `==` or `!=`, or the target of an assignment: returned,
  stored, sent, passed as an argument, aliased, taken as a method value,
  or captured by a function literal.
- Enforcement: `Check` (`checkUnclosedIOCloser`).
- Fixture: `test/logos/lint/unclosed_io_closer.lgo` plus
  `unclosed_io_closer_go.go`.
- Config keys and thresholds: none.

### correctness::unbounded_channel_op

Source: `src/cmd/compile/internal/logos/lint/rule_unbounded_channel_op.go:7-27`.

- Severity: **Default: Deny** (SPEC carve-out, same reason as above).
- Scope: Go and .lgo.
- Checks: a channel send or receive that has no cancellation path. An
  operation is bounded in two cases only. First, it is a case of a select
  statement that has a `<-x.Done()` case, where `x` is any expression and
  the call returns a channel; a receive of `x.Done()` on its own is the
  wait for cancellation itself. Second, its channel is a local variable
  made once, in the function, by `make(chan T, k)` with a constant
  capacity `k`, and the function sends on the channel at most `k` times
  in total, never inside a loop. For the second case the function is the
  declaration or top-level literal together with every literal nested in
  it; a literal inside a loop counts as inside it, and a send in a select
  case counts. The two conditions are properties of the channel: when the
  sends exceed `k`, or any send sits in a loop, every operation on that
  channel is reported, including the receives. A receive is bounded by
  the same conditions as the sends. In loGo/s, a plain channel operation
  directly inside a `go group` child block is bounded too, because
  lowering turns it into a select on the group context's `Done` channel.
- Exemptions: the statement bodies of a done select are ordinary code; a
  range over a channel, `close`, and `len` are not operations of this
  rule. A select with a `default` case but no done case is not exempt. A
  function literal inside a `go group` child block is not covered by the
  group-context bounding.
- Enforcement: `Check` (`checkUnboundedChannelOp`), reading syntax plus
  checker objects; the channel-capacity leg resolves constants through
  `go/constant`.
- Fixture: `test/logos/lint/unbounded_channel_op.lgo` plus
  `unbounded_channel_op_go.go`.
- Config keys and thresholds: none.

### correctness::nil_deref_unproven

Source: `src/cmd/compile/internal/logos/lint/rule_nil_deref_unproven.go:11-22`;
enforcement in `src/cmd/compile/internal/types2/option_logos.go` and
`narrow_logos.go` (per survey, not re-read).

- Severity: Forbid.
- Scope: .lgo only. Go has no options, so the rule never applies to Go.
- Checks: field or method selection on an option (`x.f`, `x.m()` with
  `x` of type `T?`) before a nil test proves presence.
- Exemptions: `//logos:allow` and a directive that names the rule have
  the standard contract; the finding stops the compile before `Run`, so
  the allow is applied by the resolver, not the rule.
- Enforcement: **types2 handover**. The checker enforces it under this ID
  while checking; the rule registers with a nil `Check`, the finding is
  reported once, and it carries the group and scope so severity
  resolution, the `allow_forbidden_rule` check, and `TestRuleScopes`
  cover it. The compiler routes the checker's error through
  `NilDerefResult` (`nilderef.go`), which adds the JSON autofix
  candidates of SPEC Sec 1.2: optional chaining with `?` default and an
  early-return guard.
- Fixture: `test/logos/lint/nil_deref_unproven.lgo` (carries
  `// errorcheck -gcflags=-e`, not plain `errorcheck`) and
  `nil_deref_unproven_allow.lgo` (the silenced twin).
- Config keys and thresholds: none.

### correctness::defer_erases_errdefer_state

Source: `src/cmd/compile/internal/logos/lint/rule_defer_erases_errdefer_state.go:12-25`.

- Severity: Forbid (group default).
- Scope: .lgo only.
- Checks: in a function with `errdefer`, a plain deferred function
  literal assigning nil to the error result. The deferred literal runs
  before the errdefer cleanup, clears the result, and the errdefer sees a
  success and skips its rollback. The rule reads the function that owns
  the defer: an errdefer statement and a deferred literal count for the
  function that contains them directly, never for an enclosing or nested
  function literal, and the assigned variable must be the last, named
  result of that function, matched by object so a shadowing variable does
  not count. The order of the defer and the errdefer does not matter.
- Exemptions: a plain deferred literal is `defer func() {...}()`;
  `defer f()?`, scope defer, and a deferred call of a named function are
  not plain literals and are not examined. An assignment inside a
  function literal nested in the deferred literal is not searched,
  because that literal may never run.
- Enforcement: `Check` (`checkDeferErasesErrdeferState`); forbid, and
  applies to loGo/s packages only.
- Fixture: `test/logos/lint/defer_erases_errdefer_state.lgo`; no `_go.go`
  twin (not language-neutral).
- Config keys and thresholds: none.

### concurrency::concurrent_multiword_race

Source: `src/cmd/compile/internal/logos/lint/rule_concurrent_multiword_race.go:12-32`.

- Severity: Forbid (group default).
- Scope: Go and .lgo.
- Checks: a local variable of multi-word type that a child captures and
  either side writes after the spawn. A child is the function literal of
  a `go` statement or a loGo/s group child block. Multi-word: a string
  (two words), a slice (three), an interface or union (two), a complex
  number (two), and a struct or array whose value-bearing storage
  (padding and zero-sized fields excluded) spans more than one target
  machine word. A write is an assignment, an increment, or a range
  assignment to the variable itself or to a field or array element held
  in it. The spawn starts the write window, which ends with the innermost
  enclosing `go group` block (the group joins its children there) or
  otherwise with the enclosing declaration; a write by the child is
  inside the window.
- Exemptions: a value of a logos/sync type is exempt (synchronized by
  contract), and so is a const view, which nothing writes through;
  storing through a pointer, slice element, or map entry writes other
  storage and is not a write of the variable.
- Enforcement: `Check` (`checkConcurrentMultiwordRace`); syntax plus
  checker objects, so it holds for Go packages through the `go` statement
  leg and for loGo/s packages through both legs. The word-size question
  reads `buildcfg` target word size (`internal/buildcfg` import).
- Fixture: `test/logos/lint/concurrent_multiword_race.lgo` plus
  `concurrent_multiword_race_go.go`.
- Config keys and thresholds: none.

### concurrency::reentrant_withlock_deadlock

Source: `src/cmd/compile/internal/logos/lint/rule_reentrant_withlock_deadlock.go:16-35`.

- Severity: Forbid (group default).
- Scope: Go and .lgo.
- Checks: inside the function literal or lambda passed to `WithLock` of a
  `logos/sync.Locked`, a call to `WithLock` on the same receiver
  expression, which blocks forever on the mutex the outer call holds. The
  inner call counts when it appears directly in the callback, in the body
  of a directly invoked or deferred function literal, or anywhere in the
  synchronous call graph of the same-package functions the callback
  calls, at any depth. Same-package functions compose receiver keys
  across call sites through the existing `lockKey` substitution, so
  mutually recursive helpers reach a fixed point with no call-depth cap.
  Two receiver expressions match when they name the same variable through
  the same chain of field selections, ignoring address-of and
  dereference.
- Exemptions: a `go` statement evaluates its receiver and arguments
  synchronously and only invokes its call asynchronously, so the invoked
  call and a literal that is its function are excluded while argument
  subexpressions stay in the flow. A stored function literal that is
  never invoked is skipped. A receiver that is any other expression never
  matches, so the rule fails safe.
- Enforcement: `Check` (`checkReentrantWithLockDeadlock`); syntax plus
  checker objects, so it holds for Go and loGo/s packages alike; a lambda
  exists in loGo/s only.
- Fixture: `test/logos/lint/reentrant_withlock_deadlock.lgo` plus
  `reentrant_withlock_deadlock_go.go`.
- Config keys and thresholds: none.

### concurrency::unsupervised_goroutine

Source: `src/cmd/compile/internal/logos/lint/rule_unsupervised_goroutine.go:11-23`;
enforcement in `src/cmd/compile/internal/types2/group_logos.go` (M8, per
survey).

- Severity: Forbid.
- Scope: .lgo only. Go packages keep the bare `go` statement, so the rule
  never applies to Go.
- Checks: a `go` statement in a loGo/s package outside a `go group`
  block, that is a bare statement in a loGo/s package outside a `go group`
  block.
- Exemptions: none listed. A `go` statement inside a `go group` block is
  the supervised form.
- Enforcement: **types2 handover**. The checker already reports it under
  this ID while it checks the package (M8, `types2/group_logos.go`),
  before `Run` is called. The entry has no `Check`, so the finding is
  reported once, and it carries the group and scope so it resolves as a
  forbid rule and keeps ignoring `//logos:allow`; a directive that names
  it is reported as `correctness::allow_forbidden_rule`.
- Fixture: `test/logos/lint/unsupervised_goroutine.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### slop::discarded_error

Source: `src/cmd/compile/internal/logos/lint/rule_discarded_error.go:12-27`.

- Severity: Deny (group default).
- Scope: Go and .lgo.
- Checks: an expression statement or a blank assignment (`_ =`) that
  drops an error result.
- Exemptions, with reasons: callees whose package facts carry
  `AlwaysNilError` (the callee is declared to never return an error; no
  producer of the fact exists yet, so the exemption applies only to facts
  the importer has recorded), `fmt.Print*` and `fmt.Fprint*` to
  `os.Stdout` or `os.Stderr` (printing is the effect; their error results
  are conventionally ignored), and the `Write*` methods of
  `*bytes.Buffer` and `*strings.Builder` (writes to in-memory buffers
  cannot fail in practice).
- Enforcement: `Check` (`checkDiscardedError`); a `discardCheck` walks one
  file and reports dropped error results, per `returnsError`.
- Fixture: `test/logos/lint/discarded_error.lgo` plus
  `discarded_error_go.go`.
- Config keys and thresholds: none.

### slop::err_chain_reset

Source: `src/cmd/compile/internal/logos/lint/rule_err_chain_reset.go:12-25`.

- Severity: Deny (group default).
- Scope: Go and .lgo.
- Checks: a `fmt.Errorf` call that formats an operand of error type with a
  verb other than `%w`, or that passes `err.Error()` or a string
  concatenated from it, so `errors.Is` and `errors.As` cannot see through
  the new error. The verbs come from the constant format string. A call
  whose arguments are one format string with the operand each one formats
  found the way package `fmt` finds it: a `*` width or precision and an
  explicit `!h` index move the operand, and `%w` takes none.
- Exemptions: calls to functions of the form `Format(s fmt.State, verb
  rune) bool` (the `fmt.Formatter` interface method) are skipped, since
  that call does not mean a broken chain. A call whose arguments are not
  constant format has no known errors per operand, so it is not flagged.
- Enforcement: `Check` (`checkErrChainReset`). It offers a fix that
  replaces the verb of every offending operand with `%w` when the format
  is a string literal and each offender has such a verb.
- Fixture: `test/logos/lint/err_chain_reset.lgo` plus
  `err_chain_reset_go.go`.
- Config keys and thresholds: none.

### slop::todo_placeholder

Source: `src/cmd/compile/internal/logos/lint/rule_todo_placeholder.go:12-27`.

- Severity: Deny (group default).
- Scope: Go and .lgo.
- Checks: unfinished work left in a non-test file, in two forms. A comment
  whose text, after the comment marker and any white space, starts with
  the word `TODO` or `FIXME`; a word ends at a character that cannot be
  part of an identifier, so `TODOS` and `FIXMEs` are prose. A call of the
  predeclared `panic` whose argument is a constant string holding
  `"unimplemented"` or `"not implemented"` in any letter case, or
  `"TODO"` as written.
- Exemptions: a file whose name ends in `_test` is skipped entirely (test
  code is allowed to carry work notes).
- Enforcement: `Check` (`checkTodoPlaceholder`); comments come from the
  scanner via `syntax.CommentsDoMode`, the form of `syntax.CommentsDo`
  that reads a loGo/s file with its own token rules, so a `//` inside an
  interpolated string is never a comment.
- Fixture: `test/logos/lint/todo_placeholder.lgo` plus
  `todo_placeholder_go.go`.
- Config keys and thresholds: none.

### complexity::cognitive_overflow

Source: `src/cmd/compile/internal/logos/lint/rule_cognitive_overflow.go:12-26`.

- Severity: Deny (group default).
- Scope: Go and .lgo.
- Checks: a function that exceeds one of four limits of the
  `logos.toml [lints.thresholds]` table: `max_nesting_depth`,
  `max_function_params`, `max_function_lines`, or
  `max_cognitive_complexity`. A function declaration, a function literal,
  and a loGo/s lambda are each a function, measured on their own: a
  nested function body adds nothing to the function around it, though its
  lines belong to that function's line count. A function with more than
  one exceeded limit gets one finding that names them all.
- Scoring, as stated in the rule header: the nesting structures are `if`,
  `for`, `switch`, and `select`, with the loGo/s `const if`,
  `const switch`, and switch expression. The nesting depth is the deepest
  level of nested structures, counting the deepest structure itself, so a
  `for` inside an `if` inside a `for` has depth 3. The cognitive score
  adds 1 plus the number of structures around the point for each of: a
  structure, a case with a condition (not a default clause), a goto, and
  a labeled break or continue; a case sits at the level of its switch, as
  gofmt writes it. An `else if` continues its `if` at the same level and
  adds the same as an `if` there. Each change between `&&` and `||` in an
  unparenthesized chain adds 1, so `a && b && c` adds nothing and
  `a && b || c` adds 1. The line count runs from the `func` line through
  the closing brace, blank and comment lines included. The parameter
  count is the number of declared parameters, not the receiver.
- Exemptions: none in the rule header.
- Enforcement: `Check` (`checkCognitiveOverflow`), measured against the
  policy thresholds.
- Fixture: `test/logos/lint/cognitive_overflow.lgo` plus
  `cognitive_overflow_go.go`.
- Config keys and thresholds: `max_nesting_depth` (3),
  `max_cognitive_complexity` (15), `max_function_lines` (60),
  `max_function_params` (4); all four must be positive.

### perf::large_backing_array_pin

Source: `src/cmd/compile/internal/logos/lint/rule_large_backing_array_pin.go:11-23`.

- Severity: Warn (group default).
- Scope: Go and .lgo.
- Checks: a subslice or substring of a backing array larger than 64 KB
  that escapes, so the whole array stays reachable for as long as the
  small part does. `PinFinding` carries where the slice expression starts,
  how many bytes the backing store has, and whether the sliced value is a
  string.
- Exemptions: none in the rule header.
- Enforcement: **escape-phase**. The compiler decides the escape inside
  escape analysis, a phase after `Run`: the rule has no `Check`, like a
  rule an earlier phase enforces, and `RunPins` resolves the findings the
  escape phase hands over. `PinActive` reports whether the escape phase
  must track large backing stores for a package (the rule is on at the
  severity of the package and applies to it), and `RunPins` resolves each
  escaping subslice once, ignoring inlined bodies and generic
  instantiations analyzed more than once, and ignores a finding in a file
  the package does not hold.
- Fixture: `test/logos/lint/large_backing_array_pin.lgo` plus
  `large_backing_array_pin_go.go`.
- Config keys and thresholds: the 64 KB threshold is fixed in the rule
  (not a `logos.toml` threshold).

### perf::time_after_in_loop

Source: `src/cmd/compile/internal/logos/lint/rule_time_after_in_loop.go:12-22`.

- Severity: Warn (group default).
- Scope: Go and .lgo.
- Checks: a call to `time.After` inside a loop body. Each pass through
  the body creates a timer that stays allocated until it fires, and the
  compiler never hoists the call out of the loop, so the program behaves
  the same at every optimization level.
- Exemptions: a `time.After` call in the header of a `for` statement
  (the `init`, condition, or post statement) and in a range operand run
  outside the body. The rule is language-neutral: it reads Go syntax and
  loGo/s syntax only.
- Enforcement: `Check` (`checkTimeAfterInLoop`); `timeAfterWalk` walks one
  file, depth counting the loop bodies a statement sits under from the
  nodes `timeAfterWalk` visits; a `for` statement handled by visitor
  checks whether a `time.After` call is one of its body statements
  (`rule_time_after_in_loop.go` `timeAfterWalk`).
- Fixture: `test/logos/lint/time_after_in_loop.lgo` (carries
  `// errorcheck -0`, the warn-only convention) plus
  `time_after_in_loop_go.go`.
- Config keys and thresholds: none.

### suspicious::unapplied_struct_default

Source: `src/cmd/compile/internal/logos/lint/rule_unapplied_struct_default.go:11-19`;
enforcement in `src/cmd/compile/internal/types2/structdefault_logos.go`
(M6-S3, per survey, not re-read).

- Severity: Warn (group default).
- Scope: .lgo only.
- Checks: `var x T` or `new(T)` for a struct type with field defaults,
  which leaves every default unapplied.
- Exemptions: `//logos:allow` applies like any other finding; the rule is
  a soft warning.
- Enforcement: **types2 handover**. The types2 checker finds it while it
  checks the package (`types2/structdefault_logos.go`, M6-S3), before
  `Run` is called, and hands the finding to `Run` in `Input.Reported`,
  which applies the severity to the suspicious group and `//logos:allow`
  like any other finding. The entry has no `Check`, so the finding is
  reported once, and it carries the group and scope. The rule never
  applies to Go packages, which have no field defaults.
- Fixture: `test/logos/lint/unapplied_struct_default.lgo`; no `_go.go`
  twin.
- Config keys and thresholds: none.

### suspicious::side_effect_before_sort

Source: `src/cmd/compile/internal/logos/lint/rule_side_effect_before_sort.go:11-19`;
enforcement in `src/cmd/compile/internal/types2/chains_logos.go:594-597`
(`logosWarnSideEffectBeforeSort`, per survey, not re-read).

- Severity: Warn (group default).
- Scope: .lgo only.
- Checks: a chain lambda with a call of unknown purity before
  `.sortBy()`: the fused prefix may run its effects an unexpected number
  of times once sorting materializes the run. Only the lambdas of the
  chain prefix (the sortBy receiver, transitively) are scanned.
- Exemptions, with reasons: the sortBy key lambda itself and the receiver
  base expression are out of scope (only prefix lambdas are scanned).
  Known-pure calls are exempt: a conversion (the callee names a type) and
  an effect-free builtin. Copy, clear, delete, and append mutate their
  arguments; panic, recover, print, and println have observable effects;
  all of those count as unknown. Every other call, including method and
  package-qualified calls, counts as unknown. Nested function literals
  are pruned: their calls run only when the literal is invoked, not per
  element.
- Enforcement: **types2 handover**. The types2 checker finds it while it
  checks the package (`types2/chains_logos.go:589-601`,
  `logosWarnSideEffectBeforeSort`), reporting through `softErrorf`, and
  `LogosWarnOnly` keeps it out of `firstErr`; the compiler hands the
  finding to `Run` in `Input.Reported`, which applies the severity of the
  suspicious group and `//logos:allow` like any other finding. The entry
  has no `Check`, so the finding is reported once, and it carries the
  group and scope. The rule never applies to Go packages, which have no
  chain lambdas.
- Fixture: `test/logos/lint/side_effect_before_sort.lgo`; no `_go.go`
  twin.
- Config keys and thresholds: none.

### portability::serialized_unsized_int

Source: `src/cmd/compile/internal/logos/lint/rule_serialized_unsized_int.go:12-27`.

- Severity: Warn (group default).
- Scope: .lgo only.
- Checks: a field of type `int` or `uint` in a struct that has a field
  with a `json` tag. The generated binary layout (M11-S3) writes every
  integer as 8 bytes and ignores tag options, so a field is flagged
  whatever its own tag says and whether or not it is exported, but `int`
  and `uint` are 4 bytes on a 32-bit target: the same struct serializes
  differently per target width.
- Exemptions: a defined type over `int`, a pointer, and a slice are not
  flagged: they are other types.
- Enforcement: `Check` (`checkSerializedUnsizedInt`).
- Fixture: `test/logos/lint/serialized_unsized_int.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### portability::alias_extension

Source: `src/cmd/compile/internal/logos/lint/rule_alias_extension.go:12-19`;
raised by the go command in `src/cmd/go/internal/load/logos_alias.go`
(per survey, not re-read).

- Severity: Warn (group default).
- Scope: .lgo alias-extension sources only; never applies to Go packages.
- Checks: a source file with a loGo/s alias extension in a main module
  whose path starts with a dotted element, where the module could be
  published, so module zip rules reject the file name. The go command
  reports this under this ID while it loads the package, before the
  compiler runs; the compiler registers the severity, portability group
  in `logos.toml`, and `//logos:allow` directives directly above the
  package clause of the file, because the compiler sees only the source
  text, not the file extension rules of the module, and it carries the
  group and scope. The rule never applies to Go packages.
- Exemptions: a `//logos:allow(portability::alias_extension)` in the
  comment block directly above the package clause silences it at warn
  and deny; the directive sits there because that is the only line the
  compiler sees before the package clause.
- Enforcement: **go command**. The entry has no `Check`; the finding is
  reported once, and it carries the group and scope.
- Fixture: `test/logos/lint/alias_extension.lgo` (carries
  `// errorcheck -0`, since warnings do not fail `logos lint`).
- Config keys and thresholds: none.

### pedantic::exhaustive_union_switch

Source: `src/cmd/compile/internal/logos/lint/rule_exhaustive_union_switch.go:12-20`.

- Severity: Allow (group default; reports only under `--pedantic` or a
  `pedantic` key).
- Scope: .lgo only.
- Checks: a `default` clause in a type switch over a closed union, where
  the cases are exhaustive over the union value.
- Exemptions: none listed in the rule header.
- Enforcement: `Check` (`checkExhaustiveUnionSwitch`).
- Fixture: `test/logos/lint/exhaustive_union_switch.lgo`; no `_go.go`
  twin.
- Config keys and thresholds: none; a `pedantic` group key of `warn` in
  `logos.toml` turns it on, as does `--pedantic`.

### perf::zeroalloc_violation (special, not a registered Rule)

Source: `src/cmd/compile/internal/logos/lint/zeroalloc.go:13-23`.

- Severity: every result is at Forbid (`ZeroAllocResults`).
- Scope: applies wherever the directive is parsed and escape analysis
  runs; the Go-package leg is pinned by
  `test/logos/lint/zeroalloc_go.go` (the Go twin of
  `test/logos/codegen/zeroalloc.lgo`, applied by `logos lint` in Go) and
  by the `logos_zeroalloc.txt` script test. The lint hook sets
  `LogosZeroAlloc` only when the package carries marked functions, so a
  compile with no `//logos:zeroalloc` directive keeps the flow graph and
  `-m` output of plain Go (`escape/zeroalloc_logos.go:15-27`). Whether
  the .lgo build path arms it outside `logos lint` on every compile
  [UNVERIFIED: not surveyed].
- Checks: `//logos:zeroalloc` on the line above a function declaration
  asserts that the function (plus inlined code and its literals)
  performs no heap allocation, and escape analysis verifies.
- Exemptions: none; the rule is deliberately unconfigurable and
  unsilenceable (see the zeroalloc contract section).
- Enforcement: **escape-phase**. `ZeroAllocFuncs` scans directives and
  returns misplaced-directive results; the escape phase finds the
  allocation sites (`LogosLintEscape`, `gc/logos_lint.go:12-27`), and
  `ZeroAllocResults` resolves them to Forbid results.
- Fixture: `test/logos/lint/zeroalloc_go.go` (carries `// errorcheck`);
  the .lgo origin is `test/logos/codegen/zeroalloc.lgo`.
- Config keys and thresholds: none; `perf::zeroalloc_violation` is not a
  `[lints]` group key and no severity lowers it.

Count: 21 registered rules (13 language-neutral plus 8 .lgo-only, pinned
by `TestRuleScopes`, `lint_test.go:22-52`) plus the unregistered
zeroalloc diagnostic.

## Planned rules

Every entry below is planned, not registered. The one normative source is
[LINT-RULESET.md](LINT-RULESET.md); where that document names no field,
this file records `unknown` rather than inventing one. Enforcement,
fixtures, exemptions, and thresholds for planned rules are unverifiable
until the rules land, except where LINT-RULESET.md states them.

Severity is decidable from the existing machinery (`severity.go:73-87`,
`registry.go`): the `logos.toml` group key wins, else a per-rule
`Default` set at registration, else the group default. LINT-RULESET.md
names no per-rule Defaults, so a planned rule reports at its group
default unless registration adds one: `correctness` Forbid, `style`
Warn, `slop` Deny, `portability` Warn, `pedantic` Allow. Tier 2 states
one exception outright: `linkname_discipline` is Deny, which implies a
per-rule `Default: Deny` at registration, the mechanism
`unclosed_io_closer` uses today in the forbid group.

Phase 1, type and syntax only (LINT-RULESET.md Tier 1): each new rule
registers in the existing registry with an .lgo attack fixture and a
`_go.go` twin (stated by LINT-RULESET.md). `TestRuleScopes` extends and
the registry-count assertion updates. `BenchmarkLintOverhead` stays under
the 2 percent budget with the full phase-1 set on; a rule that breaks the
budget does not land.

### correctness::printf_misuse (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble; no per-rule `Default` stated).
- Scope: Go and .lgo (LINT-RULESET.md titles the tier "correctness
  analyzers for both languages", and each phase-1 rule lands with an
  .lgo attack fixture and a `_go.go` twin, which is the registry's
  language-neutral shape).
- Checks: verb and argument count per call, including `$"..."`
  interpolation (LINT-RULESET.md Tier 1 table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/printf_misuse.lgo` plus
  `printf_misuse_go.go` (the phase-1 convention LINT-RULESET.md states
  for every new rule; paths are the convention, not yet existing files).
- Config keys and thresholds: none; LINT-RULESET.md states "No new
  thresholds."

### correctness::unreachable_code (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: statements after return, panic, `?` propagation, or terminal
  yield (LINT-RULESET.md Tier 1 table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/unreachable_code.lgo` plus
  `unreachable_code_go.go` (phase-1 convention, not yet existing files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::lostcancel (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: a `WithCancel` or `WithTimeout` result whose cancel is dropped
  (LINT-RULESET.md Tier 1 table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/lostcancel.lgo` plus `lostcancel_go.go`
  (phase-1 convention, not yet existing files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::copy_lock (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: a value containing a lock type copied by assignment or argument
  (LINT-RULESET.md Tier 1 table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/copy_lock.lgo` plus `copy_lock_go.go`
  (phase-1 convention, not yet existing files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::bools_simplify (planned, phase 1)

- Group and severity: `style`, `warn` (LINT-RULESET.md Tier 1 table
  names group and severity explicitly, the one phase-1 exception to the
  group-default rule; the table lists the id under the `correctness`
  heading but assigns group `style`, which is the field as written).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: always-true or always-false boolean expressions.
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/bools_simplify.lgo` plus
  `bools_simplify_go.go` (phase-1 convention, not yet existing files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::string_intconv (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: `string(int)` and `int(string)` conversions (LINT-RULESET.md
  Tier 1 table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/string_intconv.lgo` plus
  `string_intconv_go.go` (phase-1 convention, not yet existing files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::structtag_malformed (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: malformed `json` or `logos` struct tags (LINT-RULESET.md Tier 1
  table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/structtag_malformed.lgo` plus
  `structtag_malformed_go.go` (phase-1 convention, not yet existing
  files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::stdmethods_shape (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go and .lgo, same tier basis as `printf_misuse`.
- Checks: wrong signatures for `Read`, `Write`, `MarshalJSON`, and the
  binary family (LINT-RULESET.md Tier 1 table).
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/stdmethods_shape.lgo` plus
  `stdmethods_shape_go.go` (phase-1 convention, not yet existing files).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### correctness::tests_malformed (planned, phase 1)

- Group and severity: `correctness`; Forbid (group default, per the
  section preamble).
- Scope: Go only, per LINT-RULESET.md ("Go only" in the tier table);
  checks malformed `Test` or `Benchmark` signatures in `_go.go` twins.
  Whether it also runs on ordinary `_test.go` files is unknown.
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: a `_go.go` twin is the rule's own subject; the fixture layout
  for a Go-only rule is unknown (the phase-1 convention pairs every rule
  with an .lgo attack file, which a Go-only rule cannot have).
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### portability::linkname_discipline (planned, Tier 2)

- Group and severity: `portability`, Deny (LINT-RULESET.md Tier 2,
  stated).
- Scope: unverifiable from the ruleset; LINT-RULESET.md states only the
  path classes ("outside logos/rt and runtime files") and no language
  scope for this rule.
- Checks: a `go:linkname` outside `logos/rt` and runtime files that names
  a runtime symbol not on the allowlist beside the rule, each entry with
  a reason (LINT-RULESET.md Tier 2, stated). This is the checklinkname
  analog; the gVisor contract (`checklinkname/README.md`) type-checks
  `//go:linkname` against pinned expected signatures and reports stdlib
  signature drift, removed stdlib symbols, new unknown linknames, and
  malformed directives.
- Exemptions and enforcement: unknown until the rule lands, except that
  the allowlist "beside the rule" is part of the design.
- Fixture: `test/logos/lint/linkname_discipline.lgo` plus a `_go.go`
  twin is unverifiable: the ruleset states the phase-1 twin convention
  for Tier 1 only, and Tier 2 names no fixture for this rule.
- Config keys and thresholds: none stated; the symbol allowlist lives
  beside the rule, not in `logos.toml`.

### slop::empty_critical_section (planned, Tier 3)

- Group and severity: `slop`; Deny (group default, per the section
  preamble).
- Scope: unknown; LINT-RULESET.md names no language scope. The
  `WithLock` callback shape exists in Go and loGo/s alike (the
  concurrency rules that read the same shape are language-neutral),
  which suggests Go and .lgo [INFERENCE].
- Checks: a `WithLock` callback or lock pair whose body touches no
  protected state (LINT-RULESET.md Tier 3, stated). The gVisor analog is
  staticcheck `SA2001` (empty critical section), which gVisor keeps on
  for internal code with named intentional excludes.
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/empty_critical_section.lgo` plus a
  `_go.go` twin if the scope proves language-neutral; unverifiable from
  the ruleset.
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

### pedantic::unused_allow (planned, Tier 3)

- Group and severity: `pedantic`; Allow (group default, per the section
  preamble). The pedantic group is the `--pedantic` flag's severity
  surface (`config/lints.go:52-55`: `--pedantic` raises an `allow`
  group to `warn`, `--pedantic=false` forces `allow`), so the rule
  reports at warn under `--pedantic` and stays silent otherwise,
  whether or not registration adds a per-rule `Default`.
- Scope: Go and .lgo, decidable from the directive machinery. The allow
  mechanism itself runs on both kinds: `collectDirectives`
  (`directive.go:41-55`) scans every file of the lint `Input`, setting
  the parser mode by `in.Kind`, and the finding would be read from the
  same directive tables the `Run` pass already builds.
- Checks: a `//logos:allow(rule)` that matched no finding in the file
  (LINT-RULESET.md Tier 3, stated). This is nolintlint strictness on the
  fork's own mechanism, and it keeps inline allows from rotting. It
  corresponds to the suppression-discipline line "An allow must name a
  rule. An unused allow is itself a finding."
- Exemptions and enforcement: unknown until the rule lands.
- Fixture: `test/logos/lint/unused_allow.lgo` plus a `_go.go` twin if
  the scope proves language-neutral; unverifiable from the ruleset.
- Config keys and thresholds: none (LINT-RULESET.md: no new thresholds).

Phase 2, SSA-dependent, blocked on the vet-over-lowered path (LINT-RULESET.md):
`nilness` joins then; `shadow` stays off everywhere (gVisor disabled it
with the stated reason "Disable for now", and this fork's inliner work
increases its noise). Phase 2 analyzers land only after the vet path
exists, with a fixture proving a finding in a .lgo file reports the .lgo
line.

## The zeroalloc contract

`perf::zeroalloc_violation` (`zeroalloc.go`) is not a registered lint
rule and deliberately so (`zeroalloc_test.go:167`,
`TestZeroAllocNotConfigurable`).

- **Unconfigurable.** The directive is the author's own assertion, so no
  `logos.toml` severity lowers the diagnostic and no `//logos:allow`
  silences it. `perf::zeroalloc_violation` accepts no severity key and
  is not one of the nine `[lints]` group keys.
- **Unsilenceable.** Every result is at Forbid (`ZeroAllocResults`),
  regardless of any directive or configuration.
- **Misplaced directives are errors.** `ZeroAllocFuncs` returns one
  result for each directive that marks nothing: a comment that is not on
  its own line, or that does not end directly above a function
  declaration, so a blank line between the directive and the function
  leaves the function unchecked and is an error.
- **Exemptions, with reasons.** The exemption classes are decided by
  escape analysis, in `logosAllocates`
  (`src/cmd/compile/internal/escape/zeroalloc_logos.go:183-205`): a
  location the solver moved to the heap counts as a finding only when it
  costs a heap allocation at run time.
  - Constant-to-interface conversions are exempt: a conversion of a
    constant, or of a value that needs no storage, is static data (a
    constant lives in read-only data, a zero-size value shares one
    address, a bool or byte indexes a static table,
    `logosStaticIface`).
  - Non-capturing closures are exempt: a function literal that captures
    nothing (`ClosureVars` empty) is static data and needs no
    allocation.
  - Zero-size objects are exempt: a zero-size object has no address of
    its own, so `&T{}`-shaped locations of size zero do not allocate.
  - Runtime fallbacks are exempt: an operation that falls back to the
    heap at run time, such as an append past capacity, a string
    concatenation or conversion longer than the stack buffer, or an
    insert that grows a map, is not an allocation site, because escape
    analysis decides these sites only (`zeroalloc_logos.go:24-27`).
  - `make(chan T)` is the inverse case: it is always a finding, because a
    channel always lives on the heap and no flow edge models it
    (`logosMakeChan`).

LINT-RULESET.md states only the contract line: "zeroalloc accepts no
allow."

## Policy machinery

### The logos.toml [lints] table

Nine group keys (`config/lints.go:24-27`), one value each from
`forbid`, `deny`, `warn`, `allow`: `correctness`, `concurrency`, `slop`,
`complexity`, `perf`, `style`, `suspicious`, `portability`, `pedantic`.
An absent table is identical to an empty table, both yield defaults.
`style` has zero registered rules: the key is accepted but currently
governs nothing.

Constraint: `correctness` and `concurrency` cannot be lowered below
`forbid` (`config/lints.go:31-32`, `policy.go` `setGroup`); the parse
error is `[configuration::forbid_lowered] correctness cannot be lowered
below forbid` (script test `logos_lint_config.txt`). Unknown groups,
unknown thresholds, unknown fields, non-positive thresholds, and a wrong
`version` are build errors (`ParsePolicy`, `policy.go`).

### Thresholds

`[lints.thresholds]` (`config/lints.go:28-30`, `policy.go` `Thresholds`),
six integer keys, all must be positive:

| Key | Default | Consumer |
|---|---|---|
| `max_nesting_depth` | 3 | `complexity::cognitive_overflow` |
| `max_cognitive_complexity` | 15 | `complexity::cognitive_overflow` |
| `max_function_lines` | 60 | `complexity::cognitive_overflow` |
| `max_function_params` | 4 | `complexity::cognitive_overflow` |
| `max_comptime_ast_nodes` | 4096 | comptime expansion, not a lint rule (`[comptime::node_limit]`, SPEC T-205) |
| `max_ctfe_steps` | 100000 | comptime evaluation, not a lint rule (`[comptime::ctfe_step_limit]`, SPEC T-201) |

The complexity family is the only lint consumer of the six;
LINT-RULESET.md states "No new thresholds. The six existing thresholds
cover the complexity family."

### Transport

`logos.toml` -> `Lints.PolicyJSON(pedantic)` -> deterministic JSON
(`LintPolicyVersion = 1`, `config/lints.go:12-17`) -> the `-logos.cfg=<file>`
compiler flag (`base/logos.go:40-43`, written per action by
`logosLintFlags`, `work/logos_lint.go:144-166`) -> `ParsePolicy` in the
compiler (`policy.go`). Policy bytes feed the build action ID, so a
`logos.toml` or `--pedantic` change recompiles (`logosLintActionID`).
`--pedantic` raises `pedantic` from `allow` to `warn` and never lowers a
stricter key; `--pedantic=false` forces `allow` (`config/lints.go:52-55`).
`--json-diagnostics` sets `-logos.json`; the text versus JSON choice is
also part of the action ID.

### Enablement

`logosLintActive` is `LogosCfg != "" && (Logos || LogosLintOnly)`
(`noder/logos_lint.go:80-84`). .lgo packages lint on every compile that
names a policy; Go packages lint only under `-logos.lintonly`, which
only `logos lint` passes, using a dedicated lint actor compile that
type-checks against import export data and stops after escape analysis
with no object file (`work/logos_lint.go:24-54`). Marking:
`logosLintMark` records the policy on every main-module, non-std,
non-testmain package whose module has a parseable `logos.toml`
(third-party and other-module packages get nil and are never linted);
`logosLintXTest` copies it to external test packages
(`load/logos_lint.go:20-39`).

### Planned [lints.scope] classes

LINT-RULESET.md: the `[lints]` table gains a scope block. Classes cap
what can fire per path; Forbid correctness stays on everywhere.

```toml
[lints.scope]
generated = "correctness+concurrency+perf"
external  = "correctness"
internal  = "all"
```

Mapping (stated): `generated` matches emitted or vendored paths (files
named with the `_logos_` prefix, `src/vendor`, `cmd/vendor`); `external`
matches dependency modules outside the main module; `internal` is
everything else. Severity caps, not rule lists: one question, one
answer. Rejected (stated): per-rule per-path tables in the raw nogo.yaml
style; the group ladder plus path classes answers the same question in
one layer.

gVisor provenance (reference extraction): gVisor's nogo.yaml uses an
ordered group list `{name, regex, default}` (first match wins) over four
groups (`checklocks_tests`, `generated`, `external`, `internal`), with
per-group exclude (matches `Finding.Position`) and suppress (matches
`Finding.Message`) lists. The fork's design keeps the class concept but
drops the per-analyzer tables and regex order: three fixed classes and
severity caps.

### The //logos:allow contract

The directive (`directive.go:11-12`) matches
`^//logos:allow(<group::name>)(\s.*)?$` where the rule id part is
`[a-z][a-z_]*::[a-z][a-z_]*`, and must be the first token on its line
(`ownLine`).

- **Statement-level.** The scope runs from the line after the directive
  through the last line of the longest statement or declaration starting
  on the first non-comment line below; a blank line ends the block,
  intervening comments are fine. It silences `deny` and `warn` only.
- **Package-clause allow for alias_extension.** For an alias-extension
  file the directive sits in the comment block directly above its
  package clause, because that is the only location the compiler sees
  for a finding raised by the go command at load time.
- **The three enforcement rules.** (1) `allow` drops the finding and the
  rule does not run. (2) `forbid` always reports, and a
  `//logos:allow(R)` directive naming it in a loGo/s package is itself
  reported as `correctness::allow_forbidden_rule` (`directive.go:15-16`).
  (3) `deny` and `warn` are silenced by the directive on the line
  directly above a statement or declaration containing the finding.
  Severity resolution order (`lint.go` `Run`/`runRule`, `severity.go`):
  group severity from policy, else the rule `Default`, else the group
  default; then allow drops; then forbid reports plus
  `allow_forbidden_rule`; then deny and warn are subject to directives.
  A Go package never reports at forbid (the Go cap lowers it to Deny),
  so the directive can silence Go false positives.

Findings are ordered by file, line, column, then rule; `Result.Severity`
is never `allow`.

## Fixture conventions

- Attack files live at `test/logos/lint/<rule>.lgo`, plus a
  `<rule>_go.go` twin for each of the 13 language-neutral rules
  (`lint/lint.go:34-36`: add the attack file, plus a `_go.go` twin for
  language-neutral rules; nothing else changes). The testdir harness
  (`src/cmd/internal/testdir/logos_test.go`, M0-S6 and M9-S2) runs them:
  `.lgo` files with `logos`, `.go` files under `test/logos/lint/` with
  `logos lint` (M9-S2).
- Present inventory (verified by directory listing): 23 `.lgo` files and
  14 `_go.go` files. Every rule has its `.lgo` twin; nil-deref has two
  (`nil_deref_unproven.lgo` and `nil_deref_unproven_allow.lgo`); every
  Go-scoped rule has its `_go.go` twin; plus `zeroalloc_go.go` (the Go
  twin of `test/logos/codegen/zeroalloc.lgo`, applied only by
  `logos lint` in Go). One extra: `infinite_recursion_const_switch.lgo`
  (const-switch shape).
- `// errorcheck` versus `// errorcheck -0`: deny and forbid rules carry
  `// errorcheck`; warn-only rules carry `// errorcheck -0`, since
  warnings do not fail `logos lint` (for example
  `alias_extension.lgo:1`, `time_after_in_loop.lgo:1`).
- Throwaway module staging: each fixture is staged alone in a throwaway
  `module logostest` and checked with `logos lint ./...`, whose output is
  errorChecked against the `ERROR` comments (`runLogosLint`,
  `logos_test.go:328-348`). `logos lint` skips files named on the
  command line, hence `./...`.
- Unit tests: per-rule detection, exemption, message, span, and allow
  tests in `rule_*_test.go`; scope registry tests
  (`TestRuleScopes`, `TestTypes2EnforcedRule`,
  `TestRegisteredSeverityAndScope`, `TestEnforcedElsewhereRules` in
  `lint_test.go`, `rule_portability_test.go`, `rule_default_test.go`);
  `ReportedOf` and `Resolve` tests (`reported_test.go`); `PinActive` and
  `RunPins` tests (`rule_large_backing_array_pin_test.go`); zeroalloc
  non-configurability (`zeroalloc_test.go`); the perf budget
  `BenchmarkLintOverhead`, target 2 percent of compile time over median
  (`bench_test.go:88-91`).
- cmd/go script tests: eight files under `src/cmd/go/testdata/script/`
  cover the end-to-end behavior: `logos_lint_alias_extension.txt`,
  `logos_lint_escape.txt`, `logos_lint_go.txt`, `logos_lint_json_cache.txt`,
  `logos_lint_nil_deref.txt`, `logos_lint_routed.txt`,
  `logos_lint_xtest.txt`, `logos_lint_config.txt`, plus
  `logos_zeroalloc.txt` for the Go zeroalloc behavior.

## golangci-lint

Current `logos-docs/golangci.yml` (six lines): `version: "2"`,
`linters.default: standard`, exclusions preset `std-error-handling`. It
names no packages and configures no custom linters. Coverage is defined
by the run commands in the repo docs, not the yml: golangci-lint v2
2.15.0, new packages only, once per module, fork `go` first on `PATH`
(`IMPLEMENTATION.md:51`). From `src/cmd`:
`golangci-lint run --config ../../logos-docs/golangci.yml
./compile/internal/logos/... ./go/internal/logos/...`; from `src`:
`golangci-lint run --config ../logos-docs/golangci.yml ./logos/...`.
Hook lines in upstream files and `runtime/*_logos.go` stay under
`go vet` and `run.bash` only, because the upstream tree is not
golangci-clean and G3 forbids unrelated edits there. No formatter
section: `gofmt -l` in the slice gate already covers formatting.

LINT-RULESET.md plans the move from `default: standard` to a curated
set, scope unchanged (new packages only, fork go first on PATH):

| Linter | Decision | Reason |
|---|---|---|
| errcheck | keep | standard set; drops-error coverage complements `slop::discarded_error` on tooling code |
| govet | keep | standard set; the shared analyzer base of the planned phase-1 rules |
| ineffassign | keep | standard set; assignments that are never used |
| staticcheck | keep | standard set; broad bug finder for tooling code |
| unused | keep | standard set; dead identifiers |
| errorlint | add, with asserts off | wraps `%v`-formatted errors and comparisons; the `%w` discipline matches `slop::err_chain_reset` |
| unconvert | add | flags redundant conversions |
| unparam | add | always-same-value parameters |
| noctx | add | HTTP requests without context |
| intrange | add | int range modernization |
| misspell | add, US locale | spelling |
| copyloopvar | add | redundant per-iteration copies (Go 1.22+ loop semantics) |
| gocheckcompilerdirectives | add | validates compiler directive placement |
| gochecknoinits | add, with the one registration-path carve-out | init functions; the carve-out is the registration path |
| nolintlint | add, strict | machine-readable, explained suppressions; the golangci analog of the `//logos:allow` discipline |
| govet nilness | add | SSA-dependent unreachable/nil analysis (the gVisor `nilness` analyzer; joins the external gate now, the compiler-side phase 2 later) |
| shadow | leave off | gVisor disabled it everywhere with the stated reason "Disable for now"; this fork's inliner work increases its noise |
| gocritic (tags beyond diagnostic) | leave off | diagnostic tags only, if enabled at all |
| dupl | leave off | duplicate-code noise |
| funlen | leave off | overlaps `complexity::cognitive_overflow` |
| lll | leave off | line-length noise |
| QF1001 | leave off | De Morgan rewrites; gVisor marked it "Controversial" |
| QF1008 | leave off | omit embedded selector; gVisor marked it "Controversial" |

gVisor provenance for the leave-off rows: gVisor's `nogo.yaml` disables
`shadow` and `composites` everywhere ("Disable for now"), `QF1001` and
`QF1008` partially ("Controversial"), and `SA1019` for generated plus
internal code ("misattribution from golang.org/issue/44195"). The fork's
leave-off list records reasons in the file, in the gVisor style.

### depguard boundary rules

One depguard rule, the fork's own boundary (LINT-RULESET.md): `logos/rt`
and `logos/sync` may not import each other, and nothing under
`cmd/compile` imports `logos/rt` outside the lowering seam files. The
seam-file list is not enumerated in LINT-RULESET.md [UNVERIFIED: the
exact glob list is a decision for the implementation, or lives in the
implementation plan].
