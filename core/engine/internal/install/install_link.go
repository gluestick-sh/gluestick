package install

import (
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"sync"
	"sync/atomic"

	eruntime "github.com/gluestick-sh/core/engine/internal/runtime"

	"github.com/gluestick-sh/core/archmember"
	"github.com/gluestick-sh/core/safepath"
	"github.com/gluestick-sh/core/store"
)

type linkExtractItem struct {
	relPath string
	hash    string
}

// LinkExtractedFiles hardlinks cache store blobs into installDir. Fails on first link/mkdir error.
// Returns the number of files linked (non-hidden paths with a non-empty target).
func LinkExtractedFiles(store *store.Store, installDir, extractTo, extractDir string, files map[string]string, recordFile func(hash, rel string)) (int, error) {
	items := make([]linkExtractItem, 0, len(files))
	for relPath, hash := range files {
		if eruntime.IsHiddenInstallPath(relPath) || archmember.IsDirectoryPlaceholderName(relPath) {
			continue
		}
		items = append(items, linkExtractItem{
			relPath: archmember.NormalizeMember(relPath),
			hash:    hash,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		di, dj := archmember.Depth(items[i].relPath), archmember.Depth(items[j].relPath)
		if di != dj {
			return di < dj
		}
		return items[i].relPath < items[j].relPath
	})

	type linkJob struct {
		item          linkExtractItem
		targetRelPath string
		targetPath    string
	}

	jobs := make([]linkJob, 0, len(items))
	dirSet := make(map[string]struct{})
	for _, item := range items {
		targetRelPath, err := installMemberRelPath(extractTo, extractDir, item.relPath)
		if err != nil {
			return 0, err
		}
		if targetRelPath == "" {
			continue
		}
		targetPath, err := safepath.JoinUnderBase(installDir, targetRelPath)
		if err != nil {
			return 0, fmt.Errorf("link %s: %w", item.relPath, err)
		}
		jobs = append(jobs, linkJob{item: item, targetRelPath: targetRelPath, targetPath: targetPath})
		for dir := filepath.Dir(targetPath); dir != "" && dir != "."; {
			dirSet[dir] = struct{}{}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if len(jobs) == 0 {
		return 0, nil
	}

	dirs := make([]string, 0, len(dirSet))
	for dir := range dirSet {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		di, dj := archmember.Depth(filepath.ToSlash(dirs[i])), archmember.Depth(filepath.ToSlash(dirs[j]))
		if di != dj {
			return di < dj
		}
		return dirs[i] < dirs[j]
	})
	created := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if err := ensureLinkParentDirCached(dir, created); err != nil {
			return 0, err
		}
	}

	workers := linkWorkers(len(jobs))
	if workers <= 1 {
		var linked int
		for _, job := range jobs {
			if err := store.Link(job.item.hash, job.targetPath); err != nil {
				return linked, fmt.Errorf("link %s: %w", job.item.relPath, err)
			}
			if recordFile != nil {
				recordFile(job.item.hash, job.targetRelPath)
			}
			linked++
		}
		return linked, nil
	}

	var linked atomic.Int64
	var firstErr error
	var errOnce sync.Once
	var recordMu sync.Mutex

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job linkJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if err := store.Link(job.item.hash, job.targetPath); err != nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("link %s: %w", job.item.relPath, err) })
				return
			}
			if recordFile != nil {
				recordMu.Lock()
				recordFile(job.item.hash, job.targetRelPath)
				recordMu.Unlock()
			}
			linked.Add(1)
		}(job)
	}
	wg.Wait()
	if firstErr != nil {
		return int(linked.Load()), firstErr
	}
	return int(linked.Load()), nil
}

func linkWorkers(itemCount int) int {
	if itemCount <= 1 {
		return 1
	}
	n := goruntime.NumCPU()
	if itemCount > 5000 {
		n *= 4
	} else if itemCount > 1000 {
		n *= 2
	}
	if n < 2 {
		n = 2
	}
	if n > 32 {
		n = 32
	}
	if n > itemCount {
		n = itemCount
	}
	return n
}

// ensureLinkParentDirCached creates parent directories once per install pass.
func ensureLinkParentDirCached(dir string, created map[string]struct{}) error {
	dir = filepath.Clean(dir)
	if dir == "" || dir == "." {
		return nil
	}
	if _, ok := created[dir]; ok {
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := ensureLinkParentDirCached(parent, created); err != nil {
			return err
		}
	}
	if err := ensureLinkParentDir(dir); err != nil {
		return err
	}
	created[dir] = struct{}{}
	return nil
}

// ensureLinkParentDir creates parent directories, removing zero-byte zip folder markers that block mkdir on Windows.
func ensureLinkParentDir(dir string) error {
	dir = filepath.Clean(dir)
	if dir == "" || dir == "." {
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := ensureLinkParentDir(parent); err != nil {
			return err
		}
	}
	if info, err := os.Stat(dir); err == nil {
		if info.IsDir() {
			return nil
		}
		if info.Size() == 0 {
			if err := os.Remove(dir); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("path exists as file: %s", dir)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.MkdirAll(dir, 0755)
}
