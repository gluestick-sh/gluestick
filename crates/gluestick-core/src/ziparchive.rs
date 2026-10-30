//! ZIP archive reader: central-directory parsing plus per-member deflate
//! decompression.
//!
//! The engine shells out to `7z.exe` for every format, including ZIP
//! (`Extractor.extractZIP` is an unimplemented stub). Shelling out to an
//! external binary is a hard runtime dependency the user must satisfy, so this
//! port parses ZIP in-process instead: the pure-Rust `zlib-rs` inflater
//! replaces an entire external tool for the format that covers the
//! overwhelming majority of Scoop packages. Non-ZIP formats still fall back to
//! `7z.exe` (see [`crate::extractor`]).
//!
//! Scope, matching what Scoop manifests actually ship:
//!
//! - local file headers + central directory + (optional) ZIP64 EOCD,
//! - methods 0 (store) and 8 (deflate); anything else is reported as
//!   unsupported rather than silently corrupting output,
//! - CRC-32 verification per member (the decompressor cannot be trusted to
//!   notice a truncated stream on its own).
//!
//! Reading is lazy: the archive file is kept open and only the central
//! directory is parsed up front. Member bytes are decompressed on demand so a
//! 500 MB installer never lands in memory in full.

use std::collections::BTreeMap;
use std::fs::File;
use std::io::{Read, Seek, SeekFrom};
use std::path::{Path, PathBuf};

use thiserror::Error;

