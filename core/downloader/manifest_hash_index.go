package downloader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type manifestHashEntry struct {
	CasHash string `json:"cas_hash"`
}

// manifestHashIndexPath constructs the file path for a manifest hash alias entry.
// Returns the path where the alias JSON file would be stored.
func manifestHashIndexPath(storeRoot, algo, digest string) string {
	key := strings.ToLower(algo) + "_" + strings.ToLower(digest)
	return filepath.Join(storeRoot, ".manifest-hash", key+".json")
}

// saveManifestHashAlias saves a manifest hash to CAS hash mapping for future lookups.
// Creates atomically via temp file and rename. Skips if using cache store hash directly.
func saveManifestHashAlias(storeRoot, algo, digest, casHash string) error {
	if digest == "" || casHash == "" || manifestUsesCacheStoreHash(algo) {
		return nil
	}
	dir := filepath.Join(storeRoot, ".manifest-hash")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create manifest hash index dir: %w", err)
	}
	payload, err := json.Marshal(manifestHashEntry{CasHash: casHash})
	if err != nil {
		return err
	}
	path := manifestHashIndexPath(storeRoot, algo, digest)
	tmp, err := os.CreateTemp(dir, "manifest-hash-*.tmp")
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

// loadManifestHashAlias loads a CAS hash from a manifest hash alias entry.
// Returns the CAS hash and true if found, empty string and false otherwise.
func loadManifestHashAlias(storeRoot, algo, digest string) (string, bool) {
	if digest == "" {
		return "", false
	}
	data, err := os.ReadFile(manifestHashIndexPath(storeRoot, algo, digest))
	if err != nil {
		return "", false
	}
	var entry manifestHashEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.CasHash == "" {
		return "", false
	}
	return entry.CasHash, true
}

// recordManifestHashAlias records a manifest hash to CAS hash mapping for future lookups.
// Safely handles nil receiver and store. Errors are silently ignored.
func (d *Downloader) recordManifestHashAlias(hashAlgo, hashValue, casHash string) {
	if d == nil || d.store == nil {
		return
	}
	_ = saveManifestHashAlias(d.store.Path(), hashAlgo, hashValue, casHash)
}

// lookupStoreBlob looks up a CAS blob by manifest hash, using direct hash or alias lookup.
// Returns CAS hash and true if found in store, empty string and false otherwise.
func (d *Downloader) lookupStoreBlob(task Task) (string, bool) {
	if task.HashValue == "" {
		return "", false
	}
	if manifestUsesCacheStoreHash(task.HashAlgo) {
		if d.store.Has(task.HashValue) {
			return task.HashValue, true
		}
		return "", false
	}
	casHash, ok := loadManifestHashAlias(d.store.Path(), task.HashAlgo, task.HashValue)
	if !ok || !d.store.Has(casHash) {
		return "", false
	}
	return casHash, true
}
