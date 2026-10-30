//! In-process tar reader with a streaming gzip wrapper.
//!
//! Extends the ZIP divergence documented in [`crate::extractor`] to the tar
//! family: plain `.tar`, `.tar.gz` and `.tgz` archives are read here, so those
//! formats need no external `7z.exe` either. `.tar.bz2` / `.tar.xz` still
//! route through 7z (no in-process bzip2/LZMA decoders).
//!
//! The reader is sequential — tar has no central directory — so callers pull
//! entries with [`TarArchive::next_entry`] and stream each payload with
//! [`TarArchive::copy_entry`]. gzip is transparently decompressed by
//! [`GzipReader`] in bounded-memory chunks: a `.tar.gz` is never fully held
//! in RAM, and its CRC-32 / ISIZE trailer is verified like the ZIP path
//! verifies per-member CRCs.
//!
//! Supported header flavours: v7 (no magic), POSIX ustar (with `prefix`),
//! GNU (`L`/`K` long-name entries, base-256 numeric fields) and pax
//! (`x`/`g` extended headers, at least `path`, `linkpath`, `size`).

use std::fs::File;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::path::Path;

use thiserror::Error;
use zlib_rs::{Inflate, InflateFlush, Status};

use crate::ziparchive::Crc32;

/// tar block size.
const BLOCK_SIZE: usize = 512;

/// Streaming window for gzip inflate and tar payload copies.
const CHUNK: usize = 64 * 1024;

/// Errors from tar parsing / extraction.
#[derive(Debug, Error)]
pub enum TarError {
    #[error("read tar: {0}")]
    Read(io::Error),
    #[error("tar: write file: {0}")]
    Write(io::Error),
    #[error("tar: truncated header at offset {offset}")]
    TruncatedHeader { offset: u64 },
    #[error("tar: truncated payload for {path}")]
    TruncatedPayload { path: String },
    #[error("tar: bad header checksum at offset {offset}")]
    BadChecksum { offset: u64 },
    #[error("tar: bad numeric field at offset {offset}")]
    BadNumeric { offset: u64 },
    #[error("tar: unsupported member type {typeflag} for {path}")]
    UnsupportedMemberType { typeflag: u8, path: String },
    #[error("tar: hardlink target {target} not found for {path}")]
    HardlinkTargetMissing { target: String, path: String },
    #[error("gzip: {0}")]
    Gzip(String),
}

/// One tar member (metadata entries — `L`, `K`, `x`, `g` — are consumed
/// internally and never surface as entries).
#[derive(Debug, Clone)]
pub struct TarEntry {
    /// Member path as written in the archive (before normalization).
    pub name: String,
    /// Payload size in bytes (after pax `size` overrides).
    pub size: u64,
    /// Type flag: `b'0'`/`0` regular file, `b'5'` directory,
    /// `b'1'` hardlink, `b'2'` symlink.
    pub typeflag: u8,
    /// Link target for hardlink/symlink entries.
    pub linkname: String,
}

impl TarEntry {
    pub fn is_file(&self) -> bool {
        self.typeflag == b'0' || self.typeflag == 0
    }

    pub fn is_dir(&self) -> bool {
        self.typeflag == b'5'
    }
}

/// Sequential tar reader over a plain or gzip-compressed file.
pub struct TarArchive {
    inner: Box<dyn Read>,
    /// Byte offset of the next block to read (for error messages).
    offset: u64,
    /// Payload bytes of the most recent entry not yet consumed by
    /// [`TarArchive::copy_entry`]; skipped by the next `next_entry` call.
    pending: u64,
    /// pax global (`g`) overrides: they apply to *every* subsequent entry
    /// until end of archive, unlike per-entry `x` headers.
    pax_global_path: Option<String>,
    pax_global_link: Option<String>,
    pax_global_size: Option<u64>,
}

impl TarArchive {
    /// Open a tar (or gzip-compressed tar) file.
    ///
    /// gzip is detected by magic bytes, so a mislabelled `.tar.gz` that is
    /// really a plain tar (or vice versa) still reads correctly.
    pub fn open(path: impl AsRef<Path>) -> Result<TarArchive, TarError> {
        let file = File::open(path).map_err(TarError::Read)?;
        let mut reader = BufReader::with_capacity(CHUNK, file);
        let gzip = {
            let buf = reader.fill_buf().map_err(TarError::Read)?;
            buf.len() >= 2 && buf[0] == 0x1f && buf[1] == 0x8b
        };
        let inner: Box<dyn Read> = if gzip {
            Box::new(GzipReader::new(reader)?)
        } else {
            Box::new(reader)
        };
        Ok(TarArchive {
            inner,
            offset: 0,
            pending: 0,
            pax_global_path: None,
            pax_global_link: None,
            pax_global_size: None,
        })
    }

