package apps

import (
	"os"
	"path/filepath"
)

// ListVersions returns installed version directory names, excluding current.
func ListVersions(pkgRoot string) ([]string, error) {
	entries, err := os.ReadDir(pkgRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var versions []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == CurrentLinkName {
			continue
		}
		verDir := filepath.Join(pkgRoot, entry.Name())
		if dirIsEmpty(verDir) {
			continue
		}
		versions = append(versions, entry.Name())
	}
	return versions, nil
}

// ActiveInstallDir returns the install directory and version to scan for on-disk refs.
func ActiveInstallDir(pkgRoot string) (installDir, version string, ok bool) {
	if ver, err := ReadCurrent(pkgRoot); err == nil && ver != "" {
		dir := filepath.Join(pkgRoot, ver)
		if st, err := os.Stat(dir); err == nil && st.IsDir() && !dirIsEmpty(dir) {
			return dir, ver, true
		}
	}
	ver, okPick := PickDefaultVersion(pkgRoot)
	if !okPick {
		return "", "", false
	}
	return filepath.Join(pkgRoot, ver), ver, true
}

// PickDefaultVersion chooses the best version when no current link exists (migration).
func PickDefaultVersion(pkgRoot string) (string, bool) {
	versions, err := ListVersions(pkgRoot)
	if err != nil || len(versions) == 0 {
		return "", false
	}
	best := versions[0]
	for _, v := range versions[1:] {
		if v > best {
			best = v
		}
	}
	return best, true
}

// EnsureCurrent creates the current link when missing (lazy migration).
func EnsureCurrent(pkgRoot string) (version string, err error) {
	if ver, err := ReadCurrent(pkgRoot); err == nil && ver != "" {
		if _, statErr := os.Stat(filepath.Join(pkgRoot, ver)); statErr == nil {
			return ver, nil
		}
	}
	ver, ok := PickDefaultVersion(pkgRoot)
	if !ok {
		return "", nil
	}
	if err := LinkCurrent(pkgRoot, ver); err != nil {
		return "", err
	}
	return ver, nil
}

// dirIsEmpty checks if a directory is empty or doesn't exist.
func dirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err != nil || len(entries) == 0
}
