//! The `glue` command-line tool.
//!
//! Exit-code conventions: 0 success, 1 operation failure, 2 usage error
//! (clap default).
//!
//! Command tree: install / uninstall / update / list / search / doctor / template
//! / bucket / cache / config / root / hold / unhold / depends / info / reset /
//! completion / path, plus the hidden `shim-run`, `tee` and `__complete`
//! helpers.

use std::collections::{HashMap, VecDeque};
use std::io::{BufRead, IsTerminal, Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command as ProcessCommand, ExitCode, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use clap::{Command, CommandFactory, Parser, Subcommand};

use gluestick_core::bootstrap;
use gluestick_core::bucket;
use gluestick_core::cache::refs::CachePhase;
use gluestick_core::catalog::all_catalog_entries;
use gluestick_core::config;
use gluestick_core::downloader::{
    normalize_user_workers, DEFAULT_WORKERS, MAX_USER_WORKERS, MIN_USER_WORKERS,
};
use gluestick_core::engine::services::EngineConfig;
use gluestick_core::engine::{
    Engine, EngineError, InstallOptions, InstallOutcome, PackageUpdate, ProgressEvent, Services,
};
use gluestick_core::git;
use gluestick_core::pathenv;
use gluestick_core::paths;
use gluestick_core::shim;
use gluestick_core::templates;

#[derive(Parser, Debug)]
#[command(
    name = "glue",
    version,
    about = "The official glue command line - Scoop-compatible, powered by the gluestick core engine"
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Commands>,

    /// Override the workspace (development only)
    #[arg(long, global = true, hide = true)]
    root: Option<PathBuf>,
}

#[derive(Subcommand, Debug)]
enum Commands {
    /// Install a package
    Install {
        /// Package reference(s): `pkg`, `bucket/pkg`, or `pkg@version`.
        /// An older version resolves from the bucket's git history when the
        /// current manifest no longer holds it (`glue bucket deepen <name>`
        /// once if the bucket is a shallow clone).
        /// Two or more packages install with their downloads/extracts
        /// running concurrently.
        packages: Vec<String>,
        /// Architecture override (64bit, arm64, 32bit)
        #[arg(long, default_value = "")]
        arch: String,
        /// Version pin (only a version the bucket manifest declares)
        #[arg(long, default_value = "")]
        version: String,
        /// Force reinstall
        #[arg(long)]
        force: bool,
        /// Print per-phase install timings (download/store/extract/link/shim)
        #[arg(long)]
        timings: bool,
        /// Run the installer interactively (like Scoop's --interactive)
        #[arg(short, long)]
        interactive: bool,
        /// Do not install the manifest's `depends` first (Scoop's
        /// `--independent`)
        ///
        /// Long-only: `-i` is `--interactive` here.
        #[arg(long)]
        independent: bool,
    },
    /// Uninstall a package (or `pkg@version` to delete one old version)
    Uninstall {
        /// Package name, or `package@version` to remove a single installed
        /// version (the active version must be switched away from first)
        package: String,
    },
    /// Update a package to the latest bucket version
    #[command(alias = "up")]
    Update {
        /// Package name(s); omit to only list available updates
        #[arg(num_args = 1..)]
        packages: Vec<String>,
        /// Update all outdated packages
        #[arg(short, long)]
        all: bool,
    },
    /// List installed packages
    List {
        /// Show detailed information
        #[arg(short, long)]
        detailed: bool,
        /// Show all installed versions
        #[arg(long)]
        all: bool,
        /// Filter by package name(s)
        packages: Vec<String>,
    },
    /// Search available packages
    Search {
        /// Search query
        query: String,
        /// Restrict to specific buckets (repeatable)
        #[arg(long = "bucket")]
        buckets: Vec<String>,
        /// Maximum number of results
        #[arg(long, default_value_t = 50)]
        limit: usize,
    },
    /// Environment and dependency checks
    Doctor,
    /// Manage templates (named package bundles stored in the workspace)
    Template {
        #[command(subcommand)]
        command: TemplateCommands,
    },
    /// Manage buckets (package repositories)
    Bucket {
        #[command(subcommand)]
        command: BucketCommands,
    },
    /// Manage download cache (index and store blobs)
    Cache {
        #[command(subcommand)]
        command: CacheCommands,
    },
    /// Manage glue configuration
    Config {
        #[command(subcommand)]
        command: ConfigCommands,
    },
    /// Manage the workspace (apps, shims, store, buckets)
    Root {
        #[command(subcommand)]
        command: Option<RootCommands>,
    },
    /// Prevent package from being upgraded
    Hold {
        /// Package name(s)
        #[arg(num_args = 1.., required = true)]
        packages: Vec<String>,
    },
    /// Allow package upgrades again
    Unhold {
        /// Package name(s)
        #[arg(num_args = 1.., required = true)]
        packages: Vec<String>,
    },
    /// Show missing dependencies and optional suggestions for packages
    Depends {
        /// Package reference(s)
        #[arg(num_args = 1.., required = true)]
        packages: Vec<String>,
    },
    /// Show information about installed packages
    Info {
        /// Package name(s)
        #[arg(num_args = 1.., required = true)]
        packages: Vec<String>,
    },
    /// Switch to an installed version and rebuild its shims and shortcuts
    /// (requires `pkg@version`)
    Reset {
        /// Package reference(s) in `pkg@version` form
        #[arg(num_args = 1.., required = true)]
        packages: Vec<String>,
    },
    /// Generate shell completion script
    Completion {
        /// Shell: bash | zsh | fish | powershell
        #[arg(value_parser = ["bash", "zsh", "fish", "powershell"])]
        shell: String,
    },
    /// Internal: answer one shell completion request
    ///
    /// The scripts `glue completion <shell>` prints call this back on every
    /// <TAB>, so candidates come from the same engine the command itself uses
    /// (bucket catalog, installed versions) and never go stale the way a
    /// generated name list would.
    #[command(hide = true, name = "__complete")]
    Complete {
        /// 0-based index of the word being completed
        index: usize,
        /// Command line words; the word being completed is `words[index]`
        #[arg(trailing_var_arg = true, allow_hyphen_values = true)]
        words: Vec<String>,
    },
    /// Manage PATH integration
    Path {
        #[command(subcommand)]
        command: PathCommands,
    },
    /// Internal command to run a shim (used by generated shims)
    #[command(hide = true)]
    ShimRun {
        /// Shim name
        name: String,
        /// Arguments forwarded to the target
        args: Vec<String>,
    },
    /// Internal: run a program and copy its output to a log file
    #[command(hide = true, name = "tee")]
    Tee {
        /// File to append the output to
        log: PathBuf,
        /// Program to run
        program: PathBuf,
        /// Arguments forwarded to the program
        #[arg(trailing_var_arg = true, allow_hyphen_values = true)]
        args: Vec<String>,
    },
}

#[derive(Subcommand, Debug)]
enum BucketCommands {
    /// Add a bucket
    Add {
        /// Bucket name
        name: String,
        /// Repository URL (optional; falls back to known buckets)
        repository: Option<String>,
    },
    /// Remove buckets
    Remove {
        /// Bucket name(s)
        #[arg(num_args = 1.., required = true)]
        names: Vec<String>,
    },
    /// List all buckets
    List,
    /// Update buckets (fetch latest manifests)
    Update {
        /// Bucket name(s); empty updates all
        names: Vec<String>,
    },
    /// Check whether buckets have upstream updates
    Check {
        /// Bucket name(s); empty checks all
        names: Vec<String>,
    },
    /// Fetch the bucket's full git history (needed for `install pkg@oldversion`)
    ///
    /// Buckets are cloned with `--depth 1`, which keeps adding one fast but
    /// leaves no older manifests. This is the one-time fix: it downloads the
    /// rest of the history, after which `glue install pkg@<old version>` works
    /// offline.
    Deepen {
        /// Bucket name(s); empty deepens every bucket
        names: Vec<String>,
    },
    /// List known buckets
    Known,
}

#[derive(Subcommand, Debug)]
enum TemplateCommands {
    /// List templates (official plus files in <root>\templates)
    List,
    /// Show a template and its packages
    Show {
        /// Template name
        name: String,
    },
    /// Install every package in a template
    Install {
        /// Template name
        name: String,
        /// Reinstall packages that are already installed
        #[arg(long)]
        force: bool,
    },
    /// Update the installed packages of a template
    Update {
        /// Template name
        name: String,
    },
    /// Uninstall the installed packages of a template
    Uninstall {
        /// Template name
        name: String,
    },
}

#[derive(Subcommand, Debug)]
enum CacheCommands {
    /// List cached packages
    List,
    /// Drop reinstall metadata for packages that are no longer installed
    ///
    /// Installed packages are skipped, since their index rows carry the
    /// version lock and install metadata. Nothing is freed from disk: the
    /// store blobs stay hardlinked into apps/. Use `glue cache gc` to reclaim
    /// space instead.
    Clear {
        /// Clear every uninstalled package's index rows
        #[arg(short, long)]
        all: bool,
        /// Package name(s)
        packages: Vec<String>,
    },
    /// Rebuild cache index from installed apps (scans hardlinks)
    Rebuild,
    /// Remove unreferenced store blobs (orphan GC)
    Gc,
    /// Delete resumable download fragments under `store\.partial`
    PurgePartials,
    /// Re-hash store blobs and report corrupt objects
    Verify {
        /// Delete corrupt blobs so the next install re-downloads them
        #[arg(long)]
        repair: bool,
    },
}

#[derive(Subcommand, Debug)]
enum ConfigCommands {
    /// Get a configuration value
    Get {
        /// Configuration key
        key: String,
    },
    /// Set a configuration value
    Set {
        /// Configuration key
        key: String,
        /// Value
        value: String,
    },
    /// Unset a configuration value
    Unset {
        /// Configuration key
        key: String,
    },
    /// List all configuration values
    List,
}

#[derive(Subcommand, Debug)]
enum RootCommands {
    /// Print the effective workspace and where it came from
    Show,
    /// Create a workspace and make every future glue command use it
    Set {
        /// Directory holding apps, shims, store and buckets
        path: PathBuf,
        /// Move a pre-shared-workspace installation's data into `path`
        ///
        /// Earlier versions kept the workspace in the installation directory.
        /// The installer passes this so that data follows the binaries to the
        /// shared location instead of being stranded. Entries that already
        /// exist in `path` are left alone: two workspaces are never merged
        /// automatically.
        #[arg(long)]
        adopt: bool,
    },
    /// Point every installation back at `%USERPROFILE%\.glue`
    Reset,
}

#[derive(Subcommand, Debug)]
enum PathCommands {
    /// Show the PATH entry for glue
    Show,
    /// Check if Glue is in PATH
    Check,
    /// Add glue shims to the front of PATH (no admin required on Windows)
    Setup,
    /// Remove glue's PATH entries (used by the uninstaller)
    Remove,
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    let root = cli.root.clone();
    // Completion runs on every <TAB>: it prints only the candidates and skips
    // the trailing blank line below, which would otherwise become an empty
    // completion entry.
    if let Some(Commands::Complete { index, words }) = &cli.command {
        return complete_cmd(*index, words, root.clone());
    }
    let data_root = resolve_root(root.clone());
    let color_cfg = config::read_basics(&data_root)
        .ok()
        .and_then(|basics| basics.color);
    init_color(color_cfg);
    // A self-update *notice* only: the check is opt-in through
    // `self_update_check_url` and never downloads or replaces anything. Tab
    // completion runs on every <TAB> and must stay silent and fast, and the
    // same is true of `completion` (its output is a script) and of the
    // shim/tee helpers, whose stderr belongs to the wrapped program rather
    // than to Gluestick.
    let allow_update_check = !matches!(
        cli.command.as_ref(),
        Some(Commands::Complete { .. })
            | Some(Commands::Completion { .. })
            | Some(Commands::ShimRun { .. })
            | Some(Commands::Tee { .. })
    );
    let notice = if allow_update_check {
        gluestick_core::self_update::check(&data_root, false)
    } else {
        None
    };
    let code = match cli.command {
        Some(Commands::Install {
            packages,
            arch,
            version,
            force,
            timings,
            interactive,
            independent,
        }) => install_cmd(
            packages,
            arch,
            version,
            InstallFlags {
                force,
                timings,
                interactive,
                independent,
            },
            root,
        ),
        Some(Commands::Uninstall { package }) => uninstall_cmd(package, root),
        Some(Commands::Update { packages, all }) => update_cmd(packages, all, root),
        Some(Commands::List {
            detailed,
            all,
            packages,
        }) => list_cmd(detailed, all, packages, root),
        Some(Commands::Search {
            query,
            buckets,
            limit,
        }) => search_cmd(query, buckets, limit, root),
        Some(Commands::Doctor) => doctor_cmd(root),
        Some(Commands::Template { command }) => template_cmd(command, root),
        Some(Commands::Bucket { command }) => bucket_cmd(command, root),
        Some(Commands::Cache { command }) => cache_cmd(command, root),
        Some(Commands::Config { command }) => config_cmd(command, root),
        Some(Commands::Root { command }) => root_cmd(command, root),
        Some(Commands::Hold { packages }) => hold_cmd(packages, true, root),
        Some(Commands::Unhold { packages }) => hold_cmd(packages, false, root),
        Some(Commands::Depends { packages }) => depends_cmd(packages, root),
        Some(Commands::Info { packages }) => info_cmd(packages, root),
        Some(Commands::Reset { packages }) => reset_cmd(packages, root),
        Some(Commands::Completion { shell }) => completion_cmd(&shell),
        // Answered before this match, so completion output has no blank line.
        Some(Commands::Complete { .. }) => ExitCode::SUCCESS,
        Some(Commands::Path { command }) => path_cmd(command, root),
        Some(Commands::ShimRun { name, args }) => shim_run_cmd(&name, &args),
        Some(Commands::Tee { log, program, args }) => tee_cmd(&log, &program, &args),
        None => {
            let _ = Cli::command().print_help();
            ExitCode::SUCCESS
        }
    };
    // The notice goes to stderr: `glue config get … | …` and shell completion
    // must keep stdout clean, and only a successful command is followed by the
    // hint — a failed install already has enough to say. `check()` itself is
    // cached for 24 h, so the short fetch timeouts only bite on the first
    // command of the day, and only when the key is set.
    if code == ExitCode::SUCCESS {
        if let Some(notice) = &notice {
            eprintln!(
                "note: Gluestick {} is available (you have {}); run a newer installer to upgrade",
                notice.latest, notice.current
            );
        }
    }
    // One blank line after every command so the shell prompt does not sit
    // directly against the command's last output line.
    println!();
    code
}

// ---------------------------------------------------------------------------
// Terminal color (color.go / color_windows.go / terminal.go)
// ---------------------------------------------------------------------------

static COLOR_ENABLED: std::sync::OnceLock<bool> = std::sync::OnceLock::new();

/// Terminal color resolution: `NO_COLOR`, `FORCE_COLOR`, stderr TTY, config.
fn resolve_color(config_enabled: Option<bool>) -> bool {
    if std::env::var_os("NO_COLOR").is_some_and(|v| !v.to_string_lossy().trim().is_empty()) {
        return false;
    }
    if let Some(v) = std::env::var_os("FORCE_COLOR") {
        let v = v.to_string_lossy();
        let v = v.trim();
        if !v.is_empty() {
            return !(v == "0" || v.eq_ignore_ascii_case("false") || v.eq_ignore_ascii_case("no"));
        }
    }
    if !std::io::stderr().is_terminal() {
        return false;
    }
    config_enabled.unwrap_or(true)
}

fn init_color(config_enabled: Option<bool>) {
    let enabled = resolve_color(config_enabled);
    let _ = COLOR_ENABLED.set(enabled);
    if enabled {
        enable_console_vt();
    }
}

fn color_enabled() -> bool {
    *COLOR_ENABLED.get_or_init(|| true)
}

/// Green check mark for successes (plain `ok` when colors are off).
fn mark_success() -> &'static str {
    if color_enabled() {
        "\x1b[32mok\x1b[0m"
    } else {
        "ok"
    }
}

/// Red cross for failures (plain `x` when colors are off).
fn mark_fail() -> &'static str {
    if color_enabled() {
        "\x1b[31mx\x1b[0m"
    } else {
        "x"
    }
}

/// Internal context prefixes the engine prepends to an error. They say where
/// the failure came from, not what the user can act on, so the CLI strips them:
/// `resolve package: find manifest: manifest not found: aaa` is shown as
/// `manifest not found: aaa`.
const ERROR_CONTEXT_PREFIXES: [&str; 2] = ["resolve package: ", "find manifest: "];

/// The user-facing form of an error message (see [`ERROR_CONTEXT_PREFIXES`]).
fn clean_error(message: &str) -> String {
    let mut text = message.trim();
    while let Some(rest) = ERROR_CONTEXT_PREFIXES
        .iter()
        .find_map(|prefix| text.strip_prefix(prefix))
    {
        text = rest.trim_start();
    }
    text.to_string()
}

/// Set once the progress callbacks have printed a failure, so the command does
/// not print the error it receives a second time.
static FAILURE_REPORTED: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

/// Whether the live link-progress line is on screen. It is drawn without a
/// trailing newline, so whatever prints next has to close it first.
static LINK_LINE_OPEN: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

/// Enable ANSI/VT processing on the console.
#[cfg(windows)]
fn enable_console_vt() {
    use windows_sys::Win32::System::Console::{
        GetConsoleMode, GetStdHandle, SetConsoleMode, ENABLE_VIRTUAL_TERMINAL_PROCESSING,
        STD_ERROR_HANDLE, STD_OUTPUT_HANDLE,
    };
    for id in [STD_OUTPUT_HANDLE, STD_ERROR_HANDLE] {
        // SAFETY: the standard handles are owned by the process; `mode` is a
        // valid out-pointer.
        let handle = unsafe { GetStdHandle(id) };
        if handle.is_null() {
            continue;
        }
        let mut mode = 0u32;
        if unsafe { GetConsoleMode(handle, &mut mode) } != 0 {
            unsafe { SetConsoleMode(handle, mode | ENABLE_VIRTUAL_TERMINAL_PROCESSING) };
        }
    }
    enable_vt_registry();
}

#[cfg(not(windows))]
fn enable_console_vt() {}

/// Makes classic cmd.exe honour ANSI for
/// future sessions too (`HKCU\Console\VirtualTerminalLevel = 1`).
#[cfg(windows)]
fn enable_vt_registry() {
    use std::ptr;

    use windows_sys::Win32::System::Registry::{
        RegCloseKey, RegOpenKeyExW, RegQueryValueExW, RegSetValueExW, HKEY, HKEY_CURRENT_USER,
        KEY_QUERY_VALUE, KEY_SET_VALUE, REG_DWORD,
    };

    fn wide(s: &str) -> Vec<u16> {
        s.encode_utf16().chain(std::iter::once(0)).collect()
    }

    let subkey = wide("Console");
    let mut key: HKEY = ptr::null_mut();
    // SAFETY: `key` is a valid out-pointer and `subkey` is NUL-terminated.
    let rc = unsafe {
        RegOpenKeyExW(
            HKEY_CURRENT_USER,
            subkey.as_ptr(),
            0,
            KEY_QUERY_VALUE | KEY_SET_VALUE,
            &mut key,
        )
    };
    if rc != 0 {
        return;
    }
    let name = wide("VirtualTerminalLevel");
    let mut value: u32 = 0;
    let mut size: u32 = 4;
    let mut kind: u32 = 0;
    // SAFETY: `key` is open; `value`/`size` describe a u32 REG_DWORD buffer.
    let rc = unsafe {
        RegQueryValueExW(
            key,
            name.as_ptr(),
            ptr::null_mut(),
            &mut kind,
            &mut value as *mut u32 as *mut u8,
            &mut size,
        )
    };
    if rc != 0 || value == 0 {
        let one: u32 = 1;
        // SAFETY: same key and buffer; writing a 4-byte REG_DWORD.
        unsafe {
            RegSetValueExW(
                key,
                name.as_ptr(),
                0,
                REG_DWORD,
                &one as *const u32 as *const u8,
                4,
            )
        };
    }
    // SAFETY: `key` came from a successful `RegOpenKeyExW`.
    unsafe { RegCloseKey(key) };
}

/// Open the engine for a command that never reads installed state
/// (`search`, `bucket`, `config`, `doctor`, `cache gc` …).
///
/// The startup heal of `installed_packages` is skipped: it costs a stat per
/// installed package plus a small table scan, and these commands would throw
/// the result away. Commands that do read installed state must use
/// [`open_engine_healing`].
fn open_engine(root_override: Option<PathBuf>) -> Result<Engine, ExitCode> {
    open_engine_config(root_override, false)
}

