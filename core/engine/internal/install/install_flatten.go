package install

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// isScoopMoveFlattenPreInstall detects Scoop-style "mv */* ." archive flatten hooks.
// This identifies hooks that move all files from a subdirectory to the root,
// which is a common pattern for flattening nested archive structures.
// Parameters:
//   - hooks: List of pre-install hook commands
// Returns true if a flatten hook is detected.
func isScoopMoveFlattenPreInstall(hooks []string) bool {
	for _, hook := range hooks {
		h := strings.ToLower(strings.Join(strings.Fields(hook), " "))
		if strings.Contains(h, "mv */*") || strings.Contains(h, "mv *\\*") {
			return true
		}
	}
	return false
}

// standardInstallSubdirs are layout folders that must not be hoisted by mv */* flatten.
var standardInstallSubdirs = map[string]bool{
	"bin": true, "lib": true, "libs": true, "share": true, "doc": true, "docs": true,
	"man": true, "etc": true, "mod": true, "modules": true, "plugins": true,
}

// installNeedsMoveFlatten reports whether installDir has a single archive wrapper to hoist.
// Detects if the install directory contains only a single subdirectory with files,
// indicating the archive should be flattened (files moved up one level).
// Excludes standard layout directories (bin, lib, share, etc.) from hoisting.
// Parameters:
//   - installDir: Installation directory to check
// Returns true if flattening is needed.
func installNeedsMoveFlatten(installDir string) bool {
	entries, err := os.ReadDir(installDir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return false
	}
	name := strings.ToLower(entries[0].Name())
	if standardInstallSubdirs[name] {
		return false
	}
	sub, err := os.ReadDir(filepath.Join(installDir, entries[0].Name()))
	return err == nil && len(sub) > 0
}

// ensureScoopMoveFlattenInstallDir runs native "mv */* ." while a wrapper directory remains.
// This function implements Scoop-style archive flattening by moving all files
// from a single subdirectory to the parent directory, then removing the wrapper.
// It uses native rename operations (no re-hashing) for efficiency.
// Parameters:
//   - installDir: Installation directory to flatten
//   - installedFiles: Map of content hashes to paths (updated with new paths)
// Returns error if directory operations fail.
func ensureScoopMoveFlattenInstallDir(installDir string, installedFiles map[string]string) error {
	for installNeedsMoveFlatten(installDir) {
		stripPrefix, err := scoopMoveFlattenInstallDir(installDir)
		if err != nil {
			return err
		}
		if stripPrefix == "" {
			break
		}
		remapInstalledFilePaths(installedFiles, stripPrefix)
	}
	return nil
}

// scoopMoveFlattenInstallDir implements "mv */* ." natively (same-volume rename, no re-hash).
// Performs the actual flattening by moving files from the wrapper directory
// to the install directory using OS rename operations. This is much faster
// than copying and doesn't require re-hashing.
// Parameters:
//   - installDir: Installation directory to flatten
// Returns:
//   - stripPrefix: The prefix that was stripped from file paths (e.g., "wrapper/")
//   - error: Any error during directory operations
func scoopMoveFlattenInstallDir(installDir string) (stripPrefix string, err error) {
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", nil
	}
	wrapper := entries[0].Name()
	wrapperPath := filepath.Join(installDir, wrapper)
	subEntries, err := os.ReadDir(wrapperPath)
	if err != nil {
		return "", err
	}
	for _, entry := range subEntries {
		src := filepath.Join(wrapperPath, entry.Name())
		dst := filepath.Join(installDir, entry.Name())
		if err := os.Rename(src, dst); err != nil {
			return "", fmt.Errorf("flatten %s: %w", entry.Name(), err)
		}
	}
	if err := os.Remove(wrapperPath); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	after, err := os.ReadDir(installDir)
	if err != nil {
		return "", err
	}
	for _, entry := range after {
		name := entry.Name()
		if strings.HasPrefix(strings.ToLower(name), "freecad_") {
			path := filepath.Join(installDir, name)
			_ = os.RemoveAll(path)
		}
	}
	return wrapper + "/", nil
}

