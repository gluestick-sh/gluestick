package cache

import (
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
)

// PruneUninstalledPackages removes stale installed_packages rows for packages with no install on disk.
// Content cache entries in packages/package_files are kept for fast reinstall.
func (idx *Index) PruneUninstalledPackages(rootDir string) (int, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	return idx.pruneUninstalledPackagesLocked(rootDir)
}

func (idx *Index) pruneUninstalledPackagesLocked(rootDir string) (int, error) {
	rows, err := idx.db.Query(`SELECT name FROM installed_packages`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		pkgRoot := apps.PkgRoot(rootDir, name)
		if _, _, ok := apps.ActiveInstallDir(pkgRoot); !ok {
			stale = append(stale, name)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}

	tx, err := idx.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	for _, name := range stale {
		if _, err := tx.Exec(`DELETE FROM installed_packages WHERE name = ?`, name); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(stale), nil
}

func packageVersionInstalled(rootDir, pkgName, version string) bool {
	if version == "" {
		return false
	}
	dir := filepath.Join(apps.PkgRoot(rootDir, pkgName), version)
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