/// Open the engine for a command that reads or writes installed state
/// (`list`, `info`, `uninstall`, `hold`, `cache clear`, …).
fn open_engine_healing(root_override: Option<PathBuf>) -> Result<Engine, ExitCode> {
    open_engine_config(root_override, true)
}

fn open_engine_config(
    root_override: Option<PathBuf>,
    sync_installed: bool,
) -> Result<Engine, ExitCode> {
    let root = resolve_root(root_override);
    let mut config = EngineConfig::new(&root);
    config.sync_installed = sync_installed;
    Engine::with_config(config).map_err(|e| {
        eprintln!("error: failed to open engine at {}: {}", root.display(), e);
        ExitCode::FAILURE
    })
}

/// Open the engine for commands that resolve packages out of bucket metadata
/// (`search`, `info`, `depends`): the first bucket is ensured so a fresh root
/// has a catalog, but no automatic refresh runs. `sync_installed` follows the
/// caller: `search` does not read installed state, `info`/`depends` do.
fn open_engine_with_catalog(
    root_override: Option<PathBuf>,
    sync_installed: bool,
) -> Result<Engine, ExitCode> {
    let mut engine = open_engine_config(root_override, sync_installed)?;
    if std::env::var_os("GLUE_SKIP_BOOTSTRAP").is_none() {
        ensure_github_proxy(&mut engine);
        ensure_first_run_bucket(&mut engine);
    }
    Ok(engine)
}

/// Open the engine for package operations: full environment check (helper
/// tools, first bucket) plus the automatic bucket refresh when it is due.
fn open_engine_for_packages(root_override: Option<PathBuf>) -> Result<Engine, ExitCode> {
    let mut engine = open_engine_config(root_override, true)?;
    // Environment checks run before every package operation, so a fresh
    // install resolves its dependencies up front instead of failing halfway
    // through. Tests and scripted callers opt out (the check may hit the
    // network on a fresh root).
    if std::env::var_os("GLUE_SKIP_BOOTSTRAP").is_none() {
        ensure_environment(&mut engine);
    }
    Ok(engine)
}

/// Whether a bucket command has to give up for want of git.
///
/// Bucket operations bootstrap git themselves (`Engine::ensure_git`), so this
/// only reports `true` when that bootstrap could not run either — a machine
/// with no git and no way to download MinGit.
fn git_unavailable(engine: &Engine) -> bool {
    !git::is_available(engine.glue_root())
}

/// Package-operation dependency check: helper tools (7-Zip, innounp, WiX, and
/// git when the machine has none), a due bucket refresh, and at least one
/// bucket, all in place before an install/update needs them.
///
/// Only install/update call this. Read-only commands open the engine without
/// it so `list` or `cache list` never pays a network round trip; failures here
/// are warnings, since a missing tool or unreachable bucket must not stop the
/// operation from reporting what it can. Tool downloads show the same live
/// speed line as package downloads.
fn ensure_environment(engine: &mut Engine) {
    ensure_github_proxy(engine);
    // Choose the faster route before helper tools (7-Zip, innounp, WiX, MinGit)
    // and package downloads: direct access is the baseline, and the configured
    // proxy replaces it only when it is clearly faster.
    ensure_proxy_route(engine);
    // Helper tools run before the bucket steps below, not after: `git` is one
    // of them, and the bucket steps cannot run without it. The proxy probe
    // stays ahead of the downloads so MinGit is fetched through the mirror
    // this machine just picked.
    ensure_helper_tools(engine.services());
    ensure_bucket_sync(engine);
    ensure_first_run_bucket(engine);
}

/// Pick direct access or the configured proxy for this run by measuring both.
///
/// The proxy is used when direct access fails or the proxy is at least 25%
/// faster; otherwise direct wins. With `proxy_packages` set, a direct win still
/// keeps the proxy for those packages: their host may be blocked rather than
/// slow (claude-code's `storage.googleapis.com`), and a speed probe cannot see
/// that on a different host.
fn ensure_proxy_route(engine: &mut Engine) {
    let root = engine.glue_root().to_path_buf();
    let packages = config::read_config_proxy_packages(&root).unwrap_or_default();
    let proxy = gluestick_core::proxy::resolve(&root);
    if proxy.is_empty() {
        return;
    }
    let asset = config::PROXY_PROBE_URL;

    println!("  -> Comparing direct and proxy download speed...");
    let downloader = engine.services().downloader();
    let direct_settings = gluestick_core::proxy::ProxySettings::default();
    let (direct, via_proxy) = std::thread::scope(|scope| {
        let direct = scope.spawn(|| downloader.probe_speed_via(asset, &direct_settings));
        let via_proxy = scope.spawn(|| downloader.probe_speed_via(asset, &proxy));
        (
            direct
                .join()
                .ok()
                .and_then(|result| result.ok())
                .unwrap_or(0.0),
            via_proxy
                .join()
                .ok()
                .and_then(|result| result.ok())
                .unwrap_or(0.0),
        )
    });
    println!(
        "    direct {}  |  proxy {}",
        rate_text(direct),
        rate_text(via_proxy)
    );

    if direct <= 0.0 && via_proxy <= 0.0 {
        eprintln!(
            "warning: neither direct access nor the proxy answered; keeping the configured route"
        );
        return;
    }
    if proxy_is_faster(direct, via_proxy) {
        engine.services_mut().set_proxy_route(proxy, Vec::new());
        println!("  {} Using the proxy", mark_success());
    } else if packages.is_empty() {
        engine
            .services_mut()
            .set_proxy_route(gluestick_core::proxy::ProxySettings::default(), Vec::new());
        println!("  {} Using direct access", mark_success());
    } else {
        engine.services_mut().set_proxy_route(proxy, packages);
        println!(
            "  {} Using direct access (proxy kept for listed packages)",
            mark_success()
        );
    }
}

/// A proxy must beat direct access by 25%, or answer when direct cannot.
fn proxy_is_faster(direct: f64, via_proxy: f64) -> bool {
    via_proxy > 0.0 && (direct <= 0.0 || via_proxy > direct * 1.25)
}

/// Refresh bucket checkouts when `bucket_sync_mode=auto` and the configured
/// interval elapsed, so `update`/`install` see current manifests instead of
/// silently comparing against a stale local bucket.
fn ensure_bucket_sync(engine: &mut Engine) {
    match engine.sync_buckets_if_due() {
        Ok(Some(sync)) => {
            if sync.pulled > 0 {
                println!("  -> Refreshed {} bucket checkout(s)", sync.pulled);
            }
            // A bucket that cannot reach its remote must not be silent: the
            // "up to date" answer below is then based on a stale checkout.
            for (bucket, error) in &sync.failed {
                eprintln!("warning: bucket '{bucket}' refresh failed: {error}");
            }
        }
        Ok(None) => {}
        Err(e) => eprintln!(
            "warning: refreshing buckets failed: {}",
            clean_error(&e.to_string())
        ),
    }
}

