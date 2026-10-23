# Lint ruleset for Go and loGo/s

Status: design-only proposal for dedicated lint execution covering Go and
loGo/s packages. It specifies the target architecture and rulesets,
distinguishing the current implemented compiler inventory from the proposed
target design. It draws architectural principles from Tony Hoare (sound
assertions, invariants, preconditions, and postconditions) and David Parnas
(modular information hiding and stable module boundaries).

Detailed per-rule entries with implementation citations, attack fixtures,
and test contracts live in [LINT-RULES.md](LINT-RULES.md). Repository Go
tooling linter configurations live in [golangci.yml](golangci.yml).

## Architecture and execution model

Dedicated lint execution is decoupled from standard compilation.

- **Dedicated execution**: Lint rules run under the `logos lint` command.
  Standard builds (`logos build` and `go build`) compile code, generate
  binaries, and enforce language and type safety invariants. Standard
  builds do not execute lint rules.
- **No adoption performance budgets**: Linting is an explicit verification
  activity. Proposal validation may have execution costs, but imposes no
  percentage latency budget gates on compiler development. Analyzers are
  designed for termination and resource safety, but depth is not compromised
  for arbitrary build-time budget limits.
- **In-compiler semantic engine**: The in-compiler lint engine
  (`src/cmd/compile/internal/logos/lint`) serves both Go and loGo/s source.
  The engine directly observes AST structures, type systems, lowering
  states, and escape facts within the compilation unit without external
  serializations or schema adapters.
- **External tooling boundary**: `golangci-lint` governs repository Go
  tooling only (such as compiler and driver packages). It does not analyze
  `.lgo` source files or compiler-generated outputs.

Existing compile-time hooks in the compiler frontend remain active for
language-level enforcement. In the current compiler, `.lgo` compilations
invoke lint hooks when a policy is configured. In the proposed target
design, all lint-level rule evaluations migrate to dedicated `logos lint`
invocations across both languages.

## Analysis of lowered code: alternatives and hybrid pipeline

Static analysis must balance syntactic fidelity against semantic depth.
Evaluating code after lowering introduces compiler-generated artifacts,
while evaluating code purely before lowering blinds analyzers to escape
facts, memory layout, and dataflow.

### Alternative A: Pure source AST analysis

In this model, all lint checks execute strictly on source ASTs and type
information prior to any desugaring, inlining, or lowering.

- **Tradeoffs**: Diagnosed positions match original source tokens
  identically. Analysis is immune to synthetic nodes, compiler temporaries,
  and desugared control flow.
- **Defects**: Blind to post-escape allocation facts, monomorphized shape
  specializations, and lower-level control flow. It cannot implement
  escape-based allocation contracts or dataflow-based nilness without
  duplicating the compiler backend.

### Alternative B: Lowered representation analysis via external tools

In this model, the compiler lowers `.lgo` source into standard Go or SSA
forms, and standard external tools (such as `cmd/vet` or `golangci-lint`)
run over the lowered output.

- **Tradeoffs**: Directly reuses existing Go ecosystem analyzers and SSA
  algorithms without custom engine maintenance.
- **Defects**: Severe position drift. Desugaring constructs like range loops,
  pattern matches, or error propagation introduces synthetic branches,
  temporary variables, and runtime helper calls. External tools either
  flood users with false positives on generated code or require blanket
  suppressions that hide genuine defects.

### Selected architecture: Two-phase hybrid pipeline

The selected architecture adopts a two-phase hybrid design with strict
provenance boundaries:

1. **Source analysis stage**: Syntax, AST structure, naming,
   comments, and local type rules execute directly on author-written
   source ASTs before lowering. These analyzers run on both Go and `.lgo`
   files. Every finding attaches directly to user source tokens.
2. **Lowered semantic analysis stage**: Analyzers that
   require compiler facts (escape analysis, static allocation-site assertions,
   SSA-level nilness, and contract-driven lock state) execute on lowered
   representations. These analyzers must satisfy two invariants:
   - **Source position mapping**: Every lowered node carries a source map
     linking it back to its originating author statement.
   - **Generated-code ownership**: Synthetic nodes introduced by the
     compiler carry compiler-ownership metadata. User-facing syntax and
     style checks never evaluate synthetic nodes. Backend analyzers
     evaluate them only to verify semantic invariants (such as lock
     retention across synthetic branches).

## Implemented inventory

The current in-compiler engine contains exactly 21 registered rules in
`src/cmd/compile/internal/logos/lint`, plus one dedicated compiler invariant.
All implementation files cited below reside in that directory.

