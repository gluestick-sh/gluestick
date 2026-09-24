package install

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	eruntime "github.com/gluestick-sh/core/engine/internal/runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gluestick-sh/core/archmember"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/safepath"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/extractor"
	"github.com/gluestick-sh/core/manifest"
)

// countLinkableFromCache counts files linkFromCache would install (mirrors its skip rules).
func countLinkableFromCache(entry *cache.PackageEntry, downloadName, fileExt, archiveHash string, m *manifest.Manifest, installArch, pkgName string) int {
	if entry == nil {
		return 0
	}
	skipDownloadBlob := skipArchiveBlobOnLink(fileExt, downloadName, m, installArch, pkgName)
	var n int
	for hash, relPath := range entry.Files {
		if eruntime.IsHiddenInstallPath(relPath) {
			continue
		}
		if skipDownloadBlob && (relPath == downloadName || (archiveHash != "" && hash == archiveHash)) {
			continue
		}
		n++
	}
	return n
}

func extractArchiveFromCache(store *store.Store, ext *extractor.Extractor, installDir, extractTo string, entry *cache.PackageEntry, downloadName, archiveHash string) error {
	hash := findCacheArchiveHash(entry, downloadName, archiveHash)
	if hash == "" {
		return fmt.Errorf("archive hash not found in cache index")
	}
	if err := cleanInstallDir(installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}
	dest, err := installExtractDest(installDir, extractTo)
	if err != nil {
		return fmt.Errorf("extract destination: %w", err)
	}
	return ext.ExtractToDir(store.ObjectPath(hash), dest, downloadName)
}

// installIndexProgressFunc reports lightweight install-dir indexing progress (file counts).
type installIndexProgressFunc func(processed, total int64)

// archiveMemberIndexReady reports whether a persisted member index can hardlink-reinstall.
func archiveMemberIndexReady(d *downloader.Downloader, archiveHash string) bool {
	if d == nil || archiveHash == "" {
		return false
	}
	_, _, ok := d.ResolveZipMemberIndex(archiveHash)
	return ok
}

// installArchiveFromMemberIndex hardlinks from a persisted member index when available;
// otherwise extracts, adopts files into cache store, and saves the index for fast reinstall.
func installArchiveFromMemberIndex(e *eruntime.Engine, 
	prof *installPhaseProfile,
	casPath, installDir, downloadName, archiveHash string,
	extractTo, extractDir string,
	installedFiles map[string]string,
	totalSize *int64,
	reportIndexProgress installIndexProgressFunc,
) (int, error) {
	memberFiles, memberTotal, ok := e.Downloader.ResolveZipMemberIndex(archiveHash)
	if ok && len(memberFiles) > 0 {
		if err := cleanInstallDir(installDir); err != nil {
			return 0, fmt.Errorf("clean install dir: %w", err)
		}
		verbose.Progressf("  Linking files from store...\n")
		linkStart := time.Now()
		linked, err := LinkExtractedFiles(e.Store, installDir, extractTo, extractDir, memberFiles, nil)
		if prof != nil {
			prof.addLink(linkStart)
		}
		if err != nil {
			return 0, err
		}
		if linked == 0 {
			return 0, fmt.Errorf("no files were linked from archive")
		}
		verbose.Progressf("  Linked %d file(s)\n", linked)
		indexed, size, err := indexZipMemberLinks(e.Store, memberFiles, memberTotal, extractTo, extractDir, downloadName, archiveHash)
		if err != nil {
			return 0, err
		}
		for hash, rel := range indexed {
			installedFiles[hash] = rel
		}
		*totalSize = size
		return linked, nil
	}

	runExtractAdopt := func() error {
		if err := cleanInstallDir(installDir); err != nil {
			return fmt.Errorf("clean install dir: %w", err)
		}
		dest, err := installExtractDest(installDir, extractTo)
		if err != nil {
			return fmt.Errorf("extract destination: %w", err)
		}
		if err := e.Extractor.ExtractToDir(casPath, dest, downloadName); err != nil {
			return err
		}
		if extractDir != "" {
			if _, err := applyExtractDirLayout(dest, extractDir); err != nil {
				return fmt.Errorf("apply extract_dir: %w", err)
			}
		}
		verbose.Progressf("  Adopting installed files into store...\n")
		memberFiles, memberTotal, err = adoptInstallDirToStore(e.Store, installDir, func(processed, total int64) {
			if reportIndexProgress != nil {
				reportIndexProgress(processed, total)
			}
		})
		return err
	}
	var extractErr error
	if prof != nil {
		extractErr = prof.runExtract(runExtractAdopt)
	} else {
		extractErr = runExtractAdopt()
	}
	if extractErr != nil {
		return 0, extractErr
	}
	if archiveInfo, err := os.Stat(casPath); err == nil {
		memberTotal += archiveInfo.Size()
	}
	if err := e.Downloader.SaveZipMemberIndex(archiveHash, memberFiles, memberTotal); err != nil {
		return 0, fmt.Errorf("save archive member index: %w", err)
	}
	indexed, size, err := indexZipMemberLinks(e.Store, memberFiles, memberTotal, extractTo, extractDir, downloadName, archiveHash)
	if err != nil {
		return 0, fmt.Errorf("index adopted files: %w", err)
	}
	for hash, rel := range indexed {
		installedFiles[hash] = rel
	}
	*totalSize = size
	return len(memberFiles), nil
}

