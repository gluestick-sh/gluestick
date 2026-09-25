# Glue JSON schemas

Stability contract (see README "Compatibility promises"):

- Every `--json` command prints **one JSON document** on stdout; logs go to stderr.
- Fields are **additive-only**: new keys may appear, existing keys are never
  renamed or removed in place.
- Object keys are stable; `details`, `data`, and `result` are free-form
  extension areas where unknown keys must be tolerated.
- Exit codes: `0` success, `1` operation failure, `2` usage error.
- Error codes come from `core/apperr` (`manifest_not_found`,
  `manifest_ambiguous`, `bucket_not_installed`, `bucket_not_found`,
  `package_not_installed`, `usage`, `unknown`, plus the CLI-level
  `invalid_argument`, `bucket_unknown` and MCP `denied_by_policy` /
  `invalid_confirm_token`).

## install / uninstall / update

```json
{
  "command": "install",
  "ok": true,
  "results": [
    {
      "ref": "main/nodejs",
      "result": {
        "Name": "nodejs",
        "Version": "22.0.0",
        "Status": "success",
        "Message": "Package installed successfully",
        "Duration": 123456789,
        "Files": null,
        "Size": 0,
        "Error": null,
        "Manifest": null,
        "Suggestions": null
      }
    }
  ]
}
```

- `command`: `install` | `uninstall` | `update`.
- `results[].ref`: the requested ref (`bucket/pkg@version` accepted).
- `results[].result`: `engine.Result` (Go field names; `Duration` is
  nanoseconds, `Error` is normally `null`).
- Failure item: `{ "ref": "nodejs", "error": "...", "code": "manifest_not_found", "hint": "..." }`.

### update check (no execution)

Without `--all`/targets that resolve, `glue update` reports what is available
instead of executing:

```json
{ 
  "updates": [ 
    { 
      "name": "git", 
      "installedVersion": "2.53.0", 
      "latestVersion": "2.54.0", 
      "bucket": "main" 
    } 
  ], 
  "count": 1 
}
```

- everything up to date: `{ "updates": [], "count": 0, "upToDate": true }`
- installed packages exist but none matched the request:
  `{ "updates": [], "count": 0, "matched": false }`

## search

```json
{
  "query": "git",
  "results": [
    {
      "Name": "git",
      "Version": "2.54.0",
      "Description": "Distributed version control system",
      "Homepage": "https://git-scm.com",
      "Bucket": "main",
      "Deprecated": false,
      "InstalledAt": "",
      "InstalledSize": 0,
      "DetectedVersion": "",
      "VersionDetectSrc": "",
      "ExternallyUpdated": false,
      "Manifest": null
    }
  ],
  "count": 1
}
```

## list

```json
{ 
  "packages": [ 
    { 
      "Name": "git", 
      "Version": "2.54.0" 
    } 
  ], 
  "count": 1 
}
```

With nothing installed `packages` is `[]` (never `null`), so agents can iterate
the array without a nil check.

`glue list --all` adds `allVersions: true` and uses a different package shape:

```json
{
  "packages": [ { "name": "git", "versions": ["2.54.0"], "current": "2.54.0" } ],
  "count": 1,
  "allVersions": true
}
```

## info

```json
{
  "packages": [
    {
      "name": "git",
      "version": "2.54.0",
      "installPath": "C:\\Users\\me\\.glue\\apps\\git\\2.54.0",
      "currentPath": "C:\\Users\\me\\.glue\\apps\\git\\current",
      "installedAt": "2026-09-25T10:00:00Z",
      "size": 1024,
      "fileCount": 4,
      "shims": ["git"],
      "bucket": "main",
      "description": "Distributed version control system",
      "homepage": "https://git-scm.com",
      "license": "GPL-2.0",
      "depends": ["main/7zip"],
      "notes": [],
      "manifestVersion": "2.54.0",
      "updateAvailable": false
    }
  ],
  "count": 1,
  "failed": ["missing-pkg"]
}
```

