//! Git runner.
//!
//! Thin wrapper around the `git` executable: clone a bucket repository and
//! pull updates. Uses [`crate::procutil::hide_window`] so Windows calls do not
//! flash console windows.
//!
//! `git` is a runtime dependency, not a crate one, so a machine can be without
//! it entirely. Every entry point therefore takes the data root and resolves
//! its executable through [`executable`]: a MinGit bootstrapped by
//! [`crate::bootstrap`] under `<root>/bin/git` wins over `PATH`, so a fresh
//! machine becomes self-sufficient after the first bucket operation instead of
//! asking the user to install git by hand.
//!
//! Every invocation is bounded: interactive credential prompts are disabled
//! (`GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=Never`, stdin closed) and a
//! wall-clock timeout kills a hung network operation instead of letting it
//! block the command forever.

use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Output, Stdio};
use std::time::{Duration, Instant};

use thiserror::Error;

/// Default ceiling for a git invocation. Generous enough for a first bucket
/// clone on a slow link while still bounding a hung network or credential
/// prompt. Override with `GLUE_GIT_TIMEOUT_SECS`.
const DEFAULT_GIT_TIMEOUT: Duration = Duration::from_secs(600);
const POLL_INTERVAL: Duration = Duration::from_millis(50);

#[derive(Debug, Error)]
pub enum GitError {
    #[error("git executable not found under the workspace or on PATH")]
    NotFound,
    #[error("git {0} failed: {1}")]
    Failed(String, String),
    #[error(
        "git {command} timed out after {seconds}s (network hang or an interactive credential prompt)"
    )]
    Timeout { command: String, seconds: u64 },
}

pub type Result<T> = std::result::Result<T, GitError>;

/// `<root>`-relative location of a bootstrapped MinGit launcher.
///
/// Kept in step with the MinGit entry in [`crate::bootstrap`], which unpacks
/// the archive into `<root>/bin/git`.
pub const MINIGIT_EXE_RELATIVE: &str = "bin/git/cmd/git.exe";

/// The `git` executable to run for the data root `root`.
///
/// A MinGit bootstrapped under `<root>/bin/git` wins over `PATH`: it is the
/// one the engine installed and knows how to find, and preferring it keeps a
/// machine without a system git working. Falls back to a bare `git`, which the
/// OS resolves through `PATH` (and which surfaces as [`GitError::NotFound`]
/// when there is none).
pub fn executable(root: &Path) -> PathBuf {
    let bootstrapped = root.join(MINIGIT_EXE_RELATIVE);
    if bootstrapped.is_file() {
        bootstrapped
    } else {
        PathBuf::from("git")
    }
}

fn git_timeout() -> Duration {
    std::env::var("GLUE_GIT_TIMEOUT_SECS")
        .ok()
        .and_then(|raw| raw.trim().parse::<u64>().ok())
        .filter(|secs| *secs > 0)
        .map(Duration::from_secs)
        .unwrap_or(DEFAULT_GIT_TIMEOUT)
}

/// Run a git command, capturing stdout/stderr. Returns the trimmed stdout on
/// success and the combined output on failure.
fn run_git(exe: &Path, args: &[&str], cwd: Option<&Path>) -> Result<String> {
    run_git_with_timeout(exe, args, cwd, git_timeout())
}

/// Run a network git command, retrying transport failures.
///
/// GitHub behind schannel drops TLS sessions with "server closed abruptly
/// (missing close_notify)" often enough that a second try is the difference
/// between a red bucket row and a successful check. Only transport chatter is
/// retried: a rejected non-fast-forward, a missing repository or a bad URL
/// fails identically every time, and a timeout already waited out the whole
/// ceiling, so retrying it would just stall the caller twice as long.
fn run_git_retrying(exe: &Path, args: &[&str], cwd: Option<&Path>) -> Result<String> {
    /// One retry plus the first attempt. Each failed attempt must have already
    /// failed fast (see [`is_transient_git_failure`]).
    const ATTEMPTS: u32 = 3;
    for attempt in 1..=ATTEMPTS {
        match run_git(exe, args, cwd) {
            Ok(out) => return Ok(out),
            Err(err) if attempt < ATTEMPTS && is_transient_git_failure(&err) => {
                std::thread::sleep(Duration::from_millis(400 * u64::from(attempt)));
            }
            Err(err) => return Err(err),
        }
    }
    unreachable!("the loop returns on the last attempt")
}

