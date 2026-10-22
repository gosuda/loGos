# Lint rules for Go and loGo/s

Status: detailed per-rule reference. This file expands the ruleset design in [LINT-RULESET.md](LINT-RULESET.md) into one entry per rule. Grounded in the code of this worktree (`.outline/worktree/integ-gcmode`, commit `305b55c73b`); every existing-rule entry cites its source file. Implemented inventory is distinguished from proposed target design throughout. Companions: [LINT-RULESET.md](LINT-RULESET.md) (normative design), [SPEC.md](SPEC.md) Sec 1.2 (rule prose), [SPEC.md](SPEC.md) Sec 16 (JSON diagnostics).

## Engines and severity ladder

Two engines, one policy.

- **The in-compiler lint** (`cmd/compile/internal/logos/lint`) owns depth for both language scopes. It sees lowering, escape facts, and ownership that no external tool can recover. Per scope:
  - **Go scope**, for plain Go packages. Deep in-compiler rules run only under `logos lint`, never under `logos build` or plain `go` (T-102; `src/cmd/go/internal/work/logos_lint.go:24-54` dedicated lint actor). Rule severity is capped: `forbid` in Go lowers to `deny` so `//logos:allow` can silence a false positive (`src/cmd/compile/internal/logos/lint/policy.go:158-171`).
  - **loGo/s scope**, for `.lgo` packages. All 21 registered rules run on every compile that names a policy; findings print inline; `deny` and `forbid` stop the compile at the lint hook (`src/cmd/compile/internal/noder/logos_lint.go:80-84`, `logosLintHook`). The proposed target design specifies dedicated lint execution for both languages with no adoption performance budgets, while compiler language and type safety checks remain mandatory.
- **golangci-lint** (`logos-docs/golangci.yml`) covers repository Go tooling code only: the compiler and cmd/go packages the fork adds. It never touches `.lgo` sources or lowered compiler representations. Its severities live in its own configuration, not in `logos.toml`.

Severity ladder, lowest to highest (`src/cmd/compile/internal/logos/lint/severity.go:12-31`):

| Severity | Meaning |
|---|---|
| `allow` | Silent. The rule produces no finding and never fails builds or `logos lint`. |
| `warn` | Prints `warning: [<rule>] <message>`, never fails builds or `logos lint`. |
| `deny` | Error unless a `//logos:allow(<group::name>)` directive precedes the statement or declaration. Fails builds and `logos lint`. |
| `forbid` | Error that configuration cannot lower. In a loGo/s package, a directive cannot silence it; the directive itself is reported as `correctness::allow_forbidden_rule`. |

Build behavior by severity: `LogosReport` counts `deny` and `forbid` as errors (`src/cmd/compile/internal/base/logos_diag.go:54-59`). `warn` produces warnings only and never fails a build or lint run. `allow` suppresses rule execution or emission entirely: it produces no findings and never fails any build. Under `-logos.lintonly`, the compile continues through escape analysis so syntax-phase and escape-phase findings print together, then exits nonzero if any queued diagnostic is an error (`src/cmd/go/internal/work/logos_lint.go:24-54`).

## Rule groups and thresholds

Nine groups (`src/cmd/compile/internal/logos/lint/severity.go:41-57`). Group defaults (`DefaultSeverity`, `severity.go:73-87`): `correctness` and `concurrency` default to `forbid`, `slop` and `complexity` to `deny`, `perf`, `style`, `suspicious`, and `portability` to `warn`, `pedantic` to `allow`.

| Group | Default severity | Unlowerable? |
|---|---|---|
| `correctness` | `forbid` | Yes: `setGroup` rejects lowering (`src/cmd/compile/internal/logos/lint/policy.go`) |
| `concurrency` | `forbid` | Yes: `setGroup` rejects lowering |
| `slop` | `deny` | No |
| `complexity` | `deny` | No |
| `perf` | `warn` | No |
| `style` | `warn` | No: zero registered rules, an accepted key that currently governs nothing |
| `suspicious` | `warn` | No |
| `portability` | `warn` | No |
| `pedantic` | `allow` | No: raised to `warn` by `--pedantic` |

The forbid groups are `correctness` and `concurrency` (`src/cmd/go/internal/logos/config/lints.go:30-32` `forbidGroups`); `[configuration::forbid_lowered]` rejects any lower value at parse time. Two rules in `correctness` carry a per-rule `Default: Deny` carve-out (SPEC T-102: they are deny rules that live in a forbid group, so they do not fail every build) and are detailed below.

Policy severity resolution follows strict precedence in `Policy.Level(id, k)` (`src/cmd/compile/internal/logos/lint/policy.go:160-176`):
1. **Group override in configuration** (`p.groups[g]`): An explicit group setting in `logos.toml` under `[lints]` (e.g. `correctness = "forbid"`) takes precedence over all defaults.
2. **Rule-level default** (`r.Default` via `Lookup(id)`): If the group is absent from `p.groups`, a rule with an explicit `Default != Allow` runs at its registered default (e.g. `Deny` for `unclosed_io_closer` and `unbounded_channel_op`).
3. **Group-level default** (`DefaultSeverity(g)`): Fallback when the group is unconfigured and the rule specifies no custom default (`Forbid` for `correctness` and `concurrency`).
4. **Go-package cap**: In Go packages (`KindGo`), `Forbid` is clamped to `Deny` so `//logos:allow` can silence false positives.

Because `p.groups[g]` takes precedence over `r.Default`, setting `correctness = "forbid"` in `logos.toml` overrides rule defaults and elevates `unclosed_io_closer` and `unbounded_channel_op` from `deny` to `forbid` in `.lgo` (and `deny` in Go), as verified by `rule_default_test.go:34-40`. Therefore, a minimal, default-equivalent configuration must omit `[lints]` or provide an empty `[lints]` table to preserve these carve-outs:

```toml
# Minimal default-equivalent configuration:
# Omitting [lints] or specifying an empty [lints] table preserves the Rule.Default
# carve-outs (unclosed_io_closer and unbounded_channel_op remain Deny).
[lints]
```

Furthermore, because `correctness` is an unlowerable group (`forbidGroups`), proposed heuristic checks cannot default to `warn` under `correctness` (any group evaluation or explicit `forbid` would elevate them); they are placed under `suspicious` (default `warn`) instead.

### Thresholds

`[lints.thresholds]` (`src/cmd/go/internal/logos/config/lints.go:25-28`, `policy.go` `Thresholds`), six integer keys, all must be positive:

| Key | Default | Consumer | Purpose |
|---|---|---|---|
| `max_nesting_depth` | 3 | `complexity::cognitive_overflow` | Complexity lint control: maximum block nesting level |
| `max_cognitive_complexity` | 15 | `complexity::cognitive_overflow` | Complexity lint control: maximum cognitive score |
| `max_function_lines` | 60 | `complexity::cognitive_overflow` | Complexity lint control: maximum lines in function body |
| `max_function_params` | 4 | `complexity::cognitive_overflow` | Complexity lint control: maximum parameter count |
| `max_comptime_ast_nodes` | 4096 | Comptime expansion limit (`[comptime::node_limit]`, SPEC T-205) | Comptime resource limit, not a complexity lint control |
| `max_ctfe_steps` | 100000 | Comptime evaluation limit (`[comptime::ctfe_step_limit]`, SPEC T-201) | Comptime resource limit, not a complexity lint control |

The complexity family consumes the first four thresholds; the remaining two guard comptime compiler resources against divergent macros or infinite loops during evaluation.
## Existing rules (implemented inventory)

Count: 21 registered rules (13 language-neutral plus 8 .lgo-only, pinned by `TestRuleScopes`, `src/cmd/compile/internal/logos/lint/lint_test.go:22-52` and `registry.go`) plus the unregistered zeroalloc diagnostic.

### correctness::arena_pointer_escape

Source: `src/cmd/compile/internal/logos/lint/rule_arena_pointer_escape.go:13-36`.

- Severity: Forbid (group default, `correctness`).
- Scope: Go and .lgo (`Go: true`).
- Checks: the address of a frame object kept past the frame through an unsafe address. A frame object is a local variable, a field of a local struct, an element of a local array, the result of `new` or of an address-of composite literal; a local that every assignment sets to such an address stands for it. Slice elements, map entries, and anything reached through a pointer are not frame storage.
- Forms: the integer form `uintptr(unsafe.Pointer(&x))` is reported wherever the value is stored, including arithmetic on it (an integer keeps nothing alive and does not follow the object when its frame moves or dies). The pointer form `unsafe.Pointer(&x)` is reported only when it leaves the frame (returned, sent, assigned to a named result, or stored in non-frame storage).
- Exemptions: a pointer form held in a local variable stays in the frame and is exempt, because the pointer dies with the frame; a call argument, a comparison, and an assignment to the blank identifier store nothing.
- Enforcement: `Check` (`checkArenaPointerEscape`), reads the syntax tree and checker objects only, so it holds for both languages.
- Fixture: `test/logos/lint/arena_pointer_escape.lgo` plus `arena_pointer_escape_go.go`.
- Config keys and thresholds: none.

### correctness::infinite_recursion

Source: `src/cmd/compile/internal/logos/lint/rule_infinite_recursion.go:12-25`.