`failed` is present only when at least one requested package could not be
resolved.

## depends

```json
{
  "command": "depends",
  "ok": true,
  "plans": [
    {
      "ref": "nodejs",
      "package": "nodejs@22.0.0",
      "depends": [ { "ref": "main/7zip", "installed": true } ],
      "suggestions": [ { "ref": "main/git", "label": "Recommended", "installed": false } ]
    }
  ]
}
```

Per-ref failures stay inside the plan (`"error"`, `"code"`, `"hint"`) and the
command exits 1 when any ref failed.

## doctor (default readiness view)

```json
{
  "command": "doctor",
  "checks": [
    {
      "id": "shim_path",
      "ok": true,
      "detail": "Shim directory is on PATH",
      "level": "blocking",
      "status": "pass",
      "group": "glue",
      "data": { "bin_dir": "C:\\Users\\me\\.glue\\shims", "in_path": true }
    }
  ],
  "ok": true,
  "agentReady": true,
  "schemaVersion": 1,
  "dataRoot": "C:\\Users\\me\\.glue",
  "dataRootIsolated": false,
  "cliVersion": "0.1.0",
  "summary": {
    "total": 23,
    "passed": 20,
    "failed": 3,
    "skipped": 0,
    "blockingFailed": [],
    "advisoryFailed": ["shell_utf8"]
  },
  "score": 75,
  "nextActions": [],
  "fixes": [
    { "check": "shell_utf8", "action": "set_utf8", "applied": true, "detail": "console → 65001" }
  ],
  "error": null
}
```

- `agentReady` + exit code are the contract; `score` and `fixes` are additive.
- `checks[].data` is free-form; known keys include `tools`, `duplicates`,
  `bin_dir`, `codepage`, `policy`, and `clients` (the `agents` check).

`agents` (CLI coexistence) keeps a CLI-only verdict but carries a second,
advisory view of the MCP clients found on the machine:

```json
{
  "id": "agents",
  "level": "advisory",
  "status": "fail",
  "detail": "claude, codex, opencode not found",
  "data": {
    "tools": [ { "name": "Claude Code", "found": false } ],
    "found": [],
    "clients": [
      { "name": "Cline", "kind": "ide-extension", "detected": false, "wired": false },
      {
        "name": "GitHub Copilot Chat",
        "kind": "ide-extension",
        "detected": true,
        "evidence": "C:\\Users\\me\\AppData\\Roaming\\Code\\User\\globalStorage\\github.copilot-chat",
        "wired": true,
        "config": "C:\\Users\\me\\AppData\\Roaming\\Code\\User\\mcp.json"
      }
    ]
  }
}
```

- `clients[]`: `kind` is `cli` | `ide-extension` | `desktop`; `wired` means a
  `glue` entry exists under the client's servers key (`mcpServers`, or `servers`
  for VS Code) in `config`; `config` is empty when the file does not exist yet.
- The client matrix never changes `agentReady`, the blocking set or the score.

`buckets` (blocking) carries the counts behind its verdict:

```json
{
  "id": "buckets",
  "ok": false,
  "detailKey": "agent.buckets.no_manifests",
  "detail": "no manifests indexed from 1 bucket(s)",
  "hint": "Run glue cache rebuild to rescan the buckets; if it persists, close other glue processes using this data root",
  "data": {
    "buckets": [ { "name": "main", "packages": 0 } ],
    "bucket_count": 1,
    "buckets_indexed": 0,
    "manifest_count": 0,
    "index_ready": true
  }
}
```

- The hint, the `nextActions` entry and the `--fix` action all follow the reason:

  | Reason (`detailKey`) | hint / next action | `fixes[].action` |
  | --- | --- | --- |
  | `agent.buckets.empty` | `glue bucket add main` | `bucket_add` |
  | `agent.buckets.index_not_ready` | wait and re-run `glue doctor`; if it persists another glue process may hold the data root | `reindex_buckets` (rescans the local bucket dirs, offline-safe) |
  | `agent.buckets.no_manifests` | `glue bucket update` to re-fetch the manifests | `bucket_pull` (network; skipped under `--offline` with the reason) |

