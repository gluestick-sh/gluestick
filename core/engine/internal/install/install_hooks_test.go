package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestInstallerScriptHooksFromManifest(t *testing.T) {
	raw := `{
		"version": "1.0",
		"url": "https://example.com/setup.exe",
		"hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"installer": { "script": ["Write-Host test"] },
		"bin": "app.exe"
	}`
	m, err := manifest.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !m.HasInstallerScript() {
		t.Fatal("expected HasInstallerScript")
	}
	hooks := m.InstallerScriptHooks()
	if len(hooks) != 1 || hooks[0] != "Write-Host test" {
		t.Fatalf("hooks = %#v", hooks)
	}
}

func TestPreInstallHooksFromManifest(t *testing.T) {
	raw := `{
		"version": "1.0",
		"url": "https://example.com/app.jar",
		"hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"pre_install": "Set-Content -Path \"$dir\\run.bat\" -Value test",
		"bin": "run.bat"
	}`
	m, err := manifest.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	hooks := m.PreInstallHooks()
	if len(hooks) != 1 || !strings.Contains(hooks[0], "$dir") {
		t.Fatalf("hooks = %#v", hooks)
	}
}

func TestBuildInstallHookScriptFreecadPostInstall(t *testing.T) {
	dir := `C:\Users\test\.glue\apps\freecad\1.1.1`
	glueRoot := `C:\Users\test\.glue`
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir: dir,
		GlueRoot:   glueRoot,
		Hooks: []string{
			`startmenu_shortcut "$original_dir\bin\FreeCAD.exe" 'FreeCAD' $null $null $global`,
			`shim "$original_dir\bin\FreeCADCmd.exe" $global 'FreeCADCmd'`,
		},
	})
	for _, want := range []string{
		"function startmenu_shortcut",
		"function shim(",
		"function shortcut_folder",
		"function rm_shim",
		"'Programs', 'Glue Apps'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing %q in hook script", want)
		}
	}
}

func TestBuildInstallHookScriptPhp85(t *testing.T) {
	dir := `C:\glue\apps\php\8.5.6`
	hooks := []string{
		"# Create directory for custom PHP configuration",
		"if (!(Test-Path \"$dir\\cli\\conf.d\")) {",
		" (New-Item -Type directory \"$dir\\cli\\conf.d\") | Out-Null",
		"}",
	}
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir: dir, DownloadName: "php.zip", Version: "8.5.6", Hooks: hooks,
	})
	if !strings.Contains(script, "if (!(Test-Path \"$dir\\cli\\conf.d\")) {") {
		t.Fatalf("missing if block: %q", script)
	}
	if !strings.Contains(script, "(New-Item -Type directory \"$dir\\cli\\conf.d\") | Out-Null") {
		t.Fatalf("missing New-Item line: %q", script)
	}
	if !strings.Contains(script, "function is_admin") {
		t.Fatalf("missing is_admin helper: %q", script)
	}
	if !strings.Contains(script, "function Invoke-ExternalCommand") {
		t.Fatalf("missing Invoke-ExternalCommand helper: %q", script)
	}
	if !strings.Contains(script, "function Expand-MsiArchive") {
		t.Fatalf("missing Expand-MsiArchive helper: %q", script)
	}
	if !strings.Contains(script, `Join-Path $env:WINDIR 'System32\msiexec.exe'`) {
		t.Fatalf("Expand-MsiArchive should resolve msiexec via WINDIR: %q", script)
	}
	if !strings.Contains(script, "$dir = 'C:\\glue\\apps\\php\\8.5.6'") || !strings.Contains(script, "$fname = 'php.zip'") {
		t.Fatalf("missing dir/fname assignment: %q", script)
	}
}

func TestInvokeExternalCommandSupportsContinueExitCodes(t *testing.T) {
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\vcredist\1.0`,
		DownloadName: `vc_redist.x64.exe`,
		Version:      "1.0",
		App:          "vcredist2022",
		Hooks: []string{
			`$ec = @{ 1638 = 'already installed'; 3010 = 'restart required' }`,
			`Invoke-ExternalCommand -FilePath "$dir\vc_redist.x64.exe" -ArgumentList '/quiet' -ContinueExitCodes $ec -RunAs | Out-Null`,
		},
	})
	if !strings.Contains(script, "ContinueExitCodes") {
		t.Fatalf("missing ContinueExitCodes parameter: %q", script)
	}
}

func TestBuildInstallHookScriptPrependsInstallDirToPath(t *testing.T) {
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\tailscale\1.98.4`,
		DownloadName: `tailscale-setup-1.98.4-arm64.msi`,
		Hooks:        []string{"tailscaled.exe install-system-daemon"},
	})
	if !strings.Contains(script, `$env:PATH = "$dir;$glue_old_path"`) {
		t.Fatalf("expected install dir on PATH during hooks: %q", script)
	}
	if !strings.Contains(script, "Push-Location -LiteralPath $dir") {
		t.Fatalf("expected Push-Location to install dir: %q", script)
	}
	if !strings.Contains(script, "tailscaled.exe install-system-daemon") {
		t.Fatalf("missing hook body: %q", script)
	}
}

