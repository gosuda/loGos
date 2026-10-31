# M21 logos lsp

Normative sources: `logos-docs/SPEC.md` Sec 21,
`logos-docs/IMPLEMENTATION.md` M21-S1 to S2. This page closes the M21 P1-S3
docs row. It describes the behavior the tree implements; parent runs the
gates below to confirm the checkout.

## What the mechanism is

`logos lsp` is a stdio JSON-RPC proxy in `cmd/go/internal/logos/lsp` to
`gopls -mode=stdio`:

- Overlay directory holds lowered files produced by compile mode
  `-logos.eject` for open packages, plus a position map built from the
  origin segments of the version 2 edit map (a lowered offset inside a copied
  segment maps to its exact original offset; offsets inside compiler
  generated runs have no `.lgo` position of their own). Requests have URIs
  and positions rewritten; compiler JSON diagnostics (M9) merge into
  `publishDiagnostics`. The overlay holds lowered `.lgo` files only; Go
  packages stay on disk for `gopls`.
- Cross kind resolution uses each package own kind (R-U2): the overlay also
  holds lowered files of every loGo/s package that an open package imports,
  and a Go package resolves through `gopls` on its real `.go` files.
- Hints mode `-logos.mode=hints` writes JSON hints (narrowing sites such as
  `narrowed: *User`, union members at switches, allocation tier badges
  `stack`, `arena`, `heap`, `rodata` from escape analysis through the `-m`
  machinery) for `.go` and `.lgo` files; the proxy serves them for `inlayHint`
  and `hover`. The hints JSON carries `fixedBytes` per interpolation site;
  hover on `$"..."` renders `Fixed: <n>B + dynamic`. Hover on `?`,
  `const for`, `const switch T`, and `$"..."` shows the lowered code. Code
  actions apply M9 autofix candidates. Missing `gopls` reports
  `logos lsp: gopls not found on PATH`.

## How to enable or observe it

- Run `logos lsp` from a workspace with `gopls` on `PATH`; editors point at
  it as the language server. Edits from the client to an open `.lgo` file
  update the overlay on the next request.
- Observe with `cmd/go/internal/logos/lsp/lsp_test.go` with a fake gopls
  helper answering from a script (`hover`, `definition`, `references`,
  `completion`, `publishDiagnostics`, `codeAction`, `inlayHint` positions in
  `.lgo` files map to the lowered overlay and back including positions inside
  a `?` statement spanning three lines; mixed workspace definition across
  kinds lands in the owning file).
- Parent to run: unit tests plus script test `logos_lsp.txt` guarded by
  `[exec:gopls]`; when that guard skips execution, record the gate as blocked
  and incomplete, never as passed.

## Budgets and defaults

- No size or latency numbers are gates for the proxy; the M10 context 100
  line cap applies to the context path, not to LSP responses.
- Positions inside copied segments map exactly; generated runs intentionally
  map to nothing.

## Known limits

- `gopls` must be installed; the proxy does not vendor it.
- `definition` on an import of the other kind resolves through that package
  own kind with no disk emission for loGo/s (overlay only).
