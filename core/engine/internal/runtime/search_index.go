package runtime

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/manifest"
)

// Entry is one package row in the in-memory catalog/search index.
type Entry struct {
	Name        string
	Bucket      string
	Description string
	Version     string
	Homepage    string
	Archived    bool // manifest under deprecated/ (Scoop archive)
	Deprecated  bool // manifest JSON "deprecated" field
}

// BrowseDeprecated reports whether the entry should be treated as deprecated
// for browse and search purposes (archived manifest or deprecated field set).
func (e Entry) BrowseDeprecated() bool {
	return e.Archived || e.Deprecated
}

// Index holds a flat, sorted list of all packages across loaded buckets.
// It is rebuilt or patched incrementally as buckets are added, removed, or updated.
type Index struct {
	mu            sync.RWMutex
	entries       []Entry
	loadedBuckets map[string]string // bucket name -> root path
}

// NewIndex returns an empty search index.
func NewIndex() *Index {
	return &Index{
		loadedBuckets: make(map[string]string),
	}
}

// rebuild rescans every bucket and replaces the full index.
func (idx *Index) rebuild(buckets []*bucket.Bucket) {
	var entries []Entry
	for _, b := range buckets {
		entries = append(entries, scanBucketManifestEntries(b.Root, b.Name)...)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Bucket != entries[j].Bucket {
			return entries[i].Bucket < entries[j].Bucket
		}
		return entries[i].Name < entries[j].Name
	})

	idx.mu.Lock()
	idx.entries = entries
	idx.loadedBuckets = make(map[string]string, len(buckets))
	for _, b := range buckets {
		idx.loadedBuckets[b.Name] = b.Root
	}
	idx.mu.Unlock()
}

// removeBucket drops all entries and tracking state for one bucket.
func (idx *Index) removeBucket(bucketName string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	filtered := idx.entries[:0]
	for _, entry := range idx.entries {
		if entry.Bucket != bucketName {
			filtered = append(filtered, entry)
		}
	}
	idx.entries = filtered
	delete(idx.loadedBuckets, bucketName)
}

func (idx *Index) loadedBucketRoots() map[string]string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	out := make(map[string]string, len(idx.loadedBuckets))
	for name, root := range idx.loadedBuckets {
		out[name] = root
	}
	return out
}

// loadBucket rescans one bucket and replaces its entries in the index.
func (idx *Index) loadBucket(bucketRoot, bucketName string) {
	entries := scanBucketManifestEntries(bucketRoot, bucketName)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	filtered := idx.entries[:0]
	for _, entry := range idx.entries {
		if entry.Bucket != bucketName {
			filtered = append(filtered, entry)
		}
	}
	idx.entries = append(filtered, entries...)
	idx.loadedBuckets[bucketName] = bucketRoot
}

// HasLoadedBucket reports whether the named bucket is currently indexed.
func (idx *Index) HasLoadedBucket(bucketName string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.loadedBuckets[bucketName]
	return ok
}

func catalogPackageKey(bucket, name string) string {
	return strings.ToLower(bucket) + "/" + strings.ToLower(name)
}

// isHiddenCatalogEntry reports whether a package is hidden by user/catalog policy.
func isHiddenCatalogEntry(hidden map[string]struct{}, bucket, name string) bool {
	if len(hidden) == 0 {
		return false
	}
	_, ok := hidden[catalogPackageKey(bucket, name)]
	return ok
}

// search returns packages whose name or description contains query (case-insensitive).
// buckets limits results to the given bucket names; hidden entries are excluded.
func (idx *Index) Search(query string, buckets []string, hidden map[string]struct{}) []Entry {
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	if lowerQuery == "" {
		return nil
	}

	bucketFilter := make(map[string]struct{}, len(buckets))
	for _, name := range buckets {
		bucketFilter[name] = struct{}{}
	}

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var matches []Entry
	for _, entry := range idx.entries {
		if isHiddenCatalogEntry(hidden, entry.Bucket, entry.Name) {
			continue
		}
		if len(bucketFilter) > 0 {
			if _, ok := bucketFilter[entry.Bucket]; !ok {
				continue
			}
		}
		if strings.Contains(strings.ToLower(entry.Name), lowerQuery) ||
			(entry.Description != "" && strings.Contains(strings.ToLower(entry.Description), lowerQuery)) {
			matches = append(matches, entry)
		}
	}
	return matches
}

