package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/device"
	"github.com/gluestick-sh/core/snapshot"
)

// ExportCoreSnapshot captures device identity + installed packages, buckets, and syncable config.
func (e *Engine) ExportCoreSnapshot(meta snapshot.Meta) (*snapshot.Snapshot, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	info, err := e.DeviceInfo()
	if err != nil {
		return nil, err
	}
	core, err := e.captureCoreState(context.Background())
	if err != nil {
		return nil, err
	}

	id := strings.TrimSpace(meta.ID)
	if id == "" {
		id, err = snapshot.NewID()
		if err != nil {
			return nil, err
		}
	}
	source := strings.TrimSpace(meta.Source)
	if source == "" {
		source = snapshot.SourceManual
	}

	return &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Kind:          snapshot.Kind,
		ID:            id,
		CreatedAt:     snapshot.NowRFC3339(),
		Source:        source,
		Notes:         strings.TrimSpace(meta.Notes),
		Device:        snapshotDevice(info),
		Core:          *core,
	}, nil
}

// DiffCoreSnapshot compares the current engine state to target and returns an apply plan.
func (e *Engine) DiffCoreSnapshot(target *snapshot.Snapshot, opts snapshot.ApplyOptions) (*snapshot.Plan, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if err := snapshot.Validate(target); err != nil {
		return nil, err
	}
	current, err := e.captureCoreState(context.Background())
	if err != nil {
		return nil, err
	}
	return snapshot.Diff(*current, target.Core, opts)
}

// ApplyCoreSnapshot applies target to the local engine.
// DryRun returns the plan without mutating state.
// MVP supports install-missing only (reconcile remove actions are rejected).
func (e *Engine) ApplyCoreSnapshot(ctx context.Context, target *snapshot.Snapshot, opts snapshot.ApplyOptions, reporter ProgressReporter) (*snapshot.Plan, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if err := snapshot.Validate(target); err != nil {
		return nil, err
	}
	mode := snapshot.NormalizeMode(opts.Mode)
	if mode == snapshot.ApplyModeReconcile {
		return nil, fmt.Errorf("apply mode %q is not enabled yet; use %q", snapshot.ApplyModeReconcile, snapshot.ApplyModeInstallMissing)
	}
	opts.Mode = mode

	plan, err := e.DiffCoreSnapshot(target, opts)
	if err != nil {
		return nil, err
	}
	if opts.DryRun || plan.Empty() {
		return plan, nil
	}
	if reporter == nil {
		reporter = NewSilentReporter()
	}

	root := e.Config.RootDir

	for _, b := range plan.BucketsToAdd {
		url, err := snapshot.ResolveBucketURL(b.Name, b.URL, bucket.GetKnownBucketURL)
		if err != nil {
			return plan, err
		}
		if _, err := e.BucketRegistry.Add(b.Name, url); err != nil {
			return plan, fmt.Errorf("add bucket %q: %w", b.Name, err)
		}
		e.ReloadBuckets(false)
		_ = e.RecordBucketAddActivity(b.Name, "success", "")
	}

	for _, change := range plan.ConfigChanges {
		switch change.Key {
		case "githubProxy":
			if err := config.WriteConfigGitHubProxy(root, change.To); err != nil {
				return plan, err
			}
			e.ReloadGitHubProxies()
		case "downloadWorkers":
			n, err := parseWorkers(change.To)
			if err != nil {
				return plan, err
			}
			if err := config.WriteConfigDownloadWorkers(root, n); err != nil {
				return plan, err
			}
		case "bucketSyncMode":
			if err := config.WriteConfigBucketSyncMode(root, change.To); err != nil {
				return plan, err
			}
		default:
			return plan, fmt.Errorf("unknown config change key %q", change.Key)
		}
	}

	var applyErrs []error
	for _, pkg := range plan.PackagesToInstall {
		ref := snapshot.InstallRef(pkg)
		result, err := e.Install(ctx, &InstallRequest{Request: Request{Name: ref}}, reporter)
		if err != nil {
			applyErrs = append(applyErrs, fmt.Errorf("install %s: %w", ref, err))
			continue
		}
		if result != nil && result.Status == StatusFailed {
			if result.Error != nil {
				applyErrs = append(applyErrs, fmt.Errorf("install %s: %w", ref, result.Error))
			} else {
				applyErrs = append(applyErrs, fmt.Errorf("install %s failed: %s", ref, result.Message))
			}
			continue
		}
		if pkg.VersionLocked {
			if err := e.SetPackageVersionLock(pkg.Name, true); err != nil {
				applyErrs = append(applyErrs, fmt.Errorf("lock %s: %w", pkg.Name, err))
				continue
			}
		}
	}

	for _, pkg := range plan.PackagesToActivate {
		if err := e.SwitchPackageVersion(pkg.Name, pkg.Version); err != nil {
			applyErrs = append(applyErrs, fmt.Errorf("activate %s@%s: %w", pkg.Name, pkg.Version, err))
			continue
		}
		if pkg.VersionLocked {
			if err := e.SetPackageVersionLock(pkg.Name, true); err != nil {
				applyErrs = append(applyErrs, fmt.Errorf("lock %s: %w", pkg.Name, err))
				continue
			}
		}
	}

	if len(applyErrs) > 0 {
		return plan, errors.Join(applyErrs...)
	}
	return plan, nil
}

