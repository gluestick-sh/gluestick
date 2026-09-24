package install

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/manifest"
)

func TestAssetStudioZipInstallLayout(t *testing.T) {
	zipPath := filepath.Join(os.TempDir(), "assetstudio.zip")
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Skip("place AssetStudio zip at TEMP/assetstudio.zip to run this test")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.Itoa(len(zipBytes)))
		_, _ = w.Write(zipBytes)
	}))
	defer ts.Close()

	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	dl := downloader.NewDownloader(store)
	result := dl.DownloadWithCache(context.Background(), downloader.Task{
		URL:       ts.URL + "/AssetStudio.net6.0-windows_v0.16.53.zip",
		Filename:  "AssetStudio.net6.0-windows_v0.16.53.zip",
		HashAlgo:  "sha256",
		HashValue: "0922b22b62853cd77e7b796124d52373fc1e1257179a1e3f3d7258137723616b",
	}, "0922b22b62853cd77e7b796124d52373fc1e1257179a1e3f3d7258137723616b", false)
	if result.Error != nil {
		t.Fatalf("download: %v", result.Error)
	}
	if _, ok := result.Files["AssetStudioGUI.exe"]; !ok {
		t.Fatalf("AssetStudioGUI.exe missing from ingest; sample: %v", sampleMapKeys(result.Files, 8))
	}

	installDir := filepath.Join(root, "apps", "assetstudio", "0.16.53")
	linked, err := LinkExtractedFiles(store, installDir, "", "", result.Files, nil)
	if err != nil {
		t.Fatalf("LinkExtractedFiles: %v", err)
	}
	if linked == 0 {
		t.Fatal("linked 0 files")
	}

	m := &manifest.Manifest{Bin: "AssetStudioGUI.exe"}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}

func sampleMapKeys(m map[string]string, n int) []string {
	out := make([]string, 0, n)
	for k := range m {
		out = append(out, k)
		if len(out) >= n {
			break
		}
	}
	return out
}
