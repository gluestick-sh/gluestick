# Contributing

Contributions are welcome. Open an issue for anything that changes behaviour, and keep changes
covered by tests.

## Before you start

```powershell
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
```

`cargo test --workspace` covers every crate. The engine (`gluestick-core`) carries the third-party
dependencies — blocking `ureq` over schannel, bundled SQLite via `rusqlite`, `sevenz-rust2`,
`zlib-rs`, `crc32fast`, and the hashing crates. The shim deliberately stays on `serde` plus
`serde_json`: a copy of it sits next to every installed tool, so its size is part of the product.
SQLite is compiled from source with the trimming flags in
[`.cargo/config.toml`](.cargo/config.toml), so the first build also needs the MSVC C toolchain
(`cl.exe`).

## Toolchain

The repository pins its toolchain in [`rust-toolchain.toml`](rust-toolchain.toml): Rust 1.99.1,
`rustfmt` and `clippy`, and both MSVC targets. Rustup installs exactly that on the first `cargo`
command run from the checkout, so no separate step is required — that download is rustup's
auto-install, which `rustup set auto-install disable` turns off.

To install it up front instead:

```powershell
rustup toolchain install 1.99.1 --profile minimal `
    --component rustfmt clippy `
    --target x86_64-pc-windows-msvc aarch64-pc-windows-msvc
```

The pinned channel resolves to the toolchain matching rustup's default host. On an ARM64 machine
that is `1.99.1-aarch64-pc-windows-msvc`, which builds both architectures because
`rust-toolchain.toml` installs the x64 standard library next to the native one — a second, x64 host
toolchain is not needed. If rustup picked a host triple you do not want, change it with
`rustup set default-host <triple>`.

```powershell
rustup show          # host triple, installed toolchains, active toolchain
```

### Visual C++ build tools

The MSVC toolchain needs the Visual C++ Build Tools with the Windows SDK. Install them through the
Visual Studio Installer (workload "Desktop development with C++"), including the C++ toolsets for
the architectures you build:

- **x64 host** — toolsets for x64 and ARM64 (the amd64 payload uses x64, the arm64 payload
  cross-links with the ARM64 toolset).
- **ARM64 host** — toolsets for ARM64 and x64.

`installer\cli\build.ps1` selects the matching `vcvarsall.bat` host/target pair and fails with a
clear error when the required `link.exe` is missing.

## Building one architecture

Without an architecture or target directory, `cargo build` writes to `target\debug`, or to
`target\release` with `--release`:

```powershell
cargo build -p gluestick-cli
cargo build -p gluestick-cli --release
cargo build -p gluestick-shim --release     # shim.exe
```

Those binaries take whatever architecture the shell resolves `cargo`/`rustc` to: with rustup's
shims that is the host named in `rust-toolchain.toml`, while a standalone Rust installation earlier
in `PATH` produces its own instead.

To build one specific architecture into the layout the installer script expects, point
`CARGO_TARGET_DIR` at the architecture directory and pass the target triple. Builds are debug by
default; add `--release` for an optimised build.

```powershell
# arm64  → target\cli-arm64-msvc\aarch64-pc-windows-msvc\debug\glue.exe
$env:CARGO_TARGET_DIR = 'target\cli-arm64-msvc'
$env:RUSTC = (rustup which rustc --toolchain 1.99.1)
cargo build -p gluestick-cli --target aarch64-pc-windows-msvc

# amd64  → target\cli-amd64-msvc\x86_64-pc-windows-msvc\debug\glue.exe
$env:CARGO_TARGET_DIR = 'target\cli-amd64-msvc'
$env:RUSTC = (rustup which rustc --toolchain 1.99.1-x86_64-pc-windows-msvc)
cargo build -p gluestick-cli --target x86_64-pc-windows-msvc
```

The `--target` flag is what creates the `<triple>\` level; without it the binary lands in
`target\cli-<arch>-msvc\debug\` and `installer\cli\build.ps1 -SkipBuild` will not find it.

`RUSTC` pins the build to the toolchain named in [`rust-toolchain.toml`](rust-toolchain.toml);
without it, an unrelated standalone Rust installation earlier in `PATH` is picked up instead and a
cross build fails with `E0463: can't find crate for std`. Keep the two `--toolchain` values in sync
with the pinned channel. For a release build the installer script additionally sets
`RUSTFLAGS=-C target-feature=+crt-static`; add the same flag when you need byte-for-byte parity with
the packaged `glue.exe`.

## Packaging

Packaging needs NSIS 3.x for `makensis.exe` ([nsis.sourceforge.io](https://nsis.sourceforge.io/)):

```powershell
installer\cli\build.ps1                 # both architectures
installer\cli\build.ps1 -Arch amd64
installer\cli\build.ps1 -Arch arm64
```

To package an architecture you already built, skip recompiling:

```powershell
installer\cli\build.ps1 -Arch arm64 -SkipBuild
```

The output is `installer\cli\output\GluestickSetup-<arch>.exe` plus a matching `.sha256` file. What
that package does when it runs is documented in [the README](README.md#installing-gluestick).

## Releases

Pushing a `v*` tag (for example `v0.1.1`) triggers
[`.github/workflows/release.yml`](.github/workflows/release.yml), which builds both architectures on
a GitHub Actions Windows runner, packages the NSIS installers, and publishes
`GluestickSetup-amd64.exe` and `GluestickSetup-arm64.exe` (plus their `.sha256` files) as a GitHub
release. The tag's version must match `[workspace.package].version` in `Cargo.toml`, so bump that
first and push the matching tag.

After the GitHub release is created, the same workflow copies those four files over SSH to
`https://gluestick.sh/releases/cli/`. The `workflow_dispatch` trigger re-syncs an existing release's
files to the site without rebuilding, using the `tag` input. The SSH step needs these repository
secrets: `GLUESTICK_SSH_HOST`, `GLUESTICK_SSH_PORT`, `GLUESTICK_SSH_USER`, `GLUESTICK_SSH_KEY` and
`GLUESTICK_SSH_PATH`; `GLUESTICK_SSH_PASSPHRASE` and `GLUESTICK_SSH_KNOWN_HOSTS` are optional.

## Documentation

`README.md` and `README.zh-CN.md` are kept in sync section by section — change both in the same
pull request. Long reference material lives in `docs/` and is linked from both.
