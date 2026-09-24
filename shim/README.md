# shim

> The Gluestick shim runner — a tiny, dependency-free launcher compiled to `shim.exe`.

When [core](https://github.com/gluestick-sh/core) installs a package, it creates
a shim `<name>.exe` on `PATH` for each executable. That shim is a copy of the
binary built from this project. At launch it:

1. Derives its name from `os.Args[0]` (e.g. `git.exe` -> `git`).
2. Reads `~/.glue/shims-meta/<name>.json`.
3. Execs the real target with the configured args/env, proxying stdio and
   propagating the child's exit code.

## Platform

**Windows only.** The runner is built and shipped as `shim.exe` for Windows.

## Build

```powershell
go build -o shim.exe .
```

## Config contract

The `Config` struct in `main.go` is a shared contract with `core`'s
`shim.Config`. The on-disk JSON is written by `core`; keep the field names in
sync across both projects:

```json
{
  "name": "git",
  "command": "C:\\path\\to\\git.exe",
  "args": ["--optional-default-arg"],
  "env": { "KEY": "value" },
  "path": "C:\\path\\to\\git.exe"
}
```

## License

MIT — see [LICENSE](LICENSE).
