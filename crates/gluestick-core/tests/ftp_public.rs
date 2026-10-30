//! Public FTP smoke test (`ftp://speedtest.tele2.net/1MB.zip`). Ignored by
//! default because FTP egress is commonly blocked behind transparent proxies /
//! VPN fake-IP; when unreachable the test skips with a diagnostic.
//!
//! Run explicitly: `cargo test -p core --test ftp_public -- --ignored --nocapture`

use gluestick_core::downloader::{Downloader, Task};
use gluestick_core::store::Store;
use std::path::PathBuf;

/// Minimal RAII scratch dir for this integration test (the crate-internal
/// `testutil::TempDir` is not visible to integration tests). Unique per process
/// and removed on drop, so a skip path needs no manual cleanup.
struct ScratchDir(PathBuf);

impl ScratchDir {
    fn new(tag: &str) -> Self {
        let path = std::env::temp_dir().join(format!("glue-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&path);
        ScratchDir(path)
    }
}

impl Drop for ScratchDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

#[test]
#[ignore = "needs public FTP egress; skipped when egress is blocked"]
fn public_ftp_download() {
    let dir = ScratchDir::new("ftp-public-test");
    let dl = Downloader::new(Store::new(&dir.0).unwrap());
    let task = Task {
        url: "ftp://speedtest.tele2.net/1MB.zip".into(),
        filename: "1MB.zip".into(),
        ..Default::default()
    };
    let res = dl.download(&task, None);
    if let Some(e) = &res.error {
        eprintln!("public FTP unreachable (FTP may be blocked on this network): {e}");
        return; // skip when the network blocks FTP
    }
    assert_eq!(res.size, 1 << 20);
    assert!(!res.hash.is_empty());
}