func TestBuildInstallHookScript_wiresharkPreInstallBraces(t *testing.T) {
	hooks := []string{
		"'$PLUGINSDIR', 'vc_redist*', 'uninstall-wireshark.exe' | ForEach-Object { \"$dir/$_\" } | Remove-Item -Recurse -ErrorAction Ignore",
		"Get-ChildItem -Path \"$dir/npcap-*.exe\" | Select-Object -First 1 | Rename-Item -NewName 'npcap-installer.exe'",
		"$data = \"$persist_dir/Data\"",
		"$preferences = \"$data/preferences\"",
		"if (!(Test-Path $preferences)) {",
		"    $null = New-Item -ItemType Directory $data -ErrorAction Ignore",
		"    'gui.update.enabled: FALSE' | Out-File -Encoding utf8 $preferences",
		"}",
	}
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\wireshark\4.6.6`,
		DownloadName: `Wireshark-4.6.6-x64.exe`,
		PersistDir:   `C:\apps\wireshark\persist\wireshark`,
		Hooks:        hooks,
	})
	if strings.Contains(script, "try { if (!(Test-Path") {
		t.Fatalf("hook body must not be wrapped in try/finally: %q", script)
	}
	if !strings.Contains(script, "if (!(Test-Path $preferences)) {") {
		t.Fatalf("missing multiline if hook: %q", script)
	}
	if runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "$errors = $null; [void][System.Management.Automation.Language.Parser]::ParseInput($env:GLUE_TEST_SCRIPT, [ref]$null, [ref]$errors); if ($errors) { $errors | ForEach-Object { $_.ToString() }; exit 1 }")
	cmd.Env = append(os.Environ(), "GLUE_TEST_SCRIPT="+script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell parse failed: %v\n%s", err, out)
	}
}

func TestBuildInstallHookScript_wiresharkPostInstallOptional(t *testing.T) {
	hooks := []string{
		`Copy-Item -Force "$bucketsdir/extras/scripts/wireshark/*" "$dir"`,
		`& "$dir/enable-usbpcap.ps1" 2>$null   # Attempt try to enable USBPcap if already installed`,
	}
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\wireshark\4.6.6`,
		DownloadName: `Wireshark-4.6.6-x64.exe`,
		BucketsDir:   `C:\buckets`,
		Hooks:        hooks,
	})
	if !strings.Contains(script, `$ErrorActionPreference = 'Continue'; & "$dir/enable-usbpcap.ps1" 2>$null; $ErrorActionPreference = 'Stop'`) {
		t.Fatalf("expected optional hook wrapper: %q", script)
	}
	if strings.Contains(script, "if ((Get-Item") {
		t.Fatal("inline Get-Item 2>$null should not be wrapped")
	}
}

func TestBuildInstallHookScriptAdminCheck(t *testing.T) {
	hooks := []string{"if (!(is_admin)) { error 'Admin privileges are required.'; break }"}
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\dotnet\6.0`,
		DownloadName: `setup.exe`,
		Version:      "6.0",
		Hooks:        hooks,
	})
	if !strings.Contains(script, "function is_admin") {
		t.Fatalf("missing is_admin: %q", script)
	}
	if !strings.Contains(script, "if (!(is_admin)) { error 'Admin privileges are required.'; break }") {
		t.Fatalf("missing admin hook body: %q", script)
	}
}

func TestHookHelpersNeedDark(t *testing.T) {
	if !hookHelpersNeedDark([]string{`Expand-DarkArchive "$dir\$fname" -DestinationPath "$dir\.tmp"`}) {
		t.Fatal("expected Expand-DarkArchive hook to need dark")
	}
	if hookHelpersNeedDark([]string{`Expand-MsiArchive "$dir\setup.msi"`}) {
		t.Fatal("Expand-MsiArchive should not need dark helper")
	}
}

