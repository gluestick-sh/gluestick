package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/verbose"
)

// InstallPlanItem is a dependency or suggestion entry for install planning.
type InstallPlanItem struct {
	Ref       string `json:"ref"`                 // resolved bucket/pkg@version
	Label     string `json:"label,omitempty"`     // display label from manifest suggestions
	Installed bool   `json:"installed"`           // already present under apps/
}

// InstallPlan describes dependencies and suggestions before installing a package.
type InstallPlan struct {
	Package              string              `json:"package"`
	Depends              []InstallPlanItem   `json:"depends"`                        // missing depends_pre + depends + depends_post
	Suggestions          []InstallPlanItem   `json:"suggestions"`                    // optional manifest suggestions
	Manifest             InstallManifestInfo `json:"manifest"`
	LocalActivateVersion string              `json:"localActivateVersion,omitempty"` // version on disk ready to switch current without reinstall
}

// PlanInstall resolves missing depends and manifest suggestions for pkgRef.
func (e *Engine) PlanInstall(ctx context.Context, pkgRef string) (*InstallPlan, error) {
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
	manifestInfo, err := e.buildPlanManifestInspect(resolved, manifestPath, m)
	if err != nil {
		return nil, err
	}

	depRefs, err := e.collectMissingDepends(ctx, resolved, make(map[string]bool), false, nil)
	if err != nil {
		return nil, err
	}
	postRefs, err := e.collectMissingDependsPost(ctx, resolved, make(map[string]bool), nil)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(depRefs))
	for _, ref := range depRefs {
		seen[ref] = true
	}
	for _, ref := range postRefs {
		if !seen[ref] {
			depRefs = append(depRefs, ref)
			seen[ref] = true
		}
	}
	depends := make([]InstallPlanItem, 0, len(depRefs))
	for _, ref := range depRefs {
		depends = append(depends, InstallPlanItem{Ref: ref, Installed: false})
	}

	suggestions := make([]InstallPlanItem, 0)
	for _, s := range m.Suggestions() {
		item := InstallPlanItem{Label: s.Label, Ref: s.Ref}
		resolvedRef, err := e.ResolveInstallRef(ctx, s.Ref)
		if err != nil {
			item.Ref = s.Ref
		} else {
			item.Ref = resolvedRef
			item.Installed = e.isPackageInstalled(resolvedRef)
		}
		suggestions = append(suggestions, item)
	}

	localActivateVersion := planLocalActivateVersion(e, resolved, m)

	return &InstallPlan{
		Package:              resolved,
		Depends:              depends,
		Suggestions:          suggestions,
		Manifest:             manifestInfo,
		LocalActivateVersion: localActivateVersion,
	}, nil
}

// planLocalActivateVersion reports when the target version is already installed but
// not active (current link points elsewhere). Callers can offer "activate" instead
// of a full reinstall.
func planLocalActivateVersion(e *Engine, pkgRef string, m *manifest.Manifest) string {
	if e == nil || e.Config == nil || m == nil {
		return ""
	}
	pkgName, pinVersion := runtime.ParsePkgRef(pkgRef)
	targetVersion := m.Version
	if pinVersion != "" {
		targetVersion = pinVersion
	}
	root := e.Config.RootDir
	activeVer := install.ActiveInstallVersion(root, pkgName)
	if activeVer == targetVersion {
		return ""
	}
	if install.LocalVersionReadyToActivate(root, pkgName, targetVersion, m) {
		return targetVersion
	}
	return ""
}

// loadManifestForRef resolves pkgRef, ensures its bucket is present, and loads the manifest.
func (e *Engine) loadManifestForRef(ctx context.Context, pkgRef string, buckets []string) (*manifest.Manifest, error) {
	resolved, err := e.ResolveInstallRef(ctx, pkgRef)
	if err != nil {
		return nil, err
	}
	lookupRef := runtime.ManifestLookupRef(resolved)
	if err := catalog.EnsureBucketForInstall(e.Engine, ctx, lookupRef, buckets); err != nil {
		return nil, err
	}
	_, m, err := e.BucketRegistry.GetManifestPath(lookupRef)
	if err != nil {
		return nil, fmt.Errorf("find manifest: %w", err)
	}
	return m, nil
}

func (e *Engine) isPackageInstalled(pkgRef string) bool {
	name := runtime.PackageBaseName(pkgRef)
	_, ok := runtime.InstalledPackage(e.Config.RootDir, name)
	return ok
}