### Registered rules (21)

| Rule ID | Group | Scope | Default Severity | Implementation Source |
|---|---|---|---|---|
| `correctness::arena_pointer_escape` | correctness | Go & .lgo | forbid | `rule_arena_pointer_escape.go` |
| `correctness::infinite_recursion` | correctness | Go & .lgo | forbid | `rule_infinite_recursion.go` |
| `correctness::typed_nil_return` | correctness | Go & .lgo | forbid | `rule_typed_nil_return.go` |
| `correctness::unclosed_io_closer` | correctness | Go & .lgo | deny | `rule_unclosed_io_closer.go` |
| `correctness::unbounded_channel_op` | correctness | Go & .lgo | deny | `rule_unbounded_channel_op.go` |
| `correctness::nil_deref_unproven` | correctness | .lgo only | forbid | `nilderef.go`, `option_logos.go` |
| `correctness::defer_erases_errdefer_state` | correctness | .lgo only | forbid | `rule_defer_erases_errdefer_state.go` |
| `concurrency::concurrent_multiword_race` | concurrency | Go & .lgo | forbid | `rule_concurrent_multiword_race.go` |
| `concurrency::reentrant_withlock_deadlock` | concurrency | Go & .lgo | forbid | `rule_reentrant_withlock_deadlock.go` |
| `concurrency::unsupervised_goroutine` | concurrency | .lgo only | forbid | `rule_unsupervised_goroutine.go` |
| `slop::discarded_error` | slop | Go & .lgo | deny | `rule_discarded_error.go` |
| `slop::err_chain_reset` | slop | Go & .lgo | deny | `rule_err_chain_reset.go` |
| `slop::todo_placeholder` | slop | Go & .lgo | deny | `rule_todo_placeholder.go` |
| `complexity::cognitive_overflow` | complexity | Go & .lgo | deny | `rule_cognitive_overflow.go` |
| `perf::large_backing_array_pin` | perf | Go & .lgo | warn | `rule_large_backing_array_pin.go` |
| `perf::time_after_in_loop` | perf | Go & .lgo | warn | `rule_time_after_in_loop.go` |
| `suspicious::unapplied_struct_default` | suspicious | .lgo only | warn | `rule_unapplied_struct_default.go` |
| `suspicious::side_effect_before_sort` | suspicious | .lgo only | warn | `rule_side_effect_before_sort.go` |
| `portability::serialized_unsized_int` | portability | .lgo only | warn | `rule_serialized_unsized_int.go` |
| `portability::alias_extension` | portability | .lgo only | warn | `rule_alias_extension.go` |
| `pedantic::exhaustive_union_switch` | pedantic | .lgo only | allow | `rule_exhaustive_union_switch.go` |

The inventory comprises exactly 13 language-neutral rules (`Go & .lgo`)
and 8 `.lgo`-only rules, pinned by `TestRuleScopes`
(`src/cmd/compile/internal/logos/lint/lint_test.go:22-52`).

In Go packages, rule severity is capped: any rule configured or defaulting
to `forbid` lowers to `deny` at runtime so that inline `//logos:allow`
directives can silence findings (`policy.go:158-171`). In loGo/s packages,
`forbid` remains unlowerable and unsilenceable.

Two rules in the forbid group carry explicit `deny` default carve-outs:
`correctness::unclosed_io_closer` and `correctness::unbounded_channel_op`
(`rule_default_test.go:10-14`). In default policy (an absent or empty `[lints]`
table), `Policy.Level` evaluates `defaultLevel(id, g)` and honors `Rule.Default`,
leaving both rules at `deny` across both Go and .lgo packages so they stop
builds on violations while remaining suppressible via `//logos:allow`.

However, under the compiler policy implementation
(`src/cmd/compile/internal/logos/lint/policy.go:160-170`), configured
group severities take precedence over rule-level defaults:
`s, ok := p.groups[g]`. If a user explicitly specifies `correctness = "forbid"`
under `[lints]` in `logos.toml`, `p.groups["correctness"]` matches first,
bypassing `defaultLevel` and raising both `unclosed_io_closer` and
`unbounded_channel_op` from `deny` to `forbid` in `.lgo` packages (as
asserted in `rule_default_test.go:34-40`). In Go packages, the Go package
cap (`policy.go:167-169`) lowers the resulting `forbid` back to `deny`.

### Special compiler invariant

