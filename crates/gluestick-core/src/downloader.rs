//! Downloader: fetches package artifacts into the CAS store with mirror
//! fallback, resumable partial files, and manifest hash verification.
//!
//! Not yet implemented: streaming ZIP member ingest and a progress-bar UI (the
//! engine emits callbacks instead).

use std::collections::BTreeSet;
use std::io::{ErrorKind, Read};
use std::path::PathBuf;
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::cancel::CancelToken;
use crate::config;
use crate::schannel::SchannelConnector;
use crate::store::Store;
use ureq::unversioned::resolver::DefaultResolver;

/// HTTP body copy buffer.
const COPY_BUFFER_SIZE: usize = 128 * 1024;

/// Size below which fresh downloads stream HTTP → store without writing a
/// .partial file.
pub const DIRECT_STREAM_MAX_BYTES: u64 = 64 << 20; // 64 MiB

/// Upper bound for single-connection tuned downloads. Files at or below this
/// size never use parallel range requests.
pub const GRAB_STYLE_MAX_BYTES: u64 = 64 << 20; // 64 MiB

/// Ceiling for the single-GET path when the task carries a cancel token.
/// A cancelled direct stream cannot resume, so only small artifacts take it;
/// larger ones keep the resumable `.partial` path.
pub const CANCEL_DIRECT_MAX_BYTES: u64 = 16 << 20; // 16 MiB

/// Default parallel range download worker count (>64 MiB files).
pub const DEFAULT_WORKERS: u32 = 8;
/// Minimum user-configurable parallel download worker count.
pub const MIN_USER_WORKERS: u32 = 2;
/// Maximum user-configurable parallel download worker count.
pub const MAX_USER_WORKERS: u32 = 16;
/// Step size for user worker settings (2, 4, 6, …).
pub const USER_WORKERS_STEP: u32 = 2;

/// One extra connection per this many bytes, so a very large artifact keeps
/// more of the link busy while a 70 MiB one does not open a dozen sockets.
const BYTES_PER_PARALLEL_CONNECTION: u64 = 32 << 20;

/// Floor for the size-derived connection count (files above the parallel
/// threshold).
const MIN_SIZE_WORKERS: u32 = 4;

/// Minimum per-connection chunk for parallel downloads.
pub const MIN_PARALLEL_CHUNK_SIZE: u64 = 4 << 20; // 4 MiB

/// Minimum remaining bytes to resume with parallel ranges.
pub const MIN_PARALLEL_RESUME_BYTES: u64 = 32 << 20; // 32 MiB

/// Caps HTTP range connections for a single file.
const MAX_PARALLEL_CONNECTIONS: u32 = 16;

/// Retry attempts for a single request.
const DOWNLOAD_RETRY_ATTEMPTS: u32 = 4;
/// Backoff base for download retries.
const DOWNLOAD_RETRY_BASE: Duration = Duration::from_millis(500);

/// Attempts per chunk in a parallel download. Each retry asks only for the
/// bytes still missing and moves to the next mirror, so a reset connection or
/// a truncated body costs a retry rather than the whole chunk.
const CHUNK_ATTEMPTS: u32 = 3;

/// Total attempts for one resumable transfer (two retries after the first).
/// A dropped body connection, a truncated body or a transient status leaves
/// the `.part` file (and, for a parallel session, its chunk record) behind, so
/// the retry resumes from the bytes on disk instead of starting over.
const TRANSFER_ATTEMPTS: u32 = 3;

/// Snap `n` to a valid user worker count (2–16, step 2).
pub fn normalize_user_workers(n: u32) -> u32 {
    if n <= MIN_USER_WORKERS {
        return MIN_USER_WORKERS;
    }
    if n >= MAX_USER_WORKERS {
        return MAX_USER_WORKERS;
    }
    let rem = (n - MIN_USER_WORKERS) % USER_WORKERS_STEP;
    if rem == 0 {
        return n;
    }
    let lower = n - rem;
    let upper = lower + USER_WORKERS_STEP;
    if upper - n < n - lower {
        upper
    } else {
        lower
    }
}

#[derive(Debug, Error)]
pub enum DownloadError {
    /// The caller asked the transfer to stop ([`CancelToken`]). Nothing is
    /// discarded: bytes already in the store, and a resumable `.part` file,
    /// stay usable for the next attempt.
    #[error("download cancelled")]
    Cancelled,
    #[error("HTTP {status}: {status_text}")]
    Http { status: u16, status_text: String },
    #[error("HTTP {status}: {status_text} ({url})")]
    HttpAt {
        status: u16,
        status_text: String,
        url: String,
    },
    #[error("download failed: {0}")]
    Transport(String),
    #[error("all download URLs failed, last error: {0}")]
    AllUrlsFailed(String),
    #[error("no URLs to try")]
    NoUrls,
    #[error("integrity check failed: hash mismatch (expected {algo}:{expected}, got {got})")]
    HashMismatch {
        algo: String,
        expected: String,
        got: String,
    },
    #[error("unsupported hash algorithm: {0}")]
    UnsupportedHashAlgo(String),
    #[error("integrity check failed: no content hash to verify against {0}:{1}")]
    NoContentHash(String, String),
    #[error("integrity check failed: open content: {0}")]
    VerifyOpen(#[source] std::io::Error),
    #[error("integrity check failed: read content: {0}")]
    VerifyRead(#[source] std::io::Error),
    #[error("unexpected Content-Range start {start}, expected {expected}")]
    UnexpectedRangeStart { start: u64, expected: u64 },
    // The body ended before its declared length. Retries normally hide this;
    // when they run out the user needs to know it is a transfer problem and
    // what to do about it, not just two byte counts.
    #[error(
        "incomplete download: got {got} bytes, expected {expected} — the transfer was cut short, run the install again"
    )]
    Incomplete { got: u64, expected: u64 },
    #[error("parallel: not enough chunks")]
    ParallelNotEnoughChunks,
    #[error("parallel resume: remaining too small")]
    ParallelResumeTooSmall,
    #[error("parallel download incomplete: got {got}, want {want}")]
    ParallelIncomplete { got: u64, want: u64 },
    #[error("stat partial: {0}")]
    StatPartial(#[source] std::io::Error),
    #[error("allocate partial file: {0}")]
    AllocatePartial(#[source] std::io::Error),
    #[error("chunk {start}-{end}: {message}")]
    Chunk {
        start: u64,
        end: u64,
        message: String,
    },
    #[error("chunk {start}-{end}: HTTP {status}")]
    ChunkHttp { start: u64, end: u64, status: u16 },
    #[error("chunk size mismatch: wrote {got}, expected {expected}")]
    ChunkSizeMismatch { got: u64, expected: u64 },
    #[error("open downloaded file: {0}")]
    OpenDownloaded(#[source] std::io::Error),
    #[error("open partial file: {0}")]
    OpenPartial(#[source] std::io::Error),
    #[error("seek partial file: {0}")]
    SeekPartial(#[source] std::io::Error),
    #[error("truncate partial file: {0}")]
    TruncatePartial(#[source] std::io::Error),
    #[error("write partial file: {0}")]
    WritePartial(#[source] std::io::Error),
    #[error("sync partial file: {0}")]
    SyncPartial(#[source] std::io::Error),
    #[error("save partial meta: {0}")]
    SaveMeta(#[source] std::io::Error),
    #[error("create partial dir: {0}")]
    PartialDir(#[source] std::io::Error),
    #[error("store ingest: {0}")]
    Store(#[from] crate::store::StoreError),
    #[error(transparent)]
    Ftp(#[from] crate::ftp::FtpError),
    #[error(transparent)]
    Io(#[from] std::io::Error),
}

/// A single download task.
#[derive(Debug, Clone, Default)]
pub struct Task {
    pub url: String,
    pub filename: String,
    /// Deprecated: use `hash_value`.
    pub checksum: String,
    /// Manifest hash algorithm: sha256, sha512, sha1, md5.
    pub hash_algo: String,
    /// Manifest expected digest (no algo prefix).
    pub hash_value: String,
    /// Optional stop signal polled by every read loop; a task without one
    /// downloads to the end.
    pub cancel: Option<CancelToken>,
}

impl PartialEq for Task {
    fn eq(&self, other: &Task) -> bool {
        self.url == other.url
            && self.filename == other.filename
            && self.checksum == other.checksum
            && self.hash_algo == other.hash_algo
            && self.hash_value == other.hash_value
            && self.cancel == other.cancel
    }
}

impl Eq for Task {}

/// Whether `task` has been asked to stop.
pub(crate) fn task_cancelled(task: &Task) -> bool {
    task.cancel
        .as_ref()
        .is_some_and(|token| token.is_cancelled())
}

/// Breaks a download into network transfer and cache store ingest
///
/// For the HTTP→store direct stream the ingest is pipelined with the read, so
/// `store_ingest` stays zero and `network` covers the whole operation — same
/// convention as.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Timing {
    pub network: Duration,
    pub store_ingest: Duration,
}

impl Timing {
    fn add_network(&mut self, d: Duration) {
        self.network += d;
    }

    fn add_store_ingest(&mut self, d: Duration) {
        self.store_ingest += d;
    }
}

/// The outcome of a download task.the error
/// travels with the result so batch downloads can report per-task failures.
#[derive(Debug)]
pub struct DownloadResult {
    pub task: Task,
    /// CAS store hash (SHA-256) of the ingested blob.
    pub hash: String,
    pub size: u64,
    pub error: Option<DownloadError>,
    /// Blob reused from the store without HTTP.
    pub from_store: bool,
    /// Link-ready member map for zip archives (relPath → CAS hash), attached
    /// when a valid member index exists.
    pub files: Option<std::collections::BTreeMap<String, String>>,
    /// Total install bytes recorded in the member index
    pub zip_total_bytes: u64,
    /// Network vs store-ingest split.
    pub timing: Timing,
}

impl DownloadResult {
    fn new(task: Task) -> Self {
        DownloadResult {
            task,
            hash: String::new(),
            size: 0,
            error: None,
            from_store: false,
            files: None,
            zip_total_bytes: 0,
            timing: Timing::default(),
        }
    }
}

/// Progress event emitted while bytes land (engine-level; UIs render it).
///
/// `filename` is an `Arc<str>`, not a `String`: progress fires on *every*
/// read (one per 128 KiB, from up to 16 chunk workers), and the previous
/// owned `String` meant a heap allocation per event — paid before the
/// throttle check and then thrown away for ~99% of events. An `Arc` clone
/// is a refcount bump. No consumer currently reads `filename` at all; it
/// stays in the public struct for embedders.
#[derive(Debug, Clone)]
pub struct Progress {
    pub filename: Arc<str>,
    pub total: u64,
    pub offset: u64,
}

/// `Sync` so chunk workers can share it in parallel downloads.
pub type ProgressFn<'a> = Option<&'a (dyn Fn(&Progress) + Send + Sync)>;

/// Minimum wall-clock gap between two throttled progress emissions
pub const PROGRESS_EMIT_INTERVAL: Duration = Duration::from_millis(150);

/// Rate-limited progress sink.
///
/// [`ProgressReader`] fires on *every* `read` — tens of thousands of calls for
/// a large artifact — so consumers that touch the terminal must throttle.
/// emit when the integer percentage changed, or when the
/// interval elapsed and the byte count moved. Parallel-range workers report
/// through the same instance, hence the `Mutex`.
pub struct ThrottledProgress<'a> {
    inner: &'a (dyn Fn(&Progress) + Send + Sync),
    state: Mutex<ThrottleState>,
}

#[derive(Default)]
struct ThrottleState {
    last_emit: Option<Instant>,
    last_pct: Option<u64>,
    last_offset: u64,
    last: Option<Progress>,
}

impl<'a> ThrottledProgress<'a> {
    pub fn new(inner: &'a (dyn Fn(&Progress) + Send + Sync)) -> Self {
        ThrottledProgress {
            inner,
            state: Mutex::new(ThrottleState::default()),
        }
    }

    /// Report one observation; emits only when the throttle window allows it.
    pub fn report(&self, p: &Progress) {
        let pct = percentage(p.offset, p.total);
        let now = Instant::now();
        let mut st = match self.state.lock() {
            Ok(guard) => guard,
            // A panicking progress consumer must not poison the download.
            Err(poisoned) => poisoned.into_inner(),
        };
        let window_elapsed = match st.last_emit {
            Some(prev) => now.duration_since(prev) >= PROGRESS_EMIT_INTERVAL,
            None => true,
        };
        let pct_changed = st.last_pct != Some(pct);
        let bytes_moved = st.last_offset != p.offset;
        if !(pct_changed || (window_elapsed && bytes_moved)) {
            return;
        }
        st.last_emit = Some(now);
        st.last_pct = Some(pct);
        st.last_offset = p.offset;
        st.last = Some(p.clone());
        drop(st);
        (self.inner)(p);
    }

    /// Force the most recent values out (phase boundaries)
    /// `ThrottledProgress.Flush`.
    pub fn flush(&self) {
        let st = match self.state.lock() {
            Ok(guard) => guard,
            Err(poisoned) => poisoned.into_inner(),
        };
        if let Some(last) = &st.last {
            (self.inner)(last);
        }
    }
}

/// Integer percentage, clamped to 100; `0` when the total is unknown.
fn percentage(offset: u64, total: u64) -> u64 {
    if total == 0 {
        return 0;
    }
    (offset.min(total) * 100) / total
}

// ---------------------------------------------------------------------------
// Hash helpers (hash.go)
// ---------------------------------------------------------------------------

/// Whether the manifest digest equals the CAS object key (SHA-256).
fn manifest_uses_store_hash(algo: &str) -> bool {
    matches!(algo.to_lowercase().as_str(), "" | "sha256" | "sha2")
}

enum ManifestHasher {
    Sha1(sha1::Sha1),
    Md5(md5::Md5),
    Sha512(sha2::Sha512),
}

impl ManifestHasher {
    fn new(algo: &str) -> std::result::Result<ManifestHasher, DownloadError> {
        match algo.to_lowercase().as_str() {
            "sha1" => Ok(ManifestHasher::Sha1(sha1::Sha1::new())),
            "md5" => Ok(ManifestHasher::Md5(md5::Md5::new())),
            "sha512" => Ok(ManifestHasher::Sha512(sha2::Sha512::new())),
            other => Err(DownloadError::UnsupportedHashAlgo(other.to_string())),
        }
    }

    fn update(&mut self, data: &[u8]) {
        match self {
            ManifestHasher::Sha1(h) => h.update(data),
            ManifestHasher::Md5(h) => h.update(data),
            ManifestHasher::Sha512(h) => h.update(data),
        }
    }

    fn finish(self) -> String {
        match self {
            ManifestHasher::Sha1(h) => hex_encode(&h.finalize()),
            ManifestHasher::Md5(h) => hex_encode(&h.finalize()),
            ManifestHasher::Sha512(h) => hex_encode(&h.finalize()),
        }
    }
}

/// Reader that tees bytes into a manifest digest while the store pulls them.
struct TeeReader<R> {
    inner: R,
    hasher: Option<ManifestHasher>,
}

impl<R: Read> Read for TeeReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = self.inner.read(buf)?;
        if n > 0 {
            if let Some(h) = self.hasher.as_mut() {
                h.update(&buf[..n]);
            }
        }
        Ok(n)
    }
}

fn verify_manifest_digest(
    algo: &str,
    expected: &str,
    actual: &str,
) -> std::result::Result<(), DownloadError> {
    if expected.is_empty() {
        return Ok(());
    }
    if !actual.eq_ignore_ascii_case(expected) {
        return Err(DownloadError::HashMismatch {
            algo: algo.to_string(),
            expected: expected.to_string(),
            got: actual.to_string(),
        });
    }
    Ok(())
}

/// Check a store object against the manifest digest on `task`.
pub fn verify_archive_object(
    store: &Store,
    task: &Task,
    cas_hash: &str,
) -> std::result::Result<(), DownloadError> {
    if task.hash_value.is_empty() {
        return Ok(());
    }
    if cas_hash.is_empty() {
        return Err(DownloadError::NoContentHash(
            task.hash_algo.clone(),
            task.hash_value.clone(),
        ));
    }
    if manifest_uses_store_hash(&task.hash_algo) {
        return verify_manifest_digest(&task.hash_algo, &task.hash_value, cas_hash);
    }
    // Non-SHA-256: re-hash the blob with the manifest algorithm.
    let mut f =
        std::fs::File::open(store.object_path(cas_hash)).map_err(DownloadError::VerifyOpen)?;
    let mut h = ManifestHasher::new(&task.hash_algo)?;
    let mut buf = [0u8; COPY_BUFFER_SIZE];
    loop {
        let n = f.read(&mut buf).map_err(DownloadError::VerifyRead)?;
        if n == 0 {
            break;
        }
        h.update(&buf[..n]);
    }
    verify_manifest_digest(&task.hash_algo, &task.hash_value, &h.finish())
}

// ---------------------------------------------------------------------------
// Manifest hash alias index (manifest_hash_index.go)
// ---------------------------------------------------------------------------

#[derive(Serialize, Deserialize)]
struct ManifestHashEntry {
    cas_hash: String,
}

fn manifest_hash_index_path(store_root: &std::path::Path, algo: &str, digest: &str) -> PathBuf {
    let key = format!("{}_{}", algo.to_lowercase(), digest.to_lowercase());
    store_root
        .join(".manifest-hash")
        .join(format!("{key}.json"))
}

/// Save a manifest-hash → CAS-hash mapping (atomic temp + rename). No-op for
/// SHA-256 (the CAS key already is the manifest digest).
fn save_manifest_hash_alias(
    store_root: &std::path::Path,
    algo: &str,
    digest: &str,
    cas_hash: &str,
) -> std::io::Result<()> {
    if digest.is_empty() || cas_hash.is_empty() || manifest_uses_store_hash(algo) {
        return Ok(());
    }
    let dir = store_root.join(".manifest-hash");
    std::fs::create_dir_all(&dir)?;
    let payload = serde_json::to_vec(&ManifestHashEntry {
        cas_hash: cas_hash.to_string(),
    })?;
    let path = manifest_hash_index_path(store_root, algo, digest);
    let tmp = dir.join(format!(
        "manifest-hash-{}-{}.tmp",
        std::process::id(),
        ALIAS_COUNTER.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    ));
    std::fs::write(&tmp, payload)?;
    if let Err(e) = std::fs::rename(&tmp, &path) {
        let _ = std::fs::remove_file(&tmp);
        return Err(e);
    }
    Ok(())
}

static ALIAS_COUNTER: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

/// Load a CAS hash from a manifest hash alias entry.
fn load_manifest_hash_alias(
    store_root: &std::path::Path,
    algo: &str,
    digest: &str,
) -> Option<String> {
    if digest.is_empty() {
        return None;
    }
    let data = std::fs::read(manifest_hash_index_path(store_root, algo, digest)).ok()?;
    let entry: ManifestHashEntry = serde_json::from_slice(&data).ok()?;
    if entry.cas_hash.is_empty() {
        return None;
    }
    Some(entry.cas_hash)
}

// ---------------------------------------------------------------------------
// Partial state (partial_state.go + resume.go pieces)
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
struct PartialMeta {
    url: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    source_url: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    total_size: u64,
    /// Parallel session in flight: the full size it is filling. `total_size`
    /// stays 0 until the session finishes, so a preallocated `.part` (full
    /// length, holes) is never mistaken for a finished download.
    #[serde(default, skip_serializing_if = "is_zero")]
    planned_total: u64,
    /// Ranges an in-flight parallel session has written for real; every other
    /// byte of a preallocated `.part` is a hole. Persisted as each chunk
    /// completes, so a paused or killed download resumes with only the
    /// missing ranges instead of starting over.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    completed_chunks: Vec<CompletedChunk>,
}

/// One byte range a parallel session finished writing (`end` inclusive).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
struct CompletedChunk {
    start: u64,
    end: u64,
}

impl PartialMeta {
    /// Byte ranges of `span` the record does not cover yet.
    ///
    /// A resumed session fetches exactly these: bytes recorded by an earlier
    /// attempt are never requested twice, even when the previous session split
    /// the file differently.
    fn uncovered(&self, span: ByteRange) -> Vec<ByteRange> {
        let mut ranges = self.completed_chunks.clone();
        ranges.sort_by_key(|r| r.start);
        let mut out = Vec::new();
        let mut cursor = span.start;
        for range in ranges {
            if range.end < cursor {
                continue;
            }
            if range.start > span.end {
                break;
            }
            if range.start > cursor {
                out.push(ByteRange {
                    start: cursor,
                    end: range.start - 1,
                });
            }
            if range.end >= cursor {
                cursor = range.end + 1;
            }
            if cursor > span.end {
                return out;
            }
        }
        if cursor <= span.end {
            out.push(ByteRange {
                start: cursor,
                end: span.end,
            });
        }
        out
    }
}

/// Persists a worker's write progress into the meta as it goes.
///
/// Recording whole chunks is far too coarse for a slow line: a 400 MiB file is
/// four ~100 MiB chunks, so pausing at 40% would record nothing and the next
/// attempt would start over. Each worker reports every `granule` bytes instead,
/// which bounds what a pause, an app exit or a crash can cost (one granule per
/// connection: 400 MiB / 512, clamped to 1–8 MiB).
struct ChunkRecorder<'a> {
    record: &'a std::sync::Mutex<PartialMeta>,
    meta_path: &'a std::path::Path,
    granule: u64,
}

impl ChunkRecorder<'_> {
    fn record(&self, start: u64, end: u64) {
        if end < start {
            return;
        }
        let mut persisted = self.record.lock().unwrap_or_else(|e| e.into_inner());
        persisted
            .completed_chunks
            .push(CompletedChunk { start, end });
        let _ = save_partial_meta(self.meta_path, &persisted);
    }
}

fn is_zero(v: &u64) -> bool {
    *v == 0
}

fn partial_id(url: &str) -> String {
    let sum = Sha256::digest(url.as_bytes());
    hex_encode(&sum[..16])
}

fn load_partial_meta(meta_path: &std::path::Path) -> std::io::Result<PartialMeta> {
    let data = std::fs::read(meta_path)?;
    serde_json::from_slice(&data)
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))
}

fn save_partial_meta(meta_path: &std::path::Path, meta: &PartialMeta) -> std::io::Result<()> {
    let data = serde_json::to_vec(meta)
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;
    std::fs::write(meta_path, data)
}

/// Parse a `Content-Range` header: `bytes 500-999/1000` or `bytes 500-999/*`.
/// Returns `(start, end, total)` where `total` is 0 when the server sent `*`.
pub fn parse_content_range(header: &str) -> Option<(u64, u64, u64)> {
    let rest = header.strip_prefix("bytes ")?;
    let (range, total) = rest.split_once('/')?;
    let (start, end) = range.split_once('-')?;
    let start: u64 = start.parse().ok()?;
    let end: u64 = end.parse().ok()?;
    let total: u64 = if total == "*" { 0 } else { total.parse().ok()? };
    Some((start, end, total))
}

fn remove_partial(part_path: &std::path::Path, meta_path: &std::path::Path) {
    let _ = std::fs::remove_file(part_path);
    let _ = std::fs::remove_file(meta_path);
}

/// Shrink a `.part` back to `len`, discarding anything a failed parallel
/// session may have written (or preallocated) beyond that point. Best-effort:
/// the caller is already on an error path.
fn truncate_partial_to(part_path: &std::path::Path, len: u64) {
    if let Ok(file) = std::fs::OpenOptions::new().write(true).open(part_path) {
        let _ = file.set_len(len);
    }
}

/// Resumable on-disk state of a single download task.
struct PartialState {
    part_path: PathBuf,
    meta_path: PathBuf,
    task_url: String,
    meta: PartialMeta,
    /// Bytes already present in the .part file.
    offset: u64,
}

impl PartialState {
    fn complete(&self) -> bool {
        self.meta.total_size > 0 && self.offset >= self.meta.total_size
    }

    fn reset(&mut self) {
        remove_partial(&self.part_path, &self.meta_path);
        self.meta = PartialMeta {
            url: self.task_url.clone(),
            ..Default::default()
        };
        self.offset = 0;
    }

    fn commit(&mut self, used_url: &str, total_size: u64) {
        if used_url.is_empty() {
            return;
        }
        self.meta.url = self.task_url.clone();
        self.meta.source_url = used_url.to_string();
        self.meta.total_size = total_size;
        self.meta.planned_total = 0;
        self.meta.completed_chunks.clear();
        let _ = save_partial_meta(&self.meta_path, &self.meta);
    }

    /// Roll a failed parallel attempt back to the resumable sequential prefix:
    /// truncate the placeholder bytes and drop the chunk record.
    fn abandon_plan(&mut self, keep_bytes: u64, total_size: u64) {
        truncate_partial_to(&self.part_path, keep_bytes);
        self.offset = keep_bytes;
        self.meta.total_size = total_size;
        self.meta.planned_total = 0;
        self.meta.completed_chunks.clear();
        let _ = save_partial_meta(&self.meta_path, &self.meta);
    }
}

/// Decision derived from a download response.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ResponseOutcome {
    /// Write the body at this offset; server claims `total_size`.
    Write { offset: u64, total_size: u64 },
    /// Server refused the range: retry the whole file from zero.
    Restart,
    /// Offset is already at/past total: nothing left to fetch.
    ShortRange { total_size: u64 },
}

/// Normalize a GET response into the write offset and claimed total size.
/// Split from I/O so the status matrix stays unit-testable.
fn resolve_response(
    status: u16,
    content_range: Option<&str>,
    content_length: Option<u64>,
    meta_total: u64,
    offset: u64,
) -> std::result::Result<ResponseOutcome, DownloadError> {
    let mut offset = offset;
    let mut meta_total = meta_total;
    match status {
        206 => {
            if let Some((start, _, total)) = content_range.and_then(parse_content_range) {
                if start != offset {
                    return Err(DownloadError::UnexpectedRangeStart {
                        start,
                        expected: offset,
                    });
                }
                if total > 0 {
                    meta_total = total;
                }
            }
        }
        200 => {
            // Server ignored Range and is sending the whole file: discard any
            // bytes already on disk rather than appending to them.
            offset = 0;
        }
        416 => {
            if meta_total > 0 && offset >= meta_total {
                return Ok(ResponseOutcome::ShortRange {
                    total_size: meta_total,
                });
            }
            return Ok(ResponseOutcome::Restart);
        }
        other => {
            return Err(DownloadError::Http {
                status: other,
                status_text: status_text(other),
            });
        }
    }
    let total = if status != 206 {
        content_length.unwrap_or(meta_total)
    } else {
        meta_total
    };
    Ok(ResponseOutcome::Write {
        offset,
        total_size: total,
    })
}

/// Reason phrases.StatusText` for statuses we surface.
fn status_text(code: u16) -> String {
    let text = match code {
        200 => "200 OK",
        206 => "206 Partial Content",
        301 => "301 Moved Permanently",
        302 => "302 Found",
        304 => "304 Not Modified",
        400 => "400 Bad Request",
        401 => "401 Unauthorized",
        403 => "403 Forbidden",
        404 => "404 Not Found",
        416 => "416 Range Not Satisfiable",
        429 => "429 Too Many Requests",
        500 => "500 Internal Server Error",
        502 => "502 Bad Gateway",
        503 => "503 Service Unavailable",
        other => return format!("status code {other}"),
    };
    text.to_string()
}

// ---------------------------------------------------------------------------
// Retry (retry.go)
// ---------------------------------------------------------------------------

/// Exponential backoff: 500ms * 2^(attempt-1) for attempt > 0, else 0.
///
/// `GLUE_TEST_RETRY_DELAY_MS` (test builds only) scales the backoff down so
/// failure-path tests that deliberately hit dead mirrors do not burn the real
/// 0.5+1+2 s of wall clock — that single test used to dominate the suite at
/// 11.7 s. Production behaviour is untouched: the variable is compiled out
/// outside `cfg(test)` and never read at runtime by the shipped binary.
fn retry_download_delay(attempt: u32) -> Duration {
    if attempt == 0 {
        return Duration::ZERO;
    }
    #[cfg(test)]
    if std::env::var_os("GLUE_TEST_RETRY_DELAY_MS").is_some() {
        return Duration::ZERO;
    }
    retry_download_delay_raw(attempt)
}

/// The real backoff curve, kept separate so the parity test can assert the
/// values without depending on the test-only override above.
fn retry_download_delay_raw(attempt: u32) -> Duration {
    if attempt == 0 {
        return Duration::ZERO;
    }
    DOWNLOAD_RETRY_BASE * (1 << (attempt - 1))
}

/// Read a `u64` header off a ureq 3 response (`http` crate header map).
fn header_u64(resp: &http::Response<ureq::Body>, name: &str) -> Option<u64> {
    resp.headers()
        .get(name)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<u64>().ok())
}

/// Read a header value as an owned `String` (the `http` crate header map
/// borrows out of the response, which never lives long enough here).
fn header_str(resp: &http::Response<ureq::Body>, name: &str) -> Option<String> {
    resp.headers()
        .get(name)
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned)
}

/// Whether an error is retryable during download: timeouts, connection
/// resets, EOF-class failures (message-matched.Error handling).
///
/// ureq 3 note: statuses never reach this function — the agent sets
/// `http_status_as_error(false)` explicitly, so 4xx/5xx arrive as plain
/// responses (http.Client semantics) and only transport failures are
/// classified here. Transient statuses are classified by
/// [`is_retryable_status`] instead.
fn is_retryable(err: &ureq::Error) -> bool {
    match err {
        ureq::Error::Io(io_err) => {
            let kind = io_err.kind();
            kind == ErrorKind::ConnectionRefused
                || kind == ErrorKind::ConnectionReset
                || kind == ErrorKind::ConnectionAborted
                || kind == ErrorKind::BrokenPipe
                || kind == ErrorKind::NotConnected
                || kind == ErrorKind::UnexpectedEof
                || kind == ErrorKind::TimedOut
                || kind == ErrorKind::Interrupted
                // Route/VPN flaps: usually gone a moment later.
                || kind == ErrorKind::HostUnreachable
                || kind == ErrorKind::NetworkUnreachable
                || kind == ErrorKind::NetworkDown
        }
        ureq::Error::Timeout(_) => true,
        ureq::Error::ConnectionFailed => true,
        // A transient TLS handshake failure (reset during the handshake, a
        // schannel hiccup) often succeeds on a fresh connection, so it gets
        // the normal retry budget. Certificate and hostname validation
        // failures are permanent — retrying them only delays mirror fallback.
        ureq::Error::Tls(msg) => !msg.to_lowercase().contains("certificate"),
        ureq::Error::StatusCode(_) => false,
        ureq::Error::Protocol(_) => false,
        ureq::Error::RedirectFailed => false,
        ureq::Error::InvalidProxyUrl => false,
        ureq::Error::TooManyRedirects => false,
        ureq::Error::BodyExceedsLimit(_) => false,
        ureq::Error::BadUri(_) => false,
        ureq::Error::HostNotFound => false,
        // `http::Error` carries some socket-layer messages (connection reset,
        // EOF, etc.) when ureq's connector wraps them — keep the heuristic.
        ureq::Error::Http(http_err) => {
            let msg = http_err.to_string().to_lowercase();
            msg.contains("eof")
                || msg.contains("connection reset")
                || msg.contains("broken pipe")
                || msg.contains("forcibly closed")
                || msg.contains("connection aborted")
                || msg.contains("timed out")
        }
        // ureq::Error is #[non_exhaustive]: any future variant stays safe.
        _ => false,
    }
}

/// Whether `err` justifies another attempt at this attempt number.
///
/// A host that does not resolve is often a flapping VPN or a DNS hiccup, so it
/// gets one retry — but only one: a permanently bad URL must not stall the
/// mirror fallback for the full retry budget.
fn should_retry(err: &ureq::Error, attempt: u32) -> bool {
    is_retryable(err) || (matches!(err, ureq::Error::HostNotFound) && attempt < 2)
}

/// HTTP statuses worth retrying on the same URL: throttling and gateway or
/// origin overload. Anything else (404, 403, …) is final and goes straight to
/// mirror fallback instead of burning the retry budget.
fn is_retryable_status(status: u16) -> bool {
    matches!(status, 408 | 425 | 429 | 500 | 502 | 503 | 504)
}

/// Whether a transfer failure is worth another attempt.
///
/// Dropped connections, truncated bodies and transient statuses keep a
/// resumable `.part`, so retrying them costs only the missing tail. A missing
/// artifact (404), a refused request (403), a digest mismatch or a local
/// filesystem error would fail exactly the same way again, so they stay hard
/// errors.
fn is_retryable_transfer_error(err: &DownloadError) -> bool {
    match err {
        DownloadError::Transport(_) => true,
        DownloadError::Http { status, .. }
        | DownloadError::HttpAt { status, .. }
        | DownloadError::ChunkHttp { status, .. } => is_retryable_status(*status),
        DownloadError::Incomplete { .. }
        | DownloadError::ParallelIncomplete { .. }
        | DownloadError::ChunkSizeMismatch { .. } => true,
        _ => false,
    }
}

/// `Retry-After` in seconds, clamped so a broken or hostile server cannot
/// stall a download for minutes. Only the delta-seconds form is honoured; the
/// HTTP-date form falls back to the normal backoff curve.
fn retry_after_delay(resp: &http::Response<ureq::Body>) -> Option<Duration> {
    let seconds = header_str(resp, "Retry-After")?
        .trim()
        .parse::<u64>()
        .ok()?;
    Some(Duration::from_secs(seconds.min(10)))
}

/// Whether a parallel failure means the servers will not serve byte ranges at
/// all, so the caller must drop the plan and fall back to one connection.
fn ranges_unusable(err: &DownloadError) -> bool {
    matches!(
        err,
        DownloadError::ChunkHttp { status, .. } if *status == 200 || *status == 416
    )
}

/// Persist the tail of a chunk that reached the disk but not yet the meta.
fn record_written(
    recorder: Option<&ChunkRecorder<'_>>,
    chunk_start: u64,
    written: u64,
    recorded: u64,
) {
    if let Some(rec) = recorder {
        if written > recorded {
            rec.record(chunk_start + recorded, chunk_start + written - 1);
        }
    }
}

// ---------------------------------------------------------------------------
// Downloader
// ---------------------------------------------------------------------------

/// Result of the single-GET streaming probe ([`Downloader::try_direct_stream`]).
///
/// `Fallback` carries the artifact size learned from the probe response so the
/// `.partial` / parallel path can skip a redundant size HEAD round-trip
// `DownloadResult` is intentionally moved wholesale (same as the
// `result_large_err` allowance on streaming functions); boxing it would
// diverge from the download-result shape.
#[allow(clippy::large_enum_variant)]
enum DirectStream {
    /// The artifact streamed straight into the store in one pass.
    Downloaded(DownloadResult),
    /// No usable single-stream download; fall back to the resumable path.
    /// `known_total` is 0 when the size could not be determined.
    Fallback { known_total: u64 },
}

/// What `proxy_packages` decided for one package transfer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PackageProxy {
    /// The package is not listed; the normal route applies.
    NotListed,
    /// The package is listed and a proxy is configured for its hosts.
    Configured,
    /// The package is listed but no proxy URL is configured, so the transfer
    /// goes direct. The CLI reports this at the end of the command.
    MissingProxy,
}

/// Concurrent downloader writing into the CAS store.
pub struct Downloader {
    agent: ureq::Agent,
    store: Store,
    workers: u32,
    user_agent: String,
    /// GitHub mirror prefixes; empty uses direct URLs only.
    gh_proxies: Vec<String>,
    /// HTTP range parallel download for files > GrabStyleMaxBytes.
    parallel_download: bool,
    /// Minimum total bytes to split a download into ranges
    /// (, default 64 MiB + 1).
    parallel_min_bytes: u64,
    /// Minimum per-connection chunk size for parallel downloads
    /// (, default 4 MiB).
    parallel_min_chunk_size: u64,
    /// Size ceiling for the single-GET direct-stream path
    /// (, default 64 MiB).
    direct_stream_max_bytes: u64,
    /// Size ceiling for the direct-stream path when the task can be cancelled
    /// (default 16 MiB; 0 disables it so cancellations stay resumable).
    cancel_direct_max_bytes: u64,
    /// HTTP CONNECT proxy for both schemes (environment, system or config).
    /// Kept only in the shared state every agent's connector reads.
    proxy_state: Arc<RwLock<crate::proxy::ProxySettings>>,
    /// Package names whose download hosts may use the proxy.
    proxy_packages: Vec<String>,
    /// Listed packages that had to download without a configured proxy.
    missing_proxy: Mutex<BTreeSet<String>>,
}

impl Downloader {
    pub fn new(store: Store) -> Downloader {
        let proxy = crate::proxy::ProxySettings::default();
        let proxy_state = Arc::new(RwLock::new(proxy.clone()));
        let agent = Self::build_agent("Glue/1.0", proxy_state.clone());
        Downloader {
            agent,
            store,
            workers: DEFAULT_WORKERS,
            user_agent: "Glue/1.0".to_string(),
            gh_proxies: Vec::new(),
            parallel_download: true,
            parallel_min_bytes: GRAB_STYLE_MAX_BYTES + 1,
            parallel_min_chunk_size: MIN_PARALLEL_CHUNK_SIZE,
            direct_stream_max_bytes: DIRECT_STREAM_MAX_BYTES,
            cancel_direct_max_bytes: CANCEL_DIRECT_MAX_BYTES,
            proxy_state,
            proxy_packages: Vec::new(),
            missing_proxy: Mutex::new(BTreeSet::new()),
        }
    }

