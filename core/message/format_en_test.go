package message

import (
	"strings"
	"testing"
)

// knownKeys must always resolve to human-readable English (not the raw key).
var knownKeys = []string{
	ProgressInstallStarting,
	ProgressInstallComplete,
	ProgressInstallCancelled,
	ProgressUninstallStarting,
	ProgressUninstallComplete,
	ProgressResolvingManifest,
	ProgressPreparingDownload,
	ProgressDownloading,
	ProgressDownloadCached,
	ProgressLinkingFiles,
	ProgressCreatingShims,
	ProgressUpdatingCache,
	ProgressExtracting,
	ProgressPackageInstallComplete,
	GCPrepareStore,
	GCReadingIndexRefs,
	BucketCloning,
	BucketUpdateComplete,
	BucketNoUpdates,
	GitPulling,
	DoctorGitMissing,
	DoctorHintGitInstall,
	ErrLaunchNotOpenable,
}

func TestFormatENKnownKeys(t *testing.T) {
	for _, key := range knownKeys {
		got := FormatEN(key, nil)
		if got == "" || got == key {
			t.Fatalf("FormatEN(%q) = %q, want readable English", key, got)
		}
		if strings.HasPrefix(got, "progress.") || strings.HasPrefix(got, "doctor.") || strings.HasPrefix(got, "error.") {
			t.Fatalf("FormatEN(%q) returned untranslated key: %q", key, got)
		}
	}
}

func TestFormatENWithArgs(t *testing.T) {
	got := FormatEN(ProgressPackageInstallComplete, map[string]any{"package": "git"})
	if !strings.Contains(got, "git") {
		t.Fatalf("expected package name in message, got %q", got)
	}
	got = FormatEN(ProgressDownloading, map[string]interface{}{"file": "node-v20.zip"})
	if !strings.Contains(got, "node-v20.zip") {
		t.Fatalf("expected filename in download message, got %q", got)
	}
}
