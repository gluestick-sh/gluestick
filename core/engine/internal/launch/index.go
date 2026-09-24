package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	eruntime "github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/message"
)

// launchIndexFile stores user launch preferences at ~/.glue/launch-index.json.
type launchIndexFile struct {
	Packages map[string]map[string]string `json:"packages"`
	Removed  map[string][]string          `json:"removed,omitempty"`
}

// LaunchIndexPath returns the path to launch-index.json under rootDir.
func LaunchIndexPath(rootDir string) string {
	return launchIndexPath(rootDir)
}

// launchIndexPath returns the path to launch-index.json under rootDir.
// This file stores user launch preferences for packages.
// Parameters:
//   - rootDir: Root directory of the installation
//
// Returns the full path to launch-index.json.
func launchIndexPath(rootDir string) string {
	return filepath.Join(rootDir, "launch-index.json")
}

// LaunchIndexKind returns the user override for a launcher, if any.
func LaunchIndexKind(e *eruntime.Engine, pkgName, relPath string) (LaunchKind, bool) {
	return launchIndexKind(e, pkgName, relPath)
}

// launchIndexKind retrieves the user override for a launcher, if any.
// This is the internal implementation that reads the launch index.
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
//   - relPath: Relative path to the launcher
//
// Returns:
//   - kind: Launch kind (gui, console, skip) if override exists
//   - true if an override was found, false otherwise
func launchIndexKind(e *eruntime.Engine, pkgName, relPath string) (LaunchKind, bool) {
	if e == nil || e.Config == nil {
		return "", false
	}
	e.LaunchIndexMu.Lock()
	defer e.LaunchIndexMu.Unlock()
	idx, err := loadLaunchIndex(launchIndexPath(e.Config.RootDir))
	if err != nil || idx == nil {
		return "", false
	}
	return idx.kind(pkgName, relPath)
}

// SetLaunchPreference sets or clears the launch override for a single launcher.
// A kind of "auto" or empty clears any existing override.
func SetLaunchPreference(e *eruntime.Engine, pkgName, relPath, kind string) error {
	parsed, err := parseLaunchKindInput(kind)
	if err != nil {
		return err
	}
	relPath = normalizeLaunchRelPath(relPath)
	if relPath == "" {
		return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchInvalidRelPath, nil))
	}
	updates := map[string]string{relPath: kind}
	if parsed == "" {
		updates[relPath] = "auto"
	}
	return setLaunchPreferences(e, pkgName, updates)
}

// SetLaunchPreferences applies multiple launch preference updates in one atomic write.
// Kind values: gui, console, skip, or auto (clear override).
func SetLaunchPreferences(e *eruntime.Engine, pkgName string, updates map[string]string) error {
	if len(updates) == 0 {
		return nil
	}
	return setLaunchPreferences(e, pkgName, updates)
}

// RemoveLaunchEntry drops a discovered launcher from the open-program menu.
func RemoveLaunchEntry(e *eruntime.Engine, pkgName, relPath string) error {
	if e == nil || e.Config == nil {
		return fmt.Errorf("engine not initialized")
	}
	rel := normalizeLaunchRelPath(relPath)
	if rel == "" || strings.TrimSpace(pkgName) == "" {
		return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchInvalidRelPath, nil))
	}

	e.LaunchIndexMu.Lock()
	defer e.LaunchIndexMu.Unlock()

	path := launchIndexPath(e.Config.RootDir)
	idx, err := loadLaunchIndex(path)
	if err != nil {
		return err
	}
	if idx == nil {
		idx = &launchIndexFile{Packages: map[string]map[string]string{}}
	}
	idx.markRemoved(pkgName, rel)
	idx.deletePackageKind(pkgName, rel)
	if len(idx.Removed[pkgName]) == 0 {
		delete(idx.Removed, pkgName)
	}
	if len(idx.Packages[pkgName]) == 0 {
		delete(idx.Packages, pkgName)
	}
	return saveLaunchIndex(path, idx)
}

