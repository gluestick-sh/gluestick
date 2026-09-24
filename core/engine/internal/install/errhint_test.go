package install

import (
	"errors"
	"strings"
	"testing"
)

func TestDownloadPhaseErrorNotFoundHint(t *testing.T) {
	err := downloadPhaseError(1, errors.New("HTTP 404: 404 Not Found (https://example.com/missing.exe)"))
	msg := err.Error()
	if !strings.Contains(msg, "source-only") && !strings.Contains(msg, "missing this architecture") {
		t.Fatalf("404 hint missing: %s", msg)
	}
	if strings.Contains(msg, "GitHub mirror") {
		t.Fatalf("404 should not suggest GitHub mirror: %s", msg)
	}
}

func TestIsHTTPNotFound(t *testing.T) {
	if !isHTTPNotFound(errors.New("download failed: all download URLs failed, last error: HTTP 404: 404 Not Found (https://www.python.org/ftp/python/3.10.20/python-3.10.20-arm64.exe)")) {
		t.Fatal("expected 404 detection")
	}
	if isHTTPNotFound(errors.New("HTTP 500: Internal Server Error")) {
		t.Fatal("500 is not 404")
	}
}
