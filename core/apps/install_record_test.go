package apps

import (
	"strings"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestInstallRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := manifest.Parse(strings.NewReader(`{
		"version": "2.0.1",
		"url": "https://example.com/app.zip",
		"hash": "deadbeef"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveInstallRecord(dir, "extras", m); err != nil {
		t.Fatalf("SaveInstallRecord: %v", err)
	}

	rec, err := LoadInstallRecord(dir)
	if err != nil {
		t.Fatalf("LoadInstallRecord: %v", err)
	}
	if rec.Version != "2.0.1" {
		t.Fatalf("version = %q", rec.Version)
	}
	if rec.Bucket != "extras" {
		t.Fatalf("bucket = %q", rec.Bucket)
	}
	if rec.Manifest == nil || rec.Manifest.Version != "2.0.1" {
		t.Fatalf("manifest = %#v", rec.Manifest)
	}
	if rec.Manifest.GetURL() != "https://example.com/app.zip" {
		t.Fatalf("url = %q", rec.Manifest.GetURL())
	}
}

func TestSaveInstallRecord_nilManifest(t *testing.T) {
	err := SaveInstallRecord(t.TempDir(), "main", nil)
	if err == nil || !strings.Contains(err.Error(), "manifest is nil") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInstallRecord_missingFile(t *testing.T) {
	_, err := LoadInstallRecord(t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing install.json")
	}
}