/// One-time GitHub mirror setup: measure the built-in candidates against
/// direct access, then persist the winner so nobody has to pick a proxy by
/// hand. The stored value (`direct` when direct access wins) means the probe
/// runs exactly once per data root.
///
/// Skipped when `GITHUB_PROXY` or `github_proxy` is already set — an explicit
/// choice always wins over detection.
fn ensure_github_proxy(engine: &mut Engine) {
    if !config::env_github_proxy().is_empty() {
        return;
    }
    let root = engine.glue_root().to_path_buf();
    let Ok(mut basics) = config::read_basics(&root) else {
        return;
    };
    if !basics.github_proxy.trim().is_empty() {
        return;
    }

    println!("  -> Detecting a GitHub mirror (first run)...");
    let downloader = engine.services().downloader();
    let asset = config::PROXY_PROBE_URL;
    let mut measured: Vec<(Option<&'static str>, Option<f64>)> = Vec::new();
    std::thread::scope(|scope| {
        let mut handles = Vec::new();
        handles.push(scope.spawn(|| {
            (
                Option::<&'static str>::None,
                downloader.probe_speed(asset).ok(),
            )
        }));
        for candidate in config::PROXY_PROBE_CANDIDATES {
            handles.push(scope.spawn(move || {
                let url = format!("{candidate}/{asset}");
                (Some(*candidate), downloader.probe_speed(&url).ok())
            }));
        }
        measured = handles
            .into_iter()
            .filter_map(|handle| handle.join().ok())
            .collect();
    });

    let direct = measured
        .iter()
        .find(|(candidate, _)| candidate.is_none())
        .and_then(|(_, speed)| *speed)
        .unwrap_or(0.0);
    let mut proxies: Vec<(&'static str, f64)> = measured
        .iter()
        .filter_map(|(candidate, speed)| (*candidate).zip(*speed))
        .collect();
    proxies.sort_by(|a, b| b.1.total_cmp(&a.1));

    let mut report = vec![format!("direct {}", rate_text(direct))];
    report.extend(
        proxies
            .iter()
            .map(|(proxy, speed)| format!("{} {}", url_host(proxy), rate_text(*speed))),
    );
    println!("    {}", report.join("  |  "));

    // Nothing reachable (offline, or every mirror plus direct blocked): leave
    // the config untouched so the next run probes again instead of locking in
    // a guess.
    if direct <= 0.0 && proxies.is_empty() {
        eprintln!("warning: could not reach GitHub; mirror detection will retry next run");
        return;
    }

    // A mirror has to beat direct access by a margin, otherwise the extra hop
    // only adds a failure mode. The runner-up is kept as a fallback.
    let threshold = direct * 1.25;
    let chosen: Vec<&str> = proxies
        .iter()
        .filter(|(_, speed)| *speed > 0.0 && *speed > threshold)
        .take(2)
        .map(|(proxy, _)| *proxy)
        .collect();
    let value = if chosen.is_empty() {
        config::DIRECT_PROXY_MARKER.to_string()
    } else {
        chosen.join(",")
    };

    basics.github_proxy = value.clone();
    if let Err(e) = config::write_basics(&root, &basics) {
        eprintln!(
            "warning: could not save the detected GitHub mirror: {}",
            clean_error(&e.to_string())
        );
    }
    let _ = engine.services_mut().reload_github_proxies();

    if chosen.is_empty() {
        println!(
            "  {} Using direct GitHub (no mirror needed)",
            mark_success()
        );
    } else {
        println!("  {} Using GitHub mirror {value}", mark_success());
    }
}

/// First-run bucket: with no buckets installed, add the Scoop-compatible
/// `main` bucket.
///
/// The notice is explicit because a silent clone during resolve looks like a
/// hang on a fresh install; the clone itself is immediate and shares the
/// progress output of `glue bucket add`.
fn ensure_first_run_bucket(engine: &mut Engine) {
    if !engine.list_buckets().is_empty() {
        return;
    }
    println!();
    println!("  No buckets installed yet; adding the 'main' bucket (first run)...");
    let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress);
    if let Err(e) = engine.add_bucket("main", None, Some(progress)) {
        eprintln!(
            "warning: could not add the 'main' bucket: {}",
            clean_error(&e.to_string())
        );
    }
    println!("  {} Bucket 'main' added", mark_success());
    println!();
}

/// Download the helper tools that are missing under `<root>/bin`, with a live
/// progress line. Nothing is printed when everything is already installed.
fn ensure_helper_tools(services: &Services) {
    let missing = bootstrap::missing_helpers(services.root_dir());
    if missing.is_empty() {
        return;
    }
    let labels: Vec<&str> = missing.iter().map(|tool| tool.label()).collect();
    println!("  -> Preparing helper tools: {}", labels.join(", "));
    println!("    One-time download; this may take a moment.");

    for tool in missing {
        let label = tool.label().to_string();
        let sink = {
            let label = label.clone();
            move |bytes: u64, total: u64| {
                if std::io::stdout().is_terminal() {
                    draw_download_line(bytes, total);
                } else {
                    print_progress_plain(&label, bytes, total);
                }
            }
        };
        let sink: &(dyn Fn(u64, u64) + Send + Sync) = &sink;
        let result = bootstrap::ensure_helper(tool, services, Some(sink));
        if !std::io::stdout().is_terminal() {
            flush_plain(&label);
        }
        match result {
            Ok(path) => {
                end_progress_line();
                if tool == bootstrap::HelperTool::SevenZip {
                    // Later extractions in this process must use the binary
                    // that was just installed, not the PATH probe.
                    services
                        .extractor()
                        .set_7z_path(path.to_string_lossy().into_owned());
                }
                println!("  {} {} ready", mark_success(), tool.label());
            }
            Err(e) => {
                end_progress_line();
                eprintln!("warning: {} unavailable: {e}", tool.label());
            }
        }
    }
}

/// Resolve the data root for a command.
///
/// Precedence: an explicit `--root`, then `GLUE_ROOT`, then the root saved for
/// this installation (`data-root.txt`, written by `glue root set` and by the
/// installers as `%USERPROFILE%\.glue`), then the installation directory, then
/// the registry, then `%USERPROFILE%\.glue`.
fn resolve_root(root_override: Option<PathBuf>) -> PathBuf {
    paths::resolve_data_root(root_override.as_deref())
}

/// Live state for the single `\r`-based download line: when it opened plus a
/// short window of `(time, bytes)` samples used for the throughput estimate.
/// Download workers can report from several threads, hence the mutex.
static PROGRESS: Mutex<Option<DownloadLine>> = Mutex::new(None);

/// Width of the throughput window. Sampling the last ~4 s instead of the last
/// two events keeps the estimate stable when a parallel chunk lands in a burst
/// a naive last-delta rate is what produces nonsense
/// remaining time on a download that is 75% done.
const RATE_WINDOW: Duration = Duration::from_secs(4);

struct DownloadLine {
    start: Instant,
    samples: VecDeque<(Instant, u64)>,
    /// Highest byte count drawn so far. Parallel workers report from several
    /// threads, so events can arrive out of order; the bar must not jump back.
    last_bytes: u64,
}

/// Close an open progress line (no-op when nothing is displayed).
fn end_progress_line() {
    let mut state = match PROGRESS.lock() {
        Ok(guard) => guard,
        Err(poisoned) => poisoned.into_inner(),
    };
    let download_line = state.take().is_some();
    drop(state);
    if download_line || LINK_LINE_OPEN.swap(false, std::sync::atomic::Ordering::Relaxed) {
        println!();
    }
}

/// Bytes/second over the sampling window, plus time elapsed since the line
/// opened.
fn throughput(line: &mut DownloadLine, bytes: u64) -> (f64, Duration) {
    let now = Instant::now();
    line.samples.push_back((now, bytes));
    while line.samples.len() > 2 && now.duration_since(line.samples[0].0) > RATE_WINDOW {
        line.samples.pop_front();
    }
    let (t0, b0) = line.samples[0];
    let dt = now.duration_since(t0).as_secs_f64();
    let rate = if dt >= 0.25 {
        bytes.saturating_sub(b0) as f64 / dt
    } else {
        0.0
    };
    (rate, now.duration_since(line.start))
}

/// Compact duration: `7s`, `1m04s`, `1h02m`.
fn human_duration(d: Duration) -> String {
    let secs = d.as_secs();
    if secs < 60 {
        format!("{secs}s")
    } else if secs < 3600 {
        format!("{}m{:02}s", secs / 60, secs % 60)
    } else {
        format!("{}h{:02}m", secs / 3600, (secs % 3600) / 60)
    }
}

/// Human-readable byte count: `512 B`, `12.3 MB`, `1.4 GB`.
fn human_bytes(bytes: u64) -> String {
    const KB: f64 = 1024.0;
    let b = bytes as f64;
    if b < KB {
        return format!("{bytes} B");
    }
    if b < KB * KB {
        return format!("{:.1} KB", b / KB);
    }
    if b < KB * KB * KB {
        return format!("{:.1} MB", b / (KB * KB));
    }
    format!("{:.2} GB", b / (KB * KB * KB))
}

/// `12.3 MB/s`, or `--/s` until the first measurement lands.
fn rate_text(bytes_per_sec: f64) -> String {
    if bytes_per_sec <= 0.0 {
        "--/s".to_string()
    } else {
        format!("{}/s", human_bytes(bytes_per_sec as u64))
    }
}

/// 30-cell ASCII bar.
fn progress_bar(pct: u64) -> String {
    const WIDTH: u64 = 30;
    let filled = (pct.min(100) * WIDTH / 100) as usize;
    format!(
        "[{}{}]",
        "=".repeat(filled),
        " ".repeat(WIDTH as usize - filled)
    )
}

/// Bar for a download whose size the server never sent, swept by a moving
/// segment.
///
/// `codeload.github.com` streams a freshly built source archive with
/// `Transfer-Encoding: chunked` and no `Content-Length`, so there is no
/// percentage to show (`86box-roms` is one of these). A moving segment still
/// tells the user the transfer is alive; the byte counter and rate carry the
/// real information.
fn indeterminate_bar(elapsed: Duration) -> String {
    const WIDTH: usize = 30;
    const SEGMENT: usize = 8;
    // The segment enters on the left and exits on the right before repeating.
    let span = WIDTH + SEGMENT;
    let pos = (elapsed.as_millis() as usize / 120) % span;
    let mut cells = [' '; WIDTH];
    for i in 0..SEGMENT {
        let at = pos as isize - (SEGMENT as isize - 1) + i as isize;
        if at >= 0 && (at as usize) < WIDTH {
            cells[at as usize] = '=';
        }
    }
    format!("[{}]", cells.iter().collect::<String>())
}

/// One-line package description for install output: whitespace/newlines are
/// collapsed and the text is capped so a very long manifest description (real
/// bucket max is ~1,300 chars) cannot flood the console.
fn short_description(description: &str) -> Option<String> {
    const MAX: usize = 160;
    let text: String = description.split_whitespace().collect::<Vec<_>>().join(" ");
    if text.is_empty() {
        return None;
    }
    if text.chars().count() <= MAX {
        return Some(text);
    }
    let mut short: String = text.chars().take(MAX - 1).collect();
    short.push('~');
    Some(short)
}

/// Phase state for the -style install output (`  -> phase` + indented details).
struct InstallUi {
    /// Last phase header printed (deduped; changed by each new phase).
    phase: String,
    deploy_started: bool,
    /// `Linking current => <version>` already printed live, so the summary
    /// does not repeat it.
    current_announced: bool,
}

static INSTALL_UI: Mutex<InstallUi> = Mutex::new(InstallUi {
    phase: String::new(),
    deploy_started: false,
    current_announced: false,
});

fn with_install_ui<T>(f: impl FnOnce(&mut InstallUi) -> T) -> T {
    let mut guard = match INSTALL_UI.lock() {
        Ok(guard) => guard,
        Err(poisoned) => poisoned.into_inner(),
    };
    f(&mut guard)
}

fn install_ui_reset() {
    with_install_ui(|ui| {
        ui.phase.clear();
        ui.deploy_started = false;
        ui.current_announced = false;
    });
}

/// Prints `  -> <text>` unless it repeats the current phase.
fn install_phase(text: impl Into<String>) {
    let text = text.into();
    let changed = with_install_ui(|ui| {
        if ui.phase == text {
            false
        } else {
            ui.phase = text.clone();
            true
        }
    });
    if changed {
        println!("  -> {text}");
    }
}

/// Prints one indented detail line under the current phase.
///
/// Four spaces put the detail text directly under the phase message
/// (`  -> Starting install...` -> `    Version: …`)
/// keeping the block visually compact.
fn install_detail(text: &str) {
    println!("    {text}");
}

/// Text for the live download line. Shared by package downloads
/// (`ProgressEvent::Downloading`) and bootstrap tool downloads, so both show
/// the same bar, byte counters, throughput and ETA.
fn download_line_text(bytes: u64, total: u64) -> String {
    let (bytes, speed, elapsed) = {
        let mut state = match PROGRESS.lock() {
            Ok(guard) => guard,
            Err(poisoned) => poisoned.into_inner(),
        };
        let line = state.get_or_insert_with(|| DownloadLine {
            start: Instant::now(),
            samples: VecDeque::new(),
            last_bytes: 0,
        });
        // Parallel workers report out of order; the bar must not move back.
        let bytes = bytes.max(line.last_bytes);
        line.last_bytes = bytes;
        let (speed, elapsed) = throughput(line, bytes);
        (bytes, speed, elapsed)
    };
    if total == 0 {
        // The server sent no Content-Length (chunked body): show a sweeping
        // bar instead of a percentage that would be a lie.
        return format!(
            "    {}  {}  {}",
            indeterminate_bar(elapsed),
            human_bytes(bytes),
            rate_text(speed)
        );
    }
    let total = total.max(bytes);
    let pct = bytes.min(total) * 100 / total;
    let eta = if speed > 1.0 && elapsed >= Duration::from_secs(1) {
        human_duration(Duration::from_secs_f64(
            total.saturating_sub(bytes) as f64 / speed,
        ))
    } else {
        "--".to_string()
    };
    format!(
        "    {} {pct:>3}%  {} / {}  {}  eta {eta}",
        progress_bar(pct),
        human_bytes(bytes),
        human_bytes(total),
        rate_text(speed),
    )
}

/// Host of a URL: `https://gh-proxy.com/https://github.com/…` -> `gh-proxy.com`.
fn url_host(url: &str) -> &str {
    let rest = url.split_once("://").map(|(_, rest)| rest).unwrap_or(url);
    rest.split('/').next().unwrap_or(rest)
}

/// Redraw the live download line in place (callers gate on a terminal).
fn draw_download_line(bytes: u64, total: u64) {
    let line = download_line_text(bytes, total);
    print!("\r{line:<78}\x1b[K");
    let _ = std::io::stdout().flush();
}

/// Text for the live "linking files" line: hardlink progress over the member
/// files of the extracted archive.
fn link_line_text(done: usize, total: usize) -> String {
    if total == 0 {
        return "    linking files...".to_string();
    }
    let pct = (done.min(total) * 100 / total) as u64;
    format!(
        "    {} {pct:>3}%  {done} / {total} files",
        progress_bar(pct)
    )
}

/// Redraw the live link line in place (callers gate on a terminal).
fn draw_link_line(done: usize, total: usize) {
    let line = link_line_text(done, total);
    print!("\r{line:<78}\x1b[K");
    let _ = std::io::stdout().flush();
    LINK_LINE_OPEN.store(true, std::sync::atomic::Ordering::Relaxed);
}

fn print_progress(event: ProgressEvent) {
    // Byte-level download progress redraws a single line — but only on a
    // terminal. When output is piped the phase header is the useful form,
    // so the byte-carrying events are dropped there.
    if let ProgressEvent::Downloading {
        package,
        file,
        bytes,
        total,
    } = &event
    {
        if *total == 0 {
            if *bytes == 0 {
                // Coarse "download starting" marker from the engine; reset the
                // rate window so a retry starts from a clean slate.
                end_progress_line();
                if let Some(name) = package.strip_prefix("bucket/") {
                    install_phase(format!("Adding bucket {name}"));
                } else {
                    install_phase(format!("Preparing to download {file}..."));
                    install_detail(&format!("Downloading {file}"));
                }
                return;
            }
            // Server sent no Content-Length: byte counter + rate, no percentage.
            if !std::io::stdout().is_terminal() {
                print_progress_plain(package, *bytes, 0);
                return;
            }
            draw_download_line(*bytes, 0);
            return;
        }
        if !std::io::stdout().is_terminal() {
            print_progress_plain(package, *bytes, *total);
            return;
        }
        draw_download_line(*bytes, *total);
        return;
    }

    // Link progress redraws the same kind of line, without closing it: the
    // next phase event (or Completed) ends the line.
    if let ProgressEvent::LinkingFiles { done, total, .. } = &event {
        if !std::io::stdout().is_terminal() {
            return;
        }
        draw_link_line(*done, *total);
        return;
    }

    end_progress_line();
    match event {
        ProgressEvent::Started { package } => {
            install_ui_reset();
            match package.strip_prefix("bucket/") {
                Some(name) => install_phase(format!("Adding bucket {name}...")),
                None => install_phase("Starting install..."),
            }
        }
        // Description/version/architecture, indented under "Starting install...".
        ProgressEvent::Resolved {
            version,
            description,
            arch,
            ..
        } => {
            if let Some(text) = short_description(&description) {
                install_detail(&text);
            }
            if !version.is_empty() {
                install_detail(&format!("Version: {version}"));
            }
            if !arch.is_empty() {
                install_detail(&format!("Architecture: {arch}"));
            }
        }
        // Hash verification is part of the download phase; the "Downloaded"
        // detail line below is the user-facing result.
        ProgressEvent::Verifying { .. } => {}
        // The host being contacted, printed before the request goes out so the
        // wait for the first byte is not a silent window.
        ProgressEvent::Connecting { url, .. } => {
            install_detail(&format!("Connecting to {}...", url_host(&url)));
        }
        // Which source the transfer uses: the configured GitHub proxy's host,
        // or the asset host when downloading directly.
        ProgressEvent::DownloadSource { url, .. } => {
            install_detail(&format!("Source: {}", url_host(&url)));
        }
        ProgressEvent::DownloadFinished {
            package,
            file,
            size,
            network_ms,
            store_ms,
        } => {
            if !std::io::stdout().is_terminal() {
                flush_plain(&package);
            }
            let total_ms = network_ms.saturating_add(store_ms);
            let _ = file;
            install_detail(&format!(
                "Downloaded {} in {:.1} s (network {:.1} s, store {store_ms} ms)",
                human_bytes(size),
                total_ms as f64 / 1000.0,
                network_ms as f64 / 1000.0,
            ));
        }
        ProgressEvent::Deploying { .. } => {
            let first = with_install_ui(|ui| {
                if ui.deploy_started {
                    false
                } else {
                    ui.deploy_started = true;
                    true
                }
            });
            if first {
                install_phase("Linking files...");
                install_detail("Linking files from store...");
            }
        }
        ProgressEvent::Linking { version, .. } => {
            with_install_ui(|ui| ui.current_announced = true);
            install_detail(&format!("Linking current => {version}"));
        }
        // Finalize phases: the last stretch after the link bar reaches 100%,
        // where shim creation and the SQLite write used to be invisible.
        ProgressEvent::CreatingShims { .. } => install_phase("Creating shims..."),
        ProgressEvent::Persisting { .. } => install_phase("Updating cache index..."),
        ProgressEvent::Completed { .. } => {}
        ProgressEvent::Failed { package: _, error } => {
            FAILURE_REPORTED.store(true, std::sync::atomic::Ordering::Relaxed);
            eprintln!("  {} {}", mark_fail(), clean_error(&error));
        }
        ProgressEvent::Removing { package, .. } => {
            install_ui_reset();
            install_phase(format!("Removing {package}..."));
        }
        ProgressEvent::Uninstalled { .. } => {}
        ProgressEvent::GcStarted { bucket } => println!("GC started for bucket {bucket}"),
        ProgressEvent::GcProgress { bucket, .. } => println!("GC progress for bucket {bucket}"),
        ProgressEvent::GcCompleted { bucket, removed } => {
            println!("GC completed for bucket {bucket}: removed {removed}")
        }
        // Machine-readable `--timings` summary; printed verbatim so external
        // benchmark scripts can parse it by field position.
        ProgressEvent::Timings { line } => println!("{line}"),
        // Both download shapes returned above; listed so the match stays
        // exhaustive without a catch-all that would hide new event variants.
        ProgressEvent::Downloading { .. } => {}
        // Drawn by the early return above (never reaches this match).
        ProgressEvent::LinkingFiles { .. } => {}
    }
}

// ---------------------------------------------------------------------------
// install / uninstall / update / search / doctor (carried over from stage 2.6)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Concurrent (multi-package) progress: a live block with one redrawn line per
// package. Bytes arrive from several worker threads, so every render/print is
// serialised through one mutex; non-terminal output falls back to the discrete
// phase lines.
// ---------------------------------------------------------------------------

/// One package's byte progress inside the live block.
struct MultiSample {
    bytes: u64,
    total: u64,
    line: DownloadLine,
}

/// Live multi-package progress block. Invariant: after every operation the
/// cursor sits at column 0 of the line immediately below the block, and
/// `drawn` is the number of lines in it.
struct MultiProgress {
    /// Packages in first-announced order.
    order: Vec<String>,
    samples: HashMap<String, MultiSample>,
    drawn: usize,
}

impl MultiProgress {
    fn new() -> MultiProgress {
        MultiProgress {
            order: Vec::new(),
            samples: HashMap::new(),
            drawn: 0,
        }
    }

    fn announce(&mut self, package: &str) {
        if !self.order.iter().any(|p| p == package) {
            self.order.push(package.to_string());
        }
    }

    fn sample(&mut self, package: &str, bytes: u64, total: u64) {
        self.announce(package);
        let entry = self
            .samples
            .entry(package.to_string())
            .or_insert_with(|| MultiSample {
                bytes: 0,
                total: 0,
                line: DownloadLine {
                    start: Instant::now(),
                    samples: VecDeque::new(),
                    last_bytes: 0,
                },
            });
        entry.bytes = entry.bytes.max(bytes);
        entry.total = total;
    }

    fn finish(&mut self, package: &str) {
        self.order.retain(|p| p != package);
        self.samples.remove(package);
    }

    /// Redraw the block in place, erasing lines a shrinking block no longer
    /// needs.
    fn render(&mut self, out: &mut impl Write) -> std::io::Result<()> {
        if self.drawn > 0 {
            write!(out, "\x1b[{}A", self.drawn)?;
        }
        let mut lines = 0;
        for package in &self.order {
            let Some(sample) = self.samples.get_mut(package) else {
                continue;
            };
            let (speed, elapsed) = throughput(&mut sample.line, sample.bytes);
            let text = multi_progress_line(package, sample.bytes, sample.total, speed, elapsed);
            write!(out, "\r\x1b[K{text}\n")?;
            lines += 1;
        }
        if self.drawn > lines {
            for _ in lines..self.drawn {
                write!(out, "\r\x1b[K\n")?;
            }
            write!(out, "\x1b[{}A", self.drawn - lines)?;
        }
        self.drawn = lines;
        Ok(())
    }

    /// Erase the block and leave the cursor on its first (now blank) line so a
    /// normal `println!` can replace it.
    fn clear(&mut self, out: &mut impl Write) -> std::io::Result<()> {
        if self.drawn == 0 {
            return Ok(());
        }
        write!(out, "\x1b[{}A\r", self.drawn)?;
        for i in 0..self.drawn {
            write!(out, "\x1b[K")?;
            if i + 1 < self.drawn {
                writeln!(out)?;
            }
        }
        if self.drawn > 1 {
            write!(out, "\x1b[{}A", self.drawn - 1)?;
        }
        write!(out, "\r")?;
        self.drawn = 0;
        Ok(())
    }
}

/// 12-cell bar for the multi-package block (the single-package bar is 30);
/// each line also carries the package name and byte counters, so the bar has
/// to stay compact to fit an 80-column console.
fn compact_progress_bar(pct: u64) -> String {
    const WIDTH: u64 = 12;
    let filled = (pct.min(100) * WIDTH / 100) as usize;
    format!(
        "[{}{}]",
        "=".repeat(filled),
        " ".repeat(WIDTH as usize - filled)
    )
}

/// Package name capped so the block line stays within ~78 columns.
fn display_package(package: &str) -> String {
    // The phase line already carries the bucket ref; the progress row is
    // easier to scan with the bare name (`magpie-ai`, not `extras/magpie-ai`).
    let package = package.rsplit('/').next().unwrap_or(package);
    const MAX: usize = 16;
    if package.chars().count() <= MAX {
        package.to_string()
    } else {
        let mut short: String = package.chars().take(MAX - 1).collect();
        short.push('~');
        short
    }
}

/// One block line, kept within the same ~78-column budget as the single-bar
/// progress line.
fn multi_progress_line(
    package: &str,
    bytes: u64,
    total: u64,
    speed: f64,
    elapsed: Duration,
) -> String {
    let package = display_package(package);
    if total == 0 {
        return format!(
            "  -> {package}  {}  {}",
            human_bytes(bytes),
            rate_text(speed)
        );
    }
    let pct = bytes.min(total) * 100 / total;
    let remaining = total.saturating_sub(bytes);
    let eta = if speed > 1.0 && elapsed >= Duration::from_secs(1) {
        human_duration(Duration::from_secs_f64(remaining as f64 / speed))
    } else {
        "--".to_string()
    };
    format!(
        "  -> {package} {} {pct:>3}%  {} / {}  {}  eta {eta}",
        compact_progress_bar(pct),
        human_bytes(bytes),
        human_bytes(total),
        rate_text(speed)
    )
}

/// One progress line for a log or installer details pane: no bracket bar (it
/// would be a nearly-empty 12-cell block on every line) and a fixed-width
/// package column so a multi-package template stays readable.
fn plain_progress_line(
    package: &str,
    bytes: u64,
    total: u64,
    speed: f64,
    elapsed: Duration,
) -> String {
    let package = display_package(package);
    let name = format!("{package:<16}");
    if total == 0 {
        return format!("  -> {name} {}  {}", human_bytes(bytes), rate_text(speed));
    }
    let pct = bytes.min(total) * 100 / total;
    let remaining = total.saturating_sub(bytes);
    let eta = if speed > 1.0 && elapsed >= Duration::from_secs(1) {
        human_duration(Duration::from_secs_f64(remaining as f64 / speed))
    } else {
        "--".to_string()
    };
    format!(
        "  -> {name} {pct:>3}%  {} / {}  {}  eta {eta}",
        human_bytes(bytes),
        human_bytes(total),
        rate_text(speed)
    )
}

/// Shared live-block state. Poisons are recovered: a panicked worker must not
/// take the progress display (or the process) down with it.
static MULTI_PROGRESS: Mutex<Option<MultiProgress>> = Mutex::new(None);

fn multi_lock() -> std::sync::MutexGuard<'static, Option<MultiProgress>> {
    MULTI_PROGRESS
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Minimum spacing between discrete progress lines when stdout is not a
/// terminal (log file, CI, or a terminal panel that pipes the child's output).
const PLAIN_PROGRESS_INTERVAL: Duration = Duration::from_secs(2);

struct PlainSample {
    last_print: Instant,
    line: DownloadLine,
    bytes: u64,
    total: u64,
    /// Whether a line has been printed for this package yet.
    printed: bool,
    /// Last printed 5% bucket; a new line waits for the next one.
    last_step: u64,
}

/// Non-terminal progress state: self-contained lines, no cursor control.
static PLAIN_PROGRESS: Mutex<Option<HashMap<String, PlainSample>>> = Mutex::new(None);

fn plain_lock() -> std::sync::MutexGuard<'static, Option<HashMap<String, PlainSample>>> {
    PLAIN_PROGRESS
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// Periodic one-line-per-package progress for non-terminal stdout. The first
/// sample prints immediately; afterwards at most one line per
/// [`PLAIN_PROGRESS_INTERVAL`] per package.
fn print_progress_plain(package: &str, bytes: u64, total: u64) {
    let mut guard = plain_lock();
    let map = guard.get_or_insert_with(HashMap::new);
    let now = Instant::now();
    let entry = map
        .entry(package.to_string())
        .or_insert_with(|| PlainSample {
            // `checked_sub` keeps the very first sample printable even when the
            // process just started.
            last_print: now.checked_sub(PLAIN_PROGRESS_INTERVAL).unwrap_or(now),
            line: DownloadLine {
                start: now,
                samples: VecDeque::new(),
                last_bytes: 0,
            },
            bytes: 0,
            total: 0,
            printed: false,
            last_step: 0,
        });
    entry.bytes = bytes;
    entry.total = total;

    // The phase lines already say "Downloading ..." and "Connecting ...";
    // skip the first 0-byte sample and then one line per 5% bucket.
    let pct = (bytes.min(total) * 100).checked_div(total).unwrap_or(0);
    let step = if total > 0 { pct / 5 } else { u64::MAX };
    if !entry.printed {
        if pct == 0 {
            entry.last_step = step;
            return;
        }
    } else if total > 0 && step == entry.last_step {
        return;
    }
    let min_gap = if total == 0 {
        PLAIN_PROGRESS_INTERVAL
    } else {
        Duration::from_secs(1)
    };
    if entry.printed && now.duration_since(entry.last_print) < min_gap {
        return;
    }

    entry.last_print = now;
    entry.printed = true;
    entry.last_step = step;
    let (speed, elapsed) = throughput(&mut entry.line, bytes);
    println!(
        "{}",
        plain_progress_line(package, bytes, total, speed, elapsed)
    );
}

/// Emit the package's final state (when the last sample fell inside the
/// throttle window) and drop its state.
fn flush_plain(package: &str) {
    let mut guard = plain_lock();
    let Some(map) = guard.as_mut() else {
        return;
    };
    let Some(mut entry) = map.remove(package) else {
        return;
    };
    let (speed, elapsed) = throughput(&mut entry.line, entry.bytes);
    println!(
        "{}",
        plain_progress_line(package, entry.bytes, entry.total, speed, elapsed)
    );
}

/// Clear the live block, print one permanent line (stdout, or stderr for
/// failures), then redraw whatever is still downloading. `ending` names the
/// package whose download is over.
fn multi_permanent(line: &str, ending: Option<&str>, to_stderr: bool, tty: bool) {
    if !tty {
        // Piped/log output: flush the package's last sample so the final
        // percentage is not lost between throttle windows.
        if let Some(package) = ending {
            flush_plain(package);
        }
        if to_stderr {
            eprintln!("{line}");
        } else {
            println!("{line}");
        }
        return;
    }
    let mut guard = multi_lock();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();
    if let Some(mp) = guard.as_mut() {
        if let Some(package) = ending {
            mp.finish(package);
        }
        let _ = mp.clear(&mut out);
    }
    if to_stderr {
        eprintln!("{line}");
    } else {
        let _ = writeln!(out, "{line}");
    }
    if let Some(mp) = guard.as_mut() {
        let _ = mp.render(&mut out);
    }
    let _ = out.flush();
}

/// Progress for concurrent multi-package installs. On a terminal every
/// package gets a line in a live block redrawn in place; when stdout is piped
/// only the coarse phase lines print (no cursor control in logs).
fn print_progress_multi(event: ProgressEvent) {
    end_progress_line(); // close any stray single-install line before phase text
    let tty = std::io::stdout().is_terminal();
    match event {
        ProgressEvent::Started { package } => {
            let label = match package.strip_prefix("bucket/") {
                Some(name) => format!("  -> Adding bucket {name}"),
                None => format!("  -> Starting install of {package}"),
            };
            multi_permanent(&label, None, false, tty)
        }
        ProgressEvent::Resolved { description, .. } => {
            if let Some(text) = short_description(&description) {
                multi_permanent(&format!("  -> {text}"), None, false, tty);
            }
        }
        // `bytes == 0 && total == 0` is the engine's "download starting"
        // marker (emitted by the install pipeline) — it names the package
        // before the first byte sample arrives.
        ProgressEvent::Downloading {
            package,
            file: _,
            bytes: 0,
            total: 0,
        } => {
            if tty {
                if let Some(mp) = multi_lock().as_mut() {
                    mp.announce(&package);
                }
            }
            multi_permanent(&format!("  -> Downloading {package}"), None, false, tty);
        }
        ProgressEvent::Downloading {
            package,
            file: _,
            bytes,
            total,
        } => {
            if !tty {
                print_progress_plain(&package, bytes, total);
                return;
            }
            let mut guard = multi_lock();
            let mp = guard.get_or_insert_with(MultiProgress::new);
            mp.sample(&package, bytes, total);
            let stdout = std::io::stdout();
            let mut out = stdout.lock();
            let _ = mp.render(&mut out);
            let _ = out.flush();
        }
        ProgressEvent::DownloadFinished {
            package,
            file,
            size,
            network_ms,
            store_ms,
        } => {
            let total_ms = network_ms.saturating_add(store_ms);
            multi_permanent(
                &format!(
                    "  -> Downloaded {file} ({}) in {:.1} s",
                    human_bytes(size),
                    total_ms as f64 / 1000.0
                ),
                Some(&package),
                false,
                tty,
            );
        }
        ProgressEvent::Connecting { package, url } => multi_permanent(
            &format!("  -> Connecting to {} for {package}", url_host(&url)),
            Some(&package),
            false,
            tty,
        ),
        ProgressEvent::Verifying { package } => multi_permanent(
            &format!("  -> Verifying {package}"),
            Some(&package),
            false,
            tty,
        ),
        ProgressEvent::Deploying { package, .. } => multi_permanent(
            &format!("  -> Deploying {package}"),
            Some(&package),
            false,
            tty,
        ),
        ProgressEvent::Linking { package, .. } => multi_permanent(
            &format!("  -> Linking shims for {package}"),
            Some(&package),
            false,
            tty,
        ),
        ProgressEvent::CreatingShims { package } => multi_permanent(
            &format!("  -> Creating shims for {package}"),
            Some(&package),
            false,
            tty,
        ),
        ProgressEvent::Persisting { package } => multi_permanent(
            &format!("  -> Persisting {package}"),
            Some(&package),
            false,
            tty,
        ),
        ProgressEvent::Failed { package, error } => {
            FAILURE_REPORTED.store(true, std::sync::atomic::Ordering::Relaxed);
            multi_permanent(
                &format!("  {} {package}: {}", mark_fail(), clean_error(&error)),
                Some(&package),
                true,
                tty,
            )
        }
        ProgressEvent::Timings { line } => multi_permanent(&line, None, false, tty),
        // Completed is summarised by the command itself after `install_many`
        // returns; GC/removal events never occur during install. Link progress
        // is left out of the live block: a `\r` line would fight the per-
        // package rows, and the per-package summary already reports the count.
        ProgressEvent::Completed { .. }
        | ProgressEvent::Removing { .. }
        | ProgressEvent::Uninstalled { .. }
        | ProgressEvent::GcStarted { .. }
        | ProgressEvent::GcProgress { .. }
        | ProgressEvent::GcCompleted { .. }
        | ProgressEvent::LinkingFiles { .. }
        // Per-transfer source lines would interleave with the live block; the
        // single-package output is where they are shown.
        | ProgressEvent::DownloadSource { .. } => {}
    }
}

/// Per-package install summary in the same phase/indent style as the live
/// progress output (shims then cache index, final "installed to" line).
fn print_install_summary(outcome: &InstallOutcome) {
    if outcome.already_installed {
        println!();
        println!(
            "{} {} {} is already installed",
            mark_success(),
            outcome.package,
            outcome.version
        );
        install_ui_reset();
        return;
    }
    // Deploy is synchronous and finishes before the summary; make sure the
    // link phase is on screen even for paths that emit no Deploying event.
    let deploy_started = with_install_ui(|ui| ui.deploy_started);
    if !deploy_started {
        install_phase("Linking files...");
        install_detail("Linking files from store...");
    }
    if outcome.linked_files > 0 {
        install_detail(&format!("Linked {} file(s)", outcome.linked_files));
    }
    // Printed live by `ProgressEvent::Linking` unless that event never fired.
    let current_announced = with_install_ui(|ui| ui.current_announced);
    if !current_announced {
        install_detail(&format!("Linking current => {}", outcome.version));
    }
    if !outcome.shims.is_empty() {
        // The phase header was printed live by `ProgressEvent::CreatingShims`.
        for name in &outcome.shims {
            install_detail(&format!("{} {name}", mark_success()));
        }
    }
    // Header printed live by `ProgressEvent::Persisting`.
    install_detail("Index updated");
    if outcome.from_cache {
        install_detail("(artifact reused from the content cache)");
    }
    // Final line: unindented and separated from the phase block above, so the
    // outcome stands out from the step-by-step detail.
    println!();
    println!(
        "{} {}@{} installed to {}",
        mark_success(),
        outcome.package,
        outcome.version,
        outcome.install_dir.display()
    );
    // Optional companions are hinted, never installed: `suggest` is Scoop's
    // "you may also want" list, unlike `depends` which install does expand.
    if !outcome.suggestions.is_empty() {
        println!();
        println!("Suggested (optional, not installed):");
        for s in &outcome.suggestions {
            let label = if s.label.is_empty() {
                s.ref_.clone()
            } else {
                format!("{} -> {}", s.label, s.ref_)
            };
            println!("  * {label}  (glue install {})", s.ref_);
        }
    }
    install_ui_reset();
}

/// Remind the user when this terminal predates the PATH entry.
///
/// Windows never updates already-running consoles (the registry and the
/// WM_SETTINGCHANGE broadcast only affect new processes), so a freshly created
/// shim is not resolvable until the terminal is reopened.
fn print_path_hint(data_root: &Path) {
    // The hint is for the shell the user is looking at. In the installer's
    // piped/logged run there is no terminal to restart, so it is just noise.
    if !std::io::stdout().is_terminal() {
        return;
    }
    let shims = data_root.join("shims");
    let process_path = std::env::var("PATH").unwrap_or_default();
    if pathenv::path_contains(&process_path, &shims.to_string_lossy()) {
        return;
    }
    eprintln!(
        "note: {} is not on this terminal's PATH yet; open a new terminal (restart Windows Terminal) to run newly installed commands",
        shims.display()
    );
}

/// The token the Ctrl+C handler flips, so a running install can stop at a
/// phase boundary and report what it kept on disk.
static CANCEL_TOKEN: Mutex<Option<gluestick_core::cancel::CancelToken>> = Mutex::new(None);

/// Arms the cooperative cancel handler and returns this run's token.
///
/// Without it Windows kills the process on Ctrl+C: no "cancelled" line, no
/// flush, and the resumable `.part` is only found by the next attempt.
fn arm_cancel_handler() -> gluestick_core::cancel::CancelToken {
    let token = gluestick_core::cancel::CancelToken::new();
    match CANCEL_TOKEN.lock() {
        Ok(mut slot) => *slot = Some(token.clone()),
        Err(poisoned) => *poisoned.into_inner() = Some(token.clone()),
    }
    platform_arm_cancel_handler();
    token
}

#[cfg(windows)]
fn platform_arm_cancel_handler() {
    unsafe extern "system" fn handler(ctrl_type: u32) -> i32 {
        use windows_sys::Win32::System::Console::{CTRL_BREAK_EVENT, CTRL_C_EVENT};
        if ctrl_type != CTRL_C_EVENT && ctrl_type != CTRL_BREAK_EVENT {
            return 0;
        }
        let token = match CANCEL_TOKEN.lock() {
            Ok(slot) => slot.clone(),
            Err(poisoned) => poisoned.into_inner().clone(),
        };
        match token {
            Some(token) => {
                token.cancel();
                1 // handled: the install loop reports the cancellation
            }
            None => 0,
        }
    }
    // SAFETY: the handler only flips an atomic flag through a mutex; it never
    // allocates or unwinds, which is what the console control dispatcher
    // requires of a handler.
    unsafe {
        windows_sys::Win32::System::Console::SetConsoleCtrlHandler(Some(handler), 1);
    }
}

#[cfg(not(windows))]
fn platform_arm_cancel_handler() {}

/// Whether an install error is the cooperative cancellation requested above.
fn is_cancelled(text: &str) -> bool {
    text.to_ascii_lowercase().contains("cancel")
}

/// Install switches shared by the CLI entry points, so `install_cmd` keeps a
/// readable arity as flags are added.
#[derive(Clone, Copy, Default)]
struct InstallFlags {
    force: bool,
    timings: bool,
    interactive: bool,
    independent: bool,
}

fn install_cmd(
    packages: Vec<String>,
    arch: String,
    version: String,
    flags: InstallFlags,
    root_override: Option<PathBuf>,
) -> ExitCode {
    let InstallFlags {
        force,
        timings,
        interactive,
        independent,
    } = flags;
    if packages.is_empty() {
        eprintln!("error: install requires at least one package");
        return ExitCode::FAILURE;
    }
    let engine = match open_engine_for_packages(root_override) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let cancel = arm_cancel_handler();
    let build = |package: String| InstallOptions {
        package_ref: package,
        version: version.clone(),
        arch: arch.clone(),
        force,
        reuse_cache: false,
        timings,
        // Only an explicit `glue install --interactive` turns this on; every
        // background path (update, template, scheduled refresh) leaves it off.
        interactive,
        // Ctrl+C flips the shared token; the engine stops at the next phase
        // boundary or read and keeps the resumable `.part` for a rerun.
        cancel: Some(cancel.clone()),
        progress: None,
        independent,
    };

    // A single package keeps the live `\r` download bar; two or more install
    // concurrently with discrete per-package lines. installs them one by
    // one — overlapping them here is an intentional divergence.
    FAILURE_REPORTED.store(false, std::sync::atomic::Ordering::Relaxed);
    if packages.len() == 1 {
        let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress);
        match engine.install(build(packages.into_iter().next().unwrap()), Some(progress)) {
            Ok(outcome) => {
                print_install_summary(&outcome);
                print_path_hint(engine.services().root_dir());
                ExitCode::SUCCESS
            }
            Err(e) => {
                let text = e.to_string();
                if is_cancelled(&text) {
                    eprintln!("cancelled: rerun the same command to continue");
                    ExitCode::from(130)
                } else {
                    // The progress callback already reported the failure it was
                    // handed; printing the returned error too duplicated the
                    // line.
                    if !FAILURE_REPORTED.load(std::sync::atomic::Ordering::Relaxed) {
                        eprintln!("error: {}", clean_error(&text));
                    }
                    ExitCode::FAILURE
                }
            }
        }
    } else {
        let options_list: Vec<InstallOptions> = packages.into_iter().map(build).collect();
        let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress_multi);
        let results = engine.install_many(options_list, Some(progress));
        let mut failed = false;
        for result in results {
            match result {
                Ok(outcome) => print_install_summary(&outcome),
                Err(e) => {
                    let text = e.to_string();
                    if is_cancelled(&text) {
                        eprintln!("cancelled: rerun the same command to continue");
                    } else if !FAILURE_REPORTED.load(std::sync::atomic::Ordering::Relaxed) {
                        eprintln!("error: {}", clean_error(&text));
                    }
                    failed = true;
                }
            }
        }
        if failed {
            ExitCode::FAILURE
        } else {
            print_path_hint(engine.services().root_dir());
            ExitCode::SUCCESS
        }
    }
}

