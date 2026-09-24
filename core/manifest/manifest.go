// Package manifest parses and queries Scoop-style package JSON manifests.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Manifest represents a Scoop-compatible package manifest
type Manifest struct {
	Version       string            `json:"version"`
	Description   string            `json:"description"`
	Homepage      string            `json:"homepage"`
	License       interface{}       `json:"license"` // string or object
	URL           interface{}       `json:"url"`     // string or []string
	Hash          interface{}       `json:"hash"`    // string or []string
	Installer     Installer         `json:"installer"`
	Extractor     string            `json:"extractor,omitempty"`
	Depends       []string          `json:"depends,omitempty"`
	DependsPre    []string          `json:"depends_pre,omitempty"`
	DependsPost   []string          `json:"depends_post,omitempty"`
	Suggest       interface{}       `json:"suggest,omitempty"` // map label -> pkg ref (Scoop)
	Env           map[string]string `json:"env,omitempty"`
	EnvSet        map[string]string `json:"env_set,omitempty"`
	PreInstall    interface{}       `json:"pre_install,omitempty"`    // string or []string (Scoop)
	PostInstall   interface{}       `json:"post_install,omitempty"`   // string or []string (Scoop)
	PreUninstall  interface{}       `json:"pre_uninstall,omitempty"`  // string or []string (Scoop)
	PostUninstall interface{}       `json:"post_uninstall,omitempty"` // string or []string (Scoop)
	Uninstaller   Uninstaller       `json:"uninstaller,omitempty"`

	// Scoop standard fields
	Bin        interface{} `json:"bin"` // string, []string, or []map
	Shortcuts  interface{} `json:"shortcuts,omitempty"`
	EnvAddPath interface{} `json:"env_add_path,omitempty"` // string or []string

	// Scoop advanced fields
	InnoSetup    bool                   `json:"innosetup,omitempty"` // Inno Setup installer; extract with innounp
	Architecture map[string]interface{} `json:"architecture,omitempty"`
	ExtractDir   string                 `json:"extract_dir,omitempty"` // Extract from subdirectory in archive, then flatten
	ExtractTo    string                 `json:"extract_to,omitempty"`  // Extract into subdirectory under install dir (Scoop)
	Persist      interface{}            `json:"persist,omitempty"`     // string or []string / [install,data] pairs (Scoop)
	Notes        interface{}            `json:"notes,omitempty"`       // string or []string
	Deprecated   interface{}            `json:"deprecated,omitempty"`  // Scoop deprecation notice (string or object)
	Autoupdate   map[string]interface{} `json:"autoupdate,omitempty"`

	// VersionProbe optionally describes how to read the runtime-installed version
	// (e.g. after an app self-updates in place). Unknown to Scoop; ignored by Scoop.
	VersionProbe *VersionProbe `json:"versionProbe,omitempty"`
}

// VersionProbe configures DetectInstalledVersion strategies for a package.
type VersionProbe struct {
	// File is a path relative to the install directory (preferred probe target).
	File string `json:"file,omitempty"`
	// Strategies is an ordered list: "fileversion", "command". Empty = defaults.
	Strategies []string `json:"strategies,omitempty"`
	// Args are passed when using the command strategy (default: ["--version"]).
	Args []string `json:"args,omitempty"`
	// Pattern is an optional regex with one capture group for command stdout.
	Pattern string `json:"pattern,omitempty"`
	// JSONKey selects a field when probing a JSON sidecar (default: "version").
	JSONKey string `json:"jsonKey,omitempty"`
}

// Installer defines how to install the package
type Installer struct {
	Script  interface{} `json:"script,omitempty"` // string or map
	Args    []string    `json:"args,omitempty"`
	Keep    bool        `json:"keep,omitempty"`
	Changes []string    `json:"changes,omitempty"`
}

// GetURL returns the primary download URL
// Handles Scoop's architecture-specific URLs for the host CPU.
func (m *Manifest) GetURL() string {
	if url := getStringOrFirst(m.URL); url != "" {
		return url
	}
	if block := m.selectedArchBlock(); block != nil {
		return getStringOrFirst(block["url"])
	}
	return ""
}

