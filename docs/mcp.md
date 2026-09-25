# Glue MCP server

`glue mcp` runs the Model Context Protocol server over stdio. It is a thin shell
over the same `core/engine` as the CLI, so behavior, errors and the data root
are identical.

## Client configuration

```json
{
  "mcpServers": {
    "glue": { "command": "glue", "args": ["mcp"] }
  }
}
```

stdout carries JSON-RPC only; all logs and audit summaries go to stderr. When the
client closes stdin (or drops the session) the server shuts down with exit code
`0` — a closed pipe is a normal end of session, not a failure.

### Which command to point clients at

The development setup used by this repo: the **project dev build** plus its
**dev data root** (`~/.glue-alpha`, derived from the `glue-alpha.exe` name), so
nothing in a real `~/.glue` installation is touched.

| Setup | Client `command` / `args` | Data root |
| --- | --- | --- |
| **Dev build (recommended for testing)** | `"C:\\gluestick-sh\\glue-alpha.exe"` / `["mcp"]` | `%USERPROFILE%\.glue-alpha` |
| Dev build, explicit root (equivalent) | same / `["--root", "C:\\Users\\me\\.glue-alpha", "mcp"]` | `C:\Users\me\.glue-alpha` |
| Throwaway sandbox | same / `["--root", "C:\\dev\\glue-mcp-test", "mcp"]` | `C:\dev\glue-mcp-test` (delete after) |
| Installed Glue (on PATH) | `"glue"` / `["mcp"]` | `%USERPROFILE%\.glue` |

Paths in the table are shown in JSON form, because that is what the client
configs contain (`args` is not shell-expanded: `%VAR%` would be passed
literally, so put the real path in).

- The client and your terminal must use the **same binary** to see the same data:
  `glue-alpha.exe` ↔ `~/.glue-alpha`. Cross-check with
  `.\\glue-alpha.exe --json doctor` (field `dataRoot`) and
  `.\\glue-alpha.exe --json audit list --source mcp`.
- **Check the binary first**: an install predating the MCP work has no `mcp`
  subcommand, and the client then reports a failed server
  (`glue --help` must list `mcp`; `glue --version` shows the build date).
- In Windows client config files, escape backslashes (`"C:\\gluestick-sh\\..."`).

### Per-client configuration

| Client | Where | Notes |
| --- | --- | --- |
| Cline (VS Code / JetBrains) | MCP Servers panel → *Configure MCP Servers* (`%APPDATA%\Code\User\globalStorage\saoudrizwan.claude-dev\settings\cline_mcp_settings.json`); CLI agents use `~/.cline/mcp.json` | entry under `mcpServers` with `command`/`args`/`env`/`disabled`/`autoApprove`; the panel shows tool count and has a restart button |
| Claude Code | `claude mcp add glue --scope user -- "C:\\gluestick-sh\\glue-alpha.exe" mcp`; project scope: `.mcp.json` | verify with `claude mcp list` and `/mcp` inside a session |
| Claude Desktop | `%APPDATA%\Claude\claude_desktop_config.json` | merge the snippet below into `mcpServers`, then restart the app |
| Cursor | `%USERPROFILE%\.cursor\mcp.json` (global) or `<workspace>\.cursor\mcp.json` | verify in Settings → MCP (server shows connected + tool list) |
| VS Code (Copilot Chat) | `.vscode/mcp.json` (workspace) or *MCP: Open User Configuration* | VS Code's own file uses the top-level key `servers` (its discovery feature can also import `mcpServers` from other clients) |

The dev-build snippet (Claude Desktop / Cursor / Cline `<...>` = path to
`glue-alpha.exe`):

```json
{
  "mcpServers": {
    "glue": { "command": "C:\\gluestick-sh\\glue-alpha.exe", "args": ["mcp"] }
  }
}
```

VS Code example (note the top-level key):

```json
{
  "servers": {
    "glue": { "type": "stdio", "command": "C:\\gluestick-sh\\glue-alpha.exe", "args": ["mcp"] }
  }
}
```

Three equivalent ways to add it in VS Code:

1. Command Palette → **MCP: Add Server** → **Command (stdio)** → command
   `C:\gluestick-sh\glue-alpha.exe`, argument `mcp` → store in *User* or
   *Workspace*.
2. CLI (writes to the user profile `%APPDATA%\Code\User\mcp.json`):

   ```powershell
   code --% --add-mcp "{\"name\":\"glue\",\"command\":\"C:\\gluestick-sh\\glue-alpha.exe\",\"args\":[\"mcp\"]}"
   ```

   `--%` is required in PowerShell 5.1, otherwise the quotes are eaten and the
   CLI answers `Invalid JSON '{name:glue,...}'`.
3. Edit `%APPDATA%\Code\User\mcp.json` (*MCP: Open User Configuration*) or
   `.vscode/mcp.json` (*MCP: Open Workspace Folder MCP Configuration*) directly
   with the snippet above.

