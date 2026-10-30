//! Corpus scan: parse every `*.json` under the local Scoop buckets directory.
//!
//! lint configs etc. — are skipped, not fatal). Skipped automatically when no
//! corpus exists on this machine; override the location with `GLUE_CORPUS_DIR`.

use gluestick_core::manifest::parse_str;
use std::path::{Path, PathBuf};

fn corpus_dir() -> Option<PathBuf> {
    let dir = std::env::var_os("GLUE_CORPUS_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| gluestick_core::paths::default_data_root().join("buckets"));
    dir.is_dir().then_some(dir)
}

fn collect_json_files(dir: &Path, out: &mut Vec<PathBuf>) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let Ok(meta) = std::fs::metadata(&path) else {
            continue;
        };
        if meta.is_dir() {
            // Skip git internals if a bucket ever inlines one.
            if entry.file_name() == ".git" {
                continue;
            }
            collect_json_files(&path, out);
        } else if path.extension().and_then(|e| e.to_str()) == Some("json") {
            out.push(path);
        }
    }
}

#[test]
fn corpus_all_bucket_manifests_parse() {
    let Some(dir) = corpus_dir() else {
        eprintln!("corpus not found — skipping (set GLUE_CORPUS_DIR to enable)");
        return;
    };

    let mut files = Vec::new();
    collect_json_files(&dir, &mut files);
    assert!(
        !files.is_empty(),
        "corpus at {} has no JSON files",
        dir.display()
    );

    let mut ok = 0usize;
    let mut failed = Vec::new();
    for file in &files {
        let Ok(text) = std::fs::read_to_string(file) else {
            failed.push((file.clone(), "unreadable".to_string()));
            continue;
        };
        match parse_str(&text) {
            Ok(_) => ok += 1,
            Err(e) => failed.push((file.clone(), e.to_string())),
        }
    }

    let total = files.len();
    let rate = ok as f64 / total as f64;
    eprintln!("corpus: parsed {ok}/{total} ({:.2}%)", rate * 100.0);
    for (path, err) in failed.iter().take(10) {
        eprintln!("  failed: {} — {err}", path.display());
    }

    // A healthy bucket corpus is overwhelmingly manifests; lint configs and
    // other stray JSON account for the remainder.
    assert!(total >= 1000, "corpus unexpectedly small: {total} files");
    assert!(
        rate >= 0.95,
        "parse rate too low: {ok}/{total} ({:.2}%)",
        rate * 100.0
    );
}