- `perf::zeroalloc_violation`: A static allocation-site assertion
  enforced directly by the escape analysis solver (`zeroalloc.go`,
  `zeroalloc_logos.go`). It is an unconfigurable, unsilenceable escape-analysis
  assertion. It accepts no `//logos:allow` directive and cannot be
  lowered by configuration. As specified in `SPEC.md:2044`, this directive
  is an assertion about the allocation sites that escape analysis decides,
  not a runtime-allocation proof that the function never invokes the
  runtime allocator. Runtime dynamic operations (such as slice growth
  beyond capacity via `append`, map bucket expansion, or string concatenation
  exceeding stack buffers) are dynamic operations outside escape solver scope.

## Proposed ruleset

The proposed design extends the engine with 26 targeted rules organized
into two functional families: standard correctness and hygiene (12 rules)
and contract-driven structural strictness (14 rules). These two families
execute across the two analysis pipeline stages (source frontend and
lowered semantics-preserving analyzers), not as a phased rollout.
Companion specifications for each rule's scope and test fixtures are
detailed in [LINT-RULES.md](LINT-RULES.md).

### Standard correctness and hygiene family (12 rules)

These rules expand the core correctness, portability, and hygiene
capabilities across both Go and loGo/s code.

| Rule ID | Group | Proposed Severity | Summary Contract |
|---|---|---|---|
| `correctness::printf_misuse` | correctness | forbid | Validates format string verbs against argument count and types, including `$"..."` string interpolations. |
| `correctness::unreachable_code` | correctness | forbid | Reports unreachable statements following return, panic, terminal `?` error exit, or terminal branches. Manual removal is default; automated deletion is not performed without whole semantic proof preserving comments, labels, and directives. |
| `correctness::lostcancel` | correctness | forbid | Detects discarded or uncalled cancellation functions returned by `context.WithCancel`, `WithTimeout`, or `WithDeadline`. Cancellation ownership transfer requires modeled contract; unmodeled transfers report incomplete proof. |
| `suspicious::copy_lock` | suspicious | warn | Flags copying values that embed lock types (`sync.Mutex`, `sync.RWMutex`) by assignment or pass-by-value. Migrated from proposed `correctness::copy_lock` so heuristic warn severity is not overridden by unlowerable correctness group forbid policy. Cloning before concurrency is legal. |
| `suspicious::boolean_contradiction` | suspicious | warn | Flags boolean expressions that evaluate to constant true or false (e.g. `x && !x`). Migrated from proposed `correctness::boolean_contradiction`. Distinguishes contradictions from redundancies (`x && x`) and preserves side-evaluating operands (`a && false` still evaluates `a`). |
| `suspicious::string_intconv` | suspicious | warn | Flags suspicious `string(int)` conversions where formatted decimal strings were likely intended. Migrated from proposed `correctness::string_intconv`. Defaults to warn because explicit rune conversion is legal Go. |
| `correctness::structtag_malformed` | correctness | forbid | Checks syntax of struct field tags against standard tag conventions (such as `json:"name,omitempty"`). |
| `correctness::stdmethods_shape` | correctness | forbid | Enforces standard method signatures on canonical interfaces (such as `Read`, `Write`, and `MarshalJSON`) when recognized interface implementation or annotation proves the obligation, avoiding bans on unrelated same-named methods. |
| `correctness::tests_malformed` | correctness | forbid | Flags malformed `Test*`, `Benchmark*`, or `Fuzz*` signatures in test files: both Go (`_test.go`) and loGo/s (`_test.lgo`); never general `*_go.go` production files. |
| `portability::linkname_discipline` | portability | deny | Validates `//go:linkname` directives against known target signatures instead of granting blanket directory exemptions. |
| `slop::empty_critical_section` | slop | warn | Flags lock acquisition immediately followed by release without protected operations. Defaults to warn because synchronization barriers and quiescence drains are legal. |
| `pedantic::unused_allow` | pedantic | allow | Flags `//logos:allow(...)` directives that matched no diagnostic in the enclosed scope. |

### Contract-driven and structural strictness family (14 rules)

These rules introduce contract-driven concurrency analysis, allocation
bounds, and strict structural rules.

