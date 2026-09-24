package cache

import "github.com/gluestick-sh/core/store"

// buildStoreFileRefIndex maps hardlink identity keys to cache store hashes for every store object.
func buildStoreFileRefIndex(store *store.Store) (map[fileRefKey]string, error) {
	index, _, err := scanStoreParallel(store, nil)
	return index, err
}

func resolveInstallFileHash(path string, store *store.Store, keyIndex map[fileRefKey]string) (string, error) {
	if keyIndex != nil {
		if key, ok := fileRefKeyForPath(path); ok {
			if hash, ok := keyIndex[key]; ok {
				return hash, nil
			}
		}
	}
	return store.HashForPath(path)
}