/// Whether a git failure looks like the network gave up mid-transfer, rather
/// than like a repository or usage problem a retry cannot fix.
fn is_transient_git_failure(err: &GitError) -> bool {
    // Lower-case needles, matched against the lower-cased stderr. `Timeout` and
    // `NotFound` are deliberately absent: one already burned the full ceiling,
    // the other will not appear on a retry.
    const NEEDLES: &[&str] = &[
        "server closed abruptly",
        "missing close_notify",
        "connection reset",
        "connection was reset",
        "connection timed out",
        "could not resolve host",
        "temporary failure in name resolution",
        "network is unreachable",
        "failed to connect",
        "couldn't connect to server",
        "remote end closed connection",
        "the remote end hung up unexpectedly",
        "rpc failed",
        "early eof",
        "unexpected disconnect",
    ];
    let GitError::Failed(_, detail) = err else {
        return false;
    };
    let detail = detail.to_ascii_lowercase();
    NEEDLES.iter().any(|needle| detail.contains(needle))
}

fn run_git_with_timeout(
    exe: &Path,
    args: &[&str],
    cwd: Option<&Path>,
    timeout: Duration,
) -> Result<String> {
    let description = format!("git {}", args.join(" "));
    let mut cmd = Command::new(exe);
    cmd.args(args)
        // A bucket operation must fail with a clear error instead of blocking
        // on an interactive username/password prompt.
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GCM_INTERACTIVE", "Never")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(dir) = cwd {
        cmd.current_dir(dir);
    }
    crate::procutil::hide_window(&mut cmd);

    let output = run_with_timeout(cmd, description, timeout)?;

    if output.status.success() {
        Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
    } else {
        let mut combined = String::from_utf8_lossy(&output.stdout).into_owned();
        combined.push_str(&String::from_utf8_lossy(&output.stderr));
        Err(GitError::Failed(
            args.join(" "),
            combined.trim().to_string(),
        ))
    }
}

/// Terminate the child and everything it spawned.
///
/// `Child::kill` only terminates the direct process: a git wrapper may have
/// transport children (`git-remote-https`, …) that keep the stdout/stderr
/// pipes open, so joining the reader threads would block until they finish.
/// `taskkill /T` tears down the whole tree and lets the pipes close promptly.
#[cfg(windows)]
fn kill_tree(child: &mut std::process::Child) {
    let pid = child.id().to_string();
    let mut taskkill = Command::new("taskkill");
    taskkill
        .args(["/PID", &pid, "/T", "/F"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    crate::procutil::hide_window(&mut taskkill);
    let _ = taskkill.status();
    let _ = child.kill();
    let _ = child.wait();
}

#[cfg(not(windows))]
fn kill_tree(child: &mut std::process::Child) {
    let _ = child.kill();
    let _ = child.wait();
}

/// Spawn `cmd`, drain both pipes, and kill the child when `timeout` elapses.
fn run_with_timeout(cmd: Command, description: String, timeout: Duration) -> Result<Output> {
    run_with_timeout_input(cmd, description, timeout, None)
}

/// [`run_with_timeout`] with `input` written to the child's stdin.
///
/// `cat-file --batch` takes its work list on stdin, so the batch reader below
/// needs this; every other caller passes `None`. The writer runs on its own
/// thread (a child that exits early would otherwise leave us blocked on a
/// full pipe) and is never joined — dropping the handle detaches it.
fn run_with_timeout_input(
    mut cmd: Command,
    description: String,
    timeout: Duration,
    input: Option<Vec<u8>>,
) -> Result<Output> {
    if input.is_some() {
        cmd.stdin(Stdio::piped());
    }
    let mut child = cmd.spawn().map_err(|err| {
        if err.kind() == std::io::ErrorKind::NotFound {
            GitError::NotFound
        } else {
            GitError::Failed(description.clone(), err.to_string())
        }
    })?;

    if let Some(bytes) = input {
        if let Some(mut pipe) = child.stdin.take() {
            std::thread::spawn(move || {
                let _ = pipe.write_all(&bytes);
            });
        }
    }

    // Drain stdout/stderr on separate threads: a child that fills either pipe
    // would otherwise block before exiting and never hit `try_wait`.
    let stdout_reader = child.stdout.take().map(|mut pipe| {
        std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = pipe.read_to_end(&mut buf);
            buf
        })
    });
    let stderr_reader = child.stderr.take().map(|mut pipe| {
        std::thread::spawn(move || {
            let mut buf = Vec::new();
            let _ = pipe.read_to_end(&mut buf);
            buf
        })
    });

    let deadline = Instant::now() + timeout;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) => {
                if Instant::now() >= deadline {
                    kill_tree(&mut child);
                    let _ = stdout_reader.map(|reader| reader.join());
                    let _ = stderr_reader.map(|reader| reader.join());
                    return Err(GitError::Timeout {
                        command: description,
                        seconds: timeout.as_secs(),
                    });
                }
                std::thread::sleep(POLL_INTERVAL);
            }
            Err(err) => {
                kill_tree(&mut child);
                let _ = stdout_reader.map(|reader| reader.join());
                let _ = stderr_reader.map(|reader| reader.join());
                return Err(GitError::Failed(description, err.to_string()));
            }
        }
    };

    let stdout = stdout_reader
        .and_then(|reader| reader.join().ok())
        .unwrap_or_default();
    let stderr = stderr_reader
        .and_then(|reader| reader.join().ok())
        .unwrap_or_default();
    Ok(Output {
        status,
        stdout,
        stderr,
    })
}

