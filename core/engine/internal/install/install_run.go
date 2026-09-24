package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/extractor"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/safepath"
	"github.com/gluestick-sh/core/store"
)

// profileEnabled checks if installation profiling is enabled.
// Profiling tracks timing for each phase (resolve, fetch, deploy, etc.)
// to help identify performance bottlenecks.
// Parameters:
//   - req: Installation request with options
// Returns true if profiling is enabled.
func profileEnabled(req *etypes.InstallRequest) bool {
	if req.Options == nil {
		return false
	}
	return req.Options["profile"] == "true"
}

// downloadFileArgs builds progress reporting arguments for download operations.
// Parameters:
//   - filename: Name of the file being downloaded
// Returns a map with the filename, or nil if filename is empty.
func downloadFileArgs(filename string) map[string]interface{} {
	if filename == "" {
		return nil
	}
	return map[string]interface{}{"file": filename}
}

// PackageFull is the main installation orchestrator.
// It creates an installState and executes the installation pipeline through phases:
// 1. resolve - Find and validate manifest
// 2. fetch - Download or retrieve from cache
// 3. deploy - Extract and install files
// 4. finalize - Complete installation (hooks, shims, shortcuts, cache)
//
// Parameters:
//   - e: Runtime engine with access to all subsystems
//   - ctx: Context for cancellation (may be nil)
//   - pkgRef: Package reference (e.g., "package" or "package@version")
//   - req: Installation request with options
//   - reporter: Progress reporter for UI updates (may be nil)
//
// Returns error if any phase fails. A nil error means the package was
// successfully installed or was already installed.
func PackageFull(e *runtime.Engine, ctx context.Context, pkgRef string, req *etypes.InstallRequest, reporter etypes.ProgressReporter) error {
	// Create and initialize install state
	state := newInstallState(e, ctx, pkgRef, req, reporter)
	defer state.cleanup()

	// Phase 1: Resolve - find and validate manifest
	if err := resolveInstallPhase(state); err != nil {
		return err
	}
	if state.done {
		return nil
	}

	// Phase 2: Fetch - download or retrieve from cache
	if err := fetchInstallPhase(state); err != nil {
		return err
	}

	// Phase 3: Deploy - extract and install files
	if err := deployInstallPhase(state); err != nil {
		return err
	}

	state.installSucceeded = true
	return nil
}

// LinkFromCache hardlinks previously cached CAS objects into the install directory.
// For archives, the download blob (matching downloadName or archiveHash) is skipped; extracted files are linked.
// For plain single-file packages (.bat, .jar, etc.), the download file is the installable artifact.
func LinkFromCache(store *store.Store, installDir string,
	entry *cache.PackageEntry,
	downloadName, fileExt, archiveHash, extractTo, extractDir string,
	m *manifest.Manifest, installArch, pkgName string,
) (int, error) {
	skipDownloadBlob := skipArchiveBlobOnLink(fileExt, downloadName, m, installArch, pkgName)
	var linked int
	for hash, relPath := range entry.Files {
		if runtime.IsHiddenInstallPath(relPath) {
			continue
		}
		if skipDownloadBlob && (relPath == downloadName || (archiveHash != "" && hash == archiveHash)) {
			continue
		}
		targetRelPath, err := installMemberRelPath(extractTo, extractDir, relPath)
		if err != nil {
			return linked, err
		}
		if targetRelPath == "" {
			continue
		}
		targetPath, err := safepath.JoinUnderBase(installDir, targetRelPath)
		if err != nil {
			return linked, fmt.Errorf("link %s: %w", relPath, err)
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return linked, fmt.Errorf("create dir for %s: %w", relPath, err)
		}
		if err := store.Link(hash, targetPath); err != nil {
			return linked, fmt.Errorf("link %s: %w", relPath, err)
		}
		linked++
	}
	return linked, nil
}

// parseHash parses a hash with algorithm prefix.
// Accepts formats:
// - "sha256:abc123..." (explicit algorithm)
// - "abc123..." (bare digest, algorithm inferred by length)
//
// Length inference follows Scoop convention:
// - 32 chars = MD5
// - 40 chars = SHA-1
// - 64 chars = SHA-256
// - 128 chars = SHA-512
//
// Parameters:
//   - hash: Hash string to parse
// Returns:
//   - algo: Hash algorithm (sha256, sha1, md5, sha512)
//   - value: Hash value (hex string)
func parseHash(hash string) (algo, value string) {
	hash = strings.TrimSpace(hash)
	parts := strings.SplitN(hash, ":", 2)
	if len(parts) == 2 {
		return strings.ToLower(parts[0]), parts[1]
	}
	value = hash
	switch len(value) {
	case 32:
		return "md5", value
	case 40:
		return "sha1", value
	case 64:
		return "sha256", value
	case 128:
		return "sha512", value
	default:
		return "sha256", value
	}
}

