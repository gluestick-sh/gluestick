package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/gluestick-sh/core/apps"
	eruntime "github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/procutil"
)

func isExeInstallerScriptInstall(fileExt string, m *manifest.Manifest, installArch string) bool {
	return strings.EqualFold(fileExt, ".exe") && m != nil && m.HasInstallerScriptForInstall(installArch)
}

func installerScriptNeedsInnounp(hooks []string) bool {
	body := strings.ToLower(strings.Join(hooks, "\n"))
	return strings.Contains(body, "expand-innoarchive") || strings.Contains(body, "innounp")
}

// runInstallerScript executes Scoop installer.script hooks with minimal helper functions.
func runInstallerScript(installDir, downloadName, app, bucket, bucketsDir, persistDir, sevenZip, innounp, dark, architecture string, hooks []string, interactive bool) error {
	if len(hooks) == 0 {
		return nil
	}
	if goruntime.GOOS != "windows" {
		return fmt.Errorf("installer.script requires Windows (PowerShell)")
	}
	if strings.TrimSpace(sevenZip) == "" {
		return fmt.Errorf("installer.script requires 7z.exe (install 7zip first)")
	}

	if installerScriptNeedsInnounp(hooks) && strings.TrimSpace(innounp) == "" {
		return fmt.Errorf("innounp.exe not found\n\nInno Setup installer scripts need innounp in glue/bin")
	}
	if hookHelpersNeedDark(hooks) && strings.TrimSpace(dark) == "" {
		return fmt.Errorf("dark.exe or wix.exe not found\n\nWiX Burn installer scripts need dark or wix in glue/bin")
	}

	if interactive {
		hooks = adaptInstallerScriptForInteractive(hooks)
	}
	script := buildInstallerScript(installDir, downloadName, app, bucket, bucketsDir, persistDir, sevenZip, innounp, dark, architecture, interactive, hooks)
	cmd := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", script,
	)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := decodePowerShellOutput(out)
		if msg != "" {
			return fmt.Errorf("installer.script: %w\n%s", err, msg)
		}
		return fmt.Errorf("installer.script: %w", err)
	}
	return nil
}

func buildInstallerScript(installDir, downloadName, app, bucket, bucketsDir, persistDir, sevenZip, innounp, dark, architecture string, interactive bool, hooks []string) string {
	body := strings.Join(hooks, "\r\n")
	if architecture == "" {
		architecture = "64bit"
	}
	glueInteractive := "$false"
	if interactive {
		glueInteractive = "$true"
	}
	return fmt.Sprintf("%s\r\n$glue_interactive = %s; $global = $false; $original_dir = %s; $dir = %s; $fname = %s; $app = %s; $bucket = %s; $bucketsdir = %s; $persist_dir = %s; $architecture = %s; $glue_old_path = $env:PATH; $env:PATH = \"$dir;$glue_old_path\"; Push-Location -LiteralPath $dir; trap { Pop-Location; $env:PATH = $glue_old_path; throw $_ }\r\n%s\r\nPop-Location; $env:PATH = $glue_old_path",
		scoopInstallerHelpersPreamble(sevenZip, innounp, dark),
		glueInteractive,
		psSingleQuoted(installDir),
		psSingleQuoted(installDir),
		psSingleQuoted(downloadName),
		psSingleQuoted(app),
		psSingleQuoted(bucket),
		psSingleQuoted(bucketsDir),
		psSingleQuoted(persistDir),
		psSingleQuoted(architecture),
		body,
	)
}

