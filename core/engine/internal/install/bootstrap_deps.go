// Bootstrap dependency resolution for the Engine.
//
// deps_probe.go probes whether tools exist (system PATH or ~/.glue/bin) for doctor
// and startup notes. This file adds Engine-level path resolution and on-demand
// bootstrap downloads used by UI clients, install hooks, and bucket maintenance.
//
// Two resolution styles:
//   - ResolveBootstrapped*: only ~/.glue/bin copies (bootstrap UI).
//   - Resolve*: any usable path the probe/install layer finds (PATH, shims, apps).
package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/gluestick-sh/core/bootstrap"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

// catalogHasMatchingManifest scans every bucket manifest and returns true when
// match reports a dependency need (e.g. WiX dark for MSI extraction).
func catalogHasMatchingManifest(e *runtime.Engine, match func(*manifest.Manifest) bool) bool {
	if e == nil || e.BucketRegistry == nil || match == nil {
		return false
	}
	for _, b := range e.BucketRegistry.List() {
		for _, manifestPath := range runtime.BucketManifestPaths(b.Root, b.Name) {
			m, err := manifest.ParseFile(manifestPath)
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

// resolveGit returns the first runnable git (system PATH, then bootstrapped MinGit).
func resolveGit(glueRoot string) (string, error) {
	p := ProbeGit(glueRoot)
	if !p.OK {
		return "", fmt.Errorf("git not found")
	}
	return p.Path, nil
}

// ResolveBootstrappedGitPath returns ~/.glue/bin MinGit when installed and runnable.
// Unlike ResolveGitPath, it ignores system PATH so callers can show bootstrap state.
func ResolveBootstrappedGitPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	if root == "" {
		return "", fmt.Errorf("bootstrapped git not found")
	}
	path := BootstrappedGitPath(root)
	if bootstrap.GitExecutableReady(path) {
		return path, nil
	}
	return "", fmt.Errorf("bootstrapped git not found")
}

// ResolveBootstrappedSevenZipPath returns ~/.glue/bin/7z.exe when present.
func ResolveBootstrappedSevenZipPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	if root == "" {
		return "", fmt.Errorf("bootstrapped 7z not found")
	}
	path := filepath.Join(root, "bin", "7z.exe")
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path, nil
	}
	return "", fmt.Errorf("bootstrapped 7z not found")
}

// ResolveBootstrappedDarkPath returns ~/.glue/bin/wix/wix.exe or dark.exe when present.
func ResolveBootstrappedDarkPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	if root == "" {
		return "", fmt.Errorf("bootstrapped wix not found")
	}
	wixDir := filepath.Join(root, "bin", "wix")
	for _, name := range []string{"wix.exe", "dark.exe"} {
		path := filepath.Join(wixDir, name)
		if darkExecutableReady(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("bootstrapped wix not found")
}

// ResolveBootstrappedInnounpPath returns ~/.glue/bin/innounp/innounp.exe when present.
func ResolveBootstrappedInnounpPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	if root == "" {
		return "", fmt.Errorf("bootstrapped innounp not found")
	}
	path := filepath.Join(root, "bin", "innounp", "innounp.exe")
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path, nil
	}
	return "", fmt.Errorf("bootstrapped innounp not found")
}

// ResolveGitPath returns the first usable git executable (PATH or bootstrap).
func ResolveGitPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	return resolveGit(root)
}

// ResolveSevenZipPath returns the first usable 7-Zip binary (~/.glue/bin, glue
// 7zip app, or PATH). It does not download; callers use EnsureSevenZipBootstrap.
func ResolveSevenZipPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	if path := ResolveLocalSevenZip(root); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("7z not found")
}

// ResolveDarkPath returns the first usable WiX tool (bootstrap, shims, wix/dark apps, or PATH).
func ResolveDarkPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	return resolveDark(root)
}

// ManifestMayNeedDark reports whether install/pre_install hooks call expand-darkarchive.
func ManifestMayNeedDark(m *manifest.Manifest) bool {
	if m == nil {
		return false
	}
	if hookHelpersNeedDark(m.InstallerScriptHooks()) {
		return true
	}
	if hookHelpersNeedDark(m.PreInstallHooksForInstall("")) {
		return true
	}
	return false
}

// CatalogNeedsDark reports whether any bucket manifest needs WiX dark/wix.
// Used to decide whether to offer WiX bootstrap proactively.
func CatalogNeedsDark(e *runtime.Engine) bool {
	return catalogHasMatchingManifest(e, ManifestMayNeedDark)
}

// EnsureGitBootstrap downloads MinGit into ~/.glue/bin when git is missing.
// No-op on non-Windows; install hooks call bootstrap.EnsureGit directly on Windows.
func EnsureGitBootstrap(e *runtime.Engine, ctx context.Context) (string, error) {
	if goruntime.GOOS != "windows" {
		return "", nil
	}
	if e == nil || e.Bootstrap == nil {
		return "", fmt.Errorf("git not found")
	}
	return e.Bootstrap.EnsureGit(ctx)
}

// EnsureSevenZipBootstrap downloads minimal 7-Zip into ~/.glue/bin when missing.
func EnsureSevenZipBootstrap(e *runtime.Engine, ctx context.Context) (string, error) {
	if goruntime.GOOS != "windows" {
		return "", nil
	}
	if e == nil || e.Bootstrap == nil {
		return "", fmt.Errorf("7z not found")
	}
	path, err := e.Bootstrap.Ensure7z(ctx)
	if err != nil {
		return "", err
	}
	SetSevenZipFromLocal(e)
	return path, nil
}

// EnsureDarkBootstrap downloads WiX into ~/.glue/bin when dark/wix is missing.
func EnsureDarkBootstrap(e *runtime.Engine, ctx context.Context) (string, error) {
	if goruntime.GOOS != "windows" {
		return "", nil
	}
	if e == nil || e.Bootstrap == nil {
		return "", darkNotFoundErr(nil)
	}
	return e.Bootstrap.EnsureDark(ctx)
}
