//! CLI integration tests: launch the real `glue` binary and assert exit codes
//! and user-visible output against **isolated temporary data roots**.
//!
//! Every case points `--root` at its own temp directory and never touches the
//! real `~/.glue`; cases are independent and safe to run in parallel.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Output};
use std::sync::atomic::{AtomicU64, Ordering};

static NEXT_ID: AtomicU64 = AtomicU64::new(0);

/// RAII temporary data root.
struct TempRoot {
    path: PathBuf,
}

impl TempRoot {
    fn new(tag: &str) -> TempRoot {
        let id = NEXT_ID.fetch_add(1, Ordering::Relaxed);
        let path = std::env::temp_dir().join(format!("glue-cli-{tag}-{}-{id}", std::process::id()));
        fs::create_dir_all(&path).expect("create temp root");
        TempRoot { path }
    }

    fn path(&self) -> &Path {
        &self.path
    }

    fn write_bucket_manifest(&self, bucket: &str, name: &str, body: &str) {
        let dir = self.path.join("buckets").join(bucket).join("bucket");
        fs::create_dir_all(&dir).expect("create bucket dir");
        fs::write(dir.join(format!("{name}.json")), body).expect("write manifest");
    }

    fn lock_file(&self) -> PathBuf {
        self.path.join("cache").join("glue.lock")
    }
}

/// Seed `<root>/cache/self-update-check.json` so the startup notice answers
/// from disk instead of contacting whatever URL a test configured.
///
/// Written from Rust rather than PowerShell: `Set-Content -Encoding utf8` adds
/// a BOM on Windows, which would make the JSON unreadable.
fn seed_self_update_cache(root: &Path, latest: &str) {
    let dir = root.join("cache");
    fs::create_dir_all(&dir).unwrap();
    let checked_at = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let body = format!(r#"{{"checked_at":{checked_at},"latest":"{latest}"}}"#);
    fs::write(dir.join("self-update-check.json"), body).unwrap();
}

impl Drop for TempRoot {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.path);
    }
}

fn glue(root: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_glue"))
        // Keep the startup dependency check offline: an isolated root has no
        // helper tools, and bootstrapping them would reach the network.
        .env("GLUE_SKIP_BOOTSTRAP", "1")
        // Configuration normally follows the program's installation directory.
        // The test binary lives under `target\debug`, which may itself look
        // like an installation; pin the config file to the isolated root.
        .env("GLUE_CONFIG_DIR", root)
        .arg("--root")
        .arg(root)
        .args(args)
        .output()
        .expect("run glue binary")
}

/// Run `glue` with an isolated home directory, so the default data root lands
/// inside the temp tree instead of `~/.glue`, and PATH reads/writes go to a
/// file instead of the real user registry.
fn glue_isolated(home: &Path, args: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_glue"))
        .env("USERPROFILE", home)
        .env("HOME", home)
        .env("GLUE_PATH_FILE", home.join("user-path.txt"))
        .env("GLUE_CONFIG_DIR", home.join(".glue"))
        .env("GLUE_SKIP_BOOTSTRAP", "1")
        .args(args)
        .output()
        .expect("run glue binary")
}

fn stdout(output: &Output) -> String {
    String::from_utf8_lossy(&output.stdout).into_owned()
}

fn stderr(output: &Output) -> String {
    String::from_utf8_lossy(&output.stderr).into_owned()
}

const MANIFEST: &str = r#"{
  "version": "1.2.3",
  "description": "integration test package",
  "url": "https://example.com/foo.zip",
  "hash": "0000000000000000000000000000000000000000000000000000000000000000",
  "bin": "foo.exe"
}"#;

const SUBCOMMANDS: &[&str] = &[
    "install",
    "uninstall",
    "update",
    "list",
    "search",
    "doctor",
    "bucket",
    "cache",
    "config",
    "root",
    "hold",
    "unhold",
    "depends",
    "info",
    "reset",
    "completion",
    "path",
];