func scoopInstallerHelpersPreamble(sevenZip, innounp, dark string) string {
	preamble := scoopPathEnvHelpersPreamble() + fmt.Sprintf(`$global:GLUE_INNOUNP = %s
function Get-HelperPath {
  param(
    [Parameter(Mandatory=$true,Position=0)][ValidateSet('7zip','Innounp')][String]$Helper
  )
  switch ($Helper) {
    '7zip' { return $global:GLUE_7Z }
    'Innounp' { return $global:GLUE_INNOUNP }
  }
  return $null
}
function fname([string]$path) { return [System.IO.Path]::GetFileName($path) }
function strip_ext([string]$path) {
  $base = [System.IO.Path]::GetFileName($path)
  return [System.IO.Path]::GetFileNameWithoutExtension($base)
}
function ensure([string]$path) {
  if (!(Test-Path $path)) {
    New-Item -ItemType Directory -Path $path -Force | Out-Null
  }
  return (Resolve-Path $path).Path
}
function get_config([string]$name, $default) {
  if ($name -eq 'USE_LESSMSI') { return $false }
  return $default
}
` + scoopCopyTreeAndMovedirHelper() + `
function error([string]$msg) { throw $msg }
function abort([string]$msg) { throw $msg }
function warn([string]$msg) { Write-Host $msg }
`, psSingleQuoted(innounp)) + scoopExpandMsiArchiveHelper()
	if dark != "" {
		preamble += scoopExpandDarkArchiveHelper(dark)
	}
	if sevenZip != "" {
		preamble += scoopExpand7zipArchiveHelper(sevenZip)
	}
	return preamble + `
function Expand-InnoArchive {
  param(
    [Parameter(Mandatory=$true,Position=0)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$ExtractDir,
    [string]$Switches,
    [switch]$Removal
  )
  $DestinationPath = $DestinationPath.TrimEnd('\')
  if (-not (Test-Path $DestinationPath)) { New-Item -ItemType Directory -Path $DestinationPath | Out-Null }
  $LogPath = Join-Path (Split-Path $Path) 'innounp.log'
  $ArgList = @('-x', "-d$DestinationPath", $Path, '-y')
  if ($ExtractDir) {
    if ($ExtractDir -match '^[^{].*') { $ArgList += "-c{app}\$ExtractDir" }
    elseif ($ExtractDir -match '^{.*') { $ArgList += "-c$ExtractDir" }
    else { $ArgList += '-c{app}' }
  } else {
    $ArgList += '-c{app}'
  }
  if ($Switches) { $ArgList += (-split $Switches) }
  $innounp = Get-HelperPath -Helper Innounp
  if (-not $innounp) { throw 'innounp.exe not found in glue/bin' }
  $ok = Invoke-ExternalCommand -FilePath $innounp -ArgumentList $ArgList -LogPath $LogPath
  if (-not $ok) { throw "Failed to extract Inno archive: $Path" }
  if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
` + scoopInvokeExternalCommandHelper() + `
function is_admin {
  $admin = [Security.Principal.WindowsBuiltInRole]::Administrator
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  ([Security.Principal.WindowsPrincipal]($id)).IsInRole($admin)
}
function info([string]$msg) { Write-Host $msg }
function friendly_path([string]$p) { return $p }
function new_issue_msg($app,$bucket,$type) { return '' }
`
}

func orphanInstallRepairable(root, pkgName, version string, m *manifest.Manifest) bool {
	if m == nil || !m.HasInstallerScript() {
		return true
	}
	installDir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	return manifestBinExistsAtRoot(installDir, m)
}

func runManifestInstallerHook(e *eruntime.Engine, ctx context.Context, installDir, downloadName, pkgName, pkgRef string, m *manifest.Manifest, installArch string, interactive bool) error {
	if err := ensureExtractor7zWithProf(e, ctx, nil, pkgName); err != nil {
		return fmt.Errorf("ensure 7z: %w", err)
	}
	sevenZip := e.Extractor.SevenZipPath()
	root := e.Config.RootDir
	bucketsDir := filepath.Join(root, "buckets")
	persistDir := filepath.Join(root, "persist", pkgName)
	innounp := ""
	hooks := m.InstallerScriptHooksForInstall(installArch)
	needsInnounp := installerScriptNeedsInnounp(hooks)
	innounp, err := ensureInnounpWithProf(e, ctx, nil, needsInnounp, pkgName)
	if err != nil {
		return err
	}
	dark, err := ensureDarkWithProf(e, ctx, nil, hooks, pkgName)
	if err != nil {
		return err
	}
	if interactive {
		verbose.Progressf("  Running installer script (interactive)...\n")
	} else {
		verbose.Progressf("  Running installer script...\n")
	}
	return runInstallerScript(
		installDir, downloadName, pkgName, eruntime.PackageBucketName(pkgRef), bucketsDir, persistDir, sevenZip, innounp, dark, installArch, hooks, interactive,
	)
}