    /// Statuses ≥ 400 surface as responses (not errors) so resume can see 416
    /// and mirror fallback can rank them —.Client`.
    fn build_agent(
        user_agent: &str,
        proxy: Arc<RwLock<crate::proxy::ProxySettings>>,
    ) -> ureq::Agent {
        // Ureq 3 keeps the semantics natively when `http_status_as_error`
        // is switched off (it defaults to true): HTTP statuses come back as
        // plain responses, so the ureq 2 `Err(Status(_, resp)) -> Ok(resp)`
        // unwrapping layer is gone. Explicitly off (not default-true):
        // resume must see 416 and mirror fallback must rank statuses, like an
        // HTTP client that never errors on status.
        //
        // ureq's own proxy stays `None`: proxying happens in our schannel
        // connector (CONNECT tunnel + TLS to the target inside it), which is
        // the only place that can wrap a `native_tls` stream. TLS is likewise
        // performed there (OS root store, no `webpki-root-certs` table),
        // keeping `glue.exe` small.
        let config = ureq::Agent::config_builder()
            .timeout_connect(Some(Duration::from_secs(30)))
            .timeout_recv_response(Some(Duration::from_secs(60 * 5)))
            .timeout_recv_body(Some(Duration::from_secs(60 * 5)))
            .user_agent(user_agent)
            .max_idle_connections(100)
            .max_idle_connections_per_host(MAX_PARALLEL_CONNECTIONS as usize)
            .http_status_as_error(false)
            .proxy(None)
            .build();
        ureq::Agent::with_parts(
            config,
            SchannelConnector::with_shared(proxy),
            DefaultResolver::default(),
        )
    }

    /// Short-timeout agent for the mirror probe: it must fail fast instead of
    /// inheriting the 30 s connect / 5 min body budgets used for real
    /// downloads.
    fn build_probe_agent(
        user_agent: &str,
        proxy: Arc<RwLock<crate::proxy::ProxySettings>>,
    ) -> ureq::Agent {
        let config = ureq::Agent::config_builder()
            .timeout_connect(Some(Duration::from_secs(3)))
            .timeout_recv_response(Some(Duration::from_secs(4)))
            .timeout_recv_body(Some(Duration::from_secs(4)))
            .http_status_as_error(false)
            .proxy(None)
            .user_agent(user_agent)
            .build();
        ureq::Agent::with_parts(
            config,
            SchannelConnector::with_shared(proxy),
            DefaultResolver::default(),
        )
    }

    pub fn store(&self) -> &Store {
        &self.store
    }

    pub fn workers(&self) -> u32 {
        self.workers
    }

    /// Set parallel range download worker count (2–16, step 2).
    pub fn set_workers(&mut self, n: u32) {
        self.workers = normalize_user_workers(n);
    }

    pub fn set_user_agent(&mut self, ua: &str) {
        self.user_agent = ua.to_string();
        self.agent = Self::build_agent(&self.user_agent, self.proxy_state.clone());
    }

    /// Replace the HTTP proxy settings and rebuild both agents.
    ///
    /// Resolve the value with [`crate::proxy::resolve`]; an empty
    /// [`crate::proxy::ProxySettings`] connects directly.
    pub fn set_proxy(&mut self, proxy: crate::proxy::ProxySettings) {
        *self
            .proxy_state
            .write()
            .unwrap_or_else(|poisoned| poisoned.into_inner()) = proxy;
        self.agent = Self::build_agent(&self.user_agent, self.proxy_state.clone());
    }

    /// Packages whose download hosts may use the proxy.
    ///
    /// With a non-empty list the proxy is restricted to the hosts registered
    /// by [`Self::allow_proxy_for_package`]; an empty list keeps the proxy
    /// global.
    pub fn set_proxy_packages(&mut self, packages: Vec<String>) {
        self.proxy_packages = packages;
        let mut state = self
            .proxy_state
            .write()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        state.restrict = !self.proxy_packages.is_empty() && !state.is_empty();
    }

    /// Let `package`'s download hosts use the proxy when it is listed in
    /// `proxy_packages`. Called right before a package's transfer, so the
    /// host list is known.
    pub fn allow_proxy_for_package(&self, package: &str, urls: &[String]) -> PackageProxy {
        if self.proxy_packages.is_empty()
            || !config::package_uses_proxy(&self.proxy_packages, package)
        {
            return PackageProxy::NotListed;
        }
        let mut state = self
            .proxy_state
            .write()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if state.is_empty() {
            let name = package.rsplit('/').next().unwrap_or(package).to_string();
            if let Ok(mut missing) = self.missing_proxy.lock() {
                missing.insert(name);
            }
            return PackageProxy::MissingProxy;
        }
        for url in urls {
            if let Ok(parsed) = url::Url::parse(url) {
                if let Some(host) = parsed.host_str() {
                    state.add_proxy_host(host);
                }
            }
        }
        PackageProxy::Configured
    }

    /// Packages listed in `proxy_packages` that downloaded without a proxy.
    pub fn missing_proxy_packages(&self) -> Vec<String> {
        self.missing_proxy
            .lock()
            .map(|set| set.iter().cloned().collect())
            .unwrap_or_default()
    }

    /// Replace GitHub mirror prefixes used for downloads.
    /// Use [`config::load_proxies`] to match glue config and environment.
    pub fn set_github_proxies(&mut self, proxies: &[String]) {
        self.gh_proxies = proxies.to_vec();
    }

    /// Enable or disable parallel range downloads for files above 64 MiB.
    pub fn set_parallel_download(&mut self, enabled: bool) {
        self.parallel_download = enabled;
    }

    /// Set the direct-stream ceiling for cancellable tasks (0 disables it).
    pub fn set_cancel_direct_max_bytes(&mut self, max_bytes: u64) {
        self.cancel_direct_max_bytes = max_bytes;
    }

    // ------------------------------------------------------------------
    // ZIP member index (zip_member_index.go / zip_blob.go)
    // ------------------------------------------------------------------

    /// Persist relPath→hash member map for fast reinstall
    pub fn save_zip_member_index(
        &self,
        zip_hash: &str,
        files: &std::collections::BTreeMap<String, String>,
        total_bytes: u64,
    ) -> std::io::Result<()> {
        crate::zip::save(self.store.path(), zip_hash, files, total_bytes)
    }