- `index_ready` reflects the in-process search index (built at engine start), so a
  stale build is transient, not a disk problem: `reindex_buckets` repairs it
  without any download.
- `fixes[]` only appears with `glue doctor --fix`.

## env (traditional environment checks)

```json
{
  "checks": [
    {
      "id": "glue_root",
      "ok": true,
      "detailKey": "agent.glue_root.ok",
      "detail": "Data directory is writable",
      "hint": ""
    }
  ],
  "ok": true
}
```

## config

```json
{ "command": "config_get", "key": "agent.auto_yes", "value": true, "set": true }
{ "command": "config_set", "ok": true, "key": "agent.auto_yes", "value": true }
{ "command": "config_unset", "ok": true, "key": "agent.auto_yes", "was_set": true }
```

`config_list` keys: `github_proxy(+_set)`, `parallel_download(+_set)`,
`color(+_set)`, `verbose(+_set)`, `agent_auto_yes`, `agent_policy_mode`,
`agent_policy_deny`, `agent_policy_protected`, `audit_max_bytes`,
`audit_keep_segments`, `audit_verify_interval_hours`.

## bucket

```json
{ "buckets": [ { "name": "main", "repo_url": "https://github.com/ScoopInstaller/Main" } ], "count": 1 }
```

`glue bucket known` uses the same shape for known upstream buckets.

`glue bucket add`:

```json
{ "command": "bucket_add", "ok": true, "name": "main", "repo_url": "https://github.com/ScoopInstaller/Main" }
```

- Re-adding an installed bucket is a no-op success with
  `"already_installed": true`.
- Unknown name without a repo URL:
  `{ "command": "bucket_add", "ok": false, "name": "nope", "error": "…", "code": "bucket_unknown", "hint": "glue bucket known lists available buckets" }`.

`glue bucket update [name...]`:

```json
{ "command": "bucket_update", "ok": true, "updated": ["main", "extras"] }
```

- With no arguments `updated` is `[]` (all buckets were updated), never `null`.

`glue bucket check [name...]`:

```json
{
  "command": "bucket_check",
  "buckets": [
    {"name": "main", "ok": true, "has_updates": true, "local_commit": "aaa1111", "remote_commit": "bbb2222"}
  ],
  "failed": 0,
  "with_updates": 1,
  "ok": true
}
```

`error` is present on a failed entry and makes `failed`/`ok`/exit code 1.

`glue bucket remove <name>...` (added 2026-09-25):

```json
{ "command": "bucket_remove", "ok": true, "results": [ { "ref": "main" } ] }
```

- Per-bucket failure: the item carries `error`/`code`/`hint`
  (`code: "bucket_not_found"` for a bucket that is not installed) and the
  command exits 1 while successful removals still happen.
- `glue bucket rm` is an alias of `remove` and emits the identical schema.

## cache

`glue cache list`:

```json
{
  "packages": [],
  "total": { "packageCount": 0, "totalSize": 0, "totalFiles": 0 }
}
```

`packages[]` entries are `engine.CachePackageInfo` (`name`, `version`,
`installed`, `size`, `fileCount`). `packages` is always an array (never `null`).

`glue cache clear <name>...`:

```json
{
  "command": "cache_clear",
  "ok": true,
  "cleared": ["nodejs"],
  "cleared_files": 4,
  "not_in_index": ["missing-pkg"]
}
```

- `ok` is true only when at least one package was cleared; otherwise the command
  exits 1 (same as text mode).

`glue cache clear --all`:

```json
{ "command": "cache_clear", "ok": true, "all": true, "packages": 3, "files": 12, "size_bytes": 1024 }
```

