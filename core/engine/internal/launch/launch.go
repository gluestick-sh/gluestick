// Package launch discovers and opens the runnable files (exe, bat, cmd, jar)
// that belong to an installed package, honoring per-launcher user preferences.
package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
)

// LaunchTarget is a runnable file the user can open.
type LaunchTarget struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	Kind  string `json:"kind,omitempty"`
}

// LaunchCandidate is a discovered executable with auto/user launch classification.
type LaunchCandidate struct {
	Label    string `json:"label"`
	Path     string `json:"path"`
	RelPath  string `json:"relPath"`
	AutoKind string `json:"autoKind"`
	Kind     string `json:"kind"`
	UserSet  bool   `json:"userSet"`
	Openable bool   `json:"openable"`
}

// ListLaunchCandidates returns all discovered executables and effective launch kinds.
func ListLaunchCandidates(e *runtime.Engine, pkgName string) ([]LaunchCandidate, error) {
	installDir, m, err := launchManifestContext(e, pkgName)
	if err != nil {
		return nil, err
	}

	entries := collectLaunchPathEntries(e, pkgName, installDir, m)
	removed := launchRemovedSet(e, pkgName)
	out := make([]LaunchCandidate, 0, len(entries))
	for _, entry := range entries {
		rel, err := filepath.Rel(installDir, entry.absPath)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		rel = filepath.ToSlash(rel)
		if _, skip := removed[strings.ToLower(rel)]; skip {
			continue
		}
		autoKind := autoLaunchKind(e, installDir, entry.absPath, m, entry.source)
		kind := autoKind
		userSet := false
		if k, ok := launchIndexKind(e, pkgName, rel); ok {
			kind = k
			userSet = true
		}
		out = append(out, LaunchCandidate{
			Label:    entry.label,
			Path:     entry.absPath,
			RelPath:  rel,
			AutoKind: string(autoKind),
			Kind:     string(kind),
			UserSet:  userSet,
			Openable: kind.openable(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label == out[j].Label {
			return out[i].Path < out[j].Path
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

// ListLaunchTargets returns executables the user can open (not marked hidden).
func ListLaunchTargets(e *runtime.Engine, pkgName string) ([]LaunchTarget, error) {
	candidates, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		return nil, err
	}
	var targets []LaunchTarget
	for _, c := range candidates {
		if !c.Openable {
			continue
		}
		targets = append(targets, LaunchTarget{
			Label: c.Label,
			Path:  c.Path,
			Kind:  c.Kind,
		})
	}
	return targets, nil
}

// OpenLaunchTarget runs an exe/bat/cmd that belongs to pkgName.
func OpenLaunchTarget(e *runtime.Engine, pkgName, absPath string) error {
	installDir, m, err := launchManifestContext(e, pkgName)
	if err != nil {
		return err
	}
	clean, err := ResolveLaunchAbsPath(e, pkgName, installDir, absPath)
	if err != nil {
		return err
	}
	if !isRunnableFile(clean) {
		return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchUnsupportedType, nil))
	}
	if _, err := os.Stat(clean); err != nil {
		return fmt.Errorf("%s: %w", message.FormatEN(message.ErrLaunchFileMissing, nil), err)
	}

	source := launchSourceForPath(installDir, clean, m)
	kind := EffectiveLaunchKind(e, pkgName, installDir, clean, m, source)
	if !kind.openable() {
		return errLaunchNotOpenable
	}
	return openLaunchFile(clean, kind)
}

// ResolveLaunchAbsPath accepts a path under the active install dir, or a stale path from
// another installed version (e.g. UI cached before version switch).
func ResolveLaunchAbsPath(e *runtime.Engine, pkgName, installDir, absPath string) (string, error) {
	clean := filepath.Clean(absPath)
	rel, err := filepath.Rel(installDir, clean)
	if err == nil && !strings.HasPrefix(rel, "..") {
		return clean, nil
	}
	if relPath := relPathWithinPackageVersion(pkgName, clean); relPath != "" {
		candidate := filepath.Join(installDir, filepath.FromSlash(relPath))
		if isRunnableFile(candidate) {
			if _, statErr := os.Stat(candidate); statErr == nil {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("%s", message.FormatEN(message.ErrInvalidLaunchPath, nil))
}

// relPathWithinPackageVersion extracts the relative path within a specific package version.
// Parses an absolute path to find the relative path within the package's version directory.
// Parameters:
//   - pkgName: Package name
//   - absPath: Absolute path to parse
// Returns the relative path, or empty string if not within the package directory.
func relPathWithinPackageVersion(pkgName, absPath string) string {
	clean := filepath.ToSlash(filepath.Clean(absPath))
	marker := "/apps/" + pkgName + "/"
	idx := strings.Index(strings.ToLower(clean), strings.ToLower(marker))
	if idx < 0 {
		return ""
	}
	rest := clean[idx+len(marker):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return ""
	}
	return rest[slash+1:]
}

// launchManifestContext retrieves the installation directory and manifest for a package.
// This is a helper function that loads the manifest from the install record
// or falls back to bucket scanning if no install record exists.
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
// Returns:
//   - installDir: Installation directory path
//   - m: Package manifest (may be nil)
//   - error: Error if installation directory cannot be determined
func launchManifestContext(e *runtime.Engine, pkgName string) (installDir string, m *manifest.Manifest, err error) {
	installDir, err = packageInstallDir(e, pkgName)
	if err != nil {
		return "", nil, err
	}
	if rec, err := apps.LoadInstallRecord(installDir); err == nil && rec.Manifest != nil {
		m = rec.Manifest
	} else if _, info := installedPackageDetails(e, pkgName, ""); info != nil {
		m = manifestFromInfo(info)
	}
	return installDir, m, nil
}

// collectLaunchPathEntries discovers all launchable files for a package.
// This function scans for executables using multiple strategies:
// 1. Manifest-declared shortcuts
// 2. Manifest-declared binaries
// 3. Directory scanning (if manifest has no declarations or declarations are missing)
// 4. Launch index overrides
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
//   - installDir: Installation directory
//   - m: Package manifest (may be nil)
// Returns a list of discovered launch entries with labels and sources.
func collectLaunchPathEntries(e *runtime.Engine, pkgName, installDir string, m *manifest.Manifest) []launchPathEntry {
	seen := make(map[string]struct{})
	var entries []launchPathEntry
	add := func(absPath, label string, source LaunchSource) {
		absPath = filepath.Clean(absPath)
		if !isRunnableFile(absPath) {
			return
		}
		if _, err := os.Stat(absPath); err != nil {
			return
		}
		if _, ok := seen[absPath]; ok {
			return
		}
		seen[absPath] = struct{}{}
		if label == "" {
			label = launcherDisplayName(absPath)
		}
		entries = append(entries, launchPathEntry{
			absPath: absPath,
			label:   label,
			source:  source,
		})
	}

	manifestDeclared := m != nil && m.HasLaunchDefinitions()
	manifestResolved := 0
	if m != nil {
		for _, sc := range m.LaunchShortcuts() {
			target := shortcutTargetPath(installDir, sc.Target)
			if !isRunnableFile(target) {
				continue
			}
			before := len(entries)
			add(target, shortcutLabel(sc, target), LaunchSourceShortcut)
			manifestResolved += len(entries) - before
		}
		for _, binPattern := range m.LaunchBinaries() {
			binName, alias := install.ParseBinPattern(binPattern)
			if binName == "" {
				continue
			}
			for _, candidate := range install.ResolveBinCandidates(installDir, binName, "") {
				before := len(entries)
				add(candidate, alias, LaunchSourceBin)
				manifestResolved += len(entries) - before
			}
		}
	}

	// No bin/shortcuts in manifest, or declared paths missing on disk — scan install dir.
	if !manifestDeclared || manifestResolved == 0 {
		scanLaunchPathEntries(e, installDir, installDir, 0, 4, seen, &entries)
	}
	appendLaunchIndexEntries(e, pkgName, installDir, add)
	return entries
}

// appendLaunchIndexEntries adds launchers from the launch index to the entry list.
// This ensures that user-customized launchers (from launch-index.json) are included
// even if they wouldn't be discovered by normal scanning.
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
//   - installDir: Installation directory
//   - add: Callback function to add entries
func appendLaunchIndexEntries(e *runtime.Engine,
	pkgName, installDir string,
	add func(absPath, label string, source LaunchSource),
) {
	if e == nil || e.Config == nil || strings.TrimSpace(pkgName) == "" {
		return
	}
	e.LaunchIndexMu.Lock()
	idx, err := loadLaunchIndex(launchIndexPath(e.Config.RootDir))
	e.LaunchIndexMu.Unlock()
	if err != nil || idx == nil {
		return
	}
	relKinds, ok := idx.Packages[pkgName]
	if !ok {
		return
	}
	for rel := range relKinds {
		rel = normalizeLaunchRelPath(rel)
		if rel == "" {
			continue
		}
		add(filepath.Join(installDir, filepath.FromSlash(rel)), "", LaunchSourceScan)
	}
}

// scanLaunchPathEntries recursively scans a directory for runnable files.
// This performs a depth-limited scan to find executables that can be launched.
// Parameters:
//   - e: Runtime engine
//   - dir: Directory to scan
//   - installDir: Root installation directory (for path calculations)
//   - depth: Current depth in the directory tree
//   - maxDepth: Maximum depth to scan (0 = unlimited)
//   - seen: Set of already-seen paths (to avoid duplicates)
//   - entries: Slice to append discovered entries to
func scanLaunchPathEntries(e *runtime.Engine,
	dir, installDir string,
	depth, maxDepth int,
	seen map[string]struct{},
	entries *[]launchPathEntry,
) {
	if depth > maxDepth {
		return
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range list {
		name := entry.Name()
		if entry.IsDir() {
			if ShouldSkipLaunchDir(name) {
				continue
			}
			scanLaunchPathEntries(e, filepath.Join(dir, name), installDir, depth+1, maxDepth, seen, entries)
			continue
		}
		path := filepath.Clean(filepath.Join(dir, name))
		if !isRunnableFile(path) {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		*entries = append(*entries, launchPathEntry{
			absPath: path,
			label:   launcherDisplayName(path),
			source:  LaunchSourceScan,
		})
	}
}

// PackageIconPath returns the executable whose embedded icon should represent the package.
// Prefers shortcut targets, then manifest bins, then scanned files; GUI exe wins within each tier.
func PackageIconPath(e *runtime.Engine, pkgName string) (string, error) {
	installDir, m, err := launchManifestContext(e, pkgName)
	if err != nil {
		return "", err
	}
	entries := collectLaunchPathEntries(e, pkgName, installDir, m)
	if len(entries) == 0 {
		return "", nil
	}

	pick := func(guiOnly bool) string {
		for _, src := range []LaunchSource{LaunchSourceShortcut, LaunchSourceBin, LaunchSourceScan} {
			for _, entry := range entries {
				if entry.source != src {
					continue
				}
				lower := strings.ToLower(entry.absPath)
				if !strings.HasSuffix(lower, ".exe") {
					continue
				}
				if guiOnly && autoLaunchKind(e, installDir, entry.absPath, m, entry.source) != LaunchKindGUI {
					continue
				}
				return entry.absPath
			}
		}
		return ""
	}

	if path := pick(true); path != "" {
		return path, nil
	}
	return pick(false), nil
}

// PackageInstallDir returns the active install directory for pkgName.
func PackageInstallDir(e *runtime.Engine, pkgName string) (string, error) {
	return packageInstallDir(e, pkgName)
}

// packageInstallDir returns the active installation directory for a package.
// This is a helper function that resolves the current version and returns
// the full path to the active installation directory.
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
// Returns:
//   - Full path to the active installation directory
//   - Error if no active version exists
func packageInstallDir(e *runtime.Engine, pkgName string) (string, error) {
	pkgRoot, version, err := apps.CurrentInstalledPath(e.Config.RootDir, pkgName)
	if err != nil {
		return "", err
	}
	if version == "" {
		return "", fmt.Errorf("package %s has no active version", pkgName)
	}
	return filepath.Join(pkgRoot, version), nil
}

// installedPackageDetails retrieves package details from install records or buckets.
// This helper searches for package information in multiple locations:
// 1. Install record (if present)
// 2. Bucket manifests (fallback)
// Parameters:
//   - e: Runtime engine
//   - name: Package name
//   - version: Package version (empty = use active version)
// Returns:
//   - bucket: Bucket name where the package was found
//   - info: Package manifest information (may be nil)
func installedPackageDetails(e *runtime.Engine, name, version string) (bucket string, info *etypes.ManifestInfo) {
	if e == nil || e.Config == nil {
		return "", nil
	}
	pkgRoot := apps.PkgRoot(e.Config.RootDir, name)
	ver := version
	if ver == "" {
		if v, err := apps.ReadCurrent(pkgRoot); err == nil {
			ver = v
		}
	}
	if ver != "" {
		installDir := filepath.Join(pkgRoot, ver)
		if rec, err := apps.LoadInstallRecord(installDir); err == nil {
			if rec.Bucket != "" {
				bucket = rec.Bucket
			}
			if rec.Manifest != nil {
				info = manifestInfoFromManifest(rec.Manifest)
			}
			if bucket != "" {
				return bucket, info
			}
		}
	}

	bucketsDir := filepath.Join(e.Config.RootDir, "buckets")
	entries, err := os.ReadDir(bucketsDir)
	if err != nil {
		return "", info
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		bucketName := entry.Name()
		manifestPath := filepath.Join(bucketsDir, bucketName, "bucket", name+".json")
		if _, err := os.Stat(manifestPath); err != nil {
			continue
		}
		m, err := manifest.ParseFile(manifestPath)
		if err != nil {
			continue
		}
		return bucketName, manifestInfoFromManifest(m)
	}
	return "", info
}

// manifestInfoFromManifest converts a manifest to ManifestInfo format.
// This helper extracts key information from a manifest for display or
// API responses.
// Parameters:
//   - m: Package manifest
// Returns a ManifestInfo structure with the extracted data.
func manifestInfoFromManifest(m *manifest.Manifest) *etypes.ManifestInfo {
	if m == nil {
		return nil
	}
	return &etypes.ManifestInfo{
		URL:          m.GetURL(),
		Hash:         hashString(m.GetHashes()),
		ExtractDir:   m.GetExtractDir(),
		Binaries:     binariesFromStrings(m.Binaries()),
		EnvPath:      envAddPath(m),
		Architecture: m.SelectedArchitecture(),
		Depends:      m.Depends,
		PostInstall:  strings.Join(m.PostInstallHooks(), "\n"),
		Description:  m.Description,
		Homepage:     m.Homepage,
	}
}

// hashString extracts a single hash value from a list of hashes.
// Prefers SHA-256 or SHA-512 if available, otherwise returns the first hash.
// Parameters:
//   - hashes: List of hash strings (may have algorithm prefixes)
// Returns a hash value without algorithm prefix.
func hashString(hashes []string) string {
	if len(hashes) == 0 {
		return ""
	}
	for _, h := range hashes {
		if strings.HasPrefix(h, "sha256:") {
			return strings.TrimPrefix(h, "sha256:")
		}
		if strings.HasPrefix(h, "sha512:") {
			return strings.TrimPrefix(h, "sha512:")
		}
	}
	return hashes[0]
}

// binariesFromStrings converts bin pattern strings to BinaryInfo structures.
// This helper transforms the simple bin strings into the structured
// BinaryInfo format used by the API.
// Parameters:
//   - bins: List of bin pattern strings
// Returns a list of BinaryInfo structures.
func binariesFromStrings(bins []string) []etypes.BinaryInfo {
	out := make([]etypes.BinaryInfo, 0, len(bins))
	for _, b := range bins {
		out = append(out, etypes.BinaryInfo{Source: b})
	}
	return out
}

// envAddPath extracts env.add_path entries from a manifest.
// This helper normalizes the various forms of env.add_path (string,
// array, or empty) into a consistent string array format.
// Parameters:
//   - m: Package manifest
// Returns a list of paths to add to PATH, or nil if none specified.
func envAddPath(m *manifest.Manifest) []string {
	if m == nil {
		return nil
	}
	switch path := m.EnvAddPath.(type) {
	case string:
		if path == "" {
			return nil
		}
		return []string{path}
	case []interface{}:
		var out []string
		for _, item := range path {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return path
	default:
		return nil
	}
}

// ShouldSkipLaunchDir reports directories that must not be scanned for launchers.
func ShouldSkipLaunchDir(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "node_modules", ".git", "__pycache__":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

// isRunnableFile checks if a file is executable and can be launched.
// This identifies file types that the launcher knows how to run.
// Parameters:
//   - path: File path to check
// Returns true if the file is a runnable type.
func isRunnableFile(path string) bool {
	lower := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(lower, ".exe") ||
		strings.HasSuffix(lower, ".bat") ||
		strings.HasSuffix(lower, ".cmd") ||
		strings.HasSuffix(lower, ".jar")
}

// launcherDisplayName extracts a display name from a launcher path.
// This removes file extensions (.exe, .bat, .cmd, .jar) to create
// a user-friendly label for the launcher.
// Parameters:
//   - path: File path
// Returns the display name without extension.
func launcherDisplayName(path string) string {
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	for _, ext := range []string{".exe", ".bat", ".cmd", ".jar"} {
		if strings.HasSuffix(lower, ext) {
			return base[:len(base)-len(ext)]
		}
	}
	return base
}

// manifestFromInfo reconstructs a minimal manifest from ManifestInfo.
// This helper is used when display information needs to be converted
// back to a manifest format (e.g., for launch detection).
// Parameters:
//   - info: Package manifest information
// Returns a minimal manifest with the provided information.
func manifestFromInfo(info *etypes.ManifestInfo) *manifest.Manifest {
	if info == nil {
		return nil
	}
	m := &manifest.Manifest{
		Description: info.Description,
		Homepage:    info.Homepage,
	}
	if len(info.Binaries) > 0 {
		var bins []string
		for _, b := range info.Binaries {
			if b.Source == "" {
				continue
			}
			if b.Alias != "" {
				bins = append(bins, "["+b.Source+","+b.Alias+"]")
			} else {
				bins = append(bins, b.Source)
			}
		}
		m.Bin = bins
	}
	return m
}
