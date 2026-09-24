# cli

> The official `glue` command line — Scoop-compatible, powered by the [core](https://github.com/gluestick-sh/core) engine.

## Platform

**Windows only** for production use.

## Install

### One-click (recommended)

```powershell
irm https://gluestick.sh/install.ps1 | iex
```

### From source

Requires **Go 1.26+**.

```powershell
git clone https://github.com/gluestick-sh/cli.git
cd cli
go build -o glue.exe ./glue
.\glue.exe path setup
```

`cli` depends on a published **core** module version (see `go.mod`); a plain `go build` fetches it automatically. For local sibling-repo development, use a `go.work` file placed **next to** the repos (not committed to any repo) so local changes to `core`/`shim` are picked up:

```go
// ../go.work  (e.g. github.com/go.work covering all three repos)
go 1.26.3

use (
	./cli
	./core
	./shim
)
```

Build the shim runner (used by core when creating PATH shims):

```powershell
cd ../shim
go build -o shim.exe .
```

## Quick start

```powershell
glue bucket add extras
glue search python
glue install git
glue list
glue doctor
```

Data directory: `%USERPROFILE%\.glue`

## Commands

| Command | Description |
| --- | --- |
| `glue install <pkg>` | Install packages (`bucket/pkg`, `@version`, `--force`) |
| `glue uninstall <pkg>` | Uninstall packages |
| `glue search <query>` | Search bucket manifests |
| `glue list` | List installed packages |
| `glue info <pkg>` | Installed package details |
| `glue depends <pkg>` | Missing dependencies and suggestions |
| `glue update` | List or install package upgrades (`--all`, `*` arg) |
| `glue hold` / `unhold <pkg>` | Block or allow package upgrades |
| `glue home <pkg>` | Open package homepage in browser |
| `glue bucket add/list/update/check/known` | Manage Scoop buckets |
| `glue cache list/clear/gc/rebuild` | Cache index and orphan blob GC |
| `glue config get/set/unset/list` | Settings in `~/.glue/config.json` |
| `glue doctor` | Environment and dependency checks |
| `glue path show/check/setup` | PATH shim registration |
| `glue reset <pkg>` | Reset package to default version |
| `glue completion bash\|zsh\|fish\|powershell` | Generate shell completion script |

Run `glue --help` for the full tree.

## Shell completion

```powershell
# PowerShell (current session)
glue completion powershell | Out-String | Invoke-Expression

# Bash
source <(glue completion bash)

# Zsh
source <(glue completion zsh)
```

Package names, installed packages, bucket names, and config keys are completed dynamically when the engine index is available.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | Operation failed (install error, missing package, etc.) |
| `2` | Usage error (unknown command/flag, wrong arguments) |

## Terminal color

- `NO_COLOR` (any value) disables ANSI styling
- `FORCE_COLOR=1` enables color when supported
- When stderr is not a TTY (pipes, redirects), color is off unless `FORCE_COLOR` is set
- `glue config set color false` persists the preference in `config.json`

## Configuration

### GitHub mirror

```powershell
glue config set github_proxy https://ghproxy.net/
glue config unset github_proxy
```

Or session-only: `$env:GLUE_GITHUB_PROXY = "https://ghproxy.net/"`

### Parallel download

```powershell
glue config set parallel_download false
glue install git --no-parallel --force
```

## Architecture

```
glue CLI (this repo)
       │
       ├── github.com/gluestick-sh/core/engine   ← install/search/cache logic
       └── github.com/gluestick-sh/shim           ← shim.exe runner (sibling repo)
```

The CLI is a thin [Cobra](https://github.com/spf13/cobra) layer: parse flags, call `engine.*`, format output. No business logic should live in `cli/glue` beyond terminal UX. Shim management APIs live in `core/shim`; the standalone [shim](https://github.com/gluestick-sh/shim) repo builds the tiny `shim.exe` stub copied onto PATH.

## Build release binary

```powershell
go build -ldflags "-X github.com/gluestick-sh/cli/version.Version=0.1.0" -o glue.exe ./glue
.\glue.exe -v
```

Use your project's `build.ps1` or CI release workflow for commit hash and date injection.

## Development

```powershell
# With core as a sibling directory covered by ../go.work
go test ./glue/...
go build -o glue.exe ./glue
```

## Testing

- Unit tests: `go test ./glue/...`
- Manual smoke: `glue doctor`, `glue search git`, install in a temp `--root` (hidden flag for dev/benchmark)

## Related projects

- [core](https://github.com/gluestick-sh/core) — embeddable engine library
- [shim](https://github.com/gluestick-sh/shim) — dependency-free `shim.exe` runner
- [Scoop](https://github.com/ScoopInstaller/Scoop) — compatible bucket ecosystem

## License

MIT — see [LICENSE](LICENSE).
