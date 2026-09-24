# AGENTS.md

Guidance for AI coding agents (Cline, Claude Code, Cursor, etc.) working in this repository.

## What this repo is

The Glue monorepo: a **Windows-only**, Scoop-compatible package manager being pivoted to
**"agent-ready on Windows"**. Three Go modules share one workspace:

- `cli/` — the `glue` command (module `github.com/gluestick-sh/cli`)
- `core/` — embeddable engine (module `github.com/gluestick-sh/core`)
- `shim/` — PATH shim runner (module `github.com/gluestick-sh/shim`)

Read `docs/agent-ready-roadmap.md` before making product-level decisions. It is the
approved strategy document (decision record in its §8).

## Environment & commands

```powershell
go work sync                 # after adding modules or editing go.mod files
go build ./cli/... ./core/... ./shim/...   # compile everything in the workspace
go test ./cli/... ./core/... ./shim/...    # run all tests
go test ./core/engine/...    # run one package's tests
go build -o glue.exe ./cli/glue
go build -o shim.exe ./shim
```

- IMPORTANT: in workspace mode `./...` is **invalid from the `go.work` root** (the
  root is not a module). Always enumerate `./cli/... ./core/... ./shim/...` or
  `cd` into a module first.

- Go **1.26+**, Windows only for runtime. Some tests touch real buckets/downloads; if a
  network-dependent test fails, note it and re-run the package before assuming breakage.

## Conventions that must not be broken

1. **CLI contract** (see README "Compatibility promises"):
   - Exit codes: `0` success, `1` operation failure, `2` usage error (`cli/glue/exitcode.go`).
   - `--json` output goes through `cli/glue/json_output.go`; schemas are additive-only.
   - No interactive prompts on any code path. Destructive commands take an explicit
     `--yes`; the MCP layer defaults to `yes=false` (needs confirmation).
2. **Windows-first**: platform-specific code lives in `*_windows.go` / `*_other.go` pairs;
   `core` targets Windows exclusively, with non-Windows stubs only for tests/build checks.
3. **Error handling**: engine errors flow through `core/apperr`; do not stringify errors
   into output before structuring them.
4. **Data layout**: everything user-related lives under `%USERPROFILE%\.glue`
   (`apps/`, `buckets/`, `cache/`, `store/`, `shims/`, `config.json`, `device.json`).
   Never write outside it.
5. **Commits**: Conventional Commits style (`feat:`, `fix:`, `chore:`, `docs:`),
   scope in parentheses, e.g. `fix(path): prepend shims ahead of Store python aliases`.

## Where things are

| Task | Look in |
| --- | --- |
| Add/modify a CLI command | `cli/glue/<command>.go` (cobra; register in `cli/glue/root.go`) |
| JSON output plumbing | `cli/glue/json_output.go` |
| Engine operations (install/search/…) | `core/engine/` |
| Manifests, buckets, CAS, downloads | `core/manifest/`, `core/bucket/`, `core/store/`, `core/downloader/` |
| Shims | `core/shim/` + top-level `shim/` runner |
| Roadmap / strategy / decisions | `docs/agent-ready-roadmap.md` |

## Frozen / out of scope

`web`, `desktop`, `desktop-pro`, `gateway`, `api` are **not** in this repo and are
frozen by decision (roadmap §5.2). Do not plan work on them.

## Release scheme (decision record, roadmap §8.1)

- Module paths currently stay `github.com/gluestick-sh/*` (unchanged during Phase 0).
- Planned coordinated switch: `core` moves to vanity path `gluestick.sh/core` together
  with a go-import meta tag on the website; releases then use subdirectory tags
  (`core/v0.2.0`, `cli/v0.2.0`, `shim/v0.2.0`). Do not rename module paths ad hoc.