/// Shallow-clone `url` into `dest`. The parent directory is created if needed.
pub fn clone(root: &Path, url: &str, dest: &Path) -> Result<()> {
    if let Some(parent) = dest.parent() {
        std::fs::create_dir_all(parent)
            .map_err(|e| GitError::Failed(format!("mkdir {}", parent.display()), e.to_string()))?;
    }
    let exe = executable(root);
    let dest_str = dest.to_string_lossy().into_owned();
    run_git(&exe, &["clone", "--depth", "1", url, &dest_str], None)?;
    Ok(())
}

/// Fast-forward pull in an existing repository at `repo_dir`.
pub fn pull(root: &Path, repo_dir: &Path) -> Result<String> {
    let exe = executable(root);
    run_git_retrying(&exe, &["pull", "--ff-only"], Some(repo_dir))
}

/// Current HEAD commit hash (short) of a repository, or "" when unavailable.
pub fn rev_parse_head(root: &Path, repo_dir: &Path) -> String {
    let exe = executable(root);
    run_git(&exe, &["rev-parse", "--short", "HEAD"], Some(repo_dir)).unwrap_or_default()
}

/// Fetch from the named remote (default `origin`) to refresh upstream refs.
pub fn fetch(root: &Path, repo_dir: &Path) -> Result<()> {
    let exe = executable(root);
    run_git_retrying(&exe, &["fetch", "--quiet", "origin"], Some(repo_dir)).map(|_| ())
}

/// Resolve a git revision spec (e.g. `HEAD`, `@{u}`, `@{upstream}`) to its
/// short commit hash, returning "" when the ref does not exist or git fails.
pub fn rev_parse(root: &Path, repo_dir: &Path, spec: &str) -> String {
    let exe = executable(root);
    run_git(&exe, &["rev-parse", "--short", spec], Some(repo_dir)).unwrap_or_default()
}

/// Whether `repo_dir` is a shallow clone.
///
/// A shallow clone is the norm for buckets ([`clone`] passes `--depth 1`), and
/// it is what decides between "this version never existed" and "its manifest
/// was never fetched" when a `pkg@version` pin cannot be resolved.
///
/// Errors read as "not shallow": every caller uses this to pick an error
/// message, and blaming a clone's depth when git itself failed would point the
/// user at the wrong fix.
pub fn is_shallow(root: &Path, repo_dir: &Path) -> bool {
    let exe = executable(root);
    match run_git(
        &exe,
        &["rev-parse", "--is-shallow-repository"],
        Some(repo_dir),
    ) {
        Ok(out) => out.trim() == "true",
        Err(_) => false,
    }
}

/// Commit hashes that touched `path` (repo-relative, `/`-separated), newest
/// first, at most `limit` of them.
///
/// `--no-merges` keeps the list to the commits that actually changed the file:
/// a merge commit carries the same blob as one of its parents, so it would only
/// add duplicate work to the batch read that follows.
pub fn log_path(root: &Path, repo_dir: &Path, path: &str, limit: usize) -> Result<Vec<String>> {
    let exe = executable(root);
    let limit_arg = format!("-n{limit}");
    let out = run_git(
        &exe,
        &["log", "--no-merges", "--format=%H", &limit_arg, "--", path],
        Some(repo_dir),
    )?;
    Ok(out
        .lines()
        .map(str::trim)
        .filter(|line| !line.is_empty())
        .map(str::to_string)
        .collect())
}

