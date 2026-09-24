package install

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/gluestick-sh/core/procutil"
)

// runPreInstallHooks runs Scoop pre_install PowerShell commands in installDir.
func runPreInstallHooks(env HookScriptEnv) error {
	return runInstallHookScript("pre_install", env)
}

// runPostInstallHooks runs Scoop post_install PowerShell commands in installDir.
func runPostInstallHooks(env HookScriptEnv) error {
	return runInstallHookScript("post_install", env)
}

// RunPreUninstallHooks runs Scoop pre_uninstall commands before tearing down services/processes.
func RunPreUninstallHooks(env HookScriptEnv) error {
	env.Cmd = "uninstall"
	return runInstallHookScript("pre_uninstall", env)
}

// RunPostUninstallHooks runs Scoop post_uninstall commands after install files are removed.
func RunPostUninstallHooks(env HookScriptEnv) error {
	env.Cmd = "uninstall"
	return runInstallHookScript("post_uninstall", env)
}

// RunUninstallerHooks runs Scoop uninstaller.script hooks before removing files.
func RunUninstallerHooks(env HookScriptEnv) error {
	env.Cmd = "uninstall"
	// Native commands (e.g. reg import deleting absent keys) may leave $LASTEXITCODE != 0
	// without throwing; reset so cleanup failures do not block file removal.
	if len(env.Hooks) > 0 {
		env.Hooks = append(append([]string{}, env.Hooks...), "exit 0")
	}
	return runInstallHookScript("uninstaller.script", env)
}

// HookScriptEnv carries the values exposed to Scoop hook scripts as PowerShell
// variables ($dir, $fname, $app, etc.) plus the resolved helper tool paths.
type HookScriptEnv struct {
	InstallDir, DownloadName, Version, App, Bucket, BucketsDir, PersistDir, Arch string
	GlueRoot                                                                       string
	SevenZip, Dark, Cmd                                                            string
	Global                                                                         bool
	Hooks                                                                          []string
}

func runInstallHookScript(kind string, env HookScriptEnv) error {
	if len(env.Hooks) == 0 {
		return nil
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("%s requires Windows (PowerShell)", kind)
	}

	script := buildInstallHookScript(env)
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
			return fmt.Errorf("%s: %w\n%s", kind, err, msg)
		}
		return fmt.Errorf("%s: %w", kind, err)
	}
	return nil
}

func hookHelpersNeed7z(hooks []string) bool {
	body := strings.ToLower(strings.Join(hooks, "\n"))
	return strings.Contains(body, "expand-7ziparchive")
}

