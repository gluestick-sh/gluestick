# core

> Gluestick embeddable engine — the Windows package management core library, compatible with [Scoop](https://github.com/ScoopInstaller/Scoop) buckets and manifests.

**core** is the shared library for the official [cli](https://github.com/gluestick-sh/cli) (the `glue` command) and third-party integrations. It handles manifest parsing, CAS downloads, install/uninstall, shims, the bucket registry, and the SQLite cache index.

## Platform

**Windows only.** This library targets Windows exclusively and does not support other platforms.

## Features

- **Scoop-compatible** — JSON manifests, standard `buckets/` layout
- **Content-addressable store (CAS)** — SHA-256 blobs, hardlink installs when possible
- **Parallel downloads** — HTTP range resume, multi-connection for large files (≥32 MiB), mirror fallback
- **SQLite cache index** — package metadata, install history, activity log
- **Embeddable API** — `engine` package for CLI, GUI, or automation
- **On-demand bootstrap** — MinGit, 7-Zip, WiX dark, innounp under the data directory

## Install

```bash
go get gluestick.sh/core@v0.1.0
```

Requires **Go 1.26+**.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"gluestick.sh/core/engine"
)

func main() {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".glue")

	eng, err := engine.NewEngine(&engine.EngineConfig{
		RootDir:  root,
		Workers:  4,
		Parallel: true,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer eng.Close()

	req := &engine.InstallRequest{
		Request: engine.Request{Name: "git"},
	}

	result, err := eng.Install(context.Background(), req, engine.NewSilentReporter())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("installed %s %s in %v\n", result.Name, result.Version, result.Duration)
}
```

Data directory layout (default `~/.glue`):

| Path | Purpose |
| --- | --- |
| `apps/` | Installed package versions |
| `buckets/` | Cloned Scoop bucket repos |
| `cache/` | SQLite index + cache metadata |
| `store/` | CAS blobs |
| `shims/` | PATH shims |
| `config.json` | User settings |
| `device.json` | Stable device identity for this data root (CLI/Desktop sync) |

Environment snapshots (`gluestick.environment-snapshot`) are portable JSON files
produced by `Engine.ExportCoreSnapshot` / `glue snapshot export` — they are not
stored under the data root by default.

## Public API

Import **`gluestick.sh/core/engine`** for install, uninstall, search, catalog, cache,
doctor, and progress reporting. Advanced use cases may import `manifest`, `bucket`,
`config`, `downloader`, `store`, `cache`, or `shim` directly.

Do not import `engine/internal/*` or `apps` — use `engine` helpers instead
(`EnsureInstalledVersion`, `ListInstalledAllVersions`, `ParseInstallFilePath`, …).

## Configuration

- **Data root**: `EngineConfig.RootDir` (default `~/.glue`)
- **Workers / parallel**: `EngineConfig.Workers`, `EngineConfig.Parallel`
- **GitHub mirror**: `~/.glue/config.json` (`github_proxy`) or `GLUE_GITHUB_PROXY`; call `engine.ReloadGitHubProxies()` after changes

## Development

```powershell
git clone https://github.com/gluestick-sh/core.git
cd core
go test ./... -count=1
```

## Documentation

- [Security model](docs/security.md)
- [Contributing](docs/CONTRIBUTING.md)

## Related projects

- [Scoop](https://github.com/ScoopInstaller/Scoop) — manifest format reference

## License

MIT — see [LICENSE](LICENSE).
