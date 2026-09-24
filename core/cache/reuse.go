package cache

import "github.com/gluestick-sh/core/store"

// Reusable returns a cache entry when the same version is indexed and every
// referenced cache store object still exists on disk.
func (idx *Index) Reusable(pkgName, version string, store *store.Store) (*PackageEntry, bool) {
	entry, ok := idx.Get(pkgName)
	if !ok || entry.Version != version {
		return nil, false
	}
	if !AllObjectsPresent(store, entry) {
		return nil, false
	}
	return entry, true
}

// AllObjectsPresent reports whether every hash in entry still exists in cache store.
func AllObjectsPresent(store *store.Store, entry *PackageEntry) bool {
	if entry == nil || len(entry.Files) == 0 {
		return false
	}
	for hash := range entry.Files {
		if !store.Has(hash) {
			return false
		}
	}
	return true
}
