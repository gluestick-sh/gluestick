//! End-to-end checks against the compiled stub: each test lays out a throwaway
//! `GLUE_ROOT` with one `shims-meta/<name>.json` entry, copies `shim.exe` to
//! `<name>.exe`, and runs it. That exercises the real contract the engine
//! writes — argv[0]-derived name, config lookup, default arguments, env
//! injection, and exit-code propagation.
//!
//! `cmd.exe` stands in for an installed package executable; the tests are
//! Windows-only because the stub's launch path (`CreateProcessW`) and its
//! MAX_PATH guard are.

#![cfg(windows)]

use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};

/// The stub built for this test run.
const STUB: &str = env!("CARGO_BIN_EXE_shim");

struct Fixture {
    root: PathBuf,
}

impl Fixture {
    fn new(label: &str) -> Self {
        let root = std::env::temp_dir().join(format!("glue-shim-{label}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&root);
        fs::create_dir_all(root.join("shims-meta")).expect("create shims-meta");
        Self { root }
    }

    /// Writes `shims-meta/<shim_name>.json` and returns the copy of the stub
    /// that must be launched under that name.
    fn shim(&self, shim_name: &str, config: &str) -> PathBuf {
        fs::write(
            self.root
                .join("shims-meta")
                .join(format!("{shim_name}.json")),
            config,
        )
        .expect("write shim config");
        let exe = self.root.join(format!("{shim_name}.exe"));
        fs::copy(STUB, &exe).expect("copy stub");
        exe
    }

    fn run(&self, exe: &Path, args: &[&str]) -> Output {
        self.run_with_env(exe, args, &[])
    }

    fn run_with_env(&self, exe: &Path, args: &[&str], env: &[(&str, &str)]) -> Output {
        let mut cmd = Command::new(exe);
        cmd.args(args)
            .env("GLUE_ROOT", &self.root)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        for (key, value) in env {
            cmd.env(key, value);
        }
        cmd.output().expect("run shim")
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.root);
    }
}

fn stdout_of(output: &Output) -> String {
    String::from_utf8_lossy(&output.stdout).trim().to_string()
}

fn stderr_of(output: &Output) -> String {
    String::from_utf8_lossy(&output.stderr).trim().to_string()
}

fn json_escape(value: &str) -> String {
    let mut escaped = String::with_capacity(value.len());
    for ch in value.chars() {
        match ch {
            '\\' => escaped.push_str("\\\\"),
            '"' => escaped.push_str("\\\""),
            other => escaped.push(other),
        }
    }
    escaped
}

/// The interpreter the stub should launch: `cmd.exe` from `COMSPEC`, so the
/// tests do not depend on `cmd.exe` being reachable through `PATH`.
fn cmd() -> String {
    std::env::var("COMSPEC").unwrap_or_else(|_| "cmd.exe".to_string())
}

fn config(name: &str, args_json: &str, env_json: &str) -> String {
    let command = json_escape(&cmd());
    format!(
        r#"{{"name":"{name}","command":"{command}","args":{args_json},"env":{env_json},"path":"{command}"}}"#
    )
}

#[test]
fn propagates_the_target_exit_code() {
    let fixture = Fixture::new("exit-code");
    let exe = fixture.shim("seven", &config("seven", r#"["/C","exit","7"]"#, "{}"));

    let output = fixture.run(&exe, &[]);

    assert_eq!(
        output.status.code(),
        Some(7),
        "stderr: {}",
        stderr_of(&output)
    );
}

#[test]
fn prepends_default_args_before_the_callers_args() {
    let fixture = Fixture::new("args");
    let exe = fixture.shim("echo", &config("echo", r#"["/C","echo","fixed"]"#, "{}"));

    let output = fixture.run(&exe, &["tail"]);

    assert_eq!(
        output.status.code(),
        Some(0),
        "stderr: {}",
        stderr_of(&output)
    );
    assert_eq!(stdout_of(&output), "fixed tail");
}

#[test]
fn config_env_overrides_the_inherited_value() {
    let fixture = Fixture::new("env");
    let exe = fixture.shim(
        "env",
        &config(
            "env",
            r#"["/C","echo","%GLUE_SHIM_TEST%"]"#,
            r#"{"GLUE_SHIM_TEST":"from-config"}"#,
        ),
    );

    let output = fixture.run_with_env(&exe, &[], &[("GLUE_SHIM_TEST", "from-parent")]);

    assert_eq!(
        output.status.code(),
        Some(0),
        "stderr: {}",
        stderr_of(&output)
    );
    assert_eq!(stdout_of(&output), "from-config");
}

#[test]
fn a_missing_config_fails_with_exit_1() {
    let fixture = Fixture::new("missing-config");
    // Copy the stub without writing a matching `shims-meta/orphan.json`.
    let exe = fixture.root.join("orphan.exe");
    fs::copy(STUB, &exe).expect("copy stub");

    let output = fixture.run(&exe, &[]);

    assert_eq!(output.status.code(), Some(1));
    assert!(
        stderr_of(&output).contains("shim config not found"),
        "stderr: {}",
        stderr_of(&output)
    );
}

#[test]
fn a_malformed_config_fails_with_exit_1() {
    let fixture = Fixture::new("malformed");
    let exe = fixture.shim("broken", "{ not json");

    let output = fixture.run(&exe, &[]);

    assert_eq!(output.status.code(), Some(1));
    assert!(
        stderr_of(&output).contains("invalid shim config"),
        "stderr: {}",
        stderr_of(&output)
    );
}
