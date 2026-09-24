// Package cache maintains the SQLite index at ~/.glue/cache/index.db.
//
// Tables:
//   - packages / package_files: content-store blob index per package (kept after uninstall for fast reinstall)
//   - installed_packages: currently installed packages with paths and metadata
//   - install_history: install/uninstall audit rows tied to packages (cascade on package delete)
//   - activity_log: global activity feed with no foreign keys (not cascade-deleted on uninstall)
package cache

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
	_ "modernc.org/sqlite"
)

// Index is the SQLite-backed mapping from packages to store hashes and install metadata.
type Index struct {
	db   *sql.DB
	mu   sync.RWMutex
	path string
}

// PackageEntry represents a single package in the cache index
type PackageEntry struct {
	Version   string            `json:"version"`
	Installed string            `json:"installed"` // ISO 8601 timestamp
	Files     map[string]string `json:"files"`     // hash -> filename
	Size      int64             `json:"size"`      // total size in bytes
}

// NewIndex opens or creates ~/.glue/cache/index.db and migrates legacy cache-index.json when present.
func NewIndex(rootDir string) (*Index, error) {
	cacheDir := filepath.Join(rootDir, "cache")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache dir: %w", err)
	}

	dbPath := filepath.Join(cacheDir, "index.db")

	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	idx := &Index{
		db:   db,
		path: dbPath,
	}

	// Initialize schema
	if err := idx.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Check if we need to migrate from JSON
	jsonPath := filepath.Join(rootDir, "cache-index.json")
	if _, err := os.Stat(jsonPath); err == nil {
		if err := idx.migrateFromJSON(jsonPath); err != nil {
			verbose.Progressf("Warning: failed to migrate from JSON: %v\n", err)
		} else {
			// Backup old JSON file
			backupPath := jsonPath + ".bak"
			_ = os.Rename(jsonPath, backupPath)
			verbose.Progressf("Migrated cache index from JSON to SQLite\n")
		}
	}

	return idx, nil
}

// initSchema creates index tables if they do not exist.
func (idx *Index) initSchema() error {
	_, err := idx.db.Exec(`
		CREATE TABLE IF NOT EXISTS packages (
			name TEXT PRIMARY KEY,
			version TEXT NOT NULL,
			installed TEXT NOT NULL,
			size INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS package_files (
			package_name TEXT NOT NULL,
			hash TEXT NOT NULL,
			filename TEXT NOT NULL,
			PRIMARY KEY (package_name, hash),
			FOREIGN KEY (package_name) REFERENCES packages(name) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS installed_packages (
			name TEXT PRIMARY KEY,
			version TEXT NOT NULL,
			install_dir TEXT NOT NULL,
			install_time TEXT NOT NULL,
			size INTEGER,
			metadata JSON,
			CHECK (name NOT LIKE '%%/%%')  -- Prevent directory-like names
		);

		CREATE TABLE IF NOT EXISTS install_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			operation TEXT NOT NULL,
			package_name TEXT NOT NULL,
			version TEXT NOT NULL,
			status TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			details JSON,
			FOREIGN KEY (package_name) REFERENCES packages(name) ON DELETE CASCADE
		);

		-- Standalone activity log (no FK; rows survive package uninstall / cascade deletes)
		CREATE TABLE IF NOT EXISTS activity_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			operation TEXT NOT NULL,
			package_name TEXT NOT NULL,
			version TEXT NOT NULL,
			status TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			details TEXT
		);

		CREATE INDEX IF NOT EXISTS idx_packages_installed ON packages(installed);
		CREATE INDEX IF NOT EXISTS idx_install_history_operation ON install_history(operation);
		CREATE INDEX IF NOT EXISTS idx_install_history_package ON install_history(package_name);
		CREATE INDEX IF NOT EXISTS idx_activity_log_timestamp ON activity_log(timestamp);
	`)
	return err
}

// migrateFromJSON imports the legacy cache-index.json format into SQLite.
func (idx *Index) migrateFromJSON(jsonPath string) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return err
	}

	var jsonIndex struct {
		Packages map[string]*PackageEntry `json:"packages"`
	}

	if err := json.Unmarshal(data, &jsonIndex); err != nil {
		return err
	}

	// Migrate each package
	for name, entry := range jsonIndex.Packages {
		if err := idx.addNoSave(name, entry.Version, entry.Files, entry.Size, entry.Installed); err != nil {
			return err
		}
	}

	return nil
}