// AddLaunchEntry restores a launcher to the open-program menu.
func AddLaunchEntry(e *eruntime.Engine, pkgName, relPath, kind string) error {
	if e == nil || e.Config == nil {
		return fmt.Errorf("engine not initialized")
	}
	rel := normalizeLaunchRelPath(relPath)
	if rel == "" || strings.TrimSpace(pkgName) == "" {
		return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchInvalidRelPath, nil))
	}
	parsed, err := parseLaunchKindInput(kind)
	if err != nil {
		return err
	}
	if parsed == "" {
		parsed = LaunchKindGUI
	}

	installDir, err := packageInstallDir(e, pkgName)
	if err != nil {
		return err
	}
	abs := filepath.Join(installDir, filepath.FromSlash(rel))
	if !isRunnableFile(abs) {
		return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchUnsupportedType, nil))
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("%s: %w", message.FormatEN(message.ErrLaunchFileMissing, nil), err)
	}

	e.LaunchIndexMu.Lock()
	defer e.LaunchIndexMu.Unlock()

	path := launchIndexPath(e.Config.RootDir)
	idx, err := loadLaunchIndex(path)
	if err != nil {
		return err
	}
	if idx == nil {
		idx = &launchIndexFile{Packages: map[string]map[string]string{}}
	}
	idx.unmarkRemoved(pkgName, rel)
	if idx.Packages[pkgName] == nil {
		idx.Packages[pkgName] = map[string]string{}
	}
	idx.deletePackageKind(pkgName, rel)
	idx.Packages[pkgName][rel] = string(parsed)
	return saveLaunchIndex(path, idx)
}

// setLaunchPreferences applies multiple launch preference updates in one atomic write.
// This is the internal implementation that updates the launch index file.
// Kind values: gui, console, skip, or auto (clear override).
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
//   - updates: Map of relative paths to launch kinds
//
// Returns error if the update fails.
func setLaunchPreferences(e *eruntime.Engine, pkgName string, updates map[string]string) error {
	if e == nil || e.Config == nil {
		return fmt.Errorf("engine not initialized")
	}
	if strings.TrimSpace(pkgName) == "" {
		return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchInvalidRelPath, nil))
	}

	normalized := make(map[string]string, len(updates))
	for relPath, kind := range updates {
		rel := normalizeLaunchRelPath(relPath)
		if rel == "" {
			return fmt.Errorf("%s", message.FormatEN(message.ErrLaunchInvalidRelPath, nil))
		}
		parsed, err := parseLaunchKindInput(kind)
		if err != nil {
			return err
		}
		if parsed == "" {
			normalized[rel] = ""
		} else {
			normalized[rel] = string(parsed)
		}
	}

	e.LaunchIndexMu.Lock()
	defer e.LaunchIndexMu.Unlock()

	path := launchIndexPath(e.Config.RootDir)
	idx, err := loadLaunchIndex(path)
	if err != nil {
		return err
	}
	if idx == nil {
		idx = &launchIndexFile{Packages: map[string]map[string]string{}}
	}
	if idx.Packages[pkgName] == nil {
		idx.Packages[pkgName] = map[string]string{}
	}
	for rel, kind := range normalized {
		if kind == "" {
			idx.deletePackageKind(pkgName, rel)
		} else {
			idx.deletePackageKind(pkgName, rel)
			idx.Packages[pkgName][rel] = kind
		}
	}
	if len(idx.Packages[pkgName]) == 0 {
		delete(idx.Packages, pkgName)
	}
	return saveLaunchIndex(path, idx)
}

// parseLaunchKindInput validates and parses a launch kind string.
// Accepts: gui, console, skip, auto (or empty string).
// Parameters:
//   - kind: Launch kind string to parse
//
// Returns:
//   - Parsed launch kind (or empty for auto)
//   - Error if the kind is invalid
func parseLaunchKindInput(kind string) (LaunchKind, error) {
	switch LaunchKind(strings.ToLower(strings.TrimSpace(kind))) {
	case LaunchKindConsole, LaunchKindGUI, LaunchKindSkip:
		return LaunchKind(strings.ToLower(strings.TrimSpace(kind))), nil
	case "", "auto":
		return "", nil
	default:
		return "", fmt.Errorf("%s", message.FormatEN(message.ErrLaunchInvalidKind, map[string]any{
			"kind": kind,
		}))
	}
}