| Rule ID | Group | Proposed Severity | Summary Contract |
|---|---|---|---|
| `concurrency::lock_state` | concurrency | forbid | Contract-driven lock verification checking that required locks are held, acquired, or released across control flow and call boundaries. |
| `concurrency::atomic_value` | concurrency | forbid | Requires atomic operations to target fields marked with `+checkatomic` or typed atomic wrappers, preventing unsynchronized mixed access. |
| `portability::integer_narrowing` | portability | warn | Flags conversions that truncate integer values across target architectures. `uintptr` and `uint` are pointer-width peers and are not flagged against each other. |
| `perf::timer_churn` | perf | warn | Target rule superseding implemented `perf::time_after_in_loop`. Flags repeated creation of `time.Tick` or `time.After` inside loops (including infinite loops). Emits one finding per site; existing compiler rule `time_after_in_loop` remains active until replacement. |
| `perf::allocation_contract` | perf | warn | Verifies explicit escape assertions (`+checkescape`), treating unknown dynamic and interface dispatches as soft escapes. |
| `style::receiver_consistency` | style | warn | Enforces consistent receiver variable names across all methods of a type. |
| `style::exported_needs_doc` | style | warn | Requires doc comments on exported types, functions, and package declarations. |
| `style::naming_convention` | style | warn | Enforces standard casing rules, rejecting underscores and capital acronyms outside recognized system definition lists. |
| `suspicious::defer_in_loop` | suspicious | warn | Flags `defer` inside loops where resource release is delayed until surrounding function return. Migrated from proposed `correctness::defer_in_loop` to preserve warn classification under group policy. |
| `suspicious::empty_branch` | suspicious | warn | Flags empty branch statements (`if`, `for`, `select`) lacking functional code or explanatory comments. |
| `suspicious::self_assignment` | suspicious | warn | Flags redundant self-assignments (`x = x`). |
| `suspicious::always_true_error_check` | suspicious | warn | Flags nil checks on errors where static type facts or control flow guarantee the error is always or never nil. |
| `perf::range_copy` | perf | warn | Flags value iteration in `for ... range` over slices or arrays of large structs. |
| `perf::huge_param` | perf | warn | Flags passing large structs by value rather than pointer or const reference. |

### Retired, mapped, and migrated rule IDs

The proposed ruleset incorporates explicit migration and supersession mappings
from earlier designs and active compiler rules:

| Original Draft / Active ID | Final Proposal Status | Rationale and Migration Path |
|---|---|---|
| `correctness::copy_lock` | Migrated to `suspicious::copy_lock` | `correctness` is an unlowerable forbid group (`forbidGroups`). Heuristic checks defaulting to `warn` must reside in a configurable group to prevent setting `correctness = "forbid"` from forcing them to `forbid`. Proposed rule only; no implemented rule renamed. |
| `correctness::boolean_contradiction` | Migrated to `suspicious::boolean_contradiction` | Migrated to `suspicious` to preserve heuristic `warn` severity under unlowerable forbid group constraints. Proposed rule only. |
| `correctness::string_intconv` | Migrated to `suspicious::string_intconv` | Migrated to `suspicious` to preserve heuristic `warn` severity. Proposed rule only. |
| `correctness::defer_in_loop` | Migrated to `suspicious::defer_in_loop` | Migrated to `suspicious` to preserve heuristic `warn` severity. Proposed rule only. |
| `perf::time_after_in_loop` (implemented) | Superseded by target `perf::timer_churn` | Active in compiler today; identity remains unchanged until offline lint engine lands `perf::timer_churn`. Once landed, `timer_churn` emits exactly one consolidated diagnostic per site covering both `time.Tick` and `time.After`. User suppressions migrate explicitly via `//logos:allow(perf::timer_churn)` (no silent aliasing). |
| `draft::time_tick_leak` | Replaced by `perf::timer_churn` | Subsumed into comprehensive timer churn analysis covering loops and goroutine lifecycles. |
| `style::comment_formatting` | Retired | Comment formatting is governed by `gofmt` and repository formatting tooling, not semantic lint rules. |
| `pedantic::exhaustive_switch` | Mapped to `pedantic::exhaustive_union_switch` | Open-ended enum switching cannot guarantee exhaustiveness without sealed union types; mapped to the existing union-switch implementation. |

## Confidence, heuristics, and legal patterns

Static analysis must not equate strictness with certainty. Strict
enforcement is reserved for high-confidence invariants that can be
directly verified from source structure, types, or explicit contracts.
Heuristic patterns are classified at lower severities or equipped with
explicit exemptions for legal idioms.

### High-confidence invariants

Strict enforcement (forbid or deny) is reserved for high-confidence
invariants verifiable from syntax structure, type definitions, interface
shapes, or explicit symbol tables without reliance on heuristic intent:

- `correctness::printf_misuse`: Verifiable from constant format verbs
  and argument types.
- `correctness::unreachable_code`: Verifiable from control flow graph
  dominance following unconditional exits (return, panic, terminal `?` error
  exit). Manual removal is default; automated deletion is rejected without
  whole semantic proof preserving comments, labels, and directives.