fn uninstall_cmd(package: String, root_override: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine_healing(root_override) {
        Ok(e) => e,
        Err(code) => return code,
    };
    // `pkg@version` removes one installed version directory instead of
    // uninstalling the package: switch-version's counterpart. A bare `pkg`
    // keeps the full-uninstall behaviour below.
    if let Some((name, version)) = package.rsplit_once('@') {
        if !name.trim().is_empty() && !version.trim().is_empty() {
            return uninstall_version_cmd(&engine, name, version);
        }
    }
    let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress);
    match engine.uninstall(&package, Some(progress)) {
        Ok(outcome) => {
            println!();
            println!("Uninstalling {}@{}...", outcome.package, outcome.version);
            println!(
                "  Removing {} ({} file(s), {})",
                outcome.install_dir.display(),
                outcome.file_count,
                format_bytes(outcome.size)
            );
            if !outcome.removed_shims.is_empty() {
                println!("  Removing shims...");
                for shim in &outcome.removed_shims {
                    println!("    {} {shim}", mark_success());
                }
            }
            println!("  Install removed");
            println!(
                "  {} {}@{} uninstalled",
                mark_success(),
                outcome.package,
                outcome.version
            );
            ExitCode::SUCCESS
        }
        Err(EngineError::NotInstalled(name)) => {
            eprintln!("error: package {name} is not installed");
            ExitCode::FAILURE
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

/// `glue uninstall <pkg>@<version>`: delete one installed version directory.
///
/// The package stays installed on its active version — no shims, env or
/// `current` link are touched — so the named version must not be the active
/// one; the user switches away first (`glue reset <pkg>@<other>`).
fn uninstall_version_cmd(engine: &Engine, package: &str, version: &str) -> ExitCode {
    match engine.remove_installed_version(package, version) {
        Ok(()) => {
            let name = package_base_name(package);
            println!(
                "  {} {}@{} removed (`current` and the content cache are untouched)",
                mark_success(),
                name,
                version
            );
            ExitCode::SUCCESS
        }
        Err(EngineError::VersionInUse(name, version)) => {
            eprintln!(
                "error: {name}@{version} is the active version; switch away first (glue reset {name}@<version>)"
            );
            ExitCode::FAILURE
        }
        Err(EngineError::VersionNotFound(name, version)) => {
            eprintln!("error: {name} has no installed version {version}");
            ExitCode::FAILURE
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

fn update_cmd(packages: Vec<String>, all: bool, root_override: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine_for_packages(root_override) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let updates = match engine.check_package_updates() {
        Ok(u) => u,
        Err(e) => {
            eprintln!("error: check updates: {e}");
            return ExitCode::FAILURE;
        }
    };
    if updates.is_empty() {
        // Manual sync mode compares against whatever the local buckets hold;
        // say so instead of a bare "up to date" that hides a stale checkout.
        let auto = config::read_config_bucket_sync_mode(engine.glue_root())
            .ok()
            .flatten()
            == Some(config::BUCKET_SYNC_MODE_AUTO);

        println!("All packages are up to date.");
        println!();
        if !auto {
            println!(
                "note: compared against local bucket manifests; run `glue bucket update` to refresh them, or enable `bucket_sync_mode=auto`"
            );
        }
        return ExitCode::SUCCESS;
    }

    // without packages and without --all the
    // command only lists what is outdated.
    let mut targets: Vec<&PackageUpdate> = Vec::new();
    if all || packages.iter().any(|p| p == "*") {
        targets.extend(updates.iter());
    } else if !packages.is_empty() {
        for arg in &packages {
            let name = package_base_name(arg);
            match updates.iter().find(|u| u.name == name) {
                Some(u) => targets.push(u),
                None => println!("  {arg}: no update available"),
            }
        }
    }

    if targets.is_empty() {
        if packages.is_empty() && !all {
            println!("{} package(s) have updates:", updates.len());
            println!();
            for u in &updates {
                println!(
                    "  {}: {} -> {}",
                    u.install_ref(),
                    u.installed_version,
                    u.latest_version
                );
            }
            println!();
            println!("Run 'glue update --all' or 'glue update <package>' to upgrade.");
            return ExitCode::SUCCESS;
        }
        println!("No matching packages to update.");
        return ExitCode::SUCCESS;
    }

    if targets.len() == 1 {
        let u = targets[0];
        let ref_ = u.install_ref();
        println!(
            "Updating {} ({} -> {})...",
            ref_, u.installed_version, u.latest_version
        );
        let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress);
        FAILURE_REPORTED.store(false, std::sync::atomic::Ordering::Relaxed);
        match engine.update(&ref_, Some(progress)) {
            Ok(outcome) => {
                // The full summary (link counts, shims, "installed to") is the
                // same shape as `install`; the "Updating …" line above already
                // says this is an upgrade.
                print_install_summary(&outcome);
            }
            Err(EngineError::UpToDate(name, version)) => {
                println!("  {name} is already up to date at version {version}");
            }
            Err(EngineError::NotInstalled(name)) => {
                eprintln!("  {name}: not installed");
                return ExitCode::FAILURE;
            }
            Err(e) => {
                // `update` funnels through `install`, whose progress callback
                // already printed the failure it was handed.
                if !FAILURE_REPORTED.load(std::sync::atomic::Ordering::Relaxed) {
                    eprintln!("  {} {ref_}: {}", mark_fail(), clean_error(&e.to_string()));
                }
                return ExitCode::FAILURE;
            }
        }
        return ExitCode::SUCCESS;
    }

    // Several packages: metadata passes run in input order, then every
    // download and deploy overlaps under one scope (same shape as
    // `install_many`). Results come back one per input, so a package that is
    // current or failed does not stop the rest.
    let refs: Vec<String> = targets.iter().map(|u| u.install_ref()).collect();
    for (u, ref_) in targets.iter().zip(&refs) {
        println!(
            "Updating {} ({} -> {})...",
            ref_, u.installed_version, u.latest_version
        );
    }
    FAILURE_REPORTED.store(false, std::sync::atomic::Ordering::Relaxed);
    let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress_multi);
    let results = engine.update_many(&refs, Some(progress));
    let mut failed = false;
    for (u, result) in targets.iter().zip(results) {
        match result {
            Ok(outcome) => print_install_summary(&outcome),
            Err(EngineError::UpToDate(name, version)) => {
                println!("  {name} is already up to date at version {version}");
            }
            Err(EngineError::NotInstalled(name)) => {
                eprintln!("  {name}: not installed");
                failed = true;
            }
            Err(e) => {
                if !FAILURE_REPORTED.load(std::sync::atomic::Ordering::Relaxed) {
                    eprintln!(
                        "  {} {}: {}",
                        mark_fail(),
                        u.install_ref(),
                        clean_error(&e.to_string())
                    );
                }
                failed = true;
            }
        }
    }
    if failed {
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

fn list_cmd(
    detailed: bool,
    all: bool,
    packages: Vec<String>,
    root_override: Option<PathBuf>,
) -> ExitCode {
    let engine = match open_engine_healing(root_override) {
        Ok(e) => e,
        Err(code) => return code,
    };

    if all {
        let filter: Vec<String> = packages.iter().map(|p| package_base_name(p)).collect();
        let all_versions = engine.list_installed_all_versions(&filter);
        if all_versions.is_empty() {
            println!("No packages installed");
            println!();
            println!("Install a package with:");
            println!("  glue install <name>");
            return ExitCode::SUCCESS;
        }
        println!("{} packages installed:", all_versions.len());
        println!();
        for pkg in &all_versions {
            println!("{}", pkg.name);
            for v in pkg.versions.iter().rev() {
                let marker = if v == &pkg.current { "*" } else { " " };
                println!("  {marker} {v}");
            }
        }
        return ExitCode::SUCCESS;
    }

    match engine.list_installed() {
        Ok(pkgs) => {
            let filtered: Vec<_> = if packages.is_empty() {
                pkgs
            } else {
                let set: std::collections::HashSet<String> =
                    packages.iter().map(|p| package_base_name(p)).collect();
                pkgs.into_iter().filter(|p| set.contains(&p.name)).collect()
            };
            if filtered.is_empty() {
                println!("No packages installed");
                println!();
                println!("Install a package with:");
                println!("  glue install <name>");
                return ExitCode::SUCCESS;
            }
            println!("{} packages installed:", filtered.len());
            println!();
            for p in &filtered {
                if detailed {
                    println!("{}@{}", p.name, p.version);
                    if !p.install_dir.as_os_str().is_empty() {
                        println!("  InstallDir: {}", p.install_dir.display());
                    }
                    println!("  Size: {}", format_bytes(p.size));
                } else {
                    println!("{}@{}", p.name, p.version);
                }
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

fn search_cmd(
    query: String,
    buckets: Vec<String>,
    limit: usize,
    root_override: Option<PathBuf>,
) -> ExitCode {
    let engine = match open_engine_with_catalog(root_override, false) {
        Ok(e) => e,
        Err(code) => return code,
    };
    match engine.search(&query, &buckets, limit) {
        Ok(results) if results.is_empty() => {
            println!("no results for '{query}'");
            ExitCode::SUCCESS
        }
        Ok(results) => {
            println!("{} package(s) found for '{}':", results.len(), query);
            println!();
            // Align the ref and version columns so the descriptions line up.
            let ref_width = results
                .iter()
                .map(|r| r.bucket.chars().count() + 1 + r.name.chars().count())
                .max()
                .unwrap_or(0);
            let version_width = results
                .iter()
                .map(|r| r.version.chars().count())
                .max()
                .unwrap_or(0);
            for r in &results {
                let desc: String = if r.description.chars().count() > 60 {
                    let mut t: String = r.description.chars().take(57).collect();
                    t.push_str("...");
                    t
                } else {
                    r.description.clone()
                };
                let package_ref = format!("{}/{}", r.bucket, r.name);
                println!(
                    "  {package_ref:<ref_width$}  {:<version_width$}  {desc}",
                    r.version
                );
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

fn template_cmd(cmd: TemplateCommands, root_override: Option<PathBuf>) -> ExitCode {
    let root = resolve_root(root_override.clone());
    match cmd {
        TemplateCommands::List => template_list_cmd(&root),
        TemplateCommands::Show { name } => template_show_cmd(&root, &name, root_override),
        TemplateCommands::Install { name, force } => {
            template_install_cmd(&root, &name, force, root_override)
        }
        TemplateCommands::Update { name } => template_update_cmd(&root, &name, root_override),
        TemplateCommands::Uninstall { name } => template_uninstall_cmd(&root, &name, root_override),
    }
}

/// Language template text is printed in. `GLUE_LANG` wins, then the POSIX
/// locale variables; only the language prefix matters (`zh-Hans.UTF-8` → `zh`).
/// Template files carry the text, so this only chooses which copy to read.
fn template_lang() -> &'static str {
    ["GLUE_LANG", "LC_ALL", "LC_MESSAGES", "LANG"]
        .iter()
        .find_map(|key| std::env::var(key).ok())
        .map(|value| value.to_ascii_lowercase())
        .map(|value| if value.starts_with("zh") { "zh" } else { "en" })
        .unwrap_or("en")
}

fn template_list_cmd(root: &Path) -> ExitCode {
    let templates = templates::load_all(root);
    let lang = template_lang();
    if templates.is_empty() {
        println!("No templates.");
        println!(
            "  add one as {}",
            templates::templates_dir(root).join("<name>.json").display()
        );
        return ExitCode::SUCCESS;
    }
    let name_width = templates
        .iter()
        .map(|script| script.name.chars().count())
        .max()
        .unwrap_or(0)
        .max("NAME".len());
    let origin_width = "official".len();
    let title_width = templates
        .iter()
        .map(|script| script.title.get(lang).chars().count())
        .max()
        .unwrap_or(0)
        .max("TITLE".len());
    println!(
        "{:<name_width$}  {:<origin_width$}  {:>8}  {:<title_width$}  SUMMARY",
        "NAME", "ORIGIN", "PACKAGES", "TITLE"
    );
    for script in &templates {
        println!(
            "{:<name_width$}  {:<origin_width$}  {:>8}  {:<title_width$}  {}",
            script.name,
            script.origin.as_str(),
            script.packages.len(),
            script.title.get(lang),
            script.summary.get(lang)
        );
    }
    println!();
    println!(
        "{} template(s); user templates live in {}",
        templates.len(),
        templates::templates_dir(root).display()
    );
    ExitCode::SUCCESS
}

fn template_show_cmd(root: &Path, name: &str, root_override: Option<PathBuf>) -> ExitCode {
    let found = match templates::find(root, name) {
        Ok(found) => found,
        Err(message) => {
            eprintln!("error: {message}");
            return ExitCode::FAILURE;
        }
    };
    let engine = match open_engine_healing(root_override) {
        Ok(engine) => engine,
        Err(code) => return code,
    };
    let installed = engine.list_installed().unwrap_or_default();
    let updates = engine.check_package_updates().unwrap_or_default();
    let lang = template_lang();

    println!("Template {} ({})", found.name, found.origin.as_str());
    if let Some(path) = &found.path {
        println!("  file: {}", path.display());
    }
    let title = found.title.get(lang);
    if !title.is_empty() && title != found.name {
        println!("  title: {title}");
    }
    if !found.summary.get(lang).is_empty() {
        println!("  summary: {}", found.summary.get(lang));
    }
    if !found.description.get(lang).is_empty() {
        println!("  description: {}", found.description.get(lang));
    }
    if found.packages.is_empty() {
        println!("  packages: none");
        return ExitCode::SUCCESS;
    }
    println!("  packages ({}):", found.packages.len());
    let ref_width = found
        .packages
        .iter()
        .map(|pkg| pkg.install_ref().chars().count())
        .max()
        .unwrap_or(0)
        .max("PACKAGE".len());
    let label_width = found
        .packages
        .iter()
        .map(|pkg| pkg.display(lang).chars().count())
        .max()
        .unwrap_or(0)
        .max("LABEL".len());
    println!(
        "    {:<ref_width$}  {:<label_width$}  {:<8}  STATUS",
        "PACKAGE", "LABEL", "BUCKET"
    );
    for pkg in &found.packages {
        let status = match installed.iter().find(|entry| entry.name == pkg.name) {
            Some(entry) => {
                // An outdated template package is the one thing `show` can point
                // at that `glue update` would fix.
                if updates.iter().any(|update| update.name == pkg.name) {
                    format!("installed {} (update available)", entry.version)
                } else {
                    format!("installed {}", entry.version)
                }
            }
            None => "not installed".to_owned(),
        };
        println!(
            "    {:<ref_width$}  {:<label_width$}  {:<8}  {}",
            pkg.install_ref(),
            pkg.display(lang),
            pkg.bucket_label(),
            status
        );
    }
    ExitCode::SUCCESS
}

fn template_install_cmd(
    root: &Path,
    name: &str,
    force: bool,
    root_override: Option<PathBuf>,
) -> ExitCode {
    let found = match templates::find(root, name) {
        Ok(found) => found,
        Err(message) => {
            eprintln!("error: {message}");
            return ExitCode::FAILURE;
        }
    };
    if found.packages.is_empty() {
        eprintln!("error: template '{}' has no packages", found.name);
        return ExitCode::FAILURE;
    }
    // Installed state is metadata only; the environment bootstrap runs once,
    // inside `install_cmd`.
    let installed = match open_engine_healing(root_override.clone()) {
        Ok(engine) => engine.list_installed().unwrap_or_default(),
        Err(code) => return code,
    };
    let mut refs = Vec::new();
    for pkg in &found.packages {
        if !force && installed.iter().any(|entry| entry.name == pkg.name) {
            println!("  {}: already installed, skipped", pkg.name);
            continue;
        }
        refs.push(pkg.install_ref());
    }
    if refs.is_empty() {
        println!(
            "All {} package(s) of template '{}' are already installed.",
            found.packages.len(),
            found.name
        );
        return ExitCode::SUCCESS;
    }
    println!(
        "Installing template {} ({} of {} package(s))...",
        found.name,
        refs.len(),
        found.packages.len()
    );
    install_cmd(
        refs,
        String::new(),
        String::new(),
        // Template installs are user-requested installs, so their dependencies
        // are pulled in like a plain `glue install`. Updates and scheduled
        // refreshes stay independent.
        InstallFlags {
            force,
            independent: false,
            ..InstallFlags::default()
        },
        root_override,
    )
}

fn template_update_cmd(root: &Path, name: &str, root_override: Option<PathBuf>) -> ExitCode {
    let found = match templates::find(root, name) {
        Ok(found) => found,
        Err(message) => {
            eprintln!("error: {message}");
            return ExitCode::FAILURE;
        }
    };
    let installed = match open_engine_healing(root_override.clone()) {
        Ok(engine) => engine.list_installed().unwrap_or_default(),
        Err(code) => return code,
    };
    // Only the template packages that are actually installed: `update` reports
    // "not installed" as a failure, which a partial template would otherwise
    // trigger for every package the user never installed.
    let refs: Vec<String> = found
        .packages
        .iter()
        .filter(|pkg| installed.iter().any(|entry| entry.name == pkg.name))
        .map(|pkg| pkg.install_ref())
        .collect();
    if refs.is_empty() {
        println!(
            "None of template '{}' packages are installed; run `glue template install {}` first.",
            found.name, found.name
        );
        return ExitCode::SUCCESS;
    }
    println!(
        "Updating template {} ({} installed package(s))...",
        found.name,
        refs.len()
    );
    update_cmd(refs, false, root_override)
}

fn template_uninstall_cmd(root: &Path, name: &str, root_override: Option<PathBuf>) -> ExitCode {
    let found = match templates::find(root, name) {
        Ok(found) => found,
        Err(message) => {
            eprintln!("error: {message}");
            return ExitCode::FAILURE;
        }
    };
    let engine = match open_engine_healing(root_override) {
        Ok(engine) => engine,
        Err(code) => return code,
    };
    let installed = engine.list_installed().unwrap_or_default();
    let targets: Vec<String> = found
        .packages
        .iter()
        .filter(|pkg| installed.iter().any(|entry| entry.name == pkg.name))
        .map(|pkg| pkg.name.clone())
        .collect();
    if targets.is_empty() {
        println!("None of template '{}' packages are installed.", found.name);
        return ExitCode::SUCCESS;
    }
    println!(
        "Uninstalling template {} ({} package(s))...",
        found.name,
        targets.len()
    );
    let mut failed = false;
    for package in targets {
        match engine.uninstall(&package, None) {
            Ok(outcome) => println!(
                "  {} {}@{} uninstalled",
                mark_success(),
                outcome.package,
                outcome.version
            ),
            Err(EngineError::NotInstalled(name)) => println!("  {name}: not installed"),
            Err(e) => {
                eprintln!(
                    "  {} {package}: {}",
                    mark_fail(),
                    clean_error(&e.to_string())
                );
                failed = true;
            }
        }
    }
    if failed {
        ExitCode::FAILURE
    } else {
        println!("  {} template {} uninstalled", mark_success(), found.name);
        ExitCode::SUCCESS
    }
}

fn doctor_cmd(root_override: Option<PathBuf>) -> ExitCode {
    let override_for_report = root_override.clone();
    let engine = match open_engine(root_override) {
        Ok(e) => e,
        Err(code) => return code,
    };
    match engine.doctor() {
        Ok(report) => {
            println!(
                "  workspace: {} ({})",
                report.data_root.display(),
                if report.data_root_exists {
                    "exists"
                } else {
                    "missing"
                }
            );
            println!(
                "  source:    {}",
                describe_root_source(&report.data_root, override_for_report.as_deref())
            );
            if report.buckets.is_empty() {
                println!("  buckets: (none)");
            } else {
                println!(
                    "  buckets: {} ({})",
                    report.buckets.join(", "),
                    report.buckets.len()
                );
            }
            println!("  host arch: {}", report.host_arch);
            println!("  windows build: {}", report.win_build);
            println!(
                "  shim: ({})",
                if report.shim_present {
                    "present"
                } else {
                    "missing"
                }
            );
            println!(
                "  config: ({})",
                if report.config_present {
                    "present"
                } else {
                    "missing"
                }
            );
            let missing: Vec<&str> = report
                .helpers
                .iter()
                .filter(|status| !status.ready)
                .map(|status| status.tool.label())
                .collect();
            if missing.is_empty() {
                println!("  helper tools: all {} ready", report.helpers.len());
            } else {
                println!("  helper tools: missing {}", missing.join(", "));
            }
            println!("  installed packages: {}", report.installed_count);
            // Silent when empty, like the stale-override block below: a clean
            // run should not grow a line for every check it does not trigger.
            if !report.missing_dependencies.is_empty() {
                println!(
                    "  missing dependencies: {}",
                    report.missing_dependencies.len()
                );
                for entry in &report.missing_dependencies {
                    println!(
                        "    {} needs {} (run `glue install {}`)",
                        entry.package, entry.dependency, entry.dependency
                    );
                }
            }
            // Silent when no cached check has found something newer: the
            // default install never runs a check, so most reports omit this.
            if let Some(self_update) = &report.self_update {
                println!(
                    "  self update: {} available (run a newer installer to upgrade)",
                    self_update.latest
                );
            }
            match report.long_paths_enabled {
                Some(true) => println!("  long paths: enabled"),
                Some(false) => {
                    println!("  long paths: disabled (external tools cannot reach deep paths)");
                    println!(
                        "    enable once as admin: reg add \"HKLM\\SYSTEM\\CurrentControlSet\\Control\\FileSystem\" /v LongPathsEnabled /t REG_DWORD /d 1 /f"
                    );
                }
                None => println!("  long paths: unknown"),
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

/// Describes where the running command's data root came from. `--root` wins
/// over every stored rule, so it has to be part of the answer.
fn describe_root_source(data_root: &Path, root_override: Option<&Path>) -> String {
    let (resolved, source) = paths::resolve_data_root_with_source(root_override);
    if resolved == data_root {
        source.label().to_string()
    } else {
        format!("{} (workspace {})", source.label(), resolved.display())
    }
}

// ---------------------------------------------------------------------------
// bucket
// ---------------------------------------------------------------------------

fn bucket_cmd(cmd: BucketCommands, root: Option<PathBuf>) -> ExitCode {
    match cmd {
        BucketCommands::Add { name, repository } => bucket_add_cmd(name, repository, root),
        BucketCommands::Remove { names } => bucket_remove_cmd(names, root),
        BucketCommands::List => bucket_list_cmd(root),
        BucketCommands::Update { names } => bucket_update_cmd(names, root),
        BucketCommands::Check { names } => bucket_check_cmd(names, root),
        BucketCommands::Deepen { names } => bucket_deepen_cmd(names, root),
        BucketCommands::Known => bucket_known_cmd(),
    }
}

fn bucket_add_cmd(name: String, repository: Option<String>, root: Option<PathBuf>) -> ExitCode {
    let mut engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    // Bootstrap first: on a fresh machine this is what installs MinGit, and
    // the check below can only answer after that attempt.
    let _ = engine.ensure_git();
    if git_unavailable(&engine) {
        eprintln!("error: git unavailable and MinGit could not be bootstrapped");
        return ExitCode::FAILURE;
    }
    println!("Adding bucket '{name}'...");
    let url = match repository.filter(|s| !s.is_empty()) {
        Some(u) => u,
        None => match bucket::known_bucket_url(&name) {
            Some(u) => u.to_string(),
            None => {
                let known: Vec<String> = bucket::known_buckets()
                    .into_keys()
                    .map(|s| s.to_string())
                    .collect();
                eprintln!("error: unknown bucket '{name}', please provide repository URL");
                eprintln!("Known buckets: {}", known.join(", "));
                return ExitCode::FAILURE;
            }
        },
    };
    println!("  Repository: {url}");
    let progress: Box<dyn Fn(ProgressEvent) + Send + Sync> = Box::new(print_progress);
    match engine.add_bucket(&name, Some(url.as_str()), Some(progress)) {
        Ok(()) => {
            println!("  Bucket '{name}' added");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

fn bucket_remove_cmd(names: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    let mut engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let mut failed = Vec::new();
    for name in &names {
        match engine.remove_bucket(name, None) {
            Ok(()) => println!("  Bucket '{name}' removed"),
            Err(e) => {
                eprintln!("  error: failed to remove '{name}': {e}");
                failed.push(name.clone());
            }
        }
    }
    if !failed.is_empty() {
        eprintln!("Failed to remove: {}", failed.join(", "));
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

fn bucket_list_cmd(root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let registry = engine.services().registry();
    let buckets = registry.list();
    if buckets.is_empty() {
        println!("No buckets installed.");
        println!();
        println!("Add a bucket with:");
        println!("  glue bucket add <name>");
        return ExitCode::SUCCESS;
    }
    println!("Installed buckets ({}):", buckets.len());
    println!();
    for b in buckets {
        println!("  {}", b.name);
        if !b.repo_url.is_empty() {
            println!("    {}", b.repo_url);
        }
    }
    ExitCode::SUCCESS
}

fn bucket_update_cmd(names: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    let mut engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let _ = engine.ensure_git();
    if git_unavailable(&engine) {
        eprintln!("error: git unavailable and MinGit could not be bootstrapped");
        return ExitCode::FAILURE;
    }
    if names.is_empty() {
        if engine.services().registry().list().is_empty() {
            println!("No buckets installed.");
            return ExitCode::SUCCESS;
        }
        println!("Updating all buckets...");
        return match engine.update_buckets(None) {
            Ok(sync) => {
                for (name, error) in &sync.failed {
                    eprintln!("  error: failed to update '{name}': {error}");
                }
                if sync.failed.is_empty() {
                    println!("Updated {} bucket(s)", sync.pulled);
                    ExitCode::SUCCESS
                } else {
                    println!(
                        "Updated {} bucket(s), {} failed",
                        sync.pulled,
                        sync.failed.len()
                    );
                    ExitCode::FAILURE
                }
            }
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                ExitCode::FAILURE
            }
        };
    }
    let mut failed = Vec::new();
    for name in &names {
        match engine.pull_bucket(name, None) {
            Ok(()) => println!("Updated bucket '{name}'"),
            Err(e) => {
                eprintln!("  error: failed to update '{name}': {e}");
                failed.push(name.clone());
            }
        }
    }
    if failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

fn bucket_check_cmd(names: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let _ = engine.ensure_git();
    if git_unavailable(&engine) {
        eprintln!("error: git unavailable and MinGit could not be bootstrapped");
        return ExitCode::FAILURE;
    }
    let registry = engine.services().registry();
    let buckets = registry.list();
    if buckets.is_empty() {
        println!("No buckets installed.");
        return ExitCode::SUCCESS;
    }
    let statuses = engine.check_bucket_updates(&names);
    let mut check_failed = 0;
    let mut with_updates = 0;
    for s in &statuses {
        if !s.ok {
            eprintln!("  x {}: {}", s.name, s.error);
            check_failed += 1;
            continue;
        }
        if s.has_updates {
            println!(
                "  * {}  {} -> {}",
                s.name,
                short_commit(&s.local_commit),
                short_commit(&s.remote_commit)
            );
            with_updates += 1;
        } else if !s.local_commit.is_empty() {
            println!(
                "  ok {}  {} (up to date)",
                s.name,
                short_commit(&s.local_commit)
            );
        } else {
            println!("  ok {}  up to date", s.name);
        }
    }
    println!();
    if check_failed > 0 {
        println!("{check_failed} bucket(s) failed to check.");
        return ExitCode::FAILURE;
    }
    if with_updates > 0 {
        println!("  {with_updates} bucket(s) have updates. Run 'glue bucket update' to sync.");
    } else {
        println!("All buckets are up to date.");
    }
    ExitCode::SUCCESS
}

fn bucket_deepen_cmd(names: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    let mut engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let _ = engine.ensure_git();
    if git_unavailable(&engine) {
        eprintln!("error: git unavailable and MinGit could not be bootstrapped");
        return ExitCode::FAILURE;
    }
    let names = if names.is_empty() {
        engine
            .services()
            .registry()
            .list()
            .iter()
            .map(|b| b.name.clone())
            .collect()
    } else {
        names
    };
    if names.is_empty() {
        println!("No buckets installed.");
        return ExitCode::SUCCESS;
    }

    let mut failed = Vec::new();
    for name in &names {
        println!("Fetching history for '{name}' (one-time; the download can be large)...");
        match engine.deepen_bucket(name) {
            Ok(true) => println!("  '{name}' now has full history"),
            Ok(false) => println!("  '{name}' already has full history"),
            Err(e) => {
                eprintln!("  error: failed to deepen '{name}': {e}");
                failed.push(name.clone());
            }
        }
    }
    if failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

fn bucket_known_cmd() -> ExitCode {
    let buckets = bucket::known_buckets();
    println!("Known buckets:");
    for (name, url) in &buckets {
        println!("  {:<15} {}", name, url);
    }
    println!();
    println!("Add a bucket with:");
    println!("  glue bucket add <name>");
    ExitCode::SUCCESS
}

// ---------------------------------------------------------------------------
// cache
// ---------------------------------------------------------------------------

fn cache_cmd(cmd: CacheCommands, root: Option<PathBuf>) -> ExitCode {
    match cmd {
        CacheCommands::List => cache_list_cmd(root),
        CacheCommands::Clear { all, packages } => cache_clear_cmd(all, packages, root),
        CacheCommands::Rebuild => cache_rebuild_cmd(root),
        CacheCommands::Gc => cache_gc_cmd(root),
        CacheCommands::PurgePartials => cache_purge_partials_cmd(root),
        CacheCommands::Verify { repair } => cache_verify_cmd(repair, root),
    }
}

/// `glue cache purge-partials`: delete resumable download fragments.
///
/// `.part` files only exist for interrupted transfers, and a resumed install
/// picks them up by URL id — so this is for reclaiming space, not for fixing
/// anything.
fn cache_purge_partials_cmd(root: Option<PathBuf>) -> ExitCode {
    let root = resolve_root(root);
    let dir = root.join("store").join(".partial");
    // A plain directory sweep rather than an engine op: purging fragments must
    // work even when the index database cannot be opened.
    let purge = gluestick_core::engine::ops::purge_partials(&root);
    if purge.files == 0 {
        println!("No partial downloads at {}", dir.display());
    } else {
        println!(
            "Removed {} partial file(s), freed {} from {}",
            purge.files,
            human_bytes(purge.bytes),
            dir.display()
        );
    }
    if purge.failed > 0 {
        eprintln!("warning: {} fragment(s) could not be deleted", purge.failed);
        ExitCode::FAILURE
    } else {
        ExitCode::SUCCESS
    }
}

fn cache_list_cmd(root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let packages = engine.list_cache_packages();
    if packages.is_empty() {
        println!("No packages in cache index");
        return ExitCode::SUCCESS;
    }
    let summary = engine.cache_summary();
    println!(
        "{:<20} {:<18} {:<22} {:>12} {:<8}",
        "Package", "Version", "Updated", "Size", "Files"
    );
    println!("{}", "-".repeat(90));
    for p in &packages {
        println!(
            "{:<20} {:<18} {:<22} {:>12} {:<8}",
            p.name,
            p.version,
            format_cache_date(&p.installed),
            format_bytes(p.size),
            p.file_count
        );
    }
    println!("{}", "-".repeat(90));
    println!(
        "Total: {} packages, {} files, {}",
        summary.package_count,
        summary.total_files,
        format_bytes(summary.total_size)
    );
    ExitCode::SUCCESS
}

fn cache_clear_cmd(all: bool, packages: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    // Clearing consults the installed table to leave live installs alone.
    let engine = match open_engine_healing(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let report = if all {
        match engine.clear_all_cache_index() {
            Ok(r) => r,
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                return ExitCode::FAILURE;
            }
        }
    } else {
        if packages.is_empty() {
            eprintln!("error: specify package name(s) or use --all");
            return ExitCode::FAILURE;
        }
        match engine.clear_cache_index(&packages) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                return ExitCode::FAILURE;
            }
        }
    };

    for entry in &report.cleared {
        println!(
            "Cleared index for {} ({}, {} files, {})",
            entry.name,
            entry.version,
            entry.file_count,
            format_bytes(entry.size)
        );
    }
    for name in &report.not_indexed {
        println!("Not in cache index: {name}");
    }
    for name in &report.skipped_installed {
        println!("Skipped {name}: still installed (uninstall it first)");
    }

    let nothing_to_do = report.cleared.is_empty()
        && report.not_indexed.is_empty()
        && report.skipped_installed.is_empty();
    if nothing_to_do {
        println!("Cache index is empty");
        // `--all` on an empty index is a no-op success; naming packages that
        // are all absent is a failed request.
        return if all {
            ExitCode::SUCCESS
        } else {
            ExitCode::FAILURE
        };
    }
    if report.cleared.is_empty() {
        println!("No packages cleared");
        if !report.skipped_installed.is_empty() {
            println!(
                "The cache index only holds reinstall metadata; run `glue cache gc` to reclaim disk."
            );
        }
        return if all {
            ExitCode::SUCCESS
        } else {
            ExitCode::FAILURE
        };
    }

    let summary = report.summary();
    println!();
    println!(
        "Cleared {} package(s) from cache index ({} file(s), {})",
        summary.package_count,
        summary.total_files,
        format_bytes(summary.total_size)
    );
    println!(
        "Store blobs kept on disk; run `glue cache gc` to delete the ones nothing references."
    );
    ExitCode::SUCCESS
}

fn cache_rebuild_cmd(root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let on_pkg = |pkg: &str, version: &str, file_count: usize| {
        println!("    + {pkg} {version}@{version} ({file_count} file(s))");
    };
    let on_pkg_ref: &(dyn Fn(&str, &str, usize) + Send + Sync) = &on_pkg;
    match engine.rebuild_cache_index(Some(on_pkg_ref)) {
        Ok(count) => {
            if count == 0 {
                println!("No installed packages found to index");
            } else {
                println!("Indexed {count} package(s)");
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

/// English rendering of core cache-GC phases (the core emits structured
/// events; the CLI owns its own wording, the desktop translates its own).
fn cache_phase_text(phase: &CachePhase) -> String {
    match *phase {
        CachePhase::RemovedStaleTemps { removed, freed } => {
            format!("Removed {removed} stale temp file(s), freed {freed} bytes")
        }
        CachePhase::ScanCacheIndex => "Scanning cache index".to_string(),
        CachePhase::IndexRefs { referenced } => {
            format!("Index references {referenced} blob(s); scanning the cache store")
        }
        CachePhase::ScanAppPackages { packages } => {
            format!("Scanning {packages} installed package(s) for hardlinked blobs")
        }
        CachePhase::ScanStoreProgress { done } => {
            format!("Scanning the cache store ({done} objects)")
        }
        CachePhase::ScanAppProgress { done, total } => {
            format!("Scanning installed packages {done}/{total}")
        }
        CachePhase::Scanned {
            objects,
            referenced,
        } => format!("Scanned {objects} store object(s), {referenced} referenced"),
        CachePhase::KeepInflight { blobs } => {
            format!("Keeping {blobs} blob(s) referenced by a running install")
        }
        CachePhase::DeleteOrphans { orphans } => {
            format!("Deleting {orphans} orphan blob(s)")
        }
        CachePhase::DeleteProgress { done, total } => {
            format!("Deleting {done}/{total} orphan blob(s)")
        }
        CachePhase::ScanStore => "Scanning the cache store".to_string(),
        CachePhase::VerifyObjects { objects } => {
            format!("Verifying {objects} store object(s)")
        }
        CachePhase::VerifyProgress { done, total } => {
            format!("Verifying {done}/{total} store object(s)")
        }
    }
}

fn cache_gc_cmd(root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    // A cold scan of a large store plus every install tree takes minutes on a
    // virtualized/AV-scanned host. Without these lines the command looks hung
    // and gets interrupted before it deletes anything.
    let on_phase = |msg: &CachePhase| println!("  -> {}", cache_phase_text(msg));
    match engine.run_cache_gc_with_progress(Some(&on_phase)) {
        Ok((removed, freed)) => {
            if removed == 0 {
                println!("Cache GC: no unreferenced blobs");
            } else {
                println!(
                    "Cache GC: removed {removed} blob(s), freed {}",
                    format_bytes(freed)
                );
            }
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

/// Re-hash every store blob. Store hits are trusted by name elsewhere, so this
/// is the only way to notice a blob poisoned through a hardlink (in-place edit
/// of an installed file), by antivirus, or by a power loss during ingest.
fn cache_verify_cmd(repair: bool, root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let on_phase = |msg: &CachePhase| println!("  -> {}", cache_phase_text(msg));
    match engine.verify_cache(repair, Some(&on_phase)) {
        Ok(report) => {
            if report.corrupt.is_empty() {
                println!(
                    "Cache verify: {} blob(s) OK ({})",
                    report.checked,
                    format_bytes(report.bytes_checked)
                );
                return ExitCode::SUCCESS;
            }
            for blob in &report.corrupt {
                if blob.error.is_empty() {
                    eprintln!("  {} {} (hash mismatch)", mark_fail(), blob.hash);
                } else {
                    eprintln!("  {} {} ({})", mark_fail(), blob.hash, blob.error);
                }
            }
            if !report.affected_packages.is_empty() {
                // The installed files are hardlinks to the same inode, so
                // they are corrupt too — deleting the blob cannot fix them.
                eprintln!(
                    "  {} installed package(s) share the corrupt content: {}",
                    mark_fail(),
                    report.affected_packages.join(", ")
                );
                eprintln!("     reinstall them: glue uninstall <pkg> && glue install <pkg>");
            }
            println!();
            if repair {
                println!(
                    "Cache verify: deleted {} corrupt blob(s); reinstall the affected packages to fetch them again",
                    report.corrupt.len()
                );
                ExitCode::SUCCESS
            } else {
                println!(
                    "Cache verify: {} of {} blob(s) corrupt; run `glue cache verify --repair` to delete them",
                    report.corrupt.len(),
                    report.checked + report.corrupt.len()
                );
                ExitCode::FAILURE
            }
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

// ---------------------------------------------------------------------------
// root
// ---------------------------------------------------------------------------

fn root_cmd(cmd: Option<RootCommands>, root_override: Option<PathBuf>) -> ExitCode {
    match cmd {
        Some(RootCommands::Set { path, adopt }) => root_set_cmd(&path, adopt),
        Some(RootCommands::Reset) => root_reset_cmd(root_override),
        Some(RootCommands::Show) | None => root_show_cmd(root_override),
    }
}

/// `glue root show`: the effective workspace and where it came from.
fn root_show_cmd(root_override: Option<PathBuf>) -> ExitCode {
    let (root, source) = paths::resolve_data_root_with_source(root_override.as_deref());
    println!("workspace: {}", root.display());
    println!("  source: {}", source.label());
    println!(
        "  state:  {}",
        if root.is_dir() {
            "present"
        } else {
            "missing (created on first use)"
        }
    );
    // Configuration is program-scoped, so this can point outside the workspace
    // (typically the installation directory).
    println!("configuration: {}", config::config_path(&root).display());
    match paths::registry_root() {
        Some(saved) if gluestick_core::uninstall::same_path(&saved, &root) => println!(
            "  saved:  {} (same as the workspace above)",
            saved.display()
        ),
        // A value saved by another installation does not apply here; saying so
        // is better than letting it look like the effective root.
        Some(saved) if source != paths::RootSource::Registry => println!(
            "  saved:  {} (not used here: this binary has its own installation folder; \
             clear with `glue root reset`)",
            saved.display()
        ),
        Some(saved) => println!(
            "  saved:  {} (clear with `glue root reset`)",
            saved.display()
        ),
        None => println!("  saved:  none"),
    }
    match paths::installed_data_root() {
        Some(install) => println!("  this installation: {}", install.display()),
        None => println!("  this installation: none (standalone binary)"),
    }
    if let Some(pref) = paths::install_pref_root() {
        println!("  installation setting: {}", pref.display());
    }
    ExitCode::SUCCESS
}

/// `glue root set <path>`: create the root and save it for future commands.
fn root_set_cmd(path: &Path, adopt: bool) -> ExitCode {
    let root = match absolutize(path) {
        Ok(root) => root,
        Err(e) => {
            eprintln!("error: cannot resolve {}: {e}", path.display());
            return ExitCode::FAILURE;
        }
    };
    if adopt {
        adopt_legacy_workspace(&root);
    }
    // Record the choice before opening the engine, so the engine opens the new
    // root; a failure below points every installation back at the home default.
    if let Err(e) = paths::save_data_root(&root) {
        eprintln!("error: {e}");
        return ExitCode::FAILURE;
    }
    // Opening the engine creates the directory layout (store, cache index,
    // bucket registry), so a freshly pointed root is usable immediately.
    let mut config = EngineConfig::new(&root);
    config.sync_installed = false;
    if let Err(e) = Engine::with_config(config) {
        let _ = paths::clear_saved_root();
        eprintln!("error: cannot initialise {}: {e}", root.display());
        return ExitCode::FAILURE;
    }
    println!(
        "workspace: {} (saved for all future glue commands)",
        root.display()
    );
    if !adopt {
        println!("  note: packages installed under the previous workspace are not moved");
    }
    ExitCode::SUCCESS
}

/// Workspace entries an installation used to keep beside `glue.exe`.
const LEGACY_WORKSPACE_ENTRIES: [&str; 10] = [
    "apps",
    "buckets",
    "bin",
    "cache",
    "overrides",
    "persist",
    "templates",
    "shims",
    "shims-meta",
    "store",
];

/// Move a pre-shared-workspace installation's data into `root`.
///
/// Only used by `glue root set --adopt`, which the installers pass on an
/// upgrade. Each entry moves only when the destination does not already exist,
/// so a workspace that is already in use is never merged into or overwritten.
/// A cross-volume move is reported and skipped rather than half-copied: the
/// data is still in the old location and the user can move it by hand.
///
/// `config.json` is *not* workspace data: it belongs beside the program. When
/// the old shared workspace still has one and this installation does not, a
/// copy is placed in the installation directory (see the end of the function).
fn adopt_legacy_workspace(root: &Path) {
    use gluestick_core::uninstall::same_path;

    let Some(install) = paths::installed_data_root() else {
        return;
    };
    if same_path(&install, root) {
        return;
    }
    if let Err(e) = std::fs::create_dir_all(root) {
        eprintln!("warning: cannot create {}: {e}", root.display());
        return;
    }

    let mut moved = Vec::new();
    for name in LEGACY_WORKSPACE_ENTRIES {
        let source = install.join(name);
        if !source.is_dir() || root.join(name).exists() {
            continue;
        }
        match std::fs::rename(&source, root.join(name)) {
            Ok(()) => moved.push(format!("{name}/")),
            Err(e) => eprintln!(
                "warning: cannot move {} to {}: {e} (left in place)",
                source.display(),
                root.join(name).display()
            ),
        }
    }
    if moved.is_empty() {
        println!("  no data to move from {}", install.display());
    } else {
        println!(
            "  moved {} from {} to {}",
            moved.join(", "),
            install.display(),
            root.display()
        );
    }

    // config.json belongs to the program directory, not the workspace. Older
    // shared-workspace installs left it in `root`; copy it beside the binaries
    // when this installation has no settings yet. Copy rather than move: a
    // sibling product installed later must still inherit the same starting
    // point, and each program then writes its own file from then on.
    let legacy_config = root.join("config.json");
    let program_config = install.join("config.json");
    if legacy_config.is_file() && !program_config.exists() {
        match std::fs::copy(&legacy_config, &program_config) {
            Ok(_) => println!(
                "  copied config.json to {} (configuration lives beside the program)",
                program_config.display()
            ),
            Err(e) => eprintln!(
                "warning: cannot copy {} to {}: {e}",
                legacy_config.display(),
                program_config.display()
            ),
        }
    }
}

/// `glue root reset`: point every installation back at
/// `%USERPROFILE%\.glue`.
fn root_reset_cmd(root_override: Option<PathBuf>) -> ExitCode {
    if let Err(e) = paths::clear_saved_root() {
        eprintln!("error: {e}");
        return ExitCode::FAILURE;
    }
    let (root, source) = paths::resolve_data_root_with_source(root_override.as_deref());
    println!("workspace: {} ({})", root.display(), source.label());
    ExitCode::SUCCESS
}

/// Turns a user-supplied path into an absolute, lexically clean path.
fn absolutize(path: &Path) -> std::io::Result<PathBuf> {
    if path.is_absolute() {
        Ok(path.to_path_buf())
    } else {
        Ok(std::env::current_dir()?.join(path))
    }
}
// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

fn config_cmd(cmd: ConfigCommands, root: Option<PathBuf>) -> ExitCode {
    let root = resolve_root(root);
    match cmd {
        ConfigCommands::Get { key } => config_get_cmd(&key, &root),
        ConfigCommands::Set { key, value } => config_set_cmd(&key, &value, &root),
        ConfigCommands::Unset { key } => config_unset_cmd(&key, &root),
        ConfigCommands::List => config_list_cmd(&root),
    }
}

fn config_get_cmd(key: &str, root: &Path) -> ExitCode {
    let cfg = match config::read_basics(root) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };
    match key {
        "github_proxy" => {
            if cfg.github_proxy.is_empty() {
                println!("(not set)");
            } else {
                println!("{}", cfg.github_proxy);
            }
        }
        "self_update_check_url" => {
            if cfg.self_update_check_url.trim().is_empty() {
                println!("(not set, update checks disabled)");
            } else {
                println!("{}", cfg.self_update_check_url);
            }
        }
        "proxy_packages" => {
            if cfg.proxy_packages.is_empty() {
                println!("(not set, every download is direct)");
            } else {
                println!("{}", cfg.proxy_packages.join(","));
            }
        }
        "proxy" => match config::read_config_proxy(root) {
            Ok(value) if value.is_empty() => {
                println!("auto (environment, then the Windows system proxy)")
            }
            Ok(value) => println!("{value}"),
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                return ExitCode::FAILURE;
            }
        },
        "verbose" => println!("{}", format_bool_opt(cfg.verbose, "false (default)")),
        "bucket_sync_mode" => {
            let mode = if cfg.bucket_sync_mode.trim().is_empty() {
                config::DEFAULT_BUCKET_SYNC_MODE
            } else {
                config::normalize_bucket_sync_mode(&cfg.bucket_sync_mode)
            };
            println!("{mode}");
        }
        "bucket_check_interval_minutes" => {
            let minutes = cfg
                .bucket_check_interval_minutes
                .map(|m| config::normalize_bucket_check_interval(m.max(0) as u64))
                .unwrap_or(config::DEFAULT_BUCKET_CHECK_INTERVAL_MINUTES);
            println!("{minutes}");
        }
        "parallel_download" => {
            println!(
                "{}",
                format_bool_opt(cfg.parallel_download, "true (default)")
            )
        }
        "download_workers" => println!(
            "{}",
            cfg.download_workers
                .map(|n| n.to_string())
                .unwrap_or_else(|| format!("{DEFAULT_WORKERS} (default)"))
        ),
        "cancel_direct_max_mb" => println!(
            "{}",
            cfg.cancel_direct_max_mb
                .map(|mb| mb.to_string())
                .unwrap_or_else(|| {
                    format!("{} (default)", config::DEFAULT_CANCEL_DIRECT_MAX_MB)
                })
        ),
        "color" => println!("{}", format_bool_opt(cfg.color, "true (default)")),
        _ => {
            eprintln!("error: unknown config key: {key}");
            return ExitCode::FAILURE;
        }
    }
    ExitCode::SUCCESS
}

/// `config set proxy …` — `auto`, `direct`, or a proxy URL.
fn set_proxy_config(value: &str, root: &Path) -> ExitCode {
    let value = value.trim();
    let normalized = if value.is_empty() { "auto" } else { value };
    let valid = normalized.eq_ignore_ascii_case("auto")
        || normalized.eq_ignore_ascii_case("direct")
        || normalized.eq_ignore_ascii_case("none")
        || gluestick_core::proxy::parse_proxy_url(normalized).is_some();
    if !valid {
        eprintln!("error: proxy must be 'auto', 'direct', or a URL like http://host:port");
        return ExitCode::FAILURE;
    }
    match config::write_config_proxy(root, normalized) {
        Ok(()) => {
            println!("Set proxy = {normalized}");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

/// `config unset proxy` — back to `auto`.
fn unset_proxy_config(root: &Path) -> ExitCode {
    let current = config::read_config_proxy(root).unwrap_or_default();
    if current.is_empty() {
        println!("proxy is not set (auto)");
        return ExitCode::SUCCESS;
    }
    match config::write_config_proxy(root, "") {
        Ok(()) => {
            println!("Unset proxy (default: auto)");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

fn config_set_cmd(key: &str, value: &str, root: &Path) -> ExitCode {
    if key == "proxy" {
        return set_proxy_config(value, root);
    }
    let mut cfg = match config::read_basics(root) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };
    let message = match key {
        "github_proxy" => {
            cfg.github_proxy = value.to_string();
            format!("Set github_proxy = {value}")
        }
        "proxy_packages" => {
            let packages = parse_package_list(value);
            cfg.proxy_packages = packages.clone();
            if packages.is_empty() {
                "Cleared proxy_packages".to_string()
            } else {
                format!("Set proxy_packages = {}", packages.join(", "))
            }
        }
        "self_update_check_url" => {
            // Clearing stays explicit (`config unset`): an empty value here is
            // almost always a shell variable that did not expand, and silently
            // disabling the check would hide that mistake.
            let url = value.trim();
            let lower = url.to_ascii_lowercase();
            if !(lower.starts_with("http://") || lower.starts_with("https://")) {
                eprintln!("error: self_update_check_url must be an http(s) URL");
                return ExitCode::FAILURE;
            }
            cfg.self_update_check_url = url.to_string();
            format!("Set self_update_check_url = {url}")
        }
        "bucket_sync_mode" => {
            let mode = value.trim().to_ascii_lowercase();
            if mode != config::BUCKET_SYNC_MODE_AUTO && mode != config::BUCKET_SYNC_MODE_MANUAL {
                eprintln!("error: bucket_sync_mode must be 'auto' or 'manual'");
                return ExitCode::FAILURE;
            }
            cfg.bucket_sync_mode = mode.clone();
            format!("Set bucket_sync_mode = {mode}")
        }
        "bucket_check_interval_minutes" => match value.trim().parse::<u64>() {
            Ok(minutes) if config::ALLOWED_BUCKET_CHECK_INTERVALS.contains(&minutes) => {
                cfg.bucket_check_interval_minutes = Some(minutes as i64);
                format!("Set bucket_check_interval_minutes = {minutes}")
            }
            _ => {
                eprintln!("error: bucket_check_interval_minutes must be one of 5, 15, 30");
                return ExitCode::FAILURE;
            }
        },
        "verbose" => match parse_bool(value) {
            Ok(b) => {
                cfg.verbose = Some(b);
                format!("Set verbose = {b}")
            }
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                return ExitCode::FAILURE;
            }
        },
        "parallel_download" => match parse_bool(value) {
            Ok(b) => {
                cfg.parallel_download = Some(b);
                format!("Set parallel_download = {b}")
            }
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                return ExitCode::FAILURE;
            }
        },
        "download_workers" => match value.trim().parse::<u32>() {
            Ok(n) if (MIN_USER_WORKERS..=MAX_USER_WORKERS).contains(&n) => {
                let workers = normalize_user_workers(n);
                cfg.download_workers = Some(workers as i64);
                format!("Set download_workers = {workers}")
            }
            _ => {
                eprintln!(
                    "error: download_workers must be between {MIN_USER_WORKERS} and {MAX_USER_WORKERS}"
                );
                return ExitCode::FAILURE;
            }
        },
        "cancel_direct_max_mb" => match value.trim().parse::<u64>() {
            Ok(mb) if mb <= config::MAX_CANCEL_DIRECT_MAX_MB => {
                cfg.cancel_direct_max_mb = Some(mb as i64);
                format!("Set cancel_direct_max_mb = {mb}")
            }
            _ => {
                eprintln!(
                    "error: cancel_direct_max_mb must be between 0 and {}",
                    config::MAX_CANCEL_DIRECT_MAX_MB
                );
                return ExitCode::FAILURE;
            }
        },
        "color" => match parse_bool(value) {
            Ok(b) => {
                cfg.color = Some(b);
                format!("Set color = {b}")
            }
            Err(e) => {
                eprintln!("error: {}", clean_error(&e.to_string()));
                return ExitCode::FAILURE;
            }
        },
        _ => {
            eprintln!("error: unknown config key: {key}");
            eprintln!(
                "Available keys:\n  github_proxy\n  self_update_check_url\n  proxy\n  proxy_packages\n  bucket_sync_mode\n  bucket_check_interval_minutes\n  parallel_download\n  download_workers\n  cancel_direct_max_mb\n  color\n  verbose"
            );
            return ExitCode::FAILURE;
        }
    };
    // Only claim success after the write lands: printing first produced a
    // "Set …" line followed by a write error when the root did not exist.
    match config::write_basics(root, &cfg) {
        Ok(()) => {
            println!("{message}");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

fn config_unset_cmd(key: &str, root: &Path) -> ExitCode {
    if key == "proxy" {
        return unset_proxy_config(root);
    }
    let mut cfg = match config::read_basics(root) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };
    let message = match key {
        "github_proxy" => {
            if cfg.github_proxy.is_empty() {
                "github_proxy is not set".to_string()
            } else {
                cfg.github_proxy = String::new();
                "Unset github_proxy".to_string()
            }
        }
        "proxy_packages" => {
            if cfg.proxy_packages.is_empty() {
                "proxy_packages is not set".to_string()
            } else {
                cfg.proxy_packages.clear();
                "Unset proxy_packages (every download is direct)".to_string()
            }
        }
        "self_update_check_url" => {
            if cfg.self_update_check_url.trim().is_empty() {
                "self_update_check_url is not set".to_string()
            } else {
                cfg.self_update_check_url = String::new();
                "Unset self_update_check_url".to_string()
            }
        }
        "bucket_sync_mode" => {
            if cfg.bucket_sync_mode.trim().is_empty() {
                "bucket_sync_mode is not set".to_string()
            } else {
                cfg.bucket_sync_mode = String::new();
                "Unset bucket_sync_mode (default: manual)".to_string()
            }
        }
        "bucket_check_interval_minutes" => {
            if cfg.bucket_check_interval_minutes.is_none() {
                "bucket_check_interval_minutes is not set".to_string()
            } else {
                cfg.bucket_check_interval_minutes = None;
                "Unset bucket_check_interval_minutes (default: 15)".to_string()
            }
        }
        "verbose" => {
            if cfg.verbose.is_none() {
                "verbose is not set".to_string()
            } else {
                cfg.verbose = None;
                "Unset verbose (default: disabled)".to_string()
            }
        }
        "parallel_download" => {
            if cfg.parallel_download.is_none() {
                "parallel_download is not set".to_string()
            } else {
                cfg.parallel_download = None;
                "Unset parallel_download (default: enabled)".to_string()
            }
        }
        "download_workers" => {
            if cfg.download_workers.is_none() {
                "download_workers is not set".to_string()
            } else {
                cfg.download_workers = None;
                format!("Unset download_workers (default: {DEFAULT_WORKERS})")
            }
        }
        "cancel_direct_max_mb" => {
            if cfg.cancel_direct_max_mb.is_none() {
                "cancel_direct_max_mb is not set".to_string()
            } else {
                cfg.cancel_direct_max_mb = None;
                format!(
                    "Unset cancel_direct_max_mb (default: {})",
                    config::DEFAULT_CANCEL_DIRECT_MAX_MB
                )
            }
        }
        "color" => {
            if cfg.color.is_none() {
                "color is not set".to_string()
            } else {
                cfg.color = None;
                "Unset color (default: enabled)".to_string()
            }
        }
        _ => {
            eprintln!("error: unknown config key: {key}");
            return ExitCode::FAILURE;
        }
    };
    match config::write_basics(root, &cfg) {
        Ok(()) => {
            println!("{message}");
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            ExitCode::FAILURE
        }
    }
}

fn config_list_cmd(root: &Path) -> ExitCode {
    let cfg = match config::read_basics(root) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };
    println!("Configuration: {}", config::config_path(root).display());
    if cfg.github_proxy.is_empty() {
        println!("  github_proxy = (not set, direct GitHub)");
    } else if cfg
        .github_proxy
        .eq_ignore_ascii_case(config::DIRECT_PROXY_MARKER)
    {
        println!("  github_proxy = direct (auto-detected: no mirror needed)");
    } else {
        println!("  github_proxy = {}", cfg.github_proxy);
    }
    match config::read_config_proxy(root) {
        Ok(value) if value.is_empty() => {
            println!("  proxy = auto (environment, then the Windows system proxy)")
        }
        Ok(value) => println!("  proxy = {value}"),
        Err(_) => println!("  proxy = (unreadable)"),
    }
    if cfg.proxy_packages.is_empty() {
        println!("  proxy_packages = (not set, every download is direct)");
    } else {
        println!("  proxy_packages = {}", cfg.proxy_packages.join(","));
    }
    // Only shown when set: an unset key means the update check is off, which
    // is the default and needs no line of its own.
    if !cfg.self_update_check_url.trim().is_empty() {
        println!("  self_update_check_url = {}", cfg.self_update_check_url);
    }
    println!(
        "  parallel_download = {}",
        format_bool_opt(cfg.parallel_download, "true (default)")
    );
    println!(
        "  download_workers = {}",
        cfg.download_workers
            .map(|n| n.to_string())
            .unwrap_or_else(|| format!("{DEFAULT_WORKERS} (default)"))
    );
    println!(
        "  cancel_direct_max_mb = {}",
        cfg.cancel_direct_max_mb
            .map(|mb| mb.to_string())
            .unwrap_or_else(|| { format!("{} (default)", config::DEFAULT_CANCEL_DIRECT_MAX_MB) })
    );
    println!("  color = {}", format_bool_opt(cfg.color, "true (default)"));
    println!(
        "  verbose = {}",
        format_bool_opt(cfg.verbose, "false (default)")
    );
    let sync_mode = if cfg.bucket_sync_mode.trim().is_empty() {
        config::DEFAULT_BUCKET_SYNC_MODE
    } else {
        config::normalize_bucket_sync_mode(&cfg.bucket_sync_mode)
    };
    println!("  bucket_sync_mode = {sync_mode}");
    let interval = cfg
        .bucket_check_interval_minutes
        .map(|m| config::normalize_bucket_check_interval(m.max(0) as u64))
        .unwrap_or(config::DEFAULT_BUCKET_CHECK_INTERVAL_MINUTES);
    println!("  bucket_check_interval_minutes = {interval}");
    ExitCode::SUCCESS
}

/// Split a comma-separated config list, trimming and dropping empty and
/// duplicate entries (case-insensitively, as package names are matched).
fn parse_package_list(value: &str) -> Vec<String> {
    let mut seen = std::collections::BTreeSet::new();
    let mut out = Vec::new();
    for entry in value.split(',') {
        let name = entry.trim();
        if name.is_empty() || !seen.insert(name.to_ascii_lowercase()) {
            continue;
        }
        out.push(name.to_string());
    }
    out
}

// ---------------------------------------------------------------------------
// hold / unhold / depends / info / reset
// ---------------------------------------------------------------------------

fn hold_cmd(packages: Vec<String>, locked: bool, root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine_healing(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let verb = if locked { "held" } else { "unheld" };
    let mut failed = Vec::new();
    for pkg in &packages {
        match engine.set_package_version_lock(pkg, locked) {
            Ok(()) => println!("  + {pkg} {verb}"),
            Err(e) => {
                eprintln!("  error: {pkg}: {e}");
                failed.push(pkg.clone());
            }
        }
    }
    if failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        eprintln!("\nFailed: {}", failed.join(", "));
        ExitCode::FAILURE
    }
}

fn depends_cmd(packages: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    // The install plan marks which dependencies are already installed.
    let engine = match open_engine_with_catalog(root, true) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let mut failed = Vec::new();
    for (i, pkg) in packages.iter().enumerate() {
        if i > 0 {
            println!();
        }
        match engine.plan_install(pkg) {
            Ok(plan) => {
                println!("{}", plan.package);
                if plan.depends.is_empty() {
                    println!("  Dependencies: (none missing)");
                } else {
                    println!("  Dependencies (will install first):");
                    for d in &plan.depends {
                        println!("    - {}", d.ref_);
                    }
                }
                if !plan.suggestions.is_empty() {
                    println!("  Suggestions (optional):");
                    for s in &plan.suggestions {
                        let status = if s.installed {
                            "installed"
                        } else {
                            "not installed"
                        };
                        let label = if s.label.is_empty() {
                            s.ref_.clone()
                        } else {
                            format!("{} -> {}", s.label, s.ref_)
                        };
                        println!("    * {label} ({status})");
                    }
                }
            }
            Err(e) => {
                eprintln!("  error: {pkg}: {e}");
                failed.push(pkg.clone());
            }
        }
    }
    if failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

fn info_cmd(packages: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine_with_catalog(root, true) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let mut failed = Vec::new();
    for (i, pkg) in packages.iter().enumerate() {
        if i > 0 {
            println!();
        }
        // `info.go`: strip `bucket/` and `@version` before the installed
        // lookup — installed packages live under their bare name in
        // `apps/<name>`, so a qualified ref must not be passed through raw.
        let pkg_name = gluestick_core::catalog::package_base_name(pkg);
        match engine.get_installed_package_detail(&pkg_name) {
            Ok(d) => {
                println!("{}@{}", d.name, d.version);
                print_info_field("Status", "Installed");
                print_info_field("Path", &d.install_path);
                print_info_field("Current", &d.current_path);
                if !d.installed_at.is_empty() {
                    print_info_field("Installed", &format_cache_date(&d.installed_at));
                    print_info_field(
                        "Size",
                        &format!("{} ({} indexed files)", format_bytes(d.size), d.file_count),
                    );
                } else {
                    print_info_field(
                        "Size",
                        &format!("{} ({} files)", format_bytes(d.size), d.file_count),
                    );
                }
                if !d.shims.is_empty() {
                    let mut shims = d.shims.clone();
                    shims.sort();
                    print_info_field("Shims", &shims.join(", "));
                }
                if !d.bucket.is_empty() {
                    print_info_field("Bucket", &d.bucket);
                }
                if !d.description.is_empty() {
                    print_info_field("Description", &d.description);
                }
                if !d.homepage.is_empty() {
                    print_info_field("Homepage", &d.homepage);
                }
                if !d.license.is_empty() {
                    print_info_field("License", &d.license);
                }
                if !d.depends.is_empty() {
                    print_info_field("Dependencies", &d.depends.join(", "));
                }
                if !d.notes.is_empty() {
                    print_info_field("Notes", &d.notes.join("; "));
                }
                if d.update_available {
                    print_info_field(
                        "Manifest",
                        &format!("{} (update available)", d.latest_version),
                    );
                }
            }
            Err(e) => {
                eprintln!("  error: {pkg}: {e}");
                failed.push(pkg.clone());
            }
        }
    }
    if failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

fn reset_cmd(packages: Vec<String>, root: Option<PathBuf>) -> ExitCode {
    let engine = match open_engine_healing(root) {
        Ok(e) => e,
        Err(code) => return code,
    };
    let mut failed = Vec::new();
    for pkg in &packages {
        println!("Resetting {pkg}...");
        match engine.reset_package(pkg) {
            Ok(()) => println!("  ok {pkg} reset"),
            Err(e) => {
                eprintln!("  error: {pkg} failed: {e}");
                failed.push(pkg.clone());
            }
        }
    }
    if failed.is_empty() {
        ExitCode::SUCCESS
    } else {
        eprintln!("\nFailed to reset: {}", failed.join(", "));
        ExitCode::FAILURE
    }
}

// ---------------------------------------------------------------------------
// completion (dynamic: the shell calls `glue __complete` back on every <TAB>)
//
// `glue completion <shell>` prints a script that hands the words already on the
// line to `glue __complete`, so candidates come from the same engine the
// command itself uses: bucket catalog, installed versions, flags. A generated
// static name list would be stale as soon as a bucket updated.
// ---------------------------------------------------------------------------

const BASH_COMPLETION: &str = r#"#!/usr/bin/env bash
# glue bash completion: each <TAB> asks the binary for candidates, so package
# names and installed versions stay current. Calls the command as typed, so a
# full path works even when glue is not on PATH.
_glue_completions() {
    local out line
    out=$("${COMP_WORDS[0]}" __complete "$COMP_CWORD" "${COMP_WORDS[@]}" 2>/dev/null)
    COMPREPLY=()
    if [ -n "$out" ]; then
        while IFS= read -r line; do
            COMPREPLY+=("$line")
        done <<< "$out"
    fi
    return 0
}
complete -F _glue_completions glue
"#;

const ZSH_COMPLETION: &str = r#"#compdef glue
# glue zsh completion: each <TAB> asks the binary for candidates.
# Install as a file named `_glue` somewhere on $fpath.
_glue() {
    local line
    local -a candidates
    while IFS= read -r line; do
        [[ -n $line ]] && candidates+=("$line")
    done < <("$words[1]" __complete $((CURRENT - 1)) "${words[@]}" 2>/dev/null)
    if (( ${#candidates} )); then
        compadd -- "${candidates[@]}"
    fi
    return 0
}
_glue "$@"
"#;

const FISH_COMPLETION: &str = r#"# glue fish completion: each <TAB> asks the binary for candidates.
function __glue_complete
    set -l tokens (commandline -opc)
    set -l current (commandline -ct)
    if not set -q tokens[1]
        return
    end
    $tokens[1] __complete (count $tokens) $tokens $current 2>/dev/null
end
complete -c glue -f -a '(__glue_complete)'
"#;

const POWERSHELL_COMPLETION: &str = r#"# glue powershell completion: each <TAB> asks the binary for candidates, so it
# works for `glue` on PATH and for a path like .\target\release\glue.exe.
Register-ArgumentCompleter -Native -CommandName glue -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $words = @($commandAst.CommandElements | ForEach-Object { $_.Extent.Text })
    if ([string]::IsNullOrEmpty($wordToComplete)) {
        $index = $words.Count
        $words += ''
    } else {
        $index = $words.Count - 1
    }
    $exe = $words[0]
    & $exe __complete $index @words 2>$null | ForEach-Object {
        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
    }
}
"#;

fn completion_cmd(shell: &str) -> ExitCode {
    match shell {
        "bash" => print!("{BASH_COMPLETION}"),
        "zsh" => print!("{ZSH_COMPLETION}"),
        "fish" => print!("{FISH_COMPLETION}"),
        "powershell" => print!("{POWERSHELL_COMPLETION}"),
        _ => {
            eprintln!("error: unknown shell '{shell}' (expected bash|zsh|fish|powershell)");
            return ExitCode::FAILURE;
        }
    }
    ExitCode::SUCCESS
}

/// What the word under the cursor should be completed from.
enum CompletionKind {
    /// Top-level commands (`glue <TAB>`).
    Commands,
    /// Nested commands of one command (`glue bucket <TAB>`).
    Subcommands(String),
    /// Every package in the catalog, as `name` or `bucket/name`.
    Packages,
    /// Installed packages, and `pkg@version` once an `@` is on the line.
    Installed,
    /// Bucket names.
    Buckets,
    /// Template names.
    Templates,
    /// `glue completion <shell>` values.
    Shells,
    /// Free text (a search query) or a value we cannot know.
    Nothing,
}

/// Answer one completion request: `index` is the 0-based position of the word
/// being completed in `words`, which the shell passes whole (`glue` included).
fn complete_cmd(index: usize, words: &[String], root_override: Option<PathBuf>) -> ExitCode {
    // Some shells omit the still-empty word under the cursor; normalize so the
    // "before the cursor" / "current word" split is always the same.
    let mut words: Vec<String> = words.to_vec();
    if words.len() <= index {
        words.resize(index + 1, String::new());
    }
    let current = words[index].clone();
    let before: Vec<&str> = words[..index].iter().map(String::as_str).collect();
    let positionals = positional_words(&before);

    // A flag value we can name: `--arch`, and search's `--bucket`.
    if let Some(prev) = index.checked_sub(1).map(|i| words[i].as_str()) {
        match prev {
            "--arch" => {
                emit(["64bit", "arm64", "32bit"].map(String::from), &current);
                return ExitCode::SUCCESS;
            }
            "--bucket" => return complete_buckets(&current, root_override),
            _ => {}
        }
    }

    // The word under the cursor is a flag: list the flags in scope.
    if current.starts_with('-') {
        // `build` materializes clap's own `--help` / `--version` arguments;
        // without it they would be missing from an otherwise usable list.
        let mut cmd = command_path(&positionals);
        cmd.build();
        let mut names = Vec::new();
        for arg in cmd.get_arguments() {
            if arg.is_hide_set() {
                continue;
            }
            if let Some(long) = arg.get_long() {
                names.push(format!("--{long}"));
            }
            if let Some(short) = arg.get_short() {
                names.push(format!("-{short}"));
            }
        }
        emit(names, &current);
        return ExitCode::SUCCESS;
    }

    match completion_kind(&positionals) {
        CompletionKind::Commands => emit(command_names(), &current),
        CompletionKind::Subcommands(parent) => emit(subcommand_names(&parent), &current),
        CompletionKind::Shells => emit(
            ["bash", "zsh", "fish", "powershell"].map(String::from),
            &current,
        ),
        CompletionKind::Buckets => return complete_buckets(&current, root_override),
        CompletionKind::Templates => return complete_templates(&current, root_override),
        CompletionKind::Packages => return complete_packages(&current, root_override),
        CompletionKind::Installed => return complete_installed(&current, root_override),
        CompletionKind::Nothing => {}
    }
    ExitCode::SUCCESS
}

/// The positional words before the cursor: flags are skipped, along with the
/// value of `--root` (the one option that may sit in front of a command name).
fn positional_words<'a>(words: &[&'a str]) -> Vec<&'a str> {
    let mut out = Vec::new();
    let mut iter = words.iter().copied().skip(1); // program name
    while let Some(word) = iter.next() {
        if word == "--root" {
            iter.next();
            continue;
        }
        if word.starts_with('-') {
            continue;
        }
        out.push(word);
    }
    out
}

fn completion_kind(positionals: &[&str]) -> CompletionKind {
    let sub = positionals.first().copied();
    let sub2 = positionals.get(1).copied();
    match sub {
        None => CompletionKind::Commands,
        Some("install") => CompletionKind::Packages,
        Some(
            "uninstall" | "update" | "up" | "list" | "info" | "reset" | "hold" | "unhold"
            | "depends",
        ) => CompletionKind::Installed,
        Some("completion") => CompletionKind::Shells,
        Some("bucket") => match sub2 {
            None => CompletionKind::Subcommands("bucket".to_string()),
            Some("remove" | "update" | "check" | "deepen") => CompletionKind::Buckets,
            Some(_) => CompletionKind::Nothing,
        },
        Some("template") => match sub2 {
            None => CompletionKind::Subcommands("template".to_string()),
            Some("show" | "install" | "update" | "uninstall") => CompletionKind::Templates,
            Some(_) => CompletionKind::Nothing,
        },
        Some("config") => match sub2 {
            None => CompletionKind::Subcommands("config".to_string()),
            Some("override" | "download") => CompletionKind::Packages,
            Some(_) => CompletionKind::Nothing,
        },
        Some(parent @ ("cache" | "root" | "path")) => match sub2 {
            None => CompletionKind::Subcommands(parent.to_string()),
            Some(_) => CompletionKind::Nothing,
        },
        Some(_) => CompletionKind::Nothing,
    }
}

/// The clap command the cursor sits in, so its flags (and nested commands) can
/// be listed. Words that are not command names (flag values, package names)
/// end the walk.
fn command_path(positionals: &[&str]) -> Command {
    let mut cmd = Cli::command();
    for name in positionals {
        let next = match cmd.find_subcommand(name) {
            Some(sub) => sub.clone(),
            None => break,
        };
        cmd = next;
    }
    cmd
}

fn command_names() -> Vec<String> {
    Cli::command()
        .get_subcommands()
        .filter(|c| !c.is_hide_set())
        .map(|c| c.get_name().to_string())
        .collect()
}

fn subcommand_names(parent: &str) -> Vec<String> {
    let root = Cli::command();
    match root.find_subcommand(parent) {
        Some(cmd) => cmd
            .get_subcommands()
            .filter(|c| !c.is_hide_set())
            .map(|c| c.get_name().to_string())
            .collect(),
        None => Vec::new(),
    }
}

/// Print `candidates` that start with `prefix`, one per line, sorted.
fn emit(candidates: impl IntoIterator<Item = String>, prefix: &str) {
    let lower = prefix.to_lowercase();
    let mut out: Vec<String> = candidates
        .into_iter()
        .filter(|c| c.to_lowercase().starts_with(&lower))
        .collect();
    out.sort();
    out.dedup();
    for candidate in out {
        println!("{candidate}");
    }
}

/// Open the engine for a completion. A failure stays silent: this runs on
/// every <TAB>, so it must never print an error or reach the network.
fn completion_engine(root_override: Option<PathBuf>) -> Option<Engine> {
    let mut config = EngineConfig::new(resolve_root(root_override));
    config.sync_installed = false;
    Engine::with_config(config).ok()
}

fn complete_buckets(current: &str, root_override: Option<PathBuf>) -> ExitCode {
    let Some(engine) = completion_engine(root_override) else {
        return ExitCode::SUCCESS;
    };
    emit(engine.list_buckets(), current);
    ExitCode::SUCCESS
}

fn complete_templates(current: &str, root_override: Option<PathBuf>) -> ExitCode {
    let root = resolve_root(root_override);
    emit(
        templates::load_all(&root).into_iter().map(|r| r.name),
        current,
    );
    ExitCode::SUCCESS
}

fn complete_packages(current: &str, root_override: Option<PathBuf>) -> ExitCode {
    let Some(engine) = completion_engine(root_override) else {
        return ExitCode::SUCCESS;
    };
    // One flat pass over the catalog: an empty search query matches nothing,
    // and the paginated query caps at 200 rows, so neither can answer "all".
    let root = engine.glue_root().to_path_buf();
    let index = engine.services().search_index();
    let entries = all_catalog_entries(Some(&index), &root);
    // `bucket/name` narrows to one bucket; a bare prefix matches the name.
    let bucket = current.split_once('/').map(|(bucket, _)| bucket);
    let names = entries.into_iter().filter_map(|entry| match bucket {
        Some(bucket) if !entry.bucket.eq_ignore_ascii_case(bucket) => None,
        Some(_) => Some(format!("{}/{}", entry.bucket, entry.name)),
        None => Some(entry.name),
    });
    emit(names, current);
    ExitCode::SUCCESS
}

fn complete_installed(current: &str, root_override: Option<PathBuf>) -> ExitCode {
    let Some(engine) = completion_engine(root_override) else {
        return ExitCode::SUCCESS;
    };
    // `pkg@` completes that package's installed versions (for `reset` and
    // `uninstall`, which take `pkg@version`).
    if let Some((package, _)) = current.split_once('@') {
        let mut names = Vec::new();
        for pkg in engine.list_installed_all_versions(&[package.to_string()]) {
            for version in &pkg.versions {
                names.push(format!("{}@{version}", pkg.name));
            }
        }
        emit(names, current);
        return ExitCode::SUCCESS;
    }
    let Ok(packages) = engine.list_installed() else {
        return ExitCode::SUCCESS;
    };
    emit(packages.into_iter().map(|p| p.name), current);
    ExitCode::SUCCESS
}

// ---------------------------------------------------------------------------
// path / shim-run (Windows PATH integration and shim bridge — partial port)
// ---------------------------------------------------------------------------

fn path_cmd(cmd: PathCommands, root: Option<PathBuf>) -> ExitCode {
    match cmd {
        PathCommands::Show => path_show_cmd(root),
        PathCommands::Check => path_check_cmd(root),
        PathCommands::Setup => path_setup_cmd(root),
        PathCommands::Remove => path_remove_cmd(root),
    }
}

/// The user-PATH entries Glue owns.
///
/// The shims directory comes first so Microsoft Store app-execution aliases
/// cannot shadow Glue's shims; the directory holding `glue.exe` follows so the
/// CLI itself stays reachable. An installation keeps them apart
/// (`<ROOT>\shims` and `<ROOT>`, which also holds `glue.exe`), while a
/// standalone build collapses both onto `glue_root\shims` unless the binary
/// lives in its own directory.
fn glue_path_dirs(root: Option<PathBuf>) -> Vec<PathBuf> {
    let shims = resolve_root(root).join("shims");
    let mut dirs = vec![shims.clone()];
    if let Some(bin) = pathenv::current_exe_dir() {
        if bin != shims {
            dirs.push(bin);
        }
    }
    dirs
}

fn path_show_cmd(root: Option<PathBuf>) -> ExitCode {
    let root = resolve_root(root);
    match shim::Manager::new(&root) {
        Ok(mgr) => {
            println!("{}", mgr.bin_dir().display());
            ExitCode::SUCCESS
        }
        Err(e) => {
            eprintln!("error: failed to initialize shim manager: {e}");
            ExitCode::FAILURE
        }
    }
}

/// Reports whether both Glue PATH entries are present and not shadowed by the
/// Store's python aliases (`path check`).
fn path_check_cmd(root: Option<PathBuf>) -> ExitCode {
    let dirs = glue_path_dirs(root);
    let user = match pathenv::user_path() {
        Ok(path) => path,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };

    let missing: Vec<&PathBuf> = dirs
        .iter()
        .filter(|dir| !pathenv::path_contains(&user.value, &dir.to_string_lossy()))
        .collect();
    let shims = dirs[0].to_string_lossy().into_owned();
    let process_path = std::env::var("PATH").unwrap_or_default();
    let shadowed = pathenv::windows_apps_dir()
        .is_some_and(|apps| pathenv::dir_precedes(&process_path, &apps.to_string_lossy(), &shims));

    if missing.is_empty() {
        println!("{} Glue is in PATH", mark_success());
    } else {
        println!("{} Glue is NOT in PATH", mark_fail());
        println!();
        println!("Add the following to your PATH:");
        for dir in &missing {
            println!("{}", dir.display());
        }
        println!();
        println!("Or run: glue path setup");
    }
    if shadowed {
        println!(
            "{} Microsoft Store python aliases may shadow Glue shims",
            mark_fail()
        );
        println!("    -> Run: glue path setup");
    }
    if missing.is_empty() && !shadowed {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

fn path_setup_cmd(root: Option<PathBuf>) -> ExitCode {
    let dirs = glue_path_dirs(root.clone());
    println!("Putting {} first on user PATH...", dirs[0].display());

    let user = match pathenv::user_path() {
        Ok(path) => path,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };
    let (next, changed) = pathenv::ensure_dirs_first(&user.value, &dirs);
    if changed {
        let kind = if user.exists {
            user.kind
        } else {
            pathenv::PathKind::Sz
        };
        if let Err(e) = pathenv::set_user_path(&next, kind) {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
        pathenv::broadcast_environment_change();
    }
    pathenv::prepend_dirs_to_process_path(&dirs);
    pathenv::disable_windows_python_aliases();

    // An installer upgrade replaces shim.exe, but existing shims are copies of
    // the old stub. Refresh them so fixes reach already-installed commands.
    if let Ok(count) = shim::refresh_shim_copies(&resolve_root(root)) {
        if count > 0 {
            println!("{} Refreshed {count} shim(s)", mark_success());
        }
    }

    if changed {
        println!("{} Added to user PATH", mark_success());
        if std::io::stdout().is_terminal() {
            println!();
            println!("warning: restart your terminal for changes to take effect");
        }
    } else {
        println!("{} glue shims already first on user PATH", mark_success());
    }
    ExitCode::SUCCESS
}

/// Removes Glue's entries from the user PATH.
///
/// The CLI and the desktop app install into separate directories but share one
/// workspace, so the `shims` entry belongs to both: it is kept while the other
/// product is still installed, and only the exe directory of the product being
/// removed goes. The last product to leave removes the shims entry too.
///
/// Not in the CLI (its NSIS uninstaller runs a PowerShell script), but the
/// Rust installer has no PowerShell step and NSIS cannot hold a PATH longer
/// than 1024 characters — see [`gluestick_core::pathenv`].
fn path_remove_cmd(root: Option<PathBuf>) -> ExitCode {
    let shims = resolve_root(root.clone()).join("shims");
    let mut dirs = glue_path_dirs(root);
    if let Some(own) = pathenv::current_exe_dir() {
        let sibling_installed = gluestick_core::uninstall::registered_installation_dirs()
            .iter()
            .any(|dir| !gluestick_core::uninstall::same_path(dir, &own));
        if sibling_installed {
            dirs.retain(|dir| !gluestick_core::uninstall::same_path(dir, &shims));
        }
    }
    let user = match pathenv::user_path() {
        Ok(path) => path,
        Err(e) => {
            eprintln!("error: {}", clean_error(&e.to_string()));
            return ExitCode::FAILURE;
        }
    };
    let (next, changed) = pathenv::remove_dirs(&user.value, &dirs);
    if !changed {
        println!("Glue is not on the user PATH");
        return ExitCode::SUCCESS;
    }

    let result = if next.is_empty() {
        pathenv::delete_user_path()
    } else {
        pathenv::set_user_path(&next, user.kind)
    };
    if let Err(e) = result {
        eprintln!("error: {}", clean_error(&e.to_string()));
        return ExitCode::FAILURE;
    }
    pathenv::broadcast_environment_change();

    for dir in &dirs {
        if pathenv::path_contains(&user.value, &dir.to_string_lossy()) {
            println!("Removed from user PATH: {}", dir.display());
        }
    }
    ExitCode::SUCCESS
}

fn shim_run_cmd(name: &str, args: &[String]) -> ExitCode {
    let _ = (name, args);
    eprintln!("error: shim-run is not yet implemented in this build (known stage-3 gap)");
    ExitCode::FAILURE
}

/// Internal installer helper: run `program`, forward its stdout/stderr to this
/// process (so the NSIS details pane stays live), and append the same bytes to
/// `log_path`.
fn tee_cmd(log_path: &Path, program: &Path, args: &[String]) -> ExitCode {
    let log = match std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(log_path)
    {
        Ok(file) => Arc::new(Mutex::new(file)),
        Err(error) => {
            eprintln!("error: open log {}: {error}", log_path.display());
            return ExitCode::FAILURE;
        }
    };

    let mut command = ProcessCommand::new(program);
    for arg in args {
        command.arg(arg.as_str());
    }
    let mut child = match command
        .stdin(Stdio::inherit())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
    {
        Ok(child) => child,
        Err(error) => {
            eprintln!("error: run {}: {error}", program.display());
            return ExitCode::FAILURE;
        }
    };

    let mut readers = Vec::new();
    if let Some(stdout) = child.stdout.take() {
        let log = Arc::clone(&log);
        readers.push(std::thread::spawn(move || {
            tee_stream(stdout, std::io::stdout(), log);
        }));
    }
    if let Some(stderr) = child.stderr.take() {
        let log = Arc::clone(&log);
        readers.push(std::thread::spawn(move || {
            tee_stream(stderr, std::io::stderr(), log);
        }));
    }
    for reader in readers {
        let _ = reader.join();
    }

    match child.wait() {
        Ok(status) if status.success() => ExitCode::SUCCESS,
        Ok(_) => ExitCode::FAILURE,
        Err(error) => {
            eprintln!("error: wait for {}: {error}", program.display());
            ExitCode::FAILURE
        }
    }
}

fn tee_stream(mut source: impl Read, mut sink: impl Write, log: Arc<Mutex<std::fs::File>>) {
    let mut reader = std::io::BufReader::new(&mut source);
    let mut line = Vec::new();
    loop {
        line.clear();
        match reader.read_until(b'\n', &mut line) {
            Ok(0) => break,
            Ok(_) => {
                let _ = sink.write_all(&line);
                let _ = sink.flush();
                // The installer's own log lines use CRLF; normalize the
                // child's LF-only lines so Notepad shows one line per record.
                let mut logged = line.clone();
                if logged.last() == Some(&b'\n')
                    && (logged.len() < 2 || logged[logged.len() - 2] != b'\r')
                {
                    logged.insert(logged.len() - 1, b'\r');
                }
                if let Ok(mut file) = log.lock() {
                    let _ = file.write_all(&logged);
                    let _ = file.flush();
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(_) => break,
        }
    }
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

/// Strip a package ref down to its bare name: drop any `bucket/` prefix and
/// `@version` suffix..
fn package_base_name(ref_: &str) -> String {
    let after_slash = match ref_.rsplit_once('/') {
        Some((_, n)) => n,
        None => ref_,
    };
    let before_at = match after_slash.split_once('@') {
        Some((n, _)) => n,
        None => after_slash,
    };
    before_at.to_string()
}

fn format_bool_opt(v: Option<bool>, default_str: &str) -> String {
    match v {
        None => default_str.to_string(),
        Some(true) => "true".to_string(),
        Some(false) => "false".to_string(),
    }
}

fn parse_bool(value: &str) -> Result<bool, String> {
    match value.trim().to_lowercase().as_str() {
        "true" | "1" | "yes" | "on" => Ok(true),
        "false" | "0" | "no" | "off" | "" => Ok(false),
        _ => Err(format!("invalid boolean {:?} (use true or false)", value)),
    }
}

fn format_bytes(n: i64) -> String {
    const UNITS: [&str; 5] = ["B", "KB", "MB", "GB", "TB"];
    let mut size = n as f64;
    let mut unit = 0;
    while size >= 1024.0 && unit < UNITS.len() - 1 {
        size /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{} B", n)
    } else {
        format!("{:.1} {}", size, UNITS[unit])
    }
}

/// Format an RFC 3339 timestamp to its date portion (YYYY-MM-DD).
fn format_cache_date(s: &str) -> String {
    if s.is_empty() {
        return String::new();
    }
    s.split(['T', ' ']).next().unwrap_or(s).to_string()
}

fn short_commit(sha: &str) -> String {
    let t = sha.trim();
    if t.len() <= 7 {
        t.to_string()
    } else {
        t[..7].to_string()
    }
}

fn print_info_field(label: &str, value: &str) {
    println!("  {:<14} {}", format!("{label}:"), value);
}

#[cfg(test)]
mod progress_tests {
    use super::*;

    fn rendered(mp: &mut MultiProgress) -> String {
        let mut out = Vec::new();
        mp.render(&mut out).unwrap();
        String::from_utf8_lossy(&out).into_owned()
    }

    #[test]
    fn clean_error_strips_engine_context_prefixes() {
        assert_eq!(
            clean_error("resolve package: find manifest: manifest not found: aaa"),
            "manifest not found: aaa"
        );
        assert_eq!(
            clean_error(
                "find manifest: manifest not found: x\n\nDid you mean:\n  glue install extras/x"
            ),
            "manifest not found: x\n\nDid you mean:\n  glue install extras/x"
        );
        // Unrelated messages keep their context.
        assert_eq!(
            clean_error("download git.zip failed: timeout"),
            "download git.zip failed: timeout"
        );
    }

    #[test]
    fn proxy_wins_only_when_it_beats_direct_by_the_margin() {
        assert!(proxy_is_faster(0.0, 1.0), "direct failed");
        assert!(proxy_is_faster(100.0, 130.0));
        assert!(!proxy_is_faster(100.0, 120.0));
        assert!(!proxy_is_faster(100.0, 100.0));
        assert!(!proxy_is_faster(0.0, 0.0), "neither route answered");
    }

    /// Bootstrap downloads share the package download line, so they show the
    /// same percentage / byte counters / throughput. An unknown size (chunked
    /// body) still gets a bar, but a sweeping one with no percentage.
    #[test]
    fn download_line_shows_percentage_and_size() {
        end_progress_line();
        let unknown = download_line_text(512, 0);
        assert!(unknown.contains("512 B"), "{unknown}");
        assert!(unknown.contains("["), "{unknown}");
        assert!(!unknown.contains('%'), "{unknown}");
        let known = download_line_text(512, 1024);
        assert!(known.contains(" 50%"), "{known}");
        assert!(known.contains("512 B / 1.0 KB"), "{known}");
        end_progress_line();
    }

    /// The unknown-size bar keeps moving and never grows past its width.
    #[test]
    fn indeterminate_bar_sweeps_within_its_width() {
        let widths: Vec<usize> = (0..40)
            .map(|tick| {
                let bar = indeterminate_bar(Duration::from_millis(tick * 120));
                let cells = bar.trim_matches(|c| c == '[' || c == ']').chars().count();
                assert!(bar.starts_with('[') && bar.ends_with(']'), "{bar}");
                cells
            })
            .collect();
        // Always 30 cells wide: the bar must not shift the bytes/rate columns.
        assert!(widths.iter().all(|w| *w == 30), "{widths:?}");
        let seen: std::collections::BTreeSet<String> = (0..40)
            .map(|tick| indeterminate_bar(Duration::from_millis(tick * 120)))
            .collect();
        assert!(seen.len() > 20, "the segment must actually move");
    }

    #[test]
    fn link_line_shows_percentage_and_file_counts() {
        assert_eq!(link_line_text(0, 0), "    linking files...");
        let line = link_line_text(50, 200);
        assert!(line.contains(" 25%"), "{line}");
        assert!(line.contains("50 / 200 files"), "{line}");
    }

    /// The block draws one line per sampled package and, on a redraw, first
    /// moves the cursor back to the block start.
    #[test]
    fn multi_block_draws_and_redraws_in_place() {
        let mut mp = MultiProgress::new();
        mp.sample("yazi", 10, 100);
        mp.sample("vim", 20, 200);

        let first = rendered(&mut mp);
        assert_eq!(mp.drawn, 2);
        assert!(first.contains("-> yazi"), "{first:?}");
        assert!(first.contains("-> vim"), "{first:?}");
        assert!(!first.starts_with("\x1b["), "no cursor move on first draw");

        let second = rendered(&mut mp);
        assert!(second.starts_with("\x1b[2A"), "{second:?}");
        assert_eq!(mp.drawn, 2);
    }

    /// A shrinking block erases the stale line and leaves the cursor below the
    /// remaining one.
    #[test]
    fn multi_block_shrinks_and_erases_leftovers() {
        let mut mp = MultiProgress::new();
        mp.sample("yazi", 10, 100);
        mp.sample("vim", 20, 200);
        rendered(&mut mp);

        mp.finish("yazi");
        let shrunk = rendered(&mut mp);
        assert_eq!(mp.drawn, 1);
        assert!(shrunk.contains("\x1b[K\n\x1b[1A"), "{shrunk:?}");
        assert!(!shrunk.contains("yazi"), "{shrunk:?}");
    }

    /// `clear` parks the cursor on the first block line so a permanent
    /// `println!` replaces the block instead of appending after it.
    #[test]
    fn multi_block_clear_returns_to_first_line() {
        let mut mp = MultiProgress::new();
        mp.sample("yazi", 10, 100);
        mp.sample("vim", 20, 200);
        rendered(&mut mp);

        let mut out = Vec::new();
        mp.clear(&mut out).unwrap();
        let text = String::from_utf8_lossy(&out);
        assert_eq!(mp.drawn, 0);
        assert!(text.contains("\x1b[2A\r"), "{text:?}");
        assert!(text.contains("\x1b[1A\r"), "{text:?}");
    }

    /// Unknown totals still get a byte counter, and known totals add the
    /// percentage/ETA shape used by the single-package bar.
    #[test]
    fn multi_progress_line_shapes() {
        let unknown = multi_progress_line("pkg", 1024, 0, 0.0, Duration::ZERO);
        assert!(unknown.starts_with("  -> pkg  "), "{unknown:?}");
        assert!(unknown.contains("1.0 KB"), "{unknown:?}");

        let known = multi_progress_line("pkg", 50, 100, 10.0, Duration::from_secs(2));
        assert!(known.contains(" 50%"), "{known:?}");
        assert!(known.contains('[') && known.contains(']'), "{known:?}");
        assert!(known.contains("100 B"), "{known:?}");
        assert!(known.contains("eta "), "{known:?}");

        // Long names are truncated so the block line still fits ~78 columns.
        let long = multi_progress_line(
            "averyveryverylongpackagename",
            50,
            100,
            10.0,
            Duration::from_secs(2),
        );
        assert!(long.chars().count() <= 78, "{long:?}");
        assert!(long.contains('~'), "{long:?}");
    }

    #[test]
    fn short_description_collapses_whitespace_and_truncates() {
        assert_eq!(short_description(""), None);
        assert_eq!(short_description("  \n\t "), None);
        assert_eq!(
            short_description("Fast\nterminal  file manager").as_deref(),
            Some("Fast terminal file manager")
        );

        let long = "x".repeat(400);
        let short = short_description(&long).expect("non-empty");
        assert_eq!(short.chars().count(), 160);
        assert!(short.ends_with('~'));
    }
}
