package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestRestorePersistOnInstall_skipsFileUntilPreInstall(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	xmlPath := filepath.Join(installDir, "nativeLang.xml")
	if err := os.WriteFile(xmlPath, []byte("lang"), 0444); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "nativeLang.xml", Data: "nativeLang.xml"}}
	if err := restorePersistOnInstall(installDir, persistDir, persist); err != nil {
		t.Fatalf("restorePersistOnInstall: %v", err)
	}
	info, err := os.Stat(xmlPath)
	if err != nil {
		t.Fatalf("nativeLang.xml missing: %v", err)
	}
	if info.IsDir() {
		t.Fatal("file persist path should not become a directory before pre_install")
	}
}

func TestRestorePersistOnInstall_restoresFileFromPersist(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	persistPath := filepath.Join(persistDir, "config.xml")
	if err := os.WriteFile(persistPath, []byte("cfg"), 0644); err != nil {
		t.Fatal(err)
	}
	installPath := filepath.Join(installDir, "config.xml")
	if err := os.WriteFile(installPath, []byte("default"), 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "config.xml", Data: "config.xml"}}
	if err := restorePersistOnInstall(installDir, persistDir, persist); err != nil {
		t.Fatalf("restorePersistOnInstall: %v", err)
	}
	data, err := os.ReadFile(installPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "cfg" {
		t.Fatalf("got %q, want cfg", data)
	}
}

func TestPrepareInstallDirForPreInstallHooks_removesStalePlaceholders(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	iniPath := filepath.Join(installDir, "UltraVnc.ini")
	if err := os.WriteFile(iniPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{
		{Install: "UltraVnc.ini", Data: "UltraVnc.ini"},
		{Install: "options.vnc", Data: "options.vnc"},
	}
	if err := prepareInstallDirForPreInstallHooks(installDir, persistDir, persist); err != nil {
		t.Fatalf("prepareInstallDirForPreInstallHooks: %v", err)
	}
	if _, err := os.Stat(iniPath); !os.IsNotExist(err) {
		t.Fatalf("stale placeholder should be removed: %v", err)
	}
}

func TestPrepareInstallDirForPreInstallHooks_keepsExtractedPersistDirs(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	dataDir := filepath.Join(installDir, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "obs-studio"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "obs-studio", "global.ini"), []byte("cfg"), 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{
		{Install: "data", Data: "data"},
		{Install: "obs-plugins", Data: "obs-plugins"},
	}
	if err := prepareInstallDirForPreInstallHooks(installDir, persistDir, persist); err != nil {
		t.Fatalf("prepareInstallDirForPreInstallHooks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "obs-studio", "global.ini")); err != nil {
		t.Fatalf("extracted data should remain: %v", err)
	}
}

func TestPrepareInstallDirForPreInstallHooks_keepsWhenPersistExists(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	iniPath := filepath.Join(installDir, "UltraVnc.ini")
	persistPath := filepath.Join(persistDir, "UltraVnc.ini")
	if err := os.WriteFile(persistPath, []byte("cfg"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iniPath, []byte("cfg"), 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "UltraVnc.ini", Data: "UltraVnc.ini"}}
	if err := prepareInstallDirForPreInstallHooks(installDir, persistDir, persist); err != nil {
		t.Fatalf("prepareInstallDirForPreInstallHooks: %v", err)
	}
	if _, err := os.Stat(iniPath); err != nil {
		t.Fatalf("install file should remain when persist exists: %v", err)
	}
}

func TestEnsureInstallPersistDirectory_replacesFileStub(t *testing.T) {
	dir := t.TempDir()
	userData := filepath.Join(dir, "User Data")
	if err := os.WriteFile(userData, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureInstallPersistDirectory(userData); err != nil {
		t.Fatalf("ensureInstallPersistDirectory: %v", err)
	}
	info, err := os.Stat(userData)
	if err != nil {
		t.Fatalf("User Data missing: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("User Data should be a directory")
	}
}

func TestPrepareInstallDirForPostInstallHooks_ensuresDirectory(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	userData := filepath.Join(installDir, "User Data")
	if err := os.WriteFile(userData, nil, 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "User Data", Data: "User Data"}}
	if err := prepareInstallDirForPostInstallHooks(installDir, persistDir, persist); err != nil {
		t.Fatalf("prepareInstallDirForPostInstallHooks: %v", err)
	}
	info, err := os.Stat(userData)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory")
	}
}

func TestSavePersistOnUninstall_mergesExistingFile(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	persistPath := filepath.Join(persistDir, "Notepad4.ini")
	installPath := filepath.Join(installDir, "Notepad4.ini")
	if err := os.WriteFile(persistPath, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installPath, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "Notepad4.ini", Data: "Notepad4.ini"}}
	if err := SavePersistOnUninstall(installDir, persistDir, persist); err != nil {
		t.Fatalf("SavePersistOnUninstall: %v", err)
	}
	if _, err := os.Stat(installPath); !os.IsNotExist(err) {
		t.Fatalf("install file should be removed: %v", err)
	}
	data, err := os.ReadFile(persistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("got %q, want new", data)
	}
}

func TestPrepareInstallDirForPostInstallHooks_skipsFileEntries(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	iniPath := filepath.Join(installDir, "Notepad4.ini")
	if err := os.WriteFile(iniPath, []byte("cfg"), 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "Notepad4.ini", Data: "Notepad4.ini"}}
	if err := prepareInstallDirForPostInstallHooks(installDir, persistDir, persist); err != nil {
		t.Fatalf("prepareInstallDirForPostInstallHooks: %v", err)
	}
	info, err := os.Stat(iniPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal("file persist path should remain a file")
	}
}