/// Errors from opening or reading a ZIP archive.
#[derive(Debug, Error)]
pub enum ZipArchiveError {
    #[error("open zip: {0}")]
    Open(#[source] std::io::Error),
    #[error("read zip: {0}")]
    Read(#[source] std::io::Error),
    #[error("not a zip archive: end of central directory not found")]
    NotZip,
    #[error("corrupt zip: {0}")]
    Corrupt(String),
    #[error("unsupported compression method {method} for {name}")]
    UnsupportedMethod { method: u16, name: String },
    #[error("decompress {name}: {message}")]
    Decompress { name: String, message: String },
    #[error("crc mismatch for {name}: expected {expected:08x}, got {actual:08x}")]
    CrcMismatch {
        name: String,
        expected: u32,
        actual: u32,
    },
    #[error("zip64 member {name} exceeds addressable size")]
    TooLarge { name: String },
    #[error("zip member {name:?} exceeds the {limit}-byte decompression limit")]
    MemberTooLarge { name: String, limit: u64 },
}

pub type Result<T> = std::result::Result<T, ZipArchiveError>;

/// End-of-central-directory signature (`PK\x05\x06`).
const EOCD_SIG: u32 = 0x0605_4b50;
/// ZIP64 end-of-central-directory locator (`PK\x06\x07`).
const ZIP64_LOCATOR_SIG: u32 = 0x0706_4b50;
/// ZIP64 end-of-central-directory record (`PK\x06\x06`).
const ZIP64_EOCD_SIG: u32 = 0x0606_4b50;
/// Central-directory file header (`PK\x01\x02`).
const CENTRAL_SIG: u32 = 0x0201_4b50;
/// Local file header (`PK\x03\x04`).
const LOCAL_SIG: u32 = 0x0403_4b50;

/// Fixed part of a central-directory record (through the extra-field length).
const CENTRAL_FIXED_LEN: usize = 46;
/// Fixed part of a local file header (through the filename length).
const LOCAL_FIXED_LEN: usize = 30;

/// Compression method: stored.
const METHOD_STORE: u16 = 0;
/// Compression method: deflate.
const METHOD_DEFLATE: u16 = 8;

/// Hard ceiling for one decompressed member. Streaming keeps memory flat, but
/// a decompression bomb would still fill the disk and burn CPU; 8 GiB is far
/// above any member real Scoop artifacts ship while bounding the damage.
const MAX_MEMBER_UNCOMPRESSED_SIZE: u64 = 8 * 1024 * 1024 * 1024;

/// One member of a ZIP archive, as described by the central directory.
#[derive(Debug, Clone)]
pub struct ZipEntry {
    /// Member path exactly as stored (before backslash normalization).
    pub name: String,
    pub method: u16,
    pub crc32: u32,
    pub compressed_size: u64,
    pub uncompressed_size: u64,
    /// Offset of the local file header.
    local_header_offset: u64,
    /// General-purpose bit flags (bit 3 = sizes in data descriptor).
    flags: u16,
}

impl ZipEntry {
    /// Whether the entry denotes a directory.
    ///
    /// ZIP has no first-class directories; a trailing separator is the
    /// convention, and zero-byte marker entries are filtered later by
    /// `archmember::is_directory_placeholder`.
    pub fn is_dir(&self) -> bool {
        self.name.ends_with('/') || self.name.ends_with('\\')
    }

    /// Whether bit 3 is set, meaning sizes travel in a trailing data
    /// descriptor instead of the local header.
    fn uses_data_descriptor(&self) -> bool {
        self.flags & 0x0008 != 0
    }
}

/// A seekable ZIP archive. Holds the underlying file open; entries are
/// decompressed on demand.
pub struct ZipArchive {
    file: File,
    path: PathBuf,
    entries: Vec<ZipEntry>,
}

impl ZipArchive {
    /// Open `path` and parse its central directory.
    pub fn open(path: impl AsRef<Path>) -> Result<ZipArchive> {
        let path = path.as_ref().to_path_buf();
        let mut file = File::open(&path).map_err(ZipArchiveError::Open)?;
        let entries = read_central_directory(&mut file)?;
        Ok(ZipArchive {
            file,
            path,
            entries,
        })
    }

    /// The archive path this reader was opened from.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// All members, in central-directory order.
    pub fn entries(&self) -> &[ZipEntry] {
        &self.entries
    }

    /// Members that carry file content: directories and .NET-style zero-byte
    /// folder markers are dropped.
    pub fn file_entries(&self) -> Vec<&ZipEntry> {
        self.entries
            .iter()
            .filter(|e| {
                if e.is_dir() {
                    return false;
                }
                !crate::archmember::is_directory_placeholder(&e.name, e.uncompressed_size)
            })
            .collect()
    }

    /// Decompress one member fully into memory.
    ///
    /// Only intended for small members (tests); the extraction pipeline calls
    /// [`ZipArchive::copy_entry`] and streams instead. The capacity hint is
    /// attacker-controlled, so it is clamped before allocating.
    pub fn read_entry(&mut self, entry: &ZipEntry) -> Result<Vec<u8>> {
        let cap = usize::try_from(entry.uncompressed_size.min(1 << 20)).unwrap_or(1 << 20);
        let mut out = Vec::with_capacity(cap);
        self.copy_entry(entry, &mut out)?;
        Ok(out)
    }

    /// Decompress one member, writing the output into `sink`.
    ///
    /// Verifies the member CRC-32 after the write completes: a truncated or
    /// corrupted deflate stream can still decompress "successfully".
    pub fn copy_entry(&mut self, entry: &ZipEntry, sink: &mut impl std::io::Write) -> Result<u64> {
        self.copy_entry_with_limit(entry, sink, MAX_MEMBER_UNCOMPRESSED_SIZE)
    }

    /// [`ZipArchive::copy_entry`] with an explicit decompression cap (tests
    /// use a small one; production uses [`MAX_MEMBER_UNCOMPRESSED_SIZE`]).
    fn copy_entry_with_limit(
        &mut self,
        entry: &ZipEntry,
        sink: &mut impl std::io::Write,
        limit: u64,
    ) -> Result<u64> {
        let data = self.member_data(entry)?;
        extract_member_with_limit(&mut self.file, data, entry, sink, limit)
    }

    /// Where one member's compressed payload lives.
    ///
    /// Resolving it touches only the local header, so a caller that extracts
    /// entries on worker threads resolves every range up front (serially) and
    /// then hands [`extract_member`] a `File` it opened itself.
    pub fn member_data(&mut self, entry: &ZipEntry) -> Result<MemberData> {
        let (offset, compressed_size) = self.member_bounds(entry)?;
        Ok(MemberData {
            offset,
            compressed_size,
        })
    }

    /// Byte offset of the member's compressed payload and its length, as taken
    /// from the *local* header (which is authoritative when the central
    /// directory carries zeros because of a data descriptor).
    fn member_bounds(&mut self, entry: &ZipEntry) -> Result<(u64, u64)> {
        self.file
            .seek(SeekFrom::Start(entry.local_header_offset))
            .map_err(ZipArchiveError::Read)?;
        let mut fixed = [0u8; LOCAL_FIXED_LEN];
        self.file
            .read_exact(&mut fixed)
            .map_err(ZipArchiveError::Read)?;
        let sig = u32::from_le_bytes(fixed[0..4].try_into().unwrap());
        if sig != LOCAL_SIG {
            return Err(ZipArchiveError::Corrupt(format!(
                "bad local header for {}",
                entry.name
            )));
        }
        let name_len = u16::from_le_bytes(fixed[26..28].try_into().unwrap()) as u64;
        let extra_len = u16::from_le_bytes(fixed[28..30].try_into().unwrap()) as u64;
        let data_offset = entry.local_header_offset + LOCAL_FIXED_LEN as u64 + name_len + extra_len;

        // Prefer the local header sizes; fall back to the central directory
        // (a huge compressed size means a ZIP64 override lives in the local
        // extra field — `parse_zip64_central_extra` already resolved those
        // when reading the central directory, so the central values are safe).
        let mut comp_size = u32::from_le_bytes(fixed[18..22].try_into().unwrap()) as u64;
        let uncomp_size = u32::from_le_bytes(fixed[22..26].try_into().unwrap()) as u64;
        if entry.uses_data_descriptor() && comp_size == 0 && uncomp_size == 0 {
            comp_size = entry.compressed_size;
        }
        if comp_size == 0xFFFF_FFFF {
            comp_size = entry.compressed_size;
        }
        Ok((data_offset, comp_size))
    }
}

/// Where one member's compressed bytes are: the offset of the payload (past
/// the local header) and its length.
#[derive(Debug, Clone, Copy)]
pub struct MemberData {
    pub offset: u64,
    pub compressed_size: u64,
}

/// Decompress one member whose payload lives at `data` in `file`.
///
/// `file` may be owned by a worker thread (one handle per thread), which is
/// how the extractor fans members out without a shared seek position.
pub fn extract_member(
    file: &mut File,
    data: MemberData,
    entry: &ZipEntry,
    sink: &mut impl std::io::Write,
) -> Result<u64> {
    extract_member_with_limit(file, data, entry, sink, MAX_MEMBER_UNCOMPRESSED_SIZE)
}

/// [`extract_member`] with an explicit decompression cap.
pub fn extract_member_with_limit(
    file: &mut File,
    data: MemberData,
    entry: &ZipEntry,
    sink: &mut impl std::io::Write,
    limit: u64,
) -> Result<u64> {
    if entry.uncompressed_size > limit {
        return Err(ZipArchiveError::MemberTooLarge {
            name: entry.name.clone(),
            limit,
        });
    }
    if data.compressed_size == 0 && entry.uncompressed_size == 0 {
        return Ok(0);
    }
    file.seek(SeekFrom::Start(data.offset))
        .map_err(ZipArchiveError::Read)?;
    let source = (&mut *file).take(data.compressed_size);

    match entry.method {
        METHOD_STORE => copy_stored(source, sink, entry),
        METHOD_DEFLATE => inflate_member(source, sink, entry, limit),
        other => Err(ZipArchiveError::UnsupportedMethod {
            method: other,
            name: entry.name.clone(),
        }),
    }
}

/// Copy a stored (uncompressed) member, verifying size and CRC.
fn copy_stored(
    mut source: impl Read,
    sink: &mut impl std::io::Write,
    entry: &ZipEntry,
) -> Result<u64> {
    let mut hasher = Crc32::new();
    let mut buf = [0u8; 128 * 1024];
    let mut total: u64 = 0;
    loop {
        let n = source.read(&mut buf).map_err(ZipArchiveError::Read)?;
        if n == 0 {
            break;
        }
        let crc_start = crate::extract_profile::start();
        hasher.update(&buf[..n]);
        crate::extract_profile::add_crc(crc_start);
        let write_start = crate::extract_profile::start();
        sink.write_all(&buf[..n]).map_err(ZipArchiveError::Read)?;
        crate::extract_profile::add_write(write_start);
        crate::extract_profile::add_bytes(n as u64);
        total += n as u64;
    }
    verify_member(entry, total, hasher.finish())?;
    Ok(total)
}

/// Inflate one deflate member into `sink` in fixed-size chunks, verifying size
/// and CRC.
///
/// Raw deflate (`expect_header = false`, window bits 15), 64 KiB input/output
/// blocks, CRC and sink writes interleaved so memory stays constant and the
/// hash is computed on the way out. A decompression bomb is additionally
/// bounded by `limit` (checked before each write).
fn inflate_member(
    mut source: impl Read,
    sink: &mut impl std::io::Write,
    entry: &ZipEntry,
    limit: u64,
) -> Result<u64> {
    use zlib_rs::{Inflate, InflateFlush, Status};

    let mut state = Inflate::new(false, 15u8);
    let mut input = [0u8; 64 * 1024];
    let mut output = [0u8; 64 * 1024];
    let mut input_len = 0usize;
    let mut input_pos = 0usize;
    let mut eof = false;
    let mut hasher = Crc32::new();
    let mut total: u64 = 0;

    loop {
        if input_pos == input_len && !eof {
            input_len = source.read(&mut input).map_err(ZipArchiveError::Read)?;
            input_pos = 0;
            if input_len == 0 {
                eof = true;
            }
        }

        let before_in = state.total_in();
        let before_out = state.total_out();
        let flush = if eof {
            InflateFlush::Finish
        } else {
            InflateFlush::NoFlush
        };
        let inflate_start = crate::extract_profile::start();
        let status = state
            .decompress(&input[input_pos..input_len], &mut output, flush)
            .map_err(|err| ZipArchiveError::Decompress {
                name: entry.name.clone(),
                message: format!("{err:?}"),
            })?;
        crate::extract_profile::add_inflate(inflate_start);

        let consumed = (state.total_in() - before_in) as usize;
        let written = (state.total_out() - before_out) as usize;
        input_pos += consumed;

        if written > 0 {
            total += written as u64;
            if total > limit {
                return Err(ZipArchiveError::MemberTooLarge {
                    name: entry.name.clone(),
                    limit,
                });
            }
            let crc_start = crate::extract_profile::start();
            hasher.update(&output[..written]);
            crate::extract_profile::add_crc(crc_start);
            let write_start = crate::extract_profile::start();
            sink.write_all(&output[..written])
                .map_err(ZipArchiveError::Read)?;
            crate::extract_profile::add_write(write_start);
            crate::extract_profile::add_bytes(written as u64);
        }

        match status {
            Status::StreamEnd => break,
            Status::Ok | Status::BufError => {}
        }

        if eof && input_pos == input_len && consumed == 0 && written == 0 {
            return Err(ZipArchiveError::Decompress {
                name: entry.name.clone(),
                message: "truncated deflate stream".to_string(),
            });
        }
    }

    verify_member(entry, total, hasher.finish())?;
    Ok(total)
}

/// Reject a member whose decompressed size or CRC does not match the header.
fn verify_member(entry: &ZipEntry, size: u64, crc: u32) -> Result<()> {
    if entry.uncompressed_size != 0 && size != entry.uncompressed_size {
        return Err(ZipArchiveError::Corrupt(format!(
            "size mismatch for {}: expected {}, got {}",
            entry.name, entry.uncompressed_size, size
        )));
    }
    if entry.crc32 != 0 && crc != entry.crc32 {
        return Err(ZipArchiveError::CrcMismatch {
            name: entry.name.clone(),
            expected: entry.crc32,
            actual: crc,
        });
    }
    Ok(())
}

/// Walk backwards from EOF to find and parse the central directory.
fn read_central_directory(file: &mut File) -> Result<Vec<ZipEntry>> {
    let file_len = file.seek(SeekFrom::End(0)).map_err(ZipArchiveError::Read)?;
    if file_len < 22 {
        return Err(ZipArchiveError::NotZip);
    }
    // The EOCD is 22 bytes plus a comment of at most 65535 bytes; a prepended
    // self-extractor stub may push the real record anywhere, so search the
    // largest plausible window.
    let max_back = (22 + 65_535).min(file_len);
    let window_start = file_len - max_back;
    let mut window = vec![0u8; max_back as usize];
    file.seek(SeekFrom::Start(window_start))
        .map_err(ZipArchiveError::Read)?;
    file.read_exact(&mut window)
        .map_err(ZipArchiveError::Read)?;

    let eocd_pos = find_signature_backwards(&window, EOCD_SIG).ok_or(ZipArchiveError::NotZip)?;
    let eocd = &window[eocd_pos..];

    let mut entry_count = u16::from_le_bytes(eocd[10..12].try_into().unwrap()) as u64;
    let mut cd_size = u32::from_le_bytes(eocd[12..16].try_into().unwrap()) as u64;
    let mut cd_offset = u32::from_le_bytes(eocd[16..20].try_into().unwrap()) as u64;

    let eocd_abs = window_start + eocd_pos as u64;

    // ZIP64: the 32-bit fields saturate and the real values live in the ZIP64
    // EOCD, located through the record just before the EOCD.
    if entry_count == 0xFFFF || cd_size == 0xFFFF_FFFF || cd_offset == 0xFFFF_FFFF {
        if let Some((count, size, offset)) = read_zip64_eocd(file, eocd_abs)? {
            entry_count = count;
            cd_size = size;
            cd_offset = offset;
        }
    }

    // A prepended stub (or a rewriting archive tool) can leave `cd_offset`
    // relative to a coordinate system shifted from ours; recompute from the
    // EOCD position when the two disagree by a constant.
    let expected_cd_end = eocd_abs;
    let mut base = 0i64;
    if cd_offset + cd_size > expected_cd_end {
        base = expected_cd_end as i64 - (cd_offset + cd_size) as i64;
    }
    let actual_cd_offset = (cd_offset as i64 + base) as u64;

    if cd_size > file_len {
        return Err(ZipArchiveError::Corrupt(
            "central directory too large".into(),
        ));
    }
    let mut cd = vec![0u8; cd_size as usize];
    file.seek(SeekFrom::Start(actual_cd_offset))
        .map_err(ZipArchiveError::Read)?;
    file.read_exact(&mut cd).map_err(ZipArchiveError::Read)?;

    let mut entries = Vec::with_capacity(entry_count.min(1 << 16) as usize);
    let mut pos = 0usize;
    while pos + CENTRAL_FIXED_LEN <= cd.len() {
        let sig = u32::from_le_bytes(cd[pos..pos + 4].try_into().unwrap());
        if sig != CENTRAL_SIG {
            break;
        }
        let flags = u16::from_le_bytes(cd[pos + 8..pos + 10].try_into().unwrap());
        let method = u16::from_le_bytes(cd[pos + 10..pos + 12].try_into().unwrap());
        let crc32 = u32::from_le_bytes(cd[pos + 16..pos + 20].try_into().unwrap());
        let mut compressed_size =
            u32::from_le_bytes(cd[pos + 20..pos + 24].try_into().unwrap()) as u64;
        let mut uncompressed_size =
            u32::from_le_bytes(cd[pos + 24..pos + 28].try_into().unwrap()) as u64;
        let name_len = u16::from_le_bytes(cd[pos + 28..pos + 30].try_into().unwrap()) as usize;
        let extra_len = u16::from_le_bytes(cd[pos + 30..pos + 32].try_into().unwrap()) as usize;
        let comment_len = u16::from_le_bytes(cd[pos + 32..pos + 34].try_into().unwrap()) as usize;
        let mut local_header_offset =
            u32::from_le_bytes(cd[pos + 42..pos + 46].try_into().unwrap()) as u64;

        let name_start = pos + CENTRAL_FIXED_LEN;
        let name_end = name_start + name_len;
        if name_end > cd.len() {
            return Err(ZipArchiveError::Corrupt("truncated member name".into()));
        }
        let name = decode_name(&cd[name_start..name_end], flags)?;
        let extra_start = name_end;
        let extra_end = (extra_start + extra_len).min(cd.len());
        let extra = cd[extra_start..extra_end].to_vec();

        // ZIP64 extended information lives in the central extra field too.
        if compressed_size == 0xFFFF_FFFF
            || uncompressed_size == 0xFFFF_FFFF
            || local_header_offset == 0xFFFF_FFFF
        {
            parse_zip64_central_extra(
                &extra,
                &mut compressed_size,
                &mut uncompressed_size,
                &mut local_header_offset,
            );
        }

        entries.push(ZipEntry {
            name,
            method,
            crc32,
            compressed_size,
            uncompressed_size,
            local_header_offset: (local_header_offset as i64 + base) as u64,
            flags,
        });

        pos = extra_end + comment_len;
    }

    if entries.is_empty() {
        return Err(ZipArchiveError::Corrupt(
            "no central directory entries".into(),
        ));
    }
    Ok(entries)
}

/// Locate and parse the ZIP64 EOCD record, returning (count, cd_size, cd_offset).
fn read_zip64_eocd(file: &mut File, eocd_abs: u64) -> Result<Option<(u64, u64, u64)>> {
    let locator_len = 20u64;
    if eocd_abs < locator_len {
        return Ok(None);
    }
    let mut locator = [0u8; 20];
    file.seek(SeekFrom::Start(eocd_abs - locator_len))
        .map_err(ZipArchiveError::Read)?;
    file.read_exact(&mut locator)
        .map_err(ZipArchiveError::Read)?;
    if u32::from_le_bytes(locator[0..4].try_into().unwrap()) != ZIP64_LOCATOR_SIG {
        return Ok(None);
    }
    let z64_eocd_offset = u64::from_le_bytes(locator[8..16].try_into().unwrap());
    let mut fixed = [0u8; 56];
    file.seek(SeekFrom::Start(z64_eocd_offset))
        .map_err(ZipArchiveError::Read)?;
    if file.read_exact(&mut fixed).is_err() {
        return Ok(None);
    }
    if u32::from_le_bytes(fixed[0..4].try_into().unwrap()) != ZIP64_EOCD_SIG {
        return Ok(None);
    }
    let count = u64::from_le_bytes(fixed[32..40].try_into().unwrap());
    let cd_size = u64::from_le_bytes(fixed[40..48].try_into().unwrap());
    let cd_offset = u64::from_le_bytes(fixed[48..56].try_into().unwrap());
    Ok(Some((count, cd_size, cd_offset)))
}

/// Apply the ZIP64 extended-information extra field to saturating values.
fn parse_zip64_central_extra(
    extra: &[u8],
    compressed_size: &mut u64,
    uncompressed_size: &mut u64,
    local_header_offset: &mut u64,
) -> Option<()> {
    let mut pos = 0usize;
    while pos + 4 <= extra.len() {
        let id = u16::from_le_bytes(extra[pos..pos + 2].try_into().unwrap());
        let size = u16::from_le_bytes(extra[pos + 2..pos + 4].try_into().unwrap()) as usize;
        let body_start = pos + 4;
        let body_end = (body_start + size).min(extra.len());
        if id == 0x0001 {
            let body = &extra[body_start..body_end];
            let mut p = 0usize;
            let take = |p: &mut usize| -> Option<u64> {
                if *p + 8 > body.len() {
                    return None;
                }
                let v = u64::from_le_bytes(body[*p..*p + 8].try_into().unwrap());
                *p += 8;
                Some(v)
            };
            if *uncompressed_size == 0xFFFF_FFFF {
                if let Some(v) = take(&mut p) {
                    *uncompressed_size = v;
                }
            }
            if *compressed_size == 0xFFFF_FFFF {
                if let Some(v) = take(&mut p) {
                    *compressed_size = v;
                }
            }
            if *local_header_offset == 0xFFFF_FFFF {
                if let Some(v) = take(&mut p) {
                    *local_header_offset = v;
                }
            }
            return Some(());
        }
        pos = body_end;
    }
    None
}

/// Decode a member name. Bit 11 marks UTF-8; otherwise the bytes are treated
/// as UTF-8 when valid (what Windows tools in practice emit) and lossily
/// decoded as a last resort.
fn decode_name(raw: &[u8], _flags: u16) -> Result<String> {
    match std::str::from_utf8(raw) {
        Ok(s) => Ok(s.to_string()),
        Err(_) => Ok(String::from_utf8_lossy(raw).into_owned()),
    }
}

/// Last index of the little-endian 4-byte `sig` in `data`.
fn find_signature_backwards(data: &[u8], sig: u32) -> Option<usize> {
    let needle = sig.to_le_bytes();
    if data.len() < 4 {
        return None;
    }
    (0..=data.len() - 4)
        .rev()
        .find(|&i| data[i..i + 4] == needle)
}

/// CRC-32 (IEEE 802.3, the polynomial ZIP uses).
///
/// `crc32fast` uses PCLMUL/SSE4.2 when the CPU supports it; fall back to
/// slice-by-8 otherwise. The previous byte-at-a-time table ran at roughly
/// 300 MB/s on a 37 MB package.
pub struct Crc32 {
    hasher: crc32fast::Hasher,
}

impl Crc32 {
    /// Incremental CRC-32 hasher (also used by the tar gzip trailer check).
    pub(crate) fn new() -> Crc32 {
        Crc32 {
            hasher: crc32fast::Hasher::new(),
        }
    }

    pub(crate) fn update(&mut self, data: &[u8]) {
        self.hasher.update(data);
    }

    pub(crate) fn finish(self) -> u32 {
        self.hasher.finalize()
    }

    /// One-shot CRC-32 of a buffer.
    pub fn compute(data: &[u8]) -> u32 {
        crc32fast::hash(data)
    }
}

/// Convenience: build a relPath → uncompressed-size map for an archive, used
/// by the extraction pipeline to size its progress reporting.
pub fn member_sizes(archive: &ZipArchive) -> BTreeMap<String, u64> {
    archive
        .file_entries()
        .into_iter()
        .map(|e| {
            (
                crate::archmember::normalize_member(&e.name),
                e.uncompressed_size,
            )
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;
    use std::io::Write;

    fn write_zip(dir: &TempDir, members: &[(&str, &[u8], bool)]) -> PathBuf {
        let path = dir.join("test.zip");
        let bytes = build_zip_with_offsets(members);
        let mut f = File::create(&path).unwrap();
        f.write_all(&bytes).unwrap();
        path
    }

    /// Build a ZIP tracking each member's local-header offset correctly.
    fn build_zip_with_offsets(members: &[(&str, &[u8], bool)]) -> Vec<u8> {
        let mut out: Vec<u8> = Vec::new();
        let mut central: Vec<(String, u32, u16, u64, u64, u64)> = Vec::new();

        for (name, contents, deflate) in members {
            let crc = Crc32::compute(contents);
            let payload = if *deflate && !contents.is_empty() {
                crate::testutil::deflate_raw(contents)
            } else {
                contents.to_vec()
            };
            let method = if *deflate && !contents.is_empty() {
                8u16
            } else {
                0u16
            };
            let offset = out.len() as u64;
            let name_bytes = name.as_bytes();

            out.extend_from_slice(&LOCAL_SIG.to_le_bytes());
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
            out.extend_from_slice(&CENTRAL_SIG.to_le_bytes());
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

        out.extend_from_slice(&EOCD_SIG.to_le_bytes());
        out.extend_from_slice(&0u16.to_le_bytes());
        out.extend_from_slice(&0u16.to_le_bytes());
        out.extend_from_slice(&(central.len() as u16).to_le_bytes());
        out.extend_from_slice(&(central.len() as u16).to_le_bytes());
        out.extend_from_slice(&(cd_size as u32).to_le_bytes());
        out.extend_from_slice(&(cd_offset as u32).to_le_bytes());
        out.extend_from_slice(&0u16.to_le_bytes());
        out
    }

    /// A lying central directory (declared size small, actual stream large)
    /// must hit the streaming cap instead of inflating without bound, and no
    /// bytes past the cap may reach the sink.
    #[test]
    fn deflate_member_over_limit_is_rejected() {
        let dir = TempDir::new("glue-ziparchive-limit");
        let payload = b"0123456789abcdef"; // 16 bytes
        let mut bytes = build_zip_with_offsets(&[("big.txt", payload, true)]);

        // Patch the declared uncompressed size to 4 in both the local header
        // (offset 22) and the central-directory record (signature + 24), so
        // only the streaming cap can catch the expansion.
        bytes[22..26].copy_from_slice(&4u32.to_le_bytes());
        let cd = bytes
            .windows(4)
            .position(|w| w == CENTRAL_SIG.to_le_bytes())
            .expect("central directory");
        bytes[cd + 24..cd + 28].copy_from_slice(&4u32.to_le_bytes());

        let path = dir.join("lying.zip");
        std::fs::write(&path, &bytes).unwrap();
        let mut archive = ZipArchive::open(&path).unwrap();
        let entry = archive.file_entries()[0].clone();
        assert_eq!(entry.uncompressed_size, 4);

        let mut sink = Vec::new();
        let err = archive
            .copy_entry_with_limit(&entry, &mut sink, 4)
            .expect_err("cap must trigger");
        assert!(
            matches!(err, ZipArchiveError::MemberTooLarge { limit: 4, .. }),
            "{err:?}"
        );
        assert!(sink.is_empty(), "no bytes may be written past the cap");
    }

    /// Deflate members are streamed in bounded chunks: no single write may
    /// exceed the 64 KiB loop buffer, so memory stays flat for large members.
    #[test]
    fn large_deflate_member_streams_in_bounded_chunks() {
        struct ChunkCheck {
            total: u64,
            largest: usize,
        }
        impl std::io::Write for ChunkCheck {
            fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
                self.largest = self.largest.max(buf.len());
                self.total += buf.len() as u64;
                Ok(buf.len())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }

        let dir = TempDir::new("glue-ziparchive-stream");
        let payload = vec![0x5au8; 8 * 1024 * 1024];
        let path = write_zip(&dir, &[("large.bin", &payload, true)]);
        let mut archive = ZipArchive::open(&path).unwrap();
        let entry = archive.file_entries()[0].clone();
        assert_eq!(entry.method, METHOD_DEFLATE);

        let mut sink = ChunkCheck {
            total: 0,
            largest: 0,
        };
        let written = archive.copy_entry(&entry, &mut sink).unwrap();
        assert_eq!(written, payload.len() as u64);
        assert_eq!(sink.total, payload.len() as u64);
        assert!(
            sink.largest <= 64 * 1024,
            "chunk of {} bytes exceeds the streaming buffer",
            sink.largest
        );
    }

    #[test]
    fn crc32_matches_known_vector() {
        // The canonical CRC-32 of "123456789" is 0xCBF43926.
        assert_eq!(Crc32::compute(b"123456789"), 0xCBF4_3926);
    }

    #[test]
    fn crc32_incremental_matches_one_shot() {
        let data = b"123456789";
        let mut hasher = Crc32::new();
        hasher.update(&data[..4]);
        hasher.update(&data[4..]);
        assert_eq!(hasher.finish(), 0xCBF4_3926);
    }

    #[test]
    fn reads_stored_and_deflated_members() {
        let dir = TempDir::new("glue-ziparchive-read");
        let path = write_zip(
            &dir,
            &[
                ("bin/tool.exe", b"stored payload here", false),
                (
                    "share/readme.txt",
                    b"deflated payload, repeated repeated repeated",
                    true,
                ),
            ],
        );
        let mut archive = ZipArchive::open(&path).unwrap();
        assert_eq!(archive.entries().len(), 2);
        let files: Vec<ZipEntry> = archive.file_entries().into_iter().cloned().collect();
        assert_eq!(files.len(), 2);

        let first = files[0].clone();
        assert_eq!(first.name, "bin/tool.exe");
        assert_eq!(archive.read_entry(&first).unwrap(), b"stored payload here");

        let second = files[1].clone();
        assert_eq!(second.method, METHOD_DEFLATE);
        assert_eq!(
            archive.read_entry(&second).unwrap(),
            b"deflated payload, repeated repeated repeated"
        );
    }

    #[test]
    fn empty_member_reads_as_empty() {
        let dir = TempDir::new("glue-ziparchive-empty");
        let path = write_zip(&dir, &[("empty.txt", b"", false)]);
        let mut archive = ZipArchive::open(&path).unwrap();
        let e = archive.file_entries()[0].clone();
        assert!(archive.read_entry(&e).unwrap().is_empty());
    }

    #[test]
    fn directory_entries_are_filtered() {
        let dir = TempDir::new("glue-ziparchive-dir");
        let path = write_zip(
            &dir,
            &[
                ("runtimes/", b"", false),
                ("runtimes/win-x64/", b"", false),
                ("app.exe", b"MZ", false),
            ],
        );
        let archive = ZipArchive::open(&path).unwrap();
        let files = archive.file_entries();
        assert_eq!(files.len(), 1);
        assert_eq!(files[0].name, "app.exe");
    }

    #[test]
    fn corrupt_crc_is_rejected() {
        let dir = TempDir::new("glue-ziparchive-crc");
        let mut bytes = build_zip_with_offsets(&[("a.txt", b"hello world", false)]);
        // Corrupt the stored payload ("hello world" starts right after the
        // local header + name).
        let data_start = LOCAL_FIXED_LEN + "a.txt".len();
        bytes[data_start] ^= 0xFF;
        let path = dir.join("bad.zip");
        std::fs::write(&path, &bytes).unwrap();

        let mut archive = ZipArchive::open(&path).unwrap();
        let e = archive.file_entries()[0].clone();
        assert!(matches!(
            archive.read_entry(&e),
            Err(ZipArchiveError::CrcMismatch { .. })
        ));
    }

    #[test]
    fn non_zip_is_rejected() {
        let dir = TempDir::new("glue-ziparchive-notzip");
        let path = dir.join("plain.bin");
        std::fs::write(&path, b"this is definitely not a zip archive at all").unwrap();
        assert!(matches!(
            ZipArchive::open(&path),
            Err(ZipArchiveError::NotZip)
        ));
    }

    #[test]
    fn member_sizes_normalizes_backslashes() {
        let dir = TempDir::new("glue-ziparchive-sizes");
        let path = write_zip(&dir, &[(r"bin\tool.exe", b"abc", false)]);
        let archive = ZipArchive::open(&path).unwrap();
        let sizes = member_sizes(&archive);
        assert_eq!(sizes.get("bin/tool.exe"), Some(&3));
    }
}