// GetURLs returns all download URLs (supports multiple mirrors)
// Returns URLs from root url or the host-selected architecture block.
func (m *Manifest) GetURLs() []string {
	return m.GetURLsForInstall("")
}

// GetURLsForInstall returns download URLs for override architecture when set.
func (m *Manifest) GetURLsForInstall(override string) []string {
	if urls := stringSliceFromField(m.URL); len(urls) > 0 {
		return urls
	}
	if block := m.archBlockForInstall(override); block != nil {
		return stringSliceFromField(block["url"])
	}
	return nil
}

// GetHash returns the primary hash value
// Handles Scoop's architecture-specific hashes for the host CPU.
func (m *Manifest) GetHash() string {
	if hash := getStringOrFirst(m.Hash); hash != "" {
		return hash
	}
	if block := m.selectedArchBlock(); block != nil {
		return getStringOrFirst(block["hash"])
	}
	return ""
}

// GetHashes returns all hash values corresponding to URLs
// Supports multiple hashes for multiple URLs (mirrors)
// Returns hashes in same order as GetURLs()
func (m *Manifest) GetHashes() []string {
	return m.GetHashesForInstall("")
}

// GetHashesForInstall returns hashes for override architecture when set.
func (m *Manifest) GetHashesForInstall(override string) []string {
	if hashes := stringSliceFromField(m.Hash); len(hashes) > 0 {
		return hashes
	}
	if block := m.archBlockForInstall(override); block != nil {
		return stringSliceFromField(block["hash"])
	}
	return nil
}

// GetExtractDir returns the extraction subdirectory
// Handles Scoop's architecture-specific extract_dir for the host CPU.
func (m *Manifest) GetExtractDir() string {
	return m.GetExtractDirForInstall("")
}

// GetExtractDirForInstall returns extract_dir for override architecture when set.
func (m *Manifest) GetExtractDirForInstall(override string) string {
	if m.ExtractDir != "" {
		return m.ExtractDir
	}
	if block := m.archBlockForInstall(override); block != nil {
		if extractDir, ok := block["extract_dir"].(string); ok {
			return extractDir
		}
	}
	return ""
}

// GetExtractTo returns the install subdirectory for archive extraction (Scoop extract_to).
func (m *Manifest) GetExtractTo() string {
	return m.GetExtractToForInstall("")
}

// GetExtractToForInstall returns extract_to for override architecture when set.
func (m *Manifest) GetExtractToForInstall(override string) string {
	if m.ExtractTo != "" {
		return m.ExtractTo
	}
	if block := m.archBlockForInstall(override); block != nil {
		if extractTo, ok := block["extract_to"].(string); ok {
			return extractTo
		}
	}
	return ""
}

// GetLicense returns the license identifier
func (m *Manifest) GetLicense() string {
	switch v := m.License.(type) {
	case string:
		return v
	case map[string]interface{}:
		if identifier, ok := v["identifier"].(string); ok {
			return identifier
		}
	}
	return ""
}

// Binaries returns executable file patterns for shims
// Scoop supports: string, []string, or []map with name/exe/alias/shim params
func (m *Manifest) Binaries() []string {
	if bins := binariesFromField(m.Bin); len(bins) > 0 {
		return bins
	}

	// Check installer.script for bin patterns (alternative format)
	if script, ok := m.Installer.Script.(map[string]interface{}); ok {
		if bins := binariesFromField(script["bin"]); len(bins) > 0 {
			return bins
		}
	}

	// Architecture-specific bin (e.g. versions/r43 with bin only under 64bit)
	if block := m.selectedArchBlock(); block != nil {
		if bins := binariesFromField(block["bin"]); len(bins) > 0 {
			return bins
		}
	}

	return nil
}

func (m *Manifest) launchBinariesFromArchitecture() []string {
	if m == nil || m.Architecture == nil {
		return nil
	}
	for _, arch := range []string{ArchARM64, Arch64bit, Arch32bit} {
		block, ok := m.Architecture[arch].(map[string]interface{})
		if !ok {
			continue
		}
		if bins := binariesFromField(block["bin"]); len(bins) > 0 {
			return bins
		}
	}
	return nil
}

