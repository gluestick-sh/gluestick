package extractor

import (
	"testing"

	"github.com/gluestick-sh/core/safepath"
)

func TestListExtractedFiles_rejectsZipSlip(t *testing.T) {
	if _, err := safepath.ValidateManifestRelPath("../outside.exe"); err == nil {
		t.Fatal("expected zip-slip path to be rejected")
	}
}
