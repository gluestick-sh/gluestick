// Package config reads and writes ~/.glue/config.json (GitHub mirrors, workers,
// catalog overrides, and other glue-wide settings).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// configFile represents the JSON structure of the config file, containing all serializable settings.
type configFile struct {
	GitHubProxy                string                              `json:"github_proxy,omitempty"`
	DownloadWorkers            *int                                `json:"download_workers,omitempty"`
	BucketCheckIntervalMinutes *int                                `json:"bucket_check_interval_minutes,omitempty"`
	BucketSyncMode             string                              `json:"bucket_sync_mode,omitempty"`
	BucketDescriptions         map[string]string                   `json:"bucket_descriptions,omitempty"`
	ManifestDownloadOverrides  map[string]ManifestDownloadOverride `json:"manifest_download_overrides,omitempty"`
	ManifestJSONOverrides      map[string]ManifestJSONOverride     `json:"manifest_json_overrides,omitempty"`
	HiddenCatalogPackages      []string                            `json:"hidden_catalog_packages,omitempty"`
	Verbose                    *bool                               `json:"verbose,omitempty"`
	ParallelDownload           *bool                               `json:"parallel_download,omitempty"`
	Color                      *bool                               `json:"color,omitempty"`
}

// Basics holds config.json keys managed by the glue config CLI.
type Basics struct {
	GitHubProxy      string
	Verbose          *bool
	ParallelDownload *bool
	Color            *bool
}

// DefaultBucketCheckIntervalMinutes is the background bucket update-check interval.
const DefaultBucketCheckIntervalMinutes = 15

// AllowedBucketCheckIntervals lists supported bucket check intervals (minutes).
var AllowedBucketCheckIntervals = []int{5, 15, 30}

const (
	BucketSyncModeManual    = "manual"
	BucketSyncModeAuto      = "auto"
	DefaultBucketSyncMode   = BucketSyncModeManual
)

// NormalizeBucketSyncMode clamps to a supported bucket sync mode.
func NormalizeBucketSyncMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case BucketSyncModeAuto:
		return BucketSyncModeAuto
	default:
		return BucketSyncModeManual
	}
}

// NormalizeBucketCheckInterval clamps to a supported interval.
func NormalizeBucketCheckInterval(minutes int) int {
	if slices.Contains(AllowedBucketCheckIntervals, minutes) {
		return minutes
	}
	return DefaultBucketCheckIntervalMinutes
}

// Path returns the path to config.json under the glue root directory.
func Path(rootDir string) string {
	return filepath.Join(rootDir, "config.json")
}

// ConfigPath is an alias for Path.
func ConfigPath(rootDir string) string {
	return Path(rootDir)
}