func TestBuildInstallHookScriptExpandDark(t *testing.T) {
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\powertoys\0.100.0`,
		DownloadName: `setup.exe`,
		Version:      "0.100.0",
		App:          "powertoys",
		Arch:         "arm64",
		Dark:         `C:\glue\apps\dark\3.11.2\dark.exe`,
		Hooks:        []string{`Expand-DarkArchive "$dir\$fname" -DestinationPath "$dir\.tmp"`},
	})
	if !strings.Contains(script, "function Expand-DarkArchive") {
		t.Fatalf("missing Expand-DarkArchive helper: %q", script)
	}
	if !strings.Contains(script, "$global:GLUE_DARK = 'C:\\glue\\apps\\dark\\3.11.2\\dark.exe'") {
		t.Fatalf("missing GLUE_DARK path: %q", script)
	}
	if strings.Contains(script, "$etypes.Status") {
		t.Fatal("hook helpers must not assign $etypes.Status; use a PowerShell $ok variable")
	}
	if !strings.Contains(script, "$ok = Invoke-ExternalCommand -FilePath $DarkPath") {
		t.Fatal("Expand-DarkArchive must capture Invoke-ExternalCommand into $ok")
	}
}

func TestHookHelpersNeed7z(t *testing.T) {
	if !hookHelpersNeed7z([]string{`Expand-7zipArchive "$dir\setup.exe" -Switches '-t#'`}) {
		t.Fatal("expected Expand-7zipArchive hook to need 7z")
	}
	if hookHelpersNeed7z([]string{`Expand-MsiArchive "$dir\setup.msi"`}) {
		t.Fatal("Expand-MsiArchive should not need 7z helper")
	}
}

func TestBuildInstallHookScriptExpand7zip(t *testing.T) {
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\wps\1.0`,
		DownloadName: `setup.exe`,
		Version:      "1.0",
		App:          "wpsoffice",
		Bucket:       "main",
		BucketsDir:   `C:\glue\buckets`,
		PersistDir:   `C:\glue\persist\wpsoffice`,
		Arch:         "64bit",
		SevenZip:     `C:\glue\shims\7z.exe`,
		Hooks:        []string{`Expand-7zipArchive "$dir\$fname" -Switches '-t#'`},
	})
	if !strings.Contains(script, "$persist_dir = 'C:\\glue\\persist\\wpsoffice'") {
		t.Fatalf("missing persist_dir: %q", script)
	}
	if !strings.Contains(script, "$bucketsdir = 'C:\\glue\\buckets'") {
		t.Fatalf("missing bucketsdir: %q", script)
	}
	if !strings.Contains(script, "function Expand-7zipArchive") {
		t.Fatalf("missing Expand-7zipArchive helper: %q", script)
	}
	if !strings.Contains(script, "$global:GLUE_7Z = 'C:\\glue\\shims\\7z.exe'") {
		t.Fatalf("missing GLUE_7Z path: %q", script)
	}
}

func TestSubstituteInstallHookVars(t *testing.T) {
	got := substituteInstallHookVars(`echo $dir $fname`, `C:\apps\pkg\1.0`, `app.jar`)
	want := `echo C:\apps\pkg\1.0 app.jar`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPatchRecurseCopyItem_bravePostInstall(t *testing.T) {
	body := `if (!(Test-Path "$dir\User Data\*")) {
  Copy-Item "$env:LocalAppData\BraveSoftware\Brave-Browser\User Data\*" "$dir\User Data" -Recurse
}`
	got := patchRecurseCopyItem(body)
	want := `Copy-Tree "$env:LocalAppData\BraveSoftware\Brave-Browser\User Data" "$dir\User Data"`
	if !strings.Contains(got, want) {
		t.Fatalf("expected patched Copy-Tree, got:\n%s", got)
	}
	if strings.Contains(got, "Copy-Item") {
		t.Fatalf("Copy-Item should be replaced: %q", got)
	}
}

func TestBuildInstallHookScript_includesCopyTree(t *testing.T) {
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir: `C:\apps\brave\1.0`,
		Hooks: []string{
			`Copy-Item "$persist_dir\data\*" "$dir\User Data" -Recurse`,
		},
	})
	if !strings.Contains(script, "function Copy-Tree") {
		t.Fatalf("missing Copy-Tree helper")
	}
	if !strings.Contains(script, `Copy-Tree "$persist_dir\data" "$dir\User Data"`) {
		t.Fatalf("expected patched Copy-Tree call: %q", script)
	}
	if strings.Contains(script, "Start-Process -FilePath 'robocopy.exe'") {
		t.Fatalf("Copy-Tree must not use Start-Process -ArgumentList (breaks paths with spaces)")
	}
	if !strings.Contains(script, "& robocopy.exe $from $to") {
		t.Fatalf("expected call-operator robocopy in Copy-Tree: %q", script)
	}
	if !strings.Contains(script, "$ErrorActionPreference = 'Stop'") {
		t.Fatalf("expected strict error handling")
	}
}