func TestSaveAndRestorePersist_roundTrip(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	userData := filepath.Join(installDir, "User Data")
	if err := os.MkdirAll(filepath.Join(userData, "Default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userData, "Default", "Preferences"), []byte("prefs"), 0644); err != nil {
		t.Fatal(err)
	}
	persist := []manifest.PersistEntry{{Install: "User Data", Data: "User Data"}}

	if err := SavePersistOnUninstall(installDir, persistDir, persist); err != nil {
		t.Fatalf("SavePersistOnUninstall: %v", err)
	}
	if _, err := os.Stat(userData); !os.IsNotExist(err) {
		t.Fatalf("install User Data should be removed after save: %v", err)
	}
	persistData := filepath.Join(persistDir, "User Data", "Default", "Preferences")
	if _, err := os.Stat(persistData); err != nil {
		t.Fatalf("persist data missing: %v", err)
	}

	freshInstall := t.TempDir()
	if err := restorePersistOnInstall(freshInstall, persistDir, persist); err != nil {
		t.Fatalf("restorePersistOnInstall: %v", err)
	}
	linked := filepath.Join(freshInstall, "User Data", "Default", "Preferences")
	if _, err := os.Stat(linked); err != nil {
		t.Fatalf("restored data not visible under install dir: %v", err)
	}
}

func TestSavePersistOnUninstall_dropsJunctionWithoutDeletingData(t *testing.T) {
	installDir := t.TempDir()
	persistDir := t.TempDir()
	persistData := filepath.Join(persistDir, "User Data")
	if err := os.MkdirAll(filepath.Join(persistData, "Default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persistData, "Default", "Preferences"), []byte("prefs"), 0644); err != nil {
		t.Fatal(err)
	}
	installUserData := filepath.Join(installDir, "User Data")
	if err := linkDirectoryJunction(installUserData, persistData); err != nil {
		t.Fatalf("linkDirectoryJunction: %v", err)
	}
	persist := []manifest.PersistEntry{{Install: "User Data", Data: "User Data"}}
	if err := SavePersistOnUninstall(installDir, persistDir, persist); err != nil {
		t.Fatalf("SavePersistOnUninstall: %v", err)
	}
	if _, err := os.Stat(installUserData); !os.IsNotExist(err) {
		t.Fatalf("junction should be removed from install dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(persistData, "Default", "Preferences")); err != nil {
		t.Fatalf("persist data should remain: %v", err)
	}
}