/// Contents of `<rev>:<path>` for every spec, read through **one**
/// `git cat-file --batch` process.
///
/// One process instead of one `git show` per revision: the history lookup
/// walks every revision a manifest ever had, and a process spawn per revision
/// dominates the cost on Windows. `None` marks a spec git cannot resolve.
pub fn cat_file_batch(
    root: &Path,
    repo_dir: &Path,
    specs: &[String],
) -> Result<Vec<Option<Vec<u8>>>> {
    if specs.is_empty() {
        return Ok(Vec::new());
    }
    let exe = executable(root);
    let mut cmd = Command::new(exe);
    cmd.args(["cat-file", "--batch"])
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GCM_INTERACTIVE", "Never")
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .current_dir(repo_dir);
    crate::procutil::hide_window(&mut cmd);

    let mut input = String::new();
    for spec in specs {
        input.push_str(spec);
        input.push('\n');
    }

    let output = run_with_timeout_input(
        cmd,
        "git cat-file --batch".to_string(),
        git_timeout(),
        Some(input.into_bytes()),
    )?;
    if !output.status.success() {
        return Err(GitError::Failed(
            "cat-file --batch".to_string(),
            String::from_utf8_lossy(&output.stderr).trim().to_string(),
        ));
    }
    Ok(parse_cat_file_batch(&output.stdout, specs.len()))
}

/// Parse `cat-file --batch` output.
///
/// Each resolved object is `<oid> <type> <size>\n<size bytes>\n`; an
/// unresolvable one is a single `<spec> missing\n` line with no body. Objects
/// are consumed strictly in order so a malformed stream degrades to trailing
/// `None`s instead of silently misaligning every later object.
fn parse_cat_file_batch(mut buf: &[u8], count: usize) -> Vec<Option<Vec<u8>>> {
    let mut out = Vec::with_capacity(count);
    for _ in 0..count {
        let Some(nl) = buf.iter().position(|byte| *byte == b'\n') else {
            out.push(None);
            continue;
        };
        let header = String::from_utf8_lossy(&buf[..nl]).into_owned();
        buf = &buf[(nl + 1).min(buf.len())..];
        let size = header
            .split(' ')
            .nth(2)
            .and_then(|raw| raw.parse::<usize>().ok());
        let Some(size) = size else {
            // `<spec> missing`, `<spec> ambiguous`, …: no body follows.
            out.push(None);
            continue;
        };
        if buf.len() < size {
            out.push(None);
            buf = &buf[buf.len()..];
            continue;
        }
        out.push(Some(buf[..size].to_vec()));
        // The body is followed by one newline, which is not part of it.
        buf = &buf[(size + 1).min(buf.len())..];
    }
    out
}

/// Fetch the full history into a shallow clone (`git fetch --unshallow`).
///
/// The one-time cost that makes older `pkg@version` pins resolvable; the only
/// caller is `glue bucket deepen`, never an install.
pub fn unshallow(root: &Path, repo_dir: &Path) -> Result<()> {
    let exe = executable(root);
    run_git_retrying(
        &exe,
        &["fetch", "--unshallow", "--quiet", "origin"],
        Some(repo_dir),
    )
    .map(|_| ())
}

/// Name the OS resolves for the bare `git` that [`executable`] falls back to.
#[cfg(windows)]
const GIT_FILE_NAME: &str = "git.exe";
#[cfg(not(windows))]
const GIT_FILE_NAME: &str = "git";

/// Whether a `git` is reachable on `PATH`, without running one.
///
/// A filename scan rather than a `git --version` run: this is what decides
/// whether the ~40 MB MinGit download is worth starting, and it sits on a path
/// every command takes, where spawning git to ask the question costs more than
/// the answer is worth. Scanning for the same filename the OS resolves for
/// [`executable`]'s bare fallback keeps the two agreeing on what "on PATH"
/// means — a `git` that is found but does not work fails later, at the
/// invocation that actually needed it.
pub fn on_path() -> bool {
    let Some(path) = std::env::var_os("PATH") else {
        return false;
    };
    // An empty entry means the working directory on Windows, which is not what
    // "on PATH" should mean here; the extractor's own lookup skips it too.
    std::env::split_paths(&path)
        .filter(|dir| !dir.as_os_str().is_empty())
        .any(|dir| dir.join(GIT_FILE_NAME).is_file())
}