    /// Link-ready member map when the index and the store blobs match
    /// stale indexes are removed.
    pub fn resolve_zip_member_index(
        &self,
        zip_hash: &str,
    ) -> Option<(std::collections::BTreeMap<String, String>, u64)> {
        let idx = crate::zip::load(self.store.path(), zip_hash)?;
        if !crate::zip::ready(&self.store, &idx) {
            crate::zip::remove(self.store.path(), zip_hash);
            return None;
        }
        Some((idx.files, idx.total_bytes))
    }

    /// Cached install byte total from the member index
    pub fn zip_index_total_bytes(&self, zip_hash: &str) -> u64 {
        crate::zip::load(self.store.path(), zip_hash)
            .map(|i| i.total_bytes)
            .unwrap_or(0)
    }

    /// Attach index data to a result when the zip's member index is
    /// link-ready.
    fn attach_zip_member_index(&self, result: &mut DownloadResult, zip_hash: &str) {
        if let Some((files, total)) = self.resolve_zip_member_index(zip_hash) {
            result.files = Some(files);
            result.zip_total_bytes = total;
        }
    }

    /// Override parallel thresholds
    #[cfg(test)]
    pub(crate) fn set_parallel_thresholds(&mut self, min_bytes: u64, min_chunk_size: u64) {
        self.parallel_min_bytes = min_bytes;
        self.parallel_min_chunk_size = min_chunk_size;
    }

    /// Override the direct-stream ceiling.
    #[cfg(test)]
    pub(crate) fn set_direct_stream_max_bytes(&mut self, max_bytes: u64) {
        self.direct_stream_max_bytes = max_bytes;
    }

    fn effective_parallel_min_bytes(&self) -> u64 {
        self.parallel_min_bytes
    }

    fn effective_min_chunk_size(&self) -> u64 {
        self.parallel_min_chunk_size
    }

    fn effective_direct_stream_max_bytes(&self) -> u64 {
        self.direct_stream_max_bytes
    }

    /// Parallel range connections for a file of `total` bytes.
    fn parallel_workers_for_size(&self, total: u64) -> u32 {
        parallel_workers_for_size(
            total,
            self.workers,
            self.effective_parallel_min_bytes(),
            self.effective_min_chunk_size(),
        )
    }

    /// URL(s) that will be tried for a canonical download URL.
    pub fn resolve_download_urls(&self, canonical: &str) -> Vec<String> {
        self.urls_for_task(canonical)
    }

    /// Measure `url`'s throughput in bytes per second (first-run mirror probe).
    ///
    /// Reads at most `PROBE_MAX_BYTES` of the response into memory — nothing
    /// enters the store — and stops after `PROBE_BUDGET`, so a dead or slow
    /// mirror can never hold the CLI hostage.
    pub fn probe_speed(&self, url: &str) -> Result<f64, DownloadError> {
        self.probe_speed_with(url, self.proxy_state.clone())
    }

    /// Read a small document (a few KiB) through the current route.
    ///
    /// For answers that are text rather than artifacts: the benchmark's
    /// environment check asks an echo service what address and carrier the
    /// outside world sees, and that reply never belongs in the store. The
    /// request goes through the same agent, proxy settings and timeouts as a
    /// download, so what it reports is what a download would experience.
    pub fn fetch_text(&self, url: &str, max_bytes: u64) -> Result<String, DownloadError> {
        let agent = Self::build_probe_agent(&self.user_agent, self.proxy_state.clone());
        let resp = agent
            .get(url)
            .call()
            .map_err(|e| DownloadError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        if status != 200 {
            return Err(DownloadError::Http {
                status,
                status_text: status_text(status),
            });
        }
        let mut body = Vec::new();
        resp.into_body()
            .into_reader()
            .take(max_bytes)
            .read_to_end(&mut body)
            .map_err(|e| DownloadError::Transport(e.to_string()))?;
        Ok(String::from_utf8_lossy(&body).into_owned())
    }

    /// Measure `url` through `proxy` explicitly, ignoring the downloader's
    /// current route. Used to compare direct access with a configured proxy.
    pub fn probe_speed_via(
        &self,
        url: &str,
        proxy: &crate::proxy::ProxySettings,
    ) -> Result<f64, DownloadError> {
        self.probe_speed_with(url, Arc::new(RwLock::new(proxy.clone())))
    }

    fn probe_speed_with(
        &self,
        url: &str,
        proxy: Arc<RwLock<crate::proxy::ProxySettings>>,
    ) -> Result<f64, DownloadError> {
        const PROBE_MAX_BYTES: u64 = 512 * 1024;
        const PROBE_BUDGET: Duration = Duration::from_secs(5);

        let agent = Self::build_probe_agent(&self.user_agent, proxy);
        let start = Instant::now();
        let resp = agent
            .get(url)
            .call()
            .map_err(|e| DownloadError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        if status != 200 {
            return Err(DownloadError::Http {
                status,
                status_text: status_text(status),
            });
        }
        let mut reader = resp.into_body().into_reader().take(PROBE_MAX_BYTES);
        let mut buf = vec![0u8; 64 * 1024];
        let mut total: u64 = 0;
        loop {
            match reader.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => {
                    total += n as u64;
                    if start.elapsed() >= PROBE_BUDGET {
                        break;
                    }
                }
                Err(e) if e.kind() == ErrorKind::Interrupted => continue,
                Err(e) => return Err(DownloadError::Transport(e.to_string())),
            }
        }
        if total == 0 {
            return Err(DownloadError::Transport("empty response".to_string()));
        }
        let secs = start.elapsed().as_secs_f64().max(0.001);
        Ok(total as f64 / secs)
    }

    fn urls_for_task(&self, canonical: &str) -> Vec<String> {
        if self.gh_proxies.is_empty() {
            return vec![canonical.to_string()];
        }
        config::mirror_urls(canonical, &self.gh_proxies)
    }

    /// Candidate URLs for one chunk: the source the caller picked first, then
    /// the other mirrors of the same artifact in configured order.
    ///
    /// Chunk retries walk this list, so one dead mirror no longer costs the
    /// whole session. Every entry serves the same asset; the manifest hash
    /// still verifies the assembled file.
    fn chunk_mirrors(&self, canonical: &str, primary: &str) -> Vec<String> {
        let mut out: Vec<String> = Vec::new();
        if !primary.is_empty() {
            out.push(primary.to_string());
        }
        for url in self.urls_for_task(canonical) {
            if !out.iter().any(|existing| existing == &url) {
                out.push(url);
            }
        }
        if out.is_empty() {
            out.push(canonical.to_string());
        }
        out
    }

    fn partial_paths(&self, task: &Task) -> (PathBuf, PathBuf) {
        let id = partial_id(&task.url);
        let dir = self.store.path().join(".partial");
        (
            dir.join(format!("{id}.part")),
            dir.join(format!("{id}.meta.json")),
        )
    }

    fn has_partial_resume(&self, task: &Task) -> bool {
        let (part_path, _) = self.partial_paths(task);
        std::fs::metadata(part_path)
            .map(|m| m.len() > 0)
            .unwrap_or(false)
    }

    /// Look up a CAS blob by manifest hash (direct SHA-256 or alias).
    fn lookup_store_blob(&self, task: &Task) -> Option<String> {
        if task.hash_value.is_empty() {
            // No manifest hash to look up: the download has to happen.
            crate::extractor::trace_line(&format!(
                "store miss for {}: manifest carries no hash",
                task.filename
            ));
            return None;
        }
        if manifest_uses_store_hash(&task.hash_algo) {
            if self.store.has(&task.hash_value) {
                return Some(task.hash_value.clone());
            }
            // The blob is content-addressed by the manifest's own digest, so a
            // miss means this exact artifact was never ingested — usually a
            // manifest whose hash changed for the same version.
            crate::extractor::trace_line(&format!(
                "store miss for {}: {}:{} not in the store",
                task.filename, task.hash_algo, task.hash_value
            ));
            return None;
        }
        let Some(cas_hash) =
            load_manifest_hash_alias(self.store.path(), &task.hash_algo, &task.hash_value)
        else {
            crate::extractor::trace_line(&format!(
                "store miss for {}: no alias for {}:{}",
                task.filename, task.hash_algo, task.hash_value
            ));
            return None;
        };
        if self.store.has(&cas_hash) {
            Some(cas_hash)
        } else {
            crate::extractor::trace_line(&format!(
                "store miss for {}: alias {} is not in the store",
                task.filename, cas_hash
            ));
            None
        }
    }

    /// Download checking the store first. `skip_store_reuse` forces a network
    /// download even when the blob already exists (e.g. `glue install --force`).
    ///
    /// `progress` is forwarded to [`Downloader::download`]; a store hit never
    /// reports byte progress (nothing is transferred)
    /// cache path returns before the progress counter is created.
    ///
    /// A [`Task::cancel`] token, when set, is polled by the transfer loops; a
    /// cancelled download returns [`DownloadError::Cancelled`] and leaves the
    /// resumable `.part` file in place for the next attempt.
    /// Whether the content store already holds this task's artifact, so an
    /// install can skip the network entirely.
    pub fn is_cached(&self, task: &Task) -> bool {
        self.lookup_store_blob(task).is_some()
    }

    pub fn download_with_cache(
        &self,
        mut task: Task,
        expected_hash: &str,
        skip_store_reuse: bool,
        progress: ProgressFn,
    ) -> DownloadResult {
        // taskWithExpectedHash
        if task.hash_value.is_empty() && !expected_hash.is_empty() {
            task.hash_value = expected_hash.to_string();
            if task.hash_algo.is_empty() {
                task.hash_algo = "sha256".to_string();
            }
        }
        if task_cancelled(&task) {
            let mut result = DownloadResult::new(task);
            result.error = Some(DownloadError::Cancelled);
            return result;
        }
        if !skip_store_reuse {
            if let Some(cas_hash) = self.lookup_store_blob(&task) {
                let mut result = DownloadResult::new(task);
                result.from_store = true;
                if let Ok(m) = std::fs::metadata(self.store.object_path(&cas_hash)) {
                    result.size = m.len();
                }
                result.hash = cas_hash.clone();
                if crate::manifest::should_native_zip_ingest(
                    &result.task.filename,
                    &result.task.url,
                ) {
                    self.attach_zip_member_index(&mut result, &cas_hash);
                }
                return result;
            }
        }
        self.download(&task, progress)
    }

    /// Download a single task with resumable partial files.
    pub fn download(&self, task: &Task, progress: ProgressFn) -> DownloadResult {
        if task_cancelled(task) {
            let mut result = DownloadResult::new(task.clone());
            result.error = Some(DownloadError::Cancelled);
            return result;
        }
        // Total size learned by the single-GET probe (0 when unknown), carried
        // into the `.partial` path so a large-file parallel download can skip a
        // redundant size HEAD round-trip to the CDN.
        //
        // A cancellable task (one carrying a stop token) only takes the direct
        // stream when the response is small enough (`cancel_direct_max_bytes`,
        // default 16 MiB). Above that ceiling it falls back to the `.partial`
        // path so a pause keeps every transferred byte resumable; the headers
        // already fetched carry the size forward, replacing the size HEAD.
        let direct_max = if task.cancel.is_some() {
            self.cancel_direct_max_bytes
        } else {
            self.effective_direct_stream_max_bytes()
        };
        let known_total = if direct_max == 0 {
            0
        } else {
            match self.try_direct_stream(task, progress, direct_max) {
                Ok(DirectStream::Downloaded(result)) => {
                    if result.error.is_none() {
                        return result;
                    }
                    // Body-phase failure (transport mid-body or a hash mismatch):
                    // the GET succeeded, so the remaining mirrors were never
                    // tried. Fall through to the resumable path, which walks
                    // them. GET-phase failures return `Err` below after every
                    // mirror was already tried, so those stay hard errors.
                    result.size
                }
                Ok(DirectStream::Fallback { known_total: total }) => total,
                Err(result) => return result,
            }
        };

        let mut result = DownloadResult::new(task.clone());
        let (part_path, meta_path) = self.partial_paths(task);
        let net_start = Instant::now();
        match self.download_to_partial(task, known_total, progress) {
            Ok(total_size) => {
                result.timing.add_network(net_start.elapsed());
                // No progress here: the bytes are already on disk and this pass
                // only hashes + moves the .part into the CAS (no second copy).
                // Reporting it would restart the consumer's bar at 0% right
                // after it hit 100%.
                let ingest_start = Instant::now();
                let ingested = self.ingest_partial_file(task, &part_path, total_size);
                result.timing.add_store_ingest(ingest_start.elapsed());
                match ingested {
                    Ok(hash) => {
                        result.hash = hash.clone();
                        remove_partial(&part_path, &meta_path);
                        if crate::manifest::should_native_zip_ingest(&task.filename, &task.url) {
                            self.attach_zip_member_index(&mut result, &hash);
                        }
                    }
                    Err(e) => {
                        result.error = Some(e);
                        return result;
                    }
                }
                result.size = std::fs::metadata(self.store.object_path(&result.hash))
                    .map(|m| m.len())
                    .unwrap_or(total_size.max(result.size));
            }
            Err(e) => {
                result.timing.add_network(net_start.elapsed());
                result.error = Some(e);
            }
        }
        result
    }

    /// Single GET streamed straight into the store when size is known and
    /// ≤ `DIRECT_STREAM_MAX_BYTES`. [`DirectStream::Fallback`] falls back to
    /// the `.partial` path, carrying any size learned from the probe response
    /// so the large-file path can skip a redundant size HEAD.
    ///
    /// Returns the failed `DownloadResult` directly as the error; boxing it
    /// would only add an allocation.
    #[allow(clippy::result_large_err)]
    fn try_direct_stream(
        &self,
        task: &Task,
        progress: ProgressFn,
        max_bytes: u64,
    ) -> std::result::Result<DirectStream, DownloadResult> {
        if crate::ftp::is_ftp_url(&task.url) {
            return Ok(DirectStream::Fallback { known_total: 0 });
        }
        if self.has_partial_resume(task) {
            return Ok(DirectStream::Fallback { known_total: 0 });
        }
        let urls = self.urls_for_task(&task.url);
        let request_start = Instant::now();
        let resp = match self.do_get_with_fallback(&urls, None) {
            Ok(r) => r,
            Err(e) => {
                return Err(DownloadResult {
                    task: task.clone(),
                    hash: String::new(),
                    size: 0,
                    error: Some(e),
                    from_store: false,
                    files: None,
                    zip_total_bytes: 0,
                    timing: Timing {
                        network: request_start.elapsed(),
                        store_ingest: Duration::ZERO,
                    },
                })
            }
        };
        let status = resp.status().as_u16();
        if status != 200 {
            let err = DownloadError::Http {
                status,
                status_text: status_text(status),
            };
            return Err(DownloadResult {
                task: task.clone(),
                hash: String::new(),
                size: 0,
                error: Some(err),
                from_store: false,
                files: None,
                zip_total_bytes: 0,
                timing: Timing {
                    network: request_start.elapsed(),
                    store_ingest: Duration::ZERO,
                },
            });
        }
        let content_length = header_u64(&resp, "Content-Length").unwrap_or(0);
        if content_length == 0 {
            return Ok(DirectStream::Fallback { known_total: 0 });
        }
        if content_length > max_bytes {
            // Carry the size forward so the parallel path skips a size HEAD.
            return Ok(DirectStream::Fallback {
                known_total: content_length,
            });
        }
        let reader = resp.into_body().into_reader();
        // Direct stream pipelines the store write with the socket read, so the
        // whole span is network time and `store_ingest` stays zero. Start the
        // clock before the request: `download_ms` must cover DNS/TCP/TLS,
        // redirects and response headers too, not only the body.
        let ingested = self.ingest_reader(task, reader, content_length, progress);
        let net_elapsed = request_start.elapsed();
        match ingested {
            Ok(hash) => {
                let size = std::fs::metadata(self.store.object_path(&hash))
                    .map(|m| m.len())
                    .unwrap_or(content_length);
                let mut result = DownloadResult {
                    task: task.clone(),
                    hash: hash.clone(),
                    size,
                    error: None,
                    from_store: false,
                    files: None,
                    zip_total_bytes: 0,
                    timing: Timing {
                        network: net_elapsed,
                        store_ingest: Duration::ZERO,
                    },
                };
                if crate::manifest::should_native_zip_ingest(&task.filename, &task.url) {
                    self.attach_zip_member_index(&mut result, &hash);
                }
                Ok(DirectStream::Downloaded(result))
            }
            Err(e) => Ok(DirectStream::Downloaded(DownloadResult {
                task: task.clone(),
                hash: String::new(),
                size: content_length,
                error: Some(e),
                from_store: false,
                files: None,
                zip_total_bytes: 0,
                timing: Timing {
                    network: net_elapsed,
                    store_ingest: Duration::ZERO,
                },
            })),
        }
    }

    /// Stream a reader into the store, teeing into the manifest digest for
    /// non-SHA-256 algorithms, then verify + record alias.
    fn ingest_reader(
        &self,
        task: &Task,
        reader: impl Read,
        total_size: u64,
        progress: ProgressFn,
    ) -> std::result::Result<String, DownloadError> {
        let tee = if !task.hash_value.is_empty() && !manifest_uses_store_hash(&task.hash_algo) {
            Some(ManifestHasher::new(&task.hash_algo)?)
        } else {
            None
        };
        // Bound the body by `Content-Length`: a pooled keep-alive connection
        // does not signal EOF right after the last byte, so an unbounded read
        // waits for the server's idle timeout (measured ~59 s on this machine)
        // before finalizing every download. `take` returns EOF at the exact
        // byte count; unknown-length (chunked) bodies still read to the end.
        let limited = if total_size > 0 { total_size } else { u64::MAX };
        let mut reader = ProgressReader {
            inner: TeeReader {
                inner: reader.take(limited),
                hasher: tee,
            },
            progress,
            filename: Arc::from(task.filename.as_str()),
            total: total_size,
            offset: 0,
            cancel: task.cancel.as_ref(),
        };
        if let Some(cb) = progress {
            cb(&Progress {
                filename: reader.filename.clone(),
                total: total_size,
                offset: 0,
            });
        }
        let hash = match self.store.write(&mut reader) {
            Ok(hash) => hash,
            // A cancelled stream surfaces as an interrupted I/O error from the
            // store's read loop; report it as what it is.
            Err(_) if task_cancelled(task) => return Err(DownloadError::Cancelled),
            Err(e) => return Err(DownloadError::Store(e)),
        };
        let hasher = reader.inner.hasher.take();

        // A body shorter than its declared length ended early: a dropped
        // connection or a proxy that closed mid-transfer. The manifest hash
        // catches that only when the manifest carries one; the size check
        // catches it always, and it runs first so the reason is the transfer
        // rather than the digest. `Incomplete` is classified as retryable, so
        // the download restarts instead of handing a truncated file to the
        // extractor — a 55 MB 7z self-extractor cut short is otherwise a
        // "cannot open the file as [7z] archive" two steps downstream.
        if total_size > 0 && reader.offset != total_size {
            let _ = self.store.delete(&hash);
            return Err(DownloadError::Incomplete {
                got: reader.offset,
                expected: total_size,
            });
        }

        // finalizeIngest: verify, discard on mismatch, record alias.
        if !task.hash_value.is_empty() {
            let verify_result = if manifest_uses_store_hash(&task.hash_algo) {
                verify_manifest_digest(&task.hash_algo, &task.hash_value, &hash)
            } else {
                match hasher {
                    Some(h) => {
                        verify_manifest_digest(&task.hash_algo, &task.hash_value, &h.finish())
                    }
                    None => Ok(()),
                }
            };
            if let Err(e) = verify_result {
                let _ = self.store.delete(&hash);
                return Err(e);
            }
            let _ = save_manifest_hash_alias(
                self.store.path(),
                &task.hash_algo,
                &task.hash_value,
                &hash,
            );
        }
        Ok(hash)
    }

    /// Ingest a completed `.part` file into the CAS without re-copying it.
    ///
    /// `download_to_partial` already wrote the bytes to disk exactly once
    /// (with resume support and, for large files, parallel range connections).
    /// Re-streaming that file through [`Store::write`] would read it back and
    /// write a *second* copy into a temp file before renaming — pure wasted IO.
    /// Instead we hash the `.part` in a single pass (teed through the manifest
    /// digest for non-SHA-256 algorithms, mirroring [`Downloader::ingest_reader`]),
    /// verify against the expected manifest hash, then rename the `.part`
    /// straight into its content-addressed slot.
    ///
    /// Verification happens *before* the rename so a digest mismatch leaves the
    /// `.part` on disk for retry (parity with the previous
    /// `store.write` + `store.delete` path).
    fn ingest_partial_file(
        &self,
        task: &Task,
        part_path: &std::path::Path,
        total_size: u64,
    ) -> std::result::Result<String, DownloadError> {
        let tee = if !task.hash_value.is_empty() && !manifest_uses_store_hash(&task.hash_algo) {
            Some(ManifestHasher::new(&task.hash_algo)?)
        } else {
            None
        };
        let file = match std::fs::File::open(part_path) {
            Ok(f) => f,
            Err(e) => return Err(DownloadError::OpenDownloaded(e)),
        };
        let mut reader = ProgressReader {
            inner: TeeReader {
                inner: file,
                hasher: tee,
            },
            progress: None,
            filename: Arc::from(task.filename.as_str()),
            total: total_size,
            offset: 0,
            cancel: task.cancel.as_ref(),
        };
        // Hash-only pass: the `.part` bytes are already on disk, so we never
        // write them a second time. `store.hash_reader` reads + hashes; the
        // TeeReader concurrently folds the manifest digest for non-SHA-256.
        let hash = match self.store.hash_reader(&mut reader) {
            Ok(hash) => hash,
            Err(_) if task_cancelled(task) => return Err(DownloadError::Cancelled),
            Err(e) => return Err(DownloadError::Store(e)),
        };
        let hasher = reader.inner.hasher.take();

        // finalizeIngest: verify BEFORE consuming the partial file, so a
        // mismatch leaves it available for resume/retry.
        if !task.hash_value.is_empty() {
            let verify_result = if manifest_uses_store_hash(&task.hash_algo) {
                verify_manifest_digest(&task.hash_algo, &task.hash_value, &hash)
            } else {
                match hasher {
                    Some(h) => {
                        verify_manifest_digest(&task.hash_algo, &task.hash_value, &h.finish())
                    }
                    None => Ok(()),
                }
            };
            verify_result?;
            let _ = save_manifest_hash_alias(
                self.store.path(),
                &task.hash_algo,
                &task.hash_value,
                &hash,
            );
        }

        // Move the `.part` file into the CAS keyed by its hash (same-volume
        // rename; cross-volume copy fallback is handled inside `Store`).
        self.store.ingest_move(part_path, &hash)?;
        Ok(hash)
    }

    /// Download into the resumable .part file, retrying a dropped transfer.
    /// Each retry re-reads the partial state from disk, so it continues from
    /// the bytes that landed (or the chunk record of a parallel session).
    fn download_to_partial(
        &self,
        task: &Task,
        known_total: u64,
        progress: ProgressFn,
    ) -> std::result::Result<u64, DownloadError> {
        for attempt in 1..=TRANSFER_ATTEMPTS {
            match self.download_to_partial_once(task, known_total, progress) {
                Ok(total) => return Ok(total),
                Err(e) => {
                    if attempt == TRANSFER_ATTEMPTS
                        || !is_retryable_transfer_error(&e)
                        || task_cancelled(task)
                    {
                        return Err(e);
                    }
                    // The consumer keeps drawing the last sample while the
                    // backoff runs; the next byte event resumes from the offset
                    // on disk, so the bar does not restart at zero.
                    std::thread::sleep(retry_download_delay(attempt));
                }
            }
        }
        // Unreachable: the last iteration returns either Ok or Err.
        Err(DownloadError::NoUrls)
    }

    /// One attempt at the resumable `.part` path. `known_total` (the size
    /// learned by the direct-stream probe, 0 when unknown) seeds the partial
    /// state so the parallel-fresh path can skip a redundant size HEAD
    /// round-trip to the CDN.
    fn download_to_partial_once(
        &self,
        task: &Task,
        known_total: u64,
        progress: ProgressFn,
    ) -> std::result::Result<u64, DownloadError> {
        // A cancelled download stays resumable even when the server does not
        // support ranges: the plain GET restarts at byte zero, so the bytes on
        // disk must be re-requested to stay verifiable. The work is kept, not
        // the progress position — better than throwing the file away.
        if task_cancelled(task) {
            // A paused download hands its bytes to the next attempt; nothing
            // is removed.
            return Err(DownloadError::Cancelled);
        }
        let (part_path, meta_path) = self.partial_paths(task);
        if let Some(dir) = part_path.parent() {
            std::fs::create_dir_all(dir).map_err(DownloadError::PartialDir)?;
        }
        if crate::ftp::is_ftp_url(&task.url) {
            // FTP has no range machinery: one straight pass into the partial
            // file, sidecar written by the callee.
            remove_partial(&part_path, &meta_path);
            let total = crate::ftp::download_ftp_to_partial(
                &task.url,
                &part_path,
                &task.filename,
                progress,
            )?;
            save_partial_meta(
                &meta_path,
                &PartialMeta {
                    url: task.url.clone(),
                    source_url: task.url.clone(),
                    total_size: total,
                    ..PartialMeta::default()
                },
            )
            .map_err(DownloadError::SaveMeta)?;
            return Ok(total);
        }

        let mut st = PartialState {
            part_path: part_path.clone(),
            meta_path: meta_path.clone(),
            task_url: task.url.clone(),
            meta: PartialMeta {
                url: task.url.clone(),
                ..Default::default()
            },
            offset: 0,
        };
        // loadPartialState: reconcile sidecar + bytes on disk.
        if let Ok(existing) = load_partial_meta(&meta_path) {
            if existing.url == task.url {
                st.meta = existing;
            } else {
                st.reset();
            }
        }
        if let Ok(m) = std::fs::metadata(&part_path) {
            st.offset = m.len();
        }
        if st.offset > 0 && st.meta.source_url.is_empty() {
            if !self.gh_proxies.is_empty() && config::is_github_url(&task.url) {
                // Cannot resume across mirror vs direct hosts: discard.
                st.reset();
            } else {
                st.meta.source_url = task.url.clone();
            }
        }
        // Carry the probe-learned size into the partial state so the parallel
        // path reaches `probe_parallel_target(known > 0)` and skips the size
        // HEAD (reuses a known total the same way).
        // Only a fresh download lands here with a known size (resume falls back
        // with `known_total == 0`), so overwriting an unset meta total is safe.
        // An interrupted parallel session already knows its planned size.
        if known_total > 0 && st.meta.total_size == 0 && st.meta.planned_total == 0 {
            st.meta.total_size = known_total;
        }
        if st.complete() {
            return Ok(st.meta.total_size);
        }

        // A parallel session that never finished: its `.part` is preallocated
        // to the full size with holes, so the file length says nothing about
        // what is really there — the persisted chunk ranges do. Fetch only the
        // gaps instead of starting the file over.
        if st.meta.planned_total > 0 && st.meta.total_size == 0 {
            if st.offset == st.meta.planned_total {
                match self.resume_parallel_session(task, &mut st, progress) {
                    Ok(total) => return Ok(total),
                    Err(e) => {
                        // A server that ignores Range makes the preallocated
                        // file unusable; anything else keeps the record.
                        if ranges_unusable(&e) {
                            st.reset();
                        }
                        return Err(e);
                    }
                }
            } else {
                // Killed between recording the plan and preallocating: the
                // bytes on disk are not a prefix of anything.
                st.reset();
            }
        }

        let urls = urls_for_resume(&task.url, &self.gh_proxies, &st.meta.source_url, st.offset);
        // Large transfers only (this is the `.part` path): start from whichever
        // mirror answers fastest instead of waiting out the slow ones in order.
        let urls = self.prefer_fastest_mirror(urls);
        let mut last_err: Option<DownloadError> = None;
        for (i, fetch_url) in urls.iter().enumerate() {
            if task_cancelled(task) {
                return Err(DownloadError::Cancelled);
            }
            if i > 0 {
                // A mirror can only serve us from byte zero.
                st.reset();
            }
            // Large artifacts go over parallel range connections when the
            // server honours Range; failures fall through to single-connection.
            match self.try_parallel_download(task, &mut st, fetch_url, progress) {
                Ok(Some(total)) => return Ok(total),
                Ok(None) => {}
                // Cancelled: keep the `.part` (and its chunk record) for the
                // next attempt instead of discarding the bytes.
                Err(e) => return Err(e),
            }
            match self.fetch_to_partial_url(task, fetch_url, &mut st, progress) {
                Ok(total) => {
                    st.commit(fetch_url, total);
                    return Ok(total);
                }
                Err(e) => {
                    if matches!(e, DownloadError::Cancelled) {
                        return Err(e);
                    }
                    last_err = Some(e);
                    if st.offset > 0 {
                        // Resuming pins us to the source URL the bytes came
                        // from; no other mirror left to try.
                        break;
                    }
                }
            }
        }
        Err(last_err.unwrap_or(DownloadError::NoUrls))
    }

    /// Single-connection GET into the partial file, appending on resume.
    fn fetch_to_partial_url(
        &self,
        task: &Task,
        fetch_url: &str,
        st: &mut PartialState,
        progress: ProgressFn,
    ) -> std::result::Result<u64, DownloadError> {
        // A 416 to a ranged request restarts the download from zero — exactly
        // once. A server that answers 416 to the plain GET too is broken or
        // hostile: restarting again could never succeed (this used to be an
        // unbounded tail recursion, i.e. a remote stack-overflow crash).
        let mut restarted = false;
        loop {
            if task_cancelled(task) {
                return Err(DownloadError::Cancelled);
            }
            let offset = st.offset;
            let range_header = if offset > 0 {
                Some(format!("bytes={offset}-"))
            } else {
                None
            };
            let resp = self.get_with_retries(fetch_url, range_header.as_deref())?;

            let content_range = header_str(&resp, "Content-Range");
            let content_length = header_u64(&resp, "Content-Length");
            let outcome = resolve_response(
                resp.status().as_u16(),
                content_range.as_deref(),
                content_length,
                st.meta.total_size,
                offset,
            )?;
            let (write_offset, total_size) = match outcome {
                ResponseOutcome::Restart => {
                    if restarted {
                        return Err(DownloadError::Http {
                            status: 416,
                            status_text: status_text(416),
                        });
                    }
                    restarted = true;
                    // Server refused our range: the partial is useless, start over.
                    st.reset();
                    continue;
                }
                ResponseOutcome::ShortRange { total_size } => return Ok(total_size),
                ResponseOutcome::Write { offset, total_size } => (offset, total_size),
            };

            st.meta.total_size = total_size;
            if total_size > 0 {
                st.commit(fetch_url, total_size);
            }

            let final_size = self.append_response_body(
                &st.part_path,
                resp.into_body().into_reader(),
                write_offset,
                task,
                total_size,
                progress,
            )?;
            if st.meta.total_size == 0 {
                st.meta.total_size = final_size;
            }
            save_partial_meta(&st.meta_path, &st.meta).map_err(DownloadError::SaveMeta)?;
            if st.meta.total_size > 0 && final_size != st.meta.total_size {
                return Err(DownloadError::Incomplete {
                    got: final_size,
                    expected: st.meta.total_size,
                });
            }
            return Ok(st.meta.total_size);
        }
    }

    /// Append (or restart) the response body in the partial file; returns the
    /// final file size.
    fn append_response_body(
        &self,
        part_path: &std::path::Path,
        body: impl Read,
        offset: u64,
        task: &Task,
        total: u64,
        progress: ProgressFn,
    ) -> std::result::Result<u64, DownloadError> {
        use std::io::{Seek, SeekFrom, Write};
        let mut file = std::fs::OpenOptions::new()
            .create(true)
            .write(true)
            // Keep any already-downloaded prefix: resume seeks to `offset`, and
            // a fresh download truncates explicitly below via `set_len(0)`.
            .truncate(false)
            .open(part_path)
            .map_err(DownloadError::OpenPartial)?;
        if offset > 0 {
            file.seek(SeekFrom::Start(offset))
                .map_err(DownloadError::SeekPartial)?;
        } else {
            file.set_len(0).map_err(DownloadError::TruncatePartial)?;
            file.seek(SeekFrom::Start(0))
                .map_err(DownloadError::SeekPartial)?;
        }
        let mut reader = ProgressReader {
            // Bound by the advertised length: a keep-alive connection does not
            // report EOF after the final byte, so an unbounded read waits for
            // the server's idle timeout (~59 s measured) before returning.
            inner: body.take(if total > offset {
                total - offset
            } else {
                u64::MAX
            }),
            progress,
            filename: Arc::from(task.filename.as_str()),
            total,
            offset,
            cancel: task.cancel.as_ref(),
        };
        let mut buf = [0u8; COPY_BUFFER_SIZE];
        let mut written: u64 = 0;
        loop {
            if task_cancelled(task) {
                let _ = file.sync_all(); // flush what landed for resume
                return Err(DownloadError::Cancelled);
            }
            match reader.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => {
                    if let Err(e) = file.write_all(&buf[..n]) {
                        let _ = file.sync_all(); // flush what landed for resume
                        return Err(DownloadError::WritePartial(e));
                    }
                    written += n as u64;
                }
                Err(e) => {
                    let _ = file.sync_all();
                    if task_cancelled(task) {
                        return Err(DownloadError::Cancelled);
                    }
                    return Err(DownloadError::Transport(e.to_string()));
                }
            }
        }
        file.sync_all().map_err(DownloadError::SyncPartial)?;
        Ok(offset + written)
    }

