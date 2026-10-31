# logos-nogo

Helper module `logos/nogo` behind the Logos nogo checks. Its dependencies
(`golang.org/x/mod`, `golang.org/x/tools`, `honnef.co/go/tools` and their
indirect requirements) are pinned in `go.mod` and `go.sum`.

## Source build (`make.bash`, `make.bat`, `make.rc`)

`cmd/dist` builds the helper from source with the just-built `$GOROOT/bin/go`
and installs it with a source digest under `pkg/tool/<GOOS>_<GOARCH>/`.
The helper is the only step of the bootstrap that needs modules:

- Toolchain1/2/3 and `std`/`cmd` stay isolated and offline (`GOENV=off`,
  `GOPATH` inside `$GOROOT`, `GOPROXY=off`).
- The helper build restores your module policy as the environment that
  started `make` defines it: `GOMODCACHE`, `GOPROXY`, `GOSUMDB`, `GONOSUMDB`,
  `GOPRIVATE`, `GONOPROXY`, `GOINSECURE`, `GOVCS` and `GOAUTH`, resolved by
  `go env` (environment variables, then the `go env -w` file unless
  `GOENV=off`, then defaults). `make.bat` and `make.rc` always start dist
  with `GOENV=off` (through `dist env`), and `make.bash` does when `GOENV` is
  exported. There the `go env -w` file is ignored: set the policy as
  environment variables instead. dist prints a note when this happens.
- The build runs with `-mod=readonly`, `GOWORK=off` and `GOTOOLCHAIN=local`.
  `go.mod` and `go.sum` are never rewritten; modules are verified against
  `go.sum`.

Prerequisites:

- `GOMODCACHE` must resolve to an absolute directory outside `$GOROOT`.
  `make` stops otherwise; it never invents a cache path. Modules are cached
  there, so later builds reuse them.
- With the default online policy the first build downloads the pinned modules
  through your proxy and checksum database.
- With an offline policy (`GOPROXY=off`, or a `file://` proxy) the pinned
  modules must already be in that cache. If one is missing, `make` fails with
  the go command's error. It does not fall back to another policy.

## Installed use

`cmd/go` uses the installed helper when its digest matches the module sources.
Otherwise it rebuilds the helper offline (`GOPROXY=off`, `GOSUMDB=off`,
`-mod=readonly`) from your module cache. This path is unchanged.

Every file in this module, including this one, feeds the digest. After editing
any of them, rebuild the toolchain so the installed digest matches.