// scoopPathEnvHelpersPreamble defines Scoop PATH helpers (Add-Path, Remove-Path, etc.).
func scoopPathEnvHelpersPreamble() string {
	return `function Publish-EnvVar {
  if (-not ('Win32.NativeMethods' -as [Type])) {
    Add-Type -Namespace Win32 -Name NativeMethods -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(
  IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam,
  uint fuFlags, uint uTimeout, out UIntPtr lpdwResult
);
'@
  }
  $HWND_BROADCAST = [IntPtr] 0xffff
  $WM_SETTINGCHANGE = 0x1a
  $result = [UIntPtr]::Zero
  [Win32.NativeMethods]::SendMessageTimeout($HWND_BROADCAST,
    $WM_SETTINGCHANGE,
    [UIntPtr]::Zero,
    'Environment',
    2,
    5000,
    [ref] $result
  ) | Out-Null
}
function Get-EnvVar {
  param(
    [string]$Name,
    [switch]$Global
  )
  $registerKey = if ($Global) {
    Get-Item -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager'
  } else {
    Get-Item -Path 'HKCU:'
  }
  $envRegisterKey = $registerKey.OpenSubKey('Environment')
  $registryValueOption = [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
  $envRegisterKey.GetValue($Name, $null, $registryValueOption)
}
function Set-EnvVar {
  param(
    [string]$Name,
    [string]$Value,
    [switch]$Global
  )
  $registerKey = if ($Global) {
    Get-Item -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager'
  } else {
    Get-Item -Path 'HKCU:'
  }
  $envRegisterKey = $registerKey.OpenSubKey('Environment', $true)
  if ($null -eq $Value -or $Value -eq '') {
    if ($envRegisterKey.GetValue($Name)) {
      $envRegisterKey.DeleteValue($Name)
    }
  } else {
    $registryValueKind = if ($Value.Contains('%')) {
      [Microsoft.Win32.RegistryValueKind]::ExpandString
    } elseif ($envRegisterKey.GetValue($Name)) {
      $envRegisterKey.GetValueKind($Name)
    } else {
      [Microsoft.Win32.RegistryValueKind]::String
    }
    $envRegisterKey.SetValue($Name, $Value, $registryValueKind)
  }
  Publish-EnvVar
}
function Split-PathLikeEnvVar {
  param(
    [string[]]$Pattern,
    [string]$Path
  )
  if ($null -eq $Path -and $Path -eq '') {
    return $null, $null
  } else {
    $splitPattern = $Pattern.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)
    $splitPath = $Path.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)
    $inPath = @()
    foreach ($p in $splitPattern) {
      $inPath += $splitPath.Where({ $_ -like $p })
      $splitPath = $splitPath.Where({ $_ -notlike $p })
    }
    return ($inPath -join ';'), ($splitPath -join ';')
  }
}
function Add-Path {
  param(
    [string[]]$Path,
    [string]$TargetEnvVar = 'PATH',
    [switch]$Global,
    [switch]$Force,
    [switch]$Quiet
  )
  $inPath, $strippedPath = Split-PathLikeEnvVar $Path (Get-EnvVar -Name $TargetEnvVar -Global:$Global)
  if (!$inPath -or $Force) {
    if (!$Quiet) {
      $Path | ForEach-Object {
        Write-Host "Adding $(friendly_path $_) to $(if ($Global) {'global'} else {'your'}) path."
      }
    }
    Set-EnvVar -Name $TargetEnvVar -Value ((@($Path) + $strippedPath) -join ';') -Global:$Global
  }
  $inPath, $strippedPath = Split-PathLikeEnvVar $Path $env:PATH
  if (!$inPath -or $Force) {
    $env:PATH = (@($Path) + $strippedPath) -join ';'
  }
}
function Remove-Path {
  param(
    [string[]]$Path,
    [string]$TargetEnvVar = 'PATH',
    [switch]$Global,
    [switch]$Quiet,
    [switch]$PassThru
  )
  $inPath, $strippedPath = Split-PathLikeEnvVar $Path (Get-EnvVar -Name $TargetEnvVar -Global:$Global)
  if ($inPath) {
    if (!$Quiet) {
      $Path | ForEach-Object {
        Write-Host "Removing $(friendly_path $_) from $(if ($Global) {'global'} else {'your'}) path."
      }
    }
    Set-EnvVar -Name $TargetEnvVar -Value $strippedPath -Global:$Global
  }
  $inSessionPath, $strippedPath = Split-PathLikeEnvVar $Path $env:PATH
  if ($inSessionPath) {
    $env:PATH = $strippedPath
  }
  if ($PassThru) {
    return $inPath
  }
}
`
}

// scoopHookHelpersPreamble defines Scoop helpers used by pre_install/post_install hooks.
func scoopHookHelpersPreamble(sevenZip, dark, glueRoot string) string {
	preamble := scoopPathEnvHelpersPreamble() + `
function is_admin {
  $admin = [Security.Principal.WindowsBuiltInRole]::Administrator
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  ([Security.Principal.WindowsPrincipal]($id)).IsInRole($admin)
}
` + scoopInvokeExternalCommandHelper() + `
function error([string]$msg) { throw $msg }
function abort([string]$msg) { throw $msg }
function info([string]$msg) { Write-Host $msg }
function warn([string]$msg) { Write-Host $msg }
function get_config([string]$name, $default) {
  if ($name -eq 'USE_LESSMSI') { return $false }
  return $default
}
function ensure([string]$path) {
  if (!(Test-Path $path)) {
    New-Item -ItemType Directory -Path $path -Force | Out-Null
  }
  return (Resolve-Path $path).Path
}
function fname([string]$path) { return [System.IO.Path]::GetFileName($path) }
function strip_ext([string]$path) {
  $base = [System.IO.Path]::GetFileName($path)
  return [System.IO.Path]::GetFileNameWithoutExtension($base)
}
` + scoopCopyTreeAndMovedirHelper() + `
function friendly_path([string]$p) { return $p }
function new_issue_msg($app,$bucket,$type) { return '' }
` + scoopShortcutAndShimHelpers(glueRoot) + `
` + scoopInstalledHelpers() + `
` + scoopExpandMsiArchiveHelper()
	if dark != "" {
		preamble += scoopExpandDarkArchiveHelper(dark)
	}
	if sevenZip != "" {
		preamble += scoopExpand7zipArchiveHelper(sevenZip)
	}
	return preamble
}