/// Whether a usable `git` actually runs for `root` — either a bootstrapped
/// MinGit or one on `PATH`.
///
/// Unlike [`on_path`] this starts a process, so it is for callers that are
/// about to run git anyway (or need to tell the user git is broken), not for
/// the bootstrap's own download decision.
pub fn is_available(root: &Path) -> bool {
    // Local probe, so a short timeout is plenty; it still benefits from the
    // prompt-suppression environment.
    let exe = executable(root);
    run_git_with_timeout(&exe, &["--version"], None, Duration::from_secs(10)).is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn run_git_missing_executable_reports_not_found() {
        let err = run_git(Path::new("git"), &["--definitely-not-a-real-flag"], None).unwrap_err();
        assert!(matches!(err, GitError::Failed(_, _)));
    }

    /// A bootstrapped MinGit wins over `PATH`; without one the bare name is
    /// left for the OS to resolve.
    #[test]
    fn executable_prefers_a_bootstrapped_mingit() {
        let root = TempDir::new("glue-git-exe");
        assert_eq!(executable(root.path()), PathBuf::from("git"));

        let bootstrapped = root.path().join(MINIGIT_EXE_RELATIVE);
        std::fs::create_dir_all(bootstrapped.parent().unwrap()).unwrap();
        std::fs::write(&bootstrapped, b"x").unwrap();
        assert_eq!(executable(root.path()), bootstrapped);
    }

    #[test]
    fn clone_missing_url_reports_failure() {
        let root = TempDir::new("glue-git-clone");
        let dest = root.path().join("nope");
        let err = clone(root.path(), "https://127.0.0.1:1/invalid/repo.git", &dest).unwrap_err();
        assert!(matches!(err, GitError::Failed(_, _)));
    }

    #[test]
    fn rev_parse_head_on_non_repo_is_empty() {
        let root = TempDir::new("glue-git-rev");
        assert!(rev_parse_head(root.path(), root.path()).is_empty());
    }

    /// Everything the bucket check and pull actually see when GitHub/schannel
    /// drops the connection is retried; repository problems are not.
    #[test]
    fn transient_transport_failures_are_recognised() {
        let failed = |detail: &str| GitError::Failed("pull --ff-only".into(), detail.into());
        for detail in [
            "fatal: unable to access 'https://github.com/ScoopInstaller/Main/': \
             schannel: server closed abruptly (missing close_notify)",
            "fatal: unable to access 'https://github.com/x/y/': Recv failure: Connection reset by peer",
            "fatal: Could not resolve host: github.com",
            "error: RPC failed; curl 56 schannel: unexpected EOF",
            "fatal: the remote end hung up unexpectedly",
        ] {
            assert!(is_transient_git_failure(&failed(detail)), "{detail}");
        }
        for detail in [
            "fatal: not a git repository (or any of the parent directories): .git",
            "! [rejected] main -> main (non-fast-forward)",
            "fatal: repository 'https://github.com/x/y/' not found",
            "error: Your local changes would be overwritten by merge",
        ] {
            assert!(!is_transient_git_failure(&failed(detail)), "{detail}");
        }
        // A timeout already waited the full ceiling, and a missing executable
        // will not appear on the next try.
        assert!(!is_transient_git_failure(&GitError::Timeout {
            command: "git pull --ff-only".into(),
            seconds: 600,
        }));
        assert!(!is_transient_git_failure(&GitError::NotFound));
    }

    /// A hung child is killed at the deadline instead of blocking forever.
    #[cfg(windows)]
    #[test]
    fn run_with_timeout_kills_a_hung_child() {
        let mut cmd = Command::new("cmd");
        cmd.args(["/C", "ping", "-n", "6", "127.0.0.1"])
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let started = Instant::now();
        let err = run_with_timeout(cmd, "git test hang".to_string(), Duration::from_millis(200))
            .expect_err("child must be timed out");
        assert!(matches!(err, GitError::Timeout { .. }), "{err:?}");
        // Wide on purpose: taskkill waits on the child tree and an antivirus
        // filter can add seconds under load. The assertion still catches a
        // timeout path that never returns, without failing a busy runner.
        assert!(
            started.elapsed() < Duration::from_secs(15),
            "timeout returned too late: {:?}",
            started.elapsed()
        );
    }

    /// The timeout path still returns normally for fast commands.
    #[cfg(windows)]
    #[test]
    fn run_with_timeout_returns_output() {
        let mut cmd = Command::new("cmd");
        cmd.args(["/C", "echo", "hello"])
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        let output = run_with_timeout(cmd, "echo".to_string(), Duration::from_secs(5)).unwrap();
        assert!(output.status.success());
        assert!(String::from_utf8_lossy(&output.stdout).contains("hello"));
    }
}
