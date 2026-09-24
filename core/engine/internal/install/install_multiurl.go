package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
)

type urlHashPair struct {
	url       string
	hashAlgo  string
	hashValue string
}

func buildURLHashPairs(urls, hashes []string) []urlHashPair {
	var pairs []urlHashPair
	for i, url := range urls {
		var hashWithAlgo string
		if i < len(hashes) {
			hashWithAlgo = hashes[i]
		} else if len(hashes) > 0 {
			hashWithAlgo = hashes[0]
		}
		hashAlgo, hashValue := parseHash(hashWithAlgo)
		pairs = append(pairs, urlHashPair{
			url:       url,
			hashAlgo:  hashAlgo,
			hashValue: hashValue,
		})
	}
	return pairs
}

// manifestUsesMultiArtifactURLs reports Scoop-style manifests where each URL is a separate
// artifact (e.g. vcredist x64 + x86). When false, multiple URLs are download mirrors.
func manifestUsesMultiArtifactURLs(urls, hashes []string) bool {
	if len(urls) <= 1 || len(hashes) != len(urls) {
		return false
	}
	seen := make(map[string]struct{}, len(hashes))
	for _, raw := range hashes {
		_, value := parseHash(raw)
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func expectedMultiArtifactNames(pairs []urlHashPair) ([]string, error) {
	names := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parsed, err := manifest.ParseURL(pair.url)
		if err != nil {
			return nil, fmt.Errorf("parse download URL: %w", err)
		}
		if parsed.LocalName == "" {
			return nil, fmt.Errorf("multi-artifact URL missing filename: %s", pair.url)
		}
		names = append(names, parsed.LocalName)
	}
	return names, nil
}

func downloadManifestArtifact(e *runtime.Engine, ctx context.Context, pair urlHashPair, force, log bool) (downloader.Result, downloader.Task, error) {
	pairParsed, err := manifest.ParseURL(pair.url)
	if err != nil {
		return downloader.Result{}, downloader.Task{}, fmt.Errorf("parse download URL: %w", err)
	}
	task := downloader.Task{
		URL:       pairParsed.FetchURL,
		Filename:  pairParsed.LocalName,
		HashAlgo:  pair.hashAlgo,
		HashValue: pair.hashValue,
	}
	if force {
		e.Downloader.ClearPartial(task)
	}
	if log {
		fetchURLs := e.Downloader.ResolveDownloadURLs(pairParsed.FetchURL)
		verbose.Progressf("  Downloading %s\n", task.Filename)
		if len(fetchURLs) > 1 {
			verbose.Progressf("  (%d URLs to try, including fallbacks)\n", len(fetchURLs))
		}
	}
	result := e.Downloader.DownloadWithCache(ctx, task, pair.hashValue, force)
	if result.Error != nil {
		return result, task, result.Error
	}
	if !result.FromStore {
		if err := downloader.VerifyDownloadResult(e.Store, task, result); err != nil {
			return result, task, err
		}
	}
	return result, task, nil
}

func linkMultiArtifactFromResults(store *store.Store, installDir string, results []downloader.Result, recordFile func(string, string)) (int, error) {
	var linked int
	for _, r := range results {
		if r.Error != nil {
			return linked, r.Error
		}
		name := strings.TrimSpace(r.Task.Filename)
		if name == "" || r.Hash == "" {
			return linked, fmt.Errorf("multi-artifact download missing filename or hash")
		}
		target := filepath.Join(installDir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return linked, err
		}
		if err := store.Link(r.Hash, target); err != nil {
			return linked, fmt.Errorf("link %s: %w", name, err)
		}
		if recordFile != nil {
			recordFile(r.Hash, name)
		}
		linked++
	}
	return linked, nil
}

func linkMultiArtifactFromCache(store *store.Store, installDir string, entry *cache.PackageEntry, names []string, recordFile func(string, string)) (int, error) {
	if entry == nil {
		return 0, fmt.Errorf("cache entry missing for multi-artifact install")
	}
	want := make(map[string]string, len(entry.Files))
	for hash, rel := range entry.Files {
		if runtime.IsHiddenInstallPath(rel) {
			continue
		}
		want[strings.ToLower(filepath.ToSlash(rel))] = hash
	}
	var linked int
	for _, name := range names {
		hash, ok := want[strings.ToLower(name)]
		if !ok {
			return linked, fmt.Errorf("cache missing artifact %q (reinstall with --force)", name)
		}
		target := filepath.Join(installDir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return linked, err
		}
		if err := store.Link(hash, target); err != nil {
			return linked, fmt.Errorf("link %s: %w", name, err)
		}
		if recordFile != nil {
			recordFile(hash, name)
		}
		linked++
	}
	return linked, nil
}
