//! Progress events emitted by the engine during install/uninstall/update
//! operations. The CLI or GUI layer subscribes via a callback and renders
//! whatever UI it wants (progress bar, spinner, log lines, …).
//!
//! All events carry the package ref so the consumer can correlate events
//! across concurrent operations.

/// A single progress event emitted during an engine operation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProgressEvent {
    /// The operation has begun for this package.
    Started { package: String },
    /// The manifest was resolved, before any download/deploy work starts.
    /// `description` is the manifest's description (may be empty); consumers
    /// use it to tell the user what the package is.
    Resolved {
        package: String,
        version: String,
        bucket: String,
        description: String,
        /// Selected install architecture (`64bit` / `32bit` / `arm64`).
        arch: String,
    },
    /// Downloading the package artifact (bytes downloaded / total bytes).
    Downloading {
        package: String,
        /// Local filename of the artifact being downloaded.
        file: String,
        bytes: u64,
        total: u64,
    },
    /// The URL the transfer actually starts from — proxy-prefixed when
    /// `github_proxy` is configured, otherwise the canonical asset URL.
    /// Emitted once per transfer, before the first byte event, so consumers can
    /// show which source is in use.
    DownloadSource { package: String, url: String },
    /// Opening the network connection for an artifact: emitted *before* the
    /// request goes out, so a consumer can name the host while the first byte
    /// is still pending (the silent window between "Downloading x" and the
    /// first progress event).
    Connecting { package: String, url: String },
    /// One artifact finished downloading (timings in milliseconds).
    DownloadFinished {
        package: String,
        file: String,
        size: u64,
        network_ms: u64,
        store_ms: u64,
    },
    /// Verifying the downloaded artifact's hash.
    Verifying { package: String },
    /// Extracting/deploying files into the install directory.
    Deploying { package: String, files: usize },
    /// Hardlinking store blobs into the install directory: `done` of `total`
    /// files are in place. Emitted in strides, so consumers redraw one line.
    LinkingFiles {
        package: String,
        done: usize,
        total: usize,
    },
    /// Creating the `current` junction to the active version.
    Linking { package: String, version: String },
    /// Creating the PATH shims for the freshly linked package.
    CreatingShims { package: String },
    /// Persisting package metadata to the cache index.
    Persisting { package: String },
    /// Operation completed successfully.
    Completed { package: String, version: String },
    /// Operation failed.
    Failed { package: String, error: String },
    /// Uninstall in progress: removing files.
    Removing { package: String, version: String },
    /// Uninstall completed.
    Uninstalled { package: String },
    /// GC of a bucket's content cache has begun.
    GcStarted { bucket: String },
    /// GC progress: removed `removed` objects, `kept` still referenced.
    GcProgress {
        bucket: String,
        removed: usize,
        kept: usize,
    },
    /// GC of a bucket completed.
    GcCompleted { bucket: String, removed: usize },
    /// Per-phase install timings, emitted once per package when
    /// `InstallOptions::timings` is set. `line` is the machine-readable
    /// `GLUE_TIMINGS ...` summary; consumers print it verbatim.
    Timings { line: String },
}

/// Callback type for progress events. `Send + Sync` so it can be carried
/// across threads; consumers typically wrap it in a `Mutex` or channel.
pub type ProgressFn = Box<dyn Fn(ProgressEvent) + Send + Sync>;

/// No-op progress callback (useful when no UI is desired).
pub fn noop() -> ProgressFn {
    Box::new(|_| {})
}

/// Default progress callback that prints to stdout.
pub fn default() -> ProgressFn {
    Box::new(|event| match event {
        ProgressEvent::Started { package } => {
            println!("==> {}", package);
        }
        ProgressEvent::Resolved {
            package,
            version,
            bucket,
            description,
            arch,
        } => {
            println!("    {package} {version} from bucket {bucket} ({arch})");
            let text = description.trim();
            if !text.is_empty() {
                println!("    -> {text}");
            }
        }
        ProgressEvent::Downloading {
            package,
            file,
            bytes,
            total,
        } => match (bytes * 100).checked_div(total) {
            Some(pct) => {
                println!(
                    "    {} {} {}/{} bytes ({}%%)",
                    package, file, bytes, total, pct
                );
            }
            None => {
                println!("    {} {} {} bytes", package, file, bytes);
            }
        },
        ProgressEvent::DownloadSource { package, url } => {
            println!("    {package} source: {url}");
        }
        ProgressEvent::Connecting { package, url } => {
            println!("    {package} connecting to {url}");
        }
        ProgressEvent::DownloadFinished {
            package,
            file,
            size,
            network_ms,
            store_ms,
        } => {
            println!(
                "    {package} {file} downloaded ({size} bytes, network {network_ms} ms, store {store_ms} ms)"
            );
        }
        ProgressEvent::Verifying { package } => {
            println!("    {} verifying hash", package);
        }
        ProgressEvent::Deploying { package, files } => {
            println!("    {} deploying {} files", package, files);
        }
        ProgressEvent::LinkingFiles {
            package,
            done,
            total,
        } => {
            println!("    {} linking {done}/{total} files", package);
        }
        ProgressEvent::Linking { package, version } => {
            println!("    {} linking current -> {}", package, version);
        }
        ProgressEvent::CreatingShims { package } => {
            println!("    {} creating shims", package);
        }
        ProgressEvent::Persisting { package } => {
            println!("    {} persisting index", package);
        }
        ProgressEvent::Completed { package, version } => {
            println!("    {} {} installed", package, version);
        }
        ProgressEvent::Failed { package, error } => {
            eprintln!("    {} FAILED: {}", package, error);
        }
        ProgressEvent::Removing { package, version } => {
            println!("    {} removing {} files", package, version);
        }
        ProgressEvent::Uninstalled { package } => {
            println!("    {} uninstalled", package);
        }
        ProgressEvent::GcStarted { bucket } => {
            println!("    GC started for bucket {bucket}");
        }
        ProgressEvent::GcProgress {
            bucket,
            removed,
            kept,
        } => {
            println!("    GC {bucket}: removed {removed}, kept {kept}");
        }
        ProgressEvent::GcCompleted { bucket, removed } => {
            println!("    GC {bucket} completed: removed {removed} objects");
        }
        ProgressEvent::Timings { line } => {
            println!("{line}");
        }
    })
}
