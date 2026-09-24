package install

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/archmember"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/store"
)

type adoptInstallProgressFunc func(processed, total int64)

// adoptInstallDirToStore hashes files under installDir and hardlinks them into cache store.
// Returns relPath→hash (zip index layout) and total uncompressed bytes adopted.
func adoptInstallDirToStore(store *store.Store, installDir string, onProgress adoptInstallProgressFunc) (map[string]string, int64, error) {
	type fileJob struct {
		absPath string
		relPath string
		size    int64
	}
	var jobs []fileJob
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
		jobs = append(jobs, fileJob{
			absPath: path,
			relPath: archmember.NormalizeMember(filepath.ToSlash(relPath)),
			size:    info.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if len(jobs) == 0 {
		return nil, 0, fmt.Errorf("no files to adopt under %s", installDir)
	}

	total := int64(len(jobs))
	if onProgress != nil {
		onProgress(0, total)
	}

	files := make(map[string]string, len(jobs))
	var adoptedBytes atomic.Int64
	workers := adoptWorkers(len(jobs))
	if workers <= 1 {
		for i, job := range jobs {
			hash, err := store.Adopt(job.absPath)
			if err != nil {
				return nil, 0, err
			}
			files[job.relPath] = hash
			adoptedBytes.Add(job.size)
			if onProgress != nil {
				n := int64(i + 1)
				if n%100 == 0 || n == total {
					onProgress(n, total)
				}
			}
		}
		if onProgress != nil {
			onProgress(total, total)
		}
		return files, adoptedBytes.Load(), nil
	}

	var processed atomic.Int64
	var mu sync.Mutex
	var firstErr error
	var errOnce sync.Once
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job fileJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			hash, err := store.Adopt(job.absPath)
			if err != nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("adopt %s: %w", job.relPath, err) })
				return
			}
			mu.Lock()
			files[job.relPath] = hash
			mu.Unlock()
			adoptedBytes.Add(job.size)
			if onProgress != nil {
				if n := processed.Add(1); n%100 == 0 || n == total {
					onProgress(n, total)
				}
			}
		}(job)
	}
	wg.Wait()
	if onProgress != nil {
		onProgress(total, total)
	}
	if firstErr != nil {
		return nil, 0, firstErr
	}
	return files, adoptedBytes.Load(), nil
}

func adoptWorkers(itemCount int) int {
	if itemCount <= 1 {
		return 1
	}
	n := runtime.NumCPU()
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