func (e *Engine) captureCoreState(ctx context.Context) (*snapshot.CoreState, error) {
	_ = ctx
	all, err := e.ListInstalledAllVersions(nil)
	if err != nil {
		return nil, fmt.Errorf("list installed packages: %w", err)
	}
	root := e.Config.RootDir
	packages := make([]snapshot.Package, 0)
	for _, pkg := range all {
		name := strings.TrimSpace(pkg.Name)
		if name == "" || len(pkg.Versions) == 0 {
			continue
		}
		locked := e.IsPackageVersionLocked(name)
		pkgRoot := apps.PkgRoot(root, name)
		for _, ver := range pkg.Versions {
			ver = strings.TrimSpace(ver)
			if ver == "" {
				continue
			}
			bucketName := ""
			if rec, err := apps.LoadInstallRecord(filepath.Join(pkgRoot, ver)); err == nil && rec != nil {
				bucketName = strings.TrimSpace(rec.Bucket)
			}
			entry := snapshot.Package{
				Name:    name,
				Bucket:  bucketName,
				Version: ver,
			}
			if ver == pkg.Current {
				entry.Current = true
				if locked {
					entry.VersionLocked = true
				}
			}
			packages = append(packages, entry)
		}
	}
	sort.Slice(packages, func(i, j int) bool {
		a, b := packages[i], packages[j]
		an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if an != bn {
			return an < bn
		}
		return a.Version < b.Version
	})

	buckets := []snapshot.Bucket{}
	if e.BucketRegistry != nil {
		for _, b := range e.BucketRegistry.List() {
			if b == nil || strings.TrimSpace(b.Name) == "" {
				continue
			}
			buckets = append(buckets, snapshot.Bucket{
				Name: b.Name,
				URL:  strings.TrimSpace(b.RepoURL),
			})
		}
	}
	sort.Slice(buckets, func(i, j int) bool {
		return strings.ToLower(buckets[i].Name) < strings.ToLower(buckets[j].Name)
	})

	cfg, err := e.captureConfig()
	if err != nil {
		return nil, err
	}

	return &snapshot.CoreState{
		Packages: packages,
		Buckets:  buckets,
		Config:   cfg,
	}, nil
}

func (e *Engine) captureConfig() (snapshot.Config, error) {
	root := e.Config.RootDir
	out := snapshot.Config{}
	proxy, err := config.ReadConfigGitHubProxy(root)
	if err != nil {
		return out, err
	}
	out.GitHubProxy = strings.TrimSpace(proxy)

	if workers, ok, err := config.ReadConfigDownloadWorkers(root); err != nil {
		return out, err
	} else if ok {
		w := workers
		out.DownloadWorkers = &w
	}

	if mode, ok, err := config.ReadConfigBucketSyncMode(root); err != nil {
		return out, err
	} else if ok {
		out.BucketSyncMode = mode
	}
	return out, nil
}

func snapshotDevice(info *device.Info) snapshot.Device {
	if info == nil {
		return snapshot.Device{}
	}
	return snapshot.Device{
		DeviceID:    info.DeviceID,
		DisplayName: info.DisplayName,
		Hostname:    info.Platform.Hostname,
		OS:          info.Platform.OS,
		Arch:        info.Platform.Arch,
	}
}

func parseWorkers(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("downloadWorkers value is empty")
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 {
		return 0, fmt.Errorf("invalid downloadWorkers %q", s)
	}
	return n, nil
}