func scoopShortcutAndShimHelpers(glueRoot string) string {
	glueRoot = strings.ReplaceAll(glueRoot, "'", "''")
	return fmt.Sprintf(`
$glue_root = '%s'
function shortcut_folder($global) {
  if ($global) { $startmenu = 'CommonStartMenu' } else { $startmenu = 'StartMenu' }
  return Convert-Path (ensure ([System.IO.Path]::Combine([Environment]::GetFolderPath($startmenu), 'Programs', 'Glue Apps')))
}
function startmenu_shortcut([System.IO.FileInfo] $target, $shortcutName, $arguments, [System.IO.FileInfo]$icon, $global) {
  if ($target -is [string]) { $target = [System.IO.FileInfo]$target }
  if ($icon -is [string] -and $icon) { $icon = [System.IO.FileInfo]$icon }
  if (!$target.Exists) {
    Write-Host "Creating shortcut for $shortcutName ($(fname $target.FullName)) failed: Couldn't find $target" -ForegroundColor DarkRed
    return
  }
  if ($icon -and !$icon.Exists) {
    Write-Host "Creating shortcut for $shortcutName ($(fname $target.FullName)) failed: Couldn't find icon $icon" -ForegroundColor DarkRed
    return
  }
  $scoop_startmenu_folder = shortcut_folder $global
  $subdirectory = [System.IO.Path]::GetDirectoryName($shortcutName)
  if ($subdirectory) {
    $subdirectory = ensure $([System.IO.Path]::Combine($scoop_startmenu_folder, $subdirectory))
  }
  $lnkPath = "$scoop_startmenu_folder\$shortcutName.lnk"
  if (Test-Path -LiteralPath $lnkPath) { Remove-Item -LiteralPath $lnkPath -Force }
  $wsShell = New-Object -ComObject WScript.Shell
  $wsShell = $wsShell.CreateShortcut($lnkPath)
  $wsShell.TargetPath = $target.FullName
  $wsShell.WorkingDirectory = $target.DirectoryName
  if ($arguments) { $wsShell.Arguments = $arguments }
  if ($icon -and $icon.Exists) { $wsShell.IconLocation = $icon.FullName }
  $wsShell.Save()
  Write-Host "Shortcut: $shortcutName"
}
function shimdir($global) { Join-Path $glue_root 'shims' }
function shim($path, $global, $name, $arg) {
  if (!(Test-Path -LiteralPath $path)) { abort "Can't shim '$(fname $path)': couldn't find '$path'." }
  $abs_shimdir = ensure (shimdir $global)
  if (!$name) { $name = strip_ext (fname $path) }
  $shimName = $name.ToLower()
  $resolved = (Resolve-Path -LiteralPath $path).Path
  $metaDir = Join-Path $glue_root 'shims-meta'
  ensure $metaDir | Out-Null
  $args = @()
  if ($arg) { $args = @($arg) }
  $cfg = @{ name = $shimName; command = $resolved; args = $args; path = $resolved } | ConvertTo-Json -Compress
  Set-Content -LiteralPath (Join-Path $metaDir "$shimName.json") -Value $cfg -Encoding utf8
  $stub = Join-Path $glue_root 'shim.exe'
  if (!(Test-Path -LiteralPath $stub)) { abort "shim stub not found at $stub" }
  Copy-Item -LiteralPath $stub -Destination (Join-Path $abs_shimdir "$shimName.exe") -Force
  Write-Host "    [OK] $shimName"
}
function rm_shim($name, $shimdir, $app) {
  $shimName = $name.ToLower()
  $cfg = Join-Path (Join-Path $glue_root 'shims-meta') "$shimName.json"
  $exe = Join-Path $shimdir "$shimName.exe"
  if (Test-Path -LiteralPath $cfg) { Remove-Item -LiteralPath $cfg -Force }
  if (Test-Path -LiteralPath $exe) { Remove-Item -LiteralPath $exe -Force }
}
`, glueRoot)
}