// readConfigFile reads the config file from rootDir/config.json. Returns empty config if file doesn't exist.
func readConfigFile(rootDir string) (*configFile, error) {
	if rootDir == "" {
		return &configFile{}, nil
	}
	data, err := os.ReadFile(Path(rootDir))
	if err != nil {
		if os.IsNotExist(err) {
			return &configFile{}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg configFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// writeConfigFile writes the config to rootDir/config.json with 2-space indentation.
func writeConfigFile(rootDir string, cfg *configFile) error {
	if rootDir == "" {
		return fmt.Errorf("glue root directory unavailable")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(Path(rootDir), data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// ReadBasics returns CLI-managed settings from config.json.
func ReadBasics(rootDir string) (*Basics, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return nil, err
	}
	return &Basics{
		GitHubProxy:      cfg.GitHubProxy,
		Verbose:          copyBoolPtr(cfg.Verbose),
		ParallelDownload: copyBoolPtr(cfg.ParallelDownload),
		Color:            copyBoolPtr(cfg.Color),
	}, nil
}

// WriteBasics updates CLI-managed settings while preserving other config keys.
func WriteBasics(rootDir string, basics *Basics) error {
	if basics == nil {
		return fmt.Errorf("config basics is nil")
	}
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	cfg.GitHubProxy = basics.GitHubProxy
	cfg.Verbose = copyBoolPtr(basics.Verbose)
	cfg.ParallelDownload = copyBoolPtr(basics.ParallelDownload)
	cfg.Color = copyBoolPtr(basics.Color)
	return writeConfigFile(rootDir, cfg)
}

// copyBoolPtr creates a deep copy of a bool pointer to avoid sharing underlying data.
func copyBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	b := *v
	return &b
}

// ReadConfigVerbose returns verbose from config.json.
func ReadConfigVerbose(rootDir string) (bool, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return false, err
	}
	if cfg.Verbose == nil {
		return false, nil
	}
	return *cfg.Verbose, nil
}

// ReadConfigGitHubProxy returns github_proxy from config.json (not env).
func ReadConfigGitHubProxy(rootDir string) (string, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return "", err
	}
	return cfg.GitHubProxy, nil
}

// WriteConfigGitHubProxy updates github_proxy in config.json. Empty value clears the setting.
func WriteConfigGitHubProxy(rootDir, value string) error {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	cfg.GitHubProxy = value
	return writeConfigFile(rootDir, cfg)
}

// ReadConfigDownloadWorkers returns download_workers from config.json when set.
func ReadConfigDownloadWorkers(rootDir string) (int, bool, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return 0, false, err
	}
	if cfg.DownloadWorkers == nil {
		return 0, false, nil
	}
	return *cfg.DownloadWorkers, true, nil
}

// WriteConfigDownloadWorkers updates download_workers in config.json.
func WriteConfigDownloadWorkers(rootDir string, workers int) error {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	cfg.DownloadWorkers = &workers
	return writeConfigFile(rootDir, cfg)
}

// ReadConfigBucketCheckInterval returns bucket_check_interval_minutes from config.json when set.
func ReadConfigBucketCheckInterval(rootDir string) (int, bool, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return 0, false, err
	}
	if cfg.BucketCheckIntervalMinutes == nil {
		return 0, false, nil
	}
	return NormalizeBucketCheckInterval(*cfg.BucketCheckIntervalMinutes), true, nil
}

// WriteConfigBucketCheckInterval updates bucket_check_interval_minutes in config.json.
func WriteConfigBucketCheckInterval(rootDir string, minutes int) error {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	normalized := NormalizeBucketCheckInterval(minutes)
	cfg.BucketCheckIntervalMinutes = &normalized
	return writeConfigFile(rootDir, cfg)
}

// ReadConfigBucketSyncMode returns bucket_sync_mode from config.json when set.
func ReadConfigBucketSyncMode(rootDir string) (string, bool, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(cfg.BucketSyncMode) == "" {
		return "", false, nil
	}
	return NormalizeBucketSyncMode(cfg.BucketSyncMode), true, nil
}

// WriteConfigBucketSyncMode updates bucket_sync_mode in config.json.
func WriteConfigBucketSyncMode(rootDir, mode string) error {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	cfg.BucketSyncMode = NormalizeBucketSyncMode(mode)
	return writeConfigFile(rootDir, cfg)
}

// ReadConfigBucketDescriptions returns user-defined bucket descriptions from config.json.
func ReadConfigBucketDescriptions(rootDir string) (map[string]string, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.BucketDescriptions) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(cfg.BucketDescriptions))
	for name, desc := range cfg.BucketDescriptions {
		name = strings.TrimSpace(name)
		desc = strings.TrimSpace(desc)
		if name == "" || desc == "" {
			continue
		}
		out[name] = desc
	}
	return out, nil
}

// SetConfigBucketDescription stores or clears a bucket description override in config.json.
func SetConfigBucketDescription(rootDir, name, description string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("bucket name is required")
	}
	description = strings.TrimSpace(description)
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	if cfg.BucketDescriptions == nil {
		cfg.BucketDescriptions = map[string]string{}
	}
	if description == "" {
		delete(cfg.BucketDescriptions, name)
		if len(cfg.BucketDescriptions) == 0 {
			cfg.BucketDescriptions = nil
		}
	} else {
		cfg.BucketDescriptions[name] = description
	}
	return writeConfigFile(rootDir, cfg)
}

