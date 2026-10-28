# loGo/s

Go with compiler-checked error handling, closed variants, and clean ejection to standard Go.

---

loGos provides a single toolchain for standard Go and the extended loGo/s language. Authoring in `.lgo` files adds compiler-enforced nil safety, compile-time field unrolling, read-only views, and structured concurrency without giving up Go idioms or runtime compatibility. The `logos` compiler checks these constructs, lowers them to standard Go code, and compiles them using a Go 1.27 backend, while plain `go` keeps upstream Go language semantics and command text (see [README.go.md](README.go.md)). When standard Go source is required, `logos eject` converts `.lgo` projects into standard Go modules with original comments preserved.

Why the extra syntax:
- **Compile-time unrolling**: inspect struct fields and branch on types with `comptime` without run-time field lookups or heap allocations.
- **Read-only views**: share slices and pointers without data copies while preventing writes through that name.
- **Static nil safety**: ensure locally declared pointers are non-nil and make missing values explicit with `T?`.
- **Structured concurrency**: bound goroutines in a `go group` that coordinates cancellation, limits, and joins.
- **Type-directed dispatch**: specialize generic functions with `const switch T` without writing reflection by hand.
- **Closed unions**: enforce exhaustive variant matching across sum types at compile time.

* **Backend**: Go 1.27 (`src/go.mod` specifies `module std` with `go 1.27`).
* **Bootstrap toolchain**: Go 1.24.6 or newer (`src/make.bash` requires `bootgo=1.24.6`; source build verified with upstream Go 1.27.3 on Linux/amd64).
* **File extensions**: `.lgo` for loGo/s source packages; `.go` for plain Go packages.
* **Module boundaries**: One language per module; Go and loGo/s modules interoperate across imports.
* **Ejection**: `logos eject [-o DIR]` emits an upstream-compatible Go 1.27 module tree.
* **Diagnostics**: `logos lint` provides in-compiler analysis for main-module packages without building binaries.
* **License**: BSD-3-Clause (see [LICENSE](LICENSE)).