// scoopInstalledHelpers defines Scoop app presence helpers used by uninstaller.script
// hooks (e.g. python: $global = installed $app $true).
func scoopInstalledHelpers() string {
	return `
function appsdir($global) {
  if ($global) { return $null }
  if (-not $glue_root) { return $null }
  return Join-Path $glue_root 'apps'
}
function appdir($app, $global) {
  $root = appsdir $global
  if (-not $root) { return $null }
  $app = ($app -split '/|\\')[-1]
  return Join-Path $root $app
}
function Select-CurrentVersion {
  param(
    [Parameter(Mandatory=$true)][string]$AppName,
    [switch]$Global
  )
  $pkgRoot = appdir $AppName $Global
  if (-not $pkgRoot -or !(Test-Path -LiteralPath $pkgRoot)) { return $null }
  $current = Join-Path $pkgRoot 'current'
  if (Test-Path -LiteralPath $current) {
    try {
      $item = Get-Item -LiteralPath $current -Force
      if ($item.LinkType -eq 'Junction') {
        $target = $item.Target
        if ($target -is [array]) { $target = $target[0] }
        if ($target) { return [System.IO.Path]::GetFileName($target.TrimEnd('\')) }
      }
    } catch { }
  }
  $versions = Get-ChildItem -LiteralPath $pkgRoot -Directory -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -ne 'current' } |
    Sort-Object Name -Descending
  if ($versions) { return $versions[0].Name }
  return $null
}
function installed($app, $global) {
  if ($null -eq $global) {
    return (installed $app $false) -or (installed $app $true)
  }
  $app = ($app -split '/|\\')[-1]
  return $null -ne (Select-CurrentVersion -AppName $app -Global:([bool]$global))
}
`
}

func scoopExpandMsiArchiveHelper() string {
	return `
function Expand-MsiArchive {
  param(
    [Parameter(Mandatory=$true,Position=0)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$ExtractDir,
    [string]$Switches,
    [switch]$Removal
  )
  $DestinationPath = $DestinationPath.TrimEnd('\')
  if ($ExtractDir) {
    $OriDestinationPath = $DestinationPath
    $DestinationPath = "$DestinationPath\_tmp"
  }
  if (!(Test-Path $DestinationPath)) { New-Item -ItemType Directory -Path $DestinationPath -Force | Out-Null }
  $MsiPath = Join-Path $env:WINDIR 'System32\msiexec.exe'
  if (!(Test-Path -LiteralPath $MsiPath)) {
    $MsiPath = Join-Path $env:WINDIR 'Sysnative\msiexec.exe'
  }
  if (!(Test-Path -LiteralPath $MsiPath)) {
    $cmd = Get-Command msiexec.exe -ErrorAction SilentlyContinue
    if ($cmd) { $MsiPath = $cmd.Source }
  }
  if (!(Test-Path -LiteralPath $MsiPath)) { abort "Could not find msiexec.exe" }
  $ArgList = @('/a', $Path, '/qn', "TARGETDIR=$DestinationPath\SourceDir")
  $LogPath = Join-Path (Split-Path $Path) 'msi.log'
  if ($Switches) { $ArgList += (-split $Switches) }
  $ok = Invoke-ExternalCommand -FilePath $MsiPath -ArgumentList $ArgList -LogPath $LogPath
  if (-not $ok) { abort "Failed to extract files from $Path." }
  if ($ExtractDir -and (Test-Path "$DestinationPath\SourceDir")) {
    movedir "$DestinationPath\SourceDir\$ExtractDir" $OriDestinationPath
    Remove-Item $DestinationPath -Recurse -Force
  } elseif ($ExtractDir) {
    movedir "$DestinationPath\$ExtractDir" $OriDestinationPath
    Remove-Item $DestinationPath -Recurse -Force
  } elseif (Test-Path "$DestinationPath\SourceDir") {
    movedir "$DestinationPath\SourceDir" $DestinationPath
  }
  if ((Test-Path "$DestinationPath\SourceDir") -and -not (Get-ChildItem "$DestinationPath\SourceDir" -Force | Select-Object -First 1)) {
    Remove-Item "$DestinationPath\SourceDir" -Force -ErrorAction SilentlyContinue
  }
  if (($DestinationPath -ne (Split-Path $Path)) -and (Test-Path "$DestinationPath\$(fname $Path)")) {
    Remove-Item "$DestinationPath\$(fname $Path)" -Force
  }
  if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
`
}