    /// Advance to the next real entry, consuming metadata entries
    /// (`L`/`K`/`x`/`g`) transparently. `Ok(None)` at end of archive.
    ///
    /// The payload of a returned entry stays unread; either call
    /// [`TarArchive::copy_entry`] or let the next `next_entry` skip it.
    pub fn next_entry(&mut self) -> Result<Option<TarEntry>, TarError> {
        self.skip_pending()?;
        let mut long_name: Option<String> = None;
        let mut long_link: Option<String> = None;
        let mut pax_path: Option<String> = None;
        let mut pax_link: Option<String> = None;
        let mut pax_size: Option<u64> = None;
        loop {
            let Some(header) = self.read_block()? else {
                return Ok(None);
            };
            if header.iter().all(|&b| b == 0) {
                // End-of-archive marker (a single zero block is accepted;
                // two are the writer convention).
                return Ok(None);
            }
            let block_offset = self.offset - BLOCK_SIZE as u64;
            verify_checksum(&header, block_offset)?;
            let typeflag = header[156];
            let mut size = parse_numeric(&header[124..136]).ok_or(TarError::BadNumeric {
                offset: block_offset,
            })?;
            let name = cstr(&header[0..100]);
            let linkname = cstr(&header[157..257]);
            let prefix = cstr(&header[345..500]);

            match typeflag {
                // GNU long name / long linkname: payload belongs to the *next*
                // entry.
                b'L' => {
                    long_name = Some(read_string_payload(self, size)?);
                    continue;
                }
                b'K' => {
                    long_link = Some(read_string_payload(self, size)?);
                    continue;
                }
                // pax per-entry extended header: overrides the next entry only.
                b'x' => {
                    let data = self.read_payload_vec(size)?;
                    let (p, l, s) = parse_pax(&data);
                    if p.is_some() {
                        pax_path = p;
                    }
                    if l.is_some() {
                        pax_link = l;
                    }
                    if s.is_some() {
                        pax_size = s;
                    }
                    continue;
                }
                // pax global header: persists for every subsequent entry.
                b'g' => {
                    let data = self.read_payload_vec(size)?;
                    let (p, l, s) = parse_pax(&data);
                    if p.is_some() {
                        self.pax_global_path = p;
                    }
                    if l.is_some() {
                        self.pax_global_link = l;
                    }
                    if s.is_some() {
                        self.pax_global_size = s;
                    }
                    continue;
                }
                _ => {}
            }

            // Full name: ustar prefix, then pax global default, then GNU
            // longname, then per-entry pax — each later layer wins.
            let mut name = name;
            if !prefix.is_empty() && !name.starts_with('/') {
                name = format!("{prefix}/{name}");
            }
            if let Some(g) = &self.pax_global_path {
                name = g.clone();
            }
            if let Some(n) = long_name.take() {
                name = n;
            }
            if let Some(p) = pax_path.take() {
                name = p;
            }
            let mut linkname = linkname;
            if let Some(g) = &self.pax_global_link {
                linkname = g.clone();
            }
            if let Some(l) = long_link.take() {
                linkname = l;
            }
            if let Some(l) = pax_link.take() {
                linkname = l;
            }
            if let Some(s) = self.pax_global_size {
                size = s;
            }
            if let Some(s) = pax_size.take() {
                size = s;
            }

            self.pending = size;
            return Ok(Some(TarEntry {
                name,
                size,
                typeflag,
                linkname,
            }));
        }
    }

    /// Stream the payload of `entry` into `sink`, then skip the block
    /// padding. Verifies the full `entry.size` bytes arrive.
    pub fn copy_entry(&mut self, entry: &TarEntry, sink: &mut impl Write) -> Result<u64, TarError> {
        let mut buf = vec![0u8; CHUNK];
        let mut remaining = entry.size;
        let mut total: u64 = 0;
        while remaining > 0 {
            let want = remaining.min(buf.len() as u64) as usize;
            match self.inner.read(&mut buf[..want]) {
                Ok(0) => {
                    return Err(TarError::TruncatedPayload {
                        path: entry.name.clone(),
                    })
                }
                Ok(n) => {
                    sink.write_all(&buf[..n]).map_err(TarError::Write)?;
                    total += n as u64;
                    remaining -= n as u64;
                }
                Err(e) => return Err(wrap_io(e)),
            }
        }
        self.pending = 0;
        self.skip_padding(entry.size)?;
        Ok(total)
    }

    /// Read one 512-byte block. `Ok(None)` on a clean EOF, an error on a
    /// partial block.
    fn read_block(&mut self) -> Result<Option<[u8; BLOCK_SIZE]>, TarError> {
        let mut block = [0u8; BLOCK_SIZE];
        let mut filled = 0;
        while filled < BLOCK_SIZE {
            match self.inner.read(&mut block[filled..]) {
                Ok(0) => break,
                Ok(n) => filled += n,
                Err(e) => return Err(wrap_io(e)),
            }
        }
        if filled == 0 {
            return Ok(None);
        }
        if filled < BLOCK_SIZE {
            return Err(TarError::TruncatedHeader {
                offset: self.offset,
            });
        }
        self.offset += BLOCK_SIZE as u64;
        Ok(Some(block))
    }

    /// Read a metadata payload (`L`/`K`/`x`/`g`) fully into memory plus its
    /// padding. Metadata payloads are bounded in practice (long paths, pax
    /// records).
    fn read_payload_vec(&mut self, size: u64) -> Result<Vec<u8>, TarError> {
        let mut data = Vec::with_capacity(size.min(1 << 20) as usize);
        let mut remaining = size;
        let mut buf = [0u8; 8192];
        while remaining > 0 {
            let want = remaining.min(buf.len() as u64) as usize;
            match self.inner.read(&mut buf[..want]) {
                Ok(0) => {
                    return Err(TarError::TruncatedHeader {
                        offset: self.offset,
                    });
                }
                Ok(n) => {
                    data.extend_from_slice(&buf[..n]);
                    remaining -= n as u64;
                }
                Err(e) => return Err(wrap_io(e)),
            }
        }
        self.skip_padding(size)?;
        Ok(data)
    }

    /// Discard the payload + padding of a skipped entry.
    fn skip_pending(&mut self) -> Result<(), TarError> {
        if self.pending == 0 {
            return Ok(());
        }
        let size = self.pending;
        self.pending = 0;
        let mut remaining = size;
        let mut buf = [0u8; 8192];
        while remaining > 0 {
            let want = remaining.min(buf.len() as u64) as usize;
            match self.inner.read(&mut buf[..want]) {
                Ok(0) => {
                    return Err(TarError::TruncatedHeader {
                        offset: self.offset,
                    })
                }
                Ok(n) => remaining -= n as u64,
                Err(e) => return Err(wrap_io(e)),
            }
        }
        self.skip_padding(size)
    }

