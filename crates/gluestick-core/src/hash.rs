//! SHA-256 helpers (.go`: HashBytes / HashFile).
//!
//! # Why not just `sha2`
//!
//! `sha2` 0.10 is pure Rust: its hardware SHA path needs the `asm` feature,
//! which pulls in `sha2-asm` and therefore **nasm** at build time. Measured on
//! a 150 MiB blob it runs at ~184 MB/s here, while the OS implementation
//! (OpenSSL/CNG, both SHA-NI) does ~390 MB/s — and a large artifact is hashed
//! at least twice per install (once while ingesting into the store, once when
//! the install dir is scanned), so this is the largest CPU cost of an install.
//!
//! Windows CNG (`bcrypt.dll`) already dispatches to SHA-NI, needs no new crate
//! (`windows-sys` is a dependency anyway), and adds no binary size beyond the
//! import. It is used when available; `sha2` stays as the portable fallback.
//!
//! # Failure policy
//!
//! CNG calls with a valid handle and a valid buffer do not fail, and a silent
//! degradation would corrupt a content-addressed store, so the backend is
//! self-checked once per process against the SHA-256("abc") vector. If that
//! check fails — or the provider cannot be opened — every later hash in the
//! process uses the portable path. Per-call `NTSTATUS` values are not
//! propagated because a mid-stream failure cannot be recovered anyway; the
//! self-check is what makes that tradeoff safe.

use std::io::Read;
use std::path::Path;

use sha2::{Digest, Sha256 as SoftSha256};

/// SHA-256("abc") — the vector the CNG backend is validated against.
const ABC_DIGEST: &str = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";

/// Streaming SHA-256 that prefers the platform's accelerated implementation.
pub struct Sha256 {
    backend: Backend,
}

enum Backend {
    #[cfg(windows)]
    Cng(CngSha256),
    Soft(SoftSha256),
}

impl Sha256 {
    pub fn new() -> Self {
        #[cfg(windows)]
        if cng_usable() {
            if let Some(cng) = CngSha256::new() {
                return Sha256 {
                    backend: Backend::Cng(cng),
                };
            }
        }
        Sha256 {
            backend: Backend::Soft(SoftSha256::new()),
        }
    }

    pub fn update(&mut self, data: &[u8]) {
        match &mut self.backend {
            #[cfg(windows)]
            Backend::Cng(cng) => cng.update(data),
            Backend::Soft(h) => h.update(data),
        }
    }

    pub fn finish(self) -> [u8; 32] {
        match self.backend {
            #[cfg(windows)]
            Backend::Cng(cng) => cng.finish(),
            Backend::Soft(h) => h.finalize().into(),
        }
    }
}

impl Default for Sha256 {
    fn default() -> Self {
        Self::new()
    }
}

/// `std::io::Write` so `std::io::copy(&mut reader, &mut hasher)` works
/// (`Store::hash_reader` uses it).
impl std::io::Write for Sha256 {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.update(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Lowercase hex SHA-256 digest of `data`.
pub fn hash_bytes(data: &[u8]) -> String {
    let mut h = Sha256::new();
    h.update(data);
    hex_encode(&h.finish())
}

/// Lowercase hex SHA-256 digest of the file at `path`.
///
/// Unlike the version (which reads the whole file), this streams in 128 KiB
/// chunks; the resulting digest is identical.
pub fn hash_file(path: &Path) -> std::io::Result<String> {
    let mut file = std::fs::File::open(path)?;
    let mut hasher = Sha256::new();
    let mut buf = [0u8; 128 * 1024];
    loop {
        let n = file.read(&mut buf)?;
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    Ok(hex_encode(&hasher.finish()))
}

pub(crate) fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        out.push(HEX[(b >> 4) as usize] as char);
        out.push(HEX[(b & 0x0f) as usize] as char);
    }
    out
}

// ---------------------------------------------------------------------------
// Windows CNG backend
// ---------------------------------------------------------------------------

#[cfg(windows)]
mod cng {
    use std::sync::atomic::{AtomicU8, Ordering};
    use std::sync::OnceLock;
    use windows_sys::Win32::Security::Cryptography::{
        BCryptCreateHash, BCryptDestroyHash, BCryptFinishHash, BCryptGetProperty, BCryptHashData,
        BCryptOpenAlgorithmProvider, BCRYPT_ALG_HANDLE, BCRYPT_HASH_HANDLE, BCRYPT_OBJECT_LENGTH,
        BCRYPT_SHA256_ALGORITHM,
    };

    /// 0 = not checked, 1 = usable, 2 = unusable.
    static USABLE: AtomicU8 = AtomicU8::new(0);

    /// Process-lifetime provider handle (never closed; `BCrypt*` provider
    /// handles are cheap and intended to be reused).
    fn alg_handle() -> Option<BCRYPT_ALG_HANDLE> {
        static ALG: OnceLock<usize> = OnceLock::new();
        let raw = *ALG.get_or_init(|| {
            let mut handle: BCRYPT_ALG_HANDLE = std::ptr::null_mut();
            // SAFETY: SHA-256 is a built-in algorithm; `null` selects the
            // default provider. The handle outlives every use below.
            let status = unsafe {
                BCryptOpenAlgorithmProvider(
                    &mut handle,
                    BCRYPT_SHA256_ALGORITHM,
                    std::ptr::null(),
                    0,
                )
            };
            if status < 0 || handle.is_null() {
                0
            } else {
                handle as usize
            }
        });
        (raw != 0).then_some(raw as BCRYPT_ALG_HANDLE)
    }

