package downloader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/store"
)

// ClearStaleSidecarIndexes removes obsolete .zip-index and .manifest-hash entries
// whose target store blobs no longer exist, plus leftover *.tmp files in those dirs.
func (d *Downloader) ClearStaleSidecarIndexes() (removed int, freedBytes int64, err error) {
	if d == nil || d.store == nil {
		return 0, 0, nil
	}
	root := d.store.Path()
	n, b, err := clearStaleZipIndexes(d.store, root)
	if err != nil {
		return n, b, err
	}
	removed += n
	freedBytes += b
	n, b, err = clearStaleManifestHashAliases(d.store, root)
	if err != nil {
		return removed, freedBytes, err
	}
	removed += n
	freedBytes += b
	return removed, freedBytes, nil
}

func clearStaleZipIndexes(st *store.Store, storeRoot string) (removed int, freedBytes int64, err error) {
	dir := filepath.Join(storeRoot, ".zip-index")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(dir, name)
		size := entrySize(entry)
		stale := strings.HasSuffix(name, ".tmp")
		if !stale && strings.HasSuffix(name, ".json") {
			zipHash := strings.TrimSuffix(name, ".json")
			if zipHash == "" || !st.Has(zipHash) {
				stale = true
			} else if idx, ok := loadZipMemberIndex(storeRoot, zipHash); !ok || !ZipMemberIndexReady(st, idx) {
				stale = true
			}
		} else if !stale {
			stale = true
		}
		if !stale {
			continue
		}
		if rmErr := os.Remove(path); rmErr == nil {
			removed++
			freedBytes += size
		}
	}
	return removed, freedBytes, nil
}

func clearStaleManifestHashAliases(st *store.Store, storeRoot string) (removed int, freedBytes int64, err error) {
	dir := filepath.Join(storeRoot, ".manifest-hash")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(dir, name)
		size := entrySize(entry)
		stale := strings.HasSuffix(name, ".tmp")
		if !stale && strings.HasSuffix(name, ".json") {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				stale = true
			} else {
				var alias manifestHashEntry
				if json.Unmarshal(data, &alias) != nil || alias.CasHash == "" || !st.Has(alias.CasHash) {
					stale = true
				}
			}
		} else if !stale {
			stale = true
		}
		if !stale {
			continue
		}
		if rmErr := os.Remove(path); rmErr == nil {
			removed++
			freedBytes += size
		}
	}
	return removed, freedBytes, nil
}

func entrySize(entry os.DirEntry) int64 {
	info, err := entry.Info()
	if err != nil {
		return 0
	}
	return info.Size()
}