// collectMissingDepends walks depends_pre and depends recursively, returning an
// install order of packages not yet present. skipIfInstalled skips the root pkgRef
// when true (used for transitive deps); visiting detects cycles.
func (e *Engine) collectMissingDepends(ctx context.Context, pkgRef string, visiting map[string]bool, skipIfInstalled bool, buckets []string) ([]string, error) {
	resolved, err := e.ResolveInstallRef(ctx, pkgRef)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", pkgRef, err)
	}
	if visiting[resolved] {
		return nil, fmt.Errorf("circular dependency involving %s", resolved)
	}
	visiting[resolved] = true
	defer delete(visiting, resolved)

	if skipIfInstalled && e.isPackageInstalled(resolved) {
		return nil, nil
	}

	m, err := e.loadManifestForRef(ctx, resolved, buckets)
	if err != nil {
		return nil, err
	}

	depLists := append([]string{}, m.DependsPreList()...)
	depLists = append(depLists, m.Depends...)

	var ordered []string
	seen := make(map[string]bool)
	for _, dep := range depLists {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		sub, err := e.collectMissingDepends(ctx, dep, visiting, true, buckets)
		if err != nil {
			return nil, err
		}
		for _, ref := range sub {
			if !seen[ref] {
				seen[ref] = true
				ordered = append(ordered, ref)
			}
		}
		depResolved, err := e.ResolveInstallRef(ctx, dep)
		if err != nil {
			return nil, fmt.Errorf("resolve dependency %q: %w", dep, err)
		}
		if !e.isPackageInstalled(depResolved) && !seen[depResolved] {
			seen[depResolved] = true
			ordered = append(ordered, depResolved)
		}
	}
	return ordered, nil
}

// collectMissingDependsPost walks depends_post the same way as collectMissingDepends
// but only after the primary package would be installed.
func (e *Engine) collectMissingDependsPost(ctx context.Context, pkgRef string, visiting map[string]bool, buckets []string) ([]string, error) {
	resolved, err := e.ResolveInstallRef(ctx, pkgRef)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", pkgRef, err)
	}
	if visiting[resolved] {
		return nil, fmt.Errorf("circular dependency involving %s", resolved)
	}
	visiting[resolved] = true
	defer delete(visiting, resolved)

	m, err := e.loadManifestForRef(ctx, resolved, buckets)
	if err != nil {
		return nil, err
	}

	var ordered []string
	seen := make(map[string]bool)
	for _, dep := range m.DependsPostList() {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		sub, err := e.collectMissingDepends(ctx, dep, visiting, true, buckets)
		if err != nil {
			return nil, err
		}
		for _, ref := range sub {
			if !seen[ref] {
				seen[ref] = true
				ordered = append(ordered, ref)
			}
		}
		depResolved, err := e.ResolveInstallRef(ctx, dep)
		if err != nil {
			return nil, fmt.Errorf("resolve dependency %q: %w", dep, err)
		}
		if !e.isPackageInstalled(depResolved) && !seen[depResolved] {
			seen[depResolved] = true
			ordered = append(ordered, depResolved)
		}
	}
	return ordered, nil
}

// installDependsPost installs depends_post entries after the main package succeeds.
// Respects skip_depends on the parent request.
func (e *Engine) installDependsPost(ctx context.Context, req *InstallRequest, reporter ProgressReporter) error {
	if req.Options != nil && req.Options["skip_depends"] == "true" {
		return nil
	}
	deps, err := e.collectMissingDependsPost(ctx, req.Name, make(map[string]bool), req.Buckets)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		verbose.Progressf("  Installing post-dependency %s...\n", dep)
		subReq := &InstallRequest{
			Request: Request{
				Name:    dep,
				Force: req.Force,
			},
			Buckets: req.Buckets,
		}
		if req.Options != nil {
			subReq.Options = make(map[string]string, len(req.Options))
			for k, v := range req.Options {
				if k != "skip_depends" {
					subReq.Options[k] = v
				}
			}
		}
		result, err := e.Install(ctx, subReq, reporter)
		if err != nil {
			return fmt.Errorf("post-dependency %s: %w", dep, err)
		}
		_ = result
	}
	return nil
}

// installDependsFirst installs depends_pre and depends before the main package.
// Each sub-install sets skip_depends to avoid nested dependency expansion.
func (e *Engine) installDependsFirst(ctx context.Context, req *InstallRequest, reporter ProgressReporter) error {
	if req.Options != nil && req.Options["skip_depends"] == "true" {
		return nil
	}
	deps, err := e.collectMissingDepends(ctx, req.Name, make(map[string]bool), false, req.Buckets)
	if err != nil {
		return err
	}
	for _, dep := range deps {
		verbose.Progressf("  Installing dependency %s...\n", dep)
		subReq := &InstallRequest{
			Request: Request{
				Name:  dep,
				Force: req.Force,
				Options: map[string]string{
					"skip_depends": "true",
				},
			},
			Buckets: req.Buckets,
		}
		if req.Options != nil {
			for k, v := range req.Options {
				if k != "skip_depends" {
					subReq.Options[k] = v
				}
			}
		}
		result, err := e.Install(ctx, subReq, reporter)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", dep, err)
		}
		_ = result
	}
	return nil
}

// missingSuggestions returns manifest suggestions that are not yet installed.
func (e *Engine) missingSuggestions(ctx context.Context, pkgRef string) ([]PackageSuggestion, error) {
	m, err := e.loadManifestForRef(ctx, pkgRef, nil)
	if err != nil {
		return nil, err
	}
	var out []PackageSuggestion
	for _, s := range m.Suggestions() {
		ref := s.Ref
		if resolved, err := e.ResolveInstallRef(ctx, s.Ref); err == nil {
			ref = resolved
		}
		if e.isPackageInstalled(ref) {
			continue
		}
		out = append(out, PackageSuggestion{Label: s.Label, Ref: ref})
	}
	return out, nil
}