- `correctness::lostcancel`: Verifiable from discarded context cancel
  closures. Transferring cancel ownership across goroutines or data
  structures requires modeled contracts; unmodeled transfers report
  incomplete proof.
- `correctness::structtag_malformed`: Verifiable from struct tag syntax
  rules.
- `correctness::stdmethods_shape`: Verifiable from standard library
  interface signatures when recognized interface implementation or
  annotation proves the obligation, avoiding bans on unrelated same-named
  methods.
- `correctness::tests_malformed`: Verifiable from standard testing
  runner signatures in both Go (`_test.go`) and loGo/s (`_test.lgo`) test
  files (never general `*_go.go` production files).
- `portability::linkname_discipline`: Verifiable from explicit remote
  symbol signature tables.

### Heuristic patterns and legal exceptions

Several rules target patterns that have legitimate, legal uses in systems
software. Analyzers must recognize these patterns and report findings only
when specific guard conditions are violated:

1. **Mutex copying (`suspicious::copy_lock`)**:
   - *Legal pattern*: Embedding a value mutex in a struct that uses
     pointer-receiver methods (`func (s *T) Method()`) is canonical and
     idiomatic. Initializing a struct that embeds a zero-value mutex, or
     copying an unshared prototype value prior to concurrency, is valid.
   - *Report when*: A value containing an already-used mutex is copied by
     assignment or passed by copy across concurrent goroutine boundaries.
     Analyzers must distinguish used lock copies from zero-value construction.
2. **Empty critical sections (`slop::empty_critical_section`)**:
   - *Legal pattern*: Using a mutex lock and unlock pair without
     accessing protected state to wait for an active operation to exit
     (a quiescence drain), or as an explicit memory synchronization barrier.
   - *Legal pattern*: Concurrency testing and lock-order validation
     fixtures (such as lockdep tests) that exercise locking topologies
     without shared payload state.
   - *Report when*: Lock acquisition is immediately followed by release
     without protected state access and without designated barrier comments
     or suppressions.
3. **Integer string conversions (`suspicious::string_intconv`)**:
   - *Legal pattern*: Converting an integer rune code point to a UTF-8
     character string is valid Go when explicitly cast as a rune:
     `string(rune(codepoint))`.
   - *Report when*: An integer expression of non-rune type is converted via
     `string(v)` where formatted decimal output was intended.
   - *Note*: `int(string)` is syntactically a valid conversion expression
     `T(x)`, but is rejected by the type checker; strings cannot be
     converted to integers via type conversions (numeric parsing requires
     `strconv.Atoi`).
4. **Timer construction (`perf::timer_churn`)**:
   - *Legal pattern*: Top-level or once-per-process `time.Tick` or
     `time.After` channels in long-running listener loops.
   - *Report when*: Fresh timers or tickers are repeatedly instantiated
     inside loop bodies, including infinite loops (`for { ... }`) and
     per-request loops. Emits exactly one consolidated finding per churn
     site, superseding `perf::time_after_in_loop`.
   - *Design guard*: Do not state performance issues as correctness
     failures. Timer churn is an allocation and timer queue throughput
     hazard, not a memory safety defect. Unreferenced timer reclamation
     in Go 1.23+ requires module version `go >= 1.23` and the absence of
     `GODEBUG=asynctimerchan=1`; pre-1.23 modules retain unstopped timers
     in runtime queues until fired. Existing user suppressions must migrate
     explicitly to `perf::timer_churn`.
5. **Loop variable capture and per-iteration semantics**:
   - *Design guard*: Go 1.22+ per-iteration variable semantics apply
     strictly to new declarations (`for k, v := range ...` or
     `for i := 0; ...`). They do not protect assignment into preexisting
     variables declared outside the loop (`for i = 0; ...`), where
     closures continue to share a single variable instance across
     iterations.
6. **Defer in loops (`suspicious::defer_in_loop`)**:
   - *Legal pattern*: Short bounded loops that intentionally aggregate
     cleanup actions executed at function termination.
   - *Report when*: Deferred calls sit inside unbounded loops, streams, or
     collections where file descriptors or locks accumulate until the
     caller returns.
7. **Pointer-width integer conversions (`portability::integer_narrowing`)**:
   - *Legal pattern*: Converting between `uintptr` and `uint`. On all
     standard Go platforms (including 32-bit and 64-bit architectures),
     `uintptr` and `uint` are equal in bit width.
   - *Report when*: Converting `int64` or `uint64` to `int` or `uint`
     when targeting 32-bit architectures, or converting `int` to `int16`.
