//! Archive extraction: in-process ZIP plus an external `7z.exe` fallback for
//! everything else.
//!
//! # Divergence: ZIP is extracted in-process
//!
//! Routing every format — ZIP included — through an external `7z.exe`
//! subprocess would make `7z.exe` a hard runtime dependency even for the
//! format that covers nearly all Scoop packages.
//!
//! This implementation parses ZIP itself ([`crate::ziparchive`]) and only
//! shells out to `7z.exe` for the formats it cannot handle locally (7z, MSI,
//! SFX exe, bz2/xz). Observable behaviour is preserved: the same member map
//! comes back, the same `archmember` normalization applies, and
//! `UnsupportedFormatError` keeps its wording. One deliberate divergence: a
//! missing `7z.exe` reports where to get it (`glue install main/7zip`), because
//! this implementation never bootstraps a 7-Zip download.
//!
//! # Two extraction modes
//!
//! - [`Extractor::extract_file`] — unpack a whole archive into the CAS store
//!   and return `(archiveHash, relPath → blobHash)`.
//! - [`Extractor::extract_to_dir`] — unpack straight into an install directory
//!   (no CAS round-trip), used when the engine deploys a fetched artifact.
//!   ZIP members are decompressed on `workers` threads and every written file
//!   is hashed on the way out ([`ExtractHashes`]), so the deploy pass can take
//!   the files into the store without reading them a second time.

use std::collections::BTreeMap;
use std::fs;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};

use thiserror::Error;

use crate::store::Store;
use crate::tararchive::{TarArchive, TarEntry, TarError};
use crate::ziparchive::{ZipArchive, ZipArchiveError};

/// Reported when an archive format needs an external tool that is absent.
#[derive(Debug, Error)]
#[error("unsupported format: {format}{suggestion}")]
pub struct UnsupportedFormatError {
    pub format: String,
    /// Rendered after a newline, exactly)`.
    pub suggestion: String,
}

impl UnsupportedFormatError {
    pub fn new(format: impl Into<String>, suggestion: impl Into<String>) -> Self {
        let suggestion = suggestion.into();
        // Prefixes the suggestion with "\n" only when non-empty.
        let suggestion = if suggestion.is_empty() {
            String::new()
        } else {
            format!("\n{suggestion}")
        };
        UnsupportedFormatError {
            format: format.into(),
            suggestion,
        }
    }
}