// listEntries returns catalog rows with optional bucket filter, text query, and deprecated filtering.
func (idx *Index) ListEntries(bucketName, query string, hideDeprecated bool, hidden map[string]struct{}) []Entry {
	lowerQuery := strings.ToLower(strings.TrimSpace(query))

	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var matches []Entry
	for _, entry := range idx.entries {
		if isHiddenCatalogEntry(hidden, entry.Bucket, entry.Name) {
			continue
		}
		if hideDeprecated && entry.BrowseDeprecated() {
			continue
		}
		if bucketName != "" && entry.Bucket != bucketName {
			continue
		}
		if lowerQuery != "" {
			nameMatch := strings.Contains(strings.ToLower(entry.Name), lowerQuery)
			descMatch := entry.Description != "" && strings.Contains(strings.ToLower(entry.Description), lowerQuery)
			if !nameMatch && !descMatch {
				continue
			}
		}
		matches = append(matches, entry)
	}
	return matches
}

// CountByBucket returns the number of visible indexed packages per bucket.
func (idx *Index) CountByBucket(hideDeprecated bool, hidden map[string]struct{}) map[string]int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	counts := make(map[string]int)
	for _, entry := range idx.entries {
		if isHiddenCatalogEntry(hidden, entry.Bucket, entry.Name) {
			continue
		}
		if hideDeprecated && entry.BrowseDeprecated() {
			continue
		}
		counts[entry.Bucket]++
	}
	return counts
}

// CountAll returns the total number of visible indexed packages across buckets.
func (idx *Index) CountAll(hideDeprecated bool, hidden map[string]struct{}) int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	n := 0
	for _, entry := range idx.entries {
		if isHiddenCatalogEntry(hidden, entry.Bucket, entry.Name) {
			continue
		}
		if hideDeprecated && entry.BrowseDeprecated() {
			continue
		}
		n++
	}
	return n
}

// resolvePackage picks one index entry for a bare package name.
// Preference order: preferred bucket (non-deprecated) → preferred bucket (any) →
// main (non-deprecated) → any non-deprecated → first match.
func (idx *Index) ResolvePackage(name, preferredBucket string) *Entry {
	matches := idx.FindExactName(name)
	if len(matches) == 0 {
		return nil
	}
	pick := func(pred func(Entry) bool) *Entry {
		for i := range matches {
			if pred(matches[i]) {
				return &matches[i]
			}
		}
		return nil
	}
	if preferredBucket != "" {
		if e := pick(func(entry Entry) bool {
			return entry.Bucket == preferredBucket && !entry.BrowseDeprecated()
		}); e != nil {
			return e
		}
		if e := pick(func(entry Entry) bool { return entry.Bucket == preferredBucket }); e != nil {
			return e
		}
	}
	if e := pick(func(entry Entry) bool { return entry.Bucket == "main" && !entry.BrowseDeprecated() }); e != nil {
		return e
	}
	if e := pick(func(entry Entry) bool { return !entry.BrowseDeprecated() }); e != nil {
		return e
	}
	return &matches[0]
}

// FindExactName returns all index entries whose name matches pkgName (case-insensitive).
func (idx *Index) FindExactName(pkgName string) []Entry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var matches []Entry
	for _, entry := range idx.entries {
		if strings.EqualFold(entry.Name, pkgName) {
			matches = append(matches, entry)
		}
	}
	return matches
}

// RebuildSearchIndex runs a full index rebuild in the background goroutine started by NewEngine.
func RebuildSearchIndex(e *Engine) {
	if e.SearchIdx == nil {
		e.SearchIdx = NewIndex()
	}
	e.SearchIdx.rebuild(e.BucketRegistry.List())
	e.SearchIdxReady.Store(true)
}

// SearchIndexReady reports whether the initial bucket manifest index build has finished.
func SearchIndexReady(e *Engine) bool {
	return e.SearchIdxReady.Load()
}