8. **Boolean contradictions and simplifications (`suspicious::boolean_contradiction`)**:
   - *Legal pattern*: Defensive checks, constant-specialized conditions
     (such as `debug && false` or build-tag specialization), and expressions
     with side-evaluating operands (`fn() && false` must still evaluate `fn()`).
     Redundant expressions (such as `x && x`) represent duplication rather
     than contradiction.
   - *Report when*: Direct structural contradictions on pure variables or
     operands (such as `x && !x`) occur in user-authored logic without side
     effects. The rule defaults to `warn` to accommodate defensive coding
     and constant specialization. Automated deletion is rejected because
     side-effecting operands cannot be safely removed.

## gVisor analyzer contracts and engineering principles

The proposed contract analyzers adapt designs from the Google gVisor
`nogo` framework (`.references/gvisor/`). We distinguish borrowable
engineering principles from gVisor project-specific suppressions.

### 1. Lock and atomic contracts (checklocks)

- **Source references**: `.references/gvisor/tools/checklocks/README.md`
  and implementation in `.references/gvisor/tools/checklocks/analysis.go`.
- **Borrowable principles**:
  - Contract annotations on struct fields: `+checklocks:mu` enforces that
    mutex `mu` is held during field reads and writes (`README.md:65-98`).
  - Read-write distinction: `sync.RWMutex` fields allow read locks for
    field access, but require exclusive write locks for modification.
  - Multi-lock disjunction: `+checklocksreadany` allows any one guard to
    satisfy read access while requiring all guards for writes
    (`README.md:104-124`).
  - Function preconditions and postconditions: `+checklocks:mu` (held on
    entry), `+checklocksacquire:mu` (acquired by exit),
    `+checklocksrelease:mu` (released by exit), and
    `+checklocksexclude:mu` (must not be held on entry; `README.md:167-200`).
  - Join point consensus: All control flow paths joining at basic block
    boundaries must hold an identical set of locks in identical modes.
    Function return paths must agree on held locks (`analysis.go:1001-1004`).
  - Atomic access pairing: `+checkatomic` requires fields to be accessed
    exclusively via atomic primitives or typed wrappers (`atomic.Int32`).
- **Dangerous overclaims avoided**:
  - *No claim of whole-program race freedom*: Checklocks is a
    contract-driven annotation checker with scoped interprocedural
    propagation across annotated function calls and inline closures
    (`README.md:239-245`). It possesses no general alias lifetime
    tracking (`README.md:126-131`), elides checks on newly allocated
    objects (`analysis.go:776-794`, `freshAlloc`), cannot track locks
    across uncalled closures passed to other functions
    (`README.md:246-263`), and ignores unannotated fields. The design
    specifies scoped lock discipline, not universal data-race freedom.
- **Project-specific suppressions (do not borrow)**:
  - Disabling checklocks on entire directories or CI workers via blanket
    exclusions (`.references/gvisor/nogo.yaml:126-130`).
  - Project-specific workarounds for lockdep test harnesses.

### 2. Escape analysis contracts (checkescape)

- **Source reference**: `.references/gvisor/tools/checkescape/checkescape.go`.
- **Borrowable principles**:
  - Explicit function-level escape assertions: `+checkescape` annotations
    specify verification requirements (`checkescape.go:17-31`).
  - Separation of reasons and scope: `checkescape` separates the escape
    reasons set from call traversal scope (`checkescape.go:836-851`).
    `:all` appends all escape reasons (including soft escapes like interface
    invocations and dynamic calls), while `:hard` selects only direct heap
    allocations and allocating builtins (`checkescape.go:153-165`).
  - Scope control: Scope defaults to inter-procedural analysis across
    calls; the `:local` modifier restricts analysis to the immediate
    function body. A bare `// +checkescape` directive defaults to
    `:local,hard` (`checkescape.go:41-42, 836-839`).
  - Unknown call handling: Operations whose concrete target cannot be
    resolved statically are treated as soft escapes (`unknownPackage`,
    `interfaceInvoke`, `dynamicCall`; `checkescape.go:1078-1096, 1398-1445`).
- **Dangerous overclaims avoided**:
  - *No claim of call-graph zero allocation by default*: A bare
    `// +checkescape` directive checks only local hard allocations.
    Call-graph analysis requires omitting `:local`, while verifying soft
    escapes requires `:all`. Call-graph verification with soft escape
    detection soundly reports escapes on any unresolved dynamic dispatch
    or unanalyzed package unless explicit boundary contracts exist.
- **Project-specific suppressions (do not borrow)**:
  - Silencing missing binary errors when analyzing external test packages
    (`.references/gvisor/nogo.yaml:232-237`).

