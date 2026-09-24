package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

func hookHelpersNeedDark(hooks []string) bool {
	body := strings.ToLower(strings.Join(hooks, "\n"))
	return strings.Contains(body, "expand-darkarchive")
}

func darkExecutableReady(exePath string) bool {
	st, err := os.Stat(exePath)
	if err != nil || st.IsDir() {
		return false
	}
	if strings.EqualFold(filepath.Base(exePath), "wix.exe") {
		return true
	}
	_, err = os.Stat(filepath.Join(filepath.Dir(exePath), "wix.dll"))
	return err == nil
}

func resolveDark(glueRoot string) (string, error) {
	type candidate struct {
		app string
		exe string
	}
	// Prefer WiX 4+ wix.exe, then WiX 3 dark.exe (matches Scoop).
	helpers := []candidate{
		{app: "wix", exe: "wix.exe"},
		{app: "dark", exe: "dark.exe"},
	}
	var candidates []string
	if glueRoot != "" {
		binWix := filepath.Join(glueRoot, "bin", "wix")
		candidates = append(candidates, filepath.Join(binWix, "wix.exe"), filepath.Join(binWix, "dark.exe"))
		for _, helper := range helpers {
			candidates = append(candidates, filepath.Join(glueRoot, "shims", helper.exe))
			appRoot := filepath.Join(glueRoot, "apps", helper.app)
			if current, err := apps.ReadCurrent(appRoot); err == nil && current != "" {
				candidates = append(candidates, filepath.Join(appRoot, current, helper.exe))
			} else if versions, err := apps.ListVersions(appRoot); err == nil && len(versions) > 0 {
				best := versions[0]
				for _, v := range versions[1:] {
					if v > best {
						best = v
					}
				}
				candidates = append(candidates, filepath.Join(appRoot, best, helper.exe))
			}
		}
	}
	for _, name := range []string{"wix.exe", "dark.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			candidates = append(candidates, path)
		}
	}

	for _, candidate := range candidates {
		if darkExecutableReady(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("dark.exe or wix.exe not found")
}

func isDarkHelperPackage(pkgName string) bool {
	switch strings.ToLower(strings.TrimSpace(pkgName)) {
	case "dark", "wix":
		return true
	default:
		return false
	}
}

func darkNotFoundErr(cause error) error {
	if cause == nil {
		return fmt.Errorf("dark.exe or wix.exe not found\n\nWiX Burn packages need dark.exe in glue/bin")
	}
	return fmt.Errorf("%w\n\nWiX Burn packages need dark.exe in glue/bin", cause)
}

func ensureDarkWithProf(e *runtime.Engine, ctx context.Context, prof *installPhaseProfile, hooks []string, pkgName string) (string, error) {
	if !hookHelpersNeedDark(hooks) {
		return "", nil
	}
	root := ""
	if e != nil && e.Config.RootDir != "" {
		root = e.Config.RootDir
	}
	if path, err := resolveDark(root); err == nil {
		return path, nil
	}
	if isDarkHelperPackage(pkgName) {
		return "", darkNotFoundErr(nil)
	}

	bootstrap := func() error {
		_, err := e.Bootstrap.EnsureDark(ctx)
		return err
	}
	var err error
	if prof != nil {
		err = prof.runBootstrap(bootstrap)
	} else {
		err = bootstrap()
	}
	if err != nil {
		return "", darkNotFoundErr(err)
	}
	path, err := resolveDark(root)
	if err != nil {
		return "", darkNotFoundErr(err)
	}
	return path, nil
}