func extractViaInstallerScript(e *eruntime.Engine, 
	ctx context.Context,
	prof *installPhaseProfile,
	pkgRef, installDir, downloadName, archiveHash, installArch string,
	m *manifest.Manifest,
	interactive bool,
	installedFiles map[string]string,
	totalSize *int64,
	reportIndexProgress func(processed, total int64),
) (int, error) {
	pkgName, _ := eruntime.ParsePkgRef(pkgRef)
	if err := ensureExtractor7zWithProf(e, ctx, prof, pkgName); err != nil {
		return 0, fmt.Errorf("ensure 7z: %w", err)
	}
	sevenZip := e.Extractor.SevenZipPath()
	if sevenZip == "" {
		return 0, fmt.Errorf("installer.script requires 7z.exe (install 7zip first)")
	}
	innounp := ""
	scriptHooks := m.InstallerScriptHooksForInstall(installArch)
	needsInnounp := installerScriptNeedsInnounp(scriptHooks)
	innounp, err := ensureInnounpWithProf(e, ctx, prof, needsInnounp, pkgName)
	if err != nil {
		return 0, err
	}
	if interactive {
		verbose.Progressf("  Running installer script (interactive)...\n")
	} else {
		verbose.Progressf("  Running installer script...\n")
	}
	if err := prof.runExtract(func() error {
		if err := cleanInstallDir(installDir); err != nil {
			return fmt.Errorf("clean install dir: %w", err)
		}
		persistDir := filepath.Join(e.Config.RootDir, "persist", pkgName)
		persistEntries := m.PersistEntriesForInstall(installArch)
		if err := restorePersistOnInstall(installDir, persistDir, persistEntries); err != nil {
			return err
		}
		if err := prepareInstallDirForPreInstallHooks(installDir, persistDir, persistEntries); err != nil {
			return err
		}
		preHooks := m.PreInstallHooksForInstall(installArch)
		dark, err := ensureDarkWithProf(e, ctx, prof, preHooks, pkgName)
		if err != nil {
			return err
		}
		preEnv := NewHookScriptEnv(e, installDir, downloadName, m.Version, pkgRef, pkgName, installArch, preHooks)
		preEnv.SevenZip = sevenZip
		preEnv.Dark = dark
		if err := runPreInstallHooks(preEnv); err != nil {
			return err
		}
		if err := prepareInstallerScriptLayout(installDir, downloadName); err != nil {
			return err
		}
		if err := materializeInstallerFile(e.Store, installDir, downloadName, archiveHash); err != nil {
			return fmt.Errorf("stage installer: %w", err)
		}
		scriptDark, err := ensureDarkWithProf(e, ctx, prof, scriptHooks, pkgName)
		if err != nil {
			return err
		}
		if err := runInstallerScript(
			installDir, downloadName, pkgName, eruntime.PackageBucketName(pkgRef),
			filepath.Join(e.Config.RootDir, "buckets"),
			filepath.Join(e.Config.RootDir, "persist", pkgName),
			sevenZip, innounp, scriptDark, installArch, scriptHooks, interactive,
		); err != nil {
			return err
		}
		if err := mergeInstallerSourceDir(installDir); err != nil {
			return err
		}
		postHooks := m.PostInstallHooksForInstall(installArch)
		if err := prepareInstallDirForPostInstallHooks(installDir, persistDir, persistEntries); err != nil {
			return err
		}
		postDark, err := ensureDarkWithProf(e, ctx, prof, postHooks, pkgName)
		if err != nil {
			return err
		}
		postEnv := NewHookScriptEnv(e, installDir, downloadName, m.Version, pkgRef, pkgName, installArch, postHooks)
		postEnv.SevenZip = sevenZip
		postEnv.Dark = postDark
		return runPostInstallHooks(postEnv)
	}); err != nil {
		return 0, err
	}
	count, err := indexDirectExtractInstall(e.Store, installDir, downloadName, archiveHash, installedFiles, totalSize, reportIndexProgress)
	if err != nil {
		return 0, fmt.Errorf("index installed files: %w", err)
	}
	if count == 0 {
		return 0, fmt.Errorf("installer.script produced no files")
	}
	if err := validateManifestBins(installDir, m); err != nil {
		if mergeErr := mergeInstallerSourceDir(installDir); mergeErr != nil {
			return 0, mergeErr
		}
		if err := validateManifestBins(installDir, m); err != nil {
			return 0, err
		}
	}
	verbose.Progressf("  Installed %d file(s) via installer script\n", count)
	return count, nil
}

func prepareInstallerScriptLayout(installDir, downloadName string) error {
	parentSetup := filepath.Join(filepath.Dir(installDir), downloadName)
	if err := os.Remove(parentSetup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale installer %s: %w", parentSetup, err)
	}
	return nil
}

func materializeInstallerFile(store *store.Store, installDir, downloadName, hash string) error {
	src := store.ObjectPath(hash)
	dst := filepath.Join(installDir, downloadName)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