// normalizeLaunchRelPath normalizes a relative path for use in the launch index.
// Removes leading "./" and converts to forward slashes.
// Parameters:
//   - relPath: Relative path to normalize
//
// Returns the normalized path.
func normalizeLaunchRelPath(relPath string) string {
	relPath = strings.TrimSpace(relPath)
	relPath = filepath.ToSlash(relPath)
	return strings.TrimPrefix(relPath, "./")
}

// launchRemovedSet retrieves the set of removed launchers for a package.
// This is used to filter out launchers that the user has explicitly hidden.
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name
//
// Returns a set of relative paths (lowercase) that have been removed.
func launchRemovedSet(e *eruntime.Engine, pkgName string) map[string]struct{} {
	out := make(map[string]struct{})
	if e == nil || e.Config == nil {
		return out
	}
	e.LaunchIndexMu.Lock()
	defer e.LaunchIndexMu.Unlock()
	idx, err := loadLaunchIndex(launchIndexPath(e.Config.RootDir))
	if err != nil || idx == nil {
		return out
	}
	for _, rel := range idx.Removed[pkgName] {
		norm := strings.ToLower(normalizeLaunchRelPath(rel))
		if norm != "" {
			out[norm] = struct{}{}
		}
	}
	return out
}

// markRemoved marks a launcher as removed in the launch index.
// Parameters:
//   - idx: Launch index to update
//   - pkgName: Package name
//   - relPath: Relative path to the launcher
func (idx *launchIndexFile) markRemoved(pkgName, relPath string) {
	if idx.Removed == nil {
		idx.Removed = map[string][]string{}
	}
	if idx.isRemoved(pkgName, relPath) {
		return
	}
	idx.Removed[pkgName] = append(idx.Removed[pkgName], normalizeLaunchRelPath(relPath))
}

// unmarkRemoved removes the removed mark from a launcher.
// This restores a launcher to the visible set.
// Parameters:
//   - idx: Launch index to update
//   - pkgName: Package name
//   - relPath: Relative path to the launcher
func (idx *launchIndexFile) unmarkRemoved(pkgName, relPath string) {
	paths, ok := idx.Removed[pkgName]
	if !ok {
		return
	}
	want := strings.ToLower(normalizeLaunchRelPath(relPath))
	filtered := paths[:0]
	for _, rel := range paths {
		if strings.ToLower(normalizeLaunchRelPath(rel)) == want {
			continue
		}
		filtered = append(filtered, rel)
	}
	if len(filtered) == 0 {
		delete(idx.Removed, pkgName)
		return
	}
	idx.Removed[pkgName] = filtered
}

// isRemoved checks if a launcher is marked as removed.
// Parameters:
//   - idx: Launch index to check
//   - pkgName: Package name
//   - relPath: Relative path to the launcher
//
// Returns true if the launcher is marked as removed.
func (idx *launchIndexFile) isRemoved(pkgName, relPath string) bool {
	want := strings.ToLower(normalizeLaunchRelPath(relPath))
	for _, rel := range idx.Removed[pkgName] {
		if strings.ToLower(normalizeLaunchRelPath(rel)) == want {
			return true
		}
	}
	return false
}

// deletePackageKind removes a launch kind override for a specific launcher.
// Parameters:
//   - idx: Launch index to update
//   - pkgName: Package name
//   - relPath: Relative path to the launcher
func (idx *launchIndexFile) deletePackageKind(pkgName, relPath string) {
	entries, ok := idx.Packages[pkgName]
	if !ok {
		return
	}
	want := strings.ToLower(normalizeLaunchRelPath(relPath))
	for path := range entries {
		if strings.ToLower(normalizeLaunchRelPath(path)) == want {
			delete(entries, path)
		}
	}
}

// launchKindPriority returns the priority of a launch kind.
// Higher priority kinds override lower priority kinds when resolving
// multiple overrides for the same launcher.
// Parameters:
//   - kind: Launch kind
//
// Returns priority value (2=gui/console, 1=skip, 0=unknown).
func launchKindPriority(kind LaunchKind) int {
	switch kind {
	case LaunchKindGUI, LaunchKindConsole:
		return 2
	case LaunchKindSkip:
		return 1
	default:
		return 0
	}
}

