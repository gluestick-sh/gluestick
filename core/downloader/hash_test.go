package downloader

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestVerifyManifestDigest(t *testing.T) {
	if err := verifyManifestDigest("sha256", "abc", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := verifyManifestDigest("sha256", "ABC", "abc"); err != nil {
		t.Fatal("expected case-insensitive match")
	}
	if err := verifyManifestDigest("sha256", "abc", "def"); err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestWrapManifestDigest_SHA1(t *testing.T) {
	data := []byte("hello glue")
	r, finish := wrapManifestDigest(bytes.NewReader(data), "sha1")
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	got, err := finish()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(data)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestManifestUsesCacheStoreHash(t *testing.T) {
	for _, algo := range []string{"", "sha256", "sha2", "SHA256"} {
		if !manifestUsesCacheStoreHash(algo) {
			t.Fatalf("algo %q should use cache store hash", algo)
		}
	}
	for _, algo := range []string{"sha1", "md5", "sha512"} {
		if manifestUsesCacheStoreHash(algo) {
			t.Fatalf("algo %q should not use cache store hash", algo)
		}
	}
}

func TestVerifyTaskDigest_CacheStoreHash(t *testing.T) {
	sum := sha256.Sum256([]byte("payload"))
	hexHash := hex.EncodeToString(sum[:])
	task := Task{HashAlgo: "sha256", HashValue: hexHash}
	if err := verifyTaskDigest(task, hexHash, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyTaskDigest(task, "deadbeef", nil); err == nil {
		t.Fatal("expected mismatch")
	}
}

func TestVerifyTaskDigest_SHA1Stream(t *testing.T) {
	data := []byte("manifest-bytes")
	r, finish := wrapManifestDigest(bytes.NewReader(data), "sha1")
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	digest, err := finish()
	if err != nil {
		t.Fatal(err)
	}
	task := Task{HashAlgo: "sha1", HashValue: digest}
	// finish already consumed; verify using cache store path is not applicable — re-wrap for verifyTaskDigest test
	r2, finish2 := wrapManifestDigest(bytes.NewReader(data), "sha1")
	if _, err := io.Copy(io.Discard, r2); err != nil {
		t.Fatal(err)
	}
	if err := verifyTaskDigest(task, "cas-hash-unrelated", finish2); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDownloadResult_SHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("payload"))
	hexHash := hex.EncodeToString(sum[:])
	task := Task{HashAlgo: "sha256", HashValue: hexHash}
	if err := VerifyDownloadResult(nil, task, Result{Hash: hexHash}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDownloadResult(nil, task, Result{Hash: "deadbeef"}); err == nil {
		t.Fatal("expected mismatch")
	}
}

func TestVerifyArchiveObject_SHA512(t *testing.T) {
	data := []byte("eclipse-java-payload")
	tmpDir, err := os.MkdirTemp("", "verify-sha512-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	casHash, err := store.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	digest := hex.EncodeToString(sum[:])
	task := Task{HashAlgo: "sha512", HashValue: digest}
	if err := VerifyArchiveObject(store, task, casHash); err != nil {
		t.Fatal(err)
	}
	task.HashValue = "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"
	if err := VerifyArchiveObject(store, task, casHash); err == nil {
		t.Fatal("expected mismatch")
	}
}

func TestWrapManifestDigest_SHA512(t *testing.T) {
	data := []byte("hello glue sha512")
	r, finish := wrapManifestDigest(bytes.NewReader(data), "sha512")
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	got, err := finish()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestVerifyArchiveObject_SHA1(t *testing.T) {
	data := []byte("manifest-bytes")
	tmpDir, err := os.MkdirTemp("", "verify-sha1-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	casHash, err := store.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(data)
	digest := hex.EncodeToString(sum[:])
	task := Task{HashAlgo: "sha1", HashValue: digest}
	if err := VerifyArchiveObject(store, task, casHash); err != nil {
		t.Fatal(err)
	}
	task.HashValue = "0000000000000000000000000000000000000000"
	if err := VerifyArchiveObject(store, task, casHash); err == nil {
		t.Fatal("expected mismatch")
	}
}
