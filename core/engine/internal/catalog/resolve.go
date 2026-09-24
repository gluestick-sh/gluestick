package catalog

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
)

// BucketDirHasManifests reports whether bucketDir contains at least one manifest JSON file.
// It checks standard Scoop layout dirs only and never walks .git (full bucket walks are slow).
func BucketDirHasManifests(bucketDir string) bool {
	if bucketDir == "" {
		return false
	}
	info, err := os.Stat(bucketDir)
	if err != nil || !info.IsDir() {
		return false
	}
	if dirHasAnyJSONFile(bucketDir) {
		return true
	}
	bucketName := filepath.Base(bucketDir)
	for _, dir := range []string{
		filepath.Join(bucketDir, "bucket"),
		filepath.Join(bucketDir, bucketName),
		filepath.Join(bucketDir, "deprecated"),
	} {
		if manifestTreeHasJSON(dir) {
			return true
		}
	}
	return false
}

// dirHasAnyJSONFile checks if a directory contains any .json files.
func dirHasAnyJSONFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return true
		}
	}
	return false
}

// manifestTreeHasJSON walks a directory tree to find .json files, skipping .git directories.
func manifestTreeHasJSON(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// EnsureBucketForInstall ensures the bucket referenced by pkgRef is available.
// If buckets is non-empty, only buckets in that list are allowed for install.
func EnsureBucketForInstall(e *runtime.Engine, ctx context.Context, pkgRef string, buckets []string) error {
	ref := pkgRef
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	bucketName := runtime.PackageBucketName(pkgRef)

	// If buckets list is specified, verify the bucket is allowed
	if len(buckets) > 0 {
		allowed := false
		for _, b := range buckets {
			if b == bucketName {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("bucket %q is not in allowed buckets list: %v", bucketName, buckets)
		}
	}

	bucketDir := filepath.Join(e.Config.RootDir, "buckets", bucketName)
	if BucketDirHasManifests(bucketDir) {
		if bucketAlreadyIndexed(e, bucketName) {
			return nil
		}
		if _, err := e.BucketRegistry.Get(bucketName); err != nil {
			if err := e.BucketRegistry.ReloadFromDisk(); err != nil {
				return fmt.Errorf("reload buckets: %w", err)
			}
		}
		runtime.SyncSearchIndex(e, false)
		return nil
	}

	// Check for cancellation before expensive network operations
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	repoURL, ok := bucket.GetKnownBucketURL(bucketName)
	if !ok {
		return &apperr.BucketNotInstalled{Name: bucketName}
	}

	if err := e.BucketRegistry.EnsureGit(); err != nil {
		return fmt.Errorf("git not available: %w\n\nRun: glue bucket add %s", err, bucketName)
	}

	// Check for cancellation before git clone
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if _, err := e.BucketRegistry.Add(bucketName, repoURL); err != nil {
		return fmt.Errorf("clone bucket %q: %w", bucketName, err)
	}
	if err := e.BucketRegistry.ReloadFromDisk(); err != nil {
		return fmt.Errorf("reload buckets: %w", err)
	}
	runtime.SyncSearchIndex(e, false)
	return nil
}

// bucketAlreadyIndexed checks if a bucket is already loaded in the search index.
func bucketAlreadyIndexed(e *runtime.Engine, bucketName string) bool {
	if e == nil || e.BucketRegistry == nil {
		return false
	}
	if _, err := e.BucketRegistry.Get(bucketName); err != nil {
		return false
	}
	return e.SearchIdx != nil && e.SearchIdx.HasLoadedBucket(bucketName)
}

// InstallRefFromMatch builds the install ref for a catalog match.
func InstallRefFromMatch(m manifest.Match) string {
	if m.Bucket == "" || m.Bucket == "main" {
		return m.Name
	}
	return m.Bucket + "/" + m.Name
}

// findExactPackages finds packages by exact name match in the search index.
// findExactPackages finds packages by exact name match in the search index.
// Returns a slice of manifest matches for the given package name.
func findExactPackages(e *runtime.Engine, pkgName string) []manifest.Match {
	if e.SearchIdx == nil {
		return nil
	}
	entries := e.SearchIdx.FindExactName(pkgName)
	matches := make([]manifest.Match, len(entries))
	for i, entry := range entries {
		matches[i] = manifest.Match{
			Name:        entry.Name,
			Bucket:      entry.Bucket,
			Description: entry.Description,
			Version:     entry.Version,
		}
	}
	return matches
}

// PickInstallMatch chooses a single catalog match when multiple buckets contain the name.
func PickInstallMatch(matches []manifest.Match) *manifest.Match {
	if len(matches) == 1 {
		return &matches[0]
	}
	var nonMain []manifest.Match
	for _, m := range matches {
		if m.Bucket != "" && m.Bucket != "main" {
			nonMain = append(nonMain, m)
		}
	}
	if len(nonMain) == 1 {
		return &nonMain[0]
	}
	return nil
}

// isUnqualifiedRef checks if a package reference is unqualified (no bucket prefix).
// isUnqualifiedRef checks if a package reference is unqualified (no bucket prefix).
// Returns true if the reference doesn't contain path separators and resolves to the main bucket.
func isUnqualifiedRef(pkgRef string) bool {
	ref := strings.TrimSpace(pkgRef)
	if ref == "" {
		return false
	}
	if strings.ContainsAny(ref, `/\`) {
		return false
	}
	return runtime.PackageBucketName(ref) == "main"
}

// ResolveInstallRef resolves an unqualified package name to bucket/pkg when needed.
func ResolveInstallRef(e *runtime.Engine, ctx context.Context, pkgRef string) (string, error) {
	ref := strings.TrimSpace(pkgRef)
	if ref == "" {
		return "", fmt.Errorf("empty package name")
	}

	lookupRef := runtime.ManifestLookupRef(ref)
	if err := EnsureBucketForInstall(e, ctx, lookupRef, nil); err != nil {
		return "", err
	}
	if _, _, err := e.BucketRegistry.GetManifestPath(lookupRef); err == nil {
		return ref, nil
	}

	if !isUnqualifiedRef(ref) {
		_, _, err := e.BucketRegistry.GetManifestPath(lookupRef)
		return "", fmt.Errorf("find manifest: %w", err)
	}

	pkgName := runtime.PackageBaseName(ref)
	matches := findExactPackages(e, pkgName)
	if len(matches) == 0 {
		return "", fmt.Errorf("find manifest: %w", &apperr.ManifestNotFound{Name: pkgName})
	}

	chosen := PickInstallMatch(matches)
	if chosen == nil {
		refs := make([]string, len(matches))
		for i, m := range matches {
			refs[i] = InstallRefFromMatch(m)
		}
		return "", &apperr.ManifestAmbiguous{Name: pkgName, Matches: refs}
	}

	resolved := InstallRefFromMatch(*chosen)
	if err := EnsureBucketForInstall(e, ctx, resolved, nil); err != nil {
		return "", err
	}
	return resolved, nil
}

// WrapManifestNotFound augments manifest-not-found errors with bucket suggestions.
func WrapManifestNotFound(e *runtime.Engine, pkgRef string, err error) error {
	if !isUnqualifiedRef(pkgRef) {
		return fmt.Errorf("find manifest: %w", err)
	}

	pkgName := runtime.PackageBaseName(pkgRef)
	matches := findExactPackages(e, pkgName)
	if len(matches) == 0 {
		return fmt.Errorf("find manifest: %w", err)
	}

	hints := make([]string, 0, len(matches))
	for _, m := range matches {
		hints = append(hints, fmt.Sprintf("glue install %s", InstallRefFromMatch(m)))
	}
	return &apperr.ManifestSuggest{Cause: err, Hints: hints}
}

// ValidateInstallTarget resolves pkgRef and ensures its manifest exists before depends/install.
func ValidateInstallTarget(e *runtime.Engine, ctx context.Context, req *etypes.InstallRequest) error {
	resolved, err := ResolveInstallRef(e, ctx, req.Name)
	if err != nil {
		if isUnqualifiedRef(req.Name) {
			return WrapManifestNotFound(e, req.Name, err)
		}
		return err
	}
	lookupRef := runtime.ManifestLookupRef(resolved)
	if err := EnsureBucketForInstall(e, ctx, lookupRef, req.Buckets); err != nil {
		return err
	}
	if _, _, err := e.BucketRegistry.GetManifestPath(lookupRef); err != nil {
		return WrapManifestNotFound(e, req.Name, err)
	}
	return nil
}

// IsInstallResolveNotice reports user-facing resolve/manifest errors.
func IsInstallResolveNotice(err error) bool {
	return apperr.IsResolveNotice(err)
}

// FormatInstallResolveNotice formats a resolve/manifest error as a CLI Note block.
func FormatInstallResolveNotice(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	for {
		trimmed := msg
		if strings.HasPrefix(trimmed, "find manifest: ") {
			trimmed = strings.TrimSpace(trimmed[len("find manifest: "):])
		}
		if strings.HasPrefix(trimmed, `resolve "`) {
			if idx := strings.Index(trimmed, `": `); idx >= 0 {
				trimmed = strings.TrimSpace(trimmed[idx+3:])
			}
		}
		if trimmed == msg {
			break
		}
		msg = trimmed
	}
	lines := strings.Split(msg, "\n")
	if len(lines) > 0 {
		lines[0] = "Note: " + lines[0]
	}
	return strings.Join(lines, "\n")
}

// SearchPackages searches the catalog index.
func SearchPackages(e *runtime.Engine, query string, buckets []string, limit int) []*etypes.Package {
	if e.SearchIdx == nil {
		return nil
	}
	matches := e.SearchIdx.Search(query, buckets, hiddenCatalogPackages(e))
	packages := make([]*etypes.Package, 0, len(matches))
	for _, match := range matches {
		packages = append(packages, &etypes.Package{
			Name:        match.Name,
			Version:     match.Version,
			Description: match.Description,
			Bucket:      match.Bucket,
			Homepage:    match.Homepage,
		})
		if limit > 0 && len(packages) >= limit {
			return packages[:limit]
		}
	}
	return packages
}

// PackageCountsByBucket returns indexed package counts per installed bucket.
func PackageCountsByBucket(e *runtime.Engine) map[string]int {
	if e.SearchIdx == nil {
		return nil
	}
	return e.SearchIdx.CountByBucket(false, hiddenCatalogPackages(e))
}

// CountAvailablePackages returns indexed packages across all buckets.
func CountAvailablePackages(e *runtime.Engine, hideDeprecated bool) int {
	if e.SearchIdx == nil {
		return 0
	}
	return e.SearchIdx.CountAll(hideDeprecated, hiddenCatalogPackages(e))
}

// installerScriptNeedsInnounp checks if installer script hooks reference innounp.
// installerScriptNeedsInnounp checks if installer script hooks reference innounp.
// Returns true if the script contains "expand-innoarchive" or "innounp" references.
func installerScriptNeedsInnounp(hooks []string) bool {
	body := strings.ToLower(strings.Join(hooks, "\n"))
	return strings.Contains(body, "expand-innoarchive") || strings.Contains(body, "innounp")
}

// ManifestMayNeedInnounp reports whether a manifest needs innounp.
func ManifestMayNeedInnounp(m *manifest.Manifest) bool {
	if m == nil {
		return false
	}
	if m.InnoSetup {
		return true
	}
	if installerScriptNeedsInnounp(m.InstallerScriptHooks()) {
		return true
	}
	if installerScriptNeedsInnounp(m.PreInstallHooksForInstall("")) {
		return true
	}
	return false
}

// CatalogNeedsInnounp reports whether any indexed manifest needs innounp.
func CatalogNeedsInnounp(e *runtime.Engine) bool {
	return catalogHasMatchingManifest(e, ManifestMayNeedInnounp)
}

// catalogHasMatchingManifest checks if any manifest in the catalog matches the given predicate.
// catalogHasMatchingManifest checks if any manifest in the catalog matches the given predicate.
// Iterates through all buckets and manifest paths, returning true if any manifest satisfies the match function.
func catalogHasMatchingManifest(e *runtime.Engine, match func(*manifest.Manifest) bool) bool {
	if e == nil || e.BucketRegistry == nil || match == nil {
		return false
	}
	for _, b := range e.BucketRegistry.List() {
		for _, path := range runtime.BucketManifestPaths(b.Root, b.Name) {
			m, err := manifest.ParseFile(path)
			if err != nil {
				continue
			}
			if match(m) {
				return true
			}
		}
	}
	return false
}
