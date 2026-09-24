package manifest

import (
	"strings"
	"testing"
)

func TestApplyDownloadOverride_rootURL(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "3.5.1",
		"url": "https://example.com/old.exe",
		"hash": "abc"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyDownloadOverride(m, "", []string{"https://example.com/new.exe#/dl.7z"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.GetURL(); got != "https://example.com/new.exe#/dl.7z" {
		t.Fatalf("url = %q", got)
	}
	if m.GetURL() != "https://example.com/old.exe" {
		t.Fatal("original manifest should be unchanged")
	}
}

func TestApplyDownloadOverride_archBlock(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "1.0",
		"architecture": {
			"arm64": {
				"url": "https://example.com/old-arm64.msi",
				"hash": "h1"
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyDownloadOverride(m, "arm64", []string{"https://example.com/new-arm64.msi"}, []string{"h2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.GetURLsForInstall("arm64"); len(got) != 1 || got[0] != "https://example.com/new-arm64.msi" {
		t.Fatalf("urls = %#v", got)
	}
	if got := out.GetHashesForInstall("arm64"); len(got) != 1 || got[0] != "h2" {
		t.Fatalf("hashes = %#v", got)
	}
}