// ManifestDownloadOverride stores user-edited download URLs for a package ref.
// BaseHash is the bucket manifest file hash at save time; when it differs, the
// override is ignored so versioned URLs do not stick after bucket updates.
type ManifestDownloadOverride struct {
	URLs     []string `json:"urls,omitempty"`
	Hashes   []string `json:"hashes,omitempty"`
	BaseHash string   `json:"baseHash,omitempty"`
}

// NormalizeManifestOverrideKey lowercases a package ref for config lookup.
func NormalizeManifestOverrideKey(pkgRef string) string {
	return strings.ToLower(strings.TrimSpace(pkgRef))
}

// ReadConfigManifestDownloadOverrides returns saved manifest download overrides.
func ReadConfigManifestDownloadOverrides(rootDir string) (map[string]ManifestDownloadOverride, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.ManifestDownloadOverrides) == 0 {
		return map[string]ManifestDownloadOverride{}, nil
	}
	out := make(map[string]ManifestDownloadOverride, len(cfg.ManifestDownloadOverrides))
	for ref, item := range cfg.ManifestDownloadOverrides {
		key := NormalizeManifestOverrideKey(ref)
		if key == "" || len(item.URLs) == 0 {
			continue
		}
		out[key] = item
	}
	return out, nil
}

// SetConfigManifestDownloadOverride stores or clears a download URL override for pkgRef.
// baseHash should be the bucket manifest file hash when saving; empty baseHash is allowed
// only when clearing (empty urls).
func SetConfigManifestDownloadOverride(rootDir, pkgRef string, urls, hashes []string, baseHash string) error {
	key := NormalizeManifestOverrideKey(pkgRef)
	if key == "" {
		return fmt.Errorf("package ref is required")
	}
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	if cfg.ManifestDownloadOverrides == nil {
		cfg.ManifestDownloadOverrides = map[string]ManifestDownloadOverride{}
	}
	trimmed := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u != "" {
			trimmed = append(trimmed, u)
		}
	}
	if len(trimmed) == 0 {
		delete(cfg.ManifestDownloadOverrides, key)
		if len(cfg.ManifestDownloadOverrides) == 0 {
			cfg.ManifestDownloadOverrides = nil
		}
		return writeConfigFile(rootDir, cfg)
	}
	hashTrimmed := make([]string, 0, len(hashes))
	for _, h := range hashes {
		h = strings.TrimSpace(h)
		if h != "" {
			hashTrimmed = append(hashTrimmed, h)
		}
	}
	cfg.ManifestDownloadOverrides[key] = ManifestDownloadOverride{
		URLs:     trimmed,
		Hashes:   hashTrimmed,
		BaseHash: strings.TrimSpace(baseHash),
	}
	return writeConfigFile(rootDir, cfg)
}

// PruneManifestDownloadOverrides removes saved download overrides for which keep returns false.
// Returns the package refs that were removed.
func PruneManifestDownloadOverrides(rootDir string, keep func(pkgRef string, item ManifestDownloadOverride) bool) (removed []string, err error) {
	if keep == nil {
		return nil, fmt.Errorf("keep callback is required")
	}
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.ManifestDownloadOverrides) == 0 {
		return nil, nil
	}
	next := make(map[string]ManifestDownloadOverride, len(cfg.ManifestDownloadOverrides))
	for ref, item := range cfg.ManifestDownloadOverrides {
		key := NormalizeManifestOverrideKey(ref)
		if key == "" || len(item.URLs) == 0 {
			continue
		}
		if keep(key, item) {
			next[key] = item
			continue
		}
		removed = append(removed, key)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if len(next) == 0 {
		cfg.ManifestDownloadOverrides = nil
	} else {
		cfg.ManifestDownloadOverrides = next
	}
	if err := writeConfigFile(rootDir, cfg); err != nil {
		return nil, err
	}
	return removed, nil
}

