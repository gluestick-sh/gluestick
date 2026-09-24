package manifest

import "testing"

func TestParseURLFreedroidRPG(t *testing.T) {
	raw := "https://ftp.osuosl.org/pub/freedroid/freedroidRPG-1.0/freedroidRPG-1.0-win32-x86-64.exe#dl.zip"
	p, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantFetch := "https://ftp.osuosl.org/pub/freedroid/freedroidRPG-1.0/freedroidRPG-1.0-win32-x86-64.exe"
	if p.FetchURL != wantFetch {
		t.Fatalf("FetchURL = %q, want %q", p.FetchURL, wantFetch)
	}
	if p.LocalName != "dl.zip" {
		t.Fatalf("LocalName = %q, want dl.zip", p.LocalName)
	}
	if p.Extension != ".zip" {
		t.Fatalf("Extension = %q, want .zip", p.Extension)
	}
	if ShouldNativeZipIngest(p.LocalName, p.FetchURL) {
		t.Fatal("freedroid dl.zip alias must not use native zip ingest")
	}
}

func TestParseURLScoopFragment(t *testing.T) {
	p, err := ParseURL("https://example.org/program.exe#/dl.7z")
	if err != nil {
		t.Fatal(err)
	}
	if p.FetchURL != "https://example.org/program.exe" {
		t.Fatalf("FetchURL = %q", p.FetchURL)
	}
	if p.LocalName != "dl.7z" || p.Extension != ".7z" {
		t.Fatalf("got name=%q ext=%q", p.LocalName, p.Extension)
	}
	if !IsScoopArchiveAlias("dl.7z") {
		t.Fatal("dl.7z should be scoop alias")
	}
}

func TestParseURLPlainZip(t *testing.T) {
	p, err := ParseURL("https://example.org/app-1.0.zip")
	if err != nil {
		t.Fatal(err)
	}
	if p.LocalName != "app-1.0.zip" || p.Extension != ".zip" {
		t.Fatalf("got name=%q ext=%q", p.LocalName, p.Extension)
	}
	if !ShouldNativeZipIngest(p.LocalName, p.FetchURL) {
		t.Fatal("real zip should use native zip ingest")
	}
}

func TestParseURLLibreOfficeMsi(t *testing.T) {
	raw := "https://download.documentfoundation.org/libreoffice/stable/26.2.4/win/x86_64/LibreOffice_26.2.4_Win_x86-64.msi#/dl.msi_"
	p, err := ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.LocalName != "dl.msi_" {
		t.Fatalf("LocalName = %q, want dl.msi_", p.LocalName)
	}
	if p.Extension != ".msi_" {
		t.Fatalf("Extension = %q, want .msi_", p.Extension)
	}
	if !IsScoopMsiHookInstall(p.LocalName, p.Extension) {
		t.Fatal("expected MSI hook install")
	}
}

func TestParseURLRenameExe(t *testing.T) {
	p, err := ParseURL("https://example.org/v1.0#/pngcrush.exe")
	if err != nil {
		t.Fatal(err)
	}
	if p.LocalName != "pngcrush.exe" || p.Extension != ".exe" {
		t.Fatalf("got name=%q ext=%q", p.LocalName, p.Extension)
	}
}
