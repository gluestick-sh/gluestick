package apps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/manifest"
)

// InstallRecord is saved per version for glue reset.
type InstallRecord struct {
	Version  string             `json:"version"`
	Bucket   string             `json:"bucket"`
	Manifest *manifest.Manifest `json:"manifest"`
}

// SaveInstallRecord writes install.json into the version directory.
func SaveInstallRecord(installDir, bucket string, m *manifest.Manifest) error {
	if m == nil {
		return fmt.Errorf("manifest is nil")
	}
	rec := InstallRecord{
		Version:  m.Version,
		Bucket:   bucket,
		Manifest: m,
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(installDir, InstallRecordFile)
	return os.WriteFile(path, data, 0644)
}

// LoadInstallRecord reads install.json from a version directory.
func LoadInstallRecord(installDir string) (*InstallRecord, error) {
	path := filepath.Join(installDir, InstallRecordFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec InstallRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}
