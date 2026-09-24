package install

import (
	"strings"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/store"
)

// cacheReusableForInstall reports whether a content-cache entry can reinstall the
// requested artifact (version + download blob / same-arch members).
func cacheReusableForInstall(store *store.Store, entry *cache.PackageEntry, version, downloadName, manifestHash string) bool {
	if entry == nil || entry.Version != version || !cache.AllObjectsPresent(store, entry) {
		return false
	}
	if downloadName == "" {
		return true
	}
	if findCacheArchiveHash(entry, downloadName, manifestHash) != "" {
		return true
	}
	for _, rel := range entry.Files {
		if rel == downloadName {
			return true
		}
		if cacheArtifactConflictsWithDownload(rel, downloadName) {
			return false
		}
	}
	return true
}

func cacheArtifactConflictsWithDownload(artifactRel, downloadName string) bool {
	dl := strings.ToLower(downloadName)
	ar := strings.ToLower(artifactRel)
	if strings.HasSuffix(ar, ".zip") || strings.HasSuffix(ar, ".nupkg") {
		return ar != dl
	}
	dlToken := downloadArchToken(dl)
	if dlToken == "" {
		return false
	}
	arToken := artifactArchToken(ar)
	if arToken == "" {
		return false
	}
	return arToken != dlToken
}

func downloadArchToken(downloadName string) string {
	dl := strings.ToLower(downloadName)
	switch {
	case strings.Contains(dl, "windows_arm64"), strings.Contains(dl, "_arm64"):
		return "arm64"
	case strings.Contains(dl, "win64"), strings.Contains(dl, "x86_64"), strings.Contains(dl, "amd64"):
		return "64bit"
	case strings.Contains(dl, "win32"), strings.Contains(dl, "i386"), strings.Contains(dl, "x86-"):
		return "32bit"
	default:
		return ""
	}
}

func artifactArchToken(artifact string) string {
	ar := strings.ToLower(artifact)
	switch {
	case strings.Contains(ar, "windows_arm64"), strings.Contains(ar, "_arm64"):
		return "arm64"
	case strings.Contains(ar, "win64"), strings.Contains(ar, "x86_64"), strings.Contains(ar, "amd64"):
		return "64bit"
	case strings.Contains(ar, "win32"), strings.Contains(ar, "i386"):
		return "32bit"
	default:
		return ""
	}
}