// ManifestJSONOverride stores a user-edited manifest JSON tied to the bucket file hash at save time.
type ManifestJSONOverride struct {
	JSON     string `json:"json"`
	BaseHash string `json:"baseHash"`
}

// ReadConfigManifestJSONOverrides returns saved manifest JSON overrides.
func ReadConfigManifestJSONOverrides(rootDir string) (map[string]ManifestJSONOverride, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.ManifestJSONOverrides) == 0 {
		return map[string]ManifestJSONOverride{}, nil
	}
	out := make(map[string]ManifestJSONOverride, len(cfg.ManifestJSONOverrides))
	for ref, item := range cfg.ManifestJSONOverrides {
		key := NormalizeManifestOverrideKey(ref)
		if key == "" || strings.TrimSpace(item.JSON) == "" {
			continue
		}
		out[key] = item
	}
	return out, nil
}

// SetConfigManifestJSONOverride stores or clears a manifest JSON override for pkgRef.
func SetConfigManifestJSONOverride(rootDir, pkgRef, jsonText, baseHash string) error {
	key := NormalizeManifestOverrideKey(pkgRef)
	if key == "" {
		return fmt.Errorf("package ref is required")
	}
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	jsonText = strings.TrimSpace(jsonText)
	if jsonText == "" {
		if cfg.ManifestJSONOverrides == nil {
			return nil
		}
		delete(cfg.ManifestJSONOverrides, key)
		if len(cfg.ManifestJSONOverrides) == 0 {
			cfg.ManifestJSONOverrides = nil
		}
		return writeConfigFile(rootDir, cfg)
	}
	if cfg.ManifestJSONOverrides == nil {
		cfg.ManifestJSONOverrides = map[string]ManifestJSONOverride{}
	}
	cfg.ManifestJSONOverrides[key] = ManifestJSONOverride{
		JSON:     jsonText,
		BaseHash: strings.TrimSpace(baseHash),
	}
	return writeConfigFile(rootDir, cfg)
}

// NormalizeCatalogPackageRef canonicalizes a catalog package ref to bucket/name (main/name when omitted).
func NormalizeCatalogPackageRef(pkgRef string) string {
	key := NormalizeManifestOverrideKey(pkgRef)
	if key == "" {
		return ""
	}
	if !strings.Contains(key, "/") {
		return "main/" + key
	}
	return key
}

// ReadConfigHiddenCatalogPackages returns user-hidden catalog package refs (bucket/name).
func ReadConfigHiddenCatalogPackages(rootDir string) (map[string]struct{}, error) {
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return nil, err
	}
	if len(cfg.HiddenCatalogPackages) == 0 {
		return map[string]struct{}{}, nil
	}
	out := make(map[string]struct{}, len(cfg.HiddenCatalogPackages))
	for _, ref := range cfg.HiddenCatalogPackages {
		key := NormalizeCatalogPackageRef(ref)
		if key != "" {
			out[key] = struct{}{}
		}
	}
	return out, nil
}

// AddConfigHiddenCatalogPackage records a catalog package ref to hide from browse/search.
func AddConfigHiddenCatalogPackage(rootDir, pkgRef string) error {
	key := NormalizeCatalogPackageRef(pkgRef)
	if key == "" {
		return fmt.Errorf("package ref is required")
	}
	cfg, err := readConfigFile(rootDir)
	if err != nil {
		return err
	}
	for _, existing := range cfg.HiddenCatalogPackages {
		if NormalizeCatalogPackageRef(existing) == key {
			return nil
		}
	}
	cfg.HiddenCatalogPackages = append(cfg.HiddenCatalogPackages, key)
	return writeConfigFile(rootDir, cfg)
}