### 3. Linkname signature discipline (checklinkname)

- **Source references**: `.references/gvisor/tools/checklinkname/README.md`,
  `.references/gvisor/tools/checklinkname/check_linkname.go`, and `.references/gvisor/nogo.yaml:238-257`.
- **Borrowable principles**:
  - Signature validation: Every `//go:linkname` directive is validated
    against a known type signature for the remote symbol. Mismatches
    between local and remote parameter lists or return types are reported
    as errors (`README.md:15-38`).
  - Eliminating directory allowlists: Code cannot bypass safety simply by
    residing in a runtime directory. Linknames are allowed only when
    their signatures match verified definitions.
- **Dangerous overclaims avoided**:
  - *No pointer type erasure*: gVisor's `checklinkname` flattens all
    pointer types (`*T`, `unsafe.Pointer`) to `"uintptr"`
    (`check_linkname.go:312, 358-360`) and truncates recursion depth at 4
    (`check_linkname.go:490`), allowing incompatible pointer types
    to pass unnoticed. In addition, zero-iteration loops in
    `makeStructSignature` and `makeInterfaceSignature`
    (`check_linkname.go:439-444, 449-455`) collapse all structs to
    `struct{}` and all interfaces to `interface{}`.
  - *Logos requires an independent contract*: gVisor's implementation
    cannot be relied upon for exact structural validation. Logos demands
    a correct, independent compiler analyzer that validates field layout,
    concrete types, and register ABI compatibility.
  - *No directory-based blanket exemptions*: Keep unsafe exclusions
    separate from linkname policy. Reject patterns like
    `.references/gvisor/nogo.yaml:198-229` that grant blanket exemptions
    to entire `*_unsafe.go` files.
- **Project-specific suppressions (do not borrow)**:
  - Suppressing specific historical runtime symbols
    (`runtime_pollWaitCanceled`, `runtime_procPin`, `registerMetric`)
    pending upstream Go issue resolutions (`.references/gvisor/nogo.yaml:238-257`).

### 4. Code classification and suppression policy (nogo.yaml)

- **Source reference**: `.references/gvisor/nogo.yaml`.
- **Borrowable principles**:
  - Path classification: Differentiating generated code, external
    dependencies, and internal repository code.
  - Style suppression on generated code: Style, formatting, and naming
    rules are suppressed on generated files, while security, concurrency,
    and memory correctness checks remain active.
- **Project-specific suppressions (do not borrow)**:
  - Global suppressions for Linux system definitions (allowing ALL_CAPS
    and underscores across internal code for ioctl and syscall numbers).
  - Disabling `shadow` across all packages (`.references/gvisor/nogo.yaml:131-135`).
  - Note on composites: in `.references/gvisor/nogo.yaml:136-140`,
    `composites: # Disable for now` reflects a known key/analyzer naming
    mismatch in gVisor's nogo configuration rather than a validated global
    analyzer suppression.
  - Blanket exclusions for controversial style rules (`QF1001`, `QF1008`).

## Policy configuration and suppression discipline

Configuration governs rule severity across packages without inventing
unimplemented configuration tables or runtime knobs.

### Grounded configuration model

Configuration is defined via the existing `logos.toml` `[lints]` table
schema (`src/cmd/go/internal/logos/config/lints.go`).

In the compiler, only keys present in `logos.toml` are stored in the policy
map (`Lints.Groups`). An absent `[lints]` table or an empty table means the
compiler evaluates defaults directly.

```toml
# Default-equivalent configuration: omitting group keys leaves
# all group severities and rule-level defaults at compiler defaults.
[lints]

[lints.thresholds]
max_nesting_depth = 3
max_cognitive_complexity = 15
max_function_lines = 60
max_function_params = 4
max_comptime_ast_nodes = 4096
max_ctfe_steps = 100000
```

#### Precedence and resolution order

Rule severity resolves through a strict four-step ladder implemented in
`src/cmd/compile/internal/logos/lint/policy.go:160-170`:

1. **Configured group severity** (`p.groups[g]`): Read from `[lints]` in
   `logos.toml`. If the group is present, its configured severity applies
   directly to all rules in that group.
2. **Rule-level default** (`Rule.Default` via `defaultLevel(id, g)`): If the
   group is absent from `p.groups`, `defaultLevel` checks whether the rule
   registers an explicit non-`allow` default. This preserves the `deny`
   carve-outs for `correctness::unclosed_io_closer` and
   `correctness::unbounded_channel_op`.