func scoopExpandDarkArchiveHelper(dark string) string {
	return fmt.Sprintf(`$global:GLUE_DARK = %s
function Expand-DarkArchive {
  param(
    [Parameter(Mandatory=$true,Position=0)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$Switches,
    [switch]$Removal
  )
  $LogPath = Join-Path (Split-Path $Path) 'dark.log'
  $DarkPath = $global:GLUE_DARK
  if (-not $DarkPath -or -not (Test-Path -LiteralPath $DarkPath)) {
    abort 'dark.exe or wix.exe not found; run: glue install dark'
  }
  if ((Split-Path $DarkPath -Leaf) -eq 'wix.exe') {
    $ArgList = @('burn', 'extract', $Path, '-out', $DestinationPath, '-outba', "$DestinationPath\UX")
  } else {
    $ArgList = @('-nologo', '-x', $DestinationPath, $Path)
  }
  if ($Switches) { $ArgList += (-split $Switches) }
  $ok = Invoke-ExternalCommand -FilePath $DarkPath -ArgumentList $ArgList -LogPath $LogPath
  if (-not $ok) { abort "Failed to extract files from $Path." }
  if (Test-Path "$DestinationPath\WixAttachedContainer") {
    Rename-Item "$DestinationPath\WixAttachedContainer" 'AttachedContainer' -ErrorAction Ignore
  } elseif (Test-Path "$DestinationPath\AttachedContainer\a0") {
    $Xml = [xml](Get-Content -Raw "$DestinationPath\UX\manifest.xml" -Encoding utf8)
    $Xml.BurnManifest.UX.Payload | ForEach-Object {
      Rename-Item "$DestinationPath\UX\$($_.SourcePath)" $_.FilePath -ErrorAction Ignore
    }
    $Xml.BurnManifest.Payload | ForEach-Object {
      Rename-Item "$DestinationPath\AttachedContainer\$($_.SourcePath)" $_.FilePath -ErrorAction Ignore
    }
  }
  if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
`, psSingleQuoted(dark))
}

func scoopInvokeExternalCommandHelper() string {
	return `
function Invoke-ExternalCommand {
  param(
    [Parameter(Mandatory=$true,Position=0)][string]$FilePath,
    $ArgumentList,
    [string]$LogPath,
    [string]$Activity,
    [hashtable]$ContinueExitCodes,
    [switch]$RunAs
  )
  if ($Activity) { Write-Host "$Activity " -NoNewline }
  $args = $ArgumentList
  if ($null -eq $args) { $args = @() }
  elseif ($args -isnot [array]) { $args = @($args) }
  $exitOk = {
    param([int]$code)
    if ($code -eq 0) { return $true }
    if ($ContinueExitCodes -and $ContinueExitCodes.ContainsKey($code)) {
      warn $ContinueExitCodes[$code]
      return $true
    }
    return $false
  }
  if (-not (Test-Path -LiteralPath $FilePath)) {
    error "Could not find '$FilePath'"
    return $false
  }
  $FilePath = (Resolve-Path -LiteralPath $FilePath).Path
  if ($RunAs) {
    try {
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru -Verb RunAs
      return (& $exitOk $proc.ExitCode)
    } catch {
      error $_.Exception.Message
      return $false
    }
  }
  if ($LogPath) {
    try {
      & $FilePath @args *>&1 | Out-File -FilePath $LogPath -Encoding utf8
      return (& $exitOk $LASTEXITCODE)
    } catch {
      error $_.Exception.Message
      return $false
    }
  }
  $interactive = $false
  if (Get-Variable -Name glue_interactive -ErrorAction SilentlyContinue) {
    $interactive = [bool]$glue_interactive
  }
  try {
    if ($interactive) {
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru
    } else {
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru -NoNewWindow
    }
    return (& $exitOk $proc.ExitCode)
  } catch {
    error $_.Exception.Message
    return $false
  }
}
`
}

