//! Persistent backing for the catalog/search index (`catalog_entries` /
//! `catalog_buckets` tables in `cache/index.db`).
//!
//! Parsing every manifest of every bucket costs ~3 s on a 4,928-manifest
//! catalog, and it was paid *per process* — every `glue search`, `glue list`
//! (browse), `glue info` on a bare name. The rows only change when a bucket is
//! updated, so they are stored next to the content cache and revalidated with a
//! fingerprint ([`bucket_fingerprint`]) that costs one directory walk
//! (~0.2 s) instead of one parse per file.
//!
//! The tables are deliberately *not* part of the content cache: `cache clear`,
//! `cache rebuild` and `cache gc` never touch them, and losing them only costs
//! time. There is no FK to `packages` for the same reason.

use std::sync::{Arc, Mutex};

use rusqlite::{params, Connection, OptionalExtension};

use crate::search_index::{Entry, EntryCache};

/// SQLite-backed [`EntryCache`].
#[derive(Clone)]
pub struct CatalogCache {
    db: Arc<Mutex<Connection>>,
}

impl CatalogCache {
    pub(crate) fn new(db: Arc<Mutex<Connection>>) -> CatalogCache {
        CatalogCache { db }
    }

    /// Number of cached rows for one bucket (diagnostics/tests).
    pub fn count(&self, bucket: &str) -> usize {
        let db = self.db.lock().unwrap();
        db.query_row(
            "SELECT COUNT(*) FROM catalog_entries WHERE bucket = ?",
            params![bucket],
            |row| row.get::<_, i64>(0),
        )
        .unwrap_or(0) as usize
    }

    /// Fingerprint currently recorded for a bucket, if any.
    pub fn stamp(&self, bucket: &str) -> Option<String> {
        let db = self.db.lock().unwrap();
        db.query_row(
            "SELECT stamp FROM catalog_buckets WHERE bucket = ?",
            params![bucket],
            |row| row.get(0),
        )
        .optional()
        .ok()
        .flatten()
    }
}

impl EntryCache for CatalogCache {
    fn load(&self, bucket: &str, stamp: &str) -> Option<Vec<Entry>> {
        let db = self.db.lock().unwrap();
        let stored: Option<String> = db
            .query_row(
                "SELECT stamp FROM catalog_buckets WHERE bucket = ?",
                params![bucket],
                |row| row.get(0),
            )
            .optional()
            .ok()?;
        if stored.as_deref() != Some(stamp) {
            return None;
        }
        let mut stmt = db
            .prepare(
                "SELECT name, description, version, homepage, archived, deprecated
                 FROM catalog_entries WHERE bucket = ? ORDER BY name",
            )
            .ok()?;
        let rows = stmt
            .query_map(params![bucket], |row| {
                Ok(Entry {
                    name: row.get(0)?,
                    bucket: bucket.to_string(),
                    description: row.get(1)?,
                    version: row.get(2)?,
                    homepage: row.get(3)?,
                    archived: row.get::<_, i64>(4)? != 0,
                    deprecated: row.get::<_, i64>(5)? != 0,
                })
            })
            .ok()?;
        Some(rows.flatten().collect())
    }

    fn store(&self, bucket: &str, stamp: &str, entries: &[Entry]) {
        // Best effort: a cache write failure only costs a re-parse next run.
        let mut db = self.db.lock().unwrap();
        let Ok(tx) = db.transaction() else {
            return;
        };
        if tx
            .execute(
                "DELETE FROM catalog_entries WHERE bucket = ?",
                params![bucket],
            )
            .is_err()
        {
            return;
        }
        {
            let Ok(mut stmt) = tx.prepare(
                "INSERT OR REPLACE INTO catalog_entries
                 (bucket, name, description, version, homepage, archived, deprecated)
                 VALUES (?, ?, ?, ?, ?, ?, ?)",
            ) else {
                return;
            };
            for e in entries {
                let _ = stmt.execute(params![
                    bucket,
                    e.name,
                    e.description,
                    e.version,
                    e.homepage,
                    i64::from(e.archived),
                    i64::from(e.deprecated),
                ]);
            }
        }
        let _ = tx.execute(
            "INSERT OR REPLACE INTO catalog_buckets (bucket, stamp, updated)
             VALUES (?, ?, ?)",
            params![bucket, stamp, crate::cache::index::rfc3339_now()],
        );
        let _ = tx.commit();
    }