/// Errors from extraction.
#[derive(Debug, Error)]
pub enum ExtractError {
    #[error("detect archive type: {0}")]
    DetectType(String),
    #[error("unknown archive type: {0}")]
    UnknownType(String),
    /// Says only `7z.exe not found in PATH`; this implementation never bootstraps a
    /// 7-Zip download, so the message names the fix (see module docs).
    #[error(
        "7z.exe not found in PATH or the glue bin dir (install it first: glue install main/7zip)"
    )]
    SevenZipMissing,
    #[error("7z extract failed: {0}")]
    SevenZipFailed(String),
    #[error("create temp dir: {0}")]
    CreateTempDir(#[source] std::io::Error),
    #[error("create dest dir: {0}")]
    CreateDestDir(#[source] std::io::Error),
    #[error("read archive: {0}")]
    ReadArchive(#[source] std::io::Error),
    #[error("write file: {0}")]
    WriteFile(#[source] std::io::Error),
    #[error("process extracted files: {0}")]
    Ingest(String),
    #[error("extracted file {path:?}: {message}")]
    UnsafeMember { path: String, message: String },
    #[error(transparent)]
    Zip(#[from] ZipArchiveError),
    #[error(transparent)]
    Tar(#[from] TarError),
    #[error(transparent)]
    Store(#[from] crate::store::StoreError),
    #[error(transparent)]
    Unsupported(#[from] UnsupportedFormatError),
}

pub type Result<T> = std::result::Result<T, ExtractError>;

/// Temp-file uniqueness across concurrent extractions.
static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// 7z stream signature (`7z` + 0xBC 0xAF 0x27 0x1C); a plain `.7z` starts with
/// it at offset 0, a self-extracting archive has a stub before it.
const SEVEN_ZIP_MAGIC: [u8; 6] = [0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C];
/// How far into a file to scan for the 7z signature (SFX stubs are tiny; this
/// avoids reading a multi-MB installer into memory just to find it is not 7z).
const SFX_SCAN_LIMIT: usize = 2 * 1024 * 1024;

/// Outcome of the in-process 7z attempt, so the caller can decide whether to
/// fall back to the external `7z.exe`.
enum SevenZTried {
    /// It was a 7z and was extracted in process.
    Handled,
    /// No 7z signature — not a 7z container (e.g. a plain `.exe` installer).
    NotSevenZip,
    /// It is a 7z but this build could not decode it (unsupported codec/corrupt);
    /// let the external tool try.
    Fallback,
}

/// A `Read + Seek` view of a file whose logical position 0 is `base`.
///
/// `sevenz-rust2` locates the archive with absolute seeks (`End(0)` for the
/// length, `Start(0)` for the signature, then `Start(header_pos)`). Handing it
/// a raw `File` therefore reads the SFX stub instead of the appended 7z and
/// fails with `BadSignature` — the `#/dl.7z` shape (zoom, and 426 manifests in
/// this corpus). Re-basing every seek keeps the crate's arithmetic in archive
/// coordinates: `End(0)` reports the archive length, not the file length.
struct OffsetReader {
    file: fs::File,
    base: u64,
}

impl std::io::Read for OffsetReader {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        self.file.read(buf)
    }
}

impl std::io::Seek for OffsetReader {
    fn seek(&mut self, pos: std::io::SeekFrom) -> std::io::Result<u64> {
        use std::io::SeekFrom;
        match pos {
            SeekFrom::Start(n) => {
                let abs = self
                    .file
                    .seek(SeekFrom::Start(self.base.saturating_add(n)))?;
                Ok(abs.saturating_sub(self.base))
            }
            SeekFrom::Current(n) => {
                let abs = self.file.seek(SeekFrom::Current(n))?;
                Ok(abs.saturating_sub(self.base))
            }
            SeekFrom::End(n) => {
                let abs = self.file.seek(SeekFrom::End(n))?;
                Ok(abs.saturating_sub(self.base))
            }
        }
    }
}

/// Build a `sevenz_rust2::Error` from a message. Its `Other` variant carries a
/// `Cow`, so centralize the conversion at the in-process 7z call sites.
fn sevenz_error(msg: String) -> sevenz_rust2::Error {
    sevenz_rust2::Error::Other(std::borrow::Cow::Owned(msg))
}

/// Rough number of bytes the archive will occupy once unpacked, for a
/// pre-flight disk-space check.
///
/// ZIP is exact: the central directory carries every member's uncompressed
/// size. A plain tar uses the file size (headers and padding included), and a
/// gzip-compressed tar uses the trailer's ISIZE — both are upper bounds, which
/// is what a preflight needs. Every other format returns `None`: a guess could
/// block a valid install or promise space that is not actually there.
pub fn estimate_extract_bytes(archive_path: &Path, archive_name: &str) -> Option<u64> {
    let name = if archive_name.is_empty() {
        archive_path
            .file_name()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default()
    } else {
        archive_name.to_string()
    };

    if is_zip_name(&name) {
        let archive = ZipArchive::open(archive_path).ok()?;
        let mut total: u64 = 0;
        for entry in archive.file_entries() {
            total = total.saturating_add(entry.uncompressed_size);
        }
        return Some(total);
    }

    if is_tar_name(&name) {
        if is_gzip_file(archive_path) {
            // ISIZE is the uncompressed stream length modulo 4 GiB, and the
            // stream includes tar headers. A value below the compressed size
            // means a wrapped counter or a multi-member file, so skip the
            // check rather than under-estimate.
            let compressed = std::fs::metadata(archive_path).ok()?.len();
            let isize = gzip_isize(archive_path)?;
            return (isize >= compressed).then_some(isize);
        }
        return std::fs::metadata(archive_path).ok().map(|m| m.len());
    }

    None
}

/// Whether the file starts with the gzip magic.
fn is_gzip_file(path: &Path) -> bool {
    let Ok(mut file) = fs::File::open(path) else {
        return false;
    };
    let mut magic = [0u8; 2];
    std::io::Read::read_exact(&mut file, &mut magic).is_ok() && magic == [0x1f, 0x8b]
}

/// `ISIZE` from the gzip trailer (uncompressed length modulo 2^32).
fn gzip_isize(path: &Path) -> Option<u64> {
    use std::io::{Read, Seek, SeekFrom};
    let mut file = fs::File::open(path).ok()?;
    let len = file.metadata().ok()?.len();
    if len < 18 {
        return None;
    }
    file.seek(SeekFrom::End(-4)).ok()?;
    let mut buf = [0u8; 4];
    file.read_exact(&mut buf).ok()?;
    Some(u32::from_le_bytes(buf) as u64)
}

/// Per-file ingest progress: (processed, total).
pub type IngestProgressFn = Box<dyn Fn(i64, i64) + Send + Sync>;

/// Content hashes computed while extracting.
///
/// Each digest is stored twice: under the file's hardlink identity
/// ([`crate::cache::fileid::file_ref_key`]) and, prefixed with `rel:`, under
/// its install-relative path ([`rel_hash_key`]). Renames inside the install
/// directory (extract_dir flattening) preserve the identity key, so entries
/// stay valid after the layout is applied; the path key is the cheap lookup
/// adopt uses when no layout pass ran.
pub type ExtractHashes = BTreeMap<String, String>;

/// `Write` sink that hashes the bytes it accepts.
struct HashingWriter<W> {
    inner: W,
    hasher: crate::hash::Sha256,
}

impl<W> HashingWriter<W> {
    fn new(inner: W) -> Self {
        Self {
            inner,
            hasher: crate::hash::Sha256::new(),
        }
    }
}

impl<W: Write> HashingWriter<W> {
    /// Hex SHA-256 of everything written so far.
    fn finish(self) -> String {
        crate::hash::hex_encode(&self.hasher.finish())
    }
}

impl<W: Write> Write for HashingWriter<W> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let n = self.inner.write(buf)?;
        if n > 0 {
            self.hasher.update(&buf[..n]);
        }
        Ok(n)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        self.inner.flush()
    }
}

/// Key for a digest recorded by install-relative path.
///
/// The adopt pass walks the finished install dir, so a path key lets it reuse
/// the digest without opening every file for its hardlink identity. Paths are
/// lowercased and slash-normalized to match the walk; the `rel:` prefix keeps
/// these keys disjoint from identity keys in the same map.
pub(crate) fn rel_hash_key(rel: &str) -> String {
    format!("rel:{}", rel.replace('\\', "/").to_lowercase())
}

/// Record `hex` under the file's hardlink identity and install-relative path.
///
/// Hosts without a file identity (non-Windows) still record the path key;
/// adopt then falls back to hashing only when the path was rewritten by a
/// layout pass.
fn record_file_hash(hashes: &mut ExtractHashes, path: &Path, rel: &str, hex: String) {
    if let Some(key) = crate::cache::fileid::file_ref_key(path) {
        hashes.insert(key, hex.clone());
    }
    if !rel.is_empty() {
        hashes.insert(rel_hash_key(rel), hex);
    }
}

/// Archive → destination extraction, with an optional external 7z path.
pub struct Extractor {
    store: Option<Store>,
    temp_dir: PathBuf,
    /// Whether an external 7z.exe is usable. Probed on first use,
    /// not at construction: the probe spawns `7z.exe i` (measured 326 ms on
    /// this machine) and 62% of the corpus is zip, which never touches 7z.
    /// `None` until probed.
    use7z: std::sync::OnceLock<bool>,
    /// Path of the 7z binary. Behind a lock so the on-demand bootstrap can
    /// install it after the extractor was constructed.
    seven_zip: std::sync::RwLock<String>,
    workers: usize,
    ingest_progress: Option<IngestProgressFn>,
    /// Extraction progress, 0–100 (context-derived `Extract` handler).
    extract_progress: Option<Box<dyn Fn(i32) + Send + Sync>>,
}

impl std::fmt::Debug for Extractor {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Extractor")
            .field("temp_dir", &self.temp_dir)
            .field("use7z", &self.use7z.get())
            .field("seven_zip", &self.seven_zip_path())
            .field("workers", &self.workers)
            .finish()
    }
}

impl Extractor {
    /// Create an extractor. `store` is required by [`Extractor::extract_file`]
    /// and optional for [`Extractor::extract_to_dir`].
    pub fn new(store: Option<Store>) -> Extractor {
        Extractor {
            store,
            temp_dir: std::env::temp_dir(),
            use7z: std::sync::OnceLock::new(),
            seven_zip: std::sync::RwLock::new(String::new()),
            workers: 4,
            ingest_progress: None,
            extract_progress: None,
        }
    }

    /// Set the worker count for 7z `-mmt` and the parallel ingest pool.
    pub fn set_workers(&mut self, n: usize) {
        self.workers = n.max(1);
    }

    /// Set the callback for file-level ingest progress.
    pub fn set_ingest_progress(&mut self, f: impl Fn(i64, i64) + Send + Sync + 'static) {
        self.ingest_progress = Some(Box::new(f));
    }

    /// Set the extraction percentage callback.
    pub fn set_extract_progress(&mut self, f: impl Fn(i32) + Send + Sync + 'static) {
        self.extract_progress = Some(Box::new(f));
    }

    /// Force a specific 7z executable (implies `use7z`).
    ///
    /// Interior mutability: the on-demand 7z bootstrap installs the binary
    /// after [`crate::engine::Services`] (and its extractor) already exist.
    pub fn set_7z_path(&self, path: impl Into<String>) {
        if let Ok(mut slot) = self.seven_zip.write() {
            *slot = path.into();
        }
    }

    /// Whether an external 7z is usable. An explicitly configured path always
    /// wins; otherwise `7z.exe` is probed once per process, on first use.
    fn seven_zip_usable(&self) -> bool {
        if !self.seven_zip_path().is_empty() {
            return true;
        }
        *self.use7z.get_or_init(check_7z_available)
    }

    /// The configured 7z path, if any.
    pub fn seven_zip_path(&self) -> String {
        self.seven_zip
            .read()
            .map(|slot| slot.clone())
            .unwrap_or_default()
    }

    /// The store handle, if any.
    pub fn store(&self) -> Option<&Store> {
        self.store.as_ref()
    }

    // ------------------------------------------------------------------
    // Format detection
    // ------------------------------------------------------------------

    /// Detect the archive type from a path's extension.
    pub fn detect_type(&self, path: &Path) -> Result<String> {
        detect_type(path)
    }

    /// Detect the archive type from magic bytes.
    pub fn detect_type_from_content(data: &[u8]) -> String {
        detect_type_from_content(data)
    }

    // ------------------------------------------------------------------
    // Extraction
    // ------------------------------------------------------------------

    /// Extract an archive into the CAS store.
    ///
    /// `archive_type` is optional; when empty the type is detected from
    /// `archive_path`. Returns `(archiveHash, relPath → blobHash)`.
    pub fn extract_file(
        &self,
        archive_path: &Path,
        archive_type: Option<&str>,
    ) -> Result<(String, BTreeMap<String, String>)> {
        let at = match archive_type {
            Some(t) if !t.is_empty() => t.to_string(),
            _ => self
                .detect_type(archive_path)
                .map_err(|e| ExtractError::DetectType(e.to_string()))?,
        };
        if at == "zip" {
            return self.extract_zip_to_store(archive_path);
        }
        if at == "tar" {
            return self.extract_tar_to_store(archive_path);
        }
        // Hybrid 7z: decode `.7z` and 7z self-extracting `.exe` (`#/dl.7z`) in
        // process; fall back to the external tool only for non-7z or codecs this
        // build cannot decode. Unsafe members abort (never fall back).
        if at == "7z" || at == "exe" {
            let (tmp, cleanup) = self.new_extract_temp_dir("extract")?;
            // This path ingests from the extracted tree, so the per-file
            // hashes would be thrown away.
            let mut hashes = ExtractHashes::new();
            match self.extract_7z_into_dir(archive_path, &tmp, &mut hashes) {
                Ok(SevenZTried::Handled) => {
                    let files = self.ingest_extracted_dir(&tmp)?;
                    cleanup();
                    let archive_hash =
                        crate::hash::hash_file(archive_path).map_err(ExtractError::ReadArchive)?;
                    return Ok((archive_hash, files));
                }
                Ok(_) => {
                    cleanup();
                }
                Err(e) => {
                    cleanup();
                    return Err(e);
                }
            }
        }
        // 7z / xz / gz / bz2 / tar / msi / exe — external tool.
        self.extract_with_7z(archive_path, &at)
    }

    /// Extract an archive directly into `dest_dir`.
    ///
    /// `archive_name` is the original download filename (e.g. `pkg.tar.gz`) and
    /// guides format detection, since the store blob it lives in has no
    /// extension of its own.
    pub fn extract_to_dir(
        &self,
        archive_path: &Path,
        dest_dir: &Path,
        archive_name: &str,
    ) -> Result<ExtractHashes> {
        fs::create_dir_all(dest_dir).map_err(ExtractError::CreateDestDir)?;
        if let Some(f) = &self.extract_progress {
            f(0);
        }

        let name = if archive_name.is_empty() {
            archive_path
                .file_name()
                .map(|s| s.to_string_lossy().into_owned())
                .unwrap_or_default()
        } else {
            archive_name.to_string()
        };

        if is_zip_name(&name) {
            return self.extract_zip_to_dir(archive_path, dest_dir);
        }

        if is_tar_name(&name) {
            return self.extract_tar_to_dir(archive_path, dest_dir);
        }

        // Hybrid 7z for `.7z`/`.7z.exe` and any `.exe` that is really a 7z
        // self-extracting archive (`#/dl.7z`); a plain installer that is not a
        // 7z falls through to the external tool. Uses a lowercased probe so
        // `.EXE`/`.7Z` are matched too.
        let lname = name.to_ascii_lowercase();
        if lname.ends_with(".7z") || lname.ends_with(".7z.exe") || lname.ends_with(".exe") {
            let mut hashes = ExtractHashes::new();
            match self.extract_7z_into_dir(archive_path, dest_dir, &mut hashes) {
                Ok(outcome) => {
                    trace(format!(
                        "in-process 7z for {name}: {}",
                        match &outcome {
                            SevenZTried::Handled => "extracted",
                            SevenZTried::NotSevenZip => "not a 7z container",
                            SevenZTried::Fallback => "falling back to the external tool",
                        }
                    ));
                    if matches!(outcome, SevenZTried::Handled) {
                        return Ok(hashes);
                    }
                    // A partial in-process pass may have written members that the
                    // external tool now overwrites; their recorded hashes no
                    // longer describe what ends up on disk.
                    hashes.clear();
                }
                Err(e) => {
                    trace(format!("in-process 7z for {name}: failed: {e}"));
                    return Err(e);
                }
            }
        }

        if !self.seven_zip_usable() {
            return Err(ExtractError::SevenZipMissing);
        }
        let stage_path = self.stage_archive_for_7z(archive_path, &name)?;
        let seven_zip = self.seven_zip_binary();
        trace(format!(
            "external 7z ({seven_zip}) for {name}: archive handed over as {} ({} bytes)",
            stage_path
                .file_name()
                .map(|name| name.to_string_lossy().into_owned())
                .unwrap_or_default(),
            std::fs::metadata(&stage_path)
                .map(|meta| meta.len().to_string())
                .unwrap_or_else(|_| "unknown".to_owned())
        ));
        let result = self.run_7z_extract(
            &seven_zip,
            &stage_path,
            dest_dir,
            1,
            extract_stages_for_name(&name),
        );
        if let Err(e) = result {
            self.cleanup_stage(&stage_path, archive_path);
            return Err(e);
        }
        self.cleanup_stage(&stage_path, archive_path);

        // Compound formats (tar.gz, tar.bz2, tar.xz) leave the inner .tar
        // behind after 7z unwraps the outer layer.
        self.extract_nested_tar(&seven_zip, dest_dir, &name)?;
        // The external tool writes the files; they are hashed by adopt.
        Ok(ExtractHashes::new())
    }

    /// Extract a ZIP straight into `dest_dir`, no CAS round-trip.
    ///
    /// Members are decompressed on `self.workers` threads, each with its own
    /// archive handle: deflate is CPU-bound, and one member at a time leaves
    /// every other core idle on a multi-file package. Payload ranges are
    /// resolved up front from the local headers (serial, ~30 bytes each) so
    /// the workers never share a file position.
    ///
    /// Returns the SHA-256 of every member it wrote, keyed by file identity,
    /// so the deploy pass can adopt them without a second read.
    fn extract_zip_to_dir(&self, archive_path: &Path, dest_dir: &Path) -> Result<ExtractHashes> {
        // Drop counters left by another extraction on this thread (e.g. the
        // CAS-ingest path) so they never leak into this worker set.
        let _ = crate::extract_profile::take_local();
        struct Job {
            entry: crate::ziparchive::ZipEntry,
            target: PathBuf,
            data: crate::ziparchive::MemberData,
            /// Install-relative member path, for the adopt fast path.
            rel: String,
        }

        let mut archive = ZipArchive::open(archive_path)?;
        let entries: Vec<crate::ziparchive::ZipEntry> =
            archive.file_entries().into_iter().cloned().collect();
        let total = entries.len() as i64;
        if let Some(f) = &self.ingest_progress {
            f(0, total);
        }

        let mut jobs: Vec<Job> = Vec::with_capacity(entries.len());
        // Archives with case-insensitive name collisions (or exact duplicates)
        // must keep central-directory order: on Windows both members map to
        // one file and "last member wins" is the behaviour serial extraction
        // and 7-Zip agree on.
        let mut seen_targets: std::collections::BTreeSet<String> =
            std::collections::BTreeSet::new();
        let mut ordering_matters = false;
        for entry in entries {
            let rel = crate::archmember::normalize_member(&entry.name);
            let safe = crate::safepath::validate_manifest_rel_path(&rel).map_err(|e| {
                ExtractError::UnsafeMember {
                    path: rel.clone(),
                    message: e.to_string(),
                }
            })?;
            if !seen_targets.insert(safe.to_lowercase()) {
                ordering_matters = true;
            }
            let target = dest_dir.join(safe.replace('/', std::path::MAIN_SEPARATOR_STR));
            let data = archive.member_data(&entry)?;
            jobs.push(Job {
                entry,
                target,
                data,
                rel: safe,
            });
        }
        // Workers open their own handles; the parsing handle is not needed.
        drop(archive);

        if jobs.is_empty() {
            if let Some(f) = &self.extract_progress {
                f(100);
            }
            return Ok(ExtractHashes::new());
        }

        let workers = if ordering_matters {
            1
        } else {
            self.workers.max(1).min(jobs.len())
        };
        let cursor = AtomicUsize::new(0);
        let processed = AtomicU64::new(0);
        // First error wins; every worker stops taking new members once set.
        let first_err: std::sync::Mutex<Option<ExtractError>> = std::sync::Mutex::new(None);
        let collected: std::sync::Mutex<ExtractHashes> =
            std::sync::Mutex::new(ExtractHashes::new());
        // Serializes the progress callback so a terminal sink cannot interleave.
        let progress_lock = std::sync::Mutex::new(());
        // Opt-in GLUE_EXTRACT_PROFILE counters; per-worker values are merged
        // here when each worker exits.
        let profile = std::sync::Mutex::new(crate::extract_profile::Counters::default());

        std::thread::scope(|scope| {
            for _ in 0..workers {
                let cursor = &cursor;
                let processed = &processed;
                let first_err = &first_err;
                let collected = &collected;
                let progress_lock = &progress_lock;
                let profile = &profile;
                let jobs = &jobs;
                scope.spawn(move || {
                    let mut file = match fs::File::open(archive_path) {
                        Ok(file) => file,
                        Err(e) => {
                            let mut slot = first_err.lock().unwrap_or_else(|p| p.into_inner());
                            if slot.is_none() {
                                *slot = Some(ExtractError::ReadArchive(e));
                            }
                            return;
                        }
                    };
                    loop {
                        if first_err
                            .lock()
                            .unwrap_or_else(|p| p.into_inner())
                            .is_some()
                        {
                            break;
                        }
                        let index = cursor.fetch_add(1, Ordering::Relaxed);
                        let Some(job) = jobs.get(index) else {
                            break;
                        };
                        let result = (|| -> Result<()> {
                            let setup_start = crate::extract_profile::start();
                            if let Some(parent) = job.target.parent() {
                                fs::create_dir_all(parent).map_err(ExtractError::WriteFile)?;
                            }
                            let out =
                                fs::File::create(&job.target).map_err(ExtractError::WriteFile)?;
                            crate::extract_profile::add_setup(setup_start);
                            let mut sink = HashingWriter::new(out);
                            crate::ziparchive::extract_member(
                                &mut file, job.data, &job.entry, &mut sink,
                            )?;
                            let finish_start = crate::extract_profile::start();
                            let hex = sink.finish();
                            crate::extract_profile::add_finish(finish_start);
                            crate::extract_profile::add_file();
                            // Path key first: adopt walks the finished tree and
                            // matches it without opening the file again.
                            let mut hashes = collected.lock().unwrap_or_else(|p| p.into_inner());
                            if let Some(key) = crate::cache::fileid::file_ref_key(&job.target) {
                                hashes.insert(key, hex.clone());
                            }
                            hashes.insert(rel_hash_key(&job.rel), hex);
                            Ok(())
                        })();
                        match result {
                            Ok(()) => {
                                let done = processed.fetch_add(1, Ordering::Relaxed) + 1;
                                if let Some(cb) = &self.ingest_progress {
                                    let _guard =
                                        progress_lock.lock().unwrap_or_else(|p| p.into_inner());
                                    cb(done as i64, total);
                                }
                            }
                            Err(e) => {
                                let mut slot = first_err.lock().unwrap_or_else(|p| p.into_inner());
                                if slot.is_none() {
                                    *slot = Some(e);
                                }
                                break;
                            }
                        }
                    }
                    let local = crate::extract_profile::take_local();
                    let mut totals = profile.lock().unwrap_or_else(|p| p.into_inner());
                    crate::extract_profile::merge(&mut totals, local);
                });
            }
        });

        let mut counters = profile.into_inner().unwrap_or_else(|p| p.into_inner());
        counters.workers = workers as u64;
        crate::extract_profile::store_last("zip", counters);

        if let Some(e) = first_err.into_inner().unwrap_or_else(|p| p.into_inner()) {
            return Err(e);
        }
        if let Some(f) = &self.extract_progress {
            f(100);
        }
        Ok(collected.into_inner().unwrap_or_else(|p| p.into_inner()))
    }

    /// Extract a ZIP into the CAS store, returning the member map.
    ///
    /// Members are written with [`Store::write_member`] so large members take
    /// the dedup path and small ones the single-read path
    fn extract_zip_to_store(
        &self,
        archive_path: &Path,
    ) -> Result<(String, BTreeMap<String, String>)> {
        // The store is required by `ingest_zip_member`; fail fast here with a
        // clearer message than a downstream retry would give.
        if self.store.is_none() {
            return Err(ExtractError::Ingest(
                "no store configured for extract_file".to_string(),
            ));
        }
        let mut archive = ZipArchive::open(archive_path)?;
        let entries: Vec<crate::ziparchive::ZipEntry> =
            archive.file_entries().into_iter().cloned().collect();

        let total = entries.len() as i64;
        if let Some(f) = &self.ingest_progress {
            f(0, total);
        }

        let mut files = BTreeMap::new();
        for (i, entry) in entries.iter().enumerate() {
            let rel = crate::archmember::normalize_member(&entry.name);
            // Reject zip-slip members before they can reach the store or, later,
            // an install directory.
            crate::safepath::validate_manifest_rel_path(&rel).map_err(|e| {
                ExtractError::UnsafeMember {
                    path: rel.clone(),
                    message: e.to_string(),
                }
            })?;
            let hash = self.ingest_zip_member(&mut archive, entry)?;
            files.insert(rel, hash);
            if let Some(f) = &self.ingest_progress {
                f(i as i64 + 1, total);
            }
        }
        if let Some(f) = &self.extract_progress {
            f(100);
        }
        let archive_hash =
            crate::hash::hash_file(archive_path).map_err(ExtractError::ReadArchive)?;
        Ok((archive_hash, files))
    }

    // ------------------------------------------------------------------
    // In-process tar (plain tar / tar.gz / tgz)
    // ------------------------------------------------------------------

    /// Extract a tar (or gzip-compressed tar) straight into `dest_dir`, no
    /// CAS round-trip.
    ///
    /// Mirrors the ZIP path's safety checks: members are normalized and
    /// zip-slip-validated before touching the filesystem. Hardlinks resolve
    /// against previously extracted members of the same archive; symlinks are
    /// materialised as small files holding the link target — 7-Zip's default
    /// behaviour without `-snl`, and the only safe option: a real symlink
    /// with an attacker-chosen target would let later members written beneath
    /// it escape `dest_dir` (write-through). Unsupported member types
    /// (devices, fifos) fail loudly.
    fn extract_tar_to_dir(&self, archive_path: &Path, dest_dir: &Path) -> Result<ExtractHashes> {
        let mut archive = TarArchive::open(archive_path)?;
        let mut hardlinks: std::collections::HashMap<String, PathBuf> =
            std::collections::HashMap::new();
        let mut hashes = ExtractHashes::new();
        if let Some(f) = &self.extract_progress {
            f(0);
        }
        while let Some(entry) = archive.next_entry()? {
            let normalized = crate::archmember::normalize_member(&entry.name);
            let rel = crate::safepath::validate_manifest_rel_path(&normalized).map_err(|e| {
                ExtractError::UnsafeMember {
                    path: normalized,
                    message: e.to_string(),
                }
            })?;
            let rel_trimmed = rel.trim_end_matches('/');
            if rel_trimmed.is_empty() {
                continue; // "." / "./" root entry from BSD/GNU tar
            }
            let target = dest_dir.join(rel_trimmed.replace('/', std::path::MAIN_SEPARATOR_STR));
            if entry.is_file() {
                if let Some(parent) = target.parent() {
                    fs::create_dir_all(parent).map_err(ExtractError::WriteFile)?;
                }
                let out = fs::File::create(&target).map_err(ExtractError::WriteFile)?;
                let mut out = HashingWriter::new(out);
                archive.copy_entry(&entry, &mut out)?;
                record_file_hash(&mut hashes, &target, rel_trimmed, out.finish());
                hardlinks.insert(strip_dot_slash(rel_trimmed), target.clone());
            } else if entry.is_dir() {
                let _ = fs::create_dir_all(&target);
            } else if entry.typeflag == b'1' {
                let src = hardlinks
                    .get(&strip_dot_slash(&entry.linkname))
                    .cloned()
                    .ok_or_else(|| TarError::HardlinkTargetMissing {
                        target: entry.linkname.clone(),
                        path: entry.name.clone(),
                    })?;
                fs::hard_link(&src, &target).map_err(TarError::Write)?;
            } else if entry.typeflag == b'2' {
                // Materialise as a file holding the link target, matching
                // 7-Zip without `-snl`. A real symlink would be a
                // write-through primitive: a later member under `target`
                // would follow the link out of `dest_dir`.
                if let Some(parent) = target.parent() {
                    fs::create_dir_all(parent).map_err(ExtractError::WriteFile)?;
                }
                fs::write(&target, &entry.linkname).map_err(ExtractError::WriteFile)?;
                record_file_hash(
                    &mut hashes,
                    &target,
                    rel_trimmed,
                    crate::hash::hash_bytes(entry.linkname.as_bytes()),
                );
            } else {
                return Err(TarError::UnsupportedMemberType {
                    typeflag: entry.typeflag,
                    path: entry.name.clone(),
                }
                .into());
            }
        }
        if let Some(f) = &self.extract_progress {
            f(100);
        }
        Ok(hashes)
    }

    /// Extract a tar into the CAS store, returning
    /// `(archiveHash, relPath → blobHash)` — the tar counterpart of
    /// [`Extractor::extract_zip_to_store`].
    ///
    /// Per-member ingest progress is not reported: tar is sequential, so the
    /// member total is unknown up front (the ZIP path learns it from the
    /// central directory).
    fn extract_tar_to_store(
        &self,
        archive_path: &Path,
    ) -> Result<(String, BTreeMap<String, String>)> {
        if self.store.is_none() {
            return Err(ExtractError::Ingest(
                "no store configured for extract_file".to_string(),
            ));
        }
        let mut archive = TarArchive::open(archive_path)?;
        if let Some(f) = &self.extract_progress {
            f(0);
        }
        let mut files = BTreeMap::new();
        while let Some(entry) = archive.next_entry()? {
            if !entry.is_file() {
                continue;
            }
            let normalized = crate::archmember::normalize_member(&entry.name);
            let rel = crate::safepath::validate_manifest_rel_path(&normalized).map_err(|e| {
                ExtractError::UnsafeMember {
                    path: normalized,
                    message: e.to_string(),
                }
            })?;
            if rel.is_empty() {
                continue;
            }
            let hash = self.ingest_tar_member(&mut archive, &entry)?;
            files.insert(rel, hash);
        }
        if let Some(f) = &self.extract_progress {
            f(100);
        }
        let archive_hash =
            crate::hash::hash_file(archive_path).map_err(ExtractError::ReadArchive)?;
        Ok((archive_hash, files))
    }

    /// Ingest one tar member into the store. Small members take the
    /// in-memory `write_member` path (which never retries below its 2 MiB
    /// ceiling); larger ones spool through a temp file. Unlike the ZIP path
    /// there is no re-read retry: a tar payload cannot be decompressed
    /// twice.
    fn ingest_tar_member(&self, archive: &mut TarArchive, entry: &TarEntry) -> Result<String> {
        let store = self.store.as_ref().ok_or_else(|| {
            ExtractError::Ingest("no store configured for extract_file".to_string())
        })?;
        /// Matches `Store::write_member`'s in-memory ceiling.
        const MAX_SMALL_MEMBER_BYTES: u64 = 2 << 20;
        if entry.size <= MAX_SMALL_MEMBER_BYTES {
            let mut data = Vec::with_capacity(entry.size as usize);
            archive.copy_entry(entry, &mut data)?;
            store
                .write_member(data.as_slice(), entry.size as i64, true)
                .map_err(ExtractError::Store)
        } else {
            let mut spool = StoreSpool::new(store)?;
            archive.copy_entry(entry, &mut spool)?;
            spool.finish()
        }
    }

    /// Write one ZIP member into the CAS store.
    ///
    /// Small members go through [`Store::write_member`]'s single-read path.
    /// Larger ones cannot: `write_member` hashes first and returns
    /// `NeedReaderRetry` when the blob turns out to be new, at which point the
    /// caller must supply a fresh reader. Here that
    /// means decompressing the member a second time, straight into the store.
    fn ingest_zip_member(
        &self,
        archive: &mut ZipArchive,
        entry: &crate::ziparchive::ZipEntry,
    ) -> Result<String> {
        let store = self.store.as_ref().ok_or_else(|| {
            ExtractError::Ingest("no store configured for extract_file".to_string())
        })?;
        let size = entry.uncompressed_size as i64;

        /// Matches `Store::write_member`'s in-memory ceiling.
        const MAX_SMALL_MEMBER_BYTES: i64 = 2 << 20;
        if !(0..=MAX_SMALL_MEMBER_BYTES).contains(&size) {
            // Large member: stream it straight into the store to avoid holding
            // the whole thing in memory. Dedup still happens inside `write`,
            // which is content-addressed.
            let mut spool = StoreSpool::new(store)?;
            archive.copy_entry(entry, &mut spool)?;
            return spool.finish();
        }

        let mut data = Vec::with_capacity(size as usize);
        archive.copy_entry(entry, &mut data)?;
        match store.write_member(data.as_slice(), size, true) {
            Ok(hash) => Ok(hash),
            // The blob is new and the header sizes said "small", but
            // `write_member` still asked for a retry: re-stream it.
            Err(crate::store::StoreError::NeedReaderRetry) => {
                let mut spool = StoreSpool::new(store)?;
                archive.copy_entry(entry, &mut spool)?;
                spool.finish()
            }
            Err(e) => Err(ExtractError::Store(e)),
        }
    }

    // ------------------------------------------------------------------
    // External 7z
    // ------------------------------------------------------------------

    /// Whether 7z is available.
    pub fn uses_7z(&self) -> bool {
        self.seven_zip_usable()
    }

    /// The 7z binary to invoke (configured path or `7z.exe` on PATH).
    pub fn seven_zip_binary(&self) -> String {
        let configured = self.seven_zip_path();
        if configured.is_empty() {
            "7z.exe".to_string()
        } else {
            configured
        }
    }

    /// Offset of a 7z signature within the file (0 for a plain `.7z`, >0 for a
    /// self-extracting stub). `None` when there is no signature in the scanned
    /// prefix — i.e. this is not a 7z container.
    fn seven_zip_signature_offset(file: &mut fs::File) -> Option<u64> {
        use std::io::{Read, Seek, SeekFrom};
        file.seek(SeekFrom::Start(0)).ok()?;
        let mut buf = vec![0u8; SFX_SCAN_LIMIT];
        let n = file.read(&mut buf).unwrap_or(0);
        buf.truncate(n);
        if n < SEVEN_ZIP_MAGIC.len() {
            return None;
        }
        buf.windows(SEVEN_ZIP_MAGIC.len())
            .position(|w| w == SEVEN_ZIP_MAGIC)
            .map(|i| i as u64)
    }

    /// Extract a `.7z` — or an `.exe` that is really a 7z self-extracting
    /// archive (`#/dl.7z`, e.g. Zoom) — in process, writing members into
    /// `dest_dir`. Every member is normalized and checked with `safepath`
    /// before it is written, so the in-process path is as safe as the ZIP
    /// reader (defense in depth over the crate, cf. RUSTSEC-2026-0245 on the
    /// abandoned `sevenz-rust`).
    ///
    /// [`SevenZTried`] tells external 7z.exe whether to take over: a genuine
    /// unsafe-member abort is surfaced as `Err`, never retried externally. A
    /// codec this build lacks (ppmd/bzip2) or a parse failure reports
    /// [`SevenZTried::Fallback`] so the external tool can try.
    fn extract_7z_into_dir(
        &self,
        archive_path: &Path,
        dest_dir: &Path,
        hashes: &mut ExtractHashes,
    ) -> Result<SevenZTried> {
        let mut file = fs::File::open(archive_path).map_err(ExtractError::ReadArchive)?;
        let offset = match Self::seven_zip_signature_offset(&mut file) {
            Some(off) => off,
            None => return Ok(SevenZTried::NotSevenZip),
        };
        // sevenz-rust2 seeks absolutely (`seek(End(0))` for the archive length,
        // then `seek(Start(0))` to read the signature), so a bare `File` would
        // look at the SFX stub and fail with `BadSignature`. `OffsetReader`
        // re-bases every seek on the archive start.
        let reader = OffsetReader { file, base: offset };

        let dest = dest_dir.to_path_buf();
        let bad_member: std::cell::RefCell<Option<String>> =
            const { std::cell::RefCell::new(None) };
        let res =
            sevenz_rust2::decompress_with_extract_fn(reader, &dest, |entry, reader, _computed| {
                let name = entry.name().to_string();
                // Directory markers (trailing slash) create no file.
                if name.ends_with('/') || name.ends_with('\\') {
                    return Ok(true);
                }
                let rel = crate::archmember::normalize_member(&name);
                match crate::safepath::join_under_base(&dest, &rel) {
                    Ok(target) => {
                        if let Some(parent) = target.parent() {
                            std::fs::create_dir_all(parent).map_err(|e| {
                                sevenz_error(format!("create dir {:?}: {e}", parent))
                            })?;
                        }
                        let out = fs::File::create(&target)
                            .map_err(|e| sevenz_error(format!("create {target:?}: {e}")))?;
                        let mut out = HashingWriter::new(out);
                        // 128 KiB manual loop, NOT `io::copy`: on this
                        // toolchain `io::copy` asks the decoder for 8 KiB and
                        // writes 8 KiB per `WriteFile` (measured ~450 MB/s vs
                        // ~1.7 GB/s at 128 KiB in the CAS path, fix #1).
                        // sevenz-rust2's decode stack has no internal output
                        // buffer of its own, so the caller's request size is
                        // both the decode granularity and the write size.
                        let mut buf = [0u8; 128 * 1024];
                        loop {
                            let n = reader
                                .read(&mut buf)
                                .map_err(|e| sevenz_error(format!("write {target:?}: {e}")))?;
                            if n == 0 {
                                break;
                            }
                            out.write_all(&buf[..n])
                                .map_err(|e| sevenz_error(format!("write {target:?}: {e}")))?;
                        }
                        let hex = out.finish();
                        record_file_hash(hashes, &target, &rel, hex);
                        Ok(true)
                    }
                    Err(e) => {
                        *bad_member.borrow_mut() = Some(name.clone());
                        Err(sevenz_error(format!("unsafe 7z member {name:?}: {e}")))
                    }
                }
            });
        match res {
            Ok(()) => {
                if let Some(f) = &self.extract_progress {
                    f(100);
                }
                Ok(SevenZTried::Handled)
            }
            Err(e) => {
                // A safe-path violation aborts the whole install instead of
                // falling back to an external tool that would honor the member.
                if let Some(name) = bad_member.borrow_mut().take() {
                    return Err(ExtractError::UnsafeMember {
                        path: name,
                        message: e.to_string(),
                    });
                }
                Ok(SevenZTried::Fallback)
            }
        }
    }

    /// Extract via the external 7z binary, ingesting into the store.
    fn extract_with_7z(
        &self,
        archive_path: &Path,
        archive_type: &str,
    ) -> Result<(String, BTreeMap<String, String>)> {
        if !self.seven_zip_usable() {
            return Err(ExtractError::SevenZipMissing);
        }
        let store = self.store.as_ref().ok_or_else(|| {
            ExtractError::Ingest("no store configured for extract_file".to_string())
        })?;
        let (extract_dir, cleanup) = self.new_extract_temp_dir("extract")?;

        let seven_zip = self.seven_zip_binary();
        let stages = extract_stages(archive_path, archive_type);
        let result = self.run_7z_extract(&seven_zip, archive_path, &extract_dir, 1, stages);
        if let Err(e) = result {
            cleanup();
            return Err(e);
        }

        // Compound formats leave an inner .tar that needs a second pass.
        let nested = find_nested_tar(&extract_dir);
        let intermediate_hash = match nested {
            Some(tar_file) => {
                self.cache_and_extract_nested_tar(&seven_zip, &tar_file, &extract_dir)?
            }
            None => {
                if stages > 1 {
                    self.finish_compound_progress();
                }
                String::new()
            }
        };

        let files = self.ingest_extracted_dir(&extract_dir)?;
        cleanup();

        let mut files = files;
        // A leading dot marks intermediate blobs; install skips these.
        if !intermediate_hash.is_empty() {
            files.insert(".intermediate.tar".to_string(), intermediate_hash);
        }
        let _ = store;
        let archive_hash =
            crate::hash::hash_file(archive_path).map_err(ExtractError::ReadArchive)?;
        Ok((archive_hash, files))
    }

    /// Cache the intermediate tar and extract it as a second pass.
    fn cache_and_extract_nested_tar(
        &self,
        seven_zip: &str,
        tar_file: &Path,
        extract_dir: &Path,
    ) -> Result<String> {
        let mut intermediate_hash = String::new();
        if let (Some(store), Ok(data)) = (self.store.as_ref(), fs::read(tar_file)) {
            if let Ok(hash) = store.write(data.as_slice()) {
                intermediate_hash = hash;
            }
        }
        self.run_7z_extract(seven_zip, tar_file, extract_dir, 2, 2)?;
        let _ = fs::remove_file(tar_file);
        Ok(intermediate_hash)
    }

    /// Handle the inner `.tar` left by a compound archive in `dest_dir`.
    fn extract_nested_tar(
        &self,
        seven_zip: &str,
        dest_dir: &Path,
        archive_name: &str,
    ) -> Result<()> {
        let nested = find_nested_tar(dest_dir);
        match nested {
            Some(tar_file) => {
                self.run_7z_extract(seven_zip, &tar_file, dest_dir, 2, 2)?;
                let _ = fs::remove_file(tar_file);
                Ok(())
            }
            None => {
                if extract_stages_for_name(archive_name) > 1 {
                    self.finish_compound_progress();
                }
                Ok(())
            }
        }
    }

    /// Create a unique scratch directory and its cleanup closure.
    fn new_extract_temp_dir(&self, prefix: &str) -> Result<(PathBuf, Box<dyn Fn()>)> {
        let dir = self
            .temp_dir
            .join(format!("{prefix}-{}", unique_temp_name()));
        fs::create_dir_all(&dir).map_err(ExtractError::CreateTempDir)?;
        let cleanup_dir = dir.clone();
        Ok((
            dir,
            Box::new(move || {
                let _ = fs::remove_dir_all(&cleanup_dir);
            }),
        ))
    }

    /// Build the 7z argument list.
    pub fn build_7z_extract_args(
        &self,
        dest_dir: &Path,
        archive_path: &Path,
        enable_progress: bool,
    ) -> Vec<String> {
        let mut args: Vec<String> = Vec::new();
        if self.workers > 1 {
            args.push(format!("-mmt={}", self.workers));
        }
        if enable_progress {
            // -bso0 hides the file list; -bsp1 emits percentage on stdout.
            args.push("-bso0".to_string());
            args.push("-bsp1".to_string());
        }
        args.push("x".to_string());
        args.push("-y".to_string());
        args.push(format!("-o{}", dest_dir.display()));
        args.push(archive_path.display().to_string());
        args
    }

    /// Run one 7z extraction pass, feeding progress through the parser.
    fn run_7z_extract(
        &self,
        seven_zip: &str,
        archive_path: &Path,
        dest_dir: &Path,
        stage: usize,
        total_stages: usize,
    ) -> Result<()> {
        let enable_progress = self.extract_progress.is_some();
        let args = self.build_7z_extract_args(dest_dir, archive_path, enable_progress);

        let mut cmd = Command::new(seven_zip);
        cmd.args(&args);
        crate::procutil::hide_window(&mut cmd);
        let output = cmd.output().map_err(|e| {
            if e.kind() == std::io::ErrorKind::NotFound {
                ExtractError::SevenZipMissing
            } else {
                ExtractError::SevenZipFailed(e.to_string())
            }
        })?;

        if enable_progress {
            // Progress arrives on stdout as `-bsp1` percentage lines.
            let stdout = String::from_utf8_lossy(&output.stdout);
            let percents = std::cell::RefCell::new(Vec::new());
            {
                let mut parser =
                    Progress7zParser::new(|percent| percents.borrow_mut().push(percent));
                use std::io::Write;
                let _ = parser.write_all(stdout.as_bytes());
            }
            if let Some(f) = &self.extract_progress {
                for percent in percents.into_inner() {
                    f(scale_extract_percent(
                        percent,
                        stage as i32,
                        total_stages as i32,
                    ));
                }
            }
        }

        if !output.status.success() {
            // The log keeps the untouched output; the error message carries only
            // the lines that explain the failure, because that string is what
            // the user sees in the task list.
            trace(format!(
                "7z failed for {}: exit status {}\nStdout: {}\nStderr: {}",
                archive_path.display(),
                output.status,
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr),
            ));
            return Err(ExtractError::SevenZipFailed(summarize_7z_output(
                output.status.code(),
                &output.stdout,
                &output.stderr,
            )));
        }
        Ok(())
    }

    /// Copy the archive into temp with a recognizable extension when 7z needs
    /// one. Returns the staged path, or the original.
    fn stage_archive_for_7z(&self, archive_path: &Path, archive_name: &str) -> Result<PathBuf> {
        let ext = stage_extension(archive_name, has_pe_header(archive_path));
        if ext.is_empty()
            || archive_path
                .extension()
                .map(|e| format!(".{}", e.to_string_lossy()).eq_ignore_ascii_case(&ext))
                .unwrap_or(false)
        {
            return Ok(archive_path.to_path_buf());
        }
        let tmp = self
            .temp_dir
            .join(format!("stage-{}{}", unique_temp_name(), ext));
        if let Some(parent) = tmp.parent() {
            let _ = fs::create_dir_all(parent);
        }
        if fs::hard_link(archive_path, &tmp).is_err() {
            fs::copy(archive_path, &tmp).map_err(ExtractError::ReadArchive)?;
        }
        Ok(tmp)
    }

    /// Remove a staged copy (never the original).
    fn cleanup_stage(&self, staged: &Path, original: &Path) {
        if staged != original {
            let _ = fs::remove_file(staged);
        }
    }

    /// Write every file under `root` into the CAS store using a worker pool
    ///
    /// Fans this out over `e.workers` goroutines with a semaphore, records
    /// the *first* error and still attempts every remaining entry. The implementation
    /// mirrors that: workers steal indices from a cursor, a failed entry does
    /// not stop the others, and the returned error is the first one observed.
    /// Per-file CAS writes dominate deploy time (measured ~64–135 ms/file on a
    /// virtualized host), so the fan-out is the whole point.
    ///
    /// Output order is deterministic regardless of completion order: the map
    /// is a `BTreeMap` keyed by relative path.
    pub fn ingest_extracted_dir(&self, root: &Path) -> Result<BTreeMap<String, String>> {
        let store = self.store.as_ref().ok_or_else(|| {
            ExtractError::Ingest("no store configured for extract_file".to_string())
        })?;
        let entries = list_extracted_files(root)?;
        let total = entries.len() as i64;
        if let Some(f) = &self.ingest_progress {
            f(0, total);
        }
        if entries.is_empty() {
            return Ok(BTreeMap::new());
        }

        let workers = self.workers.max(1).min(entries.len());
        let cursor = AtomicUsize::new(0);
        let processed = AtomicU64::new(0);
        // First error wins; every entry is still attempted.
        let first_err: std::sync::Mutex<Option<ExtractError>> = std::sync::Mutex::new(None);
        let collected: std::sync::Mutex<Vec<(String, String)>> =
            std::sync::Mutex::new(Vec::with_capacity(entries.len()));
        // Serializes the progress callback so a terminal sink cannot interleave
        let progress_lock = std::sync::Mutex::new(());

        std::thread::scope(|scope| {
            let cursor = &cursor;
            let processed = &processed;
            let first_err = &first_err;
            let collected = &collected;
            let progress_lock = &progress_lock;
            let entries = &entries;
            for _ in 0..workers {
                scope.spawn(move || loop {
                    let i = cursor.fetch_add(1, Ordering::Relaxed);
                    if i >= entries.len() {
                        break;
                    }
                    let (path, rel) = &entries[i];
                    let result = fs::File::open(path)
                        .map_err(ExtractError::ReadArchive)
                        .and_then(|f| store.write(f).map_err(ExtractError::Store));
                    match result {
                        Ok(hash) => {
                            collected.lock().unwrap().push((rel.clone(), hash));
                            let done = processed.fetch_add(1, Ordering::Relaxed) + 1;
                            if let Some(cb) = &self.ingest_progress {
                                let _guard = progress_lock.lock().unwrap();
                                cb(done as i64, total);
                            }
                        }
                        Err(e) => {
                            let mut slot = first_err.lock().unwrap();
                            if slot.is_none() {
                                *slot = Some(e);
                            }
                        }
                    }
                });
            }
        });

        if let Some(e) = first_err.into_inner().unwrap() {
            return Err(e);
        }
        if let Some(cb) = &self.ingest_progress {
            cb(total, total);
        }
        Ok(collected.into_inner().unwrap().into_iter().collect())
    }

    /// Emit the final 100% for a compound extraction.
    fn finish_compound_progress(&self) {
        if let Some(f) = &self.extract_progress {
            f(100);
        }
    }
}

/// Cached PATH probe: `7z.exe i` costs ~1.2 s here (process start + Defender),
/// and [`Extractor::new`] runs once per install/extract test — without caching
/// a full suite pays that dozens of times. The answer is process-stable (PATH
/// does not change under a running process), and `set_7z_path` still overrides
/// it. probes every call, but probe is a native spawn with no AV tax.
static SEVEN_ZIP_AVAILABLE: std::sync::OnceLock<bool> = std::sync::OnceLock::new();

/// Whether `7z.exe` answers on PATH.
///
/// Runs `7z.exe help` and treats a nil error as available. 7-Zip 26 changed
/// `help` to exit **7** (command-line error), so that probe reports a perfectly
/// good 7-Zip as missing — `i` (show information) exits 0 and proves the binary
/// runs. Deliberate divergence from, which papers over the same issue with
/// its `ResolveLocalSevenZip` chain.
pub fn check_7z_available() -> bool {
    *SEVEN_ZIP_AVAILABLE.get_or_init(|| {
        let mut cmd = Command::new("7z.exe");
        cmd.arg("i");
        crate::procutil::hide_window(&mut cmd);
        cmd.output().map(|o| o.status.success()).unwrap_or(false)
    })
}

/// True when `path` is a full-codec 7-Zip console build
/// `7z.dll` must sit beside the executable, because
/// NSIS/MSI/`#/dl.7z` extracts need the codecs bundled in 7-Zip 26+.
fn is_full_7z_binary(path: &Path) -> bool {
    path.is_file() && path.with_file_name("7z.dll").is_file()
}

/// Scan the OS PATH for an executable named `exe`.
fn find_exe_on_path(exe: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .filter(|dir| !dir.as_os_str().is_empty())
        .map(|dir| dir.join(exe))
        .find(|cand| cand.is_file())
}

/// Resolve a full-codec 7-Zip console binary under `glue_root` or on PATH
/// Never downloads — callers that
/// need the brand-new-machine bootstrap path fall through to that layer.
///
/// Preferencethe glue-root install (apps/7zip) first, then
/// the bootstrap bin, then PATH. Every candidate requires `7z.dll` beside it.
pub fn resolve_full_local_7z(glue_root: &Path) -> Option<PathBuf> {
    let mut candidates: Vec<PathBuf> = Vec::new();
    if !glue_root.as_os_str().is_empty() {
        let pkg_root = glue_root.join("apps").join("7zip");
        // The `current` link, then any concrete version directory.
        candidates.push(pkg_root.join("current").join("7z.exe"));
        if let Ok(entries) = std::fs::read_dir(&pkg_root) {
            for entry in entries.flatten() {
                if entry.path().is_dir() {
                    candidates.push(entry.path().join("7z.exe"));
                }
            }
        }
        // The full build the bootstrap layer installs here.
        candidates.push(glue_root.join("bin").join("7z.exe"));
    }
    // PATH last.
    for name in ["7z", "7z.exe"] {
        if let Some(p) = find_exe_on_path(name) {
            candidates.push(p);
        }
    }
    candidates.into_iter().find(|p| is_full_7z_binary(p))
}

/// Detect the archive type from a path extension.
pub fn detect_type(path: &Path) -> Result<String> {
    let ext = path
        .extension()
        .map(|e| format!(".{}", e.to_string_lossy().to_ascii_lowercase()))
        .unwrap_or_default();
    let base = path
        .file_name()
        .map(|s| s.to_string_lossy().to_ascii_lowercase())
        .unwrap_or_default();

    let t = match ext.as_str() {
        ".zip" | ".nupkg" => "zip",
        ".7z" => "7z",
        ".gz" if base.ends_with(".tar.gz") => "tar",
        ".gz" => "gz",
        ".bz2" => "bz2",
        ".xz" => "xz",
        ".tar" => "tar",
        ".msi" | ".msi_" => "msi",
        ".exe" => "exe",
        other => return Err(ExtractError::UnknownType(other.to_string())),
    };
    Ok(t.to_string())
}

/// Detect the archive type from magic bytes.
pub fn detect_type_from_content(data: &[u8]) -> String {
    if data.len() < 4 {
        return String::new();
    }
    // ZIP: PK\x03\x04 or PK\x05\x06
    if data[0] == 0x50 && data[1] == 0x4B && (data[2] == 0x03 || data[2] == 0x05) {
        return "zip".to_string();
    }
    // 7z: 7z\xBC\xAF\x27\x1C
    if data.len() >= 6
        && data[0] == 0x37
        && data[1] == 0x7A
        && data[2] == 0xBC
        && data[3] == 0xAF
        && data[4] == 0x27
        && data[5] == 0x1C
    {
        return "7z".to_string();
    }
    String::new()
}

/// How many 7z passes an archive needs.
pub fn extract_stages(archive_path: &Path, archive_type: &str) -> usize {
    if matches!(archive_type, "gz" | "bz2" | "xz") {
        return 2;
    }
    extract_stages_for_name(
        &archive_path
            .file_name()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default(),
    )
}

/// Stage count derived from the archive *name*.
pub fn extract_stages_for_name(archive_name: &str) -> usize {
    if is_compound_archive_name(archive_name) {
        2
    } else {
        1
    }
}

/// Whether the name denotes a two-layer archive.
fn is_compound_archive_name(name: &str) -> bool {
    let lower = name.to_ascii_lowercase();
    lower.ends_with(".tar.gz")
        || lower.ends_with(".tgz")
        || lower.ends_with(".tar.bz2")
        || lower.ends_with(".tar.xz")
        || lower.ends_with(".tbz2")
        || lower.ends_with(".txz")
}

/// The extension 7z should see for `name`.
/// Callback for extraction diagnostics.
///
/// A library has no logger of its own and a GUI build has no console, so the
/// frontends install one: the desktop forwards these lines into `desktop.log`.
/// With no sink installed extraction stays silent.
type TraceSink = Box<dyn Fn(&str) + Send + Sync>;

static EXTRACT_TRACE: std::sync::OnceLock<TraceSink> = std::sync::OnceLock::new();

/// Install the extraction trace sink. The first call wins; later ones are
/// ignored (the desktop installs it once, at startup).
pub fn set_extract_trace(sink: impl Fn(&str) + Send + Sync + 'static) {
    let _ = EXTRACT_TRACE.set(Box::new(sink));
}

fn trace(message: String) {
    if let Some(sink) = EXTRACT_TRACE.get() {
        sink(&message);
    }
}

/// Trace line for the rest of the engine's worker paths (the downloader reuses
/// the sink the extractor installs, so one hook covers both).
pub(crate) fn trace_line(message: &str) {
    trace(message.to_owned());
}

/// One line that explains a 7-Zip failure.
///
/// 7-Zip writes a banner, an archive echo and percentages before the line that
/// matters, and this text ends up in the task list; the untrimmed form made a
/// failed git install read as three lines of console noise. Everything dropped
/// here still reaches the trace log.
fn summarize_7z_output(code: Option<i32>, stdout: &[u8], stderr: &[u8]) -> String {
    const KEEP: usize = 4;
    let mut lines: Vec<String> = Vec::new();
    for stream in [stderr, stdout] {
        for line in String::from_utf8_lossy(stream).lines() {
            let line = line.trim();
            if line.is_empty() {
                continue;
            }
            // Banner, drive scan and archive echo: no diagnostic value.
            let lower = line.to_ascii_lowercase();
            if lower.starts_with("7-zip ")
                || lower.starts_with("scanning the drive")
                || lower.starts_with("extracting archive:")
                || lower.starts_with("listing archive:")
                || lower.starts_with("open archive:")
            {
                continue;
            }
            // "1 file, 57432384 bytes (55 MiB)" — the drive-scan summary.
            let scan_summary = lower.split_once(' ').is_some_and(|(count, rest)| {
                !count.is_empty()
                    && count.chars().all(|c| c.is_ascii_digit())
                    && (rest.starts_with("file") || rest.starts_with("files"))
            });
            if scan_summary {
                continue;
            }
            // "ERROR: <path>" marks the file; the following lines say why.
            let line = line.strip_prefix("ERROR: ").unwrap_or(line);
            if lines.iter().any(|seen| seen == line) {
                continue;
            }
            lines.push(line.to_owned());
            if lines.len() == KEEP {
                break;
            }
        }
    }
    let code = code
        .map(|code| code.to_string())
        .unwrap_or_else(|| "unknown".to_owned());
    if lines.is_empty() {
        format!("7z failed (exit code {code}) and printed nothing")
    } else {
        format!("7z failed (exit code {code}): {}", lines.join("; "))
    }
}

/// Extension the external 7z needs on its copy of a download.
///
/// A 7z self-extracting installer is a PE with the archive appended, and 7-Zip
/// only scans for that payload when the name says `.exe`. Bucket manifests name
/// such a download `…7z.exe#/dl.7z`, so the store hands over a PE called `.7z`;
/// staging that under its own name made 7-Zip open it as a PE, find no archive
/// and exit with code 2 ("Cannot open the file as [7z] archive").
fn stage_extension(name: &str, is_pe: bool) -> String {
    let ext = archive_extension(name);
    if is_pe && !ext.to_ascii_lowercase().ends_with(".exe") {
        ".exe".to_owned()
    } else {
        ext
    }
}

/// Whether a file starts with the DOS/PE magic (`MZ`).
fn has_pe_header(path: &Path) -> bool {
    let mut head = [0u8; 2];
    std::fs::File::open(path)
        .and_then(|mut file| std::io::Read::read_exact(&mut file, &mut head))
        .is_ok()
        && &head == b"MZ"
}

pub fn archive_extension(name: &str) -> String {
    let lower = name.to_ascii_lowercase();
    if lower.ends_with(".tar.gz") || lower.ends_with(".tgz") {
        ".tar.gz".to_string()
    } else if lower.ends_with(".tar.bz2") {
        ".tar.bz2".to_string()
    } else if lower.ends_with(".tar.xz") {
        ".tar.xz".to_string()
    } else if lower.ends_with(".7z.exe") {
        ".7z.exe".to_string()
    } else {
        Path::new(name)
            .extension()
            .map(|e| format!(".{}", e.to_string_lossy()))
            .unwrap_or_default()
    }
}

/// Whether a download filename is a ZIP the local reader can handle.
fn is_zip_name(name: &str) -> bool {
    let lower = name.to_ascii_lowercase();
    lower.ends_with(".zip") || lower.ends_with(".nupkg")
}

/// Whether a download filename is a tar the local reader can handle: plain
/// `.tar`, `.tar.gz` and `.tgz`. `.tar.bz2` / `.tar.xz` (and `.tbz2` /
/// `.txz`) are deliberately excluded — no in-process bzip2/LZMA decoders,
/// those keep routing through 7z.
fn is_tar_name(name: &str) -> bool {
    let lower = name.to_ascii_lowercase();
    lower.ends_with(".tar") || lower.ends_with(".tar.gz") || lower.ends_with(".tgz")
}

/// Normalize a tar link target for hardlink bookkeeping: backslashes to
/// forward slashes, no leading `./` segments.
fn strip_dot_slash(s: &str) -> String {
    let mut s = s.replace('\\', "/");
    while let Some(rest) = s.strip_prefix("./") {
        s = rest.to_string();
    }
    s
}

/// Find an inner `.tar` in `dir`.
fn find_nested_tar(dir: &Path) -> Option<PathBuf> {
    let entries = fs::read_dir(dir).ok()?;
    for entry in entries.flatten() {
        let path = entry.path();
        if path.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().to_ascii_lowercase();
        if name.ends_with(".tar") || is_tar_file(&path) {
            return Some(path);
        }
    }
    None
}

/// Sniff a tar header: octal checksum digits at 148..154 and a
/// valid type flag at 156.
fn is_tar_file(path: &Path) -> bool {
    let mut f = match fs::File::open(path) {
        Ok(f) => f,
        Err(_) => return false,
    };
    let mut header = [0u8; 512];
    if f.read_exact(&mut header).is_err() {
        return false;
    }
    for &b in &header[148..154] {
        if !(b'0'..=b'7').contains(&b) {
            return false;
        }
    }
    let typeflag = header[156];
    typeflag == 0
        || typeflag == b'0'
        || typeflag == 1
        || typeflag == b'2'
        || typeflag == 3
        || typeflag == 4
        || typeflag == 5
        || typeflag == 6
        || typeflag == b'x'
        || typeflag == b'g'
}

/// List every file under `root` as (absolute path, normalized rel path),
/// rejecting zip-slip entries.
fn list_extracted_files(root: &Path) -> Result<Vec<(PathBuf, String)>> {
    let mut out = Vec::new();
    walk_files(root, root, &mut out)?;
    Ok(out)
}

/// Recursive walk collecting files, validating each relative path.
fn walk_files(root: &Path, dir: &Path, out: &mut Vec<(PathBuf, String)>) -> Result<()> {
    let entries = match fs::read_dir(dir) {
        Ok(e) => e,
        Err(_) => return Ok(()),
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let meta = match entry.metadata() {
            Ok(m) => m,
            Err(_) => continue,
        };
        if meta.is_dir() {
            walk_files(root, &path, out)?;
            continue;
        }
        let slash = path.to_string_lossy().replace('\\', "/");
        // 7z writes already-split archive parts under these pseudo-dirs.
        if slash.contains("[DE]") || slash.contains("[FT]") {
            continue;
        }
        let rel = match path.strip_prefix(root) {
            Ok(r) => r.to_string_lossy().replace('\\', "/"),
            Err(_) => continue,
        };
        let safe = crate::safepath::validate_manifest_rel_path(&rel).map_err(|e| {
            ExtractError::UnsafeMember {
                path: rel.clone(),
                message: e.to_string(),
            }
        })?;
        out.push((path, safe));
    }
    Ok(())
}

/// A unique temp suffix.
fn unique_temp_name() -> String {
    let seq = TEMP_COUNTER.fetch_add(1, Ordering::Relaxed);
    format!("{:08x}{:04x}", std::process::id(), seq & 0xFFFF)
}

/// A `std::io::Write` sink that feeds bytes into the CAS store.
///
/// [`Store::write`] takes a reader, so streaming a decompressor's output into
/// the store needs an adapter that buffers the stream to a temp file and then
/// hands that file to the store. This avoids materializing a large member in
/// memory.
struct StoreSpool {
    store: crate::store::Store,
    tmp_path: PathBuf,
    file: Option<fs::File>,
}

impl StoreSpool {
    fn new(store: &crate::store::Store) -> Result<StoreSpool> {
        let tmp_path = store.path().join(format!(
            ".spool-{}-{}",
            std::process::id(),
            unique_temp_name()
        ));
        let file = fs::File::create(&tmp_path).map_err(ExtractError::WriteFile)?;
        Ok(StoreSpool {
            store: store.clone(),
            tmp_path,
            file: Some(file),
        })
    }

    /// Close the spooled file and hand it to the store for hashing + placement.
    fn finish(mut self) -> Result<String> {
        // Close before the store opens it (Windows has no shared-delete).
        drop(self.file.take());
        let file = fs::File::open(&self.tmp_path).map_err(ExtractError::ReadArchive)?;
        let hash = self.store.write(file);
        let _ = fs::remove_file(&self.tmp_path);
        hash.map_err(ExtractError::Store)
    }
}

impl std::io::Write for StoreSpool {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        match self.file.as_mut() {
            Some(f) => f.write(buf),
            None => Ok(0),
        }
    }

    fn flush(&mut self) -> std::io::Result<()> {
        match self.file.as_mut() {
            Some(f) => f.flush(),
            None => Ok(()),
        }
    }
}

impl Drop for StoreSpool {
    fn drop(&mut self) {
        // Remove the spool file if `finish` never ran (early return / error).
        self.file.take();
        let _ = fs::remove_file(&self.tmp_path);
    }
}

/// Map a stage-local 0–100 % into an overall 0–100 %
pub fn scale_extract_percent(percent: i32, stage: i32, total_stages: i32) -> i32 {
    if total_stages <= 1 {
        return percent;
    }
    let stage = stage.clamp(1, total_stages);
    let percent = percent.clamp(0, 100);
    let span = 100 / total_stages;
    let base = (stage - 1) * span;
    base + (percent * span / 100)
}

// ---------------------------------------------------------------------------
// 7z progress parsing (progress_parser.go)
// ---------------------------------------------------------------------------

/// Parses `-bsp1` percentage lines off 7z's stdout.
///
/// 7z emits lines like ` 45% 2 + archive.7z`; only the percentage matters, and
/// repeats of the same value are suppressed.
pub struct Progress7zParser<'a> {
    callback: Box<dyn Fn(i32) + 'a>,
    current: i32,
    partial: Vec<u8>,
}