Contents: [What is this](#what-is-this) · [Install](#install) · [Quick start](#quick-start) · [Features](#features) · [Usage](#usage) · [Agent skills](#agent-skills) · [Development](#development) · [License](#license)

## What is this

```go
package main
import "fmt"
import "strconv"
func double() (int, error) {
	n := strconv.Atoi("21")?
	return 2 * n, nil
}
func main() { fmt.Println(double()) } // 42 <nil>
```

In `double()`, the `?` operator tests the error result of `strconv.Atoi("21")`. If `Atoi` returns a non-nil error, `double()` returns early with that error. When the call succeeds, `n` receives the integer value. Running `logos run .` prints `42 <nil>`.

The full language definition is in [logos-docs/SPEC.md](logos-docs/SPEC.md).

## Install

Building loGos from source requires Git, Bash, and a working bootstrap Go installation of version 1.24.6 or newer (see [src/make.bash](src/make.bash)).

1. Clone the repository:

   ```sh
   git clone https://github.com/gosuda/loGos.git
   cd loGos
   ```

2. Capture your clean host bootstrap Go root and build the toolchain from source:

   ```sh
   unset GOROOT GOTOOLCHAIN
   upstream_go=$(command -v go)
   bootstrap_root=$("$upstream_go" env GOROOT)
   (cd src && GOROOT_BOOTSTRAP="$bootstrap_root" ./make.bash)
   ```

   If your bootstrap Go 1.24.6+ binary is not on your `PATH`, specify `GOROOT_BOOTSTRAP=/path/to/go1.24.6` directly.

3. Configure your environment from the repository root:

   ```sh
   export GOROOT="$PWD"
   export PATH="$GOROOT/bin:$PATH"
   export GOTOOLCHAIN=local
   ```

4. Verify the installed binaries:

   ```sh
   logos version
   go version
   ```

   `logos version` reports version details (for example, `logos version go1.27.2 linux/amd64` on Linux/amd64). `go version` reports `go version go1.27.2 linux/amd64`.

## Quick start

Create a fresh module outside the toolchain tree:

```sh
mkdir ~/logos-hello && cd ~/logos-hello
logos mod init example.com/hello
cat > main.lgo <<'EOF'
package main

import (
	"fmt"
	"strconv"
)

func double() (int, error) {
	n := strconv.Atoi("21")?
	return 2 * n, nil
}

func main() {
	fmt.Println(double())
}
EOF
logos run .
```

Output:

```
42 <nil>
```

### Eject to standard Go

The `logos eject` command converts a loGo/s module into standard Go code that builds with any upstream Go 1.27 compiler:

```sh
logos eject -o ../logos-hello-go
cd ../logos-hello-go
```

Use the same shell as the installation steps, which saved the upstream executable in `upstream_go`.
In a new shell, set `upstream_go` to the absolute path of your upstream Go executable.
If you used Go 1.24 through 1.26 for bootstrap, select an upstream Go 1.27 or newer installation instead.
Check its version before running the ejected module.

```sh
: "${upstream_go:?Set upstream_go to an upstream Go 1.27+ executable}"
env -u GOROOT -u GOTOOLCHAIN "$upstream_go" version
env -u GOROOT -u GOTOOLCHAIN "$upstream_go" run .
```

Output:

```
42 <nil>
```

Both invocations print `42 <nil>`. The ejected module is pure Go.

`logos eject [-o DIR]` requires that the target directory `DIR` either does not exist or is empty. Ejection translates `.lgo` sources into standard `.go` files, preserves source comments, places dependency modules under `DIR/_logos_deps`, and writes runtime support under `DIR/_logos_deps/rt` as module `github.com/gosuda/loGos/rt`. Upstream Go 1.27 or newer is required to compile the ejected output.

## Features

### Modules and interoperability

loGos enforces a single language per module:

- A module contains either `.lgo` packages or `.go` packages, never a mixture of both in the same module.
- Packages interoperate across module boundaries through standard Go `import` paths:
  - A `.lgo` package can import standard library packages and external Go modules. Calls returning `(T, error)` bind directly to `?`.
  - A `.go` package can import a `.lgo` package. Exported functions and types are projected into standard Go signatures, with option types and closed unions represented as canonical Go types.
- Property interfaces require an explicit assertion on the implementing struct declaration (`type S struct : I`), not automatic structural subtyping.

### Comptime

`const for` unrolls loop bodies across struct fields during compilation, and `val.[f]` reads or writes each field directly without run-time field lookup. Field metadata such as `f.Name` and `f.Tag.Get` folds into string constants with no interface boxing. The compiler produces specialized code for each struct type.

`main.lgo`:

```go
package main

import (
	"comptime"
	"fmt"
)

type Server struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func main() {
	s := Server{Host: "db", Port: 5432}
	var copy Server
	const for _, f := range comptime.Fields[Server]() {
		fmt.Println(f.Name, f.Tag.Get("json"))
		copy.[f] = s.[f]
	}
	fmt.Println(copy)
}
```

Output:

```
Host host
Port port
{db 5432}
```

Comptime inspection operates during compilation only; referencing comptime APIs at run time produces a compile error. Loop expansion accepts only `comptime.Fields[T]()` over a struct type, visits fields in declaration order, skips unexported fields from other packages, and defaults to a 4,096-node expansion limit.

### Read-only views

A `const` view allows functions to read slices and pointers without defensive copies while the compiler prevents writes through that name. Converting `string` to `const []byte` performs no allocation; converting back is zero-copy when the bytes originated from a string or constant. The qualifier is checked during compilation and erased before code generation.

`main.lgo`:

```go
package main

import (
	"fmt"
	"logos/rt"
)

type Config struct{ Port int }

func total(xs const []int) (t int) {
	for _, x := range xs {
		t += x
	}
	return
}

func main() {
	xs := []int{1, 2, 3}
	var view const []int = xs
	fmt.Println(total(view))
	xs[0] = 10
	fmt.Println(total(view))

	cfg := &Config{Port: 8080}
	snap := rt.Freeze(cfg)
	cfg.Port = 9090
	fmt.Println(snap.Port, cfg.Port)
}
```

Output:

```
6
15
8080 9090
```

A view is an alias to existing memory rather than an isolated snapshot, so modifications made through the original mutable holder remain visible through the view. When true isolation is required, `logos/rt.Freeze` deep-copies the value and returns a protected view at the cost of heap allocations.

### Nil safety

Locally declared pointers written `*T` cannot be nil, eliminating defensive nil checks across local application code. Potential absence is typed explicitly as `T?`, requiring safe navigation (`?.`), coalescing (`??`), or nil guards before the compiler permits dereferencing. Field access on unnarrowed optional pointers is rejected at compile time.

`main.lgo`:

```go
package main

import "fmt"

type User struct{ Name string }

func find(name string) *User? {
	if name == "ada" {
		return &User{Name: "Ada"}
	}
	return nil
}

func describe(u *User?) string {
	if u == nil {
		return "unknown"
	}
	return u.Name
}

func main() {
	ada := find("ada")
	bob := find("bob")
	fmt.Println(ada?.Name ?? "anonymous")
	fmt.Println(bob?.Name ?? "anonymous")
	fmt.Println(describe(ada))
	fmt.Println(describe(bob))
}
```

Output:

```
Ada
anonymous
Ada
unknown
```

Pointers crossing the boundary from plain Go packages are platform pointers that may be nil. When this toolchain compiles the Go caller, a direct call that passes a nil pointer into a non-nil loGo/s parameter gets a run-time check via `logos/rt.MustNonNil` rather than failing the build. Calls through function values, method values, or interfaces are a documented exception.

### Structured concurrency

A `go group` binds concurrent tasks to a lexical block where all child goroutines are joined before exit, cancellation propagates automatically on failure, and concurrency is capped by `limit`. Paired with `logos/sync.Locked`, data and mutex travel together so shared fields cannot be accessed unlocked. Unsupervised goroutines outside a group are rejected at compile time.

`main.lgo`:

```go
package main

import (
	"context"
	"fmt"
	"logos/sync"
)

type Counter struct{ Total int }

func run(ctx context.Context) error {
	var c sync.Locked[Counter]
	go group (ctx, limit = 2) {
		for _, v := range []int{10, 20, 30} {
			go {
				c.WithLock(t => { t.Total += v })
			}
		}
	}
	var res int
	c.WithLock(t => { res = t.Total })
	fmt.Println("sum:", res)
	return nil
}

func main() {
	if err := run(context.Background()); err != nil {
		panic(err)
	}
}
```

Output:

```
sum: 60
```

Structured concurrency does not provide universal data-race freedom; concurrent writes to shared storage still require locks, channels, or atomic values. Cancellation unblocks plain channel operations written directly in a child and wakes netpoll-backed network I/O, but blocking operations like `time.Sleep`, regular file I/O, and cgo calls cannot be interrupted and must run until completion before the group joins.

### Generic type switches

`const switch T` dispatches on the type argument of a generic function, so one function holds specialized paths without user-written reflection. Matching an exact type rebinds the value to that type, `~basic` matches underlying types while preserving the named type, and interface cases expose the interface method set. Unmatched branches cost nothing when the type tag is concrete.

`main.lgo`:

```go
package main

import "fmt"

type UserID int
type Word struct{ S string }
func (w Word) String() string { return w.S }

func Label[T any](v T) string {
	const switch T {
	case int:
		return fmt.Sprintf("int:%d", v+1)
	case ~int:
		return fmt.Sprintf("userid:%v", v)
	case fmt.Stringer:
		return "stringer:" + v.String()
	}
	return "other"
}

func main() {
	fmt.Println(Label(41))
	fmt.Println(Label(UserID(7)))
	fmt.Println(Label(Word{"hi"}))
	fmt.Println(Label(1.5))
}
```

Output:

```
int:42
userid:7
stringer:hi
other
```

`const switch` is a statement rather than an expression and does not permit a `default` arm or multiple types per case; unhandled types fall through to statements following the block. Compile-time branch folding occurs only for concrete type tags and comptime generics, while standard generic functions evaluate the switch at run time.

### Closed unions and property interfaces

Closed unions define a fixed set of variants as an interface value type, allowing exhaustive type switch expressions without silent fallthrough. Property interfaces provide field-style contracts where structs implement accessors only through an explicit declaration assertion (`: I`). The compiler validates that all cases and interface properties match at compile time.

`main.lgo`:

```go
package main

import "fmt"

type Value interface{ int | string }

func tag(v Value) string {
	return switch v.(type) {
	case int => "int"
	case string => "str"
	case nil => "nil"
	}
}

type Creature interface { const Name string; HP int }

type Monster struct : Creature {
	Name string
	HP   int
}

func main() {
	m := Monster{Name: "gorg", HP: 100}
	var c Creature = &m
	c.HP -= 10
	var v Value = 7
	fmt.Println(tag(v), tag(nil), c.Name, m.HP)
}
```

Output:

```
int nil gorg 90
```

Closed unions cannot be extended outside their declaring package and use Go's standard interface representation at run time rather than a tagged-union layout. Property interfaces require pointers when writing to properties, and type switch expressions over unions must explicitly handle every member plus `nil` unless a `default` case is provided.

### Named tuples

Multi-value function results can be packed into a single first-class tuple value without declaring a standalone struct type. Tuple values can be passed as arguments, stored in channels and slices, accessed by field names (`t.host`), or unpacked via destructuring assignments. This removes the boilerplate of temporary struct declarations for internal multi-value records.

`main.lgo`:

```go
package main

import "fmt"

func resolve(service string) (host string, port int) {
	if service == "api" {
		return "api.example.com", 443
	}
	return "localhost", 8080
}

func url(e (host string, port int)) string {
	return fmt.Sprintf("http://%s:%d", e.host, e.port)
}

func main() {
	api := resolve("api")
	fmt.Println(api.host, api.port)
	fmt.Println(url(api))

	host, port := api
	fmt.Println(host, port)

	queue := make(chan (host string, port int), 1)
	queue <- api
	next := <-queue
	fmt.Println(next.host, next.port)
}
```

Output:

```
api.example.com 443
http://api.example.com:443
api.example.com 443
api.example.com 443
```

Field names are defined by the tuple type or function signature rather than in literal expressions, and positional indexing like `t.0` is rejected by the compiler. Under the hood, tuples lower to generated structs, and printing a tuple value directly displays the underlying struct representation.

### Element-wise array math

Small fixed-size numeric arrays support arithmetic operators (`+`, `-`, `*`, `/`, `&`, `|`, `^`) and reductions (`.sum()`, `.min()`, `.max()`, `.dot()`) directly without manual loops; the operators allocate nothing. Reductions use a language-specified pairwise evaluation order so floating-point results remain deterministic across supported vector lowerings and scalar fallback. The compiler eliminates manual vectorization loops while guaranteeing identical numeric results across amd64, arm64, and scalar lowerings.

`main.lgo`:

```go
package main

import "fmt"

func main() {
	a := [4]float64{1, 2, 3, 4}
	b := [4]float64{5, 6, 7, 8}

	add := a + b
	mul := a * b
	fmt.Println("add", add)
	fmt.Println("mul", mul)
	fmt.Println("sum", a.sum(), "min", a.min(), "max", a.max(), "dot", a.dot(b))

	p := [4]int32{17, -19, 100, 7}
	q := [4]int32{5, 4, -3, 2}
	fmt.Println("div", p/q)
	fmt.Println("intsum", p.sum(), "intdot", p.dot(q))
}
```

Output:

```
add [6 8 10 12]
mul [5 12 21 32]
sum 10 min 1 max 4 dot 70
div [3 -4 -33 3]
intsum 105 intdot -277
```

Element-wise math is supported on arrays only, not slices, and requires a power-of-two element count whose total size does not exceed 64 bytes. Vectorized execution requires `GOEXPERIMENT=simd` on supported architectures and instruction sets; otherwise the compiler falls back to unrolled scalar instructions with identical numeric results and no throughput guarantee.

### Also in the language

The language also provides these concise extensions defined in [logos-docs/SPEC.md](logos-docs/SPEC.md):

- **Error propagation** (`n := f()?` or `f()?`): unwraps values or returns early on non-nil error from functions returning `(T, error)` ([logos-docs/SPEC.md](logos-docs/SPEC.md) Sec 7.1).
- **Error cleanup** (`errdefer f()`): registers deferred cleanup executed only when the enclosing function exits with an error, a panic, or Goexit ([logos-docs/SPEC.md](logos-docs/SPEC.md) Sec 7.1).
- **String interpolation** (`$"user {name}: {count}"`): embeds formatted expressions directly within string literals ([logos-docs/SPEC.md](logos-docs/SPEC.md) Sec 2).
- **Iterator chains** (`xs.filter(...).map(...).take(n)`): runs functional slice transformations with predicate and mapping closures ([logos-docs/SPEC.md](logos-docs/SPEC.md) Sec 2).
- **Literal addressing** (`&100`, `&f()`): takes the address of scalar literals and function call returns without temporary variables ([logos-docs/SPEC.md](logos-docs/SPEC.md) Sec 2).
- **Struct field defaults** (`type Config struct { Port int = 8080 }`): evaluates default field values when struct literals omit fields ([logos-docs/SPEC.md](logos-docs/SPEC.md) Sec 2).

## Usage

Day to day, `logos` mirrors the `go` command: `logos run`, `logos build`, and `logos test` work on loGo/s modules the way their `go` counterparts work on Go modules. Two commands are specific to loGo/s. `logos eject [-o DIR]` writes the module as standard Go (see [Eject to standard Go](#eject-to-standard-go)). `logos lint` runs the in-compiler checks below.

### Lint

The `logos lint` command runs in-compiler checks through escape analysis and stops before code generation. It emits diagnostics without writing executable binaries:

```sh
logos lint [build flags] [--pedantic] [--json-diagnostics] [packages]
```

#### Scope and rules

- **Target scope**: `logos lint` inspects packages within the main module only. The standard library and external module dependencies are never linted.
- **Language checks**:
  - In `.lgo` packages, the compiler runs all registered compiler rules.
  - In standard `.go` packages, the compiler runs language-neutral rules only.
- **Directives**:
  - `//logos:allow(<rule>)`: Placed directly above a declaration or statement to suppress a `deny` or `warn` diagnostic. In Go packages, `forbid` rules run at `deny` severity and can be silenced by this directive; in `.lgo` packages, `forbid` rules cannot be silenced.
  - `//logos:zeroalloc`: Placed above a function declaration as a static escape-analysis site contract requiring the compiler to reject local heap allocations at that site (see [logos-docs/LINT-RULES.md](logos-docs/LINT-RULES.md)).
- **Flags**:
  - `--pedantic`: Enables pedantic diagnostics overriding module configuration.
  - `--json-diagnostics`: Emits one JSON object per diagnostic line on standard error.

For the normative rule set design and individual rule definitions, see:
- [logos-docs/LINT-RULESET.md](logos-docs/LINT-RULESET.md): Proposed normative lint design for Go and loGo/s.
- [logos-docs/LINT-RULES.md](logos-docs/LINT-RULES.md): Detailed reference catalog of in-compiler rules.

## Agent skills

The [plugins/logos/](plugins/logos/) directory packages skills for coding assistants:

- [`logos`](plugins/logos/skills/logos/SKILL.md): Author, build, test, and eject loGo/s modules.
- [`logos-review`](plugins/logos/skills/logos-review/SKILL.md): Review `.lgo` code against compiler rules, diagnostics, and language invariants.
- [`logos-from-go`](plugins/logos/skills/logos-from-go/SKILL.md): Guide migration of existing plain Go packages to loGo/s syntax.
- [`logos-performance`](plugins/logos/skills/logos-performance/SKILL.md): Assist with zero-copy views and cgo entry points.

To provide coding agents with project conventions and compiler constraints, copy [plugins/logos/templates/AGENTS.md](plugins/logos/templates/AGENTS.md) into the target repository root. See [plugins/logos/](plugins/logos/) for plugin structure and usage documentation.

## Development

Follow the [Install](#install) instructions to compile the toolchain from source.
Run the commands below from the toolchain repository root, not the example module.

### Running tests

Each command enters `src` before it runs the tests:

```sh
(cd src && go test -count=1 cmd/internal/testdir -run 'Test/logos/')
(cd src && go test -count=1 -short cmd/compile/internal/types2 -run TestLogos)
(cd src && go test -count=1 cmd/go -run 'Script/^logos_')
```

### Contributing

1. Fork the repository on GitHub.
2. Create a feature branch (`git checkout -b feature/my-feature`).
3. Add test fixtures under `test/logos/run/` or `src/internal/types/testdata/logos/` covering new syntax or compiler behavior.
4. Verify that targeted tests pass before submitting changes.
5. Commit your changes with concise, descriptive commit messages adhering to standard Go conventions.
6. Open a Pull Request on GitHub.

See [CONTRIBUTING.md](CONTRIBUTING.md) and [README.go.md](README.go.md) for contribution guidelines.

## License

Standard Go source code is licensed under the BSD-style license in [LICENSE](LICENSE). loGo/s additions and toolchain modifications are distributed under the same license terms.