    /// GET with mirror fallback: first URL that answers 200/206 wins.
    /// Put the mirror that answers a one-byte range probe first.
    ///
    /// Probes run concurrently, so a list whose fastest mirror is third no
    /// longer pays two timeouts before the transfer starts. Ties keep the
    /// configured order, and the other candidates keep theirs, so fallback
    /// still walks every mirror. A list with one URL (the common case when no
    /// mirror is configured) costs nothing.
    fn prefer_fastest_mirror(&self, urls: Vec<String>) -> Vec<String> {
        if urls.len() < 2 {
            return urls;
        }
        let agent = Self::build_probe_agent(&self.user_agent, self.proxy_state.clone());
        let (tx, rx) = std::sync::mpsc::channel::<usize>();
        for (index, url) in urls.iter().enumerate() {
            let agent = agent.clone();
            let url = url.clone();
            let tx = tx.clone();
            // Detached on purpose: a mirror that never answers must not hold
            // the download. The probe agent's short timeouts end it.
            std::thread::spawn(move || {
                let mut req = agent.get(&url);
                req = req.header("Range", "bytes=0-0");
                if let Ok(resp) = req.call() {
                    let status = resp.status().as_u16();
                    if status == 200 || status == 206 {
                        let _ = tx.send(index);
                    }
                }
            });
        }
        drop(tx);
        /// Longest wait for a winner before falling back to the configured
        /// order; a probe that loses the race is ignored. Only multi-mirror
        /// lists pay this, and the probes are one-byte range requests.
        const PROBE_WAIT: Duration = Duration::from_secs(2);
        let winner = match rx.recv_timeout(PROBE_WAIT) {
            Ok(index) => index,
            Err(_) => return urls,
        };
        if winner == 0 {
            return urls;
        }
        let mut reordered = Vec::with_capacity(urls.len());
        reordered.push(urls[winner].clone());
        for (index, url) in urls.iter().enumerate() {
            if index != winner {
                reordered.push(url.clone());
            }
        }
        reordered
    }

    fn do_get_with_fallback(
        &self,
        urls: &[String],
        range: Option<&str>,
    ) -> std::result::Result<http::Response<ureq::Body>, DownloadError> {
        let mut last_err: Option<DownloadError> = None;
        for u in urls {
            match self.get_with_retries(u, range) {
                Ok(resp) if resp.status().as_u16() == 200 || resp.status().as_u16() == 206 => {
                    return Ok(resp)
                }
                Ok(resp) => {
                    let status = resp.status().as_u16();
                    last_err = Some(DownloadError::HttpAt {
                        status,
                        status_text: status_text(status),
                        url: u.clone(),
                    });
                }
                Err(e) => last_err = Some(e),
            }
        }
        match last_err {
            Some(e) => Err(DownloadError::AllUrlsFailed(e.to_string())),
            None => Err(DownloadError::NoUrls),
        }
    }

    /// Single GET with bounded retries and exponential backoff.
    fn get_with_retries(
        &self,
        url: &str,
        range: Option<&str>,
    ) -> std::result::Result<http::Response<ureq::Body>, DownloadError> {
        let mut last_err: Option<DownloadError> = None;
        // Sleep before each attempt after the first. A transient status can
        // shorten or lengthen that wait through `Retry-After`.
        let mut delay = Duration::ZERO;
        for attempt in 1..=DOWNLOAD_RETRY_ATTEMPTS {
            if !delay.is_zero() {
                std::thread::sleep(delay);
            }
            let mut req = self.agent.get(url);
            if let Some(r) = range {
                req = req.header("Range", r);
            }
            match req.call() {
                // 4xx/5xx arrive as plain responses (`http_status_as_error`
                // is explicitly off in build_agent), exactly
                // http.Client — resume sees 416 and mirror fallback ranks
                // statuses without any unwrapping.
                Ok(resp) => {
                    let status = resp.status().as_u16();
                    if attempt < DOWNLOAD_RETRY_ATTEMPTS && is_retryable_status(status) {
                        delay = retry_after_delay(&resp)
                            .unwrap_or_else(|| retry_download_delay(attempt));
                        last_err = Some(DownloadError::Http {
                            status,
                            status_text: status_text(status),
                        });
                        // Dropping releases the pooled connection; holding the
                        // response through the sleep would keep it checked out.
                        drop(resp);
                        continue;
                    }
                    return Ok(resp);
                }
                Err(e) => {
                    let retryable = should_retry(&e, attempt);
                    last_err = Some(DownloadError::Transport(e.to_string()));
                    if !retryable || attempt == DOWNLOAD_RETRY_ATTEMPTS {
                        break;
                    }
                    delay = retry_download_delay(attempt);
                }
            }
        }
        Err(last_err.unwrap_or(DownloadError::NoUrls))
    }

    /// Remove resumable .part and .meta.json files for a download task.
    pub fn clear_partial(&self, task: &Task) {
        let (part_path, meta_path) = self.partial_paths(task);
        remove_partial(&part_path, &meta_path);
    }

    /// Remove every in-progress download under the store. Returns how many
    /// files were deleted and total bytes freed.
    pub fn clear_all_partials(&self) -> std::io::Result<(u64, u64)> {
        let dir = self.store.path().join(".partial");
        let entries = match std::fs::read_dir(&dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok((0, 0)),
            Err(e) => return Err(e),
        };
        let mut removed = 0u64;
        let mut freed = 0u64;
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                continue;
            }
            let size = entry.metadata().map(|m| m.len()).unwrap_or(0);
            if std::fs::remove_file(&path).is_ok() {
                removed += 1;
                freed += size;
            }
        }
        Ok((removed, freed))
    }

    /// Remove obsolete `.zip-index` and `.manifest-hash` sidecar entries whose
    /// target store blobs no longer exist, plus leftover `*.tmp` files in
    /// those directories (`ClearStaleSidecarIndexes` / `sidecar_gc.go`).
    ///
    /// Returns `(removed, freed_bytes)`; a missing sidecar directory is not an
    /// error.
    pub fn clear_stale_sidecar_indexes(&self) -> std::io::Result<(u64, i64)> {
        let root = self.store.path();
        let (mut removed, mut freed) = clear_stale_zip_indexes(&self.store, root)?;
        let (n, b) = clear_stale_manifest_hash_aliases(&self.store, root)?;
        removed += n;
        freed += b;
        Ok((removed, freed))
    }
}

/// Sidecar GC for `.zip-index`: drop `*.tmp` leftovers and any `*.json` whose
/// zip blob is gone or whose member index is no longer link-ready.
fn clear_stale_zip_indexes(
    store: &Store,
    store_root: &std::path::Path,
) -> std::io::Result<(u64, i64)> {
    let dir = store_root.join(".zip-index");
    let entries = match std::fs::read_dir(&dir) {
        Ok(e) => e,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok((0, 0)),
        Err(e) => return Err(e),
    };
    let mut removed = 0u64;
    let mut freed = 0i64;
    for entry in entries.flatten() {
        let meta = match entry.metadata() {
            Ok(m) => m,
            Err(_) => continue,
        };
        if meta.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let size = meta.len() as i64;

        // A `.tmp` file is always stale (interrupted atomic write).
        let mut stale = name.ends_with(".tmp");
        if !stale && name.ends_with(".json") {
            let zip_hash = name.strip_suffix(".json").unwrap_or("");
            stale = if zip_hash.is_empty() || !store.has(zip_hash) {
                true
            } else {
                match crate::zip::load(store_root, zip_hash) {
                    Some(idx) => !crate::zip::ready(store, &idx),
                    None => true,
                }
            };
        } else if !stale {
            // Any other file in the sidecar dir is unrecognized → stale.
            stale = true;
        }
        if stale && std::fs::remove_file(entry.path()).is_ok() {
            removed += 1;
            freed += size;
        }
    }
    Ok((removed, freed))
}

/// Sidecar GC for `.manifest-hash`: drop `*.tmp` leftovers and any `*.json`
/// alias pointing at a missing blob (or unreadable/malformed).
fn clear_stale_manifest_hash_aliases(
    store: &Store,
    store_root: &std::path::Path,
) -> std::io::Result<(u64, i64)> {
    let dir = store_root.join(".manifest-hash");
    let entries = match std::fs::read_dir(&dir) {
        Ok(e) => e,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok((0, 0)),
        Err(e) => return Err(e),
    };
    let mut removed = 0u64;
    let mut freed = 0i64;
    for entry in entries.flatten() {
        let meta = match entry.metadata() {
            Ok(m) => m,
            Err(_) => continue,
        };
        if meta.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let size = meta.len() as i64;
        let path = entry.path();

        let mut stale = name.ends_with(".tmp");
        if !stale && name.ends_with(".json") {
            stale = match std::fs::read(&path) {
                Ok(data) => match serde_json::from_slice::<ManifestHashEntry>(&data) {
                    Ok(alias) => alias.cas_hash.is_empty() || !store.has(&alias.cas_hash),
                    Err(_) => true,
                },
                Err(_) => true,
            };
        } else if !stale {
            stale = true;
        }
        if stale && std::fs::remove_file(&path).is_ok() {
            removed += 1;
            freed += size;
        }
    }
    Ok((removed, freed))
}

/// Resume URL list: pinned to the recorded source URL when resuming from a
/// non-zero offset (offsets are not portable across mirrors); otherwise the
/// fresh-download mirror list.
fn urls_for_resume(
    canonical: &str,
    proxies: &[String],
    source_url: &str,
    offset: u64,
) -> Vec<String> {
    if offset > 0 && !source_url.is_empty() {
        return vec![source_url.to_string()];
    }
    if proxies.is_empty() {
        return vec![canonical.to_string()];
    }
    config::mirror_urls(canonical, proxies)
}

// ---------------------------------------------------------------------------
// Parallel range downloads (parallel.go)
// ---------------------------------------------------------------------------

/// Inclusive byte range.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
struct ByteRange {
    start: u64,
    end: u64,
}

/// Split `[rangeStart, rangeEnd]` (inclusive) into up to `workers` ranges.
/// Returns fewer than 2 chunks (or none) when a single connection is better.
fn plan_chunks_range(
    range_start: u64,
    range_end: u64,
    workers: u32,
    min_chunk_size: u64,
) -> Option<Vec<ByteRange>> {
    if workers < 2 || range_end < range_start {
        return None;
    }
    let span = range_end - range_start + 1;
    if span < min_chunk_size * 2 {
        return None;
    }
    let max_chunks = (span / min_chunk_size) as u32;
    if max_chunks < 2 {
        return None;
    }
    let n = workers.min(max_chunks);
    let chunk_size = span / n as u64;
    let mut ranges = Vec::with_capacity(n as usize);
    let mut off = range_start;
    for i in 0..n {
        let mut end = off + chunk_size - 1;
        if i == n - 1 {
            end = range_end;
        }
        ranges.push(ByteRange { start: off, end });
        off = end + 1;
    }
    Some(ranges)
}