func (m *Manifest) launchShortcutsFromArchitecture() []ShortcutEntry {
	if m == nil || m.Architecture == nil {
		return nil
	}
	for _, arch := range []string{ArchARM64, Arch64bit, Arch32bit} {
		block, ok := m.Architecture[arch].(map[string]interface{})
		if !ok {
			continue
		}
		if entries := parseShortcutEntries(block["shortcuts"]); len(entries) > 0 {
			return entries
		}
	}
	return nil
}

// LaunchBinaries returns bin entries for open-program UI.
// Unlike Binaries(), architecture blocks qualify even without download URLs.
func (m *Manifest) LaunchBinaries() []string {
	if m == nil {
		return nil
	}
	if bins := binariesFromField(m.Bin); len(bins) > 0 {
		return bins
	}
	if script, ok := m.Installer.Script.(map[string]interface{}); ok {
		if bins := binariesFromField(script["bin"]); len(bins) > 0 {
			return bins
		}
	}
	if block := m.selectedArchBlock(); block != nil {
		if bins := binariesFromField(block["bin"]); len(bins) > 0 {
			return bins
		}
	}
	return m.launchBinariesFromArchitecture()
}

// LaunchShortcuts returns shortcuts for open-program UI.
func (m *Manifest) LaunchShortcuts() []ShortcutEntry {
	if m == nil {
		return nil
	}
	if entries := parseShortcutEntries(m.Shortcuts); len(entries) > 0 {
		return entries
	}
	if block := m.archBlockForInstall(""); block != nil {
		if entries := parseShortcutEntries(block["shortcuts"]); len(entries) > 0 {
			return entries
		}
	}
	return m.launchShortcutsFromArchitecture()
}

// HasLaunchDefinitions reports whether manifest declares bins and/or shortcuts.
func (m *Manifest) HasLaunchDefinitions() bool {
	if m == nil {
		return false
	}
	return len(m.LaunchBinaries()) > 0 || len(m.LaunchShortcuts()) > 0
}

// Uninstaller defines Scoop-style uninstall hooks.
type Uninstaller struct {
	Script interface{} `json:"script,omitempty"` // string or []string
}

// ShortcutEntry is a Scoop shortcuts item for the Start Menu.
type ShortcutEntry struct {
	Target string
	Label  string
	Args   string
	Icon   string
}

// ShortcutEntries returns start-menu shortcuts from the manifest root.
func (m *Manifest) ShortcutEntries() []ShortcutEntry {
	return m.ShortcutEntriesForInstall("")
}

// ShortcutEntriesForInstall returns shortcuts for override architecture when set.
func (m *Manifest) ShortcutEntriesForInstall(override string) []ShortcutEntry {
	if m == nil {
		return nil
	}
	if block := m.archBlockForInstall(override); block != nil {
		if entries := parseShortcutEntries(block["shortcuts"]); len(entries) > 0 {
			return entries
		}
	}
	return parseShortcutEntries(m.Shortcuts)
}

func parseShortcutEntries(raw interface{}) []ShortcutEntry {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	var out []ShortcutEntry
	for _, item := range items {
		entry, ok := item.([]interface{})
		if !ok || len(entry) < 2 {
			continue
		}
		target, _ := entry[0].(string)
		label, _ := entry[1].(string)
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		sc := ShortcutEntry{Target: target, Label: strings.TrimSpace(label)}
		if len(entry) >= 3 {
			sc.Args, _ = entry[2].(string)
		}
		if len(entry) >= 4 {
			sc.Icon, _ = entry[3].(string)
		}
		out = append(out, sc)
	}
	return out
}

func binariesFromField(bin interface{}) []string {
	if bin == nil {
		return nil
	}
	switch v := bin.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []string:
		return v
	case []interface{}:
		var bins []string
		for _, b := range v {
			switch item := b.(type) {
			case string:
				bins = append(bins, item)
			case []interface{}:
				// Handle nested arrays like ["path.exe", "alias"]
				if len(item) > 0 {
					if path, ok := item[0].(string); ok {
						if len(item) > 1 {
							if alias, ok := item[1].(string); ok {
								bins = append(bins, "["+path+","+alias+"]")
							} else {
								bins = append(bins, path)
							}
						} else {
							bins = append(bins, path)
						}
					}
				}
			case map[string]interface{}:
				exe := binMapString(item, "name", "file")
				alias := binMapString(item, "shim", "alias")
				if exe == "" {
					continue
				}
				if alias != "" {
					bins = append(bins, "["+exe+","+alias+"]")
				} else {
					bins = append(bins, exe)
				}
			}
		}
		return bins
	default:
		return nil
	}
}