    /// Size of the caller-supplied hash object buffer (`BCRYPT_OBJECT_LENGTH`).
    fn object_length(alg: BCRYPT_ALG_HANDLE) -> Option<u32> {
        let mut len: u32 = 0;
        let mut written: u32 = 0;
        // SAFETY: the property is a u32 and the buffer is 4 bytes.
        let status = unsafe {
            BCryptGetProperty(
                alg,
                BCRYPT_OBJECT_LENGTH,
                (&mut len as *mut u32).cast::<u8>(),
                4,
                &mut written,
                0,
            )
        };
        (status >= 0 && written == 4 && len > 0).then_some(len)
    }

    /// Validate the backend once per process against a known vector.
    pub fn usable() -> bool {
        match USABLE.load(Ordering::Relaxed) {
            1 => return true,
            2 => return false,
            _ => {}
        }
        let ok = match CngSha256::new() {
            Some(mut h) => {
                h.update(b"abc");
                super::hex_encode(&h.finish()) == super::ABC_DIGEST
            }
            None => false,
        };
        USABLE.store(if ok { 1 } else { 2 }, Ordering::Relaxed);
        ok
    }

    pub struct CngSha256 {
        handle: BCRYPT_HASH_HANDLE,
        /// The hash object buffer must outlive the handle.
        _object: Vec<u8>,
    }

    impl CngSha256 {
        pub fn new() -> Option<Self> {
            let alg = alg_handle()?;
            let len = object_length(alg)?;
            let mut object = vec![0u8; len as usize];
            let mut handle: BCRYPT_HASH_HANDLE = std::ptr::null_mut();
            // SAFETY: `object` is `len` bytes and stays alive in the struct;
            // no secret is supplied.
            let status = unsafe {
                BCryptCreateHash(
                    alg,
                    &mut handle,
                    object.as_mut_ptr(),
                    len,
                    std::ptr::null(),
                    0,
                    0,
                )
            };
            if status < 0 || handle.is_null() {
                return None;
            }
            Some(CngSha256 {
                handle,
                _object: object,
            })
        }

        pub fn update(&mut self, data: &[u8]) {
            if data.is_empty() {
                return;
            }
            // SAFETY: the handle is live until `finish`/`drop`, and the slice
            // is valid for `data.len()` bytes.
            unsafe {
                BCryptHashData(self.handle, data.as_ptr(), data.len() as u32, 0);
            }
        }

        pub fn finish(mut self) -> [u8; 32] {
            let mut out = [0u8; 32];
            // SAFETY: 32 bytes is SHA-256's digest size for this handle.
            unsafe {
                BCryptFinishHash(self.handle, out.as_mut_ptr(), 32, 0);
            }
            self.close();
            out
        }

        fn close(&mut self) {
            if !self.handle.is_null() {
                // SAFETY: destroys our own handle exactly once.
                unsafe {
                    BCryptDestroyHash(self.handle);
                }
                self.handle = std::ptr::null_mut();
            }
        }
    }

    impl Drop for CngSha256 {
        fn drop(&mut self) {
            self.close();
        }
    }
}

#[cfg(windows)]
use cng::CngSha256;

#[cfg(windows)]
fn cng_usable() -> bool {
    cng::usable()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hash_bytes_known_vector() {
        // SHA-256("abc")
        assert_eq!(hash_bytes(b"abc"), ABC_DIGEST);
    }

    #[test]
    fn streaming_matches_one_shot() {
        let data: Vec<u8> = (0..200_000u32).map(|i| (i % 251) as u8).collect();
        let mut h = Sha256::new();
        for chunk in data.chunks(4096) {
            h.update(chunk);
        }
        assert_eq!(hex_encode(&h.finish()), hash_bytes(&data));
    }

    #[test]
    fn hash_file_matches_hash_bytes() {
        let path = std::env::temp_dir().join(format!("glue-hash-test-{}", std::process::id()));
        std::fs::write(&path, b"abc").unwrap();
        let file_hash = hash_file(&path).unwrap();
        std::fs::remove_file(&path).ok();
        assert_eq!(file_hash, hash_bytes(b"abc"));
    }

    /// Empty input is the shape a streaming backend can get wrong (no
    /// `update` call ever happens).
    #[test]
    fn empty_input_matches_spec() {
        assert_eq!(
            hash_bytes(b""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
    }

    /// The CNG path must agree with the portable one on multi-block input,
    /// including a chunk that straddles the 64-byte block boundary.
    #[test]
    fn cng_matches_soft_across_block_boundaries() {
        let data: Vec<u8> = (0..5000u32).map(|i| (i * 7 % 256) as u8).collect();
        let soft = SoftSha256::digest(&data);
        for chunk in [1usize, 63, 64, 65, 127, 128, 129, 4096] {
            let mut h = Sha256::new();
            for part in data.chunks(chunk) {
                h.update(part);
            }
            assert_eq!(h.finish().to_vec(), soft.to_vec(), "chunk size {chunk}");
        }
    }
}
