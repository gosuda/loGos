# loGo/s

Go with compiler-checked error handling, closed variants, and clean ejection to standard Go.

---

loGos provides a single toolchain for Go and loGo/s. loGo/s extends Go with `?` error propagation, `errdefer` cleanup, closed union types, option types, and immutable views. Programs are authored in `.lgo` source files. The `logos` command type-checks these constructs, lowers them to standard Go code, and compiles them using a Go 1.27 backend. Standard `go` keeps upstream Go semantics and command behavior (see [README.go.md](README.go.md)). When pure Go is needed, `logos eject` generates a complete standard Go module with original comments preserved.

* **Backend**: Go 1.27 (`src/go.mod` specifies `module std` with `go 1.27`).
* **Bootstrap toolchain**: Go 1.24.6 or newer (`src/make.bash` requires `bootgo=1.24.6`; source build verified with upstream Go 1.27.3 on Linux/amd64).
* **File extensions**: `.lgo` for loGo/s source packages; `.go` for plain Go packages.
* **Module boundaries**: One language per module; Go and loGo/s modules interoperate across imports.
* **Ejection**: `logos eject [-o DIR]` emits an upstream-compatible Go 1.27 module tree.
* **Diagnostics**: `logos lint` provides in-compiler analysis for main-module packages without building binaries.
* **License**: BSD-3-Clause (see [LICENSE](LICENSE)).

Contents: [What is this](#what-is-this) · [Install](#install) · [Quick start](#quick-start) · [Language](#language) · [Lint](#lint) · [Agent skills](#agent-skills) · [Development](#development) · [License](#license)

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

## Language

### Modules and interoperability

loGos enforces a single language per module:

- A module contains either `.lgo` packages or `.go` packages, never a mixture of both in the same module.
- Packages interoperate across module boundaries through standard Go `import` paths:
  - A `.lgo` package can import standard library packages and external Go modules. Calls returning `(T, error)` bind directly to `?`.
  - A `.go` package can import a `.lgo` package. Exported functions and types are projected into standard Go signatures, with option types and closed unions represented as canonical Go types.
- Property interfaces require an explicit assertion on the implementing struct declaration (`type S struct : I`), not automatic structural subtyping.

### Feature summary

The table below summarizes language features defined in [logos-docs/SPEC.md](logos-docs/SPEC.md) alongside test fixtures in the repository:

| Feature | Syntax / Form | Description | Specification | Fixture Reference |
| :--- | :--- | :--- | :--- | :--- |
| Error propagation | `n := f()?`<br>`f()?` | Unwraps value results or returns early on non-nil error from functions returning `(T, error)` | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 7.1) | [`test/logos/example/order_service.lgo`](test/logos/example/order_service.lgo)<br>[`src/cmd/go/testdata/script/logos_eject_example.txt`](src/cmd/go/testdata/script/logos_eject_example.txt) |
| Error cleanup | `errdefer f()`<br>`errdefer f()?` | Registers deferred cleanup executed only when the function exits with a non-nil error, panic, or Goexit | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 7.1) | [`test/logos/run/errdefer_run.lgo`](test/logos/run/errdefer_run.lgo) |
| Closed unions | `type U interface { int \| string }` | Exhaustive sum types bounded to declared member types with exhaustive type-switch checking | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/union_basic.lgo`](test/logos/run/union_basic.lgo)<br>[`src/internal/types/testdata/logos/union_members.lgo`](src/internal/types/testdata/logos/union_members.lgo) |
| Option types | `var x int?`<br>`u?.Name`<br>`x ?? def` | Optional value handling with safe navigation (`?.`) and fallback coalescing (`??`) | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/option_chain.lgo`](test/logos/run/option_chain.lgo)<br>[`src/internal/types/testdata/logos/option_types.lgo`](src/internal/types/testdata/logos/option_types.lgo) |
| Property interfaces | `type I interface { const ID int; Name string }` | Structural field contracts; structs implement them only through explicit declaration (`: I`) | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/properties.lgo`](test/logos/run/properties.lgo)<br>[`src/internal/types/testdata/logos/property_iface.lgo`](src/internal/types/testdata/logos/property_iface.lgo) |
| Const views | `const []byte(s)`<br>`rt.Freeze(&v)` | Read-only slice views and deep immutability helpers for byte and string data | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/zerocopy.lgo`](test/logos/run/zerocopy.lgo)<br>[`test/logos/run/const_views.lgo`](test/logos/run/const_views.lgo) |
| String interpolation | `$"user {name}: {count}"` | Formatted string literals embedding expressions within `{}` | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/interp.lgo`](test/logos/run/interp.lgo)<br>[`src/internal/types/testdata/logos/interp.lgo`](src/internal/types/testdata/logos/interp.lgo) |
| Iterator chains | `xs.filter(...).map(...).take(n)` | Sequence operations over slices with predicate and mapping closures | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/chains.lgo`](test/logos/run/chains.lgo)<br>[`src/internal/types/testdata/logos/chains.lgo`](src/internal/types/testdata/logos/chains.lgo) |
| Literal addressing | `&100`, `&f()` | Takes the address of scalar literals and function call results directly | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/addr_lit.lgo`](test/logos/run/addr_lit.lgo)<br>[`src/internal/types/testdata/logos/addr_lit.lgo`](src/internal/types/testdata/logos/addr_lit.lgo) |
| Struct field defaults | `type Config struct { Port int = 8080 }` | Default field expressions evaluated when struct literals omit fields | [logos-docs/SPEC.md](logos-docs/SPEC.md) (Sec 2) | [`test/logos/run/struct_default.lgo`](test/logos/run/struct_default.lgo)<br>[`src/internal/types/testdata/logos/struct_default.lgo`](src/internal/types/testdata/logos/struct_default.lgo) |

## Lint

The `logos lint` command runs in-compiler checks through escape analysis and stops before code generation. It emits diagnostics without writing executable binaries:

```sh
logos lint [build flags] [--pedantic] [--json-diagnostics] [packages]
```

### Scope and rules

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