// kind retrieves the effective launch kind for a launcher.
// Handles multiple overrides for the same path by selecting the
// highest priority kind.
// Parameters:
//   - idx: Launch index to query
//   - pkgName: Package name
//   - relPath: Relative path to the launcher
//
// Returns:
//   - kind: Effective launch kind
//   - true if an override was found
func (idx *launchIndexFile) kind(pkgName, relPath string) (LaunchKind, bool) {
	if idx == nil {
		return "", false
	}
	entries, ok := idx.Packages[pkgName]
	if !ok {
		return "", false
	}
	want := strings.ToLower(normalizeLaunchRelPath(relPath))
	var best LaunchKind
	bestPri := 0
	found := false
	for path, kindStr := range entries {
		if strings.ToLower(normalizeLaunchRelPath(path)) != want {
			continue
		}
		kind := LaunchKind(strings.ToLower(strings.TrimSpace(kindStr)))
		switch kind {
		case LaunchKindConsole, LaunchKindGUI, LaunchKindSkip:
			if pri := launchKindPriority(kind); pri >= bestPri {
				bestPri = pri
				best = kind
				found = true
			}
		}
	}
	return best, found
}

// loadLaunchIndex reads the launch-index.json file from disk.
// Creates a default empty index if the file doesn't exist.
// Back up and reset the file if it's corrupted.
// Parameters:
//   - path: Path to launch-index.json
//
// Returns:
//   - Loaded launch index (or default empty index)
//   - Error if reading fails (not an error if file doesn't exist)
func loadLaunchIndex(path string) (*launchIndexFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var idx launchIndexFile
	if err := json.Unmarshal(data, &idx); err != nil {
		backup := path + ".bak"
		_ = os.Remove(backup)
		_ = os.Rename(path, backup)
		return &launchIndexFile{Packages: map[string]map[string]string{}}, nil
	}
	if idx.Packages == nil {
		idx.Packages = map[string]map[string]string{}
	}
	if idx.Removed == nil {
		idx.Removed = map[string][]string{}
	}
	idx.dedupePackageKinds()
	return &idx, nil
}

// dedupePackageKinds removes duplicate entries and selects highest priority kinds.
// This cleans up the index by removing redundant entries and ensuring
// each launcher has only one effective kind (the highest priority).
func (idx *launchIndexFile) dedupePackageKinds() {
	if idx == nil {
		return
	}
	for pkgName, entries := range idx.Packages {
		type slot struct {
			key  string
			kind LaunchKind
			pri  int
		}
		byNorm := map[string]slot{}
		for rel, kindStr := range entries {
			norm := strings.ToLower(normalizeLaunchRelPath(rel))
			if norm == "" {
				continue
			}
			kind := LaunchKind(strings.ToLower(strings.TrimSpace(kindStr)))
			switch kind {
			case LaunchKindConsole, LaunchKindGUI, LaunchKindSkip:
			default:
				continue
			}
			pri := launchKindPriority(kind)
			cur, ok := byNorm[norm]
			if !ok || pri > cur.pri || (pri == cur.pri && rel < cur.key) {
				byNorm[norm] = slot{key: rel, kind: kind, pri: pri}
			}
		}
		clean := make(map[string]string, len(byNorm))
		for _, s := range byNorm {
			clean[s.key] = string(s.kind)
		}
		if len(clean) == 0 {
			delete(idx.Packages, pkgName)
		} else {
			idx.Packages[pkgName] = clean
		}
	}
}

// saveLaunchIndex writes the launch index to disk atomically.
// Writes to a temporary file first, then renames to prevent corruption.
// Parameters:
//   - path: Path to launch-index.json
//   - idx: Launch index to save (may be nil, creates empty index)
//
// Returns error if writing fails.
func saveLaunchIndex(path string, idx *launchIndexFile) error {
	if idx == nil {
		idx = &launchIndexFile{Packages: map[string]map[string]string{}}
	}
	if idx.Packages == nil {
		idx.Packages = map[string]map[string]string{}
	}
	if idx.Removed == nil {
		idx.Removed = map[string][]string{}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
