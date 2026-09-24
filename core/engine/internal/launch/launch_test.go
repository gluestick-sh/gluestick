package launch

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/apps"
)

func writeTestPE(t *testing.T, path string, console bool) {
	t.Helper()
	data := make([]byte, 256)
	data[0], data[1] = 'M', 'Z'
	const peOffset = 128
	binary.LittleEndian.PutUint32(data[0x3C:], peOffset)
	copy(data[peOffset:], []byte("PE\x00\x00"))
	subsys := uint16(2)
	if console {
		subsys = 3
	}
	binary.LittleEndian.PutUint16(data[peOffset+92:], subsys)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func testEngine(root string) *runtime.Engine {
	return &runtime.Engine{Config: &etypes.EngineConfig{RootDir: root}}
}

func TestClassifyLaunchTarget7zip(t *testing.T) {
	root := t.TempDir()
	pkgName := "7zip"
	version := "26.01"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "7z.exe"), true)
	writeTestPE(t, filepath.Join(installDir, "7zFM.exe"), false)
	writeTestPE(t, filepath.Join(installDir, "7zG.exe"), false)
	manifestJSON := `{
		"version": "26.01",
		"bin": ["7z.exe", "7zG.exe", "7zFM.exe"],
		"shortcuts": [["7zFM.exe", "7-Zip File Manager"]]
	}`
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "26.01",
		"bucket": "main",
		"manifest": `+manifestJSON+`
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	rec, err := apps.LoadInstallRecord(installDir)
	if err != nil {
		t.Fatal(err)
	}
	m := rec.Manifest

	cases := []struct {
		path   string
		source LaunchSource
		want   LaunchKind
	}{
		{"7z.exe", LaunchSourceBin, LaunchKindConsole},
		{"7zG.exe", LaunchSourceBin, LaunchKindGUI},
		{"7zFM.exe", LaunchSourceShortcut, LaunchKindGUI},
	}
	for _, tc := range cases {
		abs := filepath.Join(installDir, tc.path)
		got := AutoLaunchKind(e, installDir, abs, m, tc.source)
		if got != tc.want {
			t.Fatalf("%s source=%v: got %q want %q", tc.path, tc.source, got, tc.want)
		}
	}
}

func TestListLaunchCandidates7zip(t *testing.T) {
	root := t.TempDir()
	pkgName := "7zip"
	version := "26.01"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "7z.exe"), true)
	writeTestPE(t, filepath.Join(installDir, "7zFM.exe"), false)
	writeTestPE(t, filepath.Join(installDir, "7zG.exe"), false)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "26.01",
		"bucket": "main",
		"manifest": {
			"version":"26.01",
			"bin":["7z.exe","7zG.exe","7zFM.exe"],
			"shortcuts":[["7zFM.exe","7-Zip File Manager"]]
		}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	candidates, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatalf("ListLaunchCandidates: %v", err)
	}

	if len(candidates) != 3 {
		t.Fatalf("candidates = %d, want 3: %+v", len(candidates), candidates)
	}
	for _, c := range candidates {
		if !c.Openable {
			t.Fatalf("%s should be openable by default: %+v", c.Label, c)
		}
	}

	targets, err := ListLaunchTargets(e, pkgName)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %d, want 3: %+v", len(targets), targets)
	}
}

func TestPackageIconPathPrefersShortcutGUI(t *testing.T) {
	root := t.TempDir()
	pkgName := "7zip"
	version := "26.01"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "7z.exe"), true)
	writeTestPE(t, filepath.Join(installDir, "7zFM.exe"), false)
	writeTestPE(t, filepath.Join(installDir, "7zG.exe"), false)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "26.01",
		"bucket": "main",
		"manifest": {
			"version":"26.01",
			"bin":["7z.exe","7zG.exe","7zFM.exe"],
			"shortcuts":[["7zFM.exe","7-Zip File Manager"]]
		}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	got, err := PackageIconPath(e, pkgName)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(installDir, "7zFM.exe")
	if got != want {
		t.Fatalf("PackageIconPath = %q, want %q", got, want)
	}
}

func TestLaunchIndexOverride(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "apps", "demo", "1.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(installDir, "tool.exe")
	writeTestPE(t, tool, false)

	e := testEngine(root)
	if err := SetLaunchPreference(e, "demo", "tool.exe", "gui"); err != nil {
		t.Fatal(err)
	}
	got := EffectiveLaunchKind(e, "demo", installDir, tool, nil, LaunchSourceScan)
	if got != LaunchKindGUI {
		t.Fatalf("override = %q, want gui", got)
	}
}