**There is no URL for Glue.** The server speaks stdio (`type: "stdio"` +
`command`/`args`); VS Code's `url` field is for remote HTTP/SSE servers, and Glue
deliberately ships none (no ports, no firewall prompts on Windows — roadmap
§4.1). After adding, run *MCP: List Servers* → Start and watch *MCP: Show Output*
for the `[glue mcp] tool=... status=...` lines; a new server is asked to be
trusted the first time it starts.

`glue doctor` (and the `glue_doctor` tool) reports which of these clients were
found and whether they already register Glue: the `agents` check keeps a
CLI-only verdict, while its `data.clients` rows list each client
(`detected`/`wired`/`config`), and the human view prints one row per client
(`installed, glue MCP registered` / `installed, glue not registered (<config>)`
/ `not installed`).

Claude Code project scope (`.mcp.json` in the repository root):

```json
{
  "mcpServers": {
    "glue": { "type": "stdio", "command": "C:\\gluestick-sh\\glue-alpha.exe", "args": ["mcp"] }
  }
}
```

Useful VS Code commands: `MCP: List Servers`, `MCP: Show Output` (server stderr,
including the `[glue mcp] tool=... status=...` lines) and `MCP: Reset Cached
Tools` after upgrading Glue.

### Pre-flight self-test (no IDE needed)

Build the dev binary and drive the stdio server directly with newline-delimited
JSON-RPC — byte for byte what a client does, against the dev root
(`~/.glue-alpha`):

