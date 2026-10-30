# <img src="assets/appicon.png" width="32" height="32" alt="Gluestick icon" /> Gluestick

**Lightweight, efficient package manager based on Scoop ecosystem on Windows.**

Gluestick is a Windows package manager that installs, updates and manages Claude Code, Codex CLI and
other Windows software from 4,500+ Scoop packages. It reads the Scoop bucket ecosystem directly
while keeping its own workspace and cache, so an existing Scoop setup is left untouched.

- **Lightweight** — `glue.exe` and `shim.exe` together are about 3 MB, with no VC++ runtime to install.
- **Fast** — parallel downloads, content-addressed file reuse and a local cache.
- **Compatible** — Scoop buckets, software manifests and common installer scripts.
- **Safe** — hash verification, path safety checks and atomic updates.

## Contents

- [Quick start](#quick-start)
- [Installing packages](#installing-packages)
- [Templates](#templates)
- [CLI reference](#cli-reference)
- [Configuration](#configuration)
- [Data layout](#data-layout)
- [Building from source](#building-from-source)
- [Installing Gluestick](#installing-gluestick)
- [Performance](#performance)
- [Project layout](#project-layout)
- [Desktop app and plans](#desktop-app-and-plans)
- [Contributing](#contributing)
- [License](#license)

## Quick start

Open PowerShell and run:

```powershell
irm https://gluestick.sh/install.ps1 | iex
```

The script detects your architecture, installs 7-Zip and MinGit if they are missing, downloads
`glue.exe` and `shim.exe` into `%USERPROFILE%\.glue`, and adds that directory to your user `PATH`.
No administrator rights are needed. Prefer a graphical installer? Download one for amd64 or arm64
from [gluestick.sh/download](https://gluestick.sh/download.html).

Then open a new shell:

```powershell
glue install claude-code     # or ripgrep, fd, vscode, yazi, ...
glue list
```

For the full command list:

```powershell
glue --version
glue --help
```

## Installing packages

For standalone software, add a Scoop bucket and install the package directly. The `main` bucket is
already there from first run, so `glue bucket add main` is only needed after removing it:

```powershell
glue bucket add main
glue search ripgrep
glue install ripgrep
glue list
glue update --all
```

Package references can be `name` or `bucket/name`:

```powershell
glue install extras/vscode
glue install ripgrep fd jq        # multiple packages install concurrently
glue uninstall ripgrep
```

### Dependencies

Like Scoop, `glue install` installs a manifest's `depends` first, so an installer script that links
into another package's directory finds it in place. A dependency that is already installed is left
alone (it is never upgraded as a side effect), and `--independent` skips the expansion entirely.
`suggest` entries are never installed automatically: a successful install ends with the optional
companions and the command that adds each one.

### Installing a specific version

`pkg@version` installs any version the bucket ever published, and also works for uninstalling and
switching between *installed* versions: pass `pkg@version` to remove one specific version, and
switch away with `glue reset <pkg>@<version>` first if that version is currently active.

### Checking the environment

Run `glue doctor` to check the environment — long-path support, `git`, download tools, and other
dependencies — and to list installed packages whose manifest `depends` are not installed.

## Templates

A template is a named collection of packages for a particular domain or workflow, such as setting up
an AI coding environment. Inspect a template before installing it, then manage its packages together:

```powershell
glue template list
glue template show vibe-coding
glue template install vibe-coding
glue template update vibe-coding
glue template uninstall vibe-coding
```

Bundled templates cover the common setups. To add your own, drop a JSON file into
`<workspace>\templates\`:

```json
{
  "kind": "gluestick.template",
  "name": "Editor",
  "title": { "en": "Editor", "zh": "编辑器" },
  "summary": { "en": "Code editors", "zh": "代码编辑器" },
  "description": { "en": "Code editors and language servers.", "zh": "代码编辑器与语言服务器。" },
  "packages": [
    { "name": "vscode", "bucket": "extras", "label": { "en": "VS Code", "zh": "VS Code 编辑器" } },
    { "name": "nodejs-lts" }
  ]
}
```

## CLI reference

| Command | Description |
| --- | --- |
| `glue install <pkg>...` | Install one or more packages. Flags: `--arch`, `--version`, `--force`, `--interactive`, `--independent`, `--timings`. |
| `glue uninstall <pkg>` | Remove a package, or one version with `pkg@version`. |
| `glue update [pkg]...` | List available updates when called without arguments; `--all` upgrades everything. Alias: `up`. |
| `glue list` | List installed packages. Flags: `-d/--detailed`, `--all` (all versions). |
| `glue search <query>` | Search bucket manifests. Flags: `--bucket <name>` (repeatable), `--limit <n>`. |
| `glue info <pkg>...` | Show information about installed packages. |
| `glue depends <pkg>...` | Report missing dependencies and optional suggestions. |
| `glue hold <pkg>...` / `glue unhold <pkg>...` | Pin packages at their current version, or release the pin. |
| `glue reset <pkg>@<version>` | Switch to an installed version and rebuild that version's shims, `env_set` values and Start-menu shortcuts. An explicit version is required. |
| `glue doctor` | Run environment and dependency checks, including missing `depends`. |
| `glue bucket <sub>` | Manage buckets: `add`, `remove`, `list`, `update`, `check`, `deepen`, `known`. |
| `glue template <sub>` | Manage package templates: `list`, `show`, `install`, `update`, `uninstall`. |
| `glue cache <sub>` | Manage the cache index and content store: `list`, `clear`, `rebuild`, `gc`, `purge-partials`, `verify [--repair]`. |
| `glue config <sub>` | Manage configuration: `get`, `set`, `unset`, `list`. |
| `glue root <sub>` | Manage the workspace: `show`, `set <dir>`, `reset`. |
| `glue path <sub>` | Manage `PATH` integration: `show`, `check`, `setup`, `remove`. |
| `glue completion <shell>` | Print a completion script for `bash`, `zsh`, `fish` or `powershell`. |

Details behind the shorter rows:

- `install --arch <64bit|arm64|32bit>`, `--version <v>` (any published version, or one built from
  `autoupdate`), `--force` (reinstall),
  `--interactive` (see below), `--independent` (do not install `depends` first), `--timings`.
- `root set <dir>` creates the workspace and saves it for this installation
  (`<install dir>\data-root.txt`, plus `HKCU\Software\Gluestick` for standalone binaries).
- `completion` scripts call `glue __complete` back on every `TAB`, so they complete command and
  nested-command names, flags (plus `--arch` / `--bucket` values), package names and `bucket/name`,
  installed packages and their `pkg@version`, buckets, and templates — always from the current
  workspace.

Load the script once per shell session:

```powershell
# PowerShell — add to $PROFILE to keep it
glue completion powershell | Out-String | Invoke-Expression
```

```bash
# bash — add to ~/.bashrc to keep it
source <(glue completion bash)
```

zsh: save the output as a file named `_glue` on `$fpath`. fish: `glue completion fish | source`.

The script calls the `glue` you typed, so a local build works without being on `PATH`
(`.\target\release\glue.exe completion powershell | Out-String | Invoke-Expression`). The binary
that answers must have the hidden `__complete` command — if `PATH` still points at an older install,
typing bare `glue` yields no candidates, so upgrade it and load the script again.

### Install output and timings

Installs print a staged report (stage lines with `→`, indented detail lines) and a live progress bar
on terminals; when output is redirected, progress degrades to periodic plain lines so logs stay
readable. `glue install --timings` adds a machine-readable summary per package.

### Interactive installers

Most packages install silently. A few manifests only have a GUI wizard (an `.exe` installer with no
silent switch): `glue install <pkg> --interactive` runs their `installer.script` and pre/post-install
hooks with `$interactive = $true` — Scoop's contract for `if (!$interactive) { $args += '/S' }`. It
applies only to the command you run: `glue update`, template installs and scheduled bucket refreshes
always stay unattended, so no dialog appears on a window nobody is watching. `"innosetup": true`
packages ignore the flag — the installer is unpacked, never run.

## Configuration

```powershell
glue config list
glue config get proxy
glue config set proxy http://127.0.0.1:8080
glue config set proxy_packages claude-code,codex
glue config unset proxy
```

The settings live in `config.json` beside the program — under
`%LOCALAPPDATA%\Programs\Gluestick CLI` — and are program-scoped rather than workspace data, so
reinstalling or switching workspaces keeps them. A fresh installation reads an old workspace
`config.json` the first time it runs, then writes its own from then on. `GLUE_CONFIG_DIR` moves the
file elsewhere (development and tests); a standalone binary with no installation layout keeps it in
the workspace.

| Key | Purpose |
| --- | --- |
| `github_proxy` | GitHub mirror prefix used for GitHub downloads only (not an HTTP proxy; it cannot fetch non-GitHub hosts such as `storage.googleapis.com`). |
| `proxy` | Proxy mode: `auto` (default), `direct`, or an `http://` proxy URL. |
| `proxy_packages` | JSON array of package names (or `bucket/name`) whose downloads use `proxy`; every other download stays direct. |
| `self_update_check_url` | Opt-in endpoint (plain text or `{"version":"x.y.z"}`) that names the latest published Gluestick version. Unset (the default) makes no network call; the answer is cached for 24 h and only ever produces a one-line notice. |
| `bucket_sync_mode` | `auto` refreshes buckets automatically when due; `manual` (default) refreshes only on command. |
| `bucket_check_interval_minutes` | Minimum interval between automatic bucket refresh checks. |
| `parallel_download` | Enable parallel ranged downloads. |
| `download_workers` | Number of concurrent download workers. |
| `cancel_direct_max_mb` | Size ceiling (MiB, 0–32, default 16) for the single-GET path used by cancellable downloads. Larger files stay resumable through `.partial`; `0` forces the resumable path for every size. |
| `color` | Colored output for interactive terminals. |
| `verbose` | Verbose diagnostics. |

`cancel_direct_max_mb` trades resumability for latency. At or below the ceiling a cancellable
download is a single GET — faster on high-latency links, but a cancelled download restarts instead
of resuming. Above it the resumable `.partial` path is used.

### GitHub mirrors

On first run Gluestick probes built-in mirrors and direct GitHub access and keeps the fastest; set
one explicitly with `glue config set github_proxy …`. The prefix applies only to GitHub URLs —
`claude-code` is served from `storage.googleapis.com`, so reach it with `proxy` plus `proxy_packages`.

### Proxies

Downloads support HTTP `CONNECT` proxies; SOCKS and PAC are not supported. To route only some
downloads through a proxy, set `proxy` to a URL and list those packages in `proxy_packages`:

```json
{
  "proxy": "http://127.0.0.1:7897",
  "proxy_packages": ["claude-code"]
}
```

`proxy` defaults to `auto`, which also consults `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` and the
Windows system proxy.

## Data layout

The workspace is the only directory Gluestick owns for **data** (default `%USERPROFILE%\.glue`; use
`glue root show` to see it or `glue root set <dir>` to move it):

```
<workspace>/
  apps/<package>/<version>/     installed package files (current/ points at the active version)
  shims/                        generated shim launchers
  shims-meta/<name>.json        shim target metadata (read by shim.exe)
  buckets/<name>/bucket/        cloned Scoop bucket manifests
  bin/                          bootstrapped helpers (7-Zip, MinGit, innounp, WiX dark)
  cache/                        SQLite cache index and the cross-process lock
  store/                        content-addressed store (SHA-256)
  templates/                    user templates (replace bundled ones by name)
  persist/<package>/            persistent package data
```

Package files are hard-linked from `store/` into `apps/`, so a reinstall normally reuses cached
content instead of downloading it again. Missing helper tools are bootstrapped into `bin/` on demand.

## Building from source

Get `rustup-init.exe` from [rustup.rs](https://rustup.rs/), run it, and accept the default
("Proceed with standard installation"). That installs the stable MSVC toolchain and puts `cargo`,
`rustc`, `rustup`, and `rustfmt` on `PATH`; if no Visual C++ build tools are present, `rustup-init`
offers to install them for you.

The repository pins Rust 1.99.1 with `rustfmt`, `clippy` and both MSVC targets in
[`rust-toolchain.toml`](rust-toolchain.toml), and rustup installs exactly that on the first `cargo`
command run from the checkout. The MSVC toolchain also needs the Visual C++ Build Tools with the
Windows SDK ("Desktop development with C++" in the Visual Studio Installer), including the C++
toolsets for the architectures you build — on an x64 host, x64 plus ARM64. Packaging additionally
needs NSIS 3.x for `makensis.exe`.

```powershell
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
installer\cli\build.ps1 -Arch amd64
```

The output is `installer\cli\output\GluestickSetup-<arch>.exe` plus a matching `.sha256` file.

Toolchain details, cross-building one architecture into the layout the installer expects, the
`CARGO_TARGET_DIR` / `RUSTC` pinning that goes with it, and packaging both architectures are in
[CONTRIBUTING.md](CONTRIBUTING.md). What the resulting package does when it runs is in
[Installing Gluestick](#installing-gluestick).

## Installing Gluestick

There are two ways to install the CLI, and they keep the program files in different places. Both
target Windows 10/11 on amd64 and arm64, and both are per-user: no administrator rights, no
machine-wide changes, and no touch of an existing Scoop installation.

| | PowerShell script | GUI installer |
| --- | --- | --- |
| Program files | `%USERPROFILE%\.glue` | `%LOCALAPPDATA%\Programs\Gluestick CLI` |
| `config.json` | `<workspace>` | beside `glue.exe` |
| Apps & Features entry | — | yes, with `Uninstall.exe` |
| Workspace | `%USERPROFILE%\.glue` | `%USERPROFILE%\.glue` |

`glue path setup` adds two entries to the user `PATH` — `<workspace>\shims` and the directory
holding `glue.exe` — so the two layouts differ only in where the binaries sit.

### GUI installer

Built from `installer\cli\build.ps1`, the GUI installer puts the binaries, the `.glue-install`
marker, `data-root.txt` and `config.json` in the installation directory, and keeps the workspace
(`apps/`, `shims/`, `store/`, `buckets/`, `cache/`) in `%USERPROFILE%\.glue`. It writes
`Uninstall.exe` beside the binaries and registers it in Apps & Features; uninstalling removes only
the program files — including `config.json` — never the workspace.

The components page installs the CLI (mandatory) and the Vibe Coding template (selected by default,
`/NOTEMPLATE=1` to skip). A package page then lists `claude-code`, `codex` and `extras/magpie-ai`:
all three are selected by default, and only the selected packages are installed.

The installer checks for at least 64 MB free — or 1 GB when the template is selected — before
writing anything. A failed template download is reported without rolling back the CLI. The GUI shows
the template's progress in the details pane, and the whole run is written to
`<install dir>\install.log`, which the finish page offers to open.

Upgrading means running a newer installer; Gluestick never downloads or replaces its own executable.
To build the installer yourself, see [Building from source](#building-from-source).

## Performance

The standalone `gluestick-bench` harness runs glue and Scoop against identical fixtures and reports
medians. Everyday commands — startup, `search`, `list` — finish in tens of milliseconds, roughly 10×
faster than Scoop, and installs are several times faster. Methodology, fixtures and full numbers
live in that repository's own documentation.

## Project layout

```
Cargo.toml                  workspace, shared dependencies, and release profile
.cargo/config.toml          bundled-SQLite compile flags (size trimming)
crates/gluestick-cli/       the `glue` command line (clap), built on gluestick-core
crates/gluestick-core/      engine: manifests, downloader, CAS store, cache index, shims
crates/gluestick-shim/      shim runner (`shim.exe`), copied next to each installed tool
installer/cli/              MSVC build script, NSIS definition, and generated payload/output
docs/                       installer reference
```

The CLI holds no package logic of its own: argument parsing, progress rendering, and the table
output live in `glue`, while every operation is an `Engine` call into `gluestick-core`.

## Desktop app and plans

The `glue` CLI in this repository is the engine. The Gluestick Desktop app puts buckets, package
browse, templates, installed packages, updates, storage and activity in one window, with the 7
official themes, 11 official templates and local snapshot backup and rollback.

The Community edition is free forever. Pro adds cloud backup and primary/secondary device
software-list sync; Team adds environment health monitoring, shared configuration templates and
member permissions.

## Contributing

Contributions are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers the toolchain, the checks to
run before opening a pull request, per-architecture builds and packaging.

## License

MIT. See [LICENSE](LICENSE).