// Close closes the database connection
func (idx *Index) Close() error {
	if idx.db != nil {
		return idx.db.Close()
	}
	return nil
}

// Add records a package's files in the index.
func (idx *Index) Add(pkgName, version string, files map[string]string, totalSize int64) error {
	installed := time.Now().Format(time.RFC3339)
	return idx.addNoSave(pkgName, version, files, totalSize, installed)
}

// addNoSave inserts a packages row and replaces package_files without updating installed_packages.
func (idx *Index) addNoSave(pkgName, version string, files map[string]string, totalSize int64, installed string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Insert or replace package
	_, err = tx.Exec(`
		INSERT OR REPLACE INTO packages (name, version, installed, size)
		VALUES (?, ?, ?, ?)
	`, pkgName, version, installed, totalSize)
	if err != nil {
		return err
	}

	// Delete existing files for this package
	_, err = tx.Exec(`DELETE FROM package_files WHERE package_name = ?`, pkgName)
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO package_files (package_name, hash, filename) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for hash, filename := range files {
		if _, err = stmt.Exec(pkgName, hash, filename); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// Remove drops content-cache rows (packages + package_files) and installed_packages for pkgName.
// install_history rows cascade; activity_log rows are kept.
func (idx *Index) Remove(pkgName string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM packages WHERE name = ?`, pkgName); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM installed_packages WHERE name = ?`, pkgName); err != nil {
		return err
	}
	return tx.Commit()
}

// SetActiveVersion updates the indexed version without changing file entries (used by reset).
func (idx *Index) SetActiveVersion(pkgName, version string) error {
	return idx.SetActiveVersionInstallDir(pkgName, version, "")
}

// SetActiveVersionInstallDir also updates installed_packages when switching active version.
func (idx *Index) SetActiveVersionInstallDir(pkgName, version, installDir string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE packages SET version = ? WHERE name = ?`, version, pkgName)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}

	if installDir != "" {
		_, err = tx.Exec(
			`UPDATE installed_packages SET version = ?, install_dir = ? WHERE name = ?`,
			version, installDir, pkgName,
		)
	} else {
		_, err = tx.Exec(`UPDATE installed_packages SET version = ? WHERE name = ?`, version, pkgName)
	}
	if err != nil {
		return err
	}

	return tx.Commit()
}

// Get returns a package's entry
func (idx *Index) Get(pkgName string) (*PackageEntry, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var entry PackageEntry
	err := idx.db.QueryRow(`
		SELECT version, installed, size
		FROM packages
		WHERE name = ?
	`, pkgName).Scan(&entry.Version, &entry.Installed, &entry.Size)

	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		return nil, false
	}

	// Load files
	entry.Files = make(map[string]string)
	rows, err := idx.db.Query(`
		SELECT hash, filename
		FROM package_files
		WHERE package_name = ?
	`, pkgName)
	if err != nil {
		return &entry, true
	}
	defer rows.Close()

	for rows.Next() {
		var hash, filename string
		if err := rows.Scan(&hash, &filename); err != nil {
			continue
		}
		entry.Files[hash] = filename
	}

	return &entry, true
}

// ListPackages returns package metadata without loading package_files.
func (idx *Index) ListPackages() map[string]*PackageEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.listPackagesLocked()
}

func (idx *Index) listPackagesLocked() map[string]*PackageEntry {
	result := make(map[string]*PackageEntry)

	rows, err := idx.db.Query(`
		SELECT name, version, installed, size
		FROM packages
	`)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var name, version, installed string
		var size int64
		if err := rows.Scan(&name, &version, &installed, &size); err != nil {
			continue
		}

		result[name] = &PackageEntry{
			Version:   version,
			Installed: installed,
			Size:      size,
		}
	}

	return result
}

// PackageFileCounts returns package_name ? file row count from package_files.
func (idx *Index) PackageFileCounts() (map[string]int, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	counts := make(map[string]int)
	rows, err := idx.db.Query(`
		SELECT package_name, COUNT(*)
		FROM package_files
		GROUP BY package_name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			continue
		}
		counts[name] = count
	}
	return counts, rows.Err()
}

// List returns all packages including every file mapping (two queries: packages + package_files).
func (idx *Index) List() map[string]*PackageEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	result := idx.listPackagesLocked()
	if len(result) == 0 {
		return result
	}

	for _, entry := range result {
		entry.Files = make(map[string]string)
	}

	rows, err := idx.db.Query(`
		SELECT package_name, hash, filename
		FROM package_files
	`)
	if err != nil {
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var pkgName, hash, filename string
		if err := rows.Scan(&pkgName, &hash, &filename); err != nil {
			continue
		}
		entry, ok := result[pkgName]
		if !ok {
			continue
		}
		entry.Files[hash] = filename
	}

	return result
}