// ParseBinPattern parses Scoop bin entries into executable and alias.
// This is a simplified version that returns just the exe and alias,
// ignoring any additional default arguments.
// Parameters:
//   - pattern: Bin pattern string (e.g., "exe", "exe,alias", "[exe,alias,args]")
// Returns:
//   - exe: Executable name/path
//   - alias: Shim alias (if any)
func ParseBinPattern(pattern string) (exe, alias string) {
	exe, alias, _ = ParseBinPatternParts(pattern)
	return exe, alias
}

// ParseBinPatternParts parses Scoop bin entries including optional default arguments.
// Supports multiple formats:
// - "exe" - just the executable
// - "exe,alias" - executable with shim alias
// - "[exe,alias,arg1,arg2]" - executable, alias, and default arguments for shim
//
// Parameters:
//   - pattern: Bin pattern string
// Returns:
//   - exe: Executable name/path
//   - alias: Shim alias (empty if not specified)
//   - extraArgs: Default arguments to pass through shim (empty if none)
func ParseBinPatternParts(pattern string) (exe, alias string, extraArgs []string) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", "", nil
	}

	var parts []string
	switch {
	case strings.HasPrefix(pattern, "["):
		inner := strings.Trim(pattern, "[]")
		for _, p := range strings.Split(inner, ",") {
			parts = append(parts, trimBinToken(p))
		}
	case strings.Contains(pattern, ","):
		for _, p := range strings.Split(pattern, ",") {
			parts = append(parts, trimBinToken(p))
		}
	default:
		return trimBinToken(pattern), "", nil
	}

	if len(parts) == 0 || parts[0] == "" {
		return "", "", nil
	}
	exe = parts[0]
	if len(parts) > 1 {
		alias = parts[1]
	}
	if len(parts) > 2 {
		extraArgs = append([]string(nil), parts[2:]...)
	}
	return exe, alias, extraArgs
}

// trimBinToken removes surrounding whitespace and quotes from a bin token.
// Handles both single and double quotes.
// Parameters:
//   - s: String to trim
// Returns the trimmed string without quotes.
func trimBinToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return strings.Trim(s, "\"'")
}

type deployProgressReporter func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]interface{}, bytes, total int64)

// reportDeployStart reports the start of the deployment phase.
// This helper sends the appropriate progress message based on whether
// files are being linked from the store (fast) or extracted (slow).
// Parameters:
//   - report: Progress reporting callback
//   - linkFromStore: true if linking from cache, false if extracting
func reportDeployStart(report deployProgressReporter, linkFromStore bool) {
	if linkFromStore {
		report(PhaseLink, StatusRunning, 0, message.ProgressLinkingFiles, nil, 0, 0)
	} else {
		report(PhaseExtract, StatusRunning, 0, message.ProgressExtracting, nil, 0, 0)
	}
}

// installNeedsSevenZip reports whether an install path will require 7-Zip for extraction.
// This checks the manifest and file type to determine if the external 7z.exe
// tool is needed for extraction (versus using built-in ZIP member indexing).
// Parameters:
//   - m: Package manifest
//   - downloadName: Name of the downloaded file
//   - fileExt: File extension (normalized)
//   - installArch: Target architecture (arm64, amd64, etc.)
// Returns true if 7-Zip is required.
func installNeedsSevenZip(m *manifest.Manifest, downloadName, fileExt, installArch string) bool {
	if m == nil || isPlainFileExt(fileExt) {
		return false
	}
	switch fileExt {
	case ".zip", ".nupkg", ".tar", ".7z", ".7z.exe":
		return true
	case ".msi":
		return !msiNeedsAdministrativeExtract(m, installArch)
	case ".msi_":
		return false
	case ".exe":
		if isPortableExeInstall(m, downloadName, installArch) {
			return false
		}
		if m.InnoSetup {
			return false
		}
		if m.HasInstallerScriptForInstall(installArch) {
			return false
		}
		return true
	default:
		if isArchiveInstallExt(fileExt) {
			return true
		}
		return !isPlainFileExt(fileExt)
	}
}