func scoopCopyTreeAndMovedirHelper() string {
	// Use the call operator (&) rather than Start-Process -ArgumentList.
	// Start-Process joins ArgumentList with spaces and does not quote entries
	// that contain spaces, so paths like "...\User Data" are split into
	// multiple robocopy parameters (source=...\User, dest=Data, ...).
	return `
function Copy-Tree([string]$from, [string]$to) {
  if (!(Test-Path -LiteralPath $from)) { return }
  if ((Test-Path -LiteralPath $to) -and -not (Test-Path -LiteralPath $to -PathType Container)) {
    Remove-Item -LiteralPath $to -Force
  }
  ensure $to | Out-Null
  & robocopy.exe $from $to /E /COPY:DAT /R:1 /W:1 /NFL /NDL /NJH /NJS /NC /NS | Out-Null
  if ($LASTEXITCODE -ge 8) { throw "Copy-Tree failed (robocopy exit $LASTEXITCODE)" }
}
function movedir([string]$from, [string]$to) {
  if (!(Test-Path -LiteralPath $from)) { return }
  if (!(Test-Path -LiteralPath $to)) {
    New-Item -ItemType Directory -Path $to -Force | Out-Null
  }
  Get-ChildItem -Path $from -Force | ForEach-Object {
    $dest = Join-Path $to $_.Name
    if ($_.PSIsContainer -and (Test-Path -LiteralPath $dest -PathType Container)) {
      Copy-Tree $_.FullName $dest
      Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
    } else {
      Move-Item -LiteralPath $_.FullName -Destination $to -Force
    }
  }
}
`
}

func scoopExpand7zipArchiveHelper(sevenZip string) string {
	return fmt.Sprintf(`$global:GLUE_7Z = %s
function Expand-7zipArchive {
  param(
    [Parameter(Mandatory=$true,Position=0)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$ExtractDir,
    [string]$Switches,
    [switch]$Removal
  )
  if (-not $global:GLUE_7Z -or -not (Test-Path -LiteralPath $global:GLUE_7Z)) {
    abort '7z.exe not available; install 7zip first'
  }
  if (-not (Test-Path -LiteralPath $DestinationPath)) {
    New-Item -ItemType Directory -Path $DestinationPath -Force | Out-Null
  }
  $DestinationPath = $DestinationPath.TrimEnd('\')
  $ArgList = @('x', $Path, "-o$DestinationPath", '-xr!*.nsis', '-y')
  $leaf = Split-Path $Path -Leaf
  $IsTar = ($leaf -match '\.tar$') -or ($Path -match '\.t[abgpx]z2?$')
  if (-not $IsTar -and $ExtractDir) { $ArgList += "-ir!$ExtractDir\*" }
  if ($Switches) { $ArgList += (-split $Switches) }
  & $global:GLUE_7Z @ArgList | Out-Null
  if ($LASTEXITCODE -ne 0) { abort "7z extract failed (exit $LASTEXITCODE)" }
  if ($IsTar) {
    $LogPath = Join-Path (Split-Path $Path) '7zip.log'
    & $global:GLUE_7Z @('l', $Path) *>&1 | Out-File -FilePath $LogPath -Encoding utf8
    $TarFile = (Select-String -Path $LogPath -Pattern '[^ ]*tar$').Matches.Value
    if ($TarFile) {
      Expand-7zipArchive -Path "$DestinationPath\$TarFile" -DestinationPath $DestinationPath -ExtractDir $ExtractDir -Removal
    }
    if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  }
  if (-not $IsTar -and $ExtractDir) {
    movedir "$DestinationPath\$ExtractDir" $DestinationPath
    $ExtractDirTopPath = "$DestinationPath\$($ExtractDir -replace '[\\/].*','')"
    if ((Get-ChildItem -Path $ExtractDirTopPath -Force -ErrorAction SilentlyContinue | Measure-Object).Count -eq 0) {
      Remove-Item -Path $ExtractDirTopPath -Recurse -Force -ErrorAction SilentlyContinue
    }
  }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
`, psSingleQuoted(sevenZip))
}