    /// Skip the NUL padding after a payload of `size` bytes.
    fn skip_padding(&mut self, size: u64) -> Result<(), TarError> {
        let pad = (BLOCK_SIZE as u64 - (size % BLOCK_SIZE as u64)) % BLOCK_SIZE as u64;
        if pad == 0 {
            return Ok(());
        }
        let mut buf = [0u8; BLOCK_SIZE];
        match self.inner.read_exact(&mut buf[..pad as usize]) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == io::ErrorKind::UnexpectedEof => Err(TarError::TruncatedHeader {
                offset: self.offset,
            }),
            Err(e) => Err(wrap_io(e)),
        }
    }
}

/// Read an `L`/`K` payload as a NUL-trimmed string.
fn read_string_payload(archive: &mut TarArchive, size: u64) -> Result<String, TarError> {
    let data = archive.read_payload_vec(size)?;
    Ok(String::from_utf8_lossy(&data)
        .trim_end_matches('\0')
        .to_string())
}

/// Validate a header checksum. Both the unsigned and the (historically used)
/// signed byte sums are accepted, matching GNU tar's reader.
fn verify_checksum(header: &[u8; BLOCK_SIZE], offset: u64) -> Result<(), TarError> {
    let bad = || TarError::BadChecksum { offset };
    let stored = {
        let mut v: u64 = 0;
        let mut any = false;
        for &b in &header[148..156] {
            match b {
                b'0'..=b'7' => {
                    v = v * 8 + (b - b'0') as u64;
                    any = true;
                }
                0 | b' ' if any => break,
                _ => return Err(bad()),
            }
        }
        if !any {
            return Err(bad());
        }
        v
    };
    let mut unsigned: u64 = 0;
    let mut signed: i64 = 0;
    for (i, &b) in header.iter().enumerate() {
        let b = if (148..156).contains(&i) { b' ' } else { b };
        unsigned += b as u64;
        signed += b as i8 as i64;
    }
    if stored == unsigned || stored as i64 == signed {
        Ok(())
    } else {
        Err(bad())
    }
}

/// Parse a tar numeric field: octal ASCII (space/NUL padded) or GNU
/// base-256 (high bit set).
fn parse_numeric(field: &[u8]) -> Option<u64> {
    if field.is_empty() {
        return Some(0);
    }
    if field[0] & 0x80 != 0 {
        // GNU base-256 big-endian, first byte's high bit is the marker.
        // Checked arithmetic: a 12-byte base-256 field can exceed u64 —
        // reject it as a bad numeric instead of silently wrapping.
        let mut v = (field[0] & 0x7f) as u64;
        for &b in &field[1..] {
            v = v.checked_mul(256)?.checked_add(b as u64)?;
        }
        return Some(v);
    }
    let mut v: u64 = 0;
    let mut any = false;
    for &b in field {
        match b {
            b'0'..=b'7' => {
                v = v.checked_mul(8)?.checked_add((b - b'0') as u64)?;
                any = true;
            }
            0 | b' ' => {
                if any {
                    break;
                }
            }
            _ => return None,
        }
    }
    Some(v)
}

/// NUL-terminated string field → owned string (lossy UTF-8).
fn cstr(field: &[u8]) -> String {
    let end = field.iter().position(|&b| b == 0).unwrap_or(field.len());
    String::from_utf8_lossy(&field[..end]).into_owned()
}

/// Parse pax extended-header records (`<len> <key>=<value>\n`), returning
/// the `path`, `linkpath` and `size` overrides.
fn parse_pax(data: &[u8]) -> (Option<String>, Option<String>, Option<u64>) {
    let mut path = None;
    let mut link = None;
    let mut size = None;
    let mut pos = 0;
    while pos < data.len() {
        let Some(space) = data[pos..].iter().position(|&b| b == b' ') else {
            break;
        };
        let space = pos + space;
        let len: usize = match std::str::from_utf8(&data[pos..space])
            .ok()
            .and_then(|s| s.parse().ok())
        {
            Some(l) => l,
            None => break,
        };
        if len == 0 || pos + len > data.len() {
            break;
        }
        let record = &data[pos..pos + len];
        // Record body after the length field, minus the trailing newline.
        let body = &record[space - pos + 1..record.len().saturating_sub(1)];
        if let Some(eq) = body.iter().position(|&b| b == b'=') {
            let key = std::str::from_utf8(&body[..eq]).unwrap_or("");
            let value = String::from_utf8_lossy(&body[eq + 1..]).into_owned();
            match key {
                "path" => path = Some(value),
                "linkpath" => link = Some(value),
                "size" => size = value.parse().ok(),
                _ => {}
            }
        }
        pos += len;
    }
    (path, link, size)
}

/// Carrier for gzip semantic failures so they survive the `io::Error`
/// round-trip through `Read` and surface as [`TarError::Gzip`].
#[derive(Debug)]
struct GzipFailure(String);

impl std::fmt::Display for GzipFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "gzip: {}", self.0)
    }
}

impl std::error::Error for GzipFailure {}

fn gzip_fail(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, GzipFailure(message.into()))
}

/// Map an io error from the underlying reader back to a tar error,
/// unwrapping gzip semantic failures.
fn wrap_io(e: io::Error) -> TarError {
    match e.get_ref().and_then(|r| r.downcast_ref::<GzipFailure>()) {
        Some(g) => TarError::Gzip(g.0.clone()),
        None => TarError::Read(e),
    }
}