3. **Group default severity** (`DefaultSeverity(g)`): If the rule specifies
   no individual default, the group default applies (`forbid` for `correctness`
   and `concurrency`; `deny` for `slop` and `complexity`; `warn` for `perf`,
   `style`, `suspicious`, and `portability`; `allow` for `pedantic`).
4. **Go package cap** (`policy.go:167-169`): For Go packages (`KindGo`), any
   rule resolving to `forbid` is capped to `deny` so that inline `//logos:allow`
   directives can silence findings. In loGo/s packages (`KindLgo`), `forbid`
   is strictly enforced and unsilenceable.

#### Carve-out raising behavior under explicit group configuration

Specifying explicit group severities like `correctness = "forbid"` in
`logos.toml` is **not** equivalent to the default configuration.

```toml
# Explicit override example: explicitly configuring group severities.
# WARNING: Setting correctness = "forbid" populates p.groups["correctness"],
# which overrides Rule.Default and elevates the two Deny carve-outs
# (unclosed_io_closer and unbounded_channel_op) to forbid in .lgo packages.
[lints]
correctness = "forbid"
concurrency = "forbid"
slop        = "deny"
complexity  = "deny"
perf        = "warn"
style       = "warn"
suspicious  = "warn"
portability = "warn"
pedantic    = "allow"
```

Because `Policy.Level` checks `p.groups[g]` before calling `defaultLevel(id, g)`,
setting `correctness = "forbid"` populates `p.groups["correctness"]` and
bypasses `Rule.Default`. As verified in `rule_default_test.go:34-40`, this
elevates `correctness::unclosed_io_closer` and `correctness::unbounded_channel_op`
from `deny` to `forbid` in `.lgo` packages, converting suppressible deny
findings into unsuppressible build blockers. To preserve the default `deny`
carve-outs, projects should leave `correctness` omitted from `[lints]` or use
the minimal default-equivalent table.

- **Rejection of invented configuration**: Invented schemas such as
  `[lints.profile]` or `[lints.scope]` are rejected. No `build = "full"`
  or `budget = "report"` knobs exist.
- **Severity caps**: In Go packages, `forbid` rules are capped at `deny`
  so that inline directives can suppress false positives when needed. In
  loGo/s packages, `forbid` remains unlowerable and cannot be suppressed
  by inline directives.

### Suppression discipline

1. **Inline suppression**: The `//logos:allow(<group::name>)` comment
   placed immediately above the target statement is the sole inline
   suppression mechanism.
2. **Explicit rule naming**: Directives must name a valid registered rule
   identifier. Blanket suppressions (such as `//logos:allow(all)`) are
   rejected.
3. **Unused suppression detection**: Under the proposed `pedantic::unused_allow`
   rule, an allow directive that matches no finding is flagged, preventing
   stale suppressions from lingering.
4. **Zeroalloc cannot be suppressed**: `perf::zeroalloc_violation` represents
   a static allocation-site assertion and rejects all allow directives.

## golangci-lint for repository Go tooling

Repository Go tooling code (the compiler, driver, and internal build
utilities) is checked by `golangci-lint` using `logos-docs/golangci.yml`.

- **Current active configuration**: Uses standard default linters
  (`default: standard`): `errcheck`, `govet`, `ineffassign`, `staticcheck`,
  and `unused`.
- **Proposed curated extensions**: The proposed target configuration adds
  targeted tooling linters without importing unvetted profiles: `errorlint`,
  `unconvert`, `unparam`, `noctx`, `intrange`, `misspell`, `copyloopvar`,
  `gocheckcompilerdirectives`, `gochecknoinits`, `nolintlint`, and
  `govet nilness`.
- **Proposed target exclusions**: The proposed configuration records
  explicit rationale for leaving noisy linters disabled: `shadow` (inliner
  noise), `dupl`, `funlen`, `lll`, `QF1001` (De Morgan rewrites), and
  `QF1008` (embedded field selectors). Neither current nor proposed
  configurations include unverified dependency bans or depguard rules.

## Verification plan

Implementation of proposed rules proceeds under strict verification:

1. **Dual attack fixtures**: Each proposed rule lands with an `.lgo`
   attack fixture and a corresponding Go fixture verifying identical
   finding behavior across both language frontends.
2. **Registry assertions**: Unit tests in `src/cmd/compile/internal/logos/lint`
   assert exact registered rule counts and verify severity mappings.
3. **Lowered mapping tests**: Rules that evaluate lowered representations
   are verified by test fixtures ensuring diagnostics report the exact
   line and column of the user source statement, with zero leakage of
   compiler-generated node positions.
