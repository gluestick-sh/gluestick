# AGENTS.md

Guidance for AI coding agents (Cline, Claude Code, Cursor, etc.) working in this repository.

## What this repo is

The Glue monorepo: a **Windows-only**, Scoop-compatible package manager being pivoted to
**"agent-ready on Windows"**. Three Go modules share one workspace:

- `cli/` — the `glue` command (module `github.com/gluestick-sh/cli`)
- `core/` — embeddable engine (module `github.com/gluestick-sh/core`)
- `shim/` — PATH shim runner (module `github.com/gluestick-sh/shim`)

## Environment & commands

```powershell
go work sync                 # after adding modules or editing go.mod files
go build ./cli/... ./core/... ./shim/...   # compile everything in the workspace
go test ./cli/... ./core/... ./shim/...    # run all tests
go test ./core/engine/...    # run one package's tests
go build -o glue-alpha.exe ./cli/glue      # dev build (see isolation below)
go build -o shim.exe ./shim

# MCP stdio E2E through the real binary (opt-in; skipped without GLUE_MCP_BIN).
# It drives the dev data root (%USERPROFILE%\.glue-alpha); GLUE_MCP_ROOT overrides.
$env:GLUE_MCP_BIN = "$PWD\glue-alpha.exe"
go test ./cli/glue -run TestMCPStdio -count=1 -v
```

- IMPORTANT: in workspace mode `./...` is **invalid from the `go.work` root** (the
  root is not a module). Always enumerate `./cli/... ./core/... ./shim/...` or
  `cd` into a module first.
- `go.work` and `go.work.sum` are **committed** (build definition of the monorepo).
  Never gitignore them.
- IDE-client MCP setup, the JSON-RPC self-test and the acceptance script live in
  `docs/mcp.md` ("Client configuration").

### Line endings (IMPORTANT)

- The repository is **LF-only** and `.gitattributes` enforces it
  (`* text=auto eol=lf`, binaries marked `binary`); this checkout also sets
  `core.autocrlf=false`. `gofmt -w` writes LF and drops UTF-8 BOMs, so on a CRLF
  checkout a `gofmt -w` over several directories leaves every rewritten file with
  a stale CRLF-era index stat: `git status` then lists it as modified ("LF will be
  replaced by CRLF the next time Git touches it") while `git diff` shows no
  content change — hundreds of phantom entries (249 vs 70 real files was one such
  storm).
- Never clear that with `git checkout -- .` / `git restore .`: it discards real
  work (on 2026-09-25 the phantom set and the real set overlapped 0 files, but do
  not rely on it). Use `git add --renormalize .` instead — content-identical files
  are not staged, only their stat cache is refreshed — then confirm with
  `git diff --name-only` (real changes) and `git diff-files --quiet` (stat view).
- `git update-index --refresh` / `--really-refresh` do **not** fix it: they print
  "needs update" and refuse to rewrite entries whose stat does not match.

### Dev / data isolation (IMPORTANT)

- **Never build a dev binary as `glue.exe`.** Build it as `glue-alpha.exe` (or any
  `glue-<suffix>`). The data root is derived from the executable name:
  `glue.exe` → `~/.glue`, `glue-alpha.exe` → `~/.glue-alpha`. This keeps dev
  builds fully isolated from the maintainer's real installation and its data —
  no manual backup or directory renaming is needed.
- The hidden `--root` flag overrides the data root (tests use it with temp dirs).
- Shim runners resolve their config relative to their own location
  (`<root>/shims/<name>.exe` → `<root>/shims-meta/<name>.json`), with
  `GLUE_DATA_ROOT` as an optional env override; the legacy `~/.glue` path is only
  a fallback. Normal installs resolve to exactly the same file as before.
- Known read-only exception: `core/engine/search_index_php_integration_test.go`
  reads the real `~/.glue` (skips when the php bucket is absent); it never writes.

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

## Frozen / out of scope

`web`, `desktop`, `desktop-pro`, `gateway`, `api` are **not** in this repo and are
frozen by decision. Do not plan work on them.
