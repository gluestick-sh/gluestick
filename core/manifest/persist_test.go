package manifest

import (
	"strings"
	"testing"
)

func TestPersistEntries_ultravnc(t *testing.T) {
	raw := `{
		"version": "1.6.4.0",
		"url": "https://example.com/ultravnc.zip",
		"hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"persist": ["UltraVnc.ini", "options.vnc"],
		"pre_install": [
			"if (!(Test-Path \"$persist_dir\\UltraVnc.ini\")) { New-Item \"$dir\\UltraVnc.ini\" | Out-Null }"
		]
	}`
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	entries := m.PersistEntries()
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].InstallName() != "UltraVnc.ini" || entries[1].InstallName() != "options.vnc" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestPersistEntry_LooksLikeFile(t *testing.T) {
	file := PersistEntry{Install: "nativeLang.xml", Data: "nativeLang.xml"}
	dir := PersistEntry{Install: "plugins", Data: "plugins"}
	profile := PersistEntry{Install: `TorBrowser\Data\Browser\profile.default`, Data: `TorBrowser\Data\Browser\profile.default`}
	if !file.LooksLikeFile() {
		t.Fatal("xml should look like file")
	}
	if dir.LooksLikeFile() {
		t.Fatal("plugins should look like directory")
	}
	if profile.LooksLikeFile() {
		t.Fatal("profile.default should look like directory")
	}
}

func TestPersistEntries_renamed(t *testing.T) {
	raw := `{"version":"1.0","url":"https://example.com/app.zip","hash":"sha256:0000000000000000000000000000000000000000000000000000000000000000","persist": [["config.ini", "app.ini"]]}`
	m, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	entries := m.PersistEntries()
	if len(entries) != 1 || entries[0].InstallName() != "config.ini" || entries[0].DataName() != "app.ini" {
		t.Fatalf("entries = %#v", entries)
	}
}