```powershell
go build -o glue-alpha.exe ./cli/glue

$in   = Join-Path $env:TEMP 'mcp-in.json'
$out  = Join-Path $env:TEMP 'mcp-out.json'
$reqs = @(
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"selfcheck","version":"1"}}}'
  '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
)
# UTF-8 without BOM: a BOM is not valid JSON and the server rejects it.
[System.IO.File]::WriteAllText($in, (($reqs -join "`n") + "`n"), (New-Object System.Text.UTF8Encoding($false)))
cmd /c "(type `"$in`" & ping -n 3 127.0.0.1 >nul) | .\glue-alpha.exe mcp > `"$out`" 2>nul"
"exit=$LASTEXITCODE"
(Get-Content $out -Raw) -split '"name":"' | Where-Object { $_ -like 'glue_*' }
```

No `--root` on purpose: the client configs above also pass none, so both resolve
to `%USERPROFILE%\.glue-alpha`. Expected output: 14 `glue_*` names (including
`glue_bucket_remove`), `serverInfo.name = "glue"`, `exit=0` and an empty stderr
(drop `2>nul` to see the `[glue mcp] tool=... status=...` lines). The trailing
`ping` keeps stdin open long enough for the responses; without it the server sees
EOF before it replies.

Confirm the same data root from the terminal:

```powershell
.\glue-alpha.exe --json doctor          # dataRoot = ...\.glue-alpha
.\glue-alpha.exe --json audit list --source mcp --limit 10
```

The same flow is automated (subprocess + `mcp.CommandTransport`, asserting the
tool surface, the `[]` payload contract, the dev data root, the
pending → `glue_confirm` destructive flow and the exit-0 shutdown):

```powershell
$env:GLUE_MCP_BIN = "$PWD\glue-alpha.exe"   # the client's command
go test ./cli/glue -run TestMCPStdio -count=1 -v
```

The test drives `--root %USERPROFILE%\.glue-alpha` (override with
`GLUE_MCP_ROOT=<dir>`), refuses to run against a real `~/.glue`, and creates then
removes a `demo` bucket inside that root.

### Acceptance scenario (Phase 2 criterion)

Ask the agent, in one session: *"make this machine agent-ready: check the
environment, install git, and verify PATH"*. What to look for:

1. `glue_doctor` → the same report as `glue doctor --json` (`agentReady`,
   `summary`, `checks[].group`).
2. `glue_install {"package":"git"}` → `{"status":"pending","confirm_token":…}`
   (nothing installed yet), then `glue_confirm {"token":…}` → the install result.
3. `glue_path_check` → `in_path` for the shim directory.
4. `glue audit list --source mcp --limit 20` shows the decision chain
   `pending → granted → success` with the client name as `actor`.

### Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| Server fails to start, 0 tools | `command` not found, or an old `glue.exe` without `mcp`; use an absolute path and check `glue --help` lists `mcp` |
| Client shows stale tools | restart the server (Cline) or run `MCP: Reset Cached Tools` (VS Code) |
| Tool returns `denied_by_policy` | `agent.policy.deny` / `protected` blocks it: `glue config list`, then edit the policy |
| Write tool answers `pending` and nothing happens | the agent must call `glue_confirm`; tokens are single-use with a 300 s TTL |
| Writes land in an unexpected place | client binary and your terminal use different data roots (exe name rule) — compare `glue doctor --json` `dataRoot`, or pin `--root` in `args` |
| Need to see server logs | they are on the server's stderr: VS Code `MCP: Show Output`, Cline panel output, Claude Code `/mcp` detail |

## Tools

| Tool | Kind | Notes |
| --- | --- | --- |
| `glue_search` | read | `{query, limit}` → `{query, results, count}` |
| `glue_list` | read | `{detailed}` → `{packages, count}` |
| `glue_info` | read | `{package}` → `{packages, count}` |
| `glue_depends` | read | `{package}` → `{command, ok, plans}` |
| `glue_path_check` | read | `{in_path, bin_dir, store_alias_shadowing, ok}` |
| `glue_bucket_list` | read | `{buckets, count}` |
| `glue_doctor` | read | same report as `glue doctor --json` |
| `glue_install` / `glue_uninstall` / `glue_update` | write | confirm token unless `agent.auto_yes` |
| `glue_bucket_add` / `glue_bucket_update` | write | confirm token unless `agent.auto_yes` |
| `glue_bucket_remove` | write (destructive) | deletes the local bucket checkout; confirm token unless `agent.auto_yes`, blockable via `deny`/`protected` |
| `glue_confirm` | write | executes a pending token (single use, 5 minutes) |

`glue_bucket_remove` example flow: call it, then confirm the returned token
(possibly from another process — tokens are persisted under
`<data root>/logs/pending/`):

```
tools/call glue_bucket_remove {"name":"demo"} → {"status":"pending","confirm_token":"…","action":"bucket_remove"}
tools/call glue_confirm       {"token":"…"}   → {"command":"bucket_remove","name":"demo","ok":true}
```

Every tool call emits one `[glue mcp] tool=<name> status=<ok|error>` line on
stderr; read calls are also recorded as `tool_call` audit rows.

## Confirmation and policy

Write tools return `{status:"pending", confirm_token, expires_in, action, package}`
by default. Call `glue_confirm` with the token to execute. In `config.json`:

```json
{
  "agent": {
    "auto_yes": false,
    "policy": {
      "mode": "confirm",
      "deny": ["uninstall"],
      "protected": ["git"]
    }
  }
}
```

- `deny` hard-blocks an operation (`deny: ["uninstall"]` disables uninstall for
  agents; `deny: ["bucket_remove"]` freezes bucket deletion).
- `protected` blocks destructive operations against a name: `uninstall` for
  packages, `bucket_remove` for buckets (`protected: ["main"]` keeps the main
  bucket). The human CLI is unaffected.
- `auto_yes` (or `mode: "auto"`) skips the token but never the deny list or
  `protected`.
- `glue hold` blocks uninstall on the MCP path as a second gate.
- Errors are JSON `{code, message, hint}` (`denied_by_policy`,
  `invalid_confirm_token`, `bucket_not_found`, ...).

These keys are also settable through the CLI:

```powershell
glue config set agent.auto_yes true
glue config set agent.policy.mode confirm
glue config set agent.policy.deny uninstall
glue config set agent.policy.protected git,nodejs
glue config set agent.policy.deny bucket_remove   # or: agent.policy.protected main
```

## Audit

- SQLite `activity_log` rows carry `source` (`cli`/`mcp`) and `actor`
  (MCP clientInfo such as `cline/3.7`); MCP bucket operations
  (`bucket_add`/`bucket_update`/`bucket_remove`) write a completion row with
  `source=mcp` when they run through the MCP server.
- `<data root>/logs/audit.jsonl` is append-only and hash-chained
  (`prev`/`hash` per entry). It rotates into `audit-<UTC timestamp>.jsonl`,
  keeping the last 5 segments; the chain continues across rotation.
- Rotation is configurable: `audit.max_bytes` (`0` = never rotate) and
  `audit.keep_segments` (negative = keep every segment).
- `glue audit verify` reports the chain `head`; pin it outside the machine and
  compare with `glue audit verify --expect <hash>` (mismatch → exit 1,
  `reason: "head mismatch"`). After pruning, verification starts from
  `<data root>/logs/audit.anchor`.
- Scheduled verification: `audit.verify_interval_hours` (default 24, `off` =
  disabled) re-verifies the chain after an audited operation once the interval
  elapsed. A healthy chain only refreshes `<data root>/logs/.audit-verify`;
  tampering records an `audit_verify` row (`status: "broken"`) and warns on
  stderr.
- `glue doctor` and `glue env` write their own activity operations (`doctor` /
  `env`), so `glue audit list --json` shows which check-up produced a row.
- Rotation is configurable: `glue config set audit.max_bytes 8388608` and
  `glue config set audit.keep_segments 5` (defaults). `audit.max_bytes off`
  disables rotation; `audit.keep_segments all` keeps every segment (`--`
  precedes a literal negative value on the CLI).
- Pruned segments leave `<data root>/logs/audit.anchor` (the retained chain
  head), so verification can still walk what is left and still flags a segment
  that was deleted by hand.
- `glue audit list --source mcp --package git --since 2026-09-25T00:00:00Z --limit 50`
- `glue audit list --jsonl ...` reads the JSONL files (rotated included)
  instead of SQLite.
- `glue audit verify` recomputes the chain across all segments and fails when an
  entry was edited; on success it prints the anchor when older segments had
  been pruned.

Audit writes are best-effort for the operation itself: failures are surfaced as
stderr warnings rather than aborting the install/uninstall.
