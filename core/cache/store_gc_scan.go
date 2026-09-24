package cache

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/store"
)

type storeObjectEntry struct {
	path string
	hash string
	size int64
}

func countStoreShards(store *store.Store) int {
	entries, err := os.ReadDir(store.Path())
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			n++
		}
	}
	return n
}

// scanStoreParallel walks the cache store once: builds the hardlink key index and lists every object.
// Progress: one work unit per top-level shard completed.
func scanStoreParallel(store *store.Store, prog *gcProgress) (map[fileRefKey]string, []storeObjectEntry, error) {
	index := make(map[fileRefKey]string)
	root := store.Path()
	displayRoot := FriendlyDisplayPath(root)

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}

	var shards []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		shards = append(shards, name)
	}

	workers := max(gcWorkers(len(shards)), maxGCWorkers)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var indexMu sync.Mutex
	var objectsMu sync.Mutex
	var objects []storeObjectEntry
	var firstErr error
	var errOnce sync.Once
	var shardsDone atomic.Int32
	var objectsScanned atomic.Int64

	recordObject := func(path string, info os.FileInfo) {
		hash, ok := hashFromStorePath(root, path)
		if !ok {
			return
		}
		objectsScanned.Add(1)
		key, keyOK := fileRefKeyForPath(path)
		if keyOK {
			indexMu.Lock()
			index[key] = hash
			indexMu.Unlock()
		}
		objectsMu.Lock()
		objects = append(objects, storeObjectEntry{
			path: path,
			hash: hash,
			size: info.Size(),
		})
		objectsMu.Unlock()
	}

	for _, name := range shards {
		// name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			entryPath := filepath.Join(root, name)
			info, statErr := os.Stat(entryPath)
			if statErr != nil {
				return
			}
			if info.IsDir() && len(name) == 2 {
				walkErr := filepath.Walk(entryPath, func(path string, fi os.FileInfo, walkErr error) error {
					if walkErr != nil || fi.IsDir() {
						return nil
					}
					recordObject(path, fi)
					return nil
				})
				if walkErr != nil {
					errOnce.Do(func() { firstErr = walkErr })
				}
			} else if !info.IsDir() {
				recordObject(entryPath, info)
			}

			done := int(shardsDone.Add(1))
			if prog != nil {
				prog.complete(1)
				prog.reportStoreShard(done, len(shards), int(objectsScanned.Load()), displayRoot)
			}
		}()
	}
	wg.Wait()
	return index, objects, firstErr
}

func filterUnreferencedStoreObjects(objects []storeObjectEntry, referenced map[string]bool) []storeOrphan {
	orphans := make([]storeOrphan, 0)
	for _, obj := range objects {
		if referenced[obj.hash] {
			continue
		}
		orphans = append(orphans, storeOrphan{path: obj.path, size: obj.size})
	}
	return orphans
}
