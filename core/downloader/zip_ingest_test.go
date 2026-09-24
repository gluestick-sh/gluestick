package downloader

import (
	"archive/zip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestIngestZipFile_fromDisk(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zip-ingest-disk-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "big.zip")
	if err := writeZipWithPayload(zipPath, 4<<20); err != nil {
		t.Fatal(err)
	}

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	f, err := os.Open(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(store)
	sum, files, err := d.ingestZipFile(context.Background(), f, info.Size(), "", "", zipPath, "", false, false)
	_ = f.Close()
	if err != nil {
		t.Fatalf("ingestZipFile: %v", err)
	}
	if sum == "" || len(files) == 0 {
		t.Fatalf("expected zip hash and members, got hash=%q files=%d", sum, len(files))
	}
	if _, err := os.Stat(store.ObjectPath(sum)); err != nil {
		t.Fatalf("zip blob not in cache store: %v", err)
	}
}

func TestIngestZipSpooled_reader(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zip-ingest-spool-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "payload.zip")
	if err := writeZipWithPayload(zipPath, 2<<20); err != nil {
		t.Fatal(err)
	}

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	src, err := os.Open(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	d := NewDownloader(store)
	sum, files, err := d.ingestZipSpooled(context.Background(), src, "", "")
	if err != nil {
		t.Fatalf("ingestZipSpooled: %v", err)
	}
	if sum == "" || len(files) == 0 {
		t.Fatalf("expected zip hash and members, got hash=%q files=%d", sum, len(files))
	}
}

func TestIngestZipSpooled_SHA512(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zip-ingest-sha512-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "payload.zip")
	if err := writeZipWithPayload(zipPath, 1<<20); err != nil {
		t.Fatal(err)
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha512.Sum512(zipBytes)
	hashValue := hex.EncodeToString(digest[:])

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	src, err := os.Open(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	d := NewDownloader(store)
	sum, files, err := d.ingestZipSpooled(context.Background(), src, "sha512", hashValue)
	if err != nil {
		t.Fatalf("ingestZipSpooled: %v", err)
	}
	if sum == "" || len(files) == 0 {
		t.Fatalf("expected zip hash and members, got hash=%q files=%d", sum, len(files))
	}
	task := Task{HashAlgo: "sha512", HashValue: hashValue}
	if err := VerifyArchiveObject(store, task, sum); err != nil {
		t.Fatalf("VerifyArchiveObject: %v", err)
	}
}

func TestIngestZipSkipsDirectoryPlaceholders(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zip-ingest-dir-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "dotnet.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, name := range []string{"runtimes\\win-x64\\", "AssetStudioGUI.exe"} {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".exe") {
			if _, err := entry.Write([]byte("MZ")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	zf, err := os.Open(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := zf.Stat()
	if err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(store)
	_, files, err := d.ingestZipFile(context.Background(), zf, info.Size(), "", "", zipPath, "", false, false)
	_ = zf.Close()
	if err != nil {
		t.Fatalf("ingestZipFile: %v", err)
	}
	if _, ok := files["runtimes/win-x64/"]; ok {
		t.Fatal("directory placeholder should be skipped")
	}
	if _, ok := files["AssetStudioGUI.exe"]; !ok {
		t.Fatal("expected AssetStudioGUI.exe in ingest map")
	}
}

func writeZipWithPayload(path string, payloadSize int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := zip.NewWriter(f)
	entry, err := w.Create("data.bin")
	if err != nil {
		return err
	}
	if _, err := io.CopyN(entry, io.LimitReader(zeroReader{}, int64(payloadSize)), int64(payloadSize)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return f.Close()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
