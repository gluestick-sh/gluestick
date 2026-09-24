// Package bucket provides unified bucket repository management.
//
// The Registry combines git operations with manifest indexing,
// providing a single entry point for bucket management.
package bucket

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/bootstrap"
	"github.com/gluestick-sh/core/git"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/verbose"
)

// Registry represents a unified bucket management registry.
// It handles both git repository operations and manifest file discovery.
type Registry struct {
	bucketsDir string
	buckets    map[string]*Bucket
	git        *git.Runner
	bootstrap  *bootstrap.Bootstrap
	checkedGit bool
	mu         sync.RWMutex
}

// NewRegistry creates a new unified bucket registry.
func NewRegistry(rootDir string) (*Registry, error) {
	bucketsDir := filepath.Join(rootDir, "buckets")
	if err := os.MkdirAll(bucketsDir, 0755); err != nil {
		return nil, fmt.Errorf("create buckets directory: %w", err)
	}

	return &Registry{
		bucketsDir: bucketsDir,
		buckets:    make(map[string]*Bucket),
		git:        git.NewRunner(),
		bootstrap:  bootstrap.NewBootstrap(rootDir),
	}, nil
}

// EnsureGit checks if git is available, bootstrapping if necessary.
func (r *Registry) EnsureGit() error {
	if r.checkedGit {
		return nil
	}

	if err := r.git.Check(); err == nil {
		r.checkedGit = true
		return nil
	}

	gitPath, err := r.bootstrap.EnsureGit(context.Background())
	if err != nil {
		return fmt.Errorf("bootstrap git failed: %w", err)
	}

	r.git.SetGitPath(gitPath)
	r.checkedGit = true
	return nil
}

// Add adds a bucket by cloning the repository.
func (r *Registry) Add(name, repoURL string) (*Bucket, error) {
	return r.AddWithProgress(name, repoURL, nil)
}

// AddWithProgress adds a bucket and reports clone/pull progress.
func (r *Registry) AddWithProgress(name, repoURL string, reporter BucketProgressReporter) (*Bucket, error) {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return nil, fmt.Errorf("invalid bucket name: %s", name)
	}

	bucketDir := filepath.Join(r.bucketsDir, name)

	if err := r.EnsureGit(); err != nil {
		return nil, err
	}

	var onGit git.ProgressCallback
	if reporter != nil {
		onGit = func(msg string, percent float64) {
			reporter(BucketProgressEvent{
				Phase:           "clone",
				MessageFallback: msg,
				Percent:         percent,
			})
		}
	}

	if err := r.git.CloneOrPullWithProgress(repoURL, bucketDir, true, onGit); err != nil {
		return nil, fmt.Errorf("clone bucket '%s': %w", name, err)
	}

	bucket := &Bucket{
		Name:    name,
		RepoURL: repoURL,
		Root:    bucketDir,
		Updated: false,
	}

	r.mu.Lock()
	r.buckets[name] = bucket
	r.mu.Unlock()
	return bucket, nil
}

// Remove removes a bucket.
func (r *Registry) Remove(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket, exists := r.buckets[name]
	if !exists {
		return fmt.Errorf("bucket not found: %s", name)
	}

	if err := os.RemoveAll(bucket.Root); err != nil {
		return fmt.Errorf("remove bucket directory: %w", err)
	}

	delete(r.buckets, name)
	return nil
}

// Get returns a bucket by name.
func (r *Registry) Get(name string) (*Bucket, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket, exists := r.buckets[name]
	if !exists {
		return nil, fmt.Errorf("bucket not found: %s", name)
	}
	return bucket, nil
}

// List returns all buckets.
func (r *Registry) List() []*Bucket {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var buckets []*Bucket
	for _, b := range r.buckets {
		buckets = append(buckets, b)
	}
	return buckets
}

