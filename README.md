# gluestick-sh

> **Glue** — the agent-ready package manager for Windows. Scoop-compatible manifests, CAS zero-copy installs, parallel downloads, and first-class AI-agent interfaces (`--json`, stable exit codes, and an MCP server).

This is the **monorepo** for the Glue project, created during the *"Agent-Ready on Windows"* pivot (2026-09). Full strategy and phase plan: [`docs/agent-ready-roadmap.md`](docs/agent-ready-roadmap.md).

## Layout

| Path | Module path | Contents |
| --- | --- | --- |
| `cli/` | `github.com/gluestick-sh/cli` | The `glue` command (all Scoop-style verbs, `--json` output, future `glue mcp`) |
| `core/` | `github.com/gluestick-sh/core` | Embeddable engine: manifest parsing, CAS store, parallel downloads, SQLite index, shims |
| `shim/` | `github.com/gluestick-sh/shim` | PATH shim runner used by core when installing executables |
| `docs/` | — | Roadmap, JSON schemas, MCP docs |

**Not in this repo (frozen, non-goals of the pivot):** `web` (marketing site), `desktop` / `desktop-pro`, `gateway` / `api`. They remain in their original repositories, online and untouched.

## Platform

**Windows only** for production use.

## Build & test

Requires **Go 1.26+**. The repo uses a Go workspace (`go.work`) so the three modules resolve against each other locally:

```powershell
go work sync
go test ./cli/... ./core/... ./shim/...   # all modules via the workspace
go build -o glue.exe ./cli/glue
go build -o shim.exe ./shim
```

> Note: in Go workspace mode, `./...` is **not** a valid pattern from the `go.work`
> root (the root itself is not a module). Enumerate the module directories instead,
> or run the commands from inside a module.

## Quick start

```powershell
glue bucket add extras
glue search python
glue install git
glue list --json
glue doctor
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

## History

This monorepo started **fresh** in Sep 2026 as the baseline of the *"Agent-Ready on
Windows"* pivot (a single initial commit — no carried-over history). Pre-pivot
history and release tags (`cli` v0.1.12, `core` v0.1.12, `shim` v0.1.6) remain
readable in the original standalone repositories:

- https://github.com/gluestick-sh/cli
- https://github.com/gluestick-sh/core
- https://github.com/gluestick-sh/shim

## License

MIT.