/// Streaming gzip decompressor with bounded memory.
///
/// Parses the gzip header (FEXTRA/FNAME/FCOMMENT/FHCRC), inflates the raw
/// deflate stream through `zlib-rs`'s streaming API, and verifies the
/// CRC-32 / ISIZE trailer. The trailer is validated in the same inflate
/// round that ends the stream: deferring it would let a corrupt trailer
/// slip through when the tar stops on its zero-block marker and nobody
/// reads past it.
struct GzipReader<R: Read> {
    inner: R,
    state: Inflate,
    /// Compressed input window and read position within it.
    in_buf: Vec<u8>,
    in_pos: usize,
    /// Decompressed output window and read position within it.
    out_buf: Vec<u8>,
    out_pos: usize,
    /// Scratch output slice handed to `inflate` (reused).
    window: Vec<u8>,
    crc: Crc32,
    produced: u64,
    finished: bool,
}

impl<R: Read> GzipReader<R> {
    /// Wrap `inner` (positioned at the gzip magic) and parse the header.
    fn new(mut inner: R) -> Result<Self, TarError> {
        let mut magic = [0u8; 2];
        inner.read_exact(&mut magic).map_err(TarError::Read)?;
        if magic != [0x1f, 0x8b] {
            return Err(TarError::Gzip("not a gzip stream".to_string()));
        }
        let cm = read_u8(&mut inner)?;
        if cm != 8 {
            return Err(TarError::Gzip(format!(
                "unsupported compression method {cm}"
            )));
        }
        let flg = read_u8(&mut inner)?;
        // mtime (4), XFL (1), OS (1)
        let mut skip = [0u8; 6];
        inner.read_exact(&mut skip).map_err(TarError::Read)?;
        if flg & 0x04 != 0 {
            // FEXTRA: 2-byte LE length, then that many bytes.
            let mut len = [0u8; 2];
            inner.read_exact(&mut len).map_err(TarError::Read)?;
            let n = u16::from_le_bytes(len) as u64;
            io::copy(&mut inner.by_ref().take(n), &mut io::sink()).map_err(TarError::Read)?;
        }
        if flg & 0x08 != 0 {
            skip_cstring(&mut inner)?; // FNAME
        }
        if flg & 0x10 != 0 {
            skip_cstring(&mut inner)?; // FCOMMENT
        }
        if flg & 0x02 != 0 {
            // FHCRC: consumed, not verified (rarely present).
            let mut h = [0u8; 2];
            inner.read_exact(&mut h).map_err(TarError::Read)?;
        }
        Ok(GzipReader {
            inner,
            state: Inflate::new(false, 15),
            in_buf: Vec::new(),
            in_pos: 0,
            out_buf: Vec::new(),
            out_pos: 0,
            window: vec![0; CHUNK],
            crc: Crc32::new(),
            produced: 0,
            finished: false,
        })
    }

    /// Pull more compressed bytes into the window; `false` on EOF.
    fn refill_input(&mut self) -> io::Result<bool> {
        if self.in_pos > 0 {
            self.in_buf.drain(..self.in_pos);
            self.in_pos = 0;
        }
        let old = self.in_buf.len();
        self.in_buf.resize(old + CHUNK, 0);
        match self.inner.read(&mut self.in_buf[old..]) {
            Ok(0) => {
                self.in_buf.truncate(old);
                Ok(false)
            }
            Ok(n) => {
                self.in_buf.truncate(old + n);
                Ok(true)
            }
            Err(e) => {
                self.in_buf.truncate(old);
                Err(e)
            }
        }
    }

    /// After the deflate stream ends, read and verify the 8-byte trailer.
    fn validate_trailer(&mut self) -> io::Result<()> {
        let mut trailer = Vec::with_capacity(8);
        trailer.extend_from_slice(&self.in_buf[self.in_pos..]);
        self.in_pos = self.in_buf.len();
        while trailer.len() < 8 {
            let mut b = [0u8; 1];
            match self.inner.read(&mut b) {
                Ok(0) => break,
                Ok(_) => trailer.push(b[0]),
                Err(e) => return Err(e),
            }
        }
        if trailer.len() < 8 {
            return Err(gzip_fail("truncated gzip trailer"));
        }
        let crc = u32::from_le_bytes(trailer[0..4].try_into().unwrap());
        let isize = u32::from_le_bytes(trailer[4..8].try_into().unwrap());
        // `finish` consumes the hasher; swap in a fresh one so the trailer
        // can still be checked on a later round if this one errors.
        let actual = std::mem::replace(&mut self.crc, Crc32::new()).finish();
        if crc != actual {
            return Err(gzip_fail("CRC32 mismatch"));
        }
        if isize as u64 != self.produced {
            return Err(gzip_fail("size mismatch"));
        }
        self.finished = true;
        Ok(())
    }
}

impl<R: Read> Read for GzipReader<R> {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        if buf.is_empty() {
            return Ok(0);
        }
        loop {
            if self.out_pos < self.out_buf.len() {
                let n = (self.out_buf.len() - self.out_pos).min(buf.len());
                buf[..n].copy_from_slice(&self.out_buf[self.out_pos..self.out_pos + n]);
                self.out_pos += n;
                return Ok(n);
            }
            if self.finished {
                return Ok(0);
            }
            if self.in_pos >= self.in_buf.len() && !self.refill_input()? {
                return Err(gzip_fail("truncated deflate stream"));
            }
            let before_in = self.state.total_in();
            let before_out = self.state.total_out();
            let status = self
                .state
                .decompress(
                    &self.in_buf[self.in_pos..],
                    &mut self.window,
                    InflateFlush::NoFlush,
                )
                .map_err(|e| gzip_fail(format!("corrupt deflate stream: {e:?}")))?;
            let consumed = (self.state.total_in() - before_in) as usize;
            let written = (self.state.total_out() - before_out) as usize;
            self.in_pos += consumed;
            self.out_buf.clear();
            self.out_buf.extend_from_slice(&self.window[..written]);
            self.out_pos = 0;
            // The CRC/size track the *decompressed* stream, so they are
            // updated when the bytes are produced, not when they are
            // delivered — the trailer check below must see the final round's
            // output even if it is still buffered.
            self.crc.update(&self.out_buf);
            self.produced += written as u64;
            if matches!(status, Status::StreamEnd) {
                // Validate eagerly: a corrupt trailer must fail even if the
                // caller never reads again (the tar end-marker usually stops
                // iteration here).
                self.validate_trailer()?;
            }
        }
    }
}