// ShouldExtractFromCache chooses cache reinstall strategy: extraction vs hardlink.
// Determines whether to re-extract archives with 7z or hardlink existing files.
// Extraction is slower but may be necessary if the archive structure differs
// from what's currently indexed.
// Parameters:
//   - fileExt: File extension
//   - entry: Cache entry for the package
//   - downloadName: Original download filename
//   - archiveHash: Content hash of the archive
//   - m: Package manifest
//   - installArch: Target architecture
//   - pkgName: Package name
// Returns true if extraction is needed, false for hardlinking.
func ShouldExtractFromCache(fileExt string, entry *cache.PackageEntry, downloadName, archiveHash string, m *manifest.Manifest, installArch, pkgName string) bool {
	if manifest.IsScoopMsiHookInstall(downloadName, fileExt) {
		return false
	}
	if isPreInstall7zHookInstall(m, downloadName, installArch, pkgName) {
		return false
	}
	if isPlainFileExt(fileExt) {
		return false
	}
	if countLinkableFromCache(entry, downloadName, fileExt, archiveHash, m, installArch, pkgName) > 0 {
		return false
	}
	return findCacheArchiveHash(entry, downloadName, archiveHash) != "" || isArchiveInstallExt(fileExt)
}

// normalizeInstallFileExt maps compound tar archives to ".tar" for install routing.
// Compound extensions like .tar.gz, .tar.bz2, and .tar.xz are normalized to .tar
// since they're all handled the same way by the tar extractor.
// Parameters:
//   - ext: File extension to normalize
// Returns the normalized extension.
func normalizeInstallFileExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".tar.gz", ".tar.bz2", ".tar.xz", ".tgz":
		return ".tar"
	default:
		return ext
	}
}

// archiveTypeForExtract returns an extractor archive type from the download name.
// Uses the extractor's type detection to determine how to handle the archive.
// Parameters:
//   - downloadName: Download filename
//   - fileExt: File extension as fallback
// Returns archive type (zip, tar, 7z, etc.) or empty string if unknown.
func archiveTypeForExtract(downloadName, fileExt string) string {
	e := &extractor.Extractor{}
	for _, name := range []string{downloadName, "stub" + fileExt} {
		if strings.TrimSpace(name) == "" || name == "stub" {
			continue
		}
		if at, err := e.DetectType(name); err == nil && at != "" {
			return at
		}
	}
	return ""
}

// isArchiveInstallExt reports formats deployed by extraction.
// These formats store only the archive blob in CAS; extracted files are
// linked during installation rather than stored separately.
// Parameters:
//   - ext: File extension to check
// Returns true if the extension is an archive format.
func isArchiveInstallExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".tar", ".tar.gz", ".tar.bz2", ".tar.xz", ".tgz", ".nupkg", ".7z", ".7z.exe":
		return true
	default:
		return false
	}
}

// isPlainFileExt reports extensions for single-file installs that need no extraction.
// These are scripts or executables that are installed as-is without extraction.
// Parameters:
//   - ext: File extension to check
// Returns true if the extension is a plain file type.
func isPlainFileExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".bat", ".cmd", ".ps1", ".jar", ".sh", ".vbs", ".py":
		return true
	default:
		return false
	}
}

// downloadFilename returns the archive filename from a download URL.
// Handles Scoop's URL fragment syntax: "url#/newname.ext"
// Parameters:
//   - url: Download URL (may contain fragment)
// Returns the local filename for the download.
func downloadFilename(url string) string {
	parsed, err := manifest.ParseURL(url)
	if err != nil {
		return filepath.Base(url)
	}
	return parsed.LocalName
}

// getFileExtensionFromURL determines the file extension from a URL.
// Handles Scoop's URL fragment syntax: "url#/newname.ext"
// The fragment can override the detected extension.
// Parameters:
//   - url: Download URL (may contain fragment)
// Returns the file extension (lowercase, with dot).
func getFileExtensionFromURL(url string) string {
	parsed, err := manifest.ParseURL(url)
	if err != nil {
		return strings.ToLower(filepath.Ext(url))
	}
	return parsed.Extension
}

// preserveInstalledMetadata preserves existing metadata when reinstalling a package.
// This ensures that custom metadata (like version locks) survives reinstallation.
// Parameters:
//   - cacheIdx: Cache index
//   - pkgName: Package name
// Returns existing metadata map, or nil if none exists.
func preserveInstalledMetadata(cacheIdx *cache.Index, pkgName string) map[string]interface{} {
	inst, ok := cacheIdx.GetInstalled(pkgName)
	if !ok || inst.Metadata == nil {
		return nil
	}
	out := make(map[string]interface{}, len(inst.Metadata))
	for k, v := range inst.Metadata {
		out[k] = v
	}
	return out
}