/// Parallel range connections for a file of `total` bytes
///
/// `base` is the configured ceiling (`download_workers`, default
/// [`DEFAULT_WORKERS`]): the effective count grows with the artifact size but
/// never exceeds it, so a user who pins 2 keeps 2 and a 2 GiB package gets
/// more of the link than a 100 MiB one.
///
/// Kept as explicit early-return + sequential caps rather than `clamp` so each
/// branch maps 1:1 to the source (the `total < min_bytes` fast path must
/// return `base` *unclamped*).
#[allow(clippy::manual_clamp)]
fn parallel_workers_for_size(total: u64, base: u32, min_bytes: u64, min_chunk_size: u64) -> u32 {
    let mut base = base.max(1);
    if total < min_bytes {
        return base;
    }
    let max_chunks = (total / min_chunk_size) as u32;
    if max_chunks < 2 {
        return base;
    }
    let by_size = (total / BYTES_PER_PARALLEL_CONNECTION)
        .clamp(MIN_SIZE_WORKERS as u64, MAX_PARALLEL_CONNECTIONS as u64) as u32;
    if base > by_size {
        base = by_size;
    }
    if base > max_chunks {
        base = max_chunks;
    }
    if base > MAX_PARALLEL_CONNECTIONS {
        base = MAX_PARALLEL_CONNECTIONS;
    }
    if base < 2 {
        base = 2;
    }
    base
}

/// Whether the remaining bytes justify a parallel resume
fn should_parallel_resume(total: u64, offset: u64) -> bool {
    if total <= offset {
        return false;
    }
    total - offset >= MIN_PARALLEL_RESUME_BYTES
}

/// Worker-count-aware plan for a fresh download of `total` bytes. Test-only
/// in practice — the live path plans ranges straight from the probed total.
#[cfg(test)]
fn plan_chunks(
    total: u64,
    workers: u32,
    min_bytes: u64,
    min_chunk_size: u64,
) -> Option<Vec<ByteRange>> {
    if total < min_bytes || workers < 2 {
        return None;
    }
    let w = parallel_workers_for_size(total, workers, min_bytes, min_chunk_size);
    plan_chunks_range(0, total - 1, w, min_chunk_size)
}

impl Downloader {
    /// Size and range support from one `Range: bytes=0-0` request.
    ///
    /// A 206 carries the full size in `Content-Range` and proves range
    /// support; a 200 means the server ignored the range, so its
    /// `Content-Length` is the total. This replaces the former HEAD + Range
    /// pair, halving the probe round trips on a high-latency link.
    fn probe_url_for_parallel(&self, url: &str) -> (u64, bool) {
        match self.get_with_retries(url, Some("bytes=0-0")) {
            Ok(resp) => {
                let is_206 = resp.status().as_u16() == 206;
                let total = if is_206 {
                    header_str(&resp, "Content-Range")
                        .and_then(|cr| parse_content_range(&cr))
                        .map(|(_, _, total)| total)
                        .unwrap_or(0)
                } else {
                    header_u64(&resp, "Content-Length").unwrap_or(0)
                };
                // Drain only the probe byte. A 200 that ignored Range may
                // advertise the whole file; leaving the rest unread closes
                // the connection instead of downloading the artifact twice.
                let expected = header_u64(&resp, "Content-Length").unwrap_or(0);
                let limit = if expected > 0 { expected.min(64) } else { 64 };
                let mut reader = resp.into_body().into_reader().take(limit);
                let _ = std::io::copy(&mut reader, &mut std::io::sink());
                (total, is_206)
            }
            Err(_) => (0, false),
        }
    }

    /// Fetch the artifact over multiple byte-range connections when it is
    /// large enough and the server honours ranges.
    ///
    /// `Ok(None)` means "a single connection is the better/only option now"
    /// (too small, no range support, or not enough chunks); a transient
    /// failure is returned as `Err` with the `.part` and its chunk record
    /// intact, so the next attempt fetches only the missing ranges.
    /// `Err(Cancelled)` behaves the same way.
    fn try_parallel_download(
        &self,
        task: &Task,
        st: &mut PartialState,
        fetch_url: &str,
        progress: ProgressFn,
    ) -> std::result::Result<Option<u64>, DownloadError> {
        if !self.parallel_download || self.workers <= 1 {
            return Ok(None);
        }
        if st.offset > 0 {
            self.try_parallel_resume(task, st, fetch_url, progress)
        } else {
            self.try_parallel_fresh(task, st, fetch_url, progress)
        }
    }

    fn try_parallel_fresh(
        &self,
        task: &Task,
        st: &mut PartialState,
        fetch_url: &str,
        progress: ProgressFn,
    ) -> std::result::Result<Option<u64>, DownloadError> {
        // A known-small file will never use ranges: skip the probe entirely.
        // The direct-stream headers already supplied the size for the common
        // CLI path, so this removes the last round trip before the transfer.
        if st.meta.total_size > 0 && st.meta.total_size < self.effective_parallel_min_bytes() {
            return Ok(None);
        }
        let (total, range_ok) = self.probe_parallel_target(fetch_url, st.meta.total_size);
        if !range_ok || total < self.effective_parallel_min_bytes() {
            return Ok(None);
        }
        let mirrors = self.chunk_mirrors(&task.url, fetch_url);
        match self.download_parallel_range_to_partial(
            task,
            fetch_url,
            &mirrors,
            st,
            total,
            0,
            Vec::new(),
            progress,
        ) {
            Ok((total_size, used_url)) => {
                st.commit(&used_url, total_size);
                Ok(Some(total_size))
            }
            Err(_) if task_cancelled(task) => {
                // Keep the chunk record: the next attempt resumes from it
                // instead of re-downloading the whole file.
                Err(DownloadError::Cancelled)
            }
            // Not enough chunks to bother: nothing was written, so a single
            // connection can take over without discarding anything.
            Err(DownloadError::ParallelNotEnoughChunks) => Ok(None),
            // Ranges are unusable on every mirror: the preallocated `.part`
            // can never complete that way, so drop it and go sequential.
            Err(e) if ranges_unusable(&e) => {
                st.reset();
                Ok(None)
            }
            // A transient failure (mirrors exhausted, body truncated): the
            // bytes on disk and the chunk record are the resume point for the
            // next attempt, so keep them instead of starting over.
            Err(e) => Err(e),
        }
    }

    fn try_parallel_resume(
        &self,
        task: &Task,
        st: &mut PartialState,
        fetch_url: &str,
        progress: ProgressFn,
    ) -> std::result::Result<Option<u64>, DownloadError> {
        let keep_bytes = st.offset;
        let mut total = st.meta.total_size;
        if total == 0 && st.offset >= MIN_PARALLEL_RESUME_BYTES {
            total = self.probe_content_length_for(&task.url);
        }
        if !should_parallel_resume(total, st.offset) {
            return Ok(None);
        }
        let (probed, range_ok) = self.probe_parallel_target(fetch_url, total);
        if !range_ok || probed == 0 {
            return Ok(None);
        }
        total = probed;
        let mirrors = self.chunk_mirrors(&task.url, fetch_url);
        match self.download_parallel_range_to_partial(
            task,
            fetch_url,
            &mirrors,
            st,
            total,
            keep_bytes,
            Vec::new(),
            progress,
        ) {
            Ok((total_size, used_url)) => {
                st.commit(&used_url, total_size);
                Ok(Some(total_size))
            }
            Err(_) if task_cancelled(task) => Err(DownloadError::Cancelled),
            Err(DownloadError::ParallelNotEnoughChunks) => Ok(None),
            // Ranges are unusable: keep the sequential prefix the
            // single-connection path can continue from.
            Err(e) if ranges_unusable(&e) => {
                st.abandon_plan(keep_bytes, total);
                Ok(None)
            }
            // Keep the chunk record: the next attempt fetches only the gaps.
            Err(e) => Err(e),
        }
    }

    /// Continue a parallel session that never finished.
    ///
    /// Its `.part` is preallocated to `planned_total` (holes everywhere) and
    /// `completed_chunks` names the ranges that really landed, so only the
    /// gaps are fetched. A pause, an app exit or a crash therefore costs at
    /// most the chunks that were in flight.
    fn resume_parallel_session(
        &self,
        task: &Task,
        st: &mut PartialState,
        progress: ProgressFn,
    ) -> std::result::Result<u64, DownloadError> {
        let total = st.meta.planned_total;
        let fetch_url = if st.meta.source_url.is_empty() {
            task.url.clone()
        } else {
            st.meta.source_url.clone()
        };
        let prior = std::mem::take(&mut st.meta.completed_chunks);
        let mirrors = self.chunk_mirrors(&task.url, &fetch_url);
        match self.download_parallel_range_to_partial(
            task, &fetch_url, &mirrors, st, total, 0, prior, progress,
        ) {
            Ok((total_size, used_url)) => {
                st.commit(&used_url, total_size);
                Ok(total_size)
            }
            Err(e) => Err(e),
        }
    }

    /// Size + range-support probe; `known_total` keeps the caller's size when
    /// the server's `Content-Range` is absent.
    fn probe_parallel_target(&self, fetch_url: &str, known_total: u64) -> (u64, bool) {
        let (probed, range_ok) = self.probe_url_for_parallel(fetch_url);
        if known_total > 0 {
            (known_total, range_ok)
        } else {
            (probed, range_ok)
        }
    }

    /// Download the missing chunks of `[completed_offset, total-1]` over range
    /// connections, writing into the partial file.
    ///
    /// `prior` is the chunk record of an interrupted session (empty on a first
    /// attempt): chunks it already covers are skipped, and the record is
    /// rewritten as each chunk completes — so a pause, an app exit or a crash
    /// costs at most the chunks that were in flight.
    ///
    /// The in-flight meta keeps `total_size` at 0 and names the full size in
    /// `planned_total`: the preallocated `.part` (full length, holes) can never
    /// pass for a finished download, and the next attempt knows which ranges
    /// are real.
    #[allow(clippy::too_many_arguments)]
    fn download_parallel_range_to_partial(
        &self,
        task: &Task,
        fetch_url: &str,
        mirrors: &[String],
        st: &mut PartialState,
        total: u64,
        completed_offset: u64,
        prior: Vec<CompletedChunk>,
        progress: ProgressFn,
    ) -> std::result::Result<(u64, String), DownloadError> {
        let workers = self.parallel_workers_for_size(total);
        let chunks = match plan_chunks_range(
            completed_offset,
            total - 1,
            workers,
            self.effective_min_chunk_size(),
        ) {
            Some(c) if c.len() >= 2 => c,
            _ => return Err(DownloadError::ParallelNotEnoughChunks),
        };

        let meta = PartialMeta {
            url: st.task_url.clone(),
            source_url: fetch_url.to_string(),
            planned_total: total,
            completed_chunks: prior,
            ..PartialMeta::default()
        };
        // The sequential prefix (a resumed `.part`) counts as present even
        // though it is not in the chunk record.
        let mut present = completed_offset;
        let mut pending: Vec<ByteRange> = Vec::with_capacity(chunks.len());
        for chunk in chunks {
            // Fetch only what the record does not vouch for: a previous
            // attempt may already hold a prefix of this chunk.
            let gaps = meta.uncovered(chunk);
            let missing: u64 = gaps.iter().map(|g| g.end - g.start + 1).sum();
            present += (chunk.end - chunk.start + 1) - missing;
            pending.extend(gaps);
        }
        let part_path = st.part_path.clone();
        let meta_path = st.meta_path.clone();
        if let Some(dir) = part_path.parent() {
            std::fs::create_dir_all(dir).map_err(DownloadError::PartialDir)?;
        }
        if pending.is_empty() {
            // Everything this session planned is already on disk.
            return Ok((total, fetch_url.to_string()));
        }

        // Mark the session BEFORE growing the file: the set_len below makes the
        // .part look complete (full length, zero holes), and a process killed
        // mid-download must not mistake it for a finished one — `complete()`
        // keys off the persisted total_size, which stays 0 here.
        save_partial_meta(&meta_path, &meta).map_err(DownloadError::SaveMeta)?;
        let file = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            // Existing bytes are preserved and the file is resized below.
            .truncate(false)
            .open(&part_path)
            .map_err(DownloadError::OpenPartial)?;
        let len = file.metadata().map(|m| m.len()).unwrap_or(0);
        if len != total {
            file.set_len(total)
                .map_err(DownloadError::AllocatePartial)?;
        }
        drop(file);

        let fname: Arc<str> = Arc::from(task.filename.as_str());
        if let Some(cb) = progress {
            cb(&Progress {
                filename: fname.clone(),
                total,
                offset: present,
            });
        }

        let completed = std::sync::atomic::AtomicU64::new(present);
        let first_error: std::sync::Mutex<Option<DownloadError>> = std::sync::Mutex::new(None);
        // The record the workers extend as bytes land; the granularity bounds
        // what an interruption can cost.
        let record: std::sync::Mutex<PartialMeta> = std::sync::Mutex::new(meta);
        let recorder = ChunkRecorder {
            record: &record,
            meta_path: &meta_path,
            granule: (total / 512).clamp(1 << 20, 8 << 20),
        };

        std::thread::scope(|scope| {
            for chunk in pending {
                if first_error.lock().unwrap().is_some() {
                    break;
                }
                let completed = &completed;
                let first_error = &first_error;
                let recorder = &recorder;
                // Each worker needs its own owned path: `part_path` is
                // consumed by every `move` closure.
                let part_path = part_path.clone();
                scope.spawn(move || {
                    if first_error.lock().unwrap().is_some() {
                        return;
                    }
                    match self.download_chunk(
                        mirrors,
                        &part_path,
                        chunk,
                        total,
                        task,
                        completed,
                        progress,
                        Some(recorder),
                    ) {
                        // `download_chunk` records the tail itself, so a
                        // finished range is on disk and in the meta.
                        Ok(()) => {}
                        Err(e) => {
                            let mut slot = first_error.lock().unwrap();
                            if slot.is_none() {
                                *slot = Some(e);
                            }
                        }
                    }
                });
            }
        });

        if let Some(e) = first_error.into_inner().unwrap() {
            // Leave the file and its chunk record in place: a cancelled run, a
            // pause, or a transient failure resumes from exactly here. Only a
            // server that cannot serve ranges at all makes the caller drop the
            // plan (`ranges_unusable`).
            return Err(e);
        }

        let size = std::fs::metadata(&part_path)
            .map_err(DownloadError::StatPartial)?
            .len();
        if size != total {
            return Err(DownloadError::ParallelIncomplete {
                got: size,
                want: total,
            });
        }
        // The caller commits the finished session, which clears the plan
        // fields in the meta.
        Ok((total, fetch_url.to_string()))
    }

    /// Fetch one byte range into the partial file.
    ///
    /// A failed attempt keeps what it already wrote and retries only the rest,
    /// walking the mirror list one entry per attempt: a reset connection, a
    /// truncated body or one dead mirror costs a retry instead of the whole
    /// chunk. Parameters stay flat; grouping them would obscure the 1:1
    /// correspondence for no real gain.
    #[allow(clippy::too_many_arguments)]
    fn download_chunk(
        &self,
        urls: &[String],
        part_path: &std::path::Path,
        chunk: ByteRange,
        total: u64,
        task: &Task,
        completed: &std::sync::atomic::AtomicU64,
        progress: ProgressFn,
        recorder: Option<&ChunkRecorder<'_>>,
    ) -> std::result::Result<(), DownloadError> {
        if urls.is_empty() {
            return Err(DownloadError::NoUrls);
        }
        // Hoisted out of the read loop: this closure fires once per 128 KiB
        // read, and the filename must not cost an allocation per event.
        let fname: Arc<str> = Arc::from(task.filename.as_str());
        let expected = chunk.end - chunk.start + 1;
        // Bytes of this chunk already on disk / already in the chunk record.
        let mut written: u64 = 0;
        let mut recorded: u64 = 0;
        let mut last_err: Option<DownloadError> = None;

        for attempt in 0..CHUNK_ATTEMPTS {
            if task_cancelled(task) {
                record_written(recorder, chunk.start, written, recorded);
                return Err(DownloadError::Cancelled);
            }
            if written >= expected {
                record_written(recorder, chunk.start, written, recorded);
                return Ok(());
            }
            if attempt > 0 {
                std::thread::sleep(retry_download_delay(attempt));
            }
            // One mirror per attempt; the last entry is reused if the list is
            // shorter than the attempt budget.
            let url = urls
                .get(attempt as usize)
                .unwrap_or_else(|| &urls[urls.len() - 1]);
            let start = chunk.start + written;
            let range = format!("bytes={start}-{}", chunk.end);
            let resp = match self.get_with_retries(url, Some(&range)) {
                Ok(resp) => resp,
                Err(e) => {
                    last_err = Some(DownloadError::Chunk {
                        start,
                        end: chunk.end,
                        message: e.to_string(),
                    });
                    continue;
                }
            };
            let status = resp.status().as_u16();
            if status != 206 {
                // 200 means this server ignores Range; the caller decides
                // whether ranges are unusable outright. Anything else is this
                // mirror failing, so try the next one.
                last_err = Some(DownloadError::ChunkHttp {
                    start,
                    end: chunk.end,
                    status,
                });
                continue;
            }
            // A 206 alone is not enough: the bytes are written at this chunk's
            // offsets, so a server answering with the WRONG range (or no
            // Content-Range at all) would plant displaced data that only a
            // manifest hash would catch — hash-less tasks must not trust it.
            match header_str(&resp, "Content-Range")
                .as_deref()
                .and_then(parse_content_range)
            {
                Some((got_start, got_end, _)) if got_start == start && got_end == chunk.end => {}
                _ => {
                    last_err = Some(DownloadError::Chunk {
                        start,
                        end: chunk.end,
                        message: format!(
                            "unexpected Content-Range for bytes={start}-{}",
                            chunk.end
                        ),
                    });
                    continue;
                }
            }

            use std::io::{Seek, SeekFrom, Write};
            let mut file = match std::fs::OpenOptions::new().write(true).open(part_path) {
                Ok(file) => file,
                Err(e) => {
                    return Err(DownloadError::Chunk {
                        start,
                        end: chunk.end,
                        message: e.to_string(),
                    })
                }
            };
            if let Err(e) = file.seek(SeekFrom::Start(start)) {
                return Err(DownloadError::Chunk {
                    start,
                    end: chunk.end,
                    message: e.to_string(),
                });
            }

            // Bounded by the range length; see `append_response_body` for why
            // an unbounded keep-alive read stalls for the server's idle
            // timeout.
            let mut reader = resp.into_body().into_reader().take(chunk.end - start + 1);
            let mut buf = [0u8; COPY_BUFFER_SIZE];
            let mut attempt_err: Option<DownloadError> = None;
            loop {
                if task_cancelled(task) {
                    // Persist what landed before stopping: the next attempt
                    // only has to fetch the rest of this range.
                    record_written(recorder, chunk.start, written, recorded);
                    return Err(DownloadError::Cancelled);
                }
                match reader.read(&mut buf) {
                    Ok(0) => break,
                    Ok(n) => {
                        if let Err(e) = file.write_all(&buf[..n]) {
                            // Local disk failure: retrying a mirror would hit
                            // it again.
                            record_written(recorder, chunk.start, written, recorded);
                            return Err(DownloadError::Chunk {
                                start: chunk.start + written,
                                end: chunk.end,
                                message: e.to_string(),
                            });
                        }
                        written += n as u64;
                        if let Some(rec) = recorder {
                            if written - recorded >= rec.granule {
                                rec.record(chunk.start + recorded, chunk.start + written - 1);
                                recorded = written;
                            }
                        }
                        let done = completed
                            .fetch_add(n as u64, std::sync::atomic::Ordering::Relaxed)
                            + n as u64;
                        if let Some(cb) = progress {
                            cb(&Progress {
                                filename: fname.clone(),
                                total,
                                offset: done,
                            });
                        }
                    }
                    Err(e) => {
                        attempt_err = Some(DownloadError::Chunk {
                            start,
                            end: chunk.end,
                            message: e.to_string(),
                        });
                        break;
                    }
                }
            }
            if written >= expected {
                // Record the whole range only once every byte is on disk.
                record_written(recorder, chunk.start, written, recorded);
                return Ok(());
            }
            // Keep what landed: the retry, and the next process, ask only for
            // the rest.
            record_written(recorder, chunk.start, written, recorded);
            recorded = written;
            last_err = attempt_err.or(Some(DownloadError::ChunkSizeMismatch {
                got: written,
                expected,
            }));
        }

        Err(last_err.unwrap_or(DownloadError::ChunkSizeMismatch {
            got: written,
            expected,
        }))
    }

    /// Content length probe over the mirror list, one `Range: 0-0` per URL.
    fn probe_content_length_for(&self, canonical_url: &str) -> u64 {
        for url in self.urls_for_task(canonical_url) {
            let (total, _) = self.probe_url_for_parallel(&url);
            if total > 0 {
                return total;
            }
        }
        0
    }
}

/// Reader wrapper emitting progress callbacks as bytes flow.
struct ProgressReader<'a, R> {
    inner: R,
    progress: ProgressFn<'a>,
    filename: Arc<str>,
    total: u64,
    offset: u64,
    /// Polled per read so a direct HTTP → store stream can be stopped
    /// mid-body; the resulting `Interrupted` I/O error is mapped back to
    /// [`DownloadError::Cancelled`] by the caller.
    cancel: Option<&'a CancelToken>,
}