- Severity: Forbid (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: a function whose every entry path reaches a self-call with identical-by-object arguments before any return, branch, panic, exit, `?`, or yield. A recursion that can leave another way, or that never reaches the call, is silent; a shadowing or renamed variable, or a parameter reassigned on the path, defeats the match because the arguments then carry other values.
- Exemptions: none beyond the fail-safe paths above; the rule fails safe on any path it cannot prove.
- Enforcement: `Check` (`checkInfiniteRecursion`), syntax plus checker objects.
- Fixtures: `test/logos/lint/infinite_recursion.lgo`, `infinite_recursion_go.go`, and `infinite_recursion_const_switch.lgo` (const-switch shape).
- Config keys and thresholds: none.

### correctness::typed_nil_return

Source: `src/cmd/compile/internal/logos/lint/rule_typed_nil_return.go:12-25`.

- Severity: Forbid (group default) in .lgo; effectively Deny in Go, where only the `(*T)(nil)` form is active (Go has no options or platform pointers) and the Go cap lowers forbid to deny.
- Scope: Go and .lgo (`Go: true`).
- Checks: a `return` statement handing a possibly nil `(*T)(nil)` conversion, an un-narrowed option, or a platform pointer to an interface or error result without a dominating nil check. An interface that holds a nil pointer is not nil, so the caller's `err != nil` test passes while `err` is nil inside.
- Exemptions: none listed; the return value must reach the interface or error result directly.
- Enforcement: `Check` (`checkTypedNilReturn`) with the nil-flow analysis in `nilflow.go`.
- Fixture: `test/logos/lint/typed_nil_return.lgo` plus `typed_nil_return_go.go`.
- Config keys and thresholds: none.

### correctness::unclosed_io_closer

Source: `src/cmd/compile/internal/logos/lint/rule_unclosed_io_closer.go:12-28`.

- Severity: **Default: Deny** (SPEC carve-out from the group default Forbid: a deny rule placed in the forbid group so ordinary code with a dropped `Close` does not fail every build).
- Scope: Go and .lgo (`Go: true`).
- Checks: a local variable that holds an `io.Closer` returned by a call, in a function that neither defers its `Close` nor hands the value to another owner. A variable is held when an assignment or declaration inside the function sets it from a call, directly or through a loGo/s `?`, and its type has a `Close() error` method. One finding per variable, at its first assignment from a call.
- Exemptions: parameters, named results, and variables of another function are not held; a conversion and a call of a builtin are not calls that return an owner. The function counts as closing the variable when a `defer` statement, or a loGo/s scope defer, calls its `Close`, either directly (with or without `?`) or inside a deferred function literal. An errdefer does not close it: it runs only on failure. The function counts as handing the variable on when the variable appears anywhere except as the receiver of a method call, a field selection, an operand of `==` or `!=`, or the target of an assignment: returned, stored, sent, passed as an argument, aliased, taken as a method value, or captured by a function literal.
- Enforcement: `Check` (`checkUnclosedIOCloser`).
- Fixture: `test/logos/lint/unclosed_io_closer.lgo` plus `unclosed_io_closer_go.go`.
- Config keys and thresholds: none.

### correctness::unbounded_channel_op

Source: `src/cmd/compile/internal/logos/lint/rule_unbounded_channel_op.go:7-27`.

- Severity: **Default: Deny** (SPEC carve-out, same reason as above).
- Scope: Go and .lgo (`Go: true`).
- Checks: a channel send or receive that has no cancellation path. An operation is bounded in two cases only. First, it is a case of a select statement that has a `<-x.Done()` case, where `x` is any expression and the call returns a channel; a receive of `x.Done()` on its own is the wait for cancellation itself. Second, its channel is a local variable made once, in the function, by `make(chan T, k)` with a constant capacity `k`, and the function sends on the channel at most `k` times in total, never inside a loop. For the second case the function is the declaration or top-level literal together with every literal nested in it; a literal inside a loop counts as inside it, and a send in a select case counts. The two conditions are properties of the channel: when the sends exceed `k`, or any send sits in a loop, every operation on that channel is reported, including the receives. A receive is bounded by the same conditions as the sends. In loGo/s, a plain channel operation directly inside a `go group` child block is bounded too, because lowering turns it into a select on the group context's `Done` channel.
- Exemptions: the statement bodies of a done select are ordinary code; a range over a channel, `close`, and `len` are not operations of this rule. A select with a `default` case but no done case is not exempt. A function literal inside a `go group` child block is not covered by the group-context bounding.
- Enforcement: `Check` (`checkUnboundedChannelOp`), reading syntax plus checker objects; the channel-capacity leg resolves constants through `go/constant`.
- Fixture: `test/logos/lint/unbounded_channel_op.lgo` plus `unbounded_channel_op_go.go`.
- Config keys and thresholds: none.

### correctness::nil_deref_unproven

Source: `src/cmd/compile/internal/logos/lint/rule_nil_deref_unproven.go:11-22`; registered in `src/cmd/compile/internal/logos/lint/nilderef.go` and `registry.go`; enforcement in `src/cmd/compile/internal/types2/option_logos.go` and `narrow_logos.go`.

- Severity: Forbid.
- Scope: .lgo only (`Go: false`). Go has no option types (`T?`), so the rule never applies to Go packages.
- Checks: field or method selection on an option (`x.f`, `x.m()` with `x` of type `T?`) before a nil test proves presence.
- Exemptions: `//logos:allow` and a directive that names the rule have the standard contract; the finding stops the compile before `Run`, so the allow is applied by the resolver, not the rule.
- Enforcement: **types2 handover**. The checker enforces it under this ID while checking; the rule registers with a nil `Check`, the finding is reported once, and it carries the group and scope so severity resolution, the `allow_forbidden_rule` check, and `TestRuleScopes` cover it. The compiler routes the checker's error through `NilDerefResult` (`nilderef.go`), which adds the JSON autofix candidates of SPEC Sec 1.2: optional chaining with `?` default and an early-return guard.
- Fixture: `test/logos/lint/nil_deref_unproven.lgo` (carries `// errorcheck -gcflags=-e`, not plain `errorcheck`) and `nil_deref_unproven_allow.lgo` (the silenced twin).
- Config keys and thresholds: none.

### correctness::defer_erases_errdefer_state

Source: `src/cmd/compile/internal/logos/lint/rule_defer_erases_errdefer_state.go:12-25`.

- Severity: Forbid (group default).
- Scope: .lgo only (`Go: false`). Plain Go has no `errdefer` construct.
- Checks: in a function with `errdefer`, a plain deferred function literal assigning nil to the error result. The deferred literal runs before the errdefer cleanup, clears the result, and the errdefer sees a success and skips its rollback. The rule reads the function that owns the defer: an errdefer statement and a deferred literal count for the function that contains them directly, never for an enclosing or nested function literal, and the assigned variable must be the last, named result of that function, matched by object so a shadowing variable does not count. The order of the defer and the errdefer does not matter.
- Exemptions: a plain deferred literal is `defer func() {...}()`; `defer f()?`, scope defer, and a deferred call of a named function are not plain literals and are not examined. An assignment inside a function literal nested in the deferred literal is not searched, because that literal may never run.
- Enforcement: `Check` (`checkDeferErasesErrdeferState`); forbid, and applies to loGo/s packages only.
- Fixture: `test/logos/lint/defer_erases_errdefer_state.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### concurrency::concurrent_multiword_race

Source: `src/cmd/compile/internal/logos/lint/rule_concurrent_multiword_race.go:12-32`.

- Severity: Forbid (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: a local variable of multi-word type that a child captures and either side writes after the spawn. A child is the function literal of a `go` statement or a loGo/s group child block. Multi-word: a string (two words), a slice (three), an interface or union (two), a complex number (two), and a struct or array whose value-bearing storage (padding and zero-sized fields excluded) spans more than one target machine word. A write is an assignment, an increment, or a range assignment to the variable itself or to a field or array element held in it. The spawn starts the write window, which ends with the innermost enclosing `go group` block (the group joins its children there) or otherwise with the enclosing declaration; a write by the child is inside the window.
- Exemptions: a value of a logos/sync type is exempt (synchronized by contract), and so is a const view, which nothing writes through; storing through a pointer, slice element, or map entry writes other storage and is not a write of the variable.
- Enforcement: `Check` (`checkConcurrentMultiwordRace`); syntax plus checker objects, so it holds for Go packages through the `go` statement leg and for loGo/s packages through both legs. The word-size question reads `buildcfg` target word size (`internal/buildcfg` import).
- Fixture: `test/logos/lint/concurrent_multiword_race.lgo` plus `concurrent_multiword_race_go.go`.
- Config keys and thresholds: none.

### concurrency::reentrant_withlock_deadlock

Source: `src/cmd/compile/internal/logos/lint/rule_reentrant_withlock_deadlock.go:16-35`.

- Severity: Forbid (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: inside the function literal or lambda passed to `WithLock` of a `logos/sync.Locked`, a call to `WithLock` on the same receiver expression, which blocks forever on the mutex the outer call holds. The inner call counts when it appears directly in the callback, in the body of a directly invoked or deferred function literal, or anywhere in the synchronous call graph of the same-package functions the callback calls, at any depth. Same-package functions compose receiver keys across call sites through the existing `lockKey` substitution, so mutually recursive helpers reach a fixed point with no call-depth cap. Two receiver expressions match when they name the same variable through the same chain of field selections, ignoring address-of and dereference.
- Exemptions: a `go` statement evaluates its receiver and arguments synchronously and only invokes its call asynchronously, so the invoked call and a literal that is its function are excluded while argument subexpressions stay in the flow. A stored function literal that is never invoked is skipped. A receiver that is any other expression never matches, so the rule fails safe.
- Enforcement: `Check` (`checkReentrantWithLockDeadlock`); syntax plus checker objects, so it holds for Go and loGo/s packages alike; a lambda exists in loGo/s only.
- Fixture: `test/logos/lint/reentrant_withlock_deadlock.lgo` plus `reentrant_withlock_deadlock_go.go`.
- Config keys and thresholds: none.

### concurrency::unsupervised_goroutine

Source: `src/cmd/compile/internal/logos/lint/rule_unsupervised_goroutine.go:11-23`; enforcement in `src/cmd/compile/internal/types2/group_logos.go` (M8).

- Severity: Forbid.
- Scope: .lgo only (`Go: false`). Plain Go packages keep the standard bare `go` statement, so this rule never applies to Go packages.
- Checks: a `go` statement in a loGo/s package outside a `go group` block. In loGo/s, concurrency must be structured: goroutines must belong to an enclosing `go group` that manages cancellation, panic handling, and termination joins.
- Exemptions: none listed. A `go` statement inside a `go group` block is the supervised form.
- Enforcement: **types2 handover**. The checker already reports it under this ID while it checks the package (M8, `types2/group_logos.go`), before `Run` is called. The entry has no `Check`, so the finding is reported once, and it carries the group and scope so it resolves as a forbid rule and keeps ignoring `//logos:allow`; a directive that names it is reported as `correctness::allow_forbidden_rule`.
- Fixture: `test/logos/lint/unsupervised_goroutine.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### slop::discarded_error

Source: `src/cmd/compile/internal/logos/lint/rule_discarded_error.go:12-27`.

- Severity: Deny (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: an expression statement or a blank assignment (`_ =`) that drops an error result.
- Exemptions, with reasons: callees whose package facts carry `AlwaysNilError` (the callee is declared to never return an error; no producer of the fact exists yet, so the exemption applies only to facts the importer has recorded), `fmt.Print*` and `fmt.Fprint*` to `os.Stdout` or `os.Stderr` (printing is the primary effect; their error results are conventionally ignored in CLI programs), and the `Write*` methods of `*bytes.Buffer` and `*strings.Builder` (writes to in-memory buffers cannot fail in practice).
- Enforcement: `Check` (`checkDiscardedError`); a `discardCheck` walks one file and reports dropped error results, per `returnsError`.
- Fixture: `test/logos/lint/discarded_error.lgo` plus `discarded_error_go.go`.
- Config keys and thresholds: none.

### slop::err_chain_reset

Source: `src/cmd/compile/internal/logos/lint/rule_err_chain_reset.go:12-25`.

- Severity: Deny (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: a `fmt.Errorf` call that formats an operand of error type with a verb other than `%w`, or that passes `err.Error()` or a string concatenated from it, so `errors.Is` and `errors.As` cannot see through the new error. The verbs come from the constant format string. A call whose arguments are one format string with the operand each one formats found the way package `fmt` finds it: a `*` width or precision and an explicit `!h` index move the operand, and `%w` takes none.
- Exemptions: calls to functions of the form `Format(s fmt.State, verb rune) bool` (the `fmt.Formatter` interface method) are skipped, since that call does not mean a broken chain. A call whose arguments are not constant format has no known errors per operand, so it is not flagged.
- Enforcement: `Check` (`checkErrChainReset`). It offers a fix that replaces the verb of every offending operand with `%w` when the format is a string literal and each offender has such a verb.
- Fixture: `test/logos/lint/err_chain_reset.lgo` plus `err_chain_reset_go.go`.
- Config keys and thresholds: none.

### slop::todo_placeholder

Source: `src/cmd/compile/internal/logos/lint/rule_todo_placeholder.go:12-27`.

- Severity: Deny (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: unfinished work left in a non-test file, in two forms. A comment whose text, after the comment marker and any white space, starts with the word `TODO` or `FIXME`; a word ends at a character that cannot be part of an identifier, so `TODOS` and `FIXMEs` are prose. A call of the predeclared `panic` whose argument is a constant string holding `"unimplemented"` or `"not implemented"` in any letter case, or `"TODO"` as written.
- Exemptions: a file whose name ends in `_test` is skipped entirely (test code is allowed to carry work notes).
- Enforcement: `Check` (`checkTodoPlaceholder`); comments come from the scanner via `syntax.CommentsDoMode`, the form of `syntax.CommentsDo` that reads a loGo/s file with its own token rules, so a `//` inside an interpolated string is never a comment.
- Fixture: `test/logos/lint/todo_placeholder.lgo` plus `todo_placeholder_go.go`.
- Config keys and thresholds: none.

### complexity::cognitive_overflow

Source: `src/cmd/compile/internal/logos/lint/rule_cognitive_overflow.go:12-26`.

- Severity: Deny (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: a function that exceeds one of four limits of the `logos.toml [lints.thresholds]` table: `max_nesting_depth`, `max_function_params`, `max_function_lines`, or `max_cognitive_complexity`. A function declaration, a function literal, and a loGo/s lambda are each a function, measured on their own: a nested function body adds nothing to the function around it, though its lines belong to that function's line count. A function with more than one exceeded limit gets one finding that names them all.
- Scoring, as stated in the rule header: the nesting structures are `if`, `for`, `switch`, and `select`, with the loGo/s `const if`, `const switch`, and switch expression. The nesting depth is the deepest level of nested structures, counting the deepest structure itself, so a `for` inside an `if` inside a `for` has depth 3. The cognitive score adds 1 plus the number of structures around the point for each of: a structure, a case with a condition (not a default clause), a goto, and a labeled break or continue; a case sits at the level of its switch, as gofmt writes it. An `else if` continues its `if` at the same level and adds the same as an `if` there. Each change between `&&` and `||` in an unparenthesized chain adds 1, so `a && b && c` adds nothing and `a && b || c` adds 1. The line count runs from the `func` line through the closing brace, blank and comment lines included. The parameter count is the number of declared parameters, not the receiver.
- Exemptions: none in the rule header.
- Enforcement: `Check` (`checkCognitiveOverflow`), measured against the policy thresholds.
- Fixture: `test/logos/lint/cognitive_overflow.lgo` plus `cognitive_overflow_go.go`.
- Config keys and thresholds: `max_nesting_depth` (3), `max_cognitive_complexity` (15), `max_function_lines` (60), `max_function_params` (4); all four must be positive.

### perf::large_backing_array_pin

Source: `src/cmd/compile/internal/logos/lint/rule_large_backing_array_pin.go:11-23`.

- Severity: Warn (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: a subslice or substring of a backing array larger than 64 KB that escapes, so the whole array stays reachable for as long as the small part does. `PinFinding` carries where the slice expression starts, how many bytes the backing store has, and whether the sliced value is a string.
- Exemptions: none in the rule header.
- Enforcement: **escape-phase**. The compiler decides the escape inside escape analysis, a phase after `Run`: the rule has no `Check`, like a rule an earlier phase enforces, and `RunPins` resolves the findings the escape phase hands over. `PinActive` reports whether the escape phase must track large backing stores for a package (the rule is on at the severity of the package and applies to it), and `RunPins` resolves each escaping subslice once, ignoring inlined bodies and generic instantiations analyzed more than once, and ignores a finding in a file the package does not hold.
- Fixture: `test/logos/lint/large_backing_array_pin.lgo` plus `large_backing_array_pin_go.go`.
- Config keys and thresholds: the 64 KB threshold is fixed in the rule (not a `logos.toml` threshold).

### perf::time_after_in_loop

Source: `src/cmd/compile/internal/logos/lint/rule_time_after_in_loop.go:12-22`.

- Severity: Warn (group default).
- Scope: Go and .lgo (`Go: true`).
- Checks: a call to `time.After` inside a loop body. Each pass through the loop repeatedly constructs a fresh timer, creating scheduler queue churn and heap allocation overhead on active execution paths. In modern Go (1.23+), unreferenced timers are reclaimed by the garbage collector (governed by module go version and `asynctimerchan` settings), so the check targets allocation and timer churn rather than permanent memory retention.
- Exemptions: a `time.After` call in the header of a `for` statement (the `init`, condition, or post statement) and in a range operand run outside the body. The rule is language-neutral: it reads Go syntax and loGo/s syntax only.
- Enforcement: `Check` (`checkTimeAfterInLoop`); `timeAfterWalk` walks one file, depth counting the loop bodies a statement sits under from the nodes `timeAfterWalk` visits; a `for` statement handled by visitor checks whether a `time.After` call is one of its body statements (`rule_time_after_in_loop.go` `timeAfterWalk`).
- Fixture: `test/logos/lint/time_after_in_loop.lgo` (carries `// errorcheck -0`, the warn-only convention) plus `time_after_in_loop_go.go`.
- Config keys and thresholds: none.

### suspicious::unapplied_struct_default

Source: `src/cmd/compile/internal/logos/lint/rule_unapplied_struct_default.go:11-19`; enforcement in `src/cmd/compile/internal/types2/structdefault_logos.go` (M6-S3).

- Severity: Warn (group default).
- Scope: .lgo only (`Go: false`). Plain Go has no struct field default values.
- Checks: `var x T` or `new(T)` for a struct type with field defaults, which leaves every default unapplied.
- Exemptions: `//logos:allow` applies like any other finding; the rule is a soft warning.
- Enforcement: **types2 handover**. The types2 checker finds it while it checks the package (`types2/structdefault_logos.go`, M6-S3), before `Run` is called, and hands the finding to `Run` in `Input.Reported`, which applies the severity to the suspicious group and `//logos:allow` like any other finding. The entry has no `Check`, so the finding is reported once, and it carries the group and scope. The rule never applies to Go packages, which have no field defaults.
- Fixture: `test/logos/lint/unapplied_struct_default.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### suspicious::side_effect_before_sort

Source: `src/cmd/compile/internal/logos/lint/rule_side_effect_before_sort.go:11-19`; enforcement in `src/cmd/compile/internal/types2/chains_logos.go:594-597` (`logosWarnSideEffectBeforeSort`).

- Severity: Warn (group default).
- Scope: .lgo only (`Go: false`). Plain Go has no chain pipeline syntax (`.sortBy()`).
- Checks: a chain lambda with a call of unknown purity before `.sortBy()`: the fused prefix may run its effects an unexpected number of times once sorting materializes the run. Only the lambdas of the chain prefix (the sortBy receiver, transitively) are scanned.
- Exemptions, with reasons: the sortBy key lambda itself and the receiver base expression are out of scope (only prefix lambdas are scanned). Known-pure calls are exempt: a conversion (the callee names a type) and an effect-free builtin. Copy, clear, delete, and append mutate their arguments; panic, recover, print, and println have observable effects; all of those count as unknown. Every other call, including method and package-qualified calls, counts as unknown. Nested function literals are pruned: their calls run only when the literal is invoked, not per element.
- Enforcement: **types2 handover**. The types2 checker finds it while it checks the package (`types2/chains_logos.go:589-601`, `logosWarnSideEffectBeforeSort`), reporting through `softErrorf`, and `LogosWarnOnly` keeps it out of `firstErr`; the compiler hands the finding to `Run` in `Input.Reported`, which applies the severity of the suspicious group and `//logos:allow` like any other finding. The entry has no `Check`, so the finding is reported once, and it carries the group and scope. The rule never applies to Go packages, which have no chain lambdas.
- Fixture: `test/logos/lint/side_effect_before_sort.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### portability::serialized_unsized_int

Source: `src/cmd/compile/internal/logos/lint/rule_serialized_unsized_int.go:12-27`.

- Severity: Warn (group default).
- Scope: .lgo only (`Go: false`).
- Checks: a field of type `int` or `uint` in a struct that has a field with a `json` tag. The generated binary layout (M11-S3) writes every integer as 8 bytes and ignores tag options, so a field is flagged whatever its own tag says and whether or not it is exported, but `int` and `uint` are 4 bytes on a 32-bit target: the same struct serializes differently per target width.
- Exemptions: a defined type over `int`, a pointer, and a slice are not flagged: they are other types.
- Enforcement: `Check` (`checkSerializedUnsizedInt`).
- Fixture: `test/logos/lint/serialized_unsized_int.lgo`; no `_go.go` twin.
- Config keys and thresholds: none.

### portability::alias_extension

Source: `src/cmd/compile/internal/logos/lint/rule_alias_extension.go:12-19`; raised by the go command in `src/cmd/go/internal/load/logos_alias.go`.

- Severity: Warn (group default).
- Scope: .lgo alias-extension sources only (`Go: false`); never applies to Go packages.
- Checks: a source file with a loGo/s alias extension in a main module whose path starts with a dotted element, where the module could be published, so module zip rules reject the file name. The go command reports this under this ID while it loads the package, before the compiler runs; the compiler registers the severity, portability group in `logos.toml`, and `//logos:allow` directives directly above the package clause of the file, because the compiler sees only the source text, not the file extension rules of the module, and it carries the group and scope. The rule never applies to Go packages.
- Exemptions: a `//logos:allow(portability::alias_extension)` in the comment block directly above the package clause silences it at warn and deny; the directive sits there because that is the only line the compiler sees before the package clause.
- Enforcement: **go command**. The entry has no `Check`; the finding is reported once, and it carries the group and scope.
- Fixture: `test/logos/lint/alias_extension.lgo` (carries `// errorcheck -0`, since warnings do not fail `logos lint`).
- Config keys and thresholds: none.

### pedantic::exhaustive_union_switch

Source: `src/cmd/compile/internal/logos/lint/rule_exhaustive_union_switch.go:12-20`.

- Severity: Allow (group default; reports only under `--pedantic` or a `pedantic` key).
- Scope: .lgo only (`Go: false`).
- Checks: a `default` clause in a type switch over a closed union, where the cases are exhaustive over the union value.
- Exemptions: none listed in the rule header.
- Enforcement: `Check` (`checkExhaustiveUnionSwitch`).
- Fixture: `test/logos/lint/exhaustive_union_switch.lgo`; no `_go.go` twin.
- Config keys and thresholds: none; a `pedantic` group key of `warn` in `logos.toml` turns it on, as does `--pedantic`.

### perf::zeroalloc_violation (special, not a registered Rule)

Source: `src/cmd/compile/internal/logos/lint/zeroalloc.go:13-23`; escape engine in `src/cmd/compile/internal/escape/zeroalloc_logos.go:183-205`.

- Severity: every result is at Forbid (`ZeroAllocResults`).
- Scope: applies wherever the directive is parsed and escape analysis runs; the Go-package leg is pinned by `test/logos/lint/zeroalloc_go.go` (the Go twin of `test/logos/codegen/zeroalloc.lgo`, applied by `logos lint` in Go) and by the `logos_zeroalloc.txt` script test. The lint hook sets `LogosZeroAlloc` only when the package carries marked functions, so a compile with no `//logos:zeroalloc` directive keeps the flow graph and `-m` output of plain Go (`escape/zeroalloc_logos.go:15-27`).
- Checks: `//logos:zeroalloc` on the line directly above a function declaration is a static allocation-site assertion enforced directly by the escape analysis solver (`zeroalloc.go`, `zeroalloc_logos.go`). It asserts that the function (plus inlined code and its literals) contains no static allocation sites that escape analysis decides must live on the heap. Per `SPEC.md:2044`, this directive is an assertion about compiler-determined static allocation sites, not a proof that the function never calls the allocator at runtime.
- Exemptions, with reasons: decided by escape analysis in `logosAllocates` (`src/cmd/compile/internal/escape/zeroalloc_logos.go:183-205`): a location the solver moved to the heap counts as a finding only when it costs a heap allocation at run time.
  - Constant-to-interface conversions are exempt: a conversion of a constant, or of a value that needs no storage, is static data (`logosStaticIface`).
  - Non-capturing closures are exempt: a function literal that captures nothing (`ClosureVars` empty) is static data and needs no allocation.
  - Zero-size objects are exempt: a zero-size object has no address of its own, so `&T{}`-shaped locations of size zero do not allocate.
  - Runtime allocation limitations: compile-time escape analysis inspects static flow graphs. Per `SPEC.md:2044`, the directive is an assertion about allocation sites that escape analysis decides, not a runtime proof that the function never calls the allocator. Dynamic heap growth at runtime (such as slice reallocation upon `append` past capacity, map bucket expansion, or string conversions exceeding stack limits) cannot be tracked statically without compiler-directed heap escapes (`zeroalloc_logos.go:24-27`). Exact allocation invariants require explicit annotations on declarations, not global bans.
  - `make(chan T)` is the inverse case: it is always a finding, because a channel always lives on the heap and no flow edge models it (`logosMakeChan`).
- Unconfigurable and unsilenceable: the directive is the author's own static assertion, so no `logos.toml` severity lowers the diagnostic and no `//logos:allow` silences it. `perf::zeroalloc_violation` accepts no severity key and is not one of the nine `[lints]` group keys. Misplaced directives are errors.
- Enforcement: **escape-phase**. `ZeroAllocFuncs` scans directives and returns misplaced-directive results; the escape phase finds the allocation sites (`LogosLintEscape`, `gc/logos_lint.go:12-27`), and `ZeroAllocResults` resolves them to Forbid results.
- Fixture: `test/logos/lint/zeroalloc_go.go` (carries `// errorcheck`); the .lgo origin is `test/logos/codegen/zeroalloc.lgo`.
- Config keys and thresholds: none.
## Proposed rules (target design)

Status: design specification. None of the entries below are registered in the compiler today (`registry.go` holds exactly the 21 entries above). They define the complete target contracts for dedicated offline lint execution in both Go and loGo/s (`logos lint`). In accordance with the project lint design, lint rules do not run during standard compilation (`logos build`), and adoption is not constrained by compiler performance budgets.

Severity calibration: reserve `forbid` for violations established under explicit analysis assumptions. Unknown facts do not prove a violation or a clean result. Heuristic checks and legal systems idioms default to `warn` or `deny`. These proposed severities affect `logos lint`, not ordinary builds.

### Initial proposed wave (12 rules)

#### correctness::printf_misuse

- ID: `correctness::printf_misuse`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (high certainty). Format verbs and argument types are statically checkable against format string literals.
- Detects: calls to formatted I/O functions (`fmt.Printf`, `fmt.Sprintf`, `fmt.Fprintf`, `log.Printf`, and loGo/s `$"..."` string interpolations) where the format string is a constant and the number or types of operands do not match the format verbs.
- Exempt / negative cases: calls where the format specifier is not a string literal or constant; custom wrapper functions not registered in format function tables; `%v`, `%+v`, `%#v`, and `%T` verbs which accept any operand type.
- Positive example:
  ```go
  fmt.Printf("user %s count %d\n", username) // Error: format specifies 2 args, 1 provided
  ```
- Negative example:
  ```go
  fmt.Printf("user %s count %d\n", username, count)
  ```
- Analysis requirements: AST syntax inspection and types2 type information for argument types.
- Diagnostic evidence: reported span covers the call expression, citing format verb index, expected argument type, and provided argument type.
- Fix safety: manual inspection required. Adding missing arguments or adjusting format specifiers changes runtime output.
- Acceptance cases: flags too few arguments, too many arguments, operand type mismatch (e.g. `%d` on string), and malformed index tags.

#### correctness::unreachable_code

- ID: `correctness::unreachable_code`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (high certainty). Statements following unconditional control flow exits cannot execute.
- Detects: statements immediately following an unconditional `return`, `panic`, terminal control flow exit, or an infinite loop without breaks. In loGo/s, `?` error propagation unwinds on error, but its normal success path continues execution and is not unreachable.
- Exempt / negative cases: statements guarded by labels targeted by `goto`; code following a non-terminal `select` or conditional `if`; empty statements.
- Positive example:
  ```go
  func compute() int {
      return 42
      println("done") // Error: unreachable statement
  }
  ```
- Negative example:
  ```go
  func compute(cond bool) int {
      if cond {
          return 42
      }
      println("fallback")
      return 0
  }
  ```
- Analysis requirements: control flow graph construction over AST statements.
- Diagnostic evidence: reported span covers the first unreachable statement, citing the terminating predecessor node.
- Fix safety: manual removal is required by default. Automated deletion of dead statements is NOT safe merely because no referenced variable declarations exist: removing dead code can drop documentation comments, break jump labels targeted by external branches, discard compiler directives or build tags (e.g. `//go:noescape`, `//logos:allow`), or obscure developer intent. Automated removal requires an end-to-end AST-edit proof verifying that no labels, directives, or critical comments are stripped.
- Acceptance cases: flags code after return, code after panic, and code after infinite loops lacking break; does not flag normal success paths following loGo/s `?`.

#### correctness::lostcancel

- ID: `correctness::lostcancel`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (high certainty). Context cancellation functions must be invoked to avoid timer and goroutine resource leaks.
- Detects: calls to `context.WithCancel`, `context.WithTimeout`, `context.WithDeadline`, or `context.WithCancelCause` where the returned `cancel` function is discarded (`_`), shadowed, or not invoked on all execution paths exiting the function.
- Exempt / negative cases: functions that return the `cancel` function to their caller; cancel functions stored into structs or sent across channels ONLY where an explicit modeled lifecycle contract exists. Storing or sending a cancel function without a modeled contract is NOT an unconditional safe exemption; an untracked or unknown ownership transfer must be reported as an incomplete proof rather than silently assumed safe.
- Positive example:
  ```go
  func query(ctx context.Context) error {
      ctx, _ = context.WithTimeout(ctx, time.Second) // Error: cancel function discarded
      return doRPC(ctx)
  }
  ```
- Negative example:
  ```go
  func query(ctx context.Context) error {
      ctx, cancel := context.WithTimeout(ctx, time.Second)
      defer cancel()
      return doRPC(ctx)
  }
  ```
- Analysis requirements: AST and types2 control flow tracing of cancel function identifier.
- Diagnostic evidence: reported span highlights the assignment or declaration where the cancel function is dropped.
- Fix safety: manual insertion of `defer cancel()` or passing cancel to caller.
- Acceptance cases: flags blank assignment `ctx, _ = ...`, unused cancel variable, missing call before return, and unmodeled channel/struct escape without lifecycle proof.

#### suspicious::copy_lock

- ID: `suspicious::copy_lock`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Replaces draft ID `correctness::copy_lock`. Moved from `correctness` to `suspicious` because heuristic checks cannot default to `warn` in an unlowerable `forbid` group where group defaults or configuration would elevate them to `forbid`. Copying an active lock duplicates synchronization state, causing deadlocks or unsynchronized access. However, copying an uninitialized or zero-value lock during initialization before concurrent use is legal.
- Detects: assignment, pass-by-value argument passing, return-by-value, or receiver copying of a struct embedding or holding `sync.Mutex`, `sync.RWMutex`, or types implementing `sync.Locker`.
- Exempt / negative cases: methods using pointer receivers (`func (s *Service) Method()`); passing pointers to lock-bearing structs (`*Service`); zero-value struct initialization literals before concurrent publication; test fixtures initializing isolated instances. Notice that embedded value mutexes with pointer receivers (`type S struct { mu sync.Mutex }` with `func (s *S) Lock()`) are idiomatic and exempt when not passed by value.
- Positive example:
  ```go
  type State struct {
      mu sync.Mutex
      count int
  }
  func snapshot(s State) int { // Warning: State embeds sync.Mutex and is passed by value
      s.mu.Lock()
      defer s.mu.Unlock()
      return s.count
  }
  ```
- Negative example:
  ```go
  func snapshot(s *State) int { // Compliant: pointer avoids copying lock
      s.mu.Lock()
      defer s.mu.Unlock()
      return s.count
  }
  ```
- Analysis requirements: types2 inspection of struct field types, checking for recursive embedding of `sync.Locker`.
- Diagnostic evidence: reported span cites the copied parameter, variable, or return value, naming the embedded lock field.
- Fix safety: change pass-by-value to pointer pass. Requires updating callsites.
- Acceptance cases: flags value receiver methods on structs with locks, value parameters, and slice copies of lock values.

#### suspicious::boolean_contradiction

- ID: `suspicious::boolean_contradiction`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Replaces draft IDs `correctness::boolean_contradiction` and former `bools_simplify`. Moved from `correctness` to `suspicious` to ensure the rule defaults to `warn` without being forced to `forbid` by the unlowerable `correctness` group. Evaluates boolean expressions that are tautological or contradictory. Defaults to warn because defensive coding and constant-specialized configurations may introduce deliberate invariant checks.
- Detects: boolean operations where an expression is logically contradictory or tautological across binary operators (`x && !x`, `x || !x`, `x == a && x == b` where `a != b`). Repeated identical operands (`x && x`) represent duplication or redundancy rather than a contradiction and are governed separately.
- Exempt / negative cases: compile-time constants conditioned on build tags; loGo/s `const if` branches; generated code where macros instantiate trivial conditions; defensive assertions before unsafe operations. Notice that in loGo/s, `?` error propagation success paths continue normal evaluation and are not contradictory.
- Positive example:
  ```go
  if count > 0 && count < 0 { // Warning: boolean contradiction: count cannot be both > 0 and < 0
      process()
  }
  ```
- Negative example:
  ```go
  if count > 0 && count < limit {
      process()
  }
  ```
- Analysis requirements: AST binary expression tree inspection with basic interval and equality analysis. The analyzer must evaluate side effects: expressions like `a && false` or `a || true` cannot be safely deleted or folded if evaluating `a` incurs side effects, volatile or concurrent memory reads, or function calls. Compile-time specialization (e.g. `const if` branches or build-tag constants) and loGo/s `?` operator success paths must not be flagged as contradictory.
- Diagnostic evidence: reported span covers the contradictory binary operation, naming the contradictory operands.
- Fix safety: manual inspection required. Expression must be rewritten to match developer intent. Automated simplification without side-effect analysis is unsafe.
- Acceptance cases: flags `a && false` (where `a` is side-effect-free), `a || true`, `x > 5 && x < 3`, and flags contradictory intervals; accepts pure redundancy `x && x` (handled by redundancy checks), accepts side-effecting operands, and accepts compile-time constant branches.

#### suspicious::string_intconv

- ID: `suspicious::string_intconv`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Replaces draft ID `correctness::string_intconv`. Moved from `correctness` to `suspicious` to ensure the rule defaults to `warn` without being forced to `forbid` by the unlowerable `correctness` group. In Go and loGo/s, `string(v)` where `v` is an integer converts a numeric Unicode code point into a UTF-8 character string. Developers frequently write `string(port)` expecting decimal formatting (e.g. `"8080"`), whereas it produces a single Unicode character. The conversion is legal language syntax, so severity defaults to warn.
- Detects: conversions of the form `string(x)` where `x` has an untyped or concrete integer type other than `rune` or `byte`.
- Exempt / negative cases: explicit rune conversions `string(rune(x))`; conversions of `byte` or `rune` variables; explicit `strconv.Itoa` or `strconv.FormatInt`. Note: the former proposal mentioning `int(string)` was invalid and is dropped because `int(string)` is a compiler type error, not a legal conversion.
- Positive example:
  ```go
  port := 8080
  addr := "localhost:" + string(port) // Warning: string(int) converts integer to rune, not decimal representation
  ```
- Negative example:
  ```go
  port := 8080
  addr := "localhost:" + strconv.Itoa(port) // Compliant: explicit decimal conversion
  r := 'A'
  charStr := string(r) // Compliant: operand is explicitly typed as rune
  ```
- Analysis requirements: types2 type determination of conversion operand expression.
- Diagnostic evidence: reported span cites the conversion expression and the integer operand type.
- Fix safety: safe automated replacement with `strconv.Itoa(...)` when operand is `int`, or `strconv.FormatInt(int64(...), 10)`.
- Acceptance cases: flags `string(intVar)` and `string(1234)`, accepts `string(runeVar)` and `strconv.Itoa`.

#### correctness::structtag_malformed

- ID: `correctness::structtag_malformed`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (high certainty). Struct tags follow a strict key-value format defined by `reflect.StructTag`. Malformed tags silently disable serialization or ORM mappings.
- Detects: struct field tag literals that do not follow conventional `key:"value"` pairs, have unquoted values, invalid escape sequences, or duplicate keys.
- Exempt / negative cases: fields without tags; raw string literals correctly formatted with space delimiters.
- Positive example:
  ```go
  type User struct {
      Name string `json:name,omitempty` // Error: missing quotes around tag value
  }
  ```
- Negative example:
  ```go
  type User struct {
      Name string `json:"name,omitempty"`
  }
  ```
- Analysis requirements: AST struct field tag inspection with tag lexer.
- Diagnostic evidence: reported span highlights the malformed tag literal, citing the parsing syntax error.
- Fix safety: semi-automated quote wrapping if tag structure is unambiguous.
- Acceptance cases: flags missing colon, missing quotes, unescaped quotes within value, and invalid key identifiers.

#### correctness::stdmethods_shape

- ID: `correctness::stdmethods_shape`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (high certainty). Methods matching standard library interface names with wrong signatures silently fail interface satisfaction at runtime.
- Detects: methods named `Read`, `Write`, `Close`, `Seek`, `Flush`, `Format`, `MarshalJSON`, `UnmarshalJSON`, `MarshalBinary`, or `UnmarshalBinary` whose parameter or result signatures do not match the standard interfaces, ONLY where recognized interface satisfaction, package export intended for interface use, or type annotations prove an obligation to implement the standard interface. The rule does not ban ordinary, domain-specific `Read` or `Write` methods with differing signatures on unexported or unrelated internal types where no standard library interface obligation exists (aligning with `go vet stdmethods` constraints, which does not flag `Read` arbitrarily).
- Exempt / negative cases: unexported methods; domain methods on types with no demonstrated intent or obligation to satisfy standard library interfaces; methods in test packages explicitly testing non-standard APIs; functions not bound to receivers.
- Positive example:
  ```go
  type Buffer struct{}
  func (b *Buffer) Read(p []byte) int { // Error: Read method intended for io.Reader must return (int, error)
      return len(p)
  }
  ```
- Negative example:
  ```go
  func (b *Buffer) Read(p []byte) (int, error) { // Compliant: matches io.Reader
      return len(p), nil
  }
  ```
- Analysis requirements: AST method declaration check against canonical interface method signatures with interface obligation analysis.
- Diagnostic evidence: reported span cites the method signature, highlighting expected vs declared parameter/result list.
- Fix safety: manual inspection required. Changing method signatures impacts all callsites.
- Acceptance cases: flags `Read([]byte) int` on interface-implementing type, `Write([]byte) error`, `Close()`, and `MarshalJSON() []byte`; accepts domain-specific `Read` on unexported helper without interface obligation.

#### correctness::tests_malformed

- ID: `correctness::tests_malformed`
- Applicability: Go and loGo/s (`Go: true`). Governs standard Go test files (`_test.go`) and loGo/s test files (`_test.lgo`), never general production `*_go.go` twin files.
- Severity recommendation and certainty: Forbid (high certainty). Test runners ignore improperly named or typed test functions, causing silent omission of tests.
- Detects: functions in `_test.go` and `_test.lgo` files with names beginning with `Test`, `Benchmark`, `Fuzz`, or `Example` whose signatures deviate from standard `testing` package conventions.
- Exempt / negative cases: non-test files; production `_go.go` twin files; helper functions not beginning with test prefixes; functions with unexported helper names (`testHelper`).
- Positive example:
  ```go
  func TestCalculate(t testing.T) { // Error: TestCalculate must accept *testing.T, not value testing.T
  }
  ```
- Negative example:
  ```go
  func TestCalculate(t *testing.T) { // Compliant: pointer to testing.T
  }
  ```
- Analysis requirements: AST function signature check in files with test naming suffixes (`_test.go` and `_test.lgo`).
- Diagnostic evidence: reported span covers the test function name and parameter list.
- Fix safety: automated fix to insert pointer indirection or expected signature.
- Acceptance cases: flags `TestX(t testing.T)`, `TestX()`, `BenchmarkX(b testing.B)`, and tests returning values in `_test.go` and `_test.lgo`; ignores production `*_go.go` files.

#### portability::linkname_discipline

- ID: `portability::linkname_discipline`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Deny (contract-driven). Replaces directory-wide blanket exemptions with strict symbol-level signature verification against known target declarations (`checklinkname` contract).
- Detects: `//go:linkname` directives that target unvetted runtime symbols, target symbols with mismatched local versus remote signatures, or lack two-argument explicit target declarations.
- Exempt / negative cases: vetted standard library runtime symbols registered in the known-linkname table whose local signature matches the remote package declaration.
- Positive example:
  ```go
  //go:linkname localSleep runtime.timeSleep
  func localSleep(ns int) // Error: remote runtime.timeSleep signature requires int64, got int
  ```
- Negative example:
  ```go
  //go:linkname localSleep runtime.timeSleep
  func localSleep(ns int64) // Compliant: signature matches remote runtime symbol
  ```
- Analysis requirements: AST directive parsing and cross-package type signature validation against remote object export data. Note: pointer types must not be collapsed to `uintptr`, and struct/interface types must be recursively validated.
- Diagnostic evidence: reported span highlights the directive, citing local signature, remote signature, and target Go toolchain version.
- Fix safety: manual update of local function signature to match remote target.
- Acceptance cases: flags unknown remote symbol, signature type mismatch, and malformed directive syntax.

#### slop::empty_critical_section

- ID: `slop::empty_critical_section`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Acquiring and immediately releasing a lock without performing operations usually indicates a misplaced unlock (e.g. forgetting `defer`). However, empty locks can be used as memory synchronization barriers or quiescence drains.
- Detects: calls to `Lock()` immediately followed by `Unlock()` on the same mutex within the same basic block with no intervening statements.
- Exempt / negative cases: critical sections containing explicit explanatory comments indicating synchronization barrier intent; code annotated with `//logos:allow(slop::empty_critical_section)`; lock ordering test fixtures.
- Positive example:
  ```go
  func (s *Service) Update() {
      s.mu.Lock()
      s.mu.Unlock() // Warning: empty critical section; did you mean defer s.mu.Unlock()?
      s.count++
  }
  ```
- Negative example:
  ```go
  func (s *Service) Drain() {
      s.workerMu.Lock()
      // Barrier: wait for all active workers holding workerMu to release
      s.workerMu.Unlock()
  }
  ```
- Analysis requirements: AST basic block inspection matching sequential lock and unlock method invocations on identical receiver expressions.
- Diagnostic evidence: reported span highlights the lock-unlock call pair.
- Fix safety: manual inspection required. May require moving unlock to `defer` or documenting barrier intent.
- Acceptance cases: flags adjacent lock/unlock calls, accepts lock/unlock with body statements or barrier comments.

#### pedantic::unused_allow

- ID: `pedantic::unused_allow`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Allow (pedantic). Raised to `warn` under `--pedantic`. Prevents directive rot when refactoring resolves underlying issues.
- Detects: `//logos:allow(group::name)` directives that matched zero lint findings in the statement or declaration block below them.
- Exempt / negative cases: directives silencing findings that only fire on specific target architectures or build tags when evaluated in a multi-platform matrix; files excluded from linting.
- Positive example:
  ```go
  //logos:allow(slop::discarded_error)
  n, err := file.Read(buf) // Warning (under --pedantic): unused allow directive; err is not discarded
  if err != nil { return err }
  ```
- Negative example:
  ```go
  //logos:allow(slop::discarded_error)
  file.Read(buf) // Compliant: allow matches discarded error finding
  ```
- Analysis requirements: lint runner aggregation comparing parsed directives against emitted findings per file.
- Diagnostic evidence: reported span covers the `//logos:allow` comment line.
- Fix safety: automated removal of the comment line is completely safe.
- Acceptance cases: flags directive above clean statement, accepts directive matching an emitted finding.
### Core lock-state contract (1 rule)

#### concurrency::lock_state

- ID: `concurrency::lock_state`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (contract-driven). Implements the gVisor `checklocks` model of explicit lock preconditions and postconditions.
- Detects: field access, method calls, and control flow paths that violate declared locking requirements. Enforces that:
  - Guarded fields (`+checklocks:mu`) are only accessed when the designated mutex is held in the required mode (read or write).
  - Functions annotated with preconditions (`+checklocks:mu` or `+checklocksread:mu`) are only called when the caller holds the lock.
  - Functions annotated with release/acquire contracts (`+checklocksacquire:mu`, `+checklocksrelease:mu`) update the caller lock state accurately.
  - Returns from functions do not leave unexpected locks held, unless marked with acquire.
- Exempt / negative cases:
  - Fresh allocations: checks on newly allocated struct instances (`new(T)`, composite literals `&T{}`) are elided before concurrent publication (`freshAlloc` bypass).
  - Same-scope closures: anonymous functions invoked directly within the same lexical scope inherit the caller active lock state.
  - Read-any guards: fields annotated with `+checklocksreadany` permit reads if any one guard is held, while writes require all guards exclusively.
  - Unknown calls: calls through unannotated external functions, dynamic function values, or interface dispatches cannot prove lock safety. The checker assumes no locks are acquired or released across unknown calls; if a lock must be held across a call, an unannotated callee cannot guarantee safety and triggers a verification warning.
- Positive example:
  ```go
  type Account struct {
      mu sync.Mutex
      // +checklocks:mu
      balance int
  }
  func (a *Account) Deposit(amount int) {
      a.balance += amount // Error: a.balance requires lock a.mu held
  }
  ```
- Negative example:
  ```go
  func (a *Account) Deposit(amount int) {
      a.mu.Lock()
      defer a.mu.Unlock()
      a.balance += amount // Compliant: lock is held
  }
  ```
- Analysis requirements: SSA-form intra-procedural dataflow with lockset tracking across control flow graph join points.
- Diagnostic evidence: reported span cites the unguarded field access or invalid function call, stating the missing lock name and required access mode.
- Fix safety: acquire required mutex before accessing field or annotate function with required precondition.
- Acceptance cases: flags unguarded field reads, unguarded field writes with read-only locks, unbalanced branches, and missing locks at function calls.

### Strict additions wave (13 rules)

#### concurrency::atomic_value

- ID: `concurrency::atomic_value`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Forbid (contract-driven). Implements the gVisor `checkatomic` model.
- Detects: non-atomic reads or writes to struct fields or global variables annotated with `// +checkatomic`. Ensures that every access to annotated memory locations is performed atomically. Mixed atomic and mutex rules are enforced only where `+checklocks` is also declared on the field.
- Exempt / negative cases: operations on newly allocated objects (`freshAlloc`) prior to concurrent publication; typed atomic wrapper types (`atomic.Int32`, `atomic.Pointer[T]`, `atomicbitops.Int32`) whose methods encapsulate atomicity without annotations; pure atomic fields accessed via goroutines or deferred calls; unannotated struct fields and local variables.
- Positive example:
  ```go
  type Stats struct {
      // +checkatomic
      requests int64
  }
  func (s *Stats) Inc() {
      s.requests++ // Error: non-atomic access to field marked +checkatomic
  }
  ```
- Negative example:
  ```go
  type Stats struct {
      // +checkatomic
      requests int64
  }
  func (s *Stats) Inc() {
      atomic.AddInt64(&s.requests, 1) // Compliant: atomic operation on +checkatomic field
  }
  ```
- Analysis requirements: AST and types2 lookup verifying all field read and write operations against `+checkatomic` annotations.
- Diagnostic evidence: reported span cites the non-atomic access statement and the target field declaration.
- Fix safety: replace non-atomic read or write with atomic primitives (`atomic.LoadInt64`, `atomic.AddInt64`, `atomic.StoreInt64`) or migrate to typed `atomic.Int64`.
- Acceptance cases: flags direct assignment or increment on `+checkatomic` field, flags non-atomic read on `+checkatomic` field, accepts atomic primitives, elides checks on `freshAlloc` instances.

#### portability::integer_narrowing

- ID: `portability::integer_narrowing`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Explicitly flags conversions between fixed-width types that risk truncation across 32-bit and 64-bit systems.
- Detects: explicit casts from a wider integer type to a narrower integer type (such as `int64` to `int32`, `uint32` to `uint16`, or `int` to `int32` on 64-bit platforms) without an explicit bounds check.
- Exempt / negative cases: conversions between pointer-width peers (`uintptr` and `uint`), which have identical bit width on all Go architectures (both 32-bit or both 64-bit); conversions preceded by a dominating range guard (`if v <= math.MaxInt32`); bitwise masking (`v & 0xFFFF`).
- Positive example:
  ```go
  func storeID(id int64) int32 {
      return int32(id) // Warning: integer narrowing from int64 to int32 without bounds check
  }
  ```
- Negative example:
  ```go
  func storeID(id int64) (int32, error) {
      if id > math.MaxInt32 || id < math.MinInt32 {
          return 0, errors.New("id overflow")
      }
      return int32(id), nil // Compliant: bounded conversion
  }
  ```
- Analysis requirements: types2 numeric bit-width comparison with dominating branch condition analysis.
- Diagnostic evidence: reported span cites the cast expression, naming source width and target width.
- Fix safety: add bounds check before conversion.
- Acceptance cases: flags `int32(int64Var)`, accepts `uint(uintptrVar)`, accepts masked `uint16(uint32Var & 0xFFFF)`.

#### perf::timer_churn

- ID: `perf::timer_churn`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Replaces former inaccurate timer leak claims. In modern Go (1.23+), unreferenced timers and tickers are garbage-collected, making this an allocation and timer queue churn issue rather than a permanent memory leak. The target contract supersedes and subsumes the implemented in-compiler rule `perf::time_after_in_loop`. The current compiler identity (`perf::time_after_in_loop`) remains unchanged until replaced. When implemented, `perf::timer_churn` emits exactly ONE finding per loop creating `time.Tick` or `time.After`, eliminating diagnostic duplication. Migration of existing user suppressions from `//logos:allow(perf::time_after_in_loop)` to `//logos:allow(perf::timer_churn)` is explicit in the same cutover, without a compatibility alias.
- Detects: calls to `time.Tick` or `time.After` inside loops (including infinite loops, range loops, and for loops) or frequently executed handler functions.
- Exempt / negative cases: `time.Tick` invoked at top-level package scope or in a long-lived daemon worker loop where the ticker lives for the life of the process; `time.NewTicker` or `time.NewTimer` created outside the loop and properly reused/reset with `defer ticker.Stop()`. Note: Go 1.23 GC recovery applies to both timers and tickers, but loop construction creates unnecessary allocation pressure regardless of loop finiteness.
- Positive example:
  ```go
  func poll(ch <-chan Event) {
      for e := range ch {
          select {
          case <-time.After(50 * time.Millisecond): // Warning: time.After in loop allocates a timer per iteration
              timeout()
          case handle(e):
          }
      }
  }
  ```
- Negative example:
  ```go
  func poll(ch <-chan Event) {
      timer := time.NewTimer(50 * time.Millisecond)
      defer timer.Stop()
      for e := range ch {
          timer.Reset(50 * time.Millisecond)
          select {
          case <-timer.C:
              timeout()
          case handle(e):
          }
      }
  }
  ```
- Analysis requirements: AST traversal checking `time.Tick` and `time.After` call nodes enclosed within loop bodies.
- Diagnostic evidence: reported span highlights the timer creation call inside the loop body.
- Fix safety: hoist timer creation outside loop and use `Reset()` or reusable `time.Ticker`.
- Acceptance cases: flags `time.Tick` in for-loop, flags `time.After` in select inside loop, accepts top-level daemon ticker.

#### perf::allocation_contract

- ID: `perf::allocation_contract`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (contract-driven). Adopts the gVisor `checkescape` model for recursive escape verification on critical performance paths.
- Detects: operations inside functions marked with `// +checkescape` that trigger heap allocations, allocating runtime builtins, or soft escapes (dynamic function dispatches, interface invocations, unanalyzed cross-package calls).
- Exempt / negative cases: functions not annotated with `// +checkescape`; local stack allocations; operations explicitly marked with `// escapes: reason`; pure value computations. Note: bare `// +checkescape` defaults to checking local hard escapes; recursive verification requires explicit `:all` annotation.
- Positive example:
  ```go
  // +checkescape:all
  func FastPath(data []byte) int {
      val := any(len(data)) // Warning: interface conversion allocates boxing interface header
      return compute(val)
  }
  ```
- Negative example:
  ```go
  // +checkescape:all
  func FastPath(data []byte) int {
      return computeDirect(len(data)) // Compliant: no heap allocation or dynamic dispatch
  }
  ```
- Analysis requirements: compiler escape analysis facts and SSA callgraph traversal.
- Diagnostic evidence: reported span cites the allocating instruction, interface dispatch, or dynamic call site.
- Fix safety: refactor to eliminate heap allocation, replace interface dispatch with concrete generics, or provide explicit `// escapes` exemption.
- Acceptance cases: flags interface boxing, flags closure allocation, flags make/new escaping, accepts stack-bound code.

#### style::receiver_consistency

- ID: `style::receiver_consistency`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Enforces standard Go and loGo/s style guidelines for method receiver types and names.
- Detects: a type whose method set mixes value receivers and pointer receivers, or uses inconsistent receiver parameter names across methods of the same type.
- Exempt / negative cases: types that define value methods to satisfy immutable interfaces and pointer methods for mutation; generated code; types with only one method.
- Positive example:
  ```go
  type Buffer struct{}
  func (b Buffer) Len() int { return 0 }
  func (buf *Buffer) Reset() {} // Warning: Buffer mixes value receiver (b) and pointer receiver (buf)
  ```
- Negative example:
  ```go
  type Buffer struct{}
  func (b *Buffer) Len() int { return 0 }
  func (b *Buffer) Reset() {} // Compliant: consistent pointer receiver and identifier 'b'
  ```
- Analysis requirements: AST collection of all method declarations grouped by receiver base type.
- Diagnostic evidence: reported span highlights the inconsistent method declaration, citing the conflicting receiver name or pointer mode.
- Fix safety: unify receiver name across methods; convert value receivers to pointer receivers where appropriate.
- Acceptance cases: flags mixed value/pointer methods, flags mixed receiver names (`s` vs `self`), accepts uniform receivers.

#### style::exported_needs_doc

- ID: `style::exported_needs_doc`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Exported package API declarations must be documented.
- Detects: exported types, functions, methods, variables, and constants in non-test files that lack a leading documentation comment.
- Exempt / negative cases: declarations in `_test.go` or `_test.lgo` files; internal or main packages; methods implementing exported interface methods where doc comments are inherited.
- Positive example:
  ```go
  package api
  type Client struct{} // Warning: exported type Client should have comment
  ```
- Negative example:
  ```go
  package api
  // Client manages remote connections.
  type Client struct{}
  ```
- Analysis requirements: AST inspection of exported declarations in exported packages.
- Diagnostic evidence: reported span cites the declaration identifier.
- Fix safety: manual addition of doc comment starting with the declaration name.
- Acceptance cases: flags undocumented exported type, function, and const; accepts documented declarations.

#### style::naming_convention

- ID: `style::naming_convention`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Enforces Go and loGo/s identifier casing and convention rules.
- Detects: identifiers containing underscores (except in test names, cgo symbols, or generated code), package names with capital letters or hyphens, or stuttering type and package combinations (`user.UserManager`).
- Exempt / negative cases: test identifiers (`Test_Feature`); cgo identifiers (`C.struct_foo`); generated code; constants matching recognized external protocol definitions.
- Positive example:
  ```go
  package user_service // Warning: package name user_service should not contain underscores
  type User_Record struct{} // Warning: struct name User_Record should be UserRecord
  ```
- Negative example:
  ```go
  package userservice
  type UserRecord struct{}
  ```
- Analysis requirements: AST identifier name token inspection.
- Diagnostic evidence: reported span highlights the identifier, citing the naming convention violation.
- Fix safety: automated rename refactoring across package references.
- Acceptance cases: flags underscored identifiers, flags capitalized package names, accepts camelCase and MixedCaps.

#### suspicious::defer_in_loop

- ID: `suspicious::defer_in_loop`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Replaces draft ID `correctness::defer_in_loop`. Moved from `correctness` to `suspicious` because heuristic checks cannot default to `warn` in an unlowerable `forbid` group where group defaults or configuration would elevate them to `forbid`. In Go, `defer` statements inside loops execute only when the enclosing function returns, not at the end of each iteration, creating descriptor or resource spikes.
- Detects: `defer` statements located directly inside `for` loops where the loop body is not wrapped in a function literal.
- Exempt / negative cases: `defer` statements inside function literals invoked immediately within the loop (`func() { defer ... }()`); loGo/s lexical `defer` inside blocks with explicit block lifetimes.
- Positive example:
  ```go
  for _, file := range files {
      f, _ := os.Open(file)
      defer f.Close() // Warning: defer in loop delays resource release until function exit
  }
  ```
- Negative example:
  ```go
  for _, file := range files {
      func(file string) {
          f, _ := os.Open(file)
          defer f.Close() // Compliant: closure executes defer on every iteration
      }(file)
  }
  ```
- Analysis requirements: AST statement traversal checking `defer` nodes under loop statement parents.
- Diagnostic evidence: reported span cites the `defer` keyword inside the loop body.
- Fix safety: extract loop body into helper function or wrap in immediately invoked closure.
- Acceptance cases: flags bare `defer` in for-loop, accepts `defer` inside loop closure.

#### suspicious::empty_branch

- ID: `suspicious::empty_branch`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Empty conditional blocks indicate omitted logic or incomplete refactoring.
- Detects: `if`, `else`, `for`, or `select` blocks containing no statements, or switch cases containing only a blank statement without explanatory comments.
- Exempt / negative cases: branches containing explanatory comments (e.g. `// intentional no-op`); switch cases explicitly fallthrough-ing.
- Positive example:
  ```go
  if err != nil {
      // Empty branch: omitted error handling
  }
  ```
- Negative example:
  ```go
  if err != nil {
      // Intentional no-op: ignore error during shutdown
  }
  ```
- Analysis requirements: AST block statement node inspection and associated comment group tracking.
- Diagnostic evidence: reported span covers the empty block or clause.
- Fix safety: manual inspection required. Code must either implement handling or document why no operation is performed.
- Acceptance cases: flags empty if block, flags empty else block, accepts commented empty block.

#### suspicious::self_assignment

- ID: `suspicious::self_assignment`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (high certainty). Self-assignment is redundant and usually indicates a typo where a different variable was intended.
- Detects: assignments of the form `x = x` or `s.field = s.field` where left-hand and right-hand expressions resolve to identical storage.
- Exempt / negative cases: none in production code. Note: pre-Go 1.22 loop variable shadowing (`item := item`) was a short variable declaration, not an assignment statement. Under Go 1.22+, `copyloopvar` governs shadowing.
- Positive example:
  ```go
  func SetWidth(width int) {
      width = width // Warning: redundant self-assignment; intended this.width = width
  }
  ```
- Negative example:
  ```go
  func (s *Screen) SetWidth(width int) {
      s.width = width
  }
  ```
- Analysis requirements: AST assignment statement inspection comparing left and right expressions by symbol resolution.
- Diagnostic evidence: reported span highlights the assignment statement.
- Fix safety: delete redundant statement or correct target identifier.
- Acceptance cases: flags `x = x`, `s.f = s.f`, accepts `s.f = other.f`.

#### suspicious::always_true_error_check

- ID: `suspicious::always_true_error_check`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Detects error checks whose outcome is statically predetermined.
- Detects: `if err != nil` checks where static control flow proves `err` is guaranteed nil or guaranteed non-nil at that program point.
- Exempt / negative cases: checks on error values returned by functions whose implementations are external or unknown; error values mutated across concurrency boundaries.
- Positive example:
  ```go
  var err error = nil
  if err != nil { // Warning: err is statically proven nil; condition is always false
      return err
  }
  ```
- Negative example:
  ```go
  val, err := parse()
  if err != nil { // Compliant: err comes from dynamic function result
      return err
  }
  ```
- Analysis requirements: SSA constant propagation and nilness analysis.
- Diagnostic evidence: reported span highlights the conditional expression, citing the proven nilness state.
- Fix safety: remove dead conditional check or adjust prior assignment.
- Acceptance cases: flags `err != nil` after `err = nil`, accepts dynamic error checks.

#### perf::range_copy

- ID: `perf::range_copy`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Ranging over large array values by copy causes repeated memory copying on each iteration.
- Detects: `for _, item := range array` where `array` is an array type (not a slice or pointer to array) whose element size exceeds 128 bytes, or ranging over a slice where the value variable copies a large struct.
- Exempt / negative cases: ranging by index only (`for i := range array`); ranging over slices of pointers (`[]*LargeStruct`); structs smaller than 128 bytes.
- Positive example:
  ```go
  var items [1000]BigStruct // each BigStruct is 256 bytes
  for _, item := range items { // Warning: copying 256-byte BigStruct on every iteration
      process(item)
  }
  ```
- Negative example:
  ```go
  for i := range items {
      process(&items[i]) // Compliant: pointer access avoids per-iteration copy
  }
  ```
- Analysis requirements: types2 type determination of range expression and element size computation.
- Diagnostic evidence: reported span cites the range value variable, stating the element size in bytes.
- Fix safety: rewrite range to index-only or slice of pointers.
- Acceptance cases: flags range by value over large array, accepts index-only range.

#### perf::huge_param

- ID: `perf::huge_param`
- Applicability: Go and loGo/s (`Go: true`).
- Severity recommendation and certainty: Warn (heuristic). Passing large structs by value copies large chunks of memory on function entry.
- Detects: functions and methods accepting parameters by value whose struct size exceeds 256 bytes.
- Exempt / negative cases: interface methods required to match external library signatures; types implementing `sync/atomic` wrappers; small structs.
- Positive example:
  ```go
  type Matrix [1024]float64
  func Det(m Matrix) float64 { // Warning: Matrix is 8192 bytes and passed by value
      return calculate(m)
  }
  ```
- Negative example:
  ```go
  func Det(m *Matrix) float64 { // Compliant: pointer pass avoids copying 8192 bytes
      return calculate(m)
  }
  ```
- Analysis requirements: types2 struct size calculation on parameter types.
- Diagnostic evidence: reported span highlights the parameter declaration, stating type size.
- Fix safety: convert parameter to pointer type; update callsites.
- Acceptance cases: flags parameter exceeding 256 bytes, accepts pointer parameter, accepts small value parameters.
## Retired and mapped proposals

The design cleanly separates valid target rules from superseded, redundant, or technically erroneous proposals. Every retired proposal is mapped below with its concrete engineering rationale:

| Retired / Former Proposal | Target Status or Replacement | Engineering Rationale |
|---|---|---|
| `correctness::copy_lock` | Mapped to `suspicious::copy_lock` | Heuristic checks cannot default to `warn` under the unlowerable `correctness` group without being elevated to `forbid` by group defaults or `[lints]` configuration. Reclassified under `suspicious` (default `warn`). |
| `correctness::boolean_contradiction` / `bools_simplify` | Mapped to `suspicious::boolean_contradiction` | The former draft placed `bools_simplify` under `style`, then under `correctness`. True boolean contradictions are heuristic bugs that cannot live in unlowerable `correctness` with a `warn` default. Reclassified under `suspicious` (default `warn`). Pure identical operand redundancy (`x && x`) is handled separately from contradiction. |
| `correctness::string_intconv` | Mapped to `suspicious::string_intconv` | `string(int)` rune-formatting confusion is legal syntax requiring a heuristic `warn` default; placing it under `correctness` would force it to `forbid`. Reclassified under `suspicious` (default `warn`). |
| `correctness::defer_in_loop` | Mapped to `suspicious::defer_in_loop` | Deferring inside a loop body is legal syntax and valid in short loops, making it an architectural heuristic that defaults to `warn`. Reclassified under `suspicious` (default `warn`). |
| `perf::time_after_in_loop` | Superseded by `perf::timer_churn` | The active in-compiler rule `perf::time_after_in_loop` remains active today. Target `perf::timer_churn` supersedes and subsumes it to emit a single diagnostic covering both `time.Tick` and `time.After`. User suppressions will migrate explicitly in the same cutover, without a compatibility alias. |
| `int(string)` conversion check | Dropped from `suspicious::string_intconv` | `int(s)` with a string operand is valid conversion syntax but fails type checking. Parse numeric text with `strconv.Atoi`. The rule correctly flags `string(int)` rune-formatting confusion. Explicit rune conversions `string(rune(x))` are exempt. |
| Universal race freedom claim | Replaced by `concurrency::lock_state` | Static analysis cannot guarantee universal data-race freedom across dynamic dispatches, unannotated external packages, and CGo boundaries. `concurrency::lock_state` enforces contract-driven locking invariants via explicit preconditions and postconditions. |
| Blanket directory linkname exemption | Replaced by `portability::linkname_discipline` | Exempting directories like `logos/rt` or `runtime` bypasses signature safety. The target contract type-checks local function declarations against remote package symbol tables (`checklinkname` contract). |
| Blanket timer leak (`time_tick_leak`) | Replaced by `perf::timer_churn` | In modern Go (1.23+), unreferenced timers and tickers are collected by the runtime garbage collector even if unstopped. They do not cause indefinite memory leaks. Constructing them in loops causes timer queue and allocation churn. |
| Generic `uintptr` to `uint` narrowing | Replaced by `portability::integer_narrowing` | In Go and loGo/s, `uintptr` and `uint` are pointer-width peers on all target architectures (both 32-bit on 32-bit systems, both 64-bit on 64-bit systems). No truncation occurs between them. True narrowing occurs between fixed-width types (e.g. `int64` to `int32`). |
| Invented `[lints.profile]` and `[lints.scope]` tables | Non-normative schema proposals | `logos.toml` strictly supports only the 9 group keys and 6 threshold keys. Speculative profile and scope tables are not implemented and must not be treated as executable configuration knobs. |
| `style::comment_formatting` | Retired as standalone rule | Comment formatting (leading symbol naming and punctuation) is enforced by standard repo formatting conventions (`gofmt`) and documentation guidelines, not compiler lint rules. |
| `pedantic::exhaustive_switch` | Mapped to `pedantic::exhaustive_union_switch` | Exhaustive type switching over closed unions is already implemented in `pedantic::exhaustive_union_switch`. Non-union enum switches remain open-ended and cannot guarantee compile-time exhaustiveness without sealed enum language primitives. |

## gVisor analyzer adoption contracts

The target design adapts four custom static analyzers from gVisor (`.references/gvisor/tools/`). Their adoption contracts define strict modular boundaries, fact export, and conservative fallback behavior.

### 1. Lock and atomic enforcement (checklocks & checkatomic)

Source references: `.references/gvisor/tools/checklocks/README.md`, `checklocks.go`, `analysis.go`.

- **Annotation primitives**:
  - Struct fields and globals: `// +checklocks:mu`, `// +checkatomic`, `// +checklocksreadany`.
  - Functions: `// +checklocks:mu` (precondition), `// +checklocksread:mu` (read-only precondition), `// +checklocksexclude:mu` (lock not held), `// +checklocksacquire:mu` (lock acquired), `// +checklocksrelease:mu` (lock released).
  - Type aliases: `// +checklocksalias:inner.mu=mu`.
- **Intra-procedural dataflow**:
  - The checker verifies locksets across control flow joins within each function declaration.
  - No alias lifetime tracking: field addresses must be consumed immediately in the same basic block; storing an address into an intermediate pointer or atomic payload loses tracking (`README.md:47-49, 126-131`).
  - Fresh allocations: checks on newly allocated objects (`*ssa.Alloc`) are elided (`README.md:44-45, 140, 313`, `analysis.go:776-794`), assuming new instances remain thread-local before publication.
- **Closures and concurrency boundaries**:
  - Closures cannot be annotated (`README.md:218-219`).
  - Closures invoked directly in the same scope inherit the active lock state (`README.md:221-233`).
  - Closures passed as arguments or spawned via `go` lose context and are analyzed assuming zero locks held (`README.md:255-263`).
  - Goroutines and deferred calls are permitted for pure atomic targets (`README.md:60-63`), but strictly forbidden for mixed atomic+mutex targets because lock state cannot be proven at deferred/goroutine execution time (`README.md:315-318`).
- **Unknown call fallback**:
  - Calls to unannotated external functions, dynamic function values, or interface dispatches cannot prove lock preservation. The analyzer assumes no locks are acquired or released across the call; if a lock must remain held across the call, an unannotated callee cannot guarantee safety and triggers a verification warning.

### 2. Allocation and escape verification (checkescape)

Source reference: `.references/gvisor/tools/checkescape/checkescape.go`.

- **Hard vs. soft escapes**:
  - Hard escapes: direct heap allocations (`allocation`) and allocating runtime builtins (`builtin`).
  - Soft escapes: interface method dispatches (`interfaceInvoke`), dynamic function calls (`dynamicCall`), possible stack splits (`stackSplit`), and unanalyzed external packages (`unknownPackage`).
- **Scope modes**:
  - Bare `// +checkescape` defaults strictly to `local,hard` (`checkescape.go:41-42, 50, 836-839`). It does not verify callers recursively and ignores soft escapes.
  - The `:local` modifier restricts call traversal. Omitting it permits call-graph analysis. The `:all` modifier selects all escape reasons, including soft escapes. These are separate choices.
- **Unknown calls and dynamic dispatches**:
  - Interface dispatches (`x.Call.IsInvoke()`) and dynamic function calls emit soft escapes (`checkescape.go:1398-1405, 1438-1445`). They prevent zero-allocation proofs unless specifically exempted with `// escapes: reason`.
  - Generic functions cannot prove allocation elimination across arbitrary instantiations; operations without concrete constraint implementations count as dynamic escapes (`checkescape.go:64-69, 1475-1484`).

### 3. Linkname signature discipline (checklinkname)

Source reference: `.references/gvisor/tools/checklinkname/README.md`, `check_linkname.go`.

- **Contract scope**:
  - Validates `//go:linkname` directives against known target signatures defined in symbol tables (`check_linkname.go:knownLinknames`, described in `README.md`).
  - Detects signature drift when standard library internal types change across Go minor releases.
- **Critical limits of existing implementation**:
  - Existing `checklinkname` has known defects: `makeStructSignature` (`check_linkname.go:440`) and `makeInterfaceSignature` (`check_linkname.go:451`) use zero-iteration loops (`for i := 0; i < len(parts); i++` where `len(parts) == 0`), leaving struct fields and interface methods uninspected.
  - Pointers (`*T`, `unsafe.Pointer`) are collapsed to `"uintptr"`, masking pointer type mismatches (`check_linkname.go:312, 358-360`).
  - The loGo/s adoption contract fixes these defects: recursive struct field comparison, exact interface method verification, and strict pointer type parity are mandatory.
- **Suppression vs. exclusion scope**:
  - In `nogo.yaml:238-257`, linkname suppressions apply strictly to named symbols (`runtime_pollWaitCanceled`, `runtime_procPin`, `registerMetric`) and one specific file exclusion (`.*/containerd/sys/subprocess_unsafe_linux.go`).
  - Directory exclusions for `*_unsafe.go` live under `unsafeptr` (`nogo.yaml:198-229`), not `checklinkname`. Blanket directory exemptions are rejected in loGo/s lint design.

### 4. Modular facts, invalidation, and failure behavior

- **Fact serialization**: Analyzers export modular facts (`analysis.Fact`) across package boundaries through compiler export data.
- **Cache invalidation**: Modifying exported types, lock annotations, or escape contracts in an upstream package invalidates the cached analysis facts of all downstream consumers.
- **Fact decode and failure behavior**: Malformed, incompatible, version-mismatched, or unreadable fact payloads invalidate dependent caches and fail closed. When fact deserialization fails, the analyzer emits a diagnostic and falls back to conservative unknown results: it treats unverified locks as unheld and external calls as soft escapes. Analysis passes must never reuse stale facts or claim verification proof after a fact decode failure.
- **Foreign code boundary**: Unanalyzed dependencies without exported facts are treated conservatively: locks are assumed unheld across foreign calls, and foreign function calls count as soft escapes.

## Policy machinery and configuration

### Supported logos.toml schema

The compiler strictly accepts the following configuration keys (`src/cmd/go/internal/logos/config/lints.go`):

1. **Nine group severity keys** (`[lints]`):
   - `correctness`: default `forbid`, unlowerable.
   - `concurrency`: default `forbid`, unlowerable.
   - `slop`: default `deny`.
   - `complexity`: default `deny`.
   - `perf`: default `warn`.
   - `style`: default `warn`.
   - `suspicious`: default `warn`.
   - `portability`: default `warn`.
   - `pedantic`: default `allow` (raised to `warn` by `--pedantic`).
2. **Six integer threshold keys** (`[lints.thresholds]`):
   - `max_nesting_depth` (default 3, complexity control)
   - `max_cognitive_complexity` (default 15, complexity control)
   - `max_function_lines` (default 60, complexity control)
   - `max_function_params` (default 4, complexity control)
   - `max_comptime_ast_nodes` (default 4096, comptime resource limit)
   - `max_ctfe_steps` (default 100000, comptime resource limit)

Any other keys or tables (such as `[lints.profile]` or `[lints.scope]`) are rejected by the policy parser (`policy.go`).

#### Precedence and carve-out preservation

Policy severity resolution adheres strictly to `Policy.Level(id, k)` (`policy.go:160-176`):
1. Configured group override (`p.groups[g]`)
2. Rule-level default (`r.Default` via `Lookup(id)`)
3. Group-level default (`DefaultSeverity(g)`)
4. Go-package cap (`Forbid` -> `Deny` in `KindGo`)

Specifying `correctness = "forbid"` in `logos.toml` is not a neutral default: it acts as a group-wide override that elevates the `Rule.Default` carve-outs (`unclosed_io_closer` and `unbounded_channel_op`) from `deny` to `forbid` in `.lgo` packages. To preserve these default carve-outs, projects should omit `[lints]` or provide an empty `[lints]` table:

```toml
# Default-equivalent configuration: preserves Rule.Default Deny carve-outs
[lints]
```

### Code ownership and generated code

- **Generated code ownership**: No file name prefix alone (such as `_logos_` or `*_gen.go`) marks code as generated. Generated files must carry standard machine-generated header comments (`^// Code generated .* DO NOT EDIT\.$`).
- **Scope discipline**:
  - Main module packages are fully linted.
  - Standard library packages and third-party dependency modules are excluded from project linting (`logosLintMark`).
  - Generated code is subject to correctness and concurrency rules, while stylistic and cognitive complexity rules are suppressed.

### Suppression discipline

- **Statement-level allow**: `//logos:allow(<group::name>)` on the line immediately preceding a statement or declaration silences `deny` and `warn` findings.
- **Package-clause allow**: `//logos:allow(portability::alias_extension)` directly preceding the package clause silences module load-time findings.
- **Forbid cannot be silenced**: In a loGo/s package, attempting to silence a `forbid` rule is reported as `correctness::allow_forbidden_rule`.
- **Zeroalloc unsilenceable**: `perf::zeroalloc_violation` is an unconfigurable, unsilenceable static allocation-site assertion (`SPEC.md:2044`). It accepts no allow directives and cannot be lowered by configuration.
- **Unused allow**: An allow directive that matches no finding is reported as `pedantic::unused_allow` under `--pedantic`.

## Fixture conventions

- Attack fixtures for existing registered rules live under `test/logos/lint/`:
  - 23 `.lgo` attack fixtures and 14 `_go.go` language-neutral twins.
  - `nil_deref_unproven` has two fixtures: `nil_deref_unproven.lgo` and `nil_deref_unproven_allow.lgo`.
  - `zeroalloc_go.go` is the Go twin of `test/logos/codegen/zeroalloc.lgo`.
- Proposed rules do not yet have fixtures in the repository; their test cases define the acceptance criteria for implementation.
- Fixtures carrying `// errorcheck` test deny and forbid rules; fixtures carrying `// errorcheck -0` test warn-only rules.

## golangci-lint reference integration

For repository Go tooling (`src/cmd/` and `src/`), `logos-docs/golangci.yml` uses the following curated set from `.references/golangci-lint/.golangci.reference.yml`:

- **Active linters**:
  - `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`: standard base.
  - `errorlint`: flags un-wrapped `%v` error formatting (asserts disabled).
  - `unconvert`: flags redundant type conversions.
  - `unparam`: flags unused parameter values.
  - `noctx`: flags HTTP requests constructed without context.
  - `intrange`: enforces modern integer range syntax.
  - `misspell`: US locale spell-checking.
  - `copyloopvar`: flags redundant per-iteration loop variable copies (Go 1.22+).
  - `gocheckcompilerdirectives`: validates compiler directive placement.
  - `gochecknoinits`: flags `init` functions outside the registration path carve-out.
  - `nolintlint`: enforces structured, explained inline suppressions (`require-explanation: true`, `require-specific: true`).
  - `govet nilness`: SSA-based nil dereference analysis.
- **Excluded linters**:
  - `shadow`: disabled due to false positive noise on inlined code (matches gVisor policy).
  - `dupl`, `funlen`, `lll`: disabled to prevent arbitrary code churn.
  - `QF1001`, `QF1008`: disabled due to controversial stylistic rewrites.
- **Depguard boundary rule**:
  - `logos/rt` and `logos/sync` may not import each other.
  - Nothing under `src/cmd/compile` may import `logos/rt` outside designated lowering seam files.
