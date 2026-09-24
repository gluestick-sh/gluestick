package install

import (
	"context"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/procutil"
)

func resolveInnounp(glueRoot string) (string, error) {
	var candidates []string
	if glueRoot != "" {
		candidates = append(candidates, filepath.Join(glueRoot, "bin", "innounp", "innounp.exe"))
		candidates = append(candidates, filepath.Join(glueRoot, "shims", "innounp.exe"))
		innounpRoot := filepath.Join(glueRoot, "apps", "innounp")
		if current, err := apps.ReadCurrent(innounpRoot); err == nil && current != "" {
			candidates = append(candidates, filepath.Join(innounpRoot, current, "innounp.exe"))
		} else if versions, err := apps.ListVersions(innounpRoot); err == nil && len(versions) > 0 {
			best := versions[0]
			for _, v := range versions[1:] {
				if v > best {
					best = v
				}
			}
			candidates = append(candidates, filepath.Join(innounpRoot, best, "innounp.exe"))
		}
	}
	if path, err := exec.LookPath("innounp.exe"); err == nil {
		candidates = append(candidates, path)
	}

	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("innounp.exe not found")
}

func isInnounpHelperPackage(pkgName string) bool {
	return strings.EqualFold(strings.TrimSpace(pkgName), "innounp")
}

func innounpNotFoundErr(cause error) error {
	if cause == nil {
		return fmt.Errorf("innounp.exe not found\n\nInno Setup packages need innounp in glue/bin")
	}
	return fmt.Errorf("%w\n\nInno Setup packages need innounp in glue/bin", cause)
}

func ensureInnounpWithProf(e *runtime.Engine, ctx context.Context, prof *installPhaseProfile, needed bool, pkgName string) (string, error) {
	if !needed {
		return "", nil
	}
	root := ""
	if e != nil && e.Config.RootDir != "" {
		root = e.Config.RootDir
	}
	if path, err := resolveInnounp(root); err == nil {
		return path, nil
	}
	if isInnounpHelperPackage(pkgName) {
		return "", innounpNotFoundErr(nil)
	}

	bootstrap := func() error {
		_, err := e.Bootstrap.EnsureInnounp(ctx)
		return err
	}
	var err error
	if prof != nil {
		err = prof.runBootstrap(bootstrap)
	} else {
		err = bootstrap()
	}
	if err != nil {
		return "", innounpNotFoundErr(err)
	}
	path, err := resolveInnounp(root)
	if err != nil {
		return "", innounpNotFoundErr(err)
	}
	return path, nil
}

// ResolveInnounpPath returns the first usable innounp.exe on Windows.
func ResolveInnounpPath(e *runtime.Engine) (string, error) {
	root := ""
	if e != nil && e.Config != nil {
		root = e.Config.RootDir
	}
	return resolveInnounp(root)
}

// EnsureInnounpBootstrap downloads and extracts innounp when it is missing.
func EnsureInnounpBootstrap(e *runtime.Engine, ctx context.Context) (string, error) {
	if e == nil || e.Bootstrap == nil {
		return "", innounpNotFoundErr(nil)
	}
	return e.Bootstrap.EnsureInnounp(ctx)
}

func innounpComponentArg(extractDir string) string {
	extractDir = strings.TrimSpace(extractDir)
	if extractDir == "" {
		return "-c{app}"
	}
	if strings.HasPrefix(extractDir, "{") {
		return "-c" + extractDir
	}
	return "-c{app}\\" + strings.ReplaceAll(extractDir, "/", "\\")
}

func runInnounpExtract(innounpPath, archivePath, destDir, extractDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	args := []string{"-x", "-d" + destDir, archivePath, "-y", innounpComponentArg(extractDir)}
	cmd := exec.Command(innounpPath, args...)
	procutil.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("innounp failed: %w\nStderr: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractInnoInstaller(e *runtime.Engine, 
	ctx context.Context,
	prof *installPhaseProfile,
	root, archivePath, installDir, pkgName string,
	m *manifest.Manifest,
	installedFiles map[string]string,
	totalSize *int64,
) error {
	innounp, err := ensureInnounpWithProf(e, ctx, prof, true, pkgName)
	if err != nil {
		return err
	}
	if err := cleanInstallDir(installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	verbose.Progressf("  Extracting Inno Setup installer with innounp...\n")
	if err := prof.runExtract(func() error {
		return runInnounpExtract(innounp, archivePath, installDir, m.GetExtractDir())
	}); err != nil {
		return err
	}
	if err := validateInstallDir(installDir); err != nil {
		return err
	}
	if err := refreshInstalledFilesFromDir(e.Store, installDir, installedFiles, totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}
	return nil
}