#[test]
fn version_prints_name_and_version() {
    let root = TempRoot::new("version");
    let out = glue(root.path(), &["--version"]);
    assert!(out.status.success(), "{}", stderr(&out));
    // Follows the workspace version: a bump must not need a test edit.
    assert!(
        stdout(&out).contains(&format!("glue {}", env!("CARGO_PKG_VERSION"))),
        "{}",
        stdout(&out)
    );
}

#[test]
fn help_lists_core_commands() {
    let root = TempRoot::new("help");
    let out = glue(root.path(), &["--help"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(text.contains("Scoop-compatible"), "{text}");
    for command in ["install", "uninstall", "update", "search", "doctor"] {
        assert!(text.contains(command), "help is missing {command}: {text}");
    }
}

#[test]
fn every_subcommand_has_help() {
    let root = TempRoot::new("subcommand-help");
    for command in SUBCOMMANDS {
        let out = glue(root.path(), &[command, "--help"]);
        assert_eq!(out.status.code(), Some(0), "{command} --help failed");
        assert!(
            stdout(&out).contains(command),
            "{command} --help does not mention {command}: {}",
            stdout(&out)
        );
    }
}

#[test]
fn install_help_describes_the_command() {
    let root = TempRoot::new("install-help");
    let out = glue(root.path(), &["install", "--help"]);
    assert!(out.status.success());
    assert!(
        stdout(&out).contains("Install a package"),
        "{}",
        stdout(&out)
    );
}

#[test]
fn unknown_command_exits_with_usage_code() {
    let root = TempRoot::new("usage");
    let out = glue(root.path(), &["definitely-not-a-command"]);
    assert_eq!(out.status.code(), Some(2), "{}", stderr(&out));
    assert!(
        stderr(&out).contains("unrecognized subcommand"),
        "{}",
        stderr(&out)
    );
}

#[test]
fn invalid_global_argument_exits_with_usage_code() {
    let root = TempRoot::new("invalid-arg");
    let out = glue(root.path(), &["--definitely-not-a-flag"]);
    assert_eq!(out.status.code(), Some(2), "{}", stderr(&out));
    assert!(stderr(&out).contains("error"), "{}", stderr(&out));
}

#[test]
fn install_without_packages_fails() {
    let root = TempRoot::new("install-empty");
    let out = glue(root.path(), &["install"]);
    assert_eq!(out.status.code(), Some(1));
    assert!(
        stderr(&out).contains("install requires at least one package"),
        "{}",
        stderr(&out)
    );
}

#[test]
fn list_on_empty_root_reports_no_packages() {
    let root = TempRoot::new("list-empty");
    let out = glue(root.path(), &["list"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(stdout(&out).contains("No packages installed"));
}

#[test]
fn list_detailed_and_all_on_empty_root() {
    let root = TempRoot::new("list-flags");
    for args in [&["list", "--detailed"][..], &["list", "--all"][..]] {
        let out = glue(root.path(), args);
        assert!(out.status.success(), "{args:?}: {}", stderr(&out));
        assert!(stdout(&out).contains("No packages installed"), "{args:?}");
    }
}

#[test]
fn search_finds_a_bucket_manifest() {
    let root = TempRoot::new("search");
    root.write_bucket_manifest("main", "foo", MANIFEST);

    let out = glue(root.path(), &["search", "foo"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(text.contains("1 package(s) found for 'foo':"), "{text}");
    assert!(text.contains("  main/foo"), "indented result row: {text}");
    assert!(text.contains("1.2.3"), "{text}");
    assert!(text.contains("integration test package"), "{text}");

    // A miss is still successful and reports the empty-result line.
    let none = glue(root.path(), &["search", "zzzz"]);
    assert!(none.status.success());
    assert!(
        stdout(&none).contains("no results for"),
        "{}",
        stdout(&none)
    );
}

#[test]
fn search_bucket_filter_is_applied() {
    let root = TempRoot::new("search-filter");
    root.write_bucket_manifest("main", "foo", MANIFEST);

    let hit = glue(root.path(), &["search", "foo", "--bucket", "main"]);
    assert!(hit.status.success());
    assert!(stdout(&hit).contains("main/foo"), "{}", stdout(&hit));

    let miss = glue(root.path(), &["search", "foo", "--bucket", "extras"]);
    assert!(miss.status.success());
    assert!(
        stdout(&miss).contains("no results for"),
        "{}",
        stdout(&miss)
    );
}

#[test]
fn info_on_missing_package_fails() {
    let root = TempRoot::new("info-missing");
    let out = glue(root.path(), &["info", "ghost"]);
    assert_eq!(out.status.code(), Some(1));
    let err = stderr(&out);
    assert!(err.contains("ghost"), "{err}");
    assert!(err.to_lowercase().contains("not installed"), "{err}");
}

#[test]
fn uninstall_on_missing_package_fails() {
    let root = TempRoot::new("uninstall-missing");
    let out = glue(root.path(), &["uninstall", "ghost"]);
    assert_eq!(out.status.code(), Some(1));
    assert!(
        stderr(&out).to_lowercase().contains("not installed"),
        "{}",
        stderr(&out)
    );
}

/// `pkg@version` deletes just that version directory; the package itself and
/// its other versions stay installed.
#[test]
fn uninstall_package_at_version_removes_only_that_version() {
    let root = TempRoot::new("uninstall-version");
    let pkg_dir = root.path().join("apps").join("demo");
    for version in ["1.0.0", "2.0.0"] {
        let dir = pkg_dir.join(version);
        fs::create_dir_all(&dir).expect("create version dir");
        fs::write(dir.join("tool.exe"), b"x").expect("write payload");
    }

    let out = glue(root.path(), &["uninstall", "demo@1.0.0"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(stdout(&out).contains("demo@1.0.0"), "{}", stdout(&out));
    assert!(!pkg_dir.join("1.0.0").exists(), "the named version is gone");
    assert!(pkg_dir.join("2.0.0").is_dir(), "the other version stays");
}

#[test]
fn uninstall_package_at_missing_version_fails() {
    let root = TempRoot::new("uninstall-version-missing");
    let dir = root.path().join("apps").join("demo").join("1.0.0");
    fs::create_dir_all(&dir).expect("create version dir");
    fs::write(dir.join("tool.exe"), b"x").expect("write payload");

    let out = glue(root.path(), &["uninstall", "demo@9.9.9"]);
    assert_eq!(out.status.code(), Some(1));
    let err = stderr(&out);
    assert!(err.contains("9.9.9"), "{err}");
    assert!(err.to_lowercase().contains("no installed version"), "{err}");
}

/// A failed install reports the error once: the progress callback prints the
/// `✗` line, and the command must not repeat the returned error on top of it.
/// The engine's context prefixes (`resolve package:`, `find manifest:`) are
/// stripped from what the user sees.
#[test]
fn install_missing_package_reports_one_clean_error() {
    let root = TempRoot::new("install-missing");
    root.write_bucket_manifest("main", "foo", MANIFEST);

    let out = glue(root.path(), &["install", "ghost"]);
    assert_eq!(out.status.code(), Some(1));
    let err = stderr(&out);
    assert_eq!(
        err.matches("manifest not found: ghost").count(),
        1,
        "exactly one failure line: {err}"
    );
    assert!(err.contains("x manifest not found: ghost"), "{err}");
    assert!(!err.contains("resolve package:"), "{err}");
    assert!(!err.contains("find manifest:"), "{err}");
    assert!(!err.contains("error:"), "{err}");
}

#[test]
fn doctor_reports_data_root_and_buckets() {
    let root = TempRoot::new("doctor");
    root.write_bucket_manifest("main", "foo", MANIFEST);

    let out = glue(root.path(), &["doctor"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(text.contains("workspace:"), "{text}");
    assert!(text.contains("buckets: main (1)"), "{text}");
    assert!(text.contains("helper tools:"), "{text}");
    assert!(text.contains("installed packages: 0"), "{text}");
    // Clean roots stay quiet: the check only speaks up when it has entries.
    assert!(!text.contains("missing dependencies"), "{text}");
}

/// An installed package whose `depends` is not installed is reported with the
/// command that installs the dependency. `doctor` reads the manifest copy in
/// `install.json`, so the fixture needs only the `apps/` tree.
#[test]
fn doctor_reports_missing_dependencies() {
    let root = TempRoot::new("doctor-missing-dep");
    let dir = root.path().join("apps").join("jhead").join("1.0.0");
    fs::create_dir_all(&dir).unwrap();
    let record = r#"{
  "version": "1.0.0",
  "bucket": "extras",
  "arch": "",
  "manifest": {
    "version": "1.0.0",
    "url": "https://example.com/jhead.zip",
    "hash": "0000",
    "depends": "extras/mozjpeg"
  }
}"#;
    fs::write(dir.join("install.json"), record).unwrap();

    let out = glue(root.path(), &["doctor"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(text.contains("missing dependencies: 1"), "{text}");
    assert!(
        text.contains("jhead needs extras/mozjpeg (run `glue install extras/mozjpeg`)"),
        "{text}"
    );
}

/// A clean root has no cached check, so `doctor` must not grow a self-update
/// line — and must not make a request to fill it in.
#[test]
fn doctor_is_silent_about_self_update_by_default() {
    let root = TempRoot::new("doctor-self-update-silent");
    let out = glue(root.path(), &["doctor"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(!stdout(&out).contains("self update:"), "{}", stdout(&out));
}

/// `doctor` reports a cached newer version without checking again.
#[test]
fn doctor_reports_a_cached_newer_version() {
    let root = TempRoot::new("doctor-self-update");
    seed_self_update_cache(root.path(), "99.0.0");

    let out = glue(root.path(), &["doctor"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(
        stdout(&out).contains("self update: 99.0.0 available"),
        "{}",
        stdout(&out)
    );
}

/// The startup dependency check is silent and offline once the helper tools
/// are installed: no "Preparing helper tools" line, no download.
#[test]
fn startup_check_is_silent_when_dependencies_are_present() {
    let root = TempRoot::new("preflight-ready");
    root.write_bucket_manifest("main", "foo", MANIFEST);
    for path in [
        "bin/7z.exe",
        "bin/7z.dll",
        "bin/innounp/innounp.exe",
        "bin/wix/dark.exe",
        "bin/wix/wix.dll",
    ] {
        let path = root.path().join(path);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, b"stub").unwrap();
    }

    let out = Command::new(env!("CARGO_BIN_EXE_glue"))
        .arg("--root")
        .arg(root.path())
        .arg("doctor")
        .output()
        .expect("run glue binary");
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(
        !stdout(&out).contains("Preparing helper tools"),
        "{}",
        stdout(&out)
    );
    assert!(!stderr(&out).contains("Bootstrapping"), "{}", stderr(&out));
}

#[test]
fn bucket_list_on_empty_root() {
    let root = TempRoot::new("bucket-empty");
    let out = glue(root.path(), &["bucket", "list"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(
        stdout(&out).contains("No buckets installed"),
        "{}",
        stdout(&out)
    );
}

#[test]
fn cache_list_on_empty_root() {
    let root = TempRoot::new("cache-empty");
    let out = glue(root.path(), &["cache", "list"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(
        stdout(&out).contains("No packages in cache index"),
        "{}",
        stdout(&out)
    );
}

#[test]
fn config_list_reports_defaults() {
    let root = TempRoot::new("config-list");
    let out = glue(root.path(), &["config", "list"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(text.contains("Configuration:"), "{text}");
    assert!(
        text.contains("parallel_download = true (default)"),
        "{text}"
    );
}

#[test]
fn config_set_get_unset_roundtrip() {
    let root = TempRoot::new("config-roundtrip");
    let set = glue(root.path(), &["config", "set", "color", "false"]);
    assert!(set.status.success(), "{}", stderr(&set));
    assert!(
        stdout(&set).contains("Set color = false"),
        "{}",
        stdout(&set)
    );
    assert!(root.path().join("config.json").is_file());

    let get = glue(root.path(), &["config", "get", "color"]);
    assert!(get.status.success());
    assert_eq!(stdout(&get).trim(), "false");

    let unset = glue(root.path(), &["config", "unset", "color"]);
    assert!(unset.status.success(), "{}", stderr(&unset));
    assert!(stdout(&unset).contains("Unset color"), "{}", stdout(&unset));
    let get_default = glue(root.path(), &["config", "get", "color"]);
    assert!(
        stdout(&get_default).contains("true"),
        "{}",
        stdout(&get_default)
    );
}

#[test]
fn config_self_update_check_url_roundtrip() {
    let root = TempRoot::new("config-self-update");
    // The startup check must not reach for the endpoint once the key is set:
    // a fresh cache with the running version answers from disk, so the test
    // stays offline and no notice is produced.
    seed_self_update_cache(root.path(), env!("CARGO_PKG_VERSION"));

    let get = glue(root.path(), &["config", "get", "self_update_check_url"]);
    assert!(get.status.success(), "{}", stderr(&get));
    assert_eq!(stdout(&get).trim_end(), "(not set, update checks disabled)");

    let url = "https://example.com/gluestick-latest.json";
    let set = glue(
        root.path(),
        &["config", "set", "self_update_check_url", url],
    );
    assert!(set.status.success(), "{}", stderr(&set));
    assert_eq!(
        stdout(&set).trim_end(),
        format!("Set self_update_check_url = {url}")
    );

    let get = glue(root.path(), &["config", "get", "self_update_check_url"]);
    assert!(get.status.success(), "{}", stderr(&get));
    assert_eq!(stdout(&get).trim_end(), url);

    let list = glue(root.path(), &["config", "list"]);
    assert!(list.status.success(), "{}", stderr(&list));
    assert!(
        stdout(&list).contains(&format!("self_update_check_url = {url}")),
        "{}",
        stdout(&list)
    );

    let unset = glue(root.path(), &["config", "unset", "self_update_check_url"]);
    assert!(unset.status.success(), "{}", stderr(&unset));
    assert_eq!(stdout(&unset).trim_end(), "Unset self_update_check_url");

    let get = glue(root.path(), &["config", "get", "self_update_check_url"]);
    assert!(get.status.success(), "{}", stderr(&get));
    assert_eq!(stdout(&get).trim_end(), "(not set, update checks disabled)");

    // A value that is not an http(s) URL is rejected and changes nothing.
    let bad = glue(
        root.path(),
        &[
            "config",
            "set",
            "self_update_check_url",
            "ftp://example.com",
        ],
    );
    assert_eq!(bad.status.code(), Some(1));
    assert!(
        stderr(&bad).contains("error: self_update_check_url must be an http(s) URL"),
        "{}",
        stderr(&bad)
    );
}

/// The one-line notice belongs on stderr, so piping a command's stdout stays
/// clean. A fresh cache keeps the check offline.
#[test]
fn self_update_notice_goes_to_stderr_on_success() {
    let root = TempRoot::new("self-update-notice");
    fs::write(
        root.path().join("config.json"),
        r#"{"self_update_check_url":"https://example.com/latest.json"}"#,
    )
    .unwrap();
    seed_self_update_cache(root.path(), "99.0.0");

    let out = glue(root.path(), &["config", "get", "color"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(
        stderr(&out).contains(&format!(
            "note: Gluestick 99.0.0 is available (you have {}); run a newer installer to upgrade",
            env!("CARGO_PKG_VERSION")
        )),
        "{}",
        stderr(&out)
    );
    assert!(!stdout(&out).contains("note:"), "{}", stdout(&out));
}

#[test]
fn config_set_does_not_claim_success_before_writing() {
    // Configuration lives in the program directory now, not the workspace, so
    // an absent `--root` no longer fails the write. Point `GLUE_CONFIG_DIR` at
    // a path a regular file blocks instead: the write must fail — and the
    // command must not print the "Set …" success line first.
    let root = TempRoot::new("config-missing-root");
    let blocker = root.path().join("blocker");
    fs::write(&blocker, b"file, not a directory").unwrap();
    let out = Command::new(env!("CARGO_BIN_EXE_glue"))
        .env("GLUE_SKIP_BOOTSTRAP", "1")
        .env("GLUE_CONFIG_DIR", blocker.join("cfg"))
        .arg("--root")
        .arg(root.path())
        .args(["config", "set", "color", "false"])
        .output()
        .expect("run glue binary");
    assert_eq!(out.status.code(), Some(1));
    assert!(!stdout(&out).contains("Set color"), "{}", stdout(&out));
    assert!(
        stderr(&out).to_lowercase().contains("error"),
        "{}",
        stderr(&out)
    );
}

#[test]
fn path_show_points_at_the_root_shims_dir() {
    let root = TempRoot::new("path-show");
    let out = glue(root.path(), &["path", "show"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(stdout(&out).trim().ends_with("shims"), "{}", stdout(&out));
}

#[test]
fn completion_scripts_for_all_shells() {
    let root = TempRoot::new("completion");
    let cases = [
        ("bash", "#!/usr/bin/env bash"),
        ("zsh", "#compdef glue"),
        ("fish", "complete -c glue -f"),
        ("powershell", "Register-ArgumentCompleter"),
    ];
    for (shell, marker) in cases {
        let out = glue(root.path(), &["completion", shell]);
        assert!(out.status.success(), "{shell}: {}", stderr(&out));
        let text = stdout(&out);
        assert!(text.contains(marker), "{shell}: {text}");
        // The script defers to the binary, so bucket updates show up in the
        // next <TAB> without regenerating anything.
        assert!(text.contains("__complete"), "{shell}: {text}");
    }
}

#[test]
fn completion_with_invalid_shell_exits_with_usage_code() {
    let root = TempRoot::new("completion-invalid");
    let out = glue(root.path(), &["completion", "invalid"]);
    assert_eq!(out.status.code(), Some(2), "{}", stderr(&out));
    assert!(stderr(&out).contains("invalid value"), "{}", stderr(&out));
}

// ---------------------------------------------------------------------------
// __complete (the hidden helper the completion scripts call back)
// ---------------------------------------------------------------------------

/// One completion request: candidates only, one per line, no blank line (that
/// line would become an empty completion entry in the shell).
fn complete(root: &Path, args: &[&str]) -> Vec<String> {
    let out = glue(root, args);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(!text.ends_with("\n\n"), "blank line in: {text:?}");
    text.lines().map(str::to_string).collect()
}

#[test]
fn complete_lists_top_level_commands_and_hides_internal_ones() {
    let root = TempRoot::new("complete-commands");
    let names = complete(root.path(), &["__complete", "1", "glue", ""]);
    for expected in ["install", "uninstall", "search", "bucket", "completion"] {
        assert!(names.contains(&expected.to_string()), "{names:?}");
    }
    for hidden in ["__complete", "shim-run", "tee"] {
        assert!(
            !names.contains(&hidden.to_string()),
            "{hidden} in {names:?}"
        );
    }
}

#[test]
fn complete_lists_nested_subcommands() {
    let root = TempRoot::new("complete-nested");
    let names = complete(root.path(), &["__complete", "2", "glue", "bucket", ""]);
    for expected in ["add", "remove", "update", "deepen"] {
        assert!(names.contains(&expected.to_string()), "{names:?}");
    }
}

#[test]
fn complete_offers_bucket_packages_by_name_and_by_bucket() {
    let root = TempRoot::new("complete-packages");
    root.write_bucket_manifest("main", "7zip", MANIFEST);
    root.write_bucket_manifest("main", "git", MANIFEST);
    root.write_bucket_manifest("extras", "vscode", MANIFEST);

    assert_eq!(
        complete(root.path(), &["__complete", "2", "glue", "install", "7z"]),
        vec!["7zip".to_string()]
    );
    assert_eq!(
        complete(
            root.path(),
            &["__complete", "2", "glue", "install", "extras/"]
        ),
        vec!["extras/vscode".to_string()]
    );
}

#[test]
fn complete_offers_flag_names_and_known_flag_values() {
    let root = TempRoot::new("complete-flags");
    let flags = complete(root.path(), &["__complete", "2", "glue", "install", "--"]);
    // `--help` comes from clap itself: it must be materialized before listing.
    for expected in ["--arch", "--version", "--force", "--help"] {
        assert!(flags.contains(&expected.to_string()), "{flags:?}");
    }
    assert_eq!(
        complete(
            root.path(),
            &["__complete", "3", "glue", "install", "--arch", ""]
        ),
        vec![
            "32bit".to_string(),
            "64bit".to_string(),
            "arm64".to_string()
        ]
    );
}

#[test]
fn complete_offers_installed_packages_and_their_versions() {
    let root = TempRoot::new("complete-installed");
    for version in ["1.0.0", "0.9.0"] {
        let dir = root.path().join("apps").join("foo").join(version);
        fs::create_dir_all(&dir).unwrap();
        fs::write(
            dir.join("install.json"),
            format!(r#"{{"version":"{version}","bucket":"main"}}"#),
        )
        .unwrap();
    }

    assert_eq!(
        complete(root.path(), &["__complete", "2", "glue", "uninstall", ""]),
        vec!["foo".to_string()]
    );
    assert_eq!(
        complete(root.path(), &["__complete", "2", "glue", "reset", "foo@"]),
        vec!["foo@0.9.0".to_string(), "foo@1.0.0".to_string()]
    );
}

// ---------------------------------------------------------------------------
// root
// ---------------------------------------------------------------------------

#[test]
fn root_help_lists_the_subcommands() {
    let home = TempRoot::new("root-help");
    let out = glue_isolated(home.path(), &["root", "--help"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    for expected in ["show", "set", "reset"] {
        assert!(text.contains(expected), "{expected} missing from: {text}");
    }
}

#[test]
fn root_show_prints_the_effective_root_and_its_source() {
    let home = TempRoot::new("root-show");
    let out = glue_isolated(home.path(), &["root", "show"]);
    assert!(out.status.success(), "{}", stderr(&out));
    let text = stdout(&out);
    assert!(text.contains("workspace:"), "{text}");
    assert!(text.contains("source:"), "{text}");
}

#[test]
fn doctor_reports_where_the_data_root_came_from() {
    let home = TempRoot::new("root-doctor");
    let out = glue_isolated(home.path(), &["doctor"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(stdout(&out).contains("source:"), "{}", stdout(&out));
}

/// `--root` outranks every stored rule, so `doctor` must name it.
#[test]
fn doctor_reports_the_root_override() {
    let root = TempRoot::new("root-doctor-override");
    let out = glue(root.path(), &["doctor"]);
    assert!(out.status.success(), "{}", stderr(&out));
    assert!(
        stdout(&out).contains("source:    --root"),
        "{}",
        stdout(&out)
    );
}

/// Read-only commands must never take the cross-process mutation lock.
#[test]
fn read_only_commands_do_not_create_the_mutation_lock() {
    let root = TempRoot::new("read-only-lock");
    root.write_bucket_manifest("main", "foo", MANIFEST);

    for args in [
        &["list"][..],
        &["list", "--detailed"][..],
        &["search", "foo"][..],
        &["doctor"][..],
        &["bucket", "list"][..],
        &["cache", "list"][..],
        &["config", "list"][..],
        &["path", "show"][..],
    ] {
        let out = glue(root.path(), args);
        assert!(out.status.success(), "{args:?}: {}", stderr(&out));
        assert!(!root.lock_file().exists(), "{args:?} created a lock file");
    }
}

/// Template display text lives in the bundle, so the CLI renders the copy for
/// the active language instead of hard-coding either translation.
#[test]
fn template_list_localizes_titles() {
    let root = TempRoot::new("template-lang");

    let run = |lang: &str| {
        Command::new(env!("CARGO_BIN_EXE_glue"))
            .env("GLUE_SKIP_BOOTSTRAP", "1")
            .env("GLUE_CONFIG_DIR", root.path())
            .env("GLUE_LANG", lang)
            .arg("--root")
            .arg(root.path())
            .args(["template", "list"])
            .output()
            .expect("run glue binary")
    };

    let english = stdout(&run("en"));
    assert!(english.contains("Vibe Coding"), "{english}");
    assert!(english.contains("AI-assisted coding"), "{english}");

    let chinese = stdout(&run("zh"));
    assert!(chinese.contains("氛围编程"), "{chinese}");
    assert!(chinese.contains("装机必备"), "{chinese}");
}