// ReloadFromDisk reloads buckets from the filesystem.
// Git repositories get remote URLs when available; other bucket directories are
// still registered for manifest lookup (legacy manifest.BucketManager behavior).
func (r *Registry) ReloadFromDisk() error {
	entries, err := os.ReadDir(r.bucketsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.buckets = make(map[string]*Bucket)

	for _, entry := range entries {
		name := entry.Name()
		bucketDir := filepath.Join(r.bucketsDir, name)
		// Use os.Stat because Windows directory junctions are not always dirs in DirEntry.
		info, err := os.Stat(bucketDir)
		if err != nil || !info.IsDir() {
			continue
		}

		repoURL := ""
		isRepo := false
		if r.git.Check() == nil {
			isRepo = r.git.IsRepository(bucketDir)
		} else if _, err := os.Stat(filepath.Join(bucketDir, ".git")); err == nil {
			isRepo = true
		}
		if isRepo {
			if url, err := r.git.GetRemoteURL(bucketDir); err == nil {
				repoURL = url
			}
		}

		r.buckets[name] = &Bucket{
			Name:    name,
			RepoURL: repoURL,
			Root:    bucketDir,
			Updated: false,
		}
	}

	return nil
}

// Update updates all or specific buckets.
func (r *Registry) Update(names []string) error {
	return r.updateBuckets(names, false)
}

// UpdateSilent updates buckets without console output.
func (r *Registry) UpdateSilent(names []string) error {
	return r.updateBuckets(names, true)
}

// UpdateWithProgress updates buckets and reports progress.
func (r *Registry) UpdateWithProgress(names []string, reporter BucketProgressReporter) error {
	if err := r.EnsureGit(); err != nil {
		return err
	}

	targets := names
	if len(targets) == 0 {
		for _, bucket := range r.List() {
			targets = append(targets, bucket.Name)
		}
	}

	for i, name := range targets {
		bucket, err := r.Get(name)
		if err != nil {
			return err
		}

		if reporter != nil {
			pct := float64(i) / float64(len(targets)) * 100
			reporter(BucketProgressEvent{
				Phase:   "update",
				Percent: pct,
			})
		}

		if err := r.updateOne(bucket, false); err != nil {
			return err
		}
	}

	if reporter != nil {
		reporter(BucketProgressEvent{
			Phase:   "complete",
			Percent: 100,
		})
	}

	return nil
}

// updateBuckets updates all or specific buckets with optional silent mode.
func (r *Registry) updateBuckets(names []string, silent bool) error {
	targets := names
	if len(targets) == 0 {
		for _, bucket := range r.List() {
			targets = append(targets, bucket.Name)
		}
	}

	for _, name := range targets {
		bucket, err := r.Get(name)
		if err != nil {
			return err
		}
		if err := r.updateOne(bucket, silent); err != nil {
			return err
		}
	}

	return nil
}

// updateOne updates a single bucket with optional silent mode for output control.
func (r *Registry) updateOne(bucket *Bucket, silent bool) error {
	if !silent {
		verbose.Progressf("Updating '%s'...\n", bucket.Name)
	}

	if err := r.git.Pull(bucket.Root); err != nil {
		return fmt.Errorf("update '%s': %w", bucket.Name, err)
	}

	if !silent {
		verbose.Progressf("  %s %s updated\n", colorGreen+"✓"+colorReset, bucket.Name)
	}

	bucket.Updated = false
	return nil
}

// CheckUpdate checks whether a single bucket has upstream updates.
func (r *Registry) CheckUpdate(name string) (git.UpdateStatus, error) {
	b, err := r.Get(name)
	if err != nil {
		return git.UpdateStatus{ErrMsg: err.Error()}, err
	}

	if err := r.EnsureGit(); err != nil {
		return git.UpdateStatus{ErrMsg: err.Error()}, err
	}

	status, err := r.git.CheckUpdateStatus(b.Root)
	if status.OK && status.HasUpdates {
		b.Updated = true
	}
	return status, err
}

// CheckUpdates checks if buckets have updates available (concurrent).
func (r *Registry) CheckUpdates() (map[string]git.UpdateStatus, error) {
	buckets := r.List()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = make(map[string]git.UpdateStatus, len(buckets))
	)

	for _, bucket := range buckets {
		wg.Add(1)
		go func(b *Bucket) {
			defer wg.Done()
			status, err := r.CheckUpdate(b.Name)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && status.LocalCommit == "" && status.ErrMsg == "" {
				status.ErrMsg = err.Error()
			}
			results[b.Name] = status
		}(bucket)
	}
	wg.Wait()

	return results, nil
}

// CurrentCommit returns the local HEAD commit for a bucket.
func (r *Registry) CurrentCommit(name string) (string, error) {
	b, err := r.Get(name)
	if err != nil {
		return "", err
	}
	return r.git.GetCurrentCommit(b.Root)
}

// FindManifest locates a package manifest across all buckets.
// Returns (bucketName, manifestPath, manifest, error).
func (r *Registry) FindManifest(pkgRef string) (string, string, *manifest.Manifest, error) {
	var bucketName, pkgName string

	if at := strings.LastIndex(pkgRef, "@"); at >= 0 {
		pkgRef = pkgRef[:at]
	}

	if strings.Contains(pkgRef, "/") {
		parts := strings.SplitN(pkgRef, "/", 2)
		bucketName, pkgName = parts[0], parts[1]
	} else {
		pkgName = pkgRef
		bucketName = "main"
	}

	r.mu.RLock()
	bucket, exists := r.buckets[bucketName]
	r.mu.RUnlock()
	if !exists {
		return "", "", nil, fmt.Errorf("bucket not found: %s", bucketName)
	}

	searchPaths := manifest.BucketManifestCandidatePaths(bucket.Root, bucketName, pkgName)

	for _, manifestPath := range searchPaths {
		if _, err := os.Stat(manifestPath); err == nil {
			m, err := manifest.ParseFile(manifestPath)
			if err != nil {
				return "", "", nil, fmt.Errorf("parse manifest: %w", err)
			}
			return bucketName, manifestPath, m, nil
		}
	}

	return "", "", nil, &apperr.ManifestNotFound{Name: pkgRef, Searched: bucket.Root}
}

// GetManifestPath is a convenience wrapper that returns (manifestPath, manifest, error).
func (r *Registry) GetManifestPath(pkgRef string) (string, *manifest.Manifest, error) {
	_, path, m, err := r.FindManifest(pkgRef)
	return path, m, err
}