// Regression: Brave portable post_install copies LocalAppData\...\User Data into
// $dir\User Data. Start-Process -ArgumentList used to split those paths on spaces
// (robocopy exit 16 / invalid parameter #3).
func TestCopyTreePathsWithSpaces(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows robocopy")
	}
	root := t.TempDir()
	src := filepath.Join(root, "Brave-Browser", "User Data")
	dst := filepath.Join(root, "apps", "brave", "1.0", "User Data")
	if err := os.MkdirAll(filepath.Join(src, "Default"), 0755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(src, "Default", "Preferences")
	if err := os.WriteFile(marker, []byte(`{"ok":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}

	hook := fmt.Sprintf(
		`Copy-Tree %s %s`,
		psSingleQuoted(src),
		psSingleQuoted(dst),
	)
	if err := runPreInstallHooks(HookScriptEnv{
		InstallDir: filepath.Dir(dst),
		Hooks:      []string{hook},
	}); err != nil {
		t.Fatalf("Copy-Tree with spaces: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "Default", "Preferences"))
	if err != nil {
		t.Fatalf("copied file missing: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("content = %q", got)
	}
}

func TestRunPreInstallHooksCreatesBat(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell")
	}
	dir := t.TempDir()
	hook := `Set-Content -Path "$dir\mindustry.bat" -Value "javaw -jar Mindustry.jar"`
	if err := runPreInstallHooks(HookScriptEnv{
		InstallDir: dir, DownloadName: "Mindustry.jar", Hooks: []string{hook},
	}); err != nil {
		t.Fatalf("runPreInstallHooks: %v", err)
	}
	bat := filepath.Join(dir, "mindustry.bat")
	data, err := os.ReadFile(bat)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "Mindustry.jar") {
		t.Fatalf("bat content = %q", string(data))
	}
}

func TestRunPreInstallHooksMultilineArray(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell")
	}
	dir := t.TempDir()
	hooks := []string{
		"if (!(Test-Path \"$dir\\cli\\conf.d\")) {",
		" (New-Item -Type directory \"$dir\\cli\\conf.d\") | Out-Null",
		"}",
	}
	if err := runPreInstallHooks(HookScriptEnv{
		InstallDir: dir, DownloadName: "php.zip", Version: "8.5", Hooks: hooks,
	}); err != nil {
		t.Fatalf("runPreInstallHooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cli", "conf.d")); err != nil {
		t.Fatalf("conf.d not created: %v", err)
	}
}

func TestBuildPreUninstallHookScriptSetsCmd(t *testing.T) {
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir: `C:\glue\apps\tailscale\1.98.4`,
		Cmd:        "uninstall",
		Hooks:      []string{"Stop-Service -Name 'Tailscale' -Force -ErrorAction SilentlyContinue"},
	})
	if !strings.Contains(script, "$cmd = 'uninstall'") {
		t.Fatalf("expected $cmd uninstall: %q", script)
	}
}

func TestBuildInstallHookScript_pythonUninstallerUsesInstalledHelper(t *testing.T) {
	hooks := []string{
		`$global = installed $app $true`,
		`if ($global) {`,
		`    $pathext = (Get-EnvVar -Name PATHEXT -Global) -replace ';.PYW?', ''`,
		`    Set-EnvVar -Name PATHEXT -Value $pathext -Global`,
		`} else {`,
		`    $pathext = (Get-EnvVar -Name PATHEXT) -replace ';.PYW?', ''`,
		`    Set-EnvVar -Name PATHEXT -Value $pathext`,
		`}`,
	}
	script := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\Users\test\.glue\apps\python\3.13.2`,
		DownloadName: `python-installer.exe`,
		Version:      "3.13.2",
		App:          "python",
		Cmd:          "uninstall",
		GlueRoot:     `C:\Users\test\.glue`,
		Hooks:        hooks,
	})
	for _, want := range []string{
		"function installed(",
		"function Select-CurrentVersion",
		"function appsdir",
		"$global = installed $app $true",
		"$cmd = 'uninstall'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing %q in hook script", want)
		}
	}
	if runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "$errors = $null; [void][System.Management.Automation.Language.Parser]::ParseInput($env:GLUE_TEST_SCRIPT, [ref]$null, [ref]$errors); if ($errors) { $errors | ForEach-Object { $_.ToString() }; exit 1 }")
	cmd.Env = append(os.Environ(), "GLUE_TEST_SCRIPT="+script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell parse failed: %v\n%s", err, out)
	}
}

func TestRunUninstallerHooksIgnoresRegImportExitCode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows PowerShell")
	}
	dir := t.TempDir()
	regPath := filepath.Join(dir, "uninstall-context.reg")
	reg := "Windows Registry Editor Version 5.00\r\n\r\n[-HKEY_CURRENT_USER\\Software\\7-Zip]\r\n"
	if err := os.WriteFile(regPath, []byte(reg), 0644); err != nil {
		t.Fatal(err)
	}
	hooks := []string{`if ($cmd -eq 'uninstall') { reg import "$dir\uninstall-context.reg" *> $null }`}
	if err := RunUninstallerHooks(HookScriptEnv{
		InstallDir: dir,
		Hooks:      hooks,
	}); err != nil {
		t.Fatalf("RunUninstallerHooks: %v", err)
	}
}
