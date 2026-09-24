// Install phase profiling for `glue install --profile`.
//
// When etypes.InstallRequest.Options["profile"] is "true", installPackageFull wraps major
// steps with installPhaseProfile timers and prints one machine-readable line:
//
//	GLUE_PROFILE download_ms=... cas_ms=... extract_ms=... link_ms=... shim_ms=... cache_ms=... bootstrap_ms=... total_ms=...
//
// cas_ms is the legacy field name for content-store ingest time (store ingest).
//
// Benchmark scripts (benchmark/install-benchmark.ps1) parse that line; parseInstallProfileLine
// supports the same format in tests.
package install

import (
	"regexp"
	"strconv"
	"time"

	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/verbose"
)

// installPhaseProfile accumulates wall-clock time per install phase for one package.
// All methods are nil-safe: when profiling is off, prof is nil and wrappers become no-ops.
type installPhaseProfile struct {
	enabled bool // set from etypes.InstallRequest.Options["profile"]

	Download    time.Duration // HTTP/FTP network time (from downloader.Result.Timing)
	StoreIngest time.Duration // writing downloaded blobs into the content store
	Extract     time.Duration // archive/script extraction into content store or install dir
	Link        time.Duration // hardlinking content store objects into apps/<pkg>/<ver>
	Shim        time.Duration // shim.exe + metadata creation
	Cache       time.Duration // SQLite cache index update
	Bootstrap   time.Duration // on-demand git / 7z / dark / innounp setup
}

// absorbDownloadResults adds per-artifact network and content store ingest times after a download batch.
func (p *installPhaseProfile) absorbDownloadResults(results []downloader.Result) {
	if p == nil {
		return
	}
	for _, r := range results {
		p.Download += r.Timing.Network
		p.StoreIngest += r.Timing.StoreIngest
	}
}

// runBootstrap times bootstrap helper setup (git, 7z, dark, innounp).
func (p *installPhaseProfile) runBootstrap(fn func() error) error {
	if p == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	p.Bootstrap += time.Since(start)
	return err
}

// runExtract times archive or installer-script extraction (7z, innounp, etc.).
func (p *installPhaseProfile) runExtract(fn func() error) error {
	start := time.Now()
	err := fn()
	if p != nil {
		p.Extract += time.Since(start)
	}
	return err
}

// addLink records elapsed time since linkStart for a store→apps hardlink batch.
func (p *installPhaseProfile) addLink(start time.Time) {
	if p == nil {
		return
	}
	p.Link += time.Since(start)
}

// runShim times shim removal/recreation at the end of install.
func (p *installPhaseProfile) runShim(fn func() error) error {
	if p == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	p.Shim += time.Since(start)
	return err
}

// runCache times the post-install cache index write.
func (p *installPhaseProfile) runCache(fn func() error) error {
	if p == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	p.Cache += time.Since(start)
	return err
}

// total returns the sum of phase buckets (not necessarily equal to end-to-end wall time).
func (p *installPhaseProfile) total() time.Duration {
	if p == nil {
		return 0
	}
	return p.Download + p.StoreIngest + p.Extract + p.Link + p.Shim + p.Cache + p.Bootstrap
}

// emit prints the GLUE_PROFILE summary when profiling is enabled for this install.
func (p *installPhaseProfile) emit() {
	if p == nil || !p.enabled {
		return
	}
	verbose.Progressf(
		"GLUE_PROFILE download_ms=%d cas_ms=%d extract_ms=%d link_ms=%d shim_ms=%d cache_ms=%d bootstrap_ms=%d total_ms=%d\n",
		p.Download.Milliseconds(),
		p.StoreIngest.Milliseconds(),
		p.Extract.Milliseconds(),
		p.Link.Milliseconds(),
		p.Shim.Milliseconds(),
		p.Cache.Milliseconds(),
		p.Bootstrap.Milliseconds(),
		p.total().Milliseconds(),
	)
}

// glueProfileLine matches the GLUE_PROFILE line emitted by emit (field order is stable).
var glueProfileLine = regexp.MustCompile(
	`^GLUE_PROFILE download_ms=(\d+) cas_ms=(\d+) extract_ms=(\d+) link_ms=(\d+) shim_ms=(\d+) cache_ms=(\d+) bootstrap_ms=(\d+) total_ms=(\d+)$`,
)

// parsedInstallProfile holds millisecond values from a GLUE_PROFILE line.
type parsedInstallProfile struct {
	DownloadMs  int64
	StoreMs     int64
	ExtractMs   int64
	LinkMs      int64
	ShimMs      int64
	CacheMs     int64
	BootstrapMs int64
	TotalMs     int64
}

// parseInstallProfileLine parses a GLUE_PROFILE log line (used by tests and benchmarks).
func parseInstallProfileLine(line string) (parsedInstallProfile, bool) {
	m := glueProfileLine.FindStringSubmatch(line)
	if m == nil {
		return parsedInstallProfile{}, false
	}
	vals := make([]int64, 8)
	for i := 0; i < 8; i++ {
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil {
			return parsedInstallProfile{}, false
		}
		vals[i] = n
	}
	return parsedInstallProfile{
		DownloadMs:  vals[0],
		StoreMs:     vals[1],
		ExtractMs:   vals[2],
		LinkMs:      vals[3],
		ShimMs:      vals[4],
		CacheMs:     vals[5],
		BootstrapMs: vals[6],
		TotalMs:     vals[7],
	}, true
}
