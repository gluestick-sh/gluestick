package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestInstallerScriptNeedsInnounp(t *testing.T) {
	if !installerScriptNeedsInnounp([]string{"Expand-InnoArchive -Path \"$dir\\$fname\""}) {
		t.Fatal("expected Expand-InnoArchive to require innounp")
	}
	if installerScriptNeedsInnounp([]string{"Expand-7zipArchive -Path \"$dir\\$fname\" -DestinationPath $dir"}) {
		t.Fatal("7zip-only script should not require innounp")
	}
}

const testSevenZip = `C:\Users\test\.glue\apps\7zip\current\7z.exe`

func expand7zipHelperFromScript(t *testing.T, script string) string {
	t.Helper()
	const marker = "function Expand-7zipArchive"
	start := strings.Index(script, marker)
	if start < 0 {
		t.Fatalf("missing %s in script", marker)
	}
	open := strings.Index(script[start:], "{")
	if open < 0 {
		t.Fatalf("missing opening brace for Expand-7zipArchive")
	}
	i := start + open
	depth := 0
	for ; i < len(script); i++ {
		switch script[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return script[start : i+1]
			}
		}
	}
	t.Fatal("unterminated Expand-7zipArchive function")
	return ""
}

func TestInstallerAndHookExpand7zipHelperIdentical(t *testing.T) {
	hookScript := buildInstallHookScript(HookScriptEnv{
		InstallDir:   `C:\apps\demo\1.0`,
		DownloadName: `setup.7z`,
		Version:      "1.0",
		App:          "demo",
		Arch:         "64bit",
		SevenZip:     testSevenZip,
		Hooks:        []string{"# noop"},
	})
	installerScript := buildInstallerScript(
		`C:\apps\demo\1.0`, `setup.7z`, `demo`, `main`, `C:\buckets`, ``, testSevenZip, ``, ``, `64bit`, false,
		[]string{"# noop"},
	)
	want := strings.TrimSpace(expand7zipHelperFromScript(t, scoopExpand7zipArchiveHelper(testSevenZip)))
	gotHook := strings.TrimSpace(expand7zipHelperFromScript(t, hookScript))
	gotInstaller := strings.TrimSpace(expand7zipHelperFromScript(t, installerScript))
	if gotHook != want {
		t.Fatalf("hook Expand-7zipArchive drifted from canonical helper\ngot:\n%s\nwant:\n%s", gotHook, want)
	}
	if gotInstaller != want {
		t.Fatalf("installer.script Expand-7zipArchive drifted from canonical helper\ngot:\n%s\nwant:\n%s", gotInstaller, want)
	}
}

func TestBuildInstallerScriptExpand7zipCallPatterns(t *testing.T) {
	const installDir = `C:\apps\pkg\1.0`
	cases := []struct {
		name string
		hook string
	}{
		{
			name: "vivaldi_destination_path",
			hook: `Expand-7zipArchive "$dir\vivaldi.7z" -DestinationPath "$dir\Application" -ExtractDir 'Vivaldi-bin' -Removal`,
		},
		{
			name: "named_path_destination",
			hook: `Expand-7zipArchive -Path "$dir\$fname" -DestinationPath $dir`,
		},
		{
			name: "positional_destination",
			hook: `Expand-7zipArchive "$dir\$fname" $dir`,
		},
		{
			name: "sfx_switches",
			hook: `Expand-7zipArchive "$dir\$fname" -Switches '-t#'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := buildInstallerScript(
				installDir, `pkg.7z`, `pkg`, `main`, `C:\buckets`, ``, testSevenZip, ``, ``, `64bit`, false,
				[]string{tc.hook},
			)
			for _, want := range []string{
				"function Expand-7zipArchive",
				"[string]$DestinationPath",
				"[string]$Switches",
				"function movedir",
				"function abort",
				"$global:GLUE_7Z = 'C:\\Users\\test\\.glue\\apps\\7zip\\current\\7z.exe'",
				tc.hook,
			} {
				if !strings.Contains(script, want) {
					t.Fatalf("script missing %q\n%s", want, script)
				}
			}
			abortPos := strings.Index(script, "function abort")
			expandPos := strings.Index(script, "function Expand-7zipArchive")
			if abortPos < 0 || expandPos < 0 || abortPos > expandPos {
				t.Fatalf("abort must be defined before Expand-7zipArchive (abort=%d expand=%d)", abortPos, expandPos)
			}
		})
	}
}

func TestBuildInstallerScriptExpand7zipMatchesScoop(t *testing.T) {
	hooks := []string{
		`Expand-7zipArchive "$dir\vivaldi.7z" -DestinationPath "$dir\Application" -ExtractDir 'Vivaldi-bin' -Removal`,
	}
	script := buildInstallerScript(
		`C:\Users\test\.glue\apps\vivaldi\8.0.4033.46`,
		`vivaldi.7z`,
		`vivaldi`,
		`extras`,
		`C:\Users\test\.glue\buckets`,
		`C:\Users\test\.glue\persist\vivaldi`,
		testSevenZip,
		``,
		``,
		`arm64`,
		false,
		hooks,
	)
	for _, want := range []string{
		"[string]$DestinationPath",
		"[string]$Switches",
		"function movedir",
		"Expand-7zipArchive \"$dir\\vivaldi.7z\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q\n%s", want, script)
		}
	}
}

func TestBuildInstallerScriptIncludesEnsureHelper(t *testing.T) {
	// Scoop manifests (e.g. opera/opera-gx) call ensure from installer.script.
	script := buildInstallerScript(
		`C:\apps\opera\1.0`,
		`opera.exe`,
		`opera`,
		`extras`,
		`C:\buckets`,
		``,
		testSevenZip,
		``,
		``,
		`64bit`,
		false,
		[]string{`$version, 'autoupdate' | ForEach-Object { ensure "$dir\$_" | Out-Null }`},
	)
	for _, want := range []string{
		"function ensure([string]$path)",
		"function warn([string]$msg)",
		"function strip_ext([string]$path)",
		`ensure "$dir\$_"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("installer.script helpers missing %q", want)
		}
	}
}

