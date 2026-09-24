package engine

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/apps"
)

// InstalledPackageDetail is metadata for one installed package (glue info / UI clients).
type InstalledPackageDetail struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	InstallPath     string   `json:"installPath"`
	CurrentPath     string   `json:"currentPath"`
	InstalledAt     string   `json:"installedAt,omitempty"`
	Size            int64    `json:"size"`
	FileCount       int      `json:"fileCount"`
	Shims           []string `json:"shims,omitempty"`
	Bucket          string   `json:"bucket,omitempty"`
	Description     string   `json:"description,omitempty"`
	Homepage        string   `json:"homepage,omitempty"`
	License         string   `json:"license,omitempty"`
	Depends         []string `json:"depends,omitempty"`
	Notes           []string `json:"notes,omitempty"`
	ManifestVersion   string   `json:"manifestVersion,omitempty"`
	UpdateAvailable   bool     `json:"updateAvailable,omitempty"`
	DetectedVersion   string   `json:"detectedVersion,omitempty"`
	VersionDetectSrc  string   `json:"versionDetectSrc,omitempty"`
	ExternallyUpdated bool     `json:"externallyUpdated,omitempty"`
}

// GetInstalledPackageDetail returns install path, cache stats, shims, and manifest metadata.
func (e *Engine) GetInstalledPackageDetail(pkgName string) (*InstalledPackageDetail, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	root := e.Config.RootDir
	pkgRoot := apps.PkgRoot(root, pkgName)
	version, err := apps.EnsureCurrent(pkgRoot)
	if err != nil || version == "" {
		return nil, &apperr.PackageNotInstalled{Name: pkgName}
	}

	installPath := filepath.Join(pkgRoot, version)
	currentPath := filepath.Join(pkgRoot, apps.CurrentLinkName)

	detail := &InstalledPackageDetail{
		Name:        pkgName,
		Version:     version,
		InstallPath: installPath,
		CurrentPath: currentPath,
	}

	if entry, ok := e.Cache.Get(pkgName); ok {
		detail.InstalledAt = entry.Installed
		detail.Size = entry.Size
		detail.FileCount = len(entry.Files)
	} else {
		fileCount, dirSize := apps.CountInstallDir(installPath)
		detail.Size = dirSize
		detail.FileCount = fileCount
	}

	if shims, err := e.listShimNamesForPackage(pkgName); err == nil && len(shims) > 0 {
		detail.Shims = shims
	}

	bucketName, _, m, err := e.BucketRegistry.FindManifest(pkgName)
	if err != nil {
		return detail, nil
	}
	if m != nil {
		detail.Bucket = bucketName
		detail.Description = m.Description
		detail.Homepage = m.Homepage
		detail.License = m.GetLicense()
		detail.Depends = append([]string(nil), m.Depends...)
		detail.Notes = append([]string(nil), m.GetNotes()...)
		detail.ManifestVersion = m.Version
		if m.Version != "" && m.Version != version {
			detail.UpdateAvailable = true
		}
	}

	detected := e.detectPackageVersion(pkgName, version)
	detail.DetectedVersion = detected.DetectedVersion
	detail.VersionDetectSrc = detected.Source
	detail.ExternallyUpdated = detected.ExternallyUpdated

	return detail, nil
}

func (e *Engine) listShimNamesForPackage(pkgName string) ([]string, error) {
	if e == nil || e.ShimMgr == nil {
		return nil, nil
	}
	configs, err := e.ShimMgr.List()
	if err != nil {
		return nil, err
	}
	var shims []string
	for _, cfg := range configs {
		name, _ := apps.ParseInstallFilePath(cfg.Path)
		if name != pkgName {
			continue
		}
		shims = append(shims, cfg.Name)
	}
	sort.Strings(shims)
	return shims, nil
}
