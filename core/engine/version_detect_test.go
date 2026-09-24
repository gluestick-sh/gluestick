package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/manifest"
)

func TestDetectInstalledVersion_fileVersion(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	sysRoot := os.Getenv("SystemRoot")
	src := filepath.Join(sysRoot, "System32", "notepad.exe")
	if _, err := os.Stat(src); err != nil {
		t.Skip("notepad.exe missing")
	}

	dir := t.TempDir()
	dst := filepath.Join(dir, "notepad.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}

	detected, err := apps.ReadFileVersion(dst)
	if err != nil || detected == "" {
		t.Fatalf("ReadFileVersion: %v", err)
	}
	managed := detected
	parts := parseVersionParts(detected)
	if len(parts) > 0 && parts[len(parts)-1].text == "" && parts[len(parts)-1].num > 0 {
		parts[len(parts)-1].num--
		managed = formatVersionParts(parts)
	}

	m := &manifest.Manifest{
		Version: managed,
		Bin:     "notepad.exe",
	}
	got := DetectInstalledVersion(dir, managed, m)
	if got.DetectedVersion == "" {
		t.Fatalf("expected detected version, got %#v", got)
	}
	if got.Source != versionProbeSourceFileVersion {
		t.Fatalf("source = %q, want %s", got.Source, versionProbeSourceFileVersion)
	}
	if !got.ExternallyUpdated {
		t.Fatalf("expected externallyUpdated for managed=%s detected=%s", managed, got.DetectedVersion)
	}
}

func TestDetectInstalledVersion_nestedVersionDir(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Application")
	if err := os.MkdirAll(filepath.Join(app, "9.0.1.2"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app, "8.1.0.0"), 0755); err != nil {
		t.Fatal(err)
	}

	got := DetectInstalledVersion(dir, "8.1.0.0", &manifest.Manifest{Version: "8.1.0.0"})
	if got.DetectedVersion != "9.0.1.2" {
		t.Fatalf("detected = %q, want 9.0.1.2 (%#v)", got.DetectedVersion, got)
	}
	if !got.ExternallyUpdated {
		t.Fatal("expected externallyUpdated")
	}
	if got.Source != versionProbeSourceDir {
		t.Fatalf("source = %q, want %s", got.Source, versionProbeSourceDir)
	}
}

func TestDetectInstalledVersion_noDrift(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Application")
	if err := os.MkdirAll(filepath.Join(app, "8.1.4087.64"), 0755); err != nil {
		t.Fatal(err)
	}
	got := DetectInstalledVersion(dir, "8.1.4087.64", nil)
	if got.ExternallyUpdated {
		t.Fatalf("unexpected drift: %#v", got)
	}
	if got.DetectedVersion != "8.1.4087.64" {
		t.Fatalf("detected = %q", got.DetectedVersion)
	}
}

func TestBinExePath(t *testing.T) {
	if got := binExePath(`[Application\vivaldi.exe,Vivaldi]`); got != `Application\vivaldi.exe` {
		t.Fatalf("got %q", got)
	}
	if got := binExePath("foo.exe"); got != "foo.exe" {
		t.Fatalf("got %q", got)
	}
}

func TestVersionsCompatibleForDrift_torBrowserEngine(t *testing.T) {
	if versionsCompatibleForDrift("15.0.19", "140.13.0.65534") {
		t.Fatal("Tor product version must not compare to Firefox engine FileVersion")
	}
}

func TestVersionsCompatibleForDrift_chromeMajor(t *testing.T) {
	if !versionsCompatibleForDrift("120.0.6099.109", "121.0.6167.85") {
		t.Fatal("adjacent Chrome majors should be comparable")
	}
}

func TestDetectInstalledVersion_torBrowserSidecar(t *testing.T) {
	dir := t.TempDir()
	payload := `{"version":"15.0.19","architecture":"windows-x86_64"}`
	if err := os.WriteFile(filepath.Join(dir, "tbb_version.json"), []byte(payload), 0644); err != nil {
		t.Fatal(err)
	}
	// Simulate misleading engine FileVersion on firefox.exe (not written; sidecar wins first).
	m := &manifest.Manifest{
		Version: "15.0.19",
		Bin:     "firefox.exe",
	}
	got := DetectInstalledVersion(dir, "15.0.19", m)
	if got.DetectedVersion != "15.0.19" {
		t.Fatalf("detected = %q, want 15.0.19 (%#v)", got.DetectedVersion, got)
	}
	if got.Source != versionProbeSourceJSON {
		t.Fatalf("source = %q, want %s", got.Source, versionProbeSourceJSON)
	}
	if got.ExternallyUpdated {
		t.Fatalf("unexpected drift: %#v", got)
	}
}

func TestDetectInstalledVersion_engineFileVersionIgnoredWhenIncompatible(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	dir := t.TempDir()
	sysFirefox := filepath.Join(os.Getenv("ProgramFiles"), "Mozilla Firefox", "firefox.exe")
	if _, err := os.Stat(sysFirefox); err != nil {
		t.Skip("firefox.exe not available for copy")
	}
	data, err := os.ReadFile(sysFirefox)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "firefox.exe"), data, 0644); err != nil {
		t.Fatal(err)
	}
	got := DetectInstalledVersion(dir, "15.0.19", &manifest.Manifest{
		Version: "15.0.19",
		Bin:     "firefox.exe",
	})
	if got.ExternallyUpdated {
		t.Fatalf("must not flag drift from incompatible engine FileVersion: %#v", got)
	}
}
