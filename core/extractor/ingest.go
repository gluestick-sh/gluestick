package extractor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/gluestick-sh/core/progress"
	"github.com/gluestick-sh/core/safepath"
)

type extractedEntry struct {
	path string
	rel  string
}

func listExtractedFiles(root string) ([]extractedEntry, error) {
	var entries []extractedEntry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		slash := filepath.ToSlash(path)
		if strings.Contains(slash, "[DE]") || strings.Contains(slash, "[FT]") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, err := safepath.ValidateManifestRelPath(filepath.ToSlash(rel)); err != nil {
			return fmt.Errorf("extracted file %q: %w", rel, err)
		}
		entries = append(entries, extractedEntry{path: path, rel: rel})
		return nil
	})
	return entries, err
}

func (e *Extractor) filesProgress() IngestProgressFunc {
	if h := progress.HandlerFrom(e.ctx); h != nil && h.Files != nil {
		return h.Files
	}
	return e.ingestProgress
}

func (e *Extractor) extractProgress() func(percent int) {
	if h := progress.HandlerFrom(e.ctx); h != nil && h.Extract != nil {
		return h.Extract
	}
	return nil
}

func (e *Extractor) stageExtractProgress(stage, totalStages int) func(percent int) {
	base := e.extractProgress()
	if base == nil {
		return nil
	}
	return func(percent int) {
		base(scaleExtractPercent(percent, stage, totalStages))
	}
}

func (e *Extractor) finishCompoundExtractProgress() {
	if fn := e.extractProgress(); fn != nil {
		fn(100)
	}
}

// ingestExtractedDir writes extracted files into cache store using a worker pool.
func (e *Extractor) ingestExtractedDir(root string) (map[string]string, time.Duration, error) {
	entries, err := listExtractedFiles(root)
	if err != nil {
		return nil, 0, err
	}
	if len(entries) == 0 {
		return map[string]string{}, 0, nil
	}

	workers := e.workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(entries) {
		workers = len(entries)
	}

	start := time.Now()
	files := make(map[string]string, len(entries))
	var mu sync.Mutex
	total := int64(len(entries))
	var processed atomic.Int64

	filesFn := e.filesProgress()
	var bar *progressbar.ProgressBar
	if filesFn == nil {
		bar = progressbar.Default(total, "processing files")
		defer bar.Close()
	} else {
		filesFn(0, total)
	}

	reportProgress := func() {
		done := processed.Load()
		if bar != nil {
			_ = bar.Set64(done)
		}
		if filesFn != nil {
			filesFn(done, total)
		}
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for _, ent := range entries {
		wg.Add(1)
		go func(ent extractedEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			f, err := os.Open(ent.path)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			hash, err := e.store.Write(f)
			_ = f.Close()
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			mu.Lock()
			files[ent.rel] = hash
			mu.Unlock()
			processed.Add(1)
			reportProgress()
		}(ent)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, time.Since(start), fmt.Errorf("process extracted files: %w", firstErr)
	}
	if filesFn != nil {
		filesFn(total, total)
	}
	return files, time.Since(start), nil
}