    fn forget(&self, bucket: &str) {
        let mut db = self.db.lock().unwrap();
        let Ok(tx) = db.transaction() else {
            return;
        };
        let _ = tx.execute(
            "DELETE FROM catalog_entries WHERE bucket = ?",
            params![bucket],
        );
        let _ = tx.execute(
            "DELETE FROM catalog_buckets WHERE bucket = ?",
            params![bucket],
        );
        let _ = tx.commit();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cache::Index as CacheIndex;
    use crate::search_index::bucket_fingerprint;
    use crate::testutil::TempDir;
    use std::path::Path;

    fn stamp(root: &Path) -> String {
        bucket_fingerprint(&root.join("main"), "main")
    }

    fn entry(bucket: &str, name: &str) -> Entry {
        Entry {
            name: name.to_string(),
            bucket: bucket.to_string(),
            description: format!("{name} package"),
            version: "1.0.0".to_string(),
            homepage: String::new(),
            archived: false,
            deprecated: false,
        }
    }

    fn cache(root: &TempDir) -> CatalogCache {
        CacheIndex::new(root.path()).unwrap().catalog_cache()
    }

    #[test]
    fn store_then_load_roundtrips_all_fields() {
        let root = TempDir::new("glue-catcache-roundtrip");
        let c = cache(&root);
        let mut e = entry("main", "git");
        e.archived = true;
        e.deprecated = true;
        e.homepage = "https://git-scm.com".to_string();
        c.store("main", "stamp-1", &[e.clone()]);

        let loaded = c.load("main", "stamp-1").expect("hit");
        assert_eq!(loaded, vec![e]);
        assert_eq!(c.count("main"), 1);
        assert_eq!(c.stamp("main").as_deref(), Some("stamp-1"));
    }

    #[test]
    fn a_different_stamp_is_a_miss() {
        let root = TempDir::new("glue-catcache-stamp");
        let c = cache(&root);
        c.store("main", "stamp-1", &[entry("main", "git")]);
        assert!(c.load("main", "stamp-2").is_none());
        // The stale row is kept until a store replaces it.
        assert_eq!(c.count("main"), 1);
    }

    #[test]
    fn store_replaces_the_previous_bucket_contents() {
        let root = TempDir::new("glue-catcache-replace");
        let c = cache(&root);
        c.store("main", "s1", &[entry("main", "git"), entry("main", "vim")]);
        c.store("main", "s2", &[entry("main", "git")]);
        let loaded = c.load("main", "s2").expect("hit");
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].name, "git");
    }

    #[test]
    fn buckets_are_independent() {
        let root = TempDir::new("glue-catcache-buckets");
        let c = cache(&root);
        c.store("main", "s1", &[entry("main", "git")]);
        c.store("extras", "s1", &[entry("extras", "zotero")]);
        assert_eq!(c.load("main", "s1").unwrap()[0].name, "git");
        assert_eq!(c.load("extras", "s1").unwrap()[0].name, "zotero");
        c.forget("main");
        assert!(c.load("main", "s1").is_none());
        assert_eq!(c.count("main"), 0);
        assert_eq!(c.load("extras", "s1").unwrap().len(), 1);
    }

    #[test]
    fn catalog_tables_survive_content_cache_clear() {
        // `cache clear` / `rebuild` must not disturb the catalog cache: they
        // only touch packages/package_files.
        let root = TempDir::new("glue-catcache-clear");
        let idx = CacheIndex::new(root.path()).unwrap();
        let c = idx.catalog_cache();
        c.store("main", "s1", &[entry("main", "git")]);

        idx.remove("git").unwrap();
        assert_eq!(c.load("main", "s1").unwrap().len(), 1);
    }

    #[test]
    fn fingerprint_changes_with_content_and_set() {
        let root = TempDir::new("glue-catcache-fp");
        let dir = root.path().join("main").join("bucket");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("git.json"), br#"{"version":"1"}"#).unwrap();
        let first = stamp(root.path());

        // Rewriting with a different size changes the fingerprint.
        std::fs::write(dir.join("git.json"), br#"{"version":"2.0"}"#).unwrap();
        let second = stamp(root.path());
        assert_ne!(first, second);

        // Adding a manifest changes it too.
        std::fs::write(dir.join("vim.json"), br#"{"version":"9"}"#).unwrap();
        let third = stamp(root.path());
        assert_ne!(second, third);

        // Same root + same tree is stable.
        assert_eq!(third, stamp(root.path()));

        // A different root is a different stamp even with identical content.
        let other = root.path().join("copy");
        std::fs::create_dir_all(other.join("bucket")).unwrap();
        std::fs::write(other.join("bucket").join("vim.json"), br#"{"version":"9"}"#).unwrap();
        assert_ne!(bucket_fingerprint(&other, "main"), third);
    }
}
