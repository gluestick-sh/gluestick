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

stdout carries JSON-RPC only; all logs and audit summaries go to stderr.

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
| `glue_confirm` | write | executes a pending token (single use, 5 minutes) |

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

- `deny` hard-blocks an operation; `protected` blocks uninstall of a package.
- `auto_yes` (or `mode: "auto"`) skips the token but never the deny list.
- `glue hold` blocks uninstall on the MCP path as a second gate.
- Errors are JSON `{code, message, hint}` (`denied_by_policy`,
  `invalid_confirm_token`, ...).

These keys are also settable through the CLI:

```powershell
glue config set agent.auto_yes true
glue config set agent.policy.mode confirm
glue config set agent.policy.deny uninstall
glue config set agent.policy.protected git,nodejs
```

## Audit

- SQLite `activity_log` rows carry `source` (`cli`/`mcp`) and `actor`
  (MCP clientInfo such as `cline/3.7`).
- `<data root>/logs/audit.jsonl` is append-only and hash-chained
  (`prev`/`hash` per entry). It rotates into `audit-<UTC timestamp>.jsonl`,
  keeping the last 5 segments; the chain continues across rotation.
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
