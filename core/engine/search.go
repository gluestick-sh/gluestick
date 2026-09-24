package engine

import (
	"strings"

	"github.com/gluestick-sh/core/manifest"
)

// createManifestInfo creates ManifestInfo from manifest
func (e *Engine) createManifestInfo(m *manifest.Manifest) *ManifestInfo {
	return &ManifestInfo{
		URL:          m.GetURL(),
		Hash:         hashString(m.GetHashes()),
		Size:         0, // Would need to be calculated or stored separately
		ExtractDir:   m.GetExtractDir(),
		Binaries:     binariesFromStrings(m.Binaries()),
		EnvPath:      getEnvAddPath(m),
		Architecture: m.SelectedArchitecture(),
		Depends:      m.Depends,
		PostInstall:  strings.Join(m.PostInstallHooks(), "\n"),
		Description:  m.Description,
		Homepage:     m.Homepage,
	}
}

func binariesFromStrings(bins []string) []BinaryInfo {
	var result []BinaryInfo
	for _, b := range bins {
		result = append(result, BinaryInfo{Name: b, Source: b})
	}
	return result
}

// getEnvAddPath gets env_add_path from manifest
func getEnvAddPath(m *manifest.Manifest) []string {
	switch path := m.EnvAddPath.(type) {
	case string:
		return []string{path}
	case []interface{}:
		var result []string
		for _, item := range path {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result
	}
	return nil
}

func hashString(hashes []string) string {
	if len(hashes) > 0 {
		return hashes[0]
	}
	return ""
}