// indexDirectExtractInstall indexes direct 7z extracts without hashing every file into cache store.
// The cache store keeps the archive blob only; reinstall re-extracts from that blob.
func indexDirectExtractInstall(store *store.Store, installDir, downloadName, archiveHash string, files map[string]string, totalSize *int64, onProgress installIndexProgressFunc) (int, error) {
	for k := range files {
		delete(files, k)
	}
	if archiveHash != "" && downloadName != "" {
		files[archiveHash] = downloadName
	}

	var installBytes int64
	var fileCount int
	const progressEvery = 1000

	err := filepath.WalkDir(installDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(installDir, path)
		if err != nil || cache.IsHiddenInstallPath(relPath) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		installBytes += info.Size()
		fileCount++
		if onProgress != nil && fileCount%progressEvery == 0 {
			onProgress(int64(fileCount), 0)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if onProgress != nil && fileCount > 0 {
		onProgress(int64(fileCount), int64(fileCount))
	}
	*totalSize = installBytes
	return fileCount, nil
}

func findCacheArchiveHash(entry *cache.PackageEntry, downloadName, expectedCASHash string) string {
	if entry == nil {
		return ""
	}
	for hash, rel := range entry.Files {
		if rel == downloadName || (expectedCASHash != "" && hash == expectedCASHash) {
			return hash
		}
	}
	// Index may store the manifest digest as filename (rebuild / legacy entries).
	if expectedCASHash != "" {
		for hash, rel := range entry.Files {
			if rel == expectedCASHash {
				return hash
			}
		}
	}
	if len(entry.Files) == 1 {
		for hash, rel := range entry.Files {
			if downloadName != "" && rel != downloadName {
				if expectedCASHash == "" || (hash != expectedCASHash && rel != expectedCASHash) {
					continue
				}
			}
			return hash
		}
	}
	return ""
}

// indexZipMemberLinks builds the cache index map for a zip hardlink install without
// per-file stat during linking.
func indexZipMemberLinks(store *store.Store, zipFiles map[string]string, zipTotalBytes int64, extractTo, extractDir, downloadName, archiveHash string) (map[string]string, int64, error) {
	installed := make(map[string]string, len(zipFiles)+1)
	for relPath, hash := range zipFiles {
		if eruntime.IsHiddenInstallPath(relPath) || archmember.IsDirectoryPlaceholderName(relPath) {
			continue
		}
		rel := archmember.NormalizeMember(relPath)
		targetRel, err := installMemberRelPath(extractTo, extractDir, rel)
		if err != nil {
			return nil, 0, err
		}
		if targetRel == "" {
			continue
		}
		installed[hash] = targetRel
	}
	if archiveHash != "" && downloadName != "" {
		installed[archiveHash] = downloadName
	}
	if zipTotalBytes > 0 {
		return installed, zipTotalBytes, nil
	}
	totalSize, err := sumUniqueStoreObjectSizes(store, installed)
	return installed, totalSize, err
}

func sumUniqueStoreObjectSizes(store *store.Store, files map[string]string) (int64, error) {
	unique := make([]string, 0, len(files))
	seen := make(map[string]struct{}, len(files))
	for hash := range files {
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		unique = append(unique, hash)
	}
	if len(unique) == 0 {
		return 0, nil
	}

	workers := goruntime.NumCPU()
	if workers < 2 {
		workers = 2
	}
	if workers > 8 {
		workers = 8
	}
	if workers > len(unique) {
		workers = len(unique)
	}

	var total atomic.Int64
	var firstErr error
	var errOnce sync.Once
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, hash := range unique {
		wg.Add(1)
		go func(hash string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := os.Stat(store.ObjectPath(hash))
			if err != nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("stat %s: %w", hash[:min(8, len(hash))], err) })
				return
			}
			total.Add(info.Size())
		}(hash)
	}
	wg.Wait()
	return total.Load(), firstErr
}

func cleanInstallDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// installExtractDest returns the 7z/link destination under installDir (Scoop extract_to).
func installExtractDest(installDir, extractTo string) (string, error) {
	safe, err := safepath.ValidateManifestRelPath(extractTo)
	if err != nil {
		return "", err
	}
	if safe == "" {
		return installDir, nil
	}
	return safepath.JoinUnderBase(installDir, safe)
}

// installMemberRelPath maps an archive member to its install-relative path.
func installMemberRelPath(extractTo, extractDir, relPath string) (string, error) {
	rel := relPathAfterExtractDir(relPath, extractDir)
	if rel == "" {
		return "", nil
	}
	safeRel, err := safepath.ValidateManifestRelPath(rel)
	if err != nil {
		return "", fmt.Errorf("archive member %q: %w", relPath, err)
	}
	if safeRel == "" {
		return "", nil
	}
	safeExtractTo, err := safepath.ValidateManifestRelPath(extractTo)
	if err != nil {
		return "", err
	}
	if safeExtractTo == "" {
		return safeRel, nil
	}
	combined := safeExtractTo + "/" + safeRel
	return safepath.ValidateManifestRelPath(combined)
}

// normalizeManifestRelPath normalizes Scoop manifest paths to forward slashes.
func normalizeManifestRelPath(p string) string {
	return filepath.ToSlash(strings.Trim(p, `/\`))
}

// relPathAfterExtractDir strips extract_dir from an archive member path (Scoop flatten semantics).
func relPathAfterExtractDir(relPath, extractDir string) string {
	for _, prefix := range extractDirLookupPaths(extractDir) {
		if rest, ok := stripExtractDirPrefix(relPath, prefix); ok {
			return rest
		}
	}
	return relPath
}

func stripExtractDirPrefix(relPath, prefix string) (rest string, ok bool) {
	rel := normalizeManifestRelPath(relPath)
	prefix = normalizeManifestRelPath(prefix)
	if prefix == "" || prefix == "." {
		return "", false
	}
	if !strings.HasPrefix(rel, prefix) {
		return "", false
	}
	rest = strings.TrimPrefix(rel, prefix)
	rest = strings.TrimPrefix(rest, "/")
	return rest, true
}

// extractDirLookupPaths returns manifest extract_dir and Scoop MSI aliases (PFiles64 vs Program Files).
func extractDirLookupPaths(extractDir string) []string {
	base := normalizeManifestRelPath(extractDir)
	if base == "" || base == "." {
		return nil
	}
	seen := map[string]struct{}{base: {}}
	out := []string{base}
	add := func(p string) {
		p = normalizeManifestRelPath(p)
		if p == "" || p == "." {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if strings.HasPrefix(base, "PFiles64/") {
		add(strings.Replace(base, "PFiles64/", "Program Files/", 1))
	}
	if strings.HasPrefix(base, "PFiles/") {
		add(strings.Replace(base, "PFiles/", "Program Files (x86)/", 1))
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		add(base[i+1:])
	}
	return out
}

// flattenExtractDir moves files from installDir/extractDir/* up into installDir (Scoop extract_dir).
func flattenExtractDir(installDir, extractDir string) error {
	extractDir, err := safepath.ValidateManifestRelPath(extractDir)
	if err != nil {
		return err
	}
	if extractDir == "" {
		return nil
	}

	src := filepath.Join(append([]string{installDir}, strings.Split(extractDir, "/")...)...)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("read extract_dir %s: %w", extractDir, err)
	}

	// Stage under a temp name first. On Windows, moving Browser/browser out while Browser/
	// still exists fails because paths are case-insensitive (Browser == browser).
	staging := src + ".glue-staging"
	if err := os.Rename(src, staging); err != nil {
		return fmt.Errorf("stage extract_dir %s: %w", extractDir, err)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("read extract_dir %s: %w", extractDir, err)
	}

	for _, entry := range entries {
		from := filepath.Join(staging, entry.Name())
		to := filepath.Join(installDir, entry.Name())
		if _, err := os.Stat(to); err == nil {
			if err := os.RemoveAll(to); err != nil {
				return fmt.Errorf("replace %s: %w", entry.Name(), err)
			}
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move %s: %w", entry.Name(), err)
		}
	}
	return os.RemoveAll(staging)
}

// findExtractDirParent locates the directory that contains extract_dir (directly or under one wrapper folder).
func findExtractDirParent(installDir, extractDir string) (parent, matchedDir string, ok bool) {
	for _, dir := range extractDirLookupPaths(extractDir) {
		if parent, ok := findExtractDirParentExact(installDir, dir); ok {
			return parent, dir, true
		}
	}
	return "", "", false
}

func findExtractDirParentExact(installDir, extractDir string) (parent string, ok bool) {
	extractDir = normalizeManifestRelPath(extractDir)
	if extractDir == "" || extractDir == "." {
		return "", false
	}
	direct := filepath.Join(installDir, extractDir)
	if info, err := os.Stat(direct); err == nil && info.IsDir() {
		return installDir, true
	}
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return "", false
	}
	var nestedParent string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(installDir, entry.Name(), extractDir)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			if nestedParent != "" {
				return "", false
			}
			nestedParent = filepath.Join(installDir, entry.Name())
		}
	}
	if nestedParent != "" {
		return nestedParent, true
	}
	return "", false
}

// applyExtractDirLayout flattens extract_dir, including when it sits under a single top-level wrapper (e.g. Tor Browser/Browser).
func applyExtractDirLayout(installDir, extractDir string) (applied bool, err error) {
	safe, err := safepath.ValidateManifestRelPath(extractDir)
	if err != nil {
		return false, err
	}
	if safe == "" {
		return false, nil
	}
	parent, matchedDir, ok := findExtractDirParent(installDir, safe)
	if !ok {
		return false, nil
	}
	if err := flattenExtractDir(parent, matchedDir); err != nil {
		return false, err
	}
	if parent != installDir {
		if err := flattenExtractDir(installDir, filepath.Base(parent)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func installDirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err != nil || len(entries) == 0
}

func validateInstallDir(installDir string) error {
	if installDirIsEmpty(installDir) {
		return fmt.Errorf("install directory is empty after cache install (try glue install --force %s)", filepath.Base(filepath.Dir(installDir)))
	}
	return nil
}

// refreshInstalledFilesFromDir rebuilds hash→path entries by scanning installDir.
// Size uses cache store blob bytes when present; otherwise falls back to on-disk file size
// (archive-only extracts where program files are not stored in cache store).
func refreshInstalledFilesFromDir(store *store.Store, installDir string, files map[string]string, totalSize *int64) error {
	*totalSize = 0
	for k := range files {
		delete(files, k)
	}
	return filepath.Walk(installDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(installDir, path)
		if err != nil || cache.IsHiddenInstallPath(relPath) {
			return nil
		}
		hash, err := store.HashForPath(path)
		if err != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		if _, exists := files[hash]; !exists {
			*totalSize += store.PayloadSize(hash, info.Size())
		}
		files[hash] = relPath
		return nil
	})
}