// postInstallNeedsFileIndexRefresh reports whether post_install hooks may add or move files.
// Certain hook operations (Expand-Archive, copy-item, move-item, reg add, etc.)
// can modify the install directory after initial extraction. This function detects
// those cases so the file index can be refreshed after hooks run.
// Parameters:
//   - hooks: List of post-install hook commands
// Returns true if file index refresh is needed after hooks.
func postInstallNeedsFileIndexRefresh(hooks []string) bool {
	for _, hook := range hooks {
		h := strings.ToLower(hook)
		switch {
		case strings.Contains(h, "set-content"),
			strings.Contains(h, "copy-item"),
			strings.Contains(h, "move-item"),
			strings.Contains(h, "new-item"),
			strings.Contains(h, "expand-"),
			strings.Contains(h, "reg add"),
			strings.Contains(h, "install-helper"):
			return true
		}
	}
	return false
}

// remapInstalledFilePaths updates install-relative paths after a top-level directory strip.
// When flattening moves files up one level, the file paths in the installedFiles map
// need to be updated to reflect the new locations.
// Parameters:
//   - files: Map of content hash -> relative path (updated in place)
//   - stripPrefix: Prefix to strip from each path (e.g., "wrapper/")
func remapInstalledFilePaths(files map[string]string, stripPrefix string) {
	stripPrefix = filepath.ToSlash(strings.Trim(stripPrefix, `/\`))
	if stripPrefix != "" {
		stripPrefix += "/"
	}
	for hash, rel := range files {
		rel = filepath.ToSlash(rel)
		if stripPrefix != "" && strings.HasPrefix(rel, stripPrefix) {
			files[hash] = strings.TrimPrefix(rel, stripPrefix)
		}
	}
}

var hookQuotedExePath = regexp.MustCompile(`"(\$(?:original_dir|dir))\\([^"]+\.exe)"`)

// patchInstallHookPaths rewrites hook targets to resolved on-disk paths when the
// manifest-relative path is missing (e.g. archive still nested under one directory).
// This handles cases where the archive layout doesn't match the manifest expectations,
// such as when files are in a subdirectory that the hook doesn't account for.
// Parameters:
//   - installDir: Installation directory to search
//   - hooks: List of hook commands to patch
// Returns patched hook commands with corrected paths.
func patchInstallHookPaths(installDir string, hooks []string) []string {
	if len(hooks) == 0 {
		return hooks
	}
	out := make([]string, len(hooks))
	for i, hook := range hooks {
		out[i] = hookQuotedExePath.ReplaceAllStringFunc(hook, func(match string) string {
			sub := hookQuotedExePath.FindStringSubmatch(match)
			if len(sub) != 3 {
				return match
			}
			rel := filepath.FromSlash(strings.ReplaceAll(sub[2], `\`, `/`))
			candidate := filepath.Join(installDir, rel)
			if _, err := os.Stat(candidate); err == nil {
				return quoteHookPath(candidate)
			}
			if found, ok := findFileUnderInstall(installDir, filepath.Base(rel), 8); ok {
				return quoteHookPath(found)
			}
			return match
		})
	}
	return out
}

// quoteHookPath wraps a file path in double quotes for use in installer hooks.
// This ensures paths with spaces are properly quoted in shell commands.
// Parameters:
//   - path: File path to quote
// Returns the quoted path with internal quotes escaped.
func quoteHookPath(path string) string {
	return `"` + strings.ReplaceAll(path, `"`, `\"`) + `"`
}

// findFileUnderInstall searches for a file by name under the install directory.
// Performs a depth-limited search for a file, ignoring case. Used to locate
// executables or other files when the exact subdirectory is unknown.
// Parameters:
//   - installDir: Root directory to search
//   - name: Filename to search for (case-insensitive)
//   - maxDepth: Maximum directory depth to search (0 = no limit)
// Returns:
//   - Absolute path to the found file
//   - true if found, false otherwise
func findFileUnderInstall(installDir, name string, maxDepth int) (string, bool) {
	if maxDepth < 0 {
		return "", false
	}
	var found string
	_ = filepath.WalkDir(installDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(installDir, path)
			if relErr != nil {
				return nil
			}
			depth := strings.Count(filepath.ToSlash(rel), "/")
			if depth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(d.Name(), name) {
			found = path
		}
		return nil
	})
	return found, found != ""
}