// WaitSearchIndexReady blocks until the background index build started by NewEngine completes.
func WaitSearchIndexReady(e *Engine, ctx context.Context) error {
	if e.SearchIdxReady.Load() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !e.SearchIdxReady.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

// SyncSearchIndex aligns the in-memory index with bucketMgr without a full rebuild.
func SyncSearchIndex(e *Engine, refreshExisting bool) {
	if e.SearchIdx == nil {
		e.SearchIdx = NewIndex()
	}

	loaded := e.SearchIdx.loadedBucketRoots()
	current := e.BucketRegistry.List()
	currentSet := make(map[string]string, len(current))
	for _, b := range current {
		currentSet[b.Name] = b.Root
	}

	// Drop buckets that were removed locally.
	for name := range loaded {
		if _, ok := currentSet[name]; !ok {
			e.SearchIdx.removeBucket(name)
		}
	}

	// Index newly added buckets right away.
	for name, root := range currentSet {
		if !e.SearchIdx.HasLoadedBucket(name) {
			e.SearchIdx.loadBucket(root, name)
		}
	}

	// Rescan buckets that already exist (e.g. after bucket update).
	if refreshExisting {
		for name, root := range currentSet {
			if e.SearchIdx.HasLoadedBucket(name) {
				e.SearchIdx.loadBucket(root, name)
			}
		}
	}
}

// LoadSearchIndexBucket immediately indexes a single bucket (e.g. after add).
func LoadSearchIndexBucket(e *Engine, name string) {
	if e.SearchIdx == nil {
		e.SearchIdx = NewIndex()
	}
	for _, b := range e.BucketRegistry.List() {
		if b.Name == name {
			e.SearchIdx.loadBucket(b.Root, b.Name)
			return
		}
	}
}

// RemoveSearchIndexBucket drops all indexed packages for a bucket (e.g. after remove).
func RemoveSearchIndexBucket(e *Engine, name string) {
	if e.SearchIdx == nil {
		return
	}
	e.SearchIdx.removeBucket(name)
}

// BucketManifestPaths lists package manifest JSON paths under a Scoop bucket repo.
func BucketManifestPaths(bucketRoot, bucketName string) []string {
	return bucketManifestPaths(bucketRoot, bucketName)
}

// bucketManifestPaths walks standard Scoop layout dirs and returns one path per package name.
// Later dirs do not override an already-seen package (first wins).
func bucketManifestPaths(bucketRoot, bucketName string) []string {
	dirs := []string{
		filepath.Join(bucketRoot, "bucket"),
		bucketRoot,
		filepath.Join(bucketRoot, bucketName),
		filepath.Join(bucketRoot, "deprecated"),
	}

	seen := make(map[string]string)
	for _, dir := range dirs {
		collectManifestPaths(dir, seen)
	}

	paths := make([]string, 0, len(seen))
	for _, manifestPath := range seen {
		paths = append(paths, manifestPath)
	}
	sort.Strings(paths)
	return paths
}

func collectManifestPaths(dir string, seen map[string]string) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		ext := filepath.Ext(name)
		if strings.ToLower(ext) != ".json" {
			return nil
		}
		pkgName := strings.TrimSuffix(name, ext)
		if pkgName == "" {
			return nil
		}
		if _, ok := seen[pkgName]; ok {
			return nil
		}
		seen[pkgName] = path
		return nil
	})
}

// ScanBucketManifestEntries parses manifest JSON files into index rows; unreadable files are skipped.
func ScanBucketManifestEntries(bucketRoot, bucketName string) []Entry {
	return scanBucketManifestEntries(bucketRoot, bucketName)
}

// scanBucketManifestEntries parses manifest JSON files into index rows; unreadable files are skipped.
func scanBucketManifestEntries(bucketRoot, bucketName string) []Entry {
	paths := bucketManifestPaths(bucketRoot, bucketName)
	entries := make([]Entry, 0, len(paths))

	for _, manifestPath := range paths {
		pkgName := strings.TrimSuffix(filepath.Base(manifestPath), ".json")
		entry := Entry{
			Name:   pkgName,
			Bucket: bucketName,
		}

		m, err := manifest.ParseFile(manifestPath)
		if err != nil {
			continue
		}
		entry.Description = m.Description
		entry.Version = m.Version
		entry.Homepage = m.Homepage
		entry.Archived = manifest.IsDeprecatedManifestPath(bucketRoot, manifestPath)
		entry.Deprecated = m.IsDeprecatedMarked()
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}
