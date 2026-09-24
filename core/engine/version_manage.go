package engine

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/install"
)

const metadataVersionLocked = "versionLocked"

// resolveInstalledVersion prefers the on-disk active version and heals stale index rows.
func (e *Engine) resolveInstalledVersion(pkgName, indexedVersion string) string {
	if e == nil || e.Config == nil {
		return indexedVersion
	}
	pkgRoot := apps.PkgRoot(e.Config.RootDir, pkgName)
	current, err := apps.ReadCurrent(pkgRoot)
	if err != nil || current == "" {
		return indexedVersion
	}
	if current != indexedVersion && e.Cache != nil {
		installDir := filepath.Join(pkgRoot, current)
		_ = e.Cache.SetActiveVersionInstallDir(pkgName, current, installDir)
	}
	return current
}

// PackageVersionEntry describes one installed version on disk.
type PackageVersionEntry struct {
	Version string `json:"version"`
	Active  bool   `json:"active"`
}

// PackageVersionsInfo lists installed versions and lock state for a package.
type PackageVersionsInfo struct {
	Name          string                `json:"name"`
	ActiveVersion string                `json:"activeVersion"`
	VersionLocked bool                  `json:"versionLocked"`
	Versions      []PackageVersionEntry `json:"versions"`
}

func versionLockedFromMetadata(metadata map[string]interface{}) bool {
	if metadata == nil {
		return false
	}
	v, ok := metadata[metadataVersionLocked]
	if !ok {
		return false
	}
	locked, ok := v.(bool)
	return ok && locked
}

// IsPackageVersionLocked reports whether upgrades are blocked for the package.
func (e *Engine) IsPackageVersionLocked(pkgName string) bool {
	inst, ok := e.Cache.GetInstalled(pkgName)
	if !ok {
		return false
	}
	return versionLockedFromMetadata(inst.Metadata)
}

// SetPackageVersionLock toggles version lock (skip upgrade checks and implicit upgrades).
func (e *Engine) SetPackageVersionLock(pkgName string, locked bool) error {
	inst, ok := e.Cache.GetInstalled(pkgName)
	if !ok {
		return fmt.Errorf("%s is not installed", pkgName)
	}
	if err := e.Cache.SetVersionLocked(pkgName, locked); err != nil {
		return err
	}
	op := "version_unlock"
	if locked {
		op = "version_lock"
	}
	label := pkgName
	if inst.Version != "" {
		label = fmt.Sprintf("%s@%s", pkgName, inst.Version)
	}
	_ = e.Cache.RecordActivity(op, label, inst.Version, "success", map[string]interface{}{
		"locked": locked,
	})
	return nil
}

// GetPackageVersions returns all on-disk versions and the active one.
func (e *Engine) GetPackageVersions(pkgName string) (*PackageVersionsInfo, error) {
	pkgRoot := apps.PkgRoot(e.Config.RootDir, pkgName)
	versions, err := apps.ListVersions(pkgRoot)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%s is not installed", pkgName)
	}

	// Newest versions first for UI display.
	sort.Slice(versions, func(i, j int) bool {
		return versionCompare(versions[i], versions[j]) > 0
	})
	active, _ := apps.ReadCurrent(pkgRoot)
	if active == "" {
		active, _ = apps.EnsureCurrent(pkgRoot)
	}

	info := &PackageVersionsInfo{
		Name:          pkgName,
		ActiveVersion: active,
		Versions:      make([]PackageVersionEntry, 0, len(versions)),
	}
	if inst, ok := e.Cache.GetInstalled(pkgName); ok {
		info.VersionLocked = versionLockedFromMetadata(inst.Metadata)
	}

	for _, ver := range versions {
		info.Versions = append(info.Versions, PackageVersionEntry{
			Version: ver,
			Active:  ver == active,
		})
	}
	return info, nil
}

// SwitchPackageVersion activates a previously installed version (rollback).
func (e *Engine) SwitchPackageVersion(pkgName, version string) error {
	if version == "" {
		return fmt.Errorf("version is required")
	}
	fromVersion := ""
	pkgRoot := apps.PkgRoot(e.Config.RootDir, pkgName)
	if active, err := apps.ReadCurrent(pkgRoot); err == nil && active != "" {
		fromVersion = active
	} else if inst, ok := e.Cache.GetInstalled(pkgName); ok {
		fromVersion = inst.Version
	}

	root := e.Config.RootDir
	appsDir := filepath.Join(root, "apps")
	shimsMetaDir := filepath.Join(root, "shims-meta")
	err := install.ResetPackage(e.Engine, appsDir, shimsMetaDir, pkgName+"@"+version)
	details := map[string]interface{}{
		"from": fromVersion,
		"to":   version,
	}
	if err != nil {
		details["error"] = err.Error()
		_ = e.Cache.RecordActivity("version_switch", pkgName, version, "failed", details)
		return err
	}
	_ = e.Cache.RecordActivity("version_switch", pkgName, version, "success", details)
	return nil
}

// ResetPackage switches the active version and rebuilds shims and shell integration (glue reset).
func (e *Engine) ResetPackage(pkgRef string) error {
	root := e.Config.RootDir
	appsDir := filepath.Join(root, "apps")
	shimsMetaDir := filepath.Join(root, "shims-meta")
	return install.ResetPackage(e.Engine, appsDir, shimsMetaDir, pkgRef)
}