`glue cache gc`:

```json
{ "command": "cache_gc", "ok": true, "removed_blobs": 2, "freed_bytes": 4096 }
```

`glue cache rebuild`:

```json
{ "command": "cache_rebuild", "ok": true, "indexed": 3 }
```

## path

`glue path show`:

```json
{ "bin_dir": "C:\\Users\\me\\.glue\\shims" }
```

`glue path check`:

```json
{ "bin_dir": "C:\\Users\\me\\.glue\\shims", "in_path": true, "store_alias_shadowing": false, "ok": true }
```

- `ok` = `in_path && !store_alias_shadowing`; a false `ok` exits 1.

`glue path setup`:

```json
{ "bin_dir": "C:\\Users\\me\\.glue\\shims", "changed": true, "ok": true }
```

- `changed` is false when the shim directory already leads the user PATH.

## hold / unhold / reset

```json
{ "command": "hold", "ok": true, "results": [ { "ref": "nodejs" } ] }
```

- `command`: `hold` | `unhold` | `reset`.
- Failure item: `{ "ref": "nodejs", "error": "…", "code": "package_not_installed" }`
  and the command exits 1.
- `reset` reports `engine.Result` on failure (`jsonResultItemFromInstall`).

## home

```json
{
  "command": "home",
  "ok": true,
  "results": [ { "ref": "main/git", "url": "https://git-scm.com", "opened": false } ]
}
```

- JSON mode never opens a browser (`opened` stays false); `--no-open` only
  affects text mode.
- Failure item: `{ "ref": "x", "opened": false, "error": "…", "code": "manifest_not_found" }`.

## audit

```json
{
  "entries": [
    {
      "operation": "install",
      "package_name": "nodejs",
      "version": "",
      "status": "pending",
      "source": "mcp",
      "actor": "cline/3.7",
      "timestamp": "2026-09-25T10:00:00Z",
      "details": { "phase": "confirm_required", "action": "install" },
      "prev": "",
      "hash": "9f2c..."
    }
  ],
  "count": 1,
  "format": "sqlite"
}
```

- `format`: `sqlite` (default) or `jsonl` (`--jsonl`); JSONL entries additionally
  carry `prev`/`hash`.
- `sqlite` entries come from `activity_log`; `jsonl` reads the rotating,
  hash-chained `<root>/logs/audit.jsonl` (+ segments).
- `glue audit verify` → `{ "ok": true, "entries": 2, "head": "9f2c…" }`; after
  segments were pruned it also reports `anchor` (the chain head the surviving
  entries start from). A break reports
  `{ "ok": false, "entries": 1, "brokenAt": 2, "reason": "hash mismatch" }`.
- `head` is the hash of the newest entry — the value to pin outside the machine.
  `glue audit verify --expect <head>` fails with
  `{ "ok": false, "reason": "head mismatch", "head": "…", "expected": "…" }` when
  the pinned hash no longer matches; `expected` echoes the pinned value whenever
  `--expect` was used.
- Scheduled verification: `audit.verify_interval_hours` (default 24, `off` =
  disabled) re-verifies the chain after an audited operation once the interval
  elapsed. A healthy chain only refreshes `<root>/logs/.audit-verify` (no audit
  row); tampering records an `audit_verify` row with `status: "broken"` and
  `details.brokenAt`, plus a stderr warning.
- `glue doctor` and `glue env` record their own operations (`doctor` / `env`), so
  the activity feed tells the two check-ups apart.

## MCP tools

MCP tool payloads reuse the CLI schemas above (`glue_search` ↔ search,
`glue_list` ↔ list, `glue_info` ↔ info, `glue_depends` ↔ depends,
`glue_doctor` ↔ doctor, `glue_bucket_list` ↔ `bucket list`,
`glue_bucket_remove` ↔ `bucket remove`). Write tools return a pending confirm
object; see [docs/mcp.md](mcp.md) for the full contract.