fn read_u8<R: Read>(inner: &mut R) -> Result<u8, TarError> {
    let mut b = [0u8; 1];
    inner.read_exact(&mut b).map_err(TarError::Read)?;
    Ok(b[0])
}

fn skip_cstring<R: Read>(inner: &mut R) -> Result<(), TarError> {
    let mut b = [0u8; 1];
    loop {
        inner.read_exact(&mut b).map_err(TarError::Read)?;
        if b[0] == 0 {
            return Ok(());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;
    use std::io::Read;
    use std::path::PathBuf;

    // ---- test fixtures -------------------------------------------------

    /// Write an octal number into a fixed tar numeric field (right-aligned,
    /// NUL-terminated).
    fn write_octal(field: &mut [u8], value: u64) {
        let digits = field.len().saturating_sub(1);
        let s = format!("{:0width$o}", value, width = digits);
        let s = &s[s.len().saturating_sub(digits)..];
        field[..s.len()].copy_from_slice(s.as_bytes());
        field[s.len()] = 0;
    }

    /// Recompute the checksum after mutating a header built by
    /// `build_header` (e.g. overwriting the size field).
    fn reseal(h: &mut [u8; BLOCK_SIZE]) {
        for b in &mut h[148..156] {
            *b = b' ';
        }
        let sum: u32 = h.iter().map(|&b| b as u32).sum();
        let cs = format!("{:06o}\0", sum);
        h[148..155].copy_from_slice(cs.as_bytes());
    }

    fn build_header(name: &str, typeflag: u8, size: u64, linkname: &str) -> [u8; BLOCK_SIZE] {
        let mut h = [0u8; BLOCK_SIZE];
        let nb = name.as_bytes();
        h[..nb.len().min(100)].copy_from_slice(&nb[..nb.len().min(100)]);
        write_octal(&mut h[124..136], size);
        h[156] = typeflag;
        let lb = linkname.as_bytes();
        h[157..157 + lb.len().min(100)].copy_from_slice(&lb[..lb.len().min(100)]);
        h[257..262].copy_from_slice(b"ustar");
        h[264..266].copy_from_slice(b"00");
        for b in &mut h[148..156] {
            *b = b' ';
        }
        let sum: u32 = h.iter().map(|&b| b as u32).sum();
        // 6 octal digits + NUL (byte 155 stays a space, as real writers do).
        write_octal(&mut h[148..155], sum as u64);
        h
    }

    /// Build a tar from (name, typeflag, content) entries.
    fn make_tar(entries: &[(&str, u8, &[u8])]) -> Vec<u8> {
        let mut out = Vec::new();
        for (name, typeflag, content) in entries {
            let header = build_header(name, *typeflag, content.len() as u64, "");
            out.extend_from_slice(&header);
            out.extend_from_slice(content);
            let pad = (BLOCK_SIZE - (content.len() % BLOCK_SIZE)) % BLOCK_SIZE;
            out.extend(std::iter::repeat_n(0u8, pad));
        }
        out.extend_from_slice(&[0u8; BLOCK_SIZE * 2]);
        out
    }

    /// One pax record; the length prefix is a fixed point of the total
    /// record length.
    fn pax_record(key: &str, value: &str) -> Vec<u8> {
        let base = key.len() + value.len() + 3; // digits + ' ' + key + '=' + value + '\n'
        let mut n = base + 1;
        while n.to_string().len() + base != n {
            n = n.to_string().len() + base;
        }
        format!("{n} {key}={value}\n").into_bytes()
    }

    /// gzip-wrap raw bytes: header + raw deflate + CRC32/ISIZE trailer.
    fn gzip_wrap(raw: &[u8]) -> Vec<u8> {
        let mut out = vec![0x1f, 0x8b, 0x08, 0x00, 0, 0, 0, 0, 0x00, 0xff];
        out.extend_from_slice(&crate::testutil::deflate_raw(raw));
        out.extend_from_slice(&Crc32::compute(raw).to_le_bytes());
        out.extend_from_slice(&(raw.len() as u32).to_le_bytes());
        out
    }

    fn write_temp(dir: &TempDir, name: &str, data: &[u8]) -> PathBuf {
        let path = dir.path().join(name);
        std::fs::write(&path, data).unwrap();
        path
    }

    /// Drive the reader until a gzip semantic error surfaces (the exact
    /// call that trips it depends on inflate window boundaries).
    fn expect_gzip_error(mut archive: TarArchive) -> TarError {
        loop {
            match archive.next_entry() {
                Err(e) => return e,
                Ok(None) => panic!("expected a gzip error before end of archive"),
                Ok(Some(e)) => {
                    if let Err(e) = archive.copy_entry(&e, &mut Vec::new()) {
                        return e;
                    }
                }
            }
        }
    }

    // ---- tar tests ------------------------------------------------------

    #[test]
    fn reads_regular_files_and_dirs() {
        let dir = TempDir::new("tar-reads-files");
        let tar = make_tar(&[
            ("bin/", b'5', b""),
            ("bin/tool.exe", b'0', b"hello world"),
            ("docs/readme.txt", b'0', &[0u8; 600]),
        ]);
        let path = write_temp(&dir, "a.tar", &tar);
        let mut archive = TarArchive::open(&path).unwrap();

        let e = archive.next_entry().unwrap().unwrap();
        assert!(e.is_dir());
        assert_eq!(e.name, "bin/");

        let e = archive.next_entry().unwrap().unwrap();
        assert!(e.is_file());
        assert_eq!(e.name, "bin/tool.exe");
        assert_eq!(e.size, 11);
        let mut out = Vec::new();
        assert_eq!(archive.copy_entry(&e, &mut out).unwrap(), 11);
        assert_eq!(out, b"hello world");

        // Skipping an entry (no copy_entry) must not desync the reader.
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, "docs/readme.txt");
        assert_eq!(e.size, 600);
        let mut out = Vec::new();
        assert_eq!(archive.copy_entry(&e, &mut out).unwrap(), 600);
        assert_eq!(out.len(), 600);

        assert!(archive.next_entry().unwrap().is_none());
    }

    #[test]
    fn reads_gnu_longname() {
        let dir = TempDir::new("tar-gnu-longname");
        let long = "a/".to_string() + &"b".repeat(120) + "/tool.exe";
        let mut tar = Vec::new();
        // GNU 'L' metadata entry carries the long name for the next entry.
        let mut lpayload = long.clone().into_bytes();
        lpayload.push(0);
        let lheader = build_header("././@LongLink", b'L', lpayload.len() as u64, "");
        tar.extend_from_slice(&lheader);
        tar.extend_from_slice(&lpayload);
        let pad = (BLOCK_SIZE - (lpayload.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        tar.extend_from_slice(&make_tar(&[(long.as_str(), b'0', b"payload")]));
        let path = write_temp(&dir, "long.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, long);
        assert_eq!(e.size, 7);
        let mut out = Vec::new();
        archive.copy_entry(&e, &mut out).unwrap();
        assert_eq!(out, b"payload");
        assert!(archive.next_entry().unwrap().is_none());
    }

    #[test]
    fn reads_gnu_longlink() {
        let dir = TempDir::new("tar-gnu-longlink");
        let long = "l/".to_string() + &"t".repeat(120) + "/target.exe";
        let mut kpayload = long.clone().into_bytes();
        kpayload.push(0);
        let kheader = build_header("././@LongLink", b'K', kpayload.len() as u64, "");
        let mut tar = Vec::new();
        // GNU 'K' metadata entry carries the long linkname for the next
        // entry — the sibling of the 'L' longname entry.
        tar.extend_from_slice(&kheader);
        tar.extend_from_slice(&kpayload);
        let pad = (BLOCK_SIZE - (kpayload.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        // The real entry: a symlink whose header linkname is truncated to
        // the 100-byte field; the 'K' metadata carries the full target.
        tar.extend_from_slice(&build_header("alias.exe", b'2', 0, "truncated"));
        tar.extend_from_slice(&[0u8; BLOCK_SIZE * 2]);
        let path = write_temp(&dir, "longlink.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.typeflag, b'2');
        assert_eq!(e.name, "alias.exe");
        assert_eq!(e.linkname, long);
        assert!(archive.next_entry().unwrap().is_none());
    }

    #[test]
    fn reads_pax_path_and_size_override() {
        let dir = TempDir::new("tar-pax");
        let long = "p/".to_string() + &"x".repeat(150) + "/tool.exe";
        let mut record = pax_record("path", &long);
        record.extend_from_slice(&pax_record("size", "11"));
        let mut tar = Vec::new();
        tar.extend_from_slice(&build_header("PaxHeaders/0", b'x', record.len() as u64, ""));
        tar.extend_from_slice(&record);
        let pad = (BLOCK_SIZE - (record.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        // Real entry: header claims size 0; the pax size record overrides it.
        tar.extend_from_slice(&build_header("tool.exe", b'0', 0, ""));
        let payload = b"pax payload";
        tar.extend_from_slice(payload);
        let pad = (BLOCK_SIZE - (payload.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        // A second pax header overrides the *link* target of the next
        // entry (a symlink whose header linkname is truncated to the
        // 100-byte field).
        let long_link = "l/".to_string() + &"y".repeat(120) + "/target.exe";
        let record = pax_record("linkpath", &long_link);
        tar.extend_from_slice(&build_header("PaxHeaders/1", b'x', record.len() as u64, ""));
        tar.extend_from_slice(&record);
        let pad = (BLOCK_SIZE - (record.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        tar.extend_from_slice(&build_header("alias.exe", b'2', 0, "truncated"));
        tar.extend_from_slice(&[0u8; BLOCK_SIZE * 2]);
        let path = write_temp(&dir, "pax.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, long);
        assert_eq!(e.size, 11, "pax size record must override header size");
        let mut out = Vec::new();
        archive.copy_entry(&e, &mut out).unwrap();
        assert_eq!(out, b"pax payload");

        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, "alias.exe");
        assert_eq!(e.typeflag, b'2');
        assert_eq!(
            e.linkname, long_link,
            "pax linkpath must override the header linkname"
        );
        assert!(archive.next_entry().unwrap().is_none());
    }

    #[test]
    fn parses_base256_size() {
        let dir = TempDir::new("tar-base256");
        let mut h = build_header("big.bin", b'0', 0, "");
        // GNU base-256: the field's high bit marks the encoding, the rest
        // is big-endian. Writers switch to it for sizes that do not fit
        // the 11 octal digits (≥ 8 GiB).
        h[124..136].fill(0);
        h[124] = 0x80;
        h[135] = 5;
        // Mutating the size field invalidates the checksum: reseal.
        reseal(&mut h);
        let mut tar = Vec::new();
        tar.extend_from_slice(&h);
        tar.extend_from_slice(b"abcde");
        let pad = (BLOCK_SIZE - 5 % BLOCK_SIZE) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        tar.extend_from_slice(&[0u8; BLOCK_SIZE * 2]);
        let path = write_temp(&dir, "base256.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.size, 5, "base-256 size field must parse");
        let mut out = Vec::new();
        assert_eq!(archive.copy_entry(&e, &mut out).unwrap(), 5);
        assert_eq!(out, b"abcde");
    }

    #[test]
    fn rejects_overflowing_base256_size() {
        let dir = TempDir::new("tar-base256-overflow");
        let mut h = build_header("huge.bin", b'0', 0, "");
        // A 12-byte base-256 field can express values beyond u64; wrapping
        // silently would desync the reader, so it must be rejected.
        h[124..136].fill(0xff);
        reseal(&mut h);
        let mut tar = Vec::new();
        tar.extend_from_slice(&h);
        tar.extend_from_slice(&[0u8; BLOCK_SIZE * 2]);
        let path = write_temp(&dir, "overflow.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let err = archive.next_entry().unwrap_err();
        assert!(matches!(err, TarError::BadNumeric { .. }), "{err}");
    }

    #[test]
    fn pax_global_header_applies_to_all_later_entries() {
        let dir = TempDir::new("tar-pax-global");
        let mut tar = Vec::new();
        // A global `path` record is the default for every subsequent entry…
        let record = pax_record("path", "global.txt");
        tar.extend_from_slice(&build_header("PaxHeader/g", b'g', record.len() as u64, ""));
        tar.extend_from_slice(&record);
        let pad = (BLOCK_SIZE - (record.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        // …until a per-entry `x` header overrides it for one entry.
        let local = pax_record("path", "local.txt");
        tar.extend_from_slice(&build_header("PaxHeader/x", b'x', local.len() as u64, ""));
        tar.extend_from_slice(&local);
        let pad = (BLOCK_SIZE - (local.len() % BLOCK_SIZE)) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        tar.extend_from_slice(&make_tar(&[("a.bin", b'0', b"x"), ("b.bin", b'0', b"y")]));
        let path = write_temp(&dir, "global.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, "local.txt", "per-entry pax must win");
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, "global.txt", "the global default must persist");
    }

    #[test]
    fn accepts_signed_checksum() {
        let dir = TempDir::new("tar-signed-sum");
        // A name carrying high-bit bytes ("tööl.exe" → 0xC3 0xB6 ...) makes
        // the signed checksum differ from the unsigned one. Old tar writers
        // stored the signed sum; `verify_checksum` must still accept it.
        let mut h = build_header("tööl.exe", b'0', 4, "");
        for b in &mut h[148..156] {
            *b = b' ';
        }
        let mut signed: i64 = 0;
        for (i, &b) in h.iter().enumerate() {
            let b = if (148..156).contains(&i) { b' ' } else { b };
            signed += b as i8 as i64;
        }
        assert!(signed > 0, "fixture must keep the signed sum positive");
        write_octal(&mut h[148..155], signed as u64);
        let mut tar = Vec::new();
        tar.extend_from_slice(&h);
        tar.extend_from_slice(b"data");
        let pad = (BLOCK_SIZE - 4 % BLOCK_SIZE) % BLOCK_SIZE;
        tar.extend(std::iter::repeat_n(0u8, pad));
        tar.extend_from_slice(&[0u8; BLOCK_SIZE * 2]);
        let path = write_temp(&dir, "signed.tar", &tar);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, "tööl.exe");
        assert_eq!(e.size, 4);
        let mut out = Vec::new();
        archive.copy_entry(&e, &mut out).unwrap();
        assert_eq!(out, b"data");
    }

    #[test]
    fn rejects_bad_checksum() {
        let dir = TempDir::new("tar-bad-checksum");
        let mut tar = make_tar(&[("tool.exe", b'0', b"data")]);
        tar[148] ^= 0x01; // flip a checksum digit
        let path = write_temp(&dir, "bad.tar", &tar);
        let mut archive = TarArchive::open(&path).unwrap();
        let err = archive.next_entry().unwrap_err();
        assert!(matches!(err, TarError::BadChecksum { .. }), "{err}");
    }

    #[test]
    fn rejects_truncated_header() {
        let dir = TempDir::new("tar-truncated-header");
        let mut tar = make_tar(&[("tool.exe", b'0', b"data")]);
        tar.truncate(300);
        let path = write_temp(&dir, "trunc.tar", &tar);
        let mut archive = TarArchive::open(&path).unwrap();
        let err = archive.next_entry().unwrap_err();
        assert!(matches!(err, TarError::TruncatedHeader { .. }), "{err}");
    }

    #[test]
    fn rejects_truncated_payload() {
        let dir = TempDir::new("tar-trunc-payload");
        let mut tar = make_tar(&[("tool.exe", b'0', b"data")]);
        tar.truncate(BLOCK_SIZE + 2);
        let path = write_temp(&dir, "trunc2.tar", &tar);
        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        let err = archive.copy_entry(&e, &mut Vec::new()).unwrap_err();
        assert!(matches!(err, TarError::TruncatedPayload { .. }), "{err}");
    }

    #[test]
    fn hardlink_entry_surfaces_link_target() {
        let dir = TempDir::new("tar-hardlink-surface");
        // The reader is format-agnostic; extraction policy (which member
        // types are acceptable) lives in the extractor. A '1' entry must
        // at least surface with its link target so the extractor can
        // resolve (or reject) it.
        let mut tar = Vec::new();
        tar.extend_from_slice(&build_header("hard/link", b'1', 0, "real/file"));
        tar.extend_from_slice(&make_tar(&[("real/file", b'0', b"x")]));
        let path = write_temp(&dir, "hardlink.tar", &tar);
        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.typeflag, b'1');
        assert_eq!(e.linkname, "real/file");
    }

    #[test]
    fn accepts_archive_without_trailing_blocks() {
        let dir = TempDir::new("tar-no-terminator");
        let mut tar = make_tar(&[("a.txt", b'0', b"x")]);
        tar.truncate(BLOCK_SIZE + BLOCK_SIZE); // drop the zero blocks
        let path = write_temp(&dir, "noterm.tar", &tar);
        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        archive.copy_entry(&e, &mut Vec::new()).unwrap();
        assert!(archive.next_entry().unwrap().is_none());
        // Repeated calls after the end marker stay at None.
        assert!(archive.next_entry().unwrap().is_none());
    }

    // ---- gzip tests -----------------------------------------------------

    #[test]
    fn gzip_roundtrip() {
        let dir = TempDir::new("gzip-roundtrip");
        // A member larger than the 64 KiB inflate window — and poorly
        // compressible, so the deflate input also spans several refills —
        // exercises the streaming continuation loop, not just the
        // single-shot StreamEnd fast path.
        let payload: Vec<u8> = (0..200_000u32)
            .map(|i| i.wrapping_mul(2_654_435_761).to_le_bytes()[0])
            .collect();
        let tar = make_tar(&[("bin/big.bin", b'0', &payload)]);
        let gz = gzip_wrap(&tar);
        let path = write_temp(&dir, "a.tar.gz", &gz);

        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        assert_eq!(e.name, "bin/big.bin");
        assert_eq!(e.size, payload.len() as u64);
        let mut out = Vec::new();
        assert_eq!(
            archive.copy_entry(&e, &mut out).unwrap(),
            payload.len() as u64
        );
        assert_eq!(out, payload);
        assert!(archive.next_entry().unwrap().is_none());
    }

    #[test]
    fn gzip_skips_fextra_and_fname_fields() {
        let dir = TempDir::new("gzip-fname");
        let payload = b"payload with fname header";
        let deflated = crate::testutil::deflate_raw(payload);
        // FLG = FEXTRA|FNAME: an XLEN-prefixed extra field must be skipped
        // before the NUL-terminated original filename.
        let extra: &[u8] = b"BC\x02\x00ab"; // subfield id "BC", len 2, data "ab"
        let mut gz = vec![0x1f, 0x8b, 0x08, 0x0c, 0, 0, 0, 0, 0x00, 0xff];
        gz.extend_from_slice(&(extra.len() as u16).to_le_bytes());
        gz.extend_from_slice(extra);
        gz.extend_from_slice(b"original-name.tar\0"); // FNAME
        gz.extend_from_slice(&deflated);
        gz.extend_from_slice(&Crc32::compute(payload).to_le_bytes());
        gz.extend_from_slice(&(payload.len() as u32).to_le_bytes());
        let path = write_temp(&dir, "named.gz", &gz);

        let mut reader =
            GzipReader::new(BufReader::new(std::fs::File::open(&path).unwrap())).unwrap();
        let mut out = Vec::new();
        reader.read_to_end(&mut out).unwrap();
        assert_eq!(out, payload);
    }

    #[test]
    fn gzip_rejects_crc_mismatch() {
        let dir = TempDir::new("gzip-crc");
        let mut gz = gzip_wrap(&make_tar(&[("tool.exe", b'0', b"payload")]));
        let last = gz.len() - 8;
        gz[last] ^= 0xff; // corrupt the stored CRC
        let path = write_temp(&dir, "bad.tar.gz", &gz);
        let err = expect_gzip_error(TarArchive::open(&path).unwrap());
        assert!(
            matches!(err, TarError::Gzip(ref m) if m.contains("CRC")),
            "{err}"
        );
    }

    #[test]
    fn gzip_rejects_size_mismatch() {
        let dir = TempDir::new("gzip-size");
        let mut gz = gzip_wrap(&make_tar(&[("tool.exe", b'0', b"payload")]));
        let last = gz.len() - 4;
        gz[last] ^= 0xff; // corrupt ISIZE
        let path = write_temp(&dir, "badsize.tar.gz", &gz);
        let err = expect_gzip_error(TarArchive::open(&path).unwrap());
        assert!(
            matches!(err, TarError::Gzip(ref m) if m.contains("size")),
            "{err}"
        );
    }

    #[test]
    fn gzip_rejects_bad_method() {
        let dir = TempDir::new("gzip-method");
        let mut gz = gzip_wrap(b"payload");
        gz[2] = 9; // CM != 8
        let path = write_temp(&dir, "method.tar.gz", &gz);
        let err = match TarArchive::open(&path) {
            Err(e) => e,
            Ok(_) => panic!("expected a gzip header error"),
        };
        assert!(
            matches!(err, TarError::Gzip(ref m) if m.contains("compression method")),
            "{err}"
        );
    }

    #[test]
    fn gzip_rejects_truncated_stream() {
        let dir = TempDir::new("gzip-truncated");
        let gz = gzip_wrap(b"payload that will be cut");
        let path = write_temp(&dir, "trunc.tar.gz", &gz[..gz.len() - 12]);
        let err = expect_gzip_error(TarArchive::open(&path).unwrap());
        assert!(matches!(err, TarError::Gzip(_)), "{err}");
    }

    #[test]
    fn plain_tar_sniffed_inside_mislabelled_gz_name() {
        let dir = TempDir::new("tar-sniff");
        let tar = make_tar(&[("tool.exe", b'0', b"plain")]);
        let path = write_temp(&dir, "actually-plain.tar.gz", &tar);
        let mut archive = TarArchive::open(&path).unwrap();
        let e = archive.next_entry().unwrap().unwrap();
        let mut out = Vec::new();
        archive.copy_entry(&e, &mut out).unwrap();
        assert_eq!(out, b"plain");
    }
}
