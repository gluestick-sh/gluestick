package install

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

// NewHookScriptEnv builds the environment (paths, arch, hook lines) passed to Scoop
// pre_install/post_install hook scripts for a package install.
func NewHookScriptEnv(e *runtime.Engine,
	installDir, downloadName, version, pkgRef, pkgName, installArch string,
	hooks []string,
) HookScriptEnv {
	root := e.Config.RootDir
	return HookScriptEnv{
		InstallDir:   installDir,
		DownloadName: downloadName,
		Version:      version,
		App:          pkgName,
		Bucket:       runtime.PackageBucketName(pkgRef),
		BucketsDir:   filepath.Join(root, "buckets"),
		PersistDir:   filepath.Join(root, "persist", pkgName),
		GlueRoot:     root,
		Arch:         installArch,
		Hooks:        hooks,
	}
}

func runManifestPostInstall(e *runtime.Engine, 
	ctx context.Context,
	pkgRef, pkgName, installDir, downloadName, version, installArch string,
	m *manifest.Manifest,
	prof *installPhaseProfile,
) error {
	hooks := m.PostInstallHooksForInstall(installArch)
	if len(hooks) == 0 {
		return nil
	}
	hooks = patchInstallHookPaths(installDir, hooks)
	persistDir := filepath.Join(e.Config.RootDir, "persist", pkgName)
	if err := prepareInstallDirForPostInstallHooks(installDir, persistDir, m.PersistEntriesForInstall(installArch)); err != nil {
		return err
	}
	sevenZip, dark, err := ResolveHookHelpers(e, ctx, hooks, prof, pkgName)
	if err != nil {
		return err
	}
	env := NewHookScriptEnv(e, installDir, downloadName, version, pkgRef, pkgName, installArch, hooks)
	env.SevenZip = sevenZip
	env.Dark = dark
	return runPostInstallHooks(env)
}

// ResolveHookHelpers ensures the external helpers a hook needs are present, returning the
// resolved 7z.exe and dark/wix paths (empty when the hooks do not require them).
func ResolveHookHelpers(e *runtime.Engine, ctx context.Context, hooks []string, prof *installPhaseProfile, pkgName string) (sevenZip, dark string, err error) {
	if hookHelpersNeed7z(hooks) {
		if err := ensureExtractor7zWithProf(e, ctx, prof, pkgName); err != nil {
			return "", "", fmt.Errorf("ensure 7z for hook: %w", err)
		}
		sevenZip = e.Extractor.SevenZipPath()
		if sevenZip == "" {
			return "", "", fmt.Errorf("hook requires 7z.exe (install 7zip first)")
		}
	}
	dark, err = ensureDarkWithProf(e, ctx, prof, hooks, pkgName)
	if err != nil {
		return "", "", err
	}
	return sevenZip, dark, nil
}
