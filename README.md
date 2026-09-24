# gluestick-sh

> **Glue** — the agent-ready package manager for Windows. Scoop-compatible manifests, CAS zero-copy installs, parallel downloads, and first-class AI-agent interfaces (`--json`, stable exit codes, and an MCP server).

This is the **monorepo** for the Glue project.

## Layout

| Path | Module path | Contents |
| --- | --- | --- |
| `cli/` | `github.com/gluestick-sh/cli` | The `glue` command (all Scoop-style verbs, `--json` output, future `glue mcp`) |
| `core/` | `github.com/gluestick-sh/core` | Embeddable engine: manifest parsing, CAS store, parallel downloads, SQLite index, shims |
| `shim/` | `github.com/gluestick-sh/shim` | PATH shim runner used by core when installing executables |

## Platform

**Windows only** for production use.

## Build & test

Requires **Go 1.26+**. The repo uses a Go workspace (`go.work`) so the three modules resolve against each other locally:

```powershell
go work sync
go test ./cli/... ./core/... ./shim/...   # all modules via the workspace
go build -o glue-alpha.exe ./cli/glue     # dev build (see note below)
go build -o shim.exe ./shim
```

> Note: in Go workspace mode, `./...` is **not** a valid pattern from the `go.work`
> root (the root itself is not a module). Enumerate the module directories instead,
> or run the commands from inside a module.

> **Dev isolation**: the data root is derived from the executable name — `glue.exe`
> uses `%USERPROFILE%\.glue`, while a dev build named `glue-alpha.exe` automatically
> uses `%USERPROFILE%\.glue-alpha`, keeping development builds fully isolated from
> a real installation.

## Quick start

```powershell
glue bucket add extras
glue search python
glue install git
glue list --json
glue doctor            # agent-ready verdict for this machine (exit code 0/1)
glue doctor --fix      # list-and-apply safe fixes (PATH, buckets, UTF-8, PowerShell, git, profiles), then re-check
glue env               # traditional environment checks (git, 7z, shims, GitHub)
glue mcp               # MCP server over stdio (for Cline/Claude Code and other MCP clients)
```

Data directory: `%USERPROFILE%\.glue`

## Exit codes (stable contract)

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | Operation failed (install error, missing package, etc.) |
| `2` | Usage error (unknown command/flag, wrong arguments) |

## Compatibility promises

- Command verbs and Scoop-compatible semantics never break.
- `--json` output schemas are semver-managed: fields are only added, never renamed or removed in place.
- Exit codes `0/1/2` never change.

## License

MIT.