impl<'a, R: Read> Read for ProgressReader<'a, R> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        if self.cancel.is_some_and(|token| token.is_cancelled()) {
            return Err(std::io::Error::new(
                ErrorKind::Interrupted,
                "download cancelled",
            ));
        }
        let n = self.inner.read(buf)?;
        if n > 0 {
            self.offset += n as u64;
            if let Some(cb) = self.progress {
                cb(&Progress {
                    filename: self.filename.clone(),
                    total: self.total,
                    offset: self.offset,
                });
            }
        }
        Ok(n)
    }
}

fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        out.push(HEX[(b >> 4) as usize] as char);
        out.push(HEX[(b & 0x0f) as usize] as char);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn workers_snapping() {
        // Ties round down (upper-n < n-lower is false when equal).
        assert_eq!(normalize_user_workers(0), 2);
        assert_eq!(normalize_user_workers(1), 2);
        assert_eq!(normalize_user_workers(2), 2);
        assert_eq!(normalize_user_workers(3), 2);
        assert_eq!(normalize_user_workers(4), 4);
        assert_eq!(normalize_user_workers(5), 4);
        assert_eq!(normalize_user_workers(6), 6);
        assert_eq!(normalize_user_workers(7), 6);
        assert_eq!(normalize_user_workers(8), 8);
        assert_eq!(normalize_user_workers(15), 14);
        assert_eq!(normalize_user_workers(16), 16);
        assert_eq!(normalize_user_workers(99), MAX_USER_WORKERS);
    }

    #[test]
    fn store_hash_algo_detection() {
        assert!(manifest_uses_store_hash(""));
        assert!(manifest_uses_store_hash("sha256"));
        assert!(manifest_uses_store_hash("SHA2"));
        assert!(!manifest_uses_store_hash("sha1"));
        assert!(!manifest_uses_store_hash("md5"));
        assert!(!manifest_uses_store_hash("sha512"));
    }

    #[test]
    fn content_range_parsing() {
        assert_eq!(
            parse_content_range("bytes 500-999/1000"),
            Some((500, 999, 1000))
        );
        assert_eq!(parse_content_range("bytes 0-0/*"), Some((0, 0, 0)));
        assert_eq!(parse_content_range("Bytes 1-2/3"), None);
        assert_eq!(parse_content_range("bytes 1/3"), None);
        assert_eq!(parse_content_range(""), None);
    }

    #[test]
    fn resolve_response_matrix() {
        // 200 full body resets offset to zero.
        assert_eq!(
            resolve_response(200, None, Some(1000), 0, 500).unwrap(),
            ResponseOutcome::Write {
                offset: 0,
                total_size: 1000
            }
        );
        // 206 matching start resumes.
        assert_eq!(
            resolve_response(206, Some("bytes 500-999/1000"), None, 0, 500).unwrap(),
            ResponseOutcome::Write {
                offset: 500,
                total_size: 1000
            }
        );
        // 206 with mismatched start errors.
        assert!(matches!(
            resolve_response(206, Some("bytes 0-999/1000"), None, 0, 500),
            Err(DownloadError::UnexpectedRangeStart { .. })
        ));
        // 416 with known total already reached → short range (done).
        assert_eq!(
            resolve_response(416, None, None, 1000, 1000).unwrap(),
            ResponseOutcome::ShortRange { total_size: 1000 }
        );
        // 416 otherwise → restart from zero.
        assert_eq!(
            resolve_response(416, None, None, 0, 500).unwrap(),
            ResponseOutcome::Restart
        );
        // Other statuses surface as HTTP errors.
        assert!(matches!(
            resolve_response(404, None, None, 0, 0),
            Err(DownloadError::Http { status: 404, .. })
        ));
    }

    #[test]
    fn throttled_progress_suppresses_duplicates() {
        // First report always emits, a percentage change emits
        // immediately, and a same-percentage report inside the 150 ms window
        // is dropped (ProgressReader fires on every 128 KiB read).
        let seen: Mutex<Vec<(u64, u64)>> = Mutex::new(Vec::new());
        let sink = |p: &Progress| seen.lock().unwrap().push((p.offset, p.total));
        let throttled = ThrottledProgress::new(&sink);
        // 1 MiB total so a 1-byte move stays at 0% (a 100-byte total would
        // already round up to 1% and legitimately count as a change).
        let at = |offset| Progress {
            filename: Arc::from("tool.zip"),
            total: 1024 * 1024,
            offset,
        };
        throttled.report(&at(0));
        throttled.report(&at(1)); // still 0% inside the window → dropped
        throttled.report(&at(512 * 1024)); // 50% → emitted despite the window
        throttled.flush(); // re-emits the last value
        let total = 1024 * 1024;
        assert_eq!(
            *seen.lock().unwrap(),
            vec![(0, total), (512 * 1024, total), (512 * 1024, total)]
        );
    }

    #[test]
    fn download_with_cache_forwards_byte_progress() {
        // The engine's CLI progress bar depends on this: the callback must
        // receive monotonically increasing offsets ending at the full size.
        let body = vec![7u8; 300 * 1024];
        let expected = body.clone();
        let (base, _h) = serve(move |_head, _range| (200, vec![], expected.clone()));
        let (dl, _dir) = temp_dl("progress");
        let seen: Mutex<Vec<(u64, u64)>> = Mutex::new(Vec::new());
        let cb = |p: &Progress| seen.lock().unwrap().push((p.offset, p.total));
        let task = Task {
            url: format!("{base}/blob.bin"),
            filename: "blob.bin".into(),
            ..Default::default()
        };
        let res = dl.download_with_cache(task, "", false, Some(&cb));
        assert!(res.error.is_none(), "err = {:?}", res.error);
        let seen = seen.lock().unwrap();
        assert!(seen.len() >= 2, "expected several reports, got {seen:?}");
        assert_eq!(seen.last().unwrap().0, body.len() as u64);
        assert!(
            seen.windows(2).all(|w| w[0].0 <= w[1].0),
            "offsets must not go backwards: {seen:?}"
        );
    }

    #[test]
    fn percentage_clamps_and_handles_unknown_total() {
        assert_eq!(percentage(0, 0), 0);
        assert_eq!(percentage(50, 0), 0);
        assert_eq!(percentage(50, 200), 25);
        assert_eq!(percentage(300, 200), 100);
    }

    #[test]
    fn retry_backoff() {
        // Pure curve — the override consulted by `retry_download_delay` is
        // deliberately bypassed so a concurrent failure-path test that zeroes
        // the backoff cannot make this assertion flake.
        assert_eq!(retry_download_delay_raw(0), Duration::ZERO);
        assert_eq!(retry_download_delay_raw(1), Duration::from_millis(500));
        assert_eq!(retry_download_delay_raw(2), Duration::from_secs(1));
        assert_eq!(retry_download_delay_raw(3), Duration::from_secs(2));
    }

    #[test]
    fn transient_dns_is_retried_once() {
        // A host that does not resolve gets one more attempt, then the mirror
        // fallback takes over instead of burning the whole retry budget.
        assert!(should_retry(&ureq::Error::HostNotFound, 1));
        assert!(!should_retry(&ureq::Error::HostNotFound, 2));
        // Other transport failures keep the full budget.
        assert!(should_retry(&ureq::Error::ConnectionFailed, 1));
        assert!(should_retry(&ureq::Error::ConnectionFailed, 3));
    }

    #[test]
    fn transient_tls_handshake_is_retried_but_certificate_errors_are_not() {
        assert!(is_retryable(&ureq::Error::Tls("schannel handshake failed")));
        assert!(!is_retryable(&ureq::Error::Tls(
            "schannel certificate validation failed"
        )));
    }

    #[test]
    fn partial_id_is_stable_and_16_bytes() {
        let id = partial_id("https://example.com/x.zip");
        assert_eq!(id.len(), 32);
        assert_eq!(id, partial_id("https://example.com/x.zip"));
        assert_ne!(id, partial_id("https://example.com/y.zip"));
    }

    #[test]
    fn verify_digest_case_insensitive() {
        assert!(verify_manifest_digest("sha256", "ABCD", "abcd").is_ok());
        assert!(verify_manifest_digest("sha256", "", "anything").is_ok());
        assert!(matches!(
            verify_manifest_digest("sha256", "abcd", "beef"),
            Err(DownloadError::HashMismatch { .. })
        ));
    }

    #[test]
    fn alias_roundtrip_and_sha256_skip() {
        let dir = TempDir::new("glue-alias-test");
        // sha256 never writes an alias.
        save_manifest_hash_alias(&dir, "sha256", "d", "c").unwrap();
        assert!(load_manifest_hash_alias(&dir, "sha256", "d").is_none());
        // sha1 round-trips, case-insensitively keyed.
        save_manifest_hash_alias(&dir, "SHA1", "ABCDEF", "cashash").unwrap();
        assert_eq!(
            load_manifest_hash_alias(&dir, "sha1", "abcdef").unwrap(),
            "cashash"
        );
    }

    #[test]
    fn urls_for_resume_pinning() {
        let canonical = "https://github.com/a/b/x.zip".to_string();
        let proxies = vec!["https://m.example".to_string()];
        // Resuming with recorded source → pinned.
        assert_eq!(
            urls_for_resume(&canonical, &proxies, "https://m.example/x", 100),
            vec!["https://m.example/x".to_string()]
        );
        // Fresh → mirror expansion.
        assert_eq!(
            urls_for_resume(&canonical, &proxies, "", 0),
            config::mirror_urls(&canonical, &proxies)
        );
        // No proxies → canonical only.
        assert_eq!(urls_for_resume(&canonical, &[], "", 0), vec![canonical]);
    }

    // ------------------------------------------------------------------
    // Parallel range planning (parallel.go)
    // ------------------------------------------------------------------

    #[test]
    fn parallel_workers_for_size_caps() {
        const MIN_BYTES: u64 = GRAB_STYLE_MAX_BYTES + 1;
        const MIN_CHUNK: u64 = MIN_PARALLEL_CHUNK_SIZE;
        assert_eq!(
            parallel_workers_for_size(50 << 20, 4, MIN_BYTES, MIN_CHUNK),
            4
        );
        assert_eq!(
            parallel_workers_for_size(80 << 20, 4, MIN_BYTES, MIN_CHUNK),
            4
        );
        assert_eq!(
            parallel_workers_for_size(400 << 20, 4, MIN_BYTES, MIN_CHUNK),
            4
        );
        assert_eq!(
            parallel_workers_for_size(600 << 20, 6, MIN_BYTES, MIN_CHUNK),
            6
        );
        // Huge file caps at 16 but never below 2.
        assert_eq!(
            parallel_workers_for_size(1 << 40, 128, MIN_BYTES, MIN_CHUNK),
            16
        );
        assert_eq!(
            parallel_workers_for_size(1 << 40, 1, MIN_BYTES, MIN_CHUNK),
            2
        );
        // Clamped by max chunks: 8 KiB at 4 KiB chunks → at most 2 workers.
        assert_eq!(parallel_workers_for_size(8192, 8, 4096, 4096), 2);
    }

    #[test]
    fn plan_chunks_rules() {
        const MIN_BYTES: u64 = GRAB_STYLE_MAX_BYTES + 1;
        const MIN_CHUNK: u64 = MIN_PARALLEL_CHUNK_SIZE;
        // Below the 64 MiB+1 threshold → single connection.
        assert!(plan_chunks(GRAB_STYLE_MAX_BYTES, 4, MIN_BYTES, MIN_CHUNK).is_none());
        assert!(plan_chunks(10, 4, MIN_BYTES, MIN_CHUNK).is_none());
        assert!(plan_chunks(1 << 30, 1, MIN_BYTES, MIN_CHUNK).is_none());
        let chunks = plan_chunks(1 << 30, 4, MIN_BYTES, MIN_CHUNK).unwrap();
        assert_eq!(chunks.len(), 4);
        assert_eq!(chunks[0].start, 0);
        assert_eq!(chunks.last().unwrap().end, (1 << 30) - 1);
        // Contiguous, no gaps.
        for pair in chunks.windows(2) {
            assert_eq!(pair[0].end + 1, pair[1].start);
        }
        // Each chunk at least one minimum chunk (4 MiB).
        for c in &chunks {
            assert!(c.end - c.start + 1 >= MIN_PARALLEL_CHUNK_SIZE);
        }
        // Span of exactly two minimum chunks still splits in two.
        let two = plan_chunks_range(0, MIN_CHUNK * 2 - 1, 4, MIN_CHUNK).unwrap();
        assert_eq!(two.len(), 2);
        assert_eq!(two[1].end, MIN_CHUNK * 2 - 1);
        // Range too small to split.
        assert!(plan_chunks_range(0, MIN_CHUNK * 2 - 2, 4, MIN_CHUNK).is_none());
        // Degenerate range.
        assert!(plan_chunks_range(5, 4, 4, MIN_CHUNK).is_none());
        assert!(plan_chunks_range(0, 100, 1, MIN_CHUNK).is_none());
        // Resume-style range.
        let total: u64 = 400 << 20;
        let offset = total * 56 / 100;
        let chunks = plan_chunks_range(offset, total - 1, 4, MIN_CHUNK).unwrap();
        assert!(chunks.len() >= 2);
        assert_eq!(chunks[0].start, offset);
        assert_eq!(chunks.last().unwrap().end, total - 1);
    }

    #[test]
    fn should_parallel_resume_threshold() {
        assert!(!should_parallel_resume(0, 0));
        assert!(!should_parallel_resume(100, 100));
        // One byte short of the 32 MiB minimum → single connection.
        assert!(!should_parallel_resume(MIN_PARALLEL_RESUME_BYTES + 1, 2));
        // Exactly 32 MiB remaining → parallel.
        assert!(should_parallel_resume(MIN_PARALLEL_RESUME_BYTES + 5, 5));
        assert!(should_parallel_resume(MIN_PARALLEL_RESUME_BYTES + 1, 0));
    }

    #[test]
    fn task_with_expected_hash_via_download_with_cache_reuse() {
        // A blob already in the store under its sha256 short-circuits HTTP.
        let dir = TempDir::new("glue-dl-cache-test");
        let store = Store::new(dir.path()).unwrap();
        let h = store.write(&b"cached!"[..]).unwrap();
        let dl = Downloader::new(Store::new(dir.path()).unwrap());
        let task = Task {
            url: "https://example.invalid/never".into(),
            filename: "never.bin".into(),
            ..Default::default()
        };
        let res = dl.download_with_cache(task, &h, false, None);
        assert!(res.error.is_none());
        assert!(res.from_store);
        assert_eq!(res.hash, h);
        assert_eq!(res.size, 7);
    }

    #[test]
    fn clear_all_partials_counts_and_frees() {
        let dir = TempDir::new("glue-dl-partials-test");
        let store = Store::new(dir.path()).unwrap();
        let partial = dir.join(".partial");
        std::fs::create_dir_all(&partial).unwrap();
        std::fs::write(partial.join("a.part"), b"12345").unwrap();
        std::fs::write(partial.join("a.meta.json"), b"{}").unwrap();
        let dl = Downloader::new(store);
        let (removed, freed) = dl.clear_all_partials().unwrap();
        assert_eq!(removed, 2);
        assert_eq!(freed, 7);
        // Empty dir → zeroes, missing dir → zeroes.
        let (removed, freed) = dl.clear_all_partials().unwrap();
        assert_eq!((removed, freed), (0, 0));
    }

    // ------------------------------------------------------------------
    // Loopback HTTP integration tests (no external network).
    // ------------------------------------------------------------------

    use std::io::{BufRead, BufReader, Write as _};
    use std::net::{TcpListener, TcpStream};

    /// Minimal HTTP/1.1 server: each connection gets a canned response built
    /// from the request head (path + Range header) by `handler`.
    fn serve<F>(handler: F) -> (String, std::thread::JoinHandle<()>)
    where
        F: Fn(&str, Option<&str>) -> (u16, Vec<(String, String)>, Vec<u8>) + Send + 'static,
    {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let base = format!("http://{addr}");
        let handle = std::thread::spawn(move || {
            loop {
                let Ok((stream, _)) = listener.accept() else {
                    break;
                };
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut head = String::new();
                let mut line = String::new();
                let mut range: Option<String> = None;
                loop {
                    line.clear();
                    if reader.read_line(&mut line).unwrap_or(0) == 0 {
                        break;
                    }
                    let trimmed = line.trim();
                    if trimmed.is_empty() {
                        break;
                    }
                    if let Some((hname, hval)) = trimmed.split_once(':') {
                        // Header names are case-insensitive (RFC 7230 3.2):
                        // ureq 3 serialises lowercase, older clients title-case.
                        if hname.eq_ignore_ascii_case("Range") {
                            range = Some(hval.trim().to_string());
                        }
                    }
                    head.push_str(trimmed);
                    head.push('\n');
                }
                if head.is_empty() {
                    continue;
                }
                let (status, headers, body) = handler(&head, range.as_deref());
                let reason = match status {
                    200 => "OK",
                    206 => "Partial Content",
                    404 => "Not Found",
                    416 => "Range Not Satisfiable",
                    _ => "X",
                };
                let mut resp = format!("HTTP/1.1 {status} {reason}\r\n");
                let mut has_len = false;
                for (k, v) in &headers {
                    if k.eq_ignore_ascii_case("Content-Length") {
                        has_len = true;
                    }
                    resp.push_str(&format!("{k}: {v}\r\n"));
                }
                if !has_len {
                    resp.push_str(&format!("Content-Length: {}\r\n", body.len()));
                }
                resp.push_str("Connection: close\r\n\r\n");
                let mut w = stream;
                let _ = w.write_all(resp.as_bytes());
                // HEAD responses carry headers only (Content-Length still
                // describes the full body).
                if !head.starts_with("HEAD ") {
                    let _ = w.write_all(&body);
                }
                let _ = w.flush();
            }
        });
        (base, handle)
    }

    /// Minimal CONNECT proxy: records every `CONNECT host:port`, dials that
    /// target, answers 200 and pipes bytes both ways.
    fn serve_connect_proxy(
        connects: std::sync::Arc<std::sync::Mutex<Vec<String>>>,
    ) -> (String, std::thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let base = format!("http://{addr}");
        let handle = std::thread::spawn(move || loop {
            let Ok((mut stream, _)) = listener.accept() else {
                break;
            };
            let connects = std::sync::Arc::clone(&connects);
            std::thread::spawn(move || {
                let mut reader = BufReader::new(stream.try_clone().unwrap());
                let mut head = String::new();
                let mut line = String::new();
                loop {
                    line.clear();
                    if reader.read_line(&mut line).unwrap_or(0) == 0 {
                        return;
                    }
                    let trimmed = line.trim_end();
                    if trimmed.is_empty() {
                        break;
                    }
                    head.push_str(trimmed);
                    head.push('\n');
                }
                let Some(target) = head
                    .lines()
                    .next()
                    .filter(|line| line.starts_with("CONNECT "))
                    .and_then(|line| line.split_whitespace().nth(1))
                else {
                    return;
                };
                connects.lock().unwrap().push(target.to_string());
                let Ok(mut upstream) = TcpStream::connect(target) else {
                    let _ = (&stream).write_all(b"HTTP/1.1 502 Bad Gateway\r\n\r\n");
                    return;
                };
                let _ = (&stream).write_all(b"HTTP/1.1 200 Connection Established\r\n\r\n");
                let mut client_to_upstream = stream.try_clone().unwrap();
                let mut upstream_write = upstream.try_clone().unwrap();
                let down = std::thread::spawn(move || {
                    let _ = std::io::copy(&mut client_to_upstream, &mut upstream_write);
                });
                let _ = std::io::copy(&mut upstream, &mut stream);
                let _ = down.join();
            });
        });
        (base, handle)
    }

    #[test]
    fn download_uses_the_configured_connect_proxy() {
        let body = b"proxied download".to_vec();
        let expected = body.clone();
        let (target, _t) = serve(move |_head, _range| {
            (
                200,
                vec![("Content-Length".to_string(), expected.len().to_string())],
                expected.clone(),
            )
        });
        let host_port = target.trim_start_matches("http://").to_string();

        let connects = std::sync::Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
        let (proxy, _p) = serve_connect_proxy(std::sync::Arc::clone(&connects));
        let spec = crate::proxy::parse_proxy_url(&proxy).unwrap();

        let (mut dl, _dir) = temp_dl("proxy-connect");
        dl.set_proxy(crate::proxy::ProxySettings {
            http: Some(spec.clone()),
            https: Some(spec),
            no_proxy: Vec::new(),
            ..crate::proxy::ProxySettings::default()
        });
        let task = Task {
            url: format!("{target}/x.bin"),
            filename: "x.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(
            std::fs::read(dl.store().object_path(&res.hash)).unwrap(),
            body
        );
        assert_eq!(connects.lock().unwrap().as_slice(), &[host_port]);
    }

    #[test]
    fn no_proxy_bypasses_the_configured_proxy() {
        let body = b"direct".to_vec();
        let expected = body.clone();
        let (target, _t) = serve(move |_head, _range| {
            (
                200,
                vec![("Content-Length".to_string(), expected.len().to_string())],
                expected.clone(),
            )
        });

        let connects = std::sync::Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
        let (proxy, _p) = serve_connect_proxy(std::sync::Arc::clone(&connects));
        let spec = crate::proxy::parse_proxy_url(&proxy).unwrap();

        let (mut dl, _dir) = temp_dl("proxy-bypass");
        dl.set_proxy(crate::proxy::ProxySettings {
            http: Some(spec.clone()),
            https: Some(spec),
            no_proxy: vec!["127.0.0.1".to_string()],
            ..crate::proxy::ProxySettings::default()
        });
        let task = Task {
            url: format!("{target}/x.bin"),
            filename: "x.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert!(
            connects.lock().unwrap().is_empty(),
            "NO_PROXY must bypass the proxy"
        );
    }

    fn temp_dl(tag: &str) -> (Downloader, TempDir) {
        let dir = TempDir::new(&format!("glue-dl-http-{tag}"));
        let store = Store::new(dir.path()).unwrap();
        (Downloader::new(store), dir)
    }

    /// `proxy_packages` must add only the listed package's hosts, so an
    /// unrelated download in the same process still goes direct.
    #[test]
    fn proxy_packages_add_only_the_listed_packages_hosts() {
        let (mut dl, _dir) = temp_dl("proxy-packages");
        let spec = crate::proxy::parse_proxy_url("127.0.0.1:7897").unwrap();
        dl.set_proxy(crate::proxy::ProxySettings {
            http: Some(spec.clone()),
            https: Some(spec),
            ..crate::proxy::ProxySettings::default()
        });
        dl.set_proxy_packages(vec!["claude-code".to_string()]);
        dl.allow_proxy_for_package(
            "main/claude-code",
            &["https://storage.googleapis.com/x/claude.exe".to_string()],
        );
        {
            let state = dl.proxy_state.read().unwrap();
            assert!(state.restrict);
            assert!(state.allows_proxy("storage.googleapis.com"));
            assert!(!state.allows_proxy("github.com"));
        }

        // A package that is not listed adds nothing.
        dl.allow_proxy_for_package("git", &["https://github.com/x/git.zip".to_string()]);
        assert!(!dl.proxy_state.read().unwrap().allows_proxy("github.com"));

        // An empty list restores the proxy's global behaviour.
        dl.set_proxy_packages(Vec::new());
        assert!(!dl.proxy_state.read().unwrap().restrict);
    }

    /// A task whose token is already flipped never opens a connection: the
    /// entry check reports `Cancelled` before any transfer starts.
    #[test]
    fn pre_cancelled_task_never_starts_a_transfer() {
        let (dl, _dir) = temp_dl("cancel-early");
        let token = CancelToken::new();
        token.cancel();
        let task = Task {
            url: "http://127.0.0.1:1/never.bin".to_string(),
            filename: "never.bin".to_string(),
            cancel: Some(token),
            ..Task::default()
        };
        let result = dl.download_with_cache(task, "", false, None);
        assert!(matches!(result.error, Some(DownloadError::Cancelled)));
    }

    #[test]
    fn direct_stream_download_and_verify() {
        let body = b"hello downloader".to_vec();
        let sha = crate::hash::hash_bytes(&body);
        let expected = body.clone();
        let (base, _h) = serve(move |_head, _range| (200, vec![], expected.clone()));
        let (dl, _dir) = temp_dl("direct");
        let task = Task {
            url: format!("{base}/x.bin"),
            filename: "x.bin".into(),
            hash_algo: "sha256".into(),
            hash_value: sha.clone(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.hash, sha);
        assert_eq!(res.size, body.len() as u64);
        assert!(dl.store().has(&sha));
    }

    /// A cancellable small download takes the single-GET path: no HEAD and no
    /// Range probe. The probe round trips are what made the CLI pay an extra
    /// fixed cost per package on high-latency links.
    #[test]
    fn cancellable_small_download_uses_one_get() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        let body = b"small cancellable body".to_vec();
        let gets = Arc::new(AtomicUsize::new(0));
        let heads = Arc::new(AtomicUsize::new(0));
        let ranges = Arc::new(AtomicUsize::new(0));
        let payload = body.clone();
        let g = gets.clone();
        let h = heads.clone();
        let r = ranges.clone();
        let (base, _h) = serve(move |head, range| {
            if head.starts_with("HEAD ") {
                h.fetch_add(1, Ordering::Relaxed);
            } else if range.is_some() {
                r.fetch_add(1, Ordering::Relaxed);
            } else {
                g.fetch_add(1, Ordering::Relaxed);
            }
            (
                200,
                vec![("Content-Length".to_string(), payload.len().to_string())],
                payload.clone(),
            )
        });
        let (mut dl, _dir) = temp_dl("cancel-direct");
        dl.set_cancel_direct_max_bytes(1024 * 1024);
        let task = Task {
            url: format!("{base}/small.bin"),
            filename: "small.bin".into(),
            cancel: Some(CancelToken::new()),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(gets.load(Ordering::Relaxed), 1, "direct path is one GET");
        assert_eq!(heads.load(Ordering::Relaxed), 0, "no HEAD");
        assert_eq!(ranges.load(Ordering::Relaxed), 0, "no Range probe");

        // 0 disables the direct path: the same small body goes through the
        // resumable `.partial` path. Size is unknown, so one merged
        // `Range: 0-0` probe runs before the full GET — never a HEAD.
        gets.store(0, Ordering::Relaxed);
        heads.store(0, Ordering::Relaxed);
        ranges.store(0, Ordering::Relaxed);
        let (mut dl0, _dir0) = temp_dl("cancel-direct-off");
        dl0.set_cancel_direct_max_bytes(0);
        let task0 = Task {
            url: format!("{base}/small.bin"),
            filename: "small.bin".into(),
            cancel: Some(CancelToken::new()),
            ..Default::default()
        };
        let res0 = dl0.download(&task0, None);
        assert!(res0.error.is_none(), "err = {:?}", res0.error);
        assert_eq!(heads.load(Ordering::Relaxed), 0, "merged probe has no HEAD");
        assert_eq!(ranges.load(Ordering::Relaxed), 1, "one Range: 0-0 probe");
        assert_eq!(gets.load(Ordering::Relaxed), 1, "then one full GET");
    }

    /// A cancellable download above the configured ceiling falls back to the
    /// resumable `.partial` path. The direct GET already supplied the size, and
    /// a known-small file needs no Range probe before the full GET.
    #[test]
    fn cancellable_large_download_falls_back_without_probing() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        const TOTAL: usize = 64 * 1024;
        let payload: Vec<u8> = (0..TOTAL).map(|i| (i % 251) as u8).collect();
        let gets = Arc::new(AtomicUsize::new(0));
        let heads = Arc::new(AtomicUsize::new(0));
        let ranges = Arc::new(AtomicUsize::new(0));
        let full = payload.clone();
        let g = gets.clone();
        let h = heads.clone();
        let r = ranges.clone();
        let (base, _h) = serve(move |head, range| {
            if head.starts_with("HEAD ") {
                h.fetch_add(1, Ordering::Relaxed);
            } else if range.is_some() {
                r.fetch_add(1, Ordering::Relaxed);
            } else {
                g.fetch_add(1, Ordering::Relaxed);
            }
            (
                200,
                vec![("Content-Length".to_string(), full.len().to_string())],
                full.clone(),
            )
        });
        let (mut dl, _dir) = temp_dl("cancel-partial");
        dl.set_cancel_direct_max_bytes(8 * 1024);
        // 64 KiB is below the 1 MiB parallel threshold, so the known size
        // short-circuits the probe entirely.
        dl.set_parallel_thresholds(1024 * 1024, 4 * 1024);
        let task = Task {
            url: format!("{base}/big.bin"),
            filename: "big.bin".into(),
            cancel: Some(CancelToken::new()),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, TOTAL as u64);
        assert_eq!(heads.load(Ordering::Relaxed), 0, "no HEAD probe");
        assert_eq!(
            ranges.load(Ordering::Relaxed),
            0,
            "known-small file needs no Range probe"
        );
        assert_eq!(
            gets.load(Ordering::Relaxed),
            2,
            "direct header GET + full GET"
        );
    }

    /// With no known size, the merged `Range: 0-0` probe supplies both the
    /// total and range support in one request (no HEAD), and a file above the
    /// parallel threshold proceeds to chunked ranges.
    #[test]
    fn unknown_size_probe_uses_one_range_request() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        const TOTAL: usize = 64 * 1024;
        let payload: Vec<u8> = (0..TOTAL).map(|i| (i % 251) as u8).collect();
        let heads = Arc::new(AtomicUsize::new(0));
        let ranges = Arc::new(AtomicUsize::new(0));
        let full = payload.clone();
        let h = heads.clone();
        let r = ranges.clone();
        let (base, _h) = serve(move |head, range| {
            if head.starts_with("HEAD ") {
                h.fetch_add(1, Ordering::Relaxed);
                return (
                    200,
                    vec![("Content-Length".to_string(), full.len().to_string())],
                    Vec::new(),
                );
            }
            match range {
                Some(spec) => {
                    r.fetch_add(1, Ordering::Relaxed);
                    let spec = spec.trim_start_matches("bytes=");
                    let (start_s, end_s) = spec.split_once('-').unwrap();
                    let start: usize = start_s.parse().unwrap();
                    let end: usize = end_s.parse::<usize>().unwrap().min(full.len() - 1);
                    let chunk = full[start..=end].to_vec();
                    (
                        206,
                        vec![
                            (
                                "Content-Range".to_string(),
                                format!("bytes {start}-{end}/{}", full.len()),
                            ),
                            ("Content-Length".to_string(), chunk.len().to_string()),
                        ],
                        chunk,
                    )
                }
                None => (
                    200,
                    vec![("Content-Length".to_string(), full.len().to_string())],
                    full.clone(),
                ),
            }
        });
        let (mut dl, _dir) = temp_dl("unknown-size-probe");
        dl.set_workers(4);
        // Force known_total = 0 so the partial path has to probe.
        dl.set_cancel_direct_max_bytes(0);
        dl.set_parallel_thresholds(8 * 1024, 4 * 1024);
        let task = Task {
            url: format!("{base}/unknown.bin"),
            filename: "unknown.bin".into(),
            cancel: Some(CancelToken::new()),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, TOTAL as u64);
        assert_eq!(
            heads.load(Ordering::Relaxed),
            0,
            "merged probe issues no HEAD"
        );
        assert!(
            ranges.load(Ordering::Relaxed) >= 2,
            "one 0-0 probe plus the chunk range requests"
        );
    }

    /// `--timings` must include connect/TLS/redirect/header time for the
    /// direct path, not only the body read.
    #[test]
    fn direct_stream_timing_includes_request_headers() {
        let delay = Duration::from_millis(120);
        let body = b"timed direct".to_vec();
        let payload = body.clone();
        let (base, _h) = serve(move |_head, _range| {
            std::thread::sleep(delay);
            (
                200,
                vec![("Content-Length".to_string(), payload.len().to_string())],
                payload.clone(),
            )
        });
        let (dl, _dir) = temp_dl("direct-timing");
        let task = Task {
            url: format!("{base}/timed.bin"),
            filename: "timed.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert!(
            res.timing.network >= delay,
            "network = {:?}, expected at least {delay:?}",
            res.timing.network
        );
    }

    #[test]
    fn hash_mismatch_discards_blob() {
        let (base, _h) = serve(|_head, _range| (200, vec![], b"payload".to_vec()));
        let (dl, dir) = temp_dl("mismatch");
        let task = Task {
            url: format!("{base}/x.bin"),
            filename: "x.bin".into(),
            hash_algo: "sha256".into(),
            hash_value: "0".repeat(64),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(matches!(
            res.error,
            Some(DownloadError::HashMismatch { .. })
        ));
        assert!(res.hash.is_empty());
        // The mismatched blob must not stay in the store.
        let entries: Vec<_> = std::fs::read_dir(&dir)
            .unwrap()
            .flatten()
            .filter(|e| e.file_name().to_string_lossy().starts_with(".tmp-"))
            .collect();
        assert!(entries.is_empty());
    }

    #[test]
    fn http_404_surfaces_status_error() {
        let (base, _h) = serve(|_head, _range| (404, vec![], vec![]));
        let (dl, _dir) = temp_dl("404");
        let task = Task {
            url: format!("{base}/missing.bin"),
            filename: "missing.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_some());
        let msg = res.error.unwrap().to_string();
        assert!(msg.contains("404"), "msg = {msg}");
    }

    #[test]
    fn a_body_that_drops_midstream_retries_and_resumes() {
        // A connection that dies at 14% used to abort the whole install. The
        // retry must ask only for the missing tail (Range) and finish, rather
        // than restarting the 57 MB artifact from zero.
        use std::sync::atomic::Ordering;
        std::env::set_var("GLUE_TEST_RETRY_DELAY_MS", "0");
        let payload: Vec<u8> = (0..64 * 1024).map(|i| (i % 251) as u8).collect();
        let total = payload.len();
        let served = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let counter = served.clone();
        let body = payload.clone();
        let (base, _h) = serve(move |_head, range| match range {
            // First request: advertise the full length but close after 8 KiB.
            None => {
                counter.fetch_add(1, Ordering::Relaxed);
                (
                    200,
                    vec![("Content-Length".to_string(), total.to_string())],
                    body[..8 * 1024].to_vec(),
                )
            }
            Some(r) if r.starts_with("bytes=") => {
                let start: usize = r["bytes=".len()..r.len() - 1].parse().unwrap();
                counter.fetch_add(1, Ordering::Relaxed);
                (
                    206,
                    vec![(
                        "Content-Range".to_string(),
                        format!("bytes {start}-{}/{total}", total - 1),
                    )],
                    body[start..].to_vec(),
                )
            }
            _ => (500, vec![], vec![]),
        });
        let (mut dl, _dir) = temp_dl("body-drop-retry");
        // Force the resumable `.part` path and keep this small file sequential.
        dl.set_direct_stream_max_bytes(0);
        dl.set_parallel_download(false);
        let task = Task {
            url: format!("{base}/flaky.bin"),
            filename: "flaky.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, total as u64);
        assert_eq!(res.hash, crate::hash::hash_bytes(&payload));
        assert_eq!(
            served.load(Ordering::Relaxed),
            2,
            "one dropped body, one resumed request"
        );
    }

    #[test]
    fn a_body_that_always_drops_gives_up_after_the_retry_budget() {
        // The retry loop must be bounded: a mirror that keeps truncating the
        // body costs three attempts and then reports the error, not an endless
        // retry storm.
        use std::sync::atomic::Ordering;
        std::env::set_var("GLUE_TEST_RETRY_DELAY_MS", "0");
        const TOTAL: usize = 64 * 1024;
        let served = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let counter = served.clone();
        let (base, _h) = serve(move |_head, _range| {
            counter.fetch_add(1, Ordering::Relaxed);
            (
                200,
                vec![("Content-Length".to_string(), TOTAL.to_string())],
                vec![0u8; 1024],
            )
        });
        let (mut dl, _dir) = temp_dl("body-drop-exhausted");
        dl.set_direct_stream_max_bytes(0);
        dl.set_parallel_download(false);
        let task = Task {
            url: format!("{base}/broken.bin"),
            filename: "broken.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_some(), "a truncated body must not pass");
        assert_eq!(served.load(Ordering::Relaxed), TRANSFER_ATTEMPTS as usize);
    }

    #[test]
    fn resume_completes_from_partial_offset() {
        let full = b"0123456789abcdef".to_vec();
        let body = full.clone();
        let (base, _h) = serve(move |_head, range| {
            let bytes = body.clone();
            match range {
                Some(r) if r.starts_with("bytes=") => {
                    let start: usize = r["bytes=".len()..r.len() - 1].parse().unwrap();
                    let chunk = bytes[start..].to_vec();
                    let total = bytes.len();
                    (
                        206,
                        vec![(
                            "Content-Range".to_string(),
                            format!("bytes {start}-{}/{total}", total - 1),
                        )],
                        chunk,
                    )
                }
                _ => (200, vec![], bytes),
            }
        });
        let (dl, dir) = temp_dl("resume");
        let task = Task {
            url: format!("{base}/big.bin"),
            filename: "big.bin".into(),
            ..Default::default()
        };
        // Seed an interrupted download: first 8 bytes + sidecar meta.
        let (part_path, meta_path) = {
            let id = partial_id(&task.url);
            let pdir = dir.join(".partial");
            std::fs::create_dir_all(&pdir).unwrap();
            (
                pdir.join(format!("{id}.part")),
                pdir.join(format!("{id}.meta.json")),
            )
        };
        std::fs::write(&part_path, &full[..8]).unwrap();
        save_partial_meta(
            &meta_path,
            &PartialMeta {
                url: task.url.clone(),
                source_url: task.url.clone(),
                total_size: full.len() as u64,
                ..PartialMeta::default()
            },
        )
        .unwrap();

        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.hash, crate::hash::hash_bytes(&full));
        // Partial files cleaned up after ingest.
        assert!(!part_path.exists());
        assert!(!meta_path.exists());
    }

    #[test]
    fn mirror_fallback_reaches_second_url() {
        // Mirror #1 answers 503 (definitive status, ranked by the fallback),
        // mirror #2 serves the file.
        //
        // A *dead port* would look more realistic, but on this machine a
        // refused connect to 127.0.0.1 costs 2.0 s (SYN retransmit) and the
        // retry loop makes 4 attempts → 8 s for a single test. A status
        // response exercises the same fallback ordering instantly.
        let body = b"via mirror".to_vec();
        let expected = body.clone();
        let (dead, _h1) = serve(|_head, _range| (503, vec![], b"busy".to_vec()));
        let (base, _h2) = serve(move |_head, _range| (200, vec![], expected.clone()));
        let (dl, _dir) = temp_dl("mirror");
        let urls = vec![format!("{dead}/file.bin"), format!("{base}/file.bin")];
        let resp = dl
            .do_get_with_fallback(&urls, None)
            .expect("fallback should reach the live mirror");
        assert_eq!(resp.status().as_u16(), 200);
        let mut got = Vec::new();
        resp.into_body()
            .into_reader()
            .read_to_end(&mut got)
            .unwrap();
        assert_eq!(got, body);
    }

    #[test]
    fn mirror_fallback_survives_transport_error() {
        // Mirror #1 accepts then closes without a response → transport error
        // (retryable). The fallback must still reach mirror #2. Backoff is
        // zeroed so the retry loop costs no wall clock.
        std::env::set_var("GLUE_TEST_RETRY_DELAY_MS", "0");
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let dead = format!("http://{}", listener.local_addr().unwrap());
        std::thread::spawn(move || {
            for stream in listener.incoming() {
                drop(stream); // close immediately, no HTTP response
            }
        });
        let body = b"after transport error".to_vec();
        let expected = body.clone();
        let (base, _h) = serve(move |_head, _range| (200, vec![], expected.clone()));
        let (dl, _dir) = temp_dl("mirror-transport");
        let urls = vec![format!("{dead}/file.bin"), format!("{base}/file.bin")];
        let resp = dl
            .do_get_with_fallback(&urls, None)
            .expect("fallback should survive a transport error");
        assert_eq!(resp.status().as_u16(), 200);
        let mut got = Vec::new();
        resp.into_body()
            .into_reader()
            .read_to_end(&mut got)
            .unwrap();
        assert_eq!(got, body);
    }

    #[test]
    fn non_sha256_alias_enables_store_reuse() {
        let body = b"md5 hashed".to_vec();
        let md5 = {
            use md5::Digest as _;
            let mut h = md5::Md5::new();
            h.update(&body);
            hex_encode(&h.finalize())
        };
        let expected = body.clone();
        let (base, _h) = serve(move |_head, _range| (200, vec![], expected.clone()));
        let (dl, dir) = temp_dl("md5");
        let task = Task {
            url: format!("{base}/m.bin"),
            filename: "m.bin".into(),
            hash_algo: "md5".into(),
            hash_value: md5.clone(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        let cas = res.hash.clone();
        assert_eq!(cas, crate::hash::hash_bytes(&body));

        // Second downloader over the same store: no HTTP needed, alias hit.
        let dl2 = Downloader::new(Store::new(dir.path()).unwrap());
        let res2 = dl2.download_with_cache(task.clone(), "", false, None);
        assert!(res2.error.is_none());
        assert!(res2.from_store);
        assert_eq!(res2.hash, cas);
    }

    /// A file large enough for ranges is
    /// fetched over multiple connections and lands byte-identical.
    #[test]
    fn parallel_range_download_over_loopback() {
        const TOTAL: usize = 20 * 1024;
        let payload: Vec<u8> = (0..TOTAL).map(|i| (i % 251) as u8).collect();
        let range_gets = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let rg = range_gets.clone();
        let body = payload.clone();
        let (base, _h) = serve(move |head, range| {
            let full = body.len();
            match range {
                Some(r) if r.starts_with("bytes=") => {
                    rg.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    let spec = &r["bytes=".len()..];
                    let (start_s, end_s) = spec.split_once('-').unwrap();
                    let start: usize = start_s.parse().unwrap();
                    let end: usize = end_s.parse().unwrap();
                    assert!(start <= end && end < full, "range {start}-{end} of {full}");
                    let chunk = body[start..=end].to_vec();
                    (
                        206,
                        vec![
                            (
                                "Content-Range".to_string(),
                                format!("bytes {start}-{end}/{full}"),
                            ),
                            ("Content-Length".to_string(), chunk.len().to_string()),
                        ],
                        chunk,
                    )
                }
                _ if head.starts_with("HEAD ") => (
                    200,
                    vec![
                        ("Content-Length".to_string(), full.to_string()),
                        ("Accept-Ranges".to_string(), "bytes".to_string()),
                    ],
                    vec![],
                ),
                _ => (
                    200,
                    vec![("Content-Length".to_string(), full.to_string())],
                    body.clone(),
                ),
            }
        });
        let (mut dl, _dir) = temp_dl("parallel");
        dl.set_workers(4);
        // Small thresholds so 20 KiB exercises the parallel path
        dl.set_direct_stream_max_bytes(8 * 1024);
        dl.set_parallel_thresholds(10 * 1024, 4 * 1024);
        let task = Task {
            url: format!("{base}/big.bin"),
            filename: "big.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, TOTAL as u64);
        assert_eq!(res.hash, crate::hash::hash_bytes(&payload));
        assert!(
            range_gets.load(std::sync::atomic::Ordering::Relaxed) >= 2,
            "expected parallel range requests"
        );
    }

    /// A range response that stops mid-body is retried from the missing
    /// offset instead of failing the whole chunk.
    #[test]
    fn truncated_chunk_resumes_from_the_missing_offset() {
        const TOTAL: usize = 20 * 1024;
        let payload: Vec<u8> = (0..TOTAL).map(|i| (i % 251) as u8).collect();
        let requests: std::sync::Arc<std::sync::Mutex<Vec<(usize, usize)>>> =
            std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
        // (chunk start, chunk end, offset the retry should resume from)
        let truncated: std::sync::Arc<std::sync::Mutex<Option<(usize, usize, usize)>>> =
            std::sync::Arc::new(std::sync::Mutex::new(None));
        let first = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(true));
        let body = payload.clone();
        let seen = requests.clone();
        let cut = truncated.clone();
        let (base, _h) = serve(move |head, range| {
            let full = body.len();
            match range {
                Some(r) if r.starts_with("bytes=") => {
                    let spec = &r["bytes=".len()..];
                    let (start_s, end_s) = spec.split_once('-').unwrap();
                    let start: usize = start_s.parse().unwrap();
                    let end: usize = end_s.parse().unwrap();
                    seen.lock().unwrap().push((start, end));
                    let chunk = body[start..=end].to_vec();
                    // Only the first real chunk is cut; the 1-byte range probe
                    // stays intact.
                    if chunk.len() > 8 && first.swap(false, std::sync::atomic::Ordering::SeqCst) {
                        let keep = chunk.len() / 2;
                        *cut.lock().unwrap() = Some((start, end, start + keep));
                        return (
                            206,
                            vec![
                                (
                                    "Content-Range".to_string(),
                                    format!("bytes {start}-{end}/{full}"),
                                ),
                                ("Content-Length".to_string(), chunk.len().to_string()),
                            ],
                            chunk[..keep].to_vec(),
                        );
                    }
                    (
                        206,
                        vec![
                            (
                                "Content-Range".to_string(),
                                format!("bytes {start}-{end}/{full}"),
                            ),
                            ("Content-Length".to_string(), chunk.len().to_string()),
                        ],
                        chunk,
                    )
                }
                _ if head.starts_with("HEAD ") => (
                    200,
                    vec![
                        ("Content-Length".to_string(), full.to_string()),
                        ("Accept-Ranges".to_string(), "bytes".to_string()),
                    ],
                    vec![],
                ),
                _ => (
                    200,
                    vec![("Content-Length".to_string(), full.to_string())],
                    body.clone(),
                ),
            }
        });
        let (mut dl, _dir) = temp_dl("chunk-retry");
        dl.set_workers(4);
        dl.set_direct_stream_max_bytes(8 * 1024);
        dl.set_parallel_thresholds(10 * 1024, 4 * 1024);
        let task = Task {
            url: format!("{base}/big.bin"),
            filename: "big.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, TOTAL as u64);
        assert_eq!(res.hash, crate::hash::hash_bytes(&payload));

        let (_start, end, resume_at) = truncated
            .lock()
            .unwrap()
            .expect("one chunk should have been truncated");
        assert!(
            requests
                .lock()
                .unwrap()
                .iter()
                .any(|(start, request_end)| *start == resume_at && *request_end == end),
            "expected a retry from offset {resume_at} for bytes up to {end}: {:?}",
            requests.lock().unwrap()
        );
    }

    /// Chunk records from a previous session may use different boundaries than
    /// the current plan, so resume fetches exactly the uncovered byte ranges.
    #[test]
    fn chunk_record_leaves_only_the_gaps_to_fetch() {
        let meta = PartialMeta {
            completed_chunks: vec![
                CompletedChunk { start: 0, end: 99 },
                CompletedChunk {
                    start: 100,
                    end: 199,
                },
                CompletedChunk {
                    start: 300,
                    end: 399,
                },
            ],
            ..PartialMeta::default()
        };
        // Adjacent ranges merge: nothing is left of [0, 199].
        assert!(meta.uncovered(ByteRange { start: 0, end: 199 }).is_empty());
        // The hole between 200 and 299 is exactly what a resume fetches.
        assert_eq!(
            meta.uncovered(ByteRange { start: 0, end: 399 }),
            vec![ByteRange {
                start: 200,
                end: 299
            }]
        );
        // A span that starts inside a recorded range only fetches its tail.
        assert_eq!(
            meta.uncovered(ByteRange {
                start: 350,
                end: 450
            }),
            vec![ByteRange {
                start: 400,
                end: 450
            }]
        );
        // Nothing recorded: the whole span is pending.
        assert_eq!(
            PartialMeta::default().uncovered(ByteRange { start: 0, end: 9 }),
            vec![ByteRange { start: 0, end: 9 }]
        );
    }

    /// A parallel session that was interrupted (paused, app closed, crash)
    /// resumes from its chunk record: the ranges already on disk are not
    /// fetched again, the gaps are, and the finished file still verifies.
    #[test]
    fn interrupted_parallel_session_resumes_missing_chunks() {
        const TOTAL: usize = 20 * 1024;
        let payload: Vec<u8> = (0..TOTAL).map(|i| (i % 251) as u8).collect();
        let seen: std::sync::Arc<std::sync::Mutex<Vec<(usize, usize)>>> =
            std::sync::Arc::new(std::sync::Mutex::new(Vec::new()));
        let seen_by_server = seen.clone();
        let body = payload.clone();
        let (base, _h) = serve(move |head, range| {
            let full = body.len();
            match range {
                Some(r) if r.starts_with("bytes=") => {
                    let spec = &r["bytes=".len()..];
                    let (start_s, end_s) = spec.split_once('-').unwrap();
                    let start: usize = start_s.parse().unwrap();
                    let end: usize = end_s.parse().unwrap();
                    seen_by_server.lock().unwrap().push((start, end));
                    let chunk = body[start..=end].to_vec();
                    (
                        206,
                        vec![
                            (
                                "Content-Range".to_string(),
                                format!("bytes {start}-{end}/{full}"),
                            ),
                            ("Content-Length".to_string(), chunk.len().to_string()),
                        ],
                        chunk,
                    )
                }
                _ if head.starts_with("HEAD ") => (
                    200,
                    vec![
                        ("Content-Length".to_string(), full.to_string()),
                        ("Accept-Ranges".to_string(), "bytes".to_string()),
                    ],
                    vec![],
                ),
                _ => (
                    200,
                    vec![("Content-Length".to_string(), full.to_string())],
                    body.clone(),
                ),
            }
        });
        let (mut dl, _dir) = temp_dl("parallel-resume");
        dl.set_workers(4);
        dl.set_direct_stream_max_bytes(8 * 1024);
        dl.set_parallel_thresholds(10 * 1024, 4 * 1024);
        let task = Task {
            url: format!("{base}/big.bin"),
            filename: "big.bin".into(),
            hash_algo: "sha256".into(),
            hash_value: crate::hash::hash_bytes(&payload),
            ..Default::default()
        };

        // What a killed session leaves behind: a preallocated .part with a
        // real byte prefix (the rest is holes) and a meta recording that
        // prefix. Recording stops mid-chunk, which is what a slow line looks
        // like — resume must fetch the rest of the chunk, not all of it.
        const RECORDED_END: usize = 4095;
        let (part_path, meta_path) = dl.partial_paths(&task);
        std::fs::create_dir_all(part_path.parent().unwrap()).unwrap();
        let mut file_bytes = vec![0u8; TOTAL];
        file_bytes[..=RECORDED_END].copy_from_slice(&payload[..=RECORDED_END]);
        std::fs::write(&part_path, &file_bytes).unwrap();
        save_partial_meta(
            &meta_path,
            &PartialMeta {
                url: task.url.clone(),
                source_url: task.url.clone(),
                planned_total: TOTAL as u64,
                completed_chunks: vec![CompletedChunk {
                    start: 0,
                    end: RECORDED_END as u64,
                }],
                ..PartialMeta::default()
            },
        )
        .unwrap();

        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, TOTAL as u64);
        assert_eq!(res.hash, crate::hash::hash_bytes(&payload));

        let seen = seen.lock().unwrap().clone();
        assert!(
            !seen.iter().any(|(start, _)| *start == 0),
            "the recorded prefix must not be fetched again: {seen:?}"
        );
        assert!(
            seen.iter().any(|(start, _)| *start == RECORDED_END + 1),
            "the unrecorded tail of the first chunk must be fetched: {seen:?}"
        );
    }

    #[test]
    fn repeated_416_fails_instead_of_restarting_forever() {
        // A hostile server answering 416 to everything: the from-zero
        // restart may happen at most once — it used to be an unbounded
        // recursion, i.e. a remote stack-overflow crash.
        std::env::set_var("GLUE_TEST_RETRY_DELAY_MS", "0");
        let (base, _h) = serve(|_head, _range| (416, vec![], vec![]));
        let (dl, _dir) = temp_dl("416-loop");
        let task = Task {
            url: format!("{base}/x.bin"),
            filename: "x.bin".into(),
            ..Default::default()
        };
        let (part_path, meta_path) = dl.partial_paths(&task);
        std::fs::create_dir_all(part_path.parent().unwrap()).unwrap();
        std::fs::write(&part_path, b"12345").unwrap();
        let mut st = PartialState {
            part_path,
            meta_path,
            task_url: task.url.clone(),
            meta: PartialMeta {
                url: task.url.clone(),
                source_url: task.url.clone(),
                total_size: 100,
                ..PartialMeta::default()
            },
            offset: 5,
        };
        let err = dl
            .fetch_to_partial_url(&task, &task.url.clone(), &mut st, None)
            .expect_err("repeated 416 must fail");
        assert!(
            matches!(err, DownloadError::Http { status: 416, .. }),
            "got {err:?}"
        );
    }

    #[test]
    fn failed_parallel_resume_keeps_the_plan_and_resumes() {
        // The parallel session preallocates the .part to the full total with
        // zero holes. A failure must not throw that away: the meta keeps
        // `total_size = 0` (so the file can never pass for finished) and the
        // chunk record, and the next attempt fetches only the gaps.
        std::env::set_var("GLUE_TEST_RETRY_DELAY_MS", "0");
        const TOTAL: usize = 40 * 1024 * 1024;
        const OFFSET: u64 = 8 * 1024 * 1024;
        let payload: Vec<u8> = (0..TOTAL).map(|i| (i % 251) as u8).collect();
        let healthy = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        let body = payload.clone();
        let healthy_server = healthy.clone();
        let (base, _h) = serve(move |head, range| {
            if !healthy_server.load(std::sync::atomic::Ordering::SeqCst) {
                // The probe succeeds so the parallel resume is entered…
                if range == Some("bytes=0-0") {
                    return (
                        206,
                        vec![
                            ("Content-Range".to_string(), format!("bytes 0-0/{TOTAL}")),
                            ("Content-Length".to_string(), "1".to_string()),
                        ],
                        vec![0],
                    );
                }
                // …but every chunk request fails.
                return (500, vec![], vec![]);
            }
            match range {
                Some(r) if r.starts_with("bytes=") => {
                    let spec = &r["bytes=".len()..];
                    let (start_s, end_s) = spec.split_once('-').unwrap();
                    let start: usize = start_s.parse().unwrap();
                    let end: usize = end_s.parse().unwrap();
                    let chunk = body[start..=end].to_vec();
                    (
                        206,
                        vec![
                            (
                                "Content-Range".to_string(),
                                format!("bytes {start}-{end}/{TOTAL}"),
                            ),
                            ("Content-Length".to_string(), chunk.len().to_string()),
                        ],
                        chunk,
                    )
                }
                _ if head.starts_with("HEAD ") => (
                    200,
                    vec![("Content-Length".to_string(), TOTAL.to_string())],
                    vec![],
                ),
                _ => (500, vec![], vec![]),
            }
        });
        let (mut dl, _dir) = temp_dl("parallel-resume-fail");
        dl.set_workers(4);
        dl.set_direct_stream_max_bytes(8 * 1024);
        dl.set_parallel_thresholds(10 * 1024, 4 * 1024);
        let task = Task {
            url: format!("{base}/big.bin"),
            filename: "big.bin".into(),
            ..Default::default()
        };
        // Seed a legitimate resume state: OFFSET bytes of a TOTAL-byte file.
        let (part_path, meta_path) = dl.partial_paths(&task);
        std::fs::create_dir_all(part_path.parent().unwrap()).unwrap();
        let seed = std::fs::OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(false)
            .open(&part_path)
            .unwrap();
        seed.set_len(OFFSET).unwrap();
        drop(seed);
        save_partial_meta(
            &meta_path,
            &PartialMeta {
                url: task.url.clone(),
                source_url: task.url.clone(),
                total_size: TOTAL as u64,
                ..PartialMeta::default()
            },
        )
        .unwrap();

        let res = dl.download(&task, None);
        assert!(res.error.is_some(), "the download must fail");
        assert_eq!(
            std::fs::metadata(&part_path).unwrap().len(),
            TOTAL as u64,
            "the preallocated .part is kept for the next attempt"
        );
        let meta: PartialMeta =
            serde_json::from_slice(&std::fs::read(&meta_path).unwrap()).unwrap();
        assert_eq!(
            meta.total_size, 0,
            "a failed session must not leave a total in the meta"
        );
        assert_eq!(
            meta.planned_total, TOTAL as u64,
            "the plan is kept so the next attempt resumes the gaps"
        );
        assert!(
            meta.completed_chunks.is_empty(),
            "no chunk completed against a failing server"
        );

        // The server recovers: the next attempt must finish the same file.
        healthy.store(true, std::sync::atomic::Ordering::SeqCst);
        let res = dl.download(&task, None);
        assert!(
            res.error.is_none(),
            "resume after recovery: {:?}",
            res.error
        );
        assert_eq!(res.size, TOTAL as u64);
        assert_eq!(res.hash, crate::hash::hash_bytes(&payload));
    }

    // ------------------------------------------------------------------
    // Loopback FTP integration tests (.go).
    // ------------------------------------------------------------------

    fn ftp_line(w: &mut std::net::TcpStream, s: &str) {
        let _ = w.write_all(s.as_bytes());
        let _ = w.write_all(b"\r\n");
        let _ = w.flush();
    }

    /// Minimal FTP server serving one anonymous file over PASV
    fn serve_ftp_conn(ctrl: std::net::TcpStream, file_name: &str, content: &[u8]) {
        ctrl.set_read_timeout(Some(Duration::from_secs(30))).ok();
        let mut reader = BufReader::new(ctrl.try_clone().unwrap());
        let mut writer = ctrl;
        ftp_line(&mut writer, "220 glue test FTP ready");
        let mut pending: Option<TcpListener> = None;
        loop {
            let mut line = String::new();
            if reader.read_line(&mut line).unwrap_or(0) == 0 {
                break;
            }
            let trimmed = line.trim();
            if trimmed.is_empty() {
                continue;
            }
            let cmd = trimmed
                .split_whitespace()
                .next()
                .unwrap_or("")
                .to_ascii_uppercase();
            match cmd.as_str() {
                "USER" => ftp_line(&mut writer, "331 password please"),
                "PASS" => ftp_line(&mut writer, "230 logged in"),
                "TYPE" => ftp_line(&mut writer, "200 type set"),
                "SYST" => ftp_line(&mut writer, "215 UNIX Type: L8"),
                "FEAT" => {
                    ftp_line(&mut writer, "211-Features");
                    ftp_line(&mut writer, " PASV");
                    ftp_line(&mut writer, "211 End");
                }
                "SIZE" => ftp_line(&mut writer, &format!("213 {}", content.len())),
                "PASV" => {
                    if let Some(l) = pending.take() {
                        drop(l);
                    }
                    match TcpListener::bind("127.0.0.1:0") {
                        Ok(data_ln) => {
                            let port = data_ln.local_addr().unwrap().port();
                            pending = Some(data_ln);
                            let p1 = port / 256;
                            let p2 = port % 256;
                            ftp_line(
                                &mut writer,
                                &format!("227 Entering Passive Mode (127,0,0,1,{p1},{p2})"),
                            );
                        }
                        Err(_) => ftp_line(&mut writer, "425 cannot open data connection"),
                    }
                }
                "RETR" => {
                    let Some(data_ln) = pending.take() else {
                        ftp_line(&mut writer, "425 use PASV first");
                        continue;
                    };
                    ftp_line(&mut writer, &format!("150 sending {file_name}"));
                    match data_ln.accept() {
                        Ok((mut data, _)) => {
                            drop(data_ln);
                            let _ = data.write_all(content);
                            let _ = data.flush();
                            drop(data);
                            ftp_line(&mut writer, "226 transfer complete");
                        }
                        Err(_) => ftp_line(&mut writer, "426 connection failed"),
                    }
                }
                "QUIT" => {
                    if let Some(l) = pending.take() {
                        drop(l);
                    }
                    ftp_line(&mut writer, "221 bye");
                    return;
                }
                "NOOP" => ftp_line(&mut writer, "200 ok"),
                "AUTH" | "PBSZ" | "PROT" => ftp_line(&mut writer, "502 not implemented"),
                _ => ftp_line(&mut writer, "500 unknown command"),
            }
        }
        if let Some(l) = pending {
            drop(l);
        }
    }

    fn spawn_test_ftp_server(
        file_name: &'static str,
        content: &'static [u8],
    ) -> (String, std::thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let handle = std::thread::spawn(move || loop {
            let Ok((ctrl, _)) = listener.accept() else {
                break;
            };
            serve_ftp_conn(ctrl, file_name, content);
        });
        (addr, handle)
    }

    #[test]
    fn ftp_download_over_loopback() {
        let content: &'static [u8] = b"glue ftp local test payload";
        let (addr, _h) = spawn_test_ftp_server("sample.bin", content);
        let (dl, _dir) = temp_dl("ftp");
        let task = Task {
            url: format!("ftp://{addr}/sample.bin"),
            filename: "sample.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert_eq!(res.size, content.len() as u64);
        assert_eq!(res.hash, crate::hash::hash_bytes(content));
        assert!(!res.from_store);
    }

    #[test]
    fn ftp_url_without_file_path_errors() {
        let content: &'static [u8] = b"unused";
        let (addr, _h) = spawn_test_ftp_server("sample.bin", content);
        let (dl, _dir) = temp_dl("ftp-nopath");
        let task = Task {
            url: format!("ftp://{addr}/"),
            filename: "sample.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(
            matches!(&res.error, Some(DownloadError::Ftp(e)) if e.to_string().contains("ftp URL has no file path")),
            "err = {:?}",
            res.error
        );
    }

    // ------------------------------------------------------------------
    // ZIP member index (zip_member_index.go)
    // ------------------------------------------------------------------

    #[test]
    fn zip_member_index_store_hit_attaches_files() {
        // Store-hit path: zip task + ready member index → result.Files.
        let (dl, _dir) = temp_dl("zipidx");
        let blob = b"pretend zip bytes";
        let zip_hash = dl.store().write(&blob[..]).unwrap();
        let member = dl.store().write(&b"member payload"[..]).unwrap();
        let mut files = std::collections::BTreeMap::new();
        files.insert("bin/tool.exe".to_string(), member.clone());
        dl.save_zip_member_index(&zip_hash, &files, 14).unwrap();

        let task = Task {
            url: "https://example.invalid/tool.zip".into(),
            filename: "tool.zip".into(),
            hash_algo: "sha256".into(),
            hash_value: zip_hash.clone(),
            ..Default::default()
        };
        let res = dl.download_with_cache(task, &zip_hash, false, None);
        assert!(res.error.is_none(), "err = {:?}", res.error);
        assert!(res.from_store);
        assert_eq!(res.files, Some(files));
        assert_eq!(res.zip_total_bytes, 14);
        assert_eq!(dl.zip_index_total_bytes(&zip_hash), 14);
    }

    #[test]
    fn stale_zip_index_removed_on_resolve() {
        // An index whose member
        // blobs are missing is dropped, not returned.
        let (dl, _dir) = temp_dl("zipidx-stale");
        let bogus_member = "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad";
        let mut files = std::collections::BTreeMap::new();
        files.insert("LICENSE".to_string(), bogus_member.to_string());
        dl.save_zip_member_index("abc123", &files, 10).unwrap();

        assert!(dl.resolve_zip_member_index("abc123").is_none());
        assert!(!crate::zip::index_path(dl.store().path(), "abc123").exists());
        // Non-zip tasks never attach a member map.
        let (base, _h) = serve(|_head, _range| (200, vec![], b"plain".to_vec()));
        let task = Task {
            url: format!("{base}/plain.bin"),
            filename: "plain.bin".into(),
            ..Default::default()
        };
        let res = dl.download(&task, None);
        assert!(res.error.is_none());
        assert!(res.files.is_none());
    }

    #[test]
    fn clear_stale_sidecars_prunes_orphans() {
        let (dl, _dir) = temp_dl("sidecar-gc");
        let root = dl.store().path();

        // A blob that is both a valid zip blob and a member blob, so its
        // member index is link-ready and must survive the sweep.
        let live = dl.store().write(&b"live-blob"[..]).unwrap();
        let dead = "a".repeat(64);

        // Fresh, still-valid index → kept; index for a missing zip blob → removed.
        let live_files = std::collections::BTreeMap::from([("a.txt".to_string(), live.clone())]);
        let dead_files = std::collections::BTreeMap::from([("b.txt".to_string(), dead.clone())]);
        dl.save_zip_member_index(&live, &live_files, 9).unwrap();
        dl.save_zip_member_index(&dead, &dead_files, 1).unwrap();
        std::fs::create_dir_all(root.join(".zip-index")).unwrap();
        std::fs::write(root.join(".zip-index").join("junk.tmp"), b"x").unwrap();

        // Alias to a live blob → kept; alias to a missing blob → removed.
        save_manifest_hash_alias(root, "sha1", "digest-live", &live).unwrap();
        save_manifest_hash_alias(root, "sha1", "digest-dead", &dead).unwrap();

        let (removed, _freed) = dl.clear_stale_sidecar_indexes().unwrap();
        // dead zip-index + junk.tmp + dead manifest-hash = 3.
        assert_eq!(removed, 3, "removed = {removed}");

        assert!(crate::zip::index_path(root, &live).exists());
        assert!(!crate::zip::index_path(root, &dead).exists());
        assert!(!root.join(".zip-index").join("junk.tmp").exists());
        assert!(manifest_hash_index_path(root, "sha1", "digest-live").exists());
        assert!(!manifest_hash_index_path(root, "sha1", "digest-dead").exists());
    }

    #[test]
    fn clear_stale_sidecars_missing_dirs_is_ok() {
        let (dl, _dir) = temp_dl("sidecar-gc-missing");
        assert_eq!(dl.clear_stale_sidecar_indexes().unwrap(), (0, 0));
    }
}