impl<'a> Progress7zParser<'a> {
    /// Build a parser calling `callback` on each distinct percentage.
    pub fn new(callback: impl Fn(i32) + 'a) -> Self {
        Progress7zParser {
            callback: Box::new(callback),
            current: -1,
            partial: Vec::new(),
        }
    }

    fn parse_line(&mut self, line: &str) {
        let line = trim_progress_line(line);
        if line.is_empty() {
            return;
        }
        let Some(percent) = extract_percent(line) else {
            return;
        };
        if percent != self.current {
            self.current = percent;
            (self.callback)(percent);
        }
    }
}

impl std::io::Write for Progress7zParser<'_> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.partial.extend_from_slice(buf);
        while let Some(idx) = self.partial.iter().position(|&b| b == b'\n') {
            let line: Vec<u8> = self.partial.drain(..idx + 1).collect();
            if self.partial.first() == Some(&b'\r') {
                self.partial.remove(0);
            }
            let line = String::from_utf8_lossy(&line[..line.len().saturating_sub(1)]).into_owned();
            self.parse_line(&line);
        }
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Leading whitespace and stray CR/LF are stripped.
fn trim_progress_line(line: &str) -> &str {
    line.trim().trim_start_matches(['\r', '\n'])
}

/// First `NN%` percentage in `line`, validated to 0–100.
fn extract_percent(line: &str) -> Option<i32> {
    let bytes = line.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i].is_ascii_digit() {
            let start = i;
            while i < bytes.len() && bytes[i].is_ascii_digit() {
                i += 1;
            }
            if i < bytes.len() && bytes[i] == b'%' {
                let value: i32 = line[start..i].parse().ok()?;
                if !(0..=100).contains(&value) {
                    return None;
                }
                return Some(value);
            }
        } else {
            i += 1;
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;
    use std::sync::Mutex;

    #[test]
    fn detect_type_by_extension() {
        assert_eq!(detect_type(Path::new(r"C:\tmp\a.zip")).unwrap(), "zip");
        assert_eq!(
            detect_type(Path::new(r"C:\cache\App-1.2-full.nupkg")).unwrap(),
            "zip"
        );
        assert_eq!(detect_type(Path::new("a.7z")).unwrap(), "7z");
        assert_eq!(detect_type(Path::new("a.tar.gz")).unwrap(), "tar");
        assert_eq!(detect_type(Path::new("a.gz")).unwrap(), "gz");
        assert_eq!(detect_type(Path::new("a.bz2")).unwrap(), "bz2");
        assert_eq!(detect_type(Path::new("a.xz")).unwrap(), "xz");
        assert_eq!(detect_type(Path::new("a.tar")).unwrap(), "tar");
        assert_eq!(detect_type(Path::new("a.msi")).unwrap(), "msi");
        assert_eq!(detect_type(Path::new("a.msi_")).unwrap(), "msi");
        assert_eq!(detect_type(Path::new("a.exe")).unwrap(), "exe");
        assert!(matches!(
            detect_type(Path::new("a.rar")),
            Err(ExtractError::UnknownType(_))
        ));
    }

    #[test]
    fn detect_type_from_content_magic() {
        assert_eq!(detect_type_from_content(b"PK\x03\x04rest"), "zip");
        assert_eq!(detect_type_from_content(b"PK\x05\x06rest"), "zip");
        assert_eq!(
            detect_type_from_content(&[0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C]),
            "7z"
        );
        assert_eq!(detect_type_from_content(b"xxxx"), "");
        assert_eq!(detect_type_from_content(b"PK"), "");
    }

    #[test]
    fn extract_stages_matrix() {
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.zip"), "zip"), 1);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.7z"), "7z"), 1);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.tar"), "tar"), 1);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.tar.gz"), ""), 2);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.tgz"), ""), 2);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.tar.bz2"), ""), 2);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.gz"), "gz"), 2);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.bz2"), "bz2"), 2);
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.xz"), "xz"), 2);
        // A compound name wins over a plain type.
        assert_eq!(extract_stages(Path::new(r"C:\tmp\pkg.tar.xz"), "7z"), 2);
    }

    #[test]
    fn seven_zip_failure_summary_keeps_only_the_explanation() {
        // The git install that started this: a 7z SFX opened under a ".7z" name.
        let stderr = b"\n7-Zip 26.06 (x64) : Copyright (c) 1999-2026 Igor Pavlov : 2026-11-06\n\nScanning the drive for archives:\n1 file, 57432384 bytes (55 MiB)\n\nERROR: C:\\Users\\x\\AppData\\Local\\Temp\\stage-00004cfc00002.7z\nCannot open the file as [7z] archive\nThe file is open as [PE] archive\n";
        assert_eq!(
            summarize_7z_output(
                Some(2),
                b"",
                stderr
            ),
            "7z failed (exit code 2): C:\\Users\\x\\AppData\\Local\\Temp\\stage-00004cfc00002.7z; Cannot open the file as [7z] archive; The file is open as [PE] archive"
        );
        // Nothing but a banner: say so instead of returning an empty message.
        assert_eq!(
            summarize_7z_output(Some(1), b"7-Zip 26.06 (x64)\n", b""),
            "7z failed (exit code 1) and printed nothing"
        );
    }

    #[test]
    fn sfx_downloads_are_staged_as_exe() {
        // The git/Scoop `#/dl.7z` shape: a PortableGit SFX renamed to ".7z".
        assert_eq!(stage_extension("dl.7z", true), ".exe");
        // A real 7z keeps its name.
        assert_eq!(stage_extension("dl.7z", false), ".7z");
        // ".7z.exe" already carries the cue, and non-SFX exe installers keep it.
        assert_eq!(stage_extension("setup.7z.exe", true), ".7z.exe");
        assert_eq!(stage_extension("installer.exe", true), ".exe");
    }

    #[test]
    fn archive_extension_matrix() {
        assert_eq!(archive_extension("upm_1.0_windows_amd64.tar.gz"), ".tar.gz");
        assert_eq!(archive_extension("tool.tgz"), ".tar.gz");
        assert_eq!(archive_extension("archive.tar.bz2"), ".tar.bz2");
        assert_eq!(archive_extension("archive.tar.xz"), ".tar.xz");
        assert_eq!(archive_extension("setup.7z.exe"), ".7z.exe");
        assert_eq!(archive_extension("tool.zip"), ".zip");
    }

    #[test]
    fn resolve_full_local_7z_requires_codec_dll() {
        // `ResolveFullLocalSevenZip`: a glue-root 7z.exe counts only when 7z.dll
        // sits beside it (NSIS/MSI #/dl.7z extracts need the bundled codecs). The
        // glue-root candidate is checked before PATH, so once it is a full build it
        // wins deterministically — the assertions below hold regardless of whether
        // the host PATH happens to carry a 7z.
        let td = TempDir::new("glue-extractor-resolve7z");
        let cur = td.join("apps/7zip/current");
        std::fs::create_dir_all(&cur).unwrap();

        // exe without a codec DLL is not a full build, so it is skipped.
        std::fs::write(cur.join("7z.exe"), b"MZ").unwrap();
        assert_ne!(resolve_full_local_7z(&td), Some(cur.join("7z.exe")));

        // Adding 7z.dll beside it makes the glue-root candidate resolve.
        std::fs::write(cur.join("7z.dll"), b"DLL").unwrap();
        assert_eq!(resolve_full_local_7z(&td), Some(cur.join("7z.exe")));
    }

    #[test]
    fn seven_zip_signature_offset_plain_and_sfx() {
        use std::io::Write;
        let td = TempDir::new("glue-extractor-7z-sig");

        // A plain .7z: magic at offset 0.
        let plain = td.join("plain.7z");
        {
            let mut f = fs::File::create(&plain).unwrap();
            f.write_all(&SEVEN_ZIP_MAGIC).unwrap();
            f.write_all(&[0, 1, 2, 3]).unwrap();
        }
        assert_eq!(
            Extractor::seven_zip_signature_offset(&mut fs::File::open(&plain).unwrap()),
            Some(0)
        );

        // A self-extracting archive: an exe stub precedes the appended 7z data
        // (the `#/dl.7z` case, e.g. Zoom).
        let sfx = td.join("ZoomInstallerFull.exe");
        {
            let mut f = fs::File::create(&sfx).unwrap();
            f.write_all(&vec![0u8; 4096]).unwrap();
            f.write_all(&SEVEN_ZIP_MAGIC).unwrap();
            f.write_all(&[9, 9, 9]).unwrap();
        }
        assert_eq!(
            Extractor::seven_zip_signature_offset(&mut fs::File::open(&sfx).unwrap()),
            Some(4096)
        );

        // Not a 7z container at all.
        let notz = td.join("setup.exe");
        fs::write(&notz, b"MZ this is not a seven-zip archive").unwrap();
        assert_eq!(
            Extractor::seven_zip_signature_offset(&mut fs::File::open(&notz).unwrap()),
            None
        );
    }

    #[test]
    fn extract_7z_in_process_round_trip() {
        // Build a .7z with the crate's own writer (test-only `compress` feature),
        // then decode it in process through `extract_to_dir`. Fully offline.
        use sevenz_rust2::ArchiveWriter;

        let src = TempDir::new("glue-extractor-7z-src");
        std::fs::create_dir_all(src.join("bin")).unwrap();
        std::fs::write(src.join("bin/tool.exe"), b"MZ-payload").unwrap();
        std::fs::write(src.join("readme.txt"), b"hello 7z in process").unwrap();

        let td = TempDir::new("glue-extractor-7z-round");
        let archive = td.join("sample.7z");
        let mut w = ArchiveWriter::create(&archive).unwrap();
        w.push_source_path(src.join("bin/tool.exe"), |_| true)
            .unwrap();
        w.push_source_path(src.join("readme.txt"), |_| true)
            .unwrap();
        w.finish().unwrap();

        let out = td.join("out");
        // No external 7z: this must be decoded in process (see the SFX test).
        let ext = Extractor::new(None);
        let _ = ext.use7z.set(false);
        ext.extract_to_dir(&archive, &out, "sample.7z").unwrap();

        // Walk the output; 7z writers vary on whether the top dir is a member,
        // so match by file basename rather than an exact layout.
        fn walk(dir: &std::path::Path, found: &mut std::collections::HashMap<String, PathBuf>) {
            if let Ok(rd) = std::fs::read_dir(dir) {
                for e in rd.flatten() {
                    let p = e.path();
                    if p.is_dir() {
                        walk(&p, found);
                    } else if let Some(name) = p.file_name() {
                        found.insert(name.to_string_lossy().into_owned(), p);
                    }
                }
            }
        }
        let mut found = std::collections::HashMap::new();
        walk(&out, &mut found);
        let tool = found.get("tool.exe").expect("tool.exe extracted");
        assert_eq!(std::fs::read(tool).unwrap(), b"MZ-payload");
        let readme = found.get("readme.txt").expect("readme.txt extracted");
        assert_eq!(std::fs::read(readme).unwrap(), b"hello 7z in process");
    }

    #[test]
    fn extract_7z_sfx_self_extracting_in_process() {
        // A 7z self-extracting archive: an EXE stub before the appended 7z data,
        // exactly Zoom's `<file>.exe#/dl.7z` shape. The download name ends in
        // `.exe`, so routing must find the signature past the stub and extract.
        use sevenz_rust2::ArchiveWriter;

        let src = TempDir::new("glue-extractor-7zsfx-src");
        std::fs::write(src.join("payload.txt"), b"sfx payload").unwrap();

        let td = TempDir::new("glue-extractor-7zsfx");
        let inner = td.join("inner.7z");
        let mut w = ArchiveWriter::create(&inner).unwrap();
        w.push_source_path(src.join("payload.txt"), |_| true)
            .unwrap();
        w.finish().unwrap();

        let sfx = td.join("ZoomInstallerFull.exe");
        {
            use std::io::Write;
            let mut f = fs::File::create(&sfx).unwrap();
            f.write_all(b"This program cannot be run in DOS mode.\r\n")
                .unwrap();
            f.write_all(&[0u8; 512]).unwrap(); // stub padding — no 7z magic here
            f.write_all(&fs::read(&inner).unwrap()).unwrap();
        }

        let out = td.join("out");
        // Guard the in-process path: with an external 7z on PATH the fallback
        // would mask a broken signature scan or a mis-based reader (it did —
        // the SFX seek bug only surfaced once this test ran without 7z).
        let ext = Extractor::new(None);
        let _ = ext.use7z.set(false);
        ext.extract_to_dir(&sfx, &out, "ZoomInstallerFull.exe")
            .unwrap();

        fn find(dir: &std::path::Path, name: &str) -> Option<PathBuf> {
            let rd = std::fs::read_dir(dir).ok()?;
            for e in rd.flatten() {
                let p = e.path();
                if p.is_dir() {
                    if let Some(hit) = find(&p, name) {
                        return Some(hit);
                    }
                } else if p.file_name().map(|n| n == name).unwrap_or(false) {
                    return Some(p);
                }
            }
            None
        }
        let payload = find(&out, "payload.txt").expect("payload extracted from SFX");
        assert_eq!(std::fs::read(payload).unwrap(), b"sfx payload");
    }

    #[test]
    fn seven_zip_binary_defaults_then_override() {
        let ext = Extractor::new(None);
        assert_eq!(ext.seven_zip_binary(), "7z.exe");
        ext.set_7z_path(r"C:\tools\7z.exe");
        assert_eq!(ext.seven_zip_binary(), r"C:\tools\7z.exe");
        assert!(ext.uses_7z());
    }

    #[test]
    fn build_7z_extract_args_includes_mmt_and_progress() {
        let mut ext = Extractor::new(None);
        ext.set_workers(8);
        let args = ext.build_7z_extract_args(Path::new(r"C:\out"), Path::new(r"C:\a.7z"), false);
        assert_eq!(args[0], "-mmt=8");

        let args = ext.build_7z_extract_args(Path::new(r"C:\out"), Path::new(r"C:\a.7z"), true);
        assert_eq!(args[0], "-mmt=8");
        assert_eq!(args[1], "-bso0");
        assert_eq!(args[2], "-bsp1");
        assert_eq!(args[3], "x");
        assert_eq!(args[4], "-y");
        assert_eq!(args[5], r"-oC:\out");
        assert_eq!(args[6], r"C:\a.7z");
    }

    #[test]
    fn workers_below_two_omits_mmt() {
        let mut ext = Extractor::new(None);
        ext.set_workers(1);
        let args = ext.build_7z_extract_args(Path::new("out"), Path::new("a.7z"), false);
        assert_eq!(args[0], "x");
    }

    #[test]
    fn extract_7z_in_process_large_member_chunk_boundary() {
        // The in-process member write loop is a hand-rolled 128 KiB read loop
        // (no `io::copy`, cf. `Store::hash_reader`): its failure mode is a
        // dropped or duplicated byte at a chunk boundary, which a small member
        // can never trigger. This member spans 4+ chunks with incompressible,
        // position-dependent content so an off-by-one anywhere fails.
        use sevenz_rust2::ArchiveWriter;

        // 400 KiB > 3 x 128 KiB, deterministic and position-dependent.
        let mut payload = vec![0u8; 400 * 1024];
        for (i, b) in payload.iter_mut().enumerate() {
            *b = ((i as u32).wrapping_mul(2654435761) >> 24) as u8;
        }

        let src = TempDir::new("glue-extractor-7zbig-src");
        std::fs::write(src.join("big.bin"), &payload).unwrap();

        let td = TempDir::new("glue-extractor-7zbig");
        let archive = td.join("big.7z");
        let mut w = ArchiveWriter::create(&archive).unwrap();
        w.push_source_path(src.join("big.bin"), |_| true).unwrap();
        w.finish().unwrap();

        let out = td.join("out");
        let ext = Extractor::new(None);
        let _ = ext.use7z.set(false);
        ext.extract_to_dir(&archive, &out, "big.7z").unwrap();

        let mut written: Option<Vec<u8>> = None;
        fn walk(dir: &std::path::Path, found: &mut Option<Vec<u8>>) {
            if let Ok(rd) = std::fs::read_dir(dir) {
                for e in rd.flatten() {
                    let p = e.path();
                    if p.is_dir() {
                        walk(&p, found);
                    } else {
                        *found = Some(std::fs::read(&p).unwrap());
                    }
                }
            }
        }
        walk(&out, &mut written);
        assert_eq!(
            written.as_deref(),
            Some(payload.as_slice()),
            "large member bytes must survive the chunk-boundary loop exactly"
        );
    }

    #[test]
    fn progress_parser_parses_7z_plus_format() {
        let got = std::sync::Arc::new(Mutex::new(Vec::new()));
        let sink = got.clone();
        let mut p = Progress7zParser::new(move |percent| sink.lock().unwrap().push(percent));
        use std::io::Write;
        p.write_all(b"  45% 2 + inkscape.7z\n100% 2 + inkscape.7z\n")
            .unwrap();
        assert_eq!(*got.lock().unwrap(), vec![45, 100]);
    }

    #[test]
    fn progress_parser_handles_chunks_and_leading_space() {
        let got = std::sync::Arc::new(Mutex::new(Vec::new()));
        let sink = got.clone();
        let mut p = Progress7zParser::new(move |percent| sink.lock().unwrap().push(percent));
        use std::io::Write;
        p.write_all(b"  12% 1/2 - Extract").unwrap();
        p.write_all(b"ing\n100% 2/2 - Everything is Ok\n").unwrap();
        assert_eq!(*got.lock().unwrap(), vec![12, 100]);
    }

    #[test]
    fn progress_parser_dedupes_repeated_percent() {
        let got = std::sync::Arc::new(Mutex::new(Vec::new()));
        let sink = got.clone();
        let mut p = Progress7zParser::new(move |percent| sink.lock().unwrap().push(percent));
        use std::io::Write;
        p.write_all(b"45% 1/2 - a\n45% 1/2 - b\n").unwrap();
        assert_eq!(*got.lock().unwrap(), vec![45]);
    }

    #[test]
    fn progress_percent_ignores_out_of_range() {
        let got = std::sync::Arc::new(Mutex::new(Vec::new()));
        let sink = got.clone();
        let mut p = Progress7zParser::new(move |percent| sink.lock().unwrap().push(percent));
        use std::io::Write;
        // 250% is out of range and must be dropped (returns on parse error).
        p.write_all(b"250% 1/2 - a\n50% 1/2 - b\n").unwrap();
        assert_eq!(*got.lock().unwrap(), vec![50]);
    }

    #[test]
    fn scale_extract_percent_two_stages() {
        assert_eq!(scale_extract_percent(100, 1, 2), 50);
        assert_eq!(scale_extract_percent(100, 2, 2), 100);
        // Single stage passes through.
        assert_eq!(scale_extract_percent(37, 1, 1), 37);
    }

    #[test]
    fn trim_progress_line_strips_cr_and_space() {
        let line = trim_progress_line("\r  12% 1/2 - file\r");
        assert!(line.starts_with("12%"), "got {line:?}");
    }

    #[test]
    fn unsupported_format_message_shape() {
        let e = UnsupportedFormatError::new(
            "MSI",
            "MSI extraction requires 7z.exe in PATH.\nInstall 7z with: glue install 7zip",
        );
        assert_eq!(
            e.to_string(),
            "unsupported format: MSI\nMSI extraction requires 7z.exe in PATH.\nInstall 7z with: glue install 7zip"
        );
        let e = UnsupportedFormatError::new("RAR", "");
        assert_eq!(e.to_string(), "unsupported format: RAR");
    }

    // ------------------------------------------------------------------
    // ZIP extraction (in-process path; no external tool needed)
    // ------------------------------------------------------------------

    /// Minimal ZIP writer for fixtures — the implementation only reads archives, so the
    /// test builds one by hand.
    fn build_zip(members: &[(&str, &[u8], bool)]) -> Vec<u8> {
        const LOCAL: u32 = 0x0403_4b50;
        const CENTRAL: u32 = 0x0201_4b50;
        const EOCD: u32 = 0x0605_4b50;
        let mut out = Vec::new();
        let mut central = Vec::new();

        for (name, contents, deflate) in members {
            let crc = crate::ziparchive::Crc32::compute(contents);
            let use_deflate = *deflate && !contents.is_empty();
            let payload = if use_deflate {
                crate::testutil::deflate_raw(contents)
            } else {
                contents.to_vec()
            };
            let method: u16 = if use_deflate { 8 } else { 0 };
            let offset = out.len() as u64;
            let name_bytes = name.as_bytes();

            out.extend_from_slice(&LOCAL.to_le_bytes());
            out.extend_from_slice(&20u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&method.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&crc.to_le_bytes());
            out.extend_from_slice(&(payload.len() as u32).to_le_bytes());
            out.extend_from_slice(&(contents.len() as u32).to_le_bytes());
            out.extend_from_slice(&(name_bytes.len() as u16).to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(name_bytes);
            out.extend_from_slice(&payload);

            central.push((
                name.to_string(),
                crc,
                method,
                payload.len() as u64,
                contents.len() as u64,
                offset,
            ));
        }

        let cd_offset = out.len() as u64;
        for (name, crc, method, comp, uncomp, offset) in &central {
            let name_bytes = name.as_bytes();
            out.extend_from_slice(&CENTRAL.to_le_bytes());
            out.extend_from_slice(&20u16.to_le_bytes());
            out.extend_from_slice(&20u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&method.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&crc.to_le_bytes());
            out.extend_from_slice(&(*comp as u32).to_le_bytes());
            out.extend_from_slice(&(*uncomp as u32).to_le_bytes());
            out.extend_from_slice(&(name_bytes.len() as u16).to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&0u16.to_le_bytes());
            out.extend_from_slice(&0u32.to_le_bytes());
            out.extend_from_slice(&(*offset as u32).to_le_bytes());
            out.extend_from_slice(name_bytes);
        }
        let cd_size = out.len() as u64 - cd_offset;

        out.extend_from_slice(&EOCD.to_le_bytes());
        out.extend_from_slice(&0u16.to_le_bytes());
        out.extend_from_slice(&0u16.to_le_bytes());
        out.extend_from_slice(&(central.len() as u16).to_le_bytes());
        out.extend_from_slice(&(central.len() as u16).to_le_bytes());
        out.extend_from_slice(&(cd_size as u32).to_le_bytes());
        out.extend_from_slice(&(cd_offset as u32).to_le_bytes());
        out.extend_from_slice(&0u16.to_le_bytes());
        out
    }

    #[test]
    fn extract_to_dir_unpacks_zip_locally() {
        let dir = TempDir::new("glue-extractor-todir");
        let zip_path = dir.join("pkg.zip");
        std::fs::write(
            &zip_path,
            build_zip(&[
                ("bin/tool.exe", b"MZ-stub", false),
                (
                    "share/docs/readme.txt",
                    b"hello deflated hello deflated",
                    true,
                ),
            ]),
        )
        .unwrap();

        let dest = dir.join("out");
        let ext = Extractor::new(None);
        ext.extract_to_dir(&zip_path, &dest, "pkg.zip").unwrap();

        assert_eq!(
            std::fs::read(dest.join("bin/tool.exe")).unwrap(),
            b"MZ-stub"
        );
        assert_eq!(
            std::fs::read(dest.join("share/docs/readme.txt")).unwrap(),
            b"hello deflated hello deflated"
        );
    }

    #[test]
    fn estimate_extract_bytes_sums_zip_members() {
        let dir = TempDir::new("glue-extractor-estimate");
        let payload_a = b"MZ-stub";
        let payload_b = b"hello deflated hello deflated";
        let zip_path = dir.join("pkg.zip");
        std::fs::write(
            &zip_path,
            build_zip(&[("a.exe", payload_a, false), ("b.txt", payload_b, true)]),
        )
        .unwrap();

        let expected = (payload_a.len() + payload_b.len()) as u64;
        assert_eq!(estimate_extract_bytes(&zip_path, "pkg.zip"), Some(expected));
        // A format without an accessible size yields no preflight at all.
        assert_eq!(estimate_extract_bytes(&zip_path, "pkg.exe"), None);
    }

    #[test]
    fn extract_to_dir_rejects_zip_slip() {
        let dir = TempDir::new("glue-extractor-slip");
        let zip_path = dir.join("evil.zip");
        std::fs::write(&zip_path, build_zip(&[("../outside.exe", b"pwn", false)])).unwrap();
        let dest = dir.join("out");
        let ext = Extractor::new(None);
        let err = ext
            .extract_to_dir(&zip_path, &dest, "evil.zip")
            .unwrap_err();
        assert!(
            matches!(err, ExtractError::UnsafeMember { .. }),
            "unexpected error: {err}"
        );
    }

    #[test]
    fn extract_file_ingests_zip_members_into_store() {
        let dir = TempDir::new("glue-extractor-store");
        let zip_path = dir.join("pkg.zip");
        std::fs::write(
            &zip_path,
            build_zip(&[
                ("a.txt", b"alpha", false),
                ("b.txt", b"bravo bravo bravo bravo", true),
            ]),
        )
        .unwrap();

        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let ext = Extractor::new(Some(store.clone()));
        let (_archive_hash, files) = ext.extract_file(&zip_path, Some("zip")).unwrap();

        assert_eq!(files.len(), 2);
        let a = files.get("a.txt").unwrap();
        assert!(store.has(a));
        let contents = std::fs::read(store.object_path(a)).unwrap();
        assert_eq!(contents, b"alpha");
    }

    #[test]
    fn extract_file_without_store_is_an_error() {
        let dir = TempDir::new("glue-extractor-nostore");
        let zip_path = dir.join("pkg.zip");
        std::fs::write(&zip_path, build_zip(&[("a.txt", b"x", false)])).unwrap();
        let ext = Extractor::new(None);
        assert!(ext.extract_file(&zip_path, Some("zip")).is_err());
    }

    #[test]
    fn extract_file_streams_large_member_through_spool() {
        // A member above the 2 MiB in-memory ceiling takes the spool path;
        // verify the bytes round-trip intact and the spool file is cleaned up.
        let dir = TempDir::new("glue-extractor-large");
        let payload = vec![0xABu8; (2 << 20) + 4096];
        let zip_path = dir.join("big.zip");
        std::fs::write(&zip_path, build_zip(&[("big.bin", &payload, true)])).unwrap();

        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let ext = Extractor::new(Some(store.clone()));
        let (_h, files) = ext.extract_file(&zip_path, Some("zip")).unwrap();

        let hash = files.get("big.bin").expect("member recorded");
        assert!(store.has(hash));
        assert_eq!(std::fs::read(store.object_path(hash)).unwrap(), payload);
        // No spool leftovers.
        let leftovers: Vec<_> = std::fs::read_dir(store.path())
            .unwrap()
            .flatten()
            .filter(|e| e.file_name().to_string_lossy().starts_with(".spool-"))
            .collect();
        assert!(
            leftovers.is_empty(),
            "spool files not cleaned: {leftovers:?}"
        );
    }

    #[test]
    fn extract_file_rejects_zip_slip_member() {
        // The zip-slip guard applies to the store path too, not only to
        // extract_to_dir.
        let dir = TempDir::new("glue-extractor-store-slip");
        let zip_path = dir.join("evil.zip");
        std::fs::write(&zip_path, build_zip(&[("../escape.exe", b"pwn", false)])).unwrap();
        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let ext = Extractor::new(Some(store));
        let err = ext.extract_file(&zip_path, Some("zip")).unwrap_err();
        assert!(
            matches!(err, ExtractError::UnsafeMember { .. }),
            "unexpected: {err}"
        );
    }

    #[test]
    fn ingest_extracted_dir_hashes_every_file() {
        let dir = TempDir::new("glue-extractor-ingest");
        let extract_dir = dir.join("out");
        std::fs::create_dir_all(extract_dir.join("sub")).unwrap();
        for i in 0..8 {
            let name = format!("sub/{}.txt", (b'a' + i) as char);
            std::fs::write(extract_dir.join(&name), b"data").unwrap();
        }

        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let ext = Extractor::new(Some(store));
        let files = ext.ingest_extracted_dir(&extract_dir).unwrap();
        assert_eq!(files.len(), 8);
    }

    #[test]
    fn ingest_extracted_dir_parallel_pool_loses_nothing() {
        // Fans ingest out over a worker pool. The failure modes that a
        // 1–8 file test cannot catch are: a file skipped when workers race on
        // the cursor, a wrong hash written for a file another worker took, or
        // a completion-order-dependent map. Use enough files (and per-file
        // content) that all workers stay busy.
        let dir = TempDir::new("glue-extractor-ingest-par");
        let extract_dir = dir.join("out");
        std::fs::create_dir_all(extract_dir.join("bin")).unwrap();
        std::fs::create_dir_all(extract_dir.join("share/docs")).unwrap();
        let n = 200usize;
        let mut expected = BTreeMap::new();
        for i in 0..n {
            // Distinct, position-dependent payloads so a swapped hash shows up.
            let payload: Vec<u8> = (0..(1024 + i))
                .map(|j| ((i * 31 + j) % 251) as u8)
                .collect();
            let rel = if i.is_multiple_of(2) {
                format!("bin/tool{i:03}.exe")
            } else {
                format!("share/docs/doc{i:03}.txt")
            };
            std::fs::write(extract_dir.join(&rel), &payload).unwrap();
            expected.insert(rel, crate::hash::hash_bytes(&payload));
        }

        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let mut ext = Extractor::new(Some(store.clone()));
        ext.set_workers(4);
        let files = ext.ingest_extracted_dir(&extract_dir).unwrap();

        assert_eq!(files.len(), n, "every extracted file must be ingested");
        assert_eq!(files, expected, "hashes must map to their own file");
        // Deterministic order regardless of which worker finished first.
        let keys: Vec<&String> = files.keys().collect();
        let mut sorted = keys.clone();
        sorted.sort();
        assert_eq!(keys, sorted, "map must be path-sorted");
        // Every blob is really in the CAS, not just a returned hash.
        for hash in files.values() {
            assert!(store.object_path(hash).exists(), "missing blob {hash}");
        }
    }

    #[test]
    fn ingest_rejects_zip_slip_path() {
        // The sanitizer itself is the guard.
        assert!(crate::safepath::validate_manifest_rel_path("../outside.exe").is_err());
    }

    #[test]
    fn extract_to_dir_without_7z_reports_missing_tool() {
        let dir = TempDir::new("glue-extractor-no7z");
        let archive = dir.join("pkg.7z");
        std::fs::write(&archive, b"7z\xBC\xAF'\x1Cnot-really").unwrap();
        let ext = Extractor::new(None);
        // Force the "no external tool" state regardless of the host.
        let _ = ext.use7z.set(false);
        let err = ext
            .extract_to_dir(&archive, &dir.join("out"), "pkg.7z")
            .unwrap_err();
        assert!(matches!(err, ExtractError::SevenZipMissing), "got {err}");
    }

    // ---- tar extraction tests ------------------------------------------

    /// One 512-byte ustar header (name, typeflag, size, linkname) with a
    /// correct checksum.
    fn tar_header(name: &str, typeflag: u8, size: u64, linkname: &str) -> [u8; 512] {
        let mut h = [0u8; 512];
        let nb = name.as_bytes();
        h[..nb.len().min(100)].copy_from_slice(&nb[..nb.len().min(100)]);
        // size: 11 octal digits + NUL
        let s = format!("{:011o}\0", size);
        h[124..136].copy_from_slice(s.as_bytes());
        h[156] = typeflag;
        let lb = linkname.as_bytes();
        h[157..157 + lb.len().min(100)].copy_from_slice(&lb[..lb.len().min(100)]);
        h[257..262].copy_from_slice(b"ustar");
        h[264..266].copy_from_slice(b"00");
        for b in &mut h[148..156] {
            *b = b' ';
        }
        let sum: u32 = h.iter().map(|&b| b as u32).sum();
        let cs = format!("{:06o}\0", sum);
        h[148..155].copy_from_slice(cs.as_bytes());
        h
    }

    /// Minimal tar builder for extractor-level tests (512-byte headers,
    /// ustar magic, correct checksums).
    fn build_tar(entries: &[(&str, u8, &[u8])]) -> Vec<u8> {
        const BLOCK: usize = 512;
        let mut out = Vec::new();
        for (name, typeflag, content) in entries {
            let h = tar_header(name, *typeflag, content.len() as u64, "");
            out.extend_from_slice(&h);
            out.extend_from_slice(content);
            let pad = (BLOCK - (content.len() % BLOCK)) % BLOCK;
            out.extend(std::iter::repeat_n(0u8, pad));
        }
        out.extend_from_slice(&[0u8; BLOCK * 2]);
        out
    }

    /// gzip-wrap raw bytes for the tar.gz tests.
    fn gzip_bytes(raw: &[u8]) -> Vec<u8> {
        let mut out = vec![0x1f, 0x8b, 0x08, 0x00, 0, 0, 0, 0, 0x00, 0xff];
        out.extend_from_slice(&crate::testutil::deflate_raw(raw));
        out.extend_from_slice(&crate::ziparchive::Crc32::compute(raw).to_le_bytes());
        out.extend_from_slice(&(raw.len() as u32).to_le_bytes());
        out
    }

    #[test]
    fn extract_tar_to_dir_writes_members() {
        let dir = TempDir::new("glue-extractor-tar-dir");
        let tar = build_tar(&[
            // BSD-style root entry: skipped, not an error (matches the
            // ZIP path, where `validate_manifest_rel_path` returns "").
            (".", b'5', b""),
            ("bin/", b'5', b""),
            ("bin/tool.exe", b'0', b"tool payload"),
            ("docs/readme.txt", b'0', &[7u8; 1000]),
        ]);
        let path = dir.join("pkg.tar");
        std::fs::write(&path, &tar).unwrap();
        let out = dir.join("out");

        let ext = Extractor::new(None);
        ext.extract_to_dir(&path, &out, "pkg.tar").unwrap();

        assert_eq!(
            std::fs::read(out.join("bin/tool.exe")).unwrap(),
            b"tool payload"
        );
        assert_eq!(
            std::fs::read(out.join("docs/readme.txt")).unwrap().len(),
            1000
        );
    }

    #[test]
    fn extract_tar_gz_to_dir_without_7z() {
        // The divergence guard: tar.gz must extract even when no external
        // 7z is configured — this is the whole point of the in-process
        // tar. Both the .tar.gz and .tgz suffixes dispatch to the tar
        // reader (bz2/xz variants are excluded and still need 7z).
        let dir = TempDir::new("glue-extractor-targz-no7z");
        let tar = build_tar(&[("tool.exe", b'0', b"gzipped tool")]);
        let ext = Extractor::new(None);
        let _ = ext.use7z.set(false);
        for name in ["pkg.tar.gz", "pkg.tgz"] {
            let path = dir.join(name);
            let out = dir.join(format!("out-{name}"));
            std::fs::write(&path, gzip_bytes(&tar)).unwrap();
            ext.extract_to_dir(&path, &out, name).unwrap();
            assert_eq!(
                std::fs::read(out.join("tool.exe")).unwrap(),
                b"gzipped tool"
            );
        }
    }

    #[test]
    fn extract_tar_to_store_ingests_members() {
        let dir = TempDir::new("glue-extractor-tar-store");
        let tar = build_tar(&[("tool.exe", b'0', b"store payload")]);
        let path = dir.join("pkg.tar");
        std::fs::write(&path, &tar).unwrap();
        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let ext = Extractor::new(Some(store));

        let (archive_hash, files) = ext.extract_file(&path, Some("tar")).unwrap();
        assert!(!archive_hash.is_empty());
        assert_eq!(files.len(), 1);
        let (rel, hash) = files.iter().next().unwrap();
        assert_eq!(rel, "tool.exe");
        // The blob landed in the CAS store.
        let obj = ext.store().unwrap().object_path(hash);
        assert!(obj.exists());
        assert_eq!(std::fs::read(obj).unwrap(), b"store payload");
    }

    #[test]
    fn extract_tar_rejects_zip_slip_member() {
        let dir = TempDir::new("glue-extractor-tar-slip");
        let tar = build_tar(&[("../escape.exe", b'0', b"pwn")]);
        let path = dir.join("evil.tar");
        std::fs::write(&path, &tar).unwrap();
        let ext = Extractor::new(None);
        let err = ext
            .extract_to_dir(&path, &dir.join("out"), "evil.tar")
            .unwrap_err();
        assert!(
            matches!(err, ExtractError::UnsafeMember { .. }),
            "got {err}"
        );
    }

    #[test]
    fn extract_tar_rejects_unsupported_member_type() {
        let dir = TempDir::new("glue-extractor-tar-devicetype");
        // Type '3' (character device) has no filesystem equivalent to write.
        let tar = build_tar(&[("dev/null", 3, b"")]);
        let path = dir.join("dev.tar");
        std::fs::write(&path, &tar).unwrap();
        let ext = Extractor::new(None);
        let err = ext
            .extract_to_dir(&path, &dir.join("out"), "dev.tar")
            .unwrap_err();
        assert!(
            matches!(
                err,
                ExtractError::Tar(crate::tararchive::TarError::UnsupportedMemberType { .. })
            ),
            "got {err}"
        );
    }

    #[test]
    fn extract_tar_creates_hardlink_to_prior_member() {
        let dir = TempDir::new("glue-extractor-tar-hardlink");
        let payload = b"shared payload";
        let mut tar = Vec::new();
        tar.extend_from_slice(&tar_header("real.txt", b'0', payload.len() as u64, ""));
        tar.extend_from_slice(payload);
        let pad = (512 - (payload.len() % 512)) % 512;
        tar.extend(std::iter::repeat_n(0u8, pad));
        // A '1' entry carries its target in the header linkname field.
        tar.extend_from_slice(&tar_header("alias.txt", b'1', 0, "real.txt"));
        tar.extend_from_slice(&[0u8; 512 * 2]);
        let path = dir.join("hard.tar");
        std::fs::write(&path, &tar).unwrap();
        let out = dir.join("out");

        let ext = Extractor::new(None);
        ext.extract_to_dir(&path, &out, "hard.tar").unwrap();

        assert_eq!(
            std::fs::read(out.join("alias.txt")).unwrap(),
            b"shared payload"
        );
    }

    #[test]
    fn extract_tar_rejects_dangling_hardlink() {
        let dir = TempDir::new("glue-extractor-tar-dangling");
        let mut tar = Vec::new();
        // A '1' entry whose target was never extracted: resolution must
        // fail loudly, never silently skip.
        tar.extend_from_slice(&tar_header("orphan.txt", b'1', 0, "never/extracted.txt"));
        tar.extend_from_slice(&[0u8; 512 * 2]);
        let path = dir.join("dangling.tar");
        std::fs::write(&path, &tar).unwrap();
        let ext = Extractor::new(None);
        let err = ext
            .extract_to_dir(&path, &dir.join("out"), "dangling.tar")
            .unwrap_err();
        assert!(
            matches!(
                err,
                ExtractError::Tar(crate::tararchive::TarError::HardlinkTargetMissing { .. })
            ),
            "got {err}"
        );
    }

    #[test]
    fn extract_tar_symlink_is_materialised_as_target_text() {
        let dir = TempDir::new("glue-extractor-tar-symlink-text");
        // Symlinks must never be created: 7-Zip's default (no `-snl`) writes
        // a small file holding the link target, which is also the only safe
        // option — a real symlink is a write-through primitive.
        let mut tar = Vec::new();
        tar.extend_from_slice(&tar_header("link.txt", b'2', 0, "../outside.txt"));
        tar.extend_from_slice(&[0u8; 512 * 2]);
        let path = dir.join("sym.tar");
        std::fs::write(&path, &tar).unwrap();
        let ext = Extractor::new(None);
        ext.extract_to_dir(&path, &dir.join("out"), "sym.tar")
            .unwrap();
        let materialised = dir.join("out/link.txt");
        assert!(materialised.is_file());
        assert!(
            !materialised
                .symlink_metadata()
                .unwrap()
                .file_type()
                .is_symlink(),
            "the symlink entry must not become a real symlink"
        );
        assert_eq!(
            std::fs::read_to_string(&materialised).unwrap(),
            "../outside.txt"
        );
    }

    #[test]
    fn extract_tar_symlink_write_through_cannot_escape() {
        let dir = TempDir::new("glue-extractor-tar-writethrough");
        // Classic tar write-through: plant a symlinked directory, then write
        // a regular file beneath it. The materialised link is a plain file,
        // so the follow-up member fails instead of escaping the destination.
        let mut tar = Vec::new();
        tar.extend_from_slice(&tar_header("sub", b'2', 0, ".."));
        tar.extend_from_slice(&tar_header("sub/evil.txt", b'0', 4, ""));
        tar.extend_from_slice(b"evil");
        let pad = 512 - b"evil".len();
        tar.extend(std::iter::repeat_n(0u8, pad));
        tar.extend_from_slice(&[0u8; 512 * 2]);
        let path = dir.join("wt.tar");
        std::fs::write(&path, &tar).unwrap();
        let ext = Extractor::new(None);
        let _ = ext.extract_to_dir(&path, &dir.join("out"), "wt.tar");
        assert!(
            !dir.join("evil.txt").exists(),
            "no member may be written outside the destination"
        );
    }

    #[test]
    fn extract_tar_to_store_spools_large_member() {
        // Members above the in-memory `write_member` ceiling take the
        // StoreSpool path — the one real large packages actually traverse.
        // Unlike the ZIP path there is no re-read retry: a tar payload
        // cannot be decompressed twice.
        let dir = TempDir::new("glue-extractor-tar-spool");
        const BIG: usize = 3 << 20; // 3 MiB > the 2 MiB in-memory ceiling
        let payload: Vec<u8> = (0..BIG).map(|i| (i % 251) as u8).collect();
        let tar = build_tar(&[("big.bin", b'0', &payload)]);
        let path = dir.join("big.tar");
        std::fs::write(&path, &tar).unwrap();
        let store = crate::store::Store::new(dir.join("store")).unwrap();
        let ext = Extractor::new(Some(store));

        let (archive_hash, files) = ext.extract_file(&path, Some("tar")).unwrap();
        assert!(!archive_hash.is_empty());
        assert_eq!(files.len(), 1);
        let (rel, hash) = files.iter().next().unwrap();
        assert_eq!(rel, "big.bin");
        // The blob landed in the CAS store via the spool path.
        let obj = ext.store().unwrap().object_path(hash);
        assert!(obj.exists());
        let stored = std::fs::read(&obj).unwrap();
        assert_eq!(stored.len(), BIG);
        assert_eq!(stored, payload);
    }
}