func TestBuildInstallerScriptIncludesInnoHelpers(t *testing.T) {
	script := buildInstallerScript(
		`C:\apps\gimp\3.2.4`,
		`gimp-setup.exe`,
		`gimp`,
		`extras`,
		`C:\Users\test\.glue\buckets`,
		`C:\Users\test\.glue\persist\gimp`,
		testSevenZip,
		`C:\Users\test\.glue\apps\innounp\current\innounp.exe`,
		``,
		`64bit`,
		false,
		[]string{`Expand-InnoArchive -Path "$dir\$fname"`},
	)
	for _, want := range []string{
		"function Expand-InnoArchive",
		"function Get-HelperPath",
		"function Invoke-ExternalCommand",
		"function Add-Path",
		"function Remove-Path",
		"[switch]$RunAs",
		"function is_admin",
		"$global = $false",
		"$architecture = '64bit'",
		"Expand-InnoArchive -Path",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q\n%s", want, script)
		}
	}
}

func TestBuildInstallerScriptPythonBurnHelpers(t *testing.T) {
	const testDark = `C:\Users\test\.glue\apps\dark\3.14.1\dark.exe`
	hooks := []string{
		`Expand-DarkArchive "$dir\setup.exe" "$dir\_tmp"`,
		`Expand-MsiArchive "$dir\_tmp\AttachedContainer\core.msi" "$dir"`,
	}
	script := buildInstallerScript(
		`C:\Users\test\.glue\apps\python\3.14.6`,
		`setup.exe`,
		`python`,
		`main`,
		`C:\Users\test\.glue\buckets`,
		`C:\Users\test\.glue\persist\python`,
		testSevenZip,
		``,
		testDark,
		`arm64`,
		false,
		hooks,
	)
	for _, want := range []string{
		"function Expand-DarkArchive",
		"function Expand-MsiArchive",
		"$global:GLUE_DARK = 'C:\\Users\\test\\.glue\\apps\\dark\\3.14.1\\dark.exe'",
		"function fname",
		"Expand-DarkArchive \"$dir\\setup.exe\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q\n%s", want, script)
		}
	}
	abortPos := strings.Index(script, "function abort")
	darkPos := strings.Index(script, "function Expand-DarkArchive")
	msiPos := strings.Index(script, "function Expand-MsiArchive")
	if abortPos < 0 || darkPos < 0 || msiPos < 0 || abortPos > darkPos || abortPos > msiPos {
		t.Fatalf("helpers must be defined after abort (abort=%d dark=%d msi=%d)", abortPos, darkPos, msiPos)
	}
	if strings.Contains(script, "$etypes.Status") {
		t.Fatal("installer helpers must not assign $etypes.Status; use a PowerShell $ok variable")
	}
	if !strings.Contains(script, "$ok = Invoke-ExternalCommand -FilePath $DarkPath") {
		t.Fatal("Expand-DarkArchive must capture Invoke-ExternalCommand into $ok")
	}
	if !strings.Contains(script, "$ok = Invoke-ExternalCommand -FilePath $MsiPath") {
		t.Fatal("Expand-MsiArchive must capture Invoke-ExternalCommand into $ok")
	}
}

func TestMaterializeInstallerFile_allowsMove(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	hash, err := store.Write(bytes.NewReader([]byte("installer-bytes")))
	if err != nil {
		t.Fatal(err)
	}

	installDir := filepath.Join(root, "apps", "demo", "1.0.0")
	if err := materializeInstallerFile(store, installDir, "setup.exe", hash); err != nil {
		t.Fatalf("materializeInstallerFile: %v", err)
	}
	src := filepath.Join(installDir, "setup.exe")
	dst := filepath.Join(filepath.Dir(installDir), "setup.exe")
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("rename installer: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected moved installer at %s: %v", dst, err)
	}
}
