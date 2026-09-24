package downloader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/store"
)

const zipMemberIndexFormatAdopt = 3

type zipMemberIndex struct {
	Format     int               `json:"format,omitempty"`
	Files      map[string]string `json:"files"`
	TotalBytes int64             `json:"total_bytes,omitempty"`
	// Sizes is deprecated; kept for indexes built before total_bytes rollout.
	Sizes map[string]int64 `json:"sizes,omitempty"`
}

// ZipMemberIndex maps zip member relative paths to cache store hashes for fast reinstall.
type ZipMemberIndex struct {
	Format     int
	Files      map[string]string
	TotalBytes int64
}

func zipMemberIndexPath(storeRoot, zipHash string) string {
	return filepath.Join(storeRoot, ".zip-index", zipHash+".json")
}

func saveZipMemberIndex(storeRoot, zipHash string, files map[string]string, totalBytes int64) error {
	if zipHash == "" || len(files) == 0 {
		return nil
	}
	dir := filepath.Join(storeRoot, ".zip-index")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create zip index dir: %w", err)
	}
	payload, err := json.Marshal(zipMemberIndex{
		Format:     zipMemberIndexFormatAdopt,
		Files:      files,
		TotalBytes: totalBytes,
	})
	if err != nil {
		return err
	}
	path := zipMemberIndexPath(storeRoot, zipHash)
	tmp, err := os.CreateTemp(dir, "zip-index-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func loadZipMemberIndex(storeRoot, zipHash string) (ZipMemberIndex, bool) {
	if zipHash == "" {
		return ZipMemberIndex{}, false
	}
	data, err := os.ReadFile(zipMemberIndexPath(storeRoot, zipHash))
	if err != nil {
		return ZipMemberIndex{}, false
	}
	var idx zipMemberIndex
	if err := json.Unmarshal(data, &idx); err != nil || len(idx.Files) == 0 {
		return ZipMemberIndex{}, false
	}
	total := idx.TotalBytes
	if total == 0 && len(idx.Sizes) > 0 {
		for _, size := range idx.Sizes {
			total += size
		}
	}
	return ZipMemberIndex{Format: idx.Format, Files: idx.Files, TotalBytes: total}, true
}

// RemoveZipMemberIndex deletes a stale zip member index.
func RemoveZipMemberIndex(storeRoot, zipHash string) {
	_ = os.Remove(zipMemberIndexPath(storeRoot, zipHash))
}

// ZipMemberIndexReady reports whether index member blobs exist in cache store (link path safe).
func ZipMemberIndexReady(store *store.Store, idx ZipMemberIndex) bool {
	if store == nil || len(idx.Files) == 0 {
		return false
	}
	return zipMemberBlobsPresent(store, idx.Files)
}

func zipMemberBlobsPresent(store *store.Store, files map[string]string) bool {
	seen := make(map[string]struct{}, len(files))
	for _, hash := range files {
		if hash == "" {
			return false
		}
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		if !store.Has(hash) {
			return false
		}
	}
	return true
}