func TestOpenLaunchTargetRemapsOtherVersionPath(t *testing.T) {
	root := t.TempDir()
	pkgName := "zotero"
	activeVer := "9.0.4"
	staleVer := "9.0.5"
	pkgRoot := filepath.Join(root, "apps", pkgName)
	activeDir := filepath.Join(pkgRoot, activeVer)
	staleDir := filepath.Join(pkgRoot, staleVer)
	for _, dir := range []string{activeDir, staleDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		writeTestPE(t, filepath.Join(dir, "zotero.exe"), false)
		if err := os.WriteFile(filepath.Join(dir, "install.json"), []byte(`{
			"version": "`+filepath.Base(dir)+`",
			"bucket": "main",
			"manifest": {"version":"`+filepath.Base(dir)+`","bin":"zotero.exe"}
		}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := apps.LinkCurrent(pkgRoot, activeVer); err != nil {
		t.Fatal(err)
	}

	stalePath := filepath.Join(staleDir, "zotero.exe")
	e := testEngine(root)
	resolved, err := ResolveLaunchAbsPath(e, pkgName, activeDir, stalePath)
	if err != nil {
		t.Fatalf("ResolveLaunchAbsPath: %v", err)
	}
	want := filepath.Join(activeDir, "zotero.exe")
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

func TestOpenLaunchTargetRejectsOutsideInstallDir(t *testing.T) {
	root := t.TempDir()
	pkgName := "demo"
	version := "1.0"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(installDir, "app.exe")
	writeTestPE(t, exePath, true)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "1.0",
		"bucket": "main",
		"manifest": {"version":"1.0","bin":"app.exe"}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(root, "outside.exe")
	writeTestPE(t, outside, true)

	e := testEngine(root)
	if err := OpenLaunchTarget(e, pkgName, outside); err == nil {
		t.Fatal("expected error for path outside install dir")
	}
}

func TestSetLaunchPreferencesBatch(t *testing.T) {
	root := t.TempDir()
	e := testEngine(root)
	if err := SetLaunchPreferences(e, "zotero", map[string]string{
		"app.exe":       "gui",
		"helper.exe":    "console",
		"uninstall.exe": "skip",
	}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]LaunchKind{
		"app.exe":       LaunchKindGUI,
		"helper.exe":    LaunchKindConsole,
		"uninstall.exe": LaunchKindSkip,
	} {
		got, ok := LaunchIndexKind(e, "zotero", rel)
		if !ok || got != want {
			t.Fatalf("%s: got (%q, %v), want %q", rel, got, ok, want)
		}
	}
	if err := SetLaunchPreferences(e, "zotero", map[string]string{
		"helper.exe":    "skip",
		"uninstall.exe": "auto",
	}); err != nil {
		t.Fatal(err)
	}
	if kind, ok := LaunchIndexKind(e, "zotero", "helper.exe"); !ok || kind != LaunchKindSkip {
		t.Fatalf("helper: got (%q, %v)", kind, ok)
	}
	if _, ok := LaunchIndexKind(e, "zotero", "uninstall.exe"); ok {
		t.Fatal("expected uninstall override cleared")
	}
}

func TestListLaunchCandidatesFiltersByManifestBin(t *testing.T) {
	root := t.TempDir()
	pkgName := "pycharm"
	version := "2026.1"
	installDir := filepath.Join(root, "apps", pkgName, version)
	binDir := filepath.Join(installDir, "IDE", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(binDir, "pycharm64.exe"), false)
	writeTestPE(t, filepath.Join(binDir, "jetbrains_client64.exe"), false)
	writeTestPE(t, filepath.Join(binDir, "restarter.exe"), false)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "2026.1",
		"bucket": "extras",
		"manifest": {
			"version":"2026.1",
			"architecture": {
				"64bit": {
					"bin": [["IDE\\bin\\pycharm64.exe", "pycharm"]],
					"shortcuts": [["IDE\\bin\\pycharm64.exe", "JetBrains\\PyCharm"]]
				}
			}
		}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	candidates, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatalf("ListLaunchCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 (manifest bin only): %+v", len(candidates), candidates)
	}
	if !strings.HasSuffix(strings.ToLower(candidates[0].Path), "pycharm64.exe") {
		t.Fatalf("unexpected candidate: %+v", candidates[0])
	}
}

func TestListLaunchCandidatesPythonBins(t *testing.T) {
	root := t.TempDir()
	pkgName := "python"
	version := "3.14.6"
	installDir := filepath.Join(root, "apps", pkgName, version)
	idleBat := filepath.Join(installDir, "Lib", "idlelib", "idle.bat")
	if err := os.MkdirAll(filepath.Dir(idleBat), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "python.exe"), true)
	writeTestPE(t, filepath.Join(installDir, "pythonw.exe"), false)
	if err := os.WriteFile(idleBat, []byte("@echo off\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(installDir, "Scripts"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "Scripts", "pip.exe"), true)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "3.14.6",
		"bucket": "main",
		"manifest": {
			"version":"3.14.6",
			"bin": [
				["python.exe", "python3"],
				"Lib\\idlelib\\idle.bat",
				["Lib\\idlelib\\idle.bat", "idle3"]
			]
		}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	candidates, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatalf("ListLaunchCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2 (python.exe + idle.bat): %+v", len(candidates), candidates)
	}
}

func TestListLaunchCandidatesFallsBackWhenBinMissing(t *testing.T) {
	root := t.TempDir()
	pkgName := "orphan-bin"
	version := "1.0"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "app.exe"), false)
	writeTestPE(t, filepath.Join(installDir, "helper.exe"), false)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "1.0",
		"bucket": "main",
		"manifest": {
			"version":"1.0",
			"bin": "missing.exe"
		}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	candidates, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatalf("ListLaunchCandidates: %v", err)
	}
	if len(candidates) < 2 {
		t.Fatalf("expected directory scan fallback, got %d: %+v", len(candidates), candidates)
	}
}

func TestListLaunchCandidatesNoBinUsesScan(t *testing.T) {
	root := t.TempDir()
	pkgName := "no-bin"
	version := "1.0"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "only.exe"), false)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "1.0",
		"bucket": "main",
		"manifest": {"version":"1.0"}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	candidates, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatalf("ListLaunchCandidates: %v", err)
	}
	if len(candidates) != 1 || !strings.HasSuffix(candidates[0].RelPath, "only.exe") {
		t.Fatalf("expected scan-only candidate, got %+v", candidates)
	}
}

func TestRemoveAndAddLaunchEntry(t *testing.T) {
	root := t.TempDir()
	pkgName := "demo"
	version := "1.0"
	installDir := filepath.Join(root, "apps", pkgName, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestPE(t, filepath.Join(installDir, "a.exe"), false)
	writeTestPE(t, filepath.Join(installDir, "b.exe"), false)
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{"version":"1.0","bucket":"main"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(filepath.Join(root, "apps", pkgName), version); err != nil {
		t.Fatal(err)
	}

	e := testEngine(root)
	before, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("before = %d, want 2", len(before))
	}
	if err := RemoveLaunchEntry(e, pkgName, "a.exe"); err != nil {
		t.Fatal(err)
	}
	afterRemove, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRemove) != 1 || afterRemove[0].RelPath != "b.exe" {
		t.Fatalf("after remove = %+v", afterRemove)
	}
	if err := AddLaunchEntry(e, pkgName, "a.exe", "gui"); err != nil {
		t.Fatal(err)
	}
	afterAdd, err := ListLaunchCandidates(e, pkgName)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterAdd) != 2 {
		t.Fatalf("after add = %d, want 2", len(afterAdd))
	}
}

func TestSetLaunchPreferencePersists(t *testing.T) {
	root := t.TempDir()
	e := testEngine(root)
	if err := SetLaunchPreference(e, "7zip", "7zG.exe", "gui"); err != nil {
		t.Fatal(err)
	}
	kind, ok := LaunchIndexKind(e, "7zip", "7zG.exe")
	if !ok || kind != LaunchKindGUI {
		t.Fatalf("got (%q, %v)", kind, ok)
	}
	if err := SetLaunchPreference(e, "7zip", "7zG.exe", "auto"); err != nil {
		t.Fatal(err)
	}
	if _, ok := LaunchIndexKind(e, "7zip", "7zG.exe"); ok {
		t.Fatal("expected override cleared")
	}
}
