// Package runtime holds the shared engine state and the low-level package,
// path, and search-index helpers used across the engine's internal packages.
package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/bootstrap"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/device"
	"github.com/gluestick-sh/core/downloader"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/extractor"
	"github.com/gluestick-sh/core/shim"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
)

// Engine holds shared runtime state for package operations.
type Engine struct {
	Config         *etypes.EngineConfig
	Store          *store.Store
	Cache          *cache.Index
	Downloader     *downloader.Downloader
	Extractor      *extractor.Extractor
	Bootstrap      *bootstrap.Bootstrap
	BucketRegistry *bucket.Registry
	ShimMgr        *shim.Manager
	SearchIdx      *Index
	SearchIdxReady atomic.Bool
	Stats          *etypes.EngineStats
	LaunchIndexMu  sync.Mutex
}

// NewEngine creates a new package engine instance.
func NewEngine(cfg *etypes.EngineConfig) (*Engine, error) {
	verbose.Set(cfg.Verbose)

	if _, err := device.Ensure(cfg.RootDir); err != nil {
		return nil, fmt.Errorf("failed to initialize device identity: %w", err)
	}

	st, err := store.NewStore(filepath.Join(cfg.RootDir, "store"))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cache store: %w", err)
	}
	if err := st.Prereqs(); err != nil {
		return nil, fmt.Errorf("failed to initialize cache store layout: %w", err)
	}

	cacheIdx, err := cache.NewIndex(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cache index: %w", err)
	}
	if err := cacheIdx.SyncInstalledFromPackages(cfg.RootDir); err != nil {
		return nil, fmt.Errorf("sync installed_packages: %w", err)
	}
	if _, err := cacheIdx.PruneUninstalledPackages(cfg.RootDir); err != nil {
		return nil, fmt.Errorf("prune cache index: %w", err)
	}

	dl := downloader.NewDownloader(st,
		downloader.WithWorkers(cfg.Workers),
		downloader.WithGitHubProxies(config.LoadProxies(cfg.RootDir)),
		downloader.WithParallelDownload(cfg.Parallel),
	)

	boot := bootstrap.NewBootstrap(cfg.RootDir)
	ext := extractor.NewExtractor(st)
	ext.SetWorkers(cfg.Workers)

	bucketRegistry, err := bucket.NewRegistry(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize bucket registry: %w", err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		return nil, fmt.Errorf("failed to reload buckets: %w", err)
	}

	shimMgr, err := shim.NewManager(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize shim manager: %w", err)
	}

	engine := &Engine{
		Config:         cfg,
		Store:          st,
		Cache:          cacheIdx,
		Downloader:     dl,
		Extractor:      ext,
		Bootstrap:      boot,
		BucketRegistry: bucketRegistry,
		ShimMgr:        shimMgr,
		SearchIdx:      NewIndex(),
		Stats:          &etypes.EngineStats{},
	}
	go RebuildSearchIndex(engine)
	return engine, nil
}

// Close releases all resources held by the engine.
func (e *Engine) Close() error {
	if e.Cache != nil {
		return e.Cache.Close()
	}
	return nil
}

// InstalledPackageCount returns the number of packages with an active install under apps/.
func (e *Engine) InstalledPackageCount() int {
	if e == nil || e.Config == nil {
		return 0
	}
	return countInstalledOnDisk(filepath.Join(e.Config.RootDir, "apps"))
}

func countInstalledOnDisk(appsDir string) int {
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pkgRoot := filepath.Join(appsDir, entry.Name())
		if _, _, ok := apps.ActiveInstallDir(pkgRoot); ok {
			n++
		}
	}
	return n
}

// TotalInstalledSize sums indexed package sizes for packages still present under apps/.
func (e *Engine) TotalInstalledSize() int64 {
	if e == nil || e.Config == nil {
		return 0
	}
	appsDir := filepath.Join(e.Config.RootDir, "apps")
	entries := e.Cache.ListPackages()
	var total int64
	for name, entry := range entries {
		pkgRoot := filepath.Join(appsDir, name)
		if _, _, ok := apps.ActiveInstallDir(pkgRoot); ok {
			total += entry.Size
		}
	}
	return total
}

// ReloadGitHubProxies reloads GitHub mirror settings from config and environment.
func (e *Engine) ReloadGitHubProxies() {
	proxies := config.LoadProxies(e.Config.RootDir)
	e.Downloader.SetGitHubProxies(proxies)
	e.Bootstrap.SetGitHubProxies(proxies)
}

// SetWorkers updates parallel download and extract worker counts.
func (e *Engine) SetWorkers(n int) {
	n = downloader.NormalizeUserWorkers(n)
	e.Config.Workers = n
	e.Downloader.SetWorkers(n)
	e.Extractor.SetWorkers(n)
}

// GetStats returns engine statistics.
func (e *Engine) GetStats() *etypes.EngineStats {
	if e == nil || e.Stats == nil {
		return &etypes.EngineStats{}
	}
	stats := *e.Stats
	return &stats
}

// RecordFailedOp records a failed operation and its duration in engine stats.
func (e *Engine) RecordFailedOp(duration time.Duration) {
	if e == nil || e.Stats == nil {
		return
	}
	e.Stats.FailedOps++
	e.Stats.TotalDuration += duration
}

// RecordSuccessfulInstall records a successful package install in engine stats.
func (e *Engine) RecordSuccessfulInstall() {
	if e == nil || e.Stats == nil {
		return
	}
	e.Stats.TotalPackages++
	e.Stats.SuccessfulOps++
}

// RecordSuccessfulOp records a successful non-install operation in engine stats.
func (e *Engine) RecordSuccessfulOp() {
	if e == nil || e.Stats == nil {
		return
	}
	e.Stats.SuccessfulOps++
}
