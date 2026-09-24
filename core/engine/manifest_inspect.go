package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/override"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

const maxManifestInspectBytes = 256 << 10

// InstallManifestInfo exposes manifest path, raw JSON, and resolved download URLs for debugging.
type InstallManifestInfo struct {
	ManifestPath           string   `json:"manifestPath"`
	ManifestJSON           string   `json:"manifestJSON"`
	BucketManifestJSON     string   `json:"bucketManifestJSON"`
	Version                string   `json:"version"`
	DownloadURLs           []string `json:"downloadUrls"`
	BucketDownloadURLs     []string `json:"bucketDownloadUrls"`
	URLOverrideActive      bool     `json:"urlOverrideActive"`
	URLOverrideStale       bool     `json:"urlOverrideStale"`
	JSONOverrideActive     bool     `json:"jsonOverrideActive"`
	JSONOverrideStale      bool     `json:"jsonOverrideStale"`
	Hashes                 []string `json:"hashes"`
	Architecture           string   `json:"architecture,omitempty"`
	AvailableArchitectures []string `json:"availableArchitectures,omitempty"`
	DefaultArchitecture    string   `json:"defaultArchitecture,omitempty"`
	HasInstallerScript     bool     `json:"hasInstallerScript,omitempty"`
}

// InspectPackageManifest resolves pkgRef and returns manifest debug info for the UI/CLI.
func (e *Engine) InspectPackageManifest(ctx context.Context, pkgRef string) (*InstallManifestInfo, error) {
	resolved, err := e.ResolveInstallRef(ctx, pkgRef)
	if err != nil {
		return nil, err
	}
	lookupRef := runtime.ManifestLookupRef(resolved)
	if err := catalog.EnsureBucketForInstall(e.Engine, ctx, lookupRef, nil); err != nil {
		return nil, err
	}
	manifestPath, m, err := e.BucketRegistry.GetManifestPath(lookupRef)
	if err != nil {
		return nil, fmt.Errorf("find manifest: %w", err)
	}
	info, err := e.buildPlanManifestInspect(resolved, manifestPath, m)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func (e *Engine) buildPlanManifestInspect(pkgRef, manifestPath string,
	m *manifest.Manifest,
) (InstallManifestInfo, error) {
	installArch := m.SelectedArchitecture()
	state, err := override.ResolveManifestOverrides(e.Engine, pkgRef, manifestPath, m, installArch, nil)
	if err != nil {
		return InstallManifestInfo{}, err
	}
	return buildManifestInspectResolved(manifestPath, m, state.EffectiveM, state)
}

func buildManifestInspectFromModel(manifestPath string,
	m *manifest.Manifest,
) (InstallManifestInfo, error) {
	return buildManifestInspectResolved(manifestPath, m, m, override.ManifestOverrideState{EffectiveM: m})
}

func buildManifestInspectResolved(manifestPath string,
	bucketM, effectiveM *manifest.Manifest,
	state override.ManifestOverrideState,
) (InstallManifestInfo, error) {
	defaultArch := bucketM.SelectedArchitecture()
	bucketURLs := bucketM.GetURLsForInstall(defaultArch)
	info := InstallManifestInfo{
		ManifestPath:           manifestPath,
		Version:                effectiveM.Version,
		DownloadURLs:           effectiveM.GetURLsForInstall(defaultArch),
		BucketDownloadURLs:     bucketURLs,
		URLOverrideActive:      state.URLActive && !state.JSONActive,
		URLOverrideStale:       state.URLStale && !state.JSONActive,
		JSONOverrideActive:     state.JSONActive,
		JSONOverrideStale:      state.JSONStale,
		Hashes:                 effectiveM.GetHashesForInstall(defaultArch),
		Architecture:           defaultArch,
		AvailableArchitectures: bucketM.AvailableArchitectures(),
		DefaultArchitecture:    defaultArch,
		HasInstallerScript:     effectiveM.HasInstallerScript(),
	}
	bucketData, err := json.Marshal(bucketM)
	if err != nil {
		return info, fmt.Errorf("marshal manifest: %w", err)
	}
	info.BucketManifestJSON = formatManifestJSON(bucketData)
	effectiveData, err := json.Marshal(effectiveM)
	if err != nil {
		return info, fmt.Errorf("marshal effective manifest: %w", err)
	}
	info.ManifestJSON = formatManifestJSON(effectiveData)
	return info, nil
}

// InspectInstalledManifest returns manifest debug info from a package version's stored install record.
func (e *Engine) InspectInstalledManifest(pkgName, version string) (*InstallManifestInfo, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	if pkgName == "" || version == "" {
		return nil, fmt.Errorf("package name and version required")
	}
	installDir := filepath.Join(apps.PkgRoot(e.Config.RootDir, pkgName), version)
	rec, err := apps.LoadInstallRecord(installDir)
	if err != nil {
		return nil, fmt.Errorf("load install record: %w", err)
	}
	if rec.Manifest == nil {
		return nil, fmt.Errorf("no manifest stored for %s@%s", pkgName, version)
	}
	manifestPath := filepath.Join(installDir, apps.InstallRecordFile)
	info, err := buildManifestInspectFromModel(manifestPath, rec.Manifest)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func formatManifestJSON(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	truncated := len(data) > maxManifestInspectBytes
	if truncated {
		data = data[:maxManifestInspectBytes]
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err == nil {
		if truncated {
			return pretty.String() + "\n… (truncated)"
		}
		return pretty.String()
	}
	out := string(data)
	if truncated {
		out += "\n… (truncated)"
	}
	return out
}