func binMapString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// DependsPreList returns depends_pre from the manifest or architecture block.
func (m *Manifest) DependsPreList() []string {
	if m == nil {
		return nil
	}
	if block := m.selectedArchBlock(); block != nil {
		if deps := stringSliceFromInterface(block["depends_pre"]); len(deps) > 0 {
			return deps
		}
	}
	return m.DependsPre
}

// DependsPostList returns depends_post from the manifest or architecture block.
func (m *Manifest) DependsPostList() []string {
	if m == nil {
		return nil
	}
	if block := m.selectedArchBlock(); block != nil {
		if deps := stringSliceFromInterface(block["depends_post"]); len(deps) > 0 {
			return deps
		}
	}
	return m.DependsPost
}

// EnvVarsForInstall returns manifest env vars for the selected architecture.
func (m *Manifest) EnvVarsForInstall(override string) map[string]string {
	if m == nil {
		return nil
	}
	out := copyStringMap(m.Env)
	if block := m.archBlockForInstall(override); block != nil {
		out = mergeStringMaps(out, stringMapFromInterface(block["env"]))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EnvSetForInstall returns env_set vars for the selected architecture.
func (m *Manifest) EnvSetForInstall(override string) map[string]string {
	if m == nil {
		return nil
	}
	out := copyStringMap(m.EnvSet)
	if block := m.archBlockForInstall(override); block != nil {
		out = mergeStringMaps(out, stringMapFromInterface(block["env_set"]))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// EnvAddPaths returns PATH directories from env_add_path (string or []string in Scoop).
func (m *Manifest) EnvAddPaths() []string {
	if paths := stringSliceFromInterface(m.EnvAddPath); len(paths) > 0 {
		return paths
	}
	if block := m.selectedArchBlock(); block != nil {
		return stringSliceFromInterface(block["env_add_path"])
	}
	return nil
}

// GetNotes returns manifest notes (string or []string in Scoop).
func (m *Manifest) GetNotes() []string {
	return stringSliceFromInterface(m.Notes)
}

// PreInstallHooks returns pre_install script lines (string or []string in Scoop).
func (m *Manifest) PreInstallHooks() []string {
	return m.PreInstallHooksForInstall("")
}

// PreInstallHooksForInstall returns pre_install for override architecture when set.
func (m *Manifest) PreInstallHooksForInstall(override string) []string {
	if block := m.archBlockForInstall(override); block != nil {
		if hooks := stringSliceFromInterface(block["pre_install"]); len(hooks) > 0 {
			return hooks
		}
	}
	return stringSliceFromInterface(m.PreInstall)
}

// InstallerScriptHooks returns installer.script lines (string or []string in Scoop).
func (m *Manifest) InstallerScriptHooks() []string {
	return m.InstallerScriptHooksForInstall("")
}

// InstallerScriptHooksForInstall returns installer.script for override architecture when set.
func (m *Manifest) InstallerScriptHooksForInstall(override string) []string {
	if hooks := stringSliceFromInterface(m.Installer.Script); len(hooks) > 0 {
		return hooks
	}
	if block := m.archBlockForInstall(override); block != nil {
		if inst, ok := block["installer"].(map[string]interface{}); ok {
			return stringSliceFromInterface(inst["script"])
		}
	}
	return nil
}

// HasInstallerScript reports whether the manifest defines installer.script.
func (m *Manifest) HasInstallerScript() bool {
	return m.HasInstallerScriptForInstall("")
}

// HasInstallerScriptForInstall reports installer.script for override architecture when set.
func (m *Manifest) HasInstallerScriptForInstall(override string) bool {
	return len(m.InstallerScriptHooksForInstall(override)) > 0
}

// PostInstallHooks returns post_install script lines (string or []string in Scoop).
func (m *Manifest) PostInstallHooks() []string {
	return m.PostInstallHooksForInstall("")
}

// PostInstallHooksForInstall returns post_install for override architecture when set.
func (m *Manifest) PostInstallHooksForInstall(override string) []string {
	if block := m.archBlockForInstall(override); block != nil {
		if hooks := stringSliceFromInterface(block["post_install"]); len(hooks) > 0 {
			return hooks
		}
	}
	return stringSliceFromInterface(m.PostInstall)
}

// PreUninstallHooks returns pre_uninstall script lines (string or []string in Scoop).
func (m *Manifest) PreUninstallHooks() []string {
	return m.PreUninstallHooksForInstall("")
}

// PreUninstallHooksForInstall returns pre_uninstall for override architecture when set.
func (m *Manifest) PreUninstallHooksForInstall(override string) []string {
	if block := m.archBlockForInstall(override); block != nil {
		if hooks := stringSliceFromInterface(block["pre_uninstall"]); len(hooks) > 0 {
			return hooks
		}
	}
	return stringSliceFromInterface(m.PreUninstall)
}

// PostUninstallHooks returns post_uninstall script lines (string or []string in Scoop).
func (m *Manifest) PostUninstallHooks() []string {
	return m.PostUninstallHooksForInstall("")
}

// PostUninstallHooksForInstall returns post_uninstall for override architecture when set.
func (m *Manifest) PostUninstallHooksForInstall(override string) []string {
	if block := m.archBlockForInstall(override); block != nil {
		if hooks := stringSliceFromInterface(block["post_uninstall"]); len(hooks) > 0 {
			return hooks
		}
	}
	return stringSliceFromInterface(m.PostUninstall)
}

// UninstallerScriptHooks returns uninstaller.script lines (string or []string in Scoop).
func (m *Manifest) UninstallerScriptHooks() []string {
	return m.UninstallerScriptHooksForInstall("")
}

// UninstallerScriptHooksForInstall returns uninstaller.script for override architecture when set.
func (m *Manifest) UninstallerScriptHooksForInstall(override string) []string {
	if m == nil {
		return nil
	}
	if block := m.archBlockForInstall(override); block != nil {
		if inst, ok := block["uninstaller"].(map[string]interface{}); ok {
			if hooks := stringSliceFromInterface(inst["script"]); len(hooks) > 0 {
				return hooks
			}
		}
	}
	return stringSliceFromInterface(m.Uninstaller.Script)
}

// ManifestSuggestion is a soft recommendation from manifest suggest (label + install ref).
type ManifestSuggestion struct {
	Label string
	Ref   string
}

// Suggestions returns suggest entries (label -> bucket/pkg ref).
func (m *Manifest) Suggestions() []ManifestSuggestion {
	if m == nil || m.Suggest == nil {
		return nil
	}
	switch val := m.Suggest.(type) {
	case map[string]interface{}:
		out := make([]ManifestSuggestion, 0, len(val))
		for label, v := range val {
			ref, ok := v.(string)
			if !ok || strings.TrimSpace(ref) == "" {
				continue
			}
			out = append(out, ManifestSuggestion{Label: label, Ref: strings.TrimSpace(ref)})
		}
		return out
	case map[string]string:
		out := make([]ManifestSuggestion, 0, len(val))
		for label, ref := range val {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			out = append(out, ManifestSuggestion{Label: label, Ref: strings.TrimSpace(ref)})
		}
		return out
	}
	return nil
}

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	if base == nil {
		base = make(map[string]string, len(overlay))
	}
	for k, v := range overlay {
		base[k] = v
	}
	return base
}

// stringMapFromInterface normalizes Scoop map fields.
func stringMapFromInterface(v interface{}) map[string]string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case map[string]string:
		return copyStringMap(val)
	case map[string]interface{}:
		out := make(map[string]string, len(val))
		for k, item := range val {
			if s, ok := item.(string); ok {
				out[k] = s
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// stringSliceFromInterface normalizes Scoop fields that may be a string or string array.
func stringSliceFromInterface(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		if val == "" {
			return nil
		}
		return []string{val}
	case []string:
		return val
	case []interface{}:
		var out []string
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// getStringOrFirst extracts string from string or []string
func getStringOrFirst(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case []interface{}:
		if len(val) > 0 {
			if s, ok := val[0].(string); ok {
				return s
			}
		}
	case []string:
		if len(val) > 0 {
			return val[0]
		}
	}
	return ""
}

// ParseFile reads and parses a manifest from a JSON file
func ParseFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	return Parse(f)
}

// Parse reads and parses a manifest from JSON
func Parse(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest JSON: %w", err)
	}

	// Validate required fields
	if m.Version == "" {
		return nil, fmt.Errorf("manifest missing required field: version")
	}

	// URL might be in architecture field, so check GetURL()
	if m.GetURL() == "" && m.Architecture == nil {
		return nil, fmt.Errorf("manifest missing required field: url")
	}

	// Hash might be in architecture field or not required for some extractors
	if m.GetHash() == "" && m.Extractor != "noproxy" && m.Architecture == nil {
		return nil, fmt.Errorf("manifest missing required field: hash")
	}

	return &m, nil
}

// Bucket represents a collection of manifests (like Scoop buckets)
type Bucket struct {
	Name    string
	Root    string
	RepoURL string // Git repository URL
}

// BucketManager indexes manifest JSON on disk for search, install resolve, and info.
// It does not run git; use core/bucket.Registry for clone/pull of bucket repositories.
//
// Deprecated: Use bucket.Registry instead. BucketManager is maintained for backward compatibility
// and will be removed in a future release. All new code should use bucket.Registry for unified
// bucket management including both git operations and manifest indexing.
type BucketManager struct {
	bucketsDir string
	buckets    map[string]*Bucket
}

// NewBucketManager creates a new bucket manager
//
// Deprecated: Use bucket.NewRegistry instead. This function is maintained for backward compatibility
// and will be removed in a future release.
func NewBucketManager(rootDir string) (*BucketManager, error) {
	bucketsDir := filepath.Join(rootDir, "buckets")
	if err := os.MkdirAll(bucketsDir, 0755); err != nil {
		return nil, fmt.Errorf("create buckets directory: %w", err)
	}

	return &BucketManager{
		bucketsDir: bucketsDir,
		buckets:    make(map[string]*Bucket),
	}, nil
}

// AddBucket adds or updates a bucket
func (bm *BucketManager) AddBucket(name, repoURL string) error {
	bucketDir := filepath.Join(bm.bucketsDir, name)

	// For now, just track it
	// In production: git clone or pull the repository
	bm.buckets[name] = &Bucket{
		Name:    name,
		Root:    bucketDir,
		RepoURL: repoURL,
	}

	return nil
}

// ReloadFromDisk clears and re-registers bucket directories under bucketsDir.
// Uses os.Stat because Windows directory junctions are not reported as dirs by fs.DirEntry.
func (bm *BucketManager) ReloadFromDisk() {
	bm.buckets = make(map[string]*Bucket)
	entries, err := os.ReadDir(bm.bucketsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(bm.bucketsDir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		_ = bm.AddBucket(entry.Name(), "")
	}
}

// IsDeprecatedMarked reports whether the manifest JSON declares the package deprecated.
func (m *Manifest) IsDeprecatedMarked() bool {
	if m == nil || m.Deprecated == nil {
		return false
	}
	switch v := m.Deprecated.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	default:
		return true
	}
}

// IsDeprecatedManifestPath reports whether manifestPath lives under bucket deprecated/.
func IsDeprecatedManifestPath(bucketRoot, manifestPath string) bool {
	rel, err := filepath.Rel(bucketRoot, manifestPath)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) >= 2 && strings.EqualFold(parts[0], "deprecated")
}

// IsDeprecatedLocation reports archived (deprecated/) or JSON-marked deprecation.
func IsDeprecatedLocation(bucketRoot, manifestPath string, m *Manifest) bool {
	if IsDeprecatedManifestPath(bucketRoot, manifestPath) {
		return true
	}
	return m != nil && m.IsDeprecatedMarked()
}

// BucketManifestCandidatePaths returns manifest file paths for a package, in search priority order.
func BucketManifestCandidatePaths(bucketRoot, bucketName, pkgName string) []string {
	return []string{
		filepath.Join(bucketRoot, pkgName+".json"),               // flat
		filepath.Join(bucketRoot, "bucket", pkgName+".json"),     // Scoop standard
		filepath.Join(bucketRoot, bucketName, pkgName+".json"),   // alternative
		filepath.Join(bucketRoot, "deprecated", pkgName+".json"), // archived (e.g. lemon bucket)
	}
}

// GetManifestPath finds the manifest file for a package
// Search order: bucket/package.json, bucket/bucket/package.json, deprecated/package.json
func (bm *BucketManager) GetManifestPath(pkgRef string) (string, *Manifest, error) {
	if at := strings.LastIndex(pkgRef, "@"); at >= 0 {
		pkgRef = pkgRef[:at]
	}
	// Parse package reference: "bucket/package" or just "package"
	var bucketName, pkgName string

	if strings.Contains(pkgRef, "/") {
		parts := strings.SplitN(pkgRef, "/", 2)
		bucketName, pkgName = parts[0], parts[1]
	} else {
		pkgName = pkgRef
		bucketName = "main" // Default bucket
	}

	bucket, exists := bm.buckets[bucketName]
	if !exists {
		return "", nil, fmt.Errorf("bucket not found: %s", bucketName)
	}

	// Scoop bucket directory structure options — see BucketManifestCandidatePaths.
	searchPaths := BucketManifestCandidatePaths(bucket.Root, bucketName, pkgName)

	for _, manifestPath := range searchPaths {
		if _, err := os.Stat(manifestPath); err == nil {
			m, err := ParseFile(manifestPath)
			if err != nil {
				return "", nil, fmt.Errorf("parse manifest: %w", err)
			}
			return manifestPath, m, nil
		}
	}

	return "", nil, fmt.Errorf("manifest not found: %s (searched in: %s)", pkgRef, bucket.Root)
}

// FindManifest locates a package manifest across all registered buckets.
// pkgName may be "pkg" or "bucket/pkg"; returns the first match.
func (bm *BucketManager) FindManifest(pkgName string) (bucketName string, m *Manifest) {
	if at := strings.LastIndex(pkgName, "@"); at >= 0 {
		pkgName = pkgName[:at]
	}
	if strings.Contains(pkgName, "/") {
		parts := strings.SplitN(pkgName, "/", 2)
		_, manifest, err := bm.GetManifestPath(pkgName)
		if err != nil {
			return "", nil
		}
		return parts[0], manifest
	}
	if _, manifest, err := bm.GetManifestPath(pkgName); err == nil {
		return "main", manifest
	}
	for name := range bm.buckets {
		if name == "main" {
			continue
		}
		_, manifest, err := bm.GetManifestPath(name + "/" + pkgName)
		if err == nil {
			return name, manifest
		}
	}
	return "", nil
}

// ListBuckets returns all registered buckets
func (bm *BucketManager) ListBuckets() []*Bucket {
	var buckets []*Bucket
	for _, b := range bm.buckets {
		buckets = append(buckets, b)
	}
	return buckets
}

// Search searches for packages across all buckets
func (bm *BucketManager) Search(query string) ([]Match, error) {
	var matches []Match

	for _, bucket := range bm.buckets {
		// Walk the bucket directory
		err := filepath.Walk(bucket.Root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
				return nil
			}

			// Parse manifest
			m, err := ParseFile(path)
			if err != nil {
				return nil // Skip invalid manifests
			}

			// Check if query matches
			pkgName := strings.TrimSuffix(filepath.Base(path), ".json")
			if strings.Contains(strings.ToLower(pkgName), strings.ToLower(query)) ||
				strings.Contains(strings.ToLower(m.Description), strings.ToLower(query)) {
				matches = append(matches, Match{
					Name:        pkgName,
					Bucket:      bucket.Name,
					Description: m.Description,
					Version:     m.Version,
					Manifest:    m,
				})
			}

			return nil
		})

		if err != nil {
			continue
		}
	}

	return matches, nil
}

// Match represents a search result
type Match struct {
	Name        string
	Bucket      string
	Description string
	Version     string
	Manifest    *Manifest
}