// ListPackageVersions returns package name ? version only (no package_files load).
// Used for bulk update checks and similar scans.
func (idx *Index) ListPackageVersions() (map[string]string, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	result := make(map[string]string)
	rows, err := idx.db.Query(`SELECT name, version FROM packages`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name, version string
		if err := rows.Scan(&name, &version); err != nil {
			continue
		}
		result[name] = version
	}
	return result, rows.Err()
}

// CountPackages returns the number of rows in packages (indexed content cache entries).
func (idx *Index) CountPackages() (int, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var n int
	err := idx.db.QueryRow(`SELECT COUNT(*) FROM packages`).Scan(&n)
	return n, err
}

// SumInstalledSize returns the sum of packages.size across all indexed packages.
func (idx *Index) SumInstalledSize() (int64, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var total sql.NullInt64
	err := idx.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM packages`).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// GetFilesForPackage returns all hashes for a package
func (idx *Index) GetFilesForPackage(pkgName string) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var hashes []string

	rows, err := idx.db.Query(`
		SELECT hash
		FROM package_files
		WHERE package_name = ?
	`, pkgName)
	if err != nil {
		return nil
	}
	defer rows.Close()

	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			continue
		}
		hashes = append(hashes, hash)
	}

	if len(hashes) == 0 {
		return nil
	}
	return hashes
}

// IndexedFileCountForPackages returns package_files row counts for the given package names.
func (idx *Index) IndexedFileCountForPackages(pkgNames []string) (int, error) {
	if idx == nil || len(pkgNames) == 0 {
		return 0, nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	query := `SELECT COUNT(*) FROM package_files WHERE package_name IN (` + sqlPlaceholders(len(pkgNames)) + `)`
	args := make([]any, len(pkgNames))
	for i, name := range pkgNames {
		args[i] = name
	}
	var count int
	if err := idx.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// sqlPlaceholders builds "?,?,?" for SQL IN (...) clauses.
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	s := "?"
	for i := 1; i < n; i++ {
		s += ",?"
	}
	return s
}

// PackagesSharingHashes returns other indexed packages that reference any of hashes (excluding exclude).
func (idx *Index) PackagesSharingHashes(hashes []string, exclude string) []string {
	hashSet := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		if h != "" {
			hashSet[h] = struct{}{}
		}
	}
	if len(hashSet) == 0 {
		return nil
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	rows, err := idx.db.Query(`SELECT DISTINCT package_name, hash FROM package_files WHERE package_name != ?`, exclude)
	if err != nil {
		return nil
	}
	defer rows.Close()

	shared := make(map[string]struct{})
	for rows.Next() {
		var pkgName, hash string
		if err := rows.Scan(&pkgName, &hash); err != nil {
			continue
		}
		if _, ok := hashSet[hash]; ok {
			shared[pkgName] = struct{}{}
		}
	}
	if len(shared) == 0 {
		return nil
	}
	names := make([]string, 0, len(shared))
	for name := range shared {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RebuildPackageFunc is called for each package indexed during Rebuild; nil disables callbacks.
type RebuildPackageFunc func(pkgName, version string, fileCount int)

// Rebuild scans installed apps and rebuilds the cache index from on-disk hardlinks.
func (idx *Index) Rebuild(appsDir string, store *store.Store, onPackage RebuildPackageFunc) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("cas store is required")
	}

	scanned, err := scanPackagesForRebuild(appsDir, store, onPackage)
	if err != nil {
		return 0, err
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM package_files`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM packages`); err != nil {
		return 0, err
	}

	fileStmt, err := tx.Prepare(`INSERT INTO package_files (package_name, hash, filename) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer fileStmt.Close()

	installed := time.Now().Format(time.RFC3339)
	for _, pkg := range scanned {
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO packages (name, version, installed, size)
			VALUES (?, ?, ?, ?)
		`, pkg.name, pkg.version, installed, pkg.totalSize); err != nil {
			return 0, err
		}
		for hash, filename := range pkg.files {
			if _, err := fileStmt.Exec(pkg.name, hash, filename); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return len(scanned), err
	}
	if _, err := idx.pruneUninstalledPackagesLocked(filepath.Dir(appsDir)); err != nil {
		return len(scanned), err
	}
	return len(scanned), nil
}

// latestVersionDir picks the lexicographically greatest version directory name (Scoop-style).
func latestVersionDir(entries []os.DirEntry) string {
	var latest string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == apps.CurrentLinkName {
			continue
		}
		name := entry.Name()
		if latest == "" || name > latest {
			latest = name
		}
	}
	return latest
}

// InstalledPackage represents a package in the installed_packages table
type InstalledPackage struct {
	Name        string                 `json:"name"`
	Version     string                 `json:"version"`
	InstallDir  string                 `json:"install_dir"`
	InstallTime time.Time              `json:"install_time"`
	Size        int64                  `json:"size"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// parseInstalledMetadataJSON decodes installed_packages.metadata; returns an empty map on error.
func parseInstalledMetadataJSON(metadataJSON string) map[string]interface{} {
	metadata := make(map[string]interface{})
	if metadataJSON == "" || metadataJSON == "null" {
		return metadata
	}
	if err := json.Unmarshal([]byte(metadataJSON), &metadata); err != nil || metadata == nil {
		return make(map[string]interface{})
	}
	return metadata
}

// AddInstalled records an active install and appends a success row to install_history.
func (idx *Index) AddInstalled(pkgName, version, installDir string, size int64, metadata map[string]interface{}) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	installTime := time.Now()
	if metadata == nil {
		metadata = make(map[string]interface{})
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT OR REPLACE INTO installed_packages
		(name, version, install_dir, install_time, size, metadata)
		VALUES (?, ?, ?, ?, ?, ?)
	`, pkgName, version, installDir, installTime.Format(time.RFC3339), size, string(metadataJSON))
	if err != nil {
		return err
	}

	// Also record in install_history
	_, err = tx.Exec(`
		INSERT INTO install_history
		(operation, package_name, version, status, timestamp, details)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "install", pkgName, version, "success", installTime.Format(time.RFC3339), `{"install_dir":"`+installDir+`"}`)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// SetVersionLocked stores version lock state in installed_packages.metadata.
func (idx *Index) SetVersionLocked(pkgName string, locked bool) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var metadataJSON string
	err := idx.db.QueryRow(`
		SELECT metadata FROM installed_packages WHERE name = ?
	`, pkgName).Scan(&metadataJSON)
	if err == sql.ErrNoRows {
		return fmt.Errorf("package %s is not installed", pkgName)
	}
	if err != nil {
		return err
	}

	metadata := parseInstalledMetadataJSON(metadataJSON)
	if locked {
		metadata["versionLocked"] = true
	} else {
		delete(metadata, "versionLocked")
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = idx.db.Exec(`UPDATE installed_packages SET metadata = ? WHERE name = ?`, string(encoded), pkgName)
	return err
}

// GetInstalled returns an installed package
func (idx *Index) GetInstalled(pkgName string) (*InstalledPackage, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var installTime string
	var metadataJSON string
	var pkg InstalledPackage

	err := idx.db.QueryRow(`
		SELECT version, install_dir, install_time, size, metadata
		FROM installed_packages
		WHERE name = ?
	`, pkgName).Scan(&pkg.Version, &pkg.InstallDir, &installTime, &pkg.Size, &metadataJSON)

	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		return nil, false
	}

	// Parse install time
	if t, err := time.Parse(time.RFC3339, installTime); err == nil {
		pkg.InstallTime = t
	}

	pkg.Metadata = parseInstalledMetadataJSON(metadataJSON)

	pkg.Name = pkgName
	return &pkg, true
}

// ListInstalled returns all installed packages
func (idx *Index) ListInstalled() ([]*InstalledPackage, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	rows, err := idx.db.Query(`
		SELECT name, version, install_dir, install_time, size, metadata
		FROM installed_packages
		ORDER BY install_time DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var packages []*InstalledPackage
	for rows.Next() {
		var pkg InstalledPackage
		var installTime, metadataJSON string

		err := rows.Scan(&pkg.Name, &pkg.Version, &pkg.InstallDir, &installTime, &pkg.Size, &metadataJSON)
		if err != nil {
			continue
		}

		// Parse install time
		if t, err := time.Parse(time.RFC3339, installTime); err == nil {
			pkg.InstallTime = t
		}

		pkg.Metadata = parseInstalledMetadataJSON(metadataJSON)

		packages = append(packages, &pkg)
	}

	return packages, nil
}

// existingInstalledMetadataJSON returns prior metadata JSON when re-syncing installed_packages.
func existingInstalledMetadataJSON(tx *sql.Tx, pkgName string) string {
	var metadataJSON string
	err := tx.QueryRow(`SELECT metadata FROM installed_packages WHERE name = ?`, pkgName).Scan(&metadataJSON)
	if err != nil || metadataJSON == "" || metadataJSON == "null" {
		return "{}"
	}
	return metadataJSON
}

// SyncInstalledFromPackages registers installed_packages only when the version dir still exists on disk.
// Content cache rows in packages are kept after uninstall for fast reinstall.
func (idx *Index) SyncInstalledFromPackages(rootDir string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	instRows, err := tx.Query(`SELECT name, version FROM installed_packages`)
	if err != nil {
		return err
	}
	for instRows.Next() {
		var name, version string
		if err := instRows.Scan(&name, &version); err != nil {
			continue
		}
		if packageVersionInstalled(rootDir, name, version) {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM installed_packages WHERE name = ?`, name); err != nil {
			instRows.Close()
			return err
		}
	}
	instRows.Close()

	rows, err := tx.Query(`SELECT name, version, installed, size FROM packages`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var name, version, installed string
		var size int64
		if err := rows.Scan(&name, &version, &installed, &size); err != nil {
			continue
		}
		if !packageVersionInstalled(rootDir, name, version) {
			continue
		}
		installDir := filepath.Join(rootDir, "apps", name, version)
		installTime := installed
		if installTime == "" {
			installTime = time.Now().Format(time.RFC3339)
		}
		metadataJSON := existingInstalledMetadataJSON(tx, name)
		if _, err := tx.Exec(`
			INSERT OR REPLACE INTO installed_packages
			(name, version, install_dir, install_time, size, metadata)
			VALUES (?, ?, ?, ?, ?, ?)
		`, name, version, installDir, installTime, size, metadataJSON); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RemoveInstalled removes installed_packages and logs uninstall in install_history.
// Content cache (packages / package_files) is not removed here.
func (idx *Index) RemoveInstalled(pkgName string) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var version string
	_ = tx.QueryRow(`SELECT version FROM installed_packages WHERE name = ?`, pkgName).Scan(&version)

	ts := time.Now().Format(time.RFC3339)
	var pkgExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM packages WHERE name = ?`, pkgName).Scan(&pkgExists); err != nil {
		return err
	}
	if pkgExists > 0 {
		_, err = tx.Exec(`
			INSERT INTO install_history
			(operation, package_name, version, status, timestamp, details)
			VALUES (?, ?, ?, ?, ?, ?)
		`, "uninstall", pkgName, version, "success", ts, "{}")
		if err != nil {
			return err
		}
	} else {
		// installed_packages has no FK to packages; log to activity_log when content cache row is gone.
		_, err = tx.Exec(`
			INSERT INTO activity_log (operation, package_name, version, status, timestamp, details)
			VALUES (?, ?, ?, ?, ?, ?)
		`, "uninstall", pkgName, version, "success", ts, "{}")
		if err != nil {
			return err
		}
	}

	_, err = tx.Exec(`DELETE FROM installed_packages WHERE name = ?`, pkgName)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// RecordActivity appends one activity log row (install/uninstall/update, etc.).
// Unlike install_history, activity_log has no FK to packages and is not cascade-deleted.
func (idx *Index) RecordActivity(operation, pkgName, version, status string, details map[string]interface{}) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	detailsJSON := "{}"
	if details != nil {
		if b, err := json.Marshal(details); err == nil {
			detailsJSON = string(b)
		}
	}

	_, err := idx.db.Exec(`
		INSERT INTO activity_log (operation, package_name, version, status, timestamp, details)
		VALUES (?, ?, ?, ?, ?, ?)
	`, operation, pkgName, version, status, time.Now().Format(time.RFC3339), detailsJSON)
	return err
}

// GetActivityLog returns rows from activity_log (newest first), optionally filtered by package.
func (idx *Index) GetActivityLog(pkgName string, limit int) ([]map[string]any, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.queryNamedHistoryTable("activity_log", pkgName, limit)
}

// QueryInstallHistory returns rows from install_history (newest first), optionally filtered by package.
func (idx *Index) QueryInstallHistory(pkgName string, limit int) ([]map[string]any, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.queryNamedHistoryTable("install_history", pkgName, limit)
}

func (idx *Index) queryNamedHistoryTable(table, pkgName string, limit int) ([]map[string]any, error) {
	var fromClause string
	switch table {
	case "activity_log":
		fromClause = "activity_log"
	case "install_history":
		fromClause = "install_history"
	default:
		return nil, fmt.Errorf("unknown log table: %s", table)
	}

	var query string
	var args []interface{}

	if pkgName == "" {
		query = `
			SELECT operation, package_name, version, status, timestamp, details
			FROM ` + fromClause + `
			ORDER BY timestamp DESC
		`
		if limit > 0 {
			query += " LIMIT ?"
			args = append(args, limit)
		}
	} else {
		query = `
			SELECT operation, package_name, version, status, timestamp, details
			FROM ` + fromClause + `
			WHERE package_name = ?
			ORDER BY timestamp DESC
		`
		args = append(args, pkgName)
		if limit > 0 {
			query += " LIMIT ?"
			args = append(args, limit)
		}
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []map[string]interface{}
	for rows.Next() {
		var operation, name, version, status, timestamp, details string
		if err := rows.Scan(&operation, &name, &version, &status, &timestamp, &details); err != nil {
			continue
		}
		history = append(history, parseHistoryLogRecord(operation, name, version, status, timestamp, details))
	}
	return history, rows.Err()
}

// parseHistoryLogRecord parses a history log record into a map for JSON serialization.
func parseHistoryLogRecord(operation, pkgName, version, status, timestamp, details string) map[string]any {
	record := map[string]interface{}{
		"operation":    operation,
		"package_name": pkgName,
		"version":      version,
		"status":       status,
		"timestamp":    timestamp,
	}
	if details != "" {
		var detailsMap map[string]interface{}
		if err := json.Unmarshal([]byte(details), &detailsMap); err == nil {
			record["details"] = detailsMap
		} else {
			record["details"] = map[string]interface{}{}
		}
	} else {
		record["details"] = map[string]interface{}{}
	}
	return record
}

// CountActivityLog returns the number of activity log rows, optionally since a timestamp (RFC3339).
func (idx *Index) CountActivityLog(since string) (int, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var count int
	var err error
	if since == "" {
		err = idx.db.QueryRow(`SELECT COUNT(*) FROM activity_log`).Scan(&count)
	} else {
		err = idx.db.QueryRow(`SELECT COUNT(*) FROM activity_log WHERE timestamp >= ?`, since).Scan(&count)
	}
	return count, err
}

// QueryActivityLog returns activity log rows with optional time filter, limit, and offset.
func (idx *Index) QueryActivityLog(since string, limit, offset int) ([]map[string]interface{}, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	query := `
		SELECT id, operation, package_name, version, status, timestamp, details
		FROM activity_log
	`
	var args []interface{}
	if since != "" {
		query += ` WHERE timestamp >= ?`
		args = append(args, since)
	}
	query += ` ORDER BY timestamp DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	if offset > 0 {
		query += ` OFFSET ?`
		args = append(args, offset)
	}

	rows, err := idx.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []map[string]interface{}
	for rows.Next() {
		var id int64
		var operation, pkgName, version, status, timestamp, details string
		if err := rows.Scan(&id, &operation, &pkgName, &version, &status, &timestamp, &details); err != nil {
			continue
		}
		record := map[string]interface{}{
			"id":           id,
			"operation":    operation,
			"package_name": pkgName,
			"version":      version,
			"status":       status,
			"timestamp":    timestamp,
		}
		if details != "" {
			var detailsMap map[string]interface{}
			if err := json.Unmarshal([]byte(details), &detailsMap); err == nil {
				record["details"] = detailsMap
			} else {
				record["details"] = map[string]any{}
			}
		} else {
			record["details"] = map[string]any{}
		}
		history = append(history, record)
	}
	return history, rows.Err()
}

// ClearActivityLog removes all rows from the activity log table.
func (idx *Index) ClearActivityLog() error {
	_, err := idx.ClearActivityLogSince("")
	return err
}

// ClearActivityLogSince removes activity log rows with timestamp >= since (RFC3339).
// An empty since deletes all rows. Returns the number of rows removed.
func (idx *Index) ClearActivityLogSince(since string) (int64, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	var res sql.Result
	var err error
	if since == "" {
		res, err = idx.db.Exec(`DELETE FROM activity_log`)
	} else {
		res, err = idx.db.Exec(`DELETE FROM activity_log WHERE timestamp >= ?`, since)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteActivityLogByID removes a single activity log row by id.
func (idx *Index) DeleteActivityLogByID(id int64) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	_, err := idx.db.Exec(`DELETE FROM activity_log WHERE id = ?`, id)
	return err
}