// buildInstallHookScript joins Scoop hook lines and defines $dir/$fname like Scoop does.
func buildInstallHookScript(env HookScriptEnv) string {
	body := patchOptionalHookLines(patchRecurseCopyItem(strings.Join(env.Hooks, "\r\n")))
	prefix := scoopHookHelpersPreamble(env.SevenZip, env.Dark, env.GlueRoot) + "\r\n"
	if env.Cmd != "uninstall" {
		prefix += "$ErrorActionPreference = 'Stop'\r\n"
	}
	if env.Version != "" {
		prefix += fmt.Sprintf("$version = %s; ", psSingleQuoted(env.Version))
	}
	if env.App != "" {
		prefix += fmt.Sprintf("$app = %s; ", psSingleQuoted(env.App))
	}
	if env.Bucket != "" {
		prefix += fmt.Sprintf("$bucket = %s; ", psSingleQuoted(env.Bucket))
	}
	if env.BucketsDir != "" {
		prefix += fmt.Sprintf("$bucketsdir = %s; ", psSingleQuoted(env.BucketsDir))
	}
	if env.PersistDir != "" {
		prefix += fmt.Sprintf("$persist_dir = %s; ", psSingleQuoted(env.PersistDir))
	}
	if env.Arch != "" {
		prefix += fmt.Sprintf("$architecture = %s; ", psSingleQuoted(env.Arch))
	}
	if env.Cmd != "" {
		prefix += fmt.Sprintf("$cmd = %s; ", psSingleQuoted(env.Cmd))
	}
	global := "$false"
	if env.Global {
		global = "$true"
	}
	hookBlock := fmt.Sprintf(
		"$glue_old_path = $env:PATH; $env:PATH = \"$dir;$glue_old_path\"; Push-Location -LiteralPath $dir; trap { Pop-Location; $env:PATH = $glue_old_path; throw $_ }\r\n%s\r\nPop-Location; $env:PATH = $glue_old_path",
		body,
	)
	return fmt.Sprintf("%s$global = %s; $original_dir = %s; $dir = %s; $fname = %s; %s",
		prefix,
		global,
		psSingleQuoted(env.InstallDir),
		psSingleQuoted(env.InstallDir),
		psSingleQuoted(env.DownloadName),
		hookBlock)
}

func psSingleQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

var copyItemRecursePattern = regexp.MustCompile(`(?i)Copy-Item\s+"([^"]*)\*"\s+"([^"]+)"\s+-Recurse`)

// patchRecurseCopyItem rewrites fragile Copy-Item * -Recurse calls to Copy-Tree (robocopy).
func patchRecurseCopyItem(body string) string {
	return copyItemRecursePattern.ReplaceAllStringFunc(body, func(match string) string {
		sub := copyItemRecursePattern.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		from := strings.TrimRight(sub[1], `\`)
		return fmt.Sprintf(`Copy-Tree "%s" "%s"`, from, sub[2])
	})
}

// patchOptionalHookLines wraps hook statements ending in 2>$null so Write-Error does not
// terminate the install when $ErrorActionPreference is Stop (Scoop optional-hook convention).
func patchOptionalHookLines(body string) string {
	body = strings.ReplaceAll(body, "\n", "\r\n")
	lines := strings.Split(body, "\r\n")
	for i, line := range lines {
		lines[i] = patchOptionalHookLine(line)
	}
	return strings.Join(lines, "\r\n")
}

func patchOptionalHookLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return line
	}
	hook := trimmed
	comment := ""
	if idx := strings.Index(trimmed, " #"); idx >= 0 {
		hook = strings.TrimSpace(trimmed[:idx])
		comment = trimmed[idx:]
	}
	if !strings.HasSuffix(hook, "2>$null") {
		return line
	}
	hook = strings.TrimSpace(strings.TrimSuffix(hook, "2>$null"))
	return fmt.Sprintf("$ErrorActionPreference = 'Continue'; %s 2>$null; $ErrorActionPreference = 'Stop'%s", hook, comment)
}

// substituteInstallHookVars replaces Scoop hook variables inline (legacy tests).
func substituteInstallHookVars(script, dir, fname string) string {
	script = strings.ReplaceAll(script, "$dir", dir)
	script = strings.ReplaceAll(script, "$fname", fname)
	return script
}
