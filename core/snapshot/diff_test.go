package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffInstallMissing(t *testing.T) {
	workers := 4
	current := CoreState{
		Packages: []Package{{Name: "git", Bucket: "main", Version: "2.0"}},
		Buckets:  []Bucket{{Name: "main", URL: "https://example/main"}},
		Config:   Config{GitHubProxy: ""},
	}
	target := CoreState{
		Packages: []Package{
			{Name: "git", Bucket: "main", Version: "2.0"},
			{Name: "nodejs", Bucket: "main", Version: "22.0.0", VersionLocked: true},
		},
		Buckets: []Bucket{
			{Name: "main", URL: "https://example/main"},
			{Name: "extras", URL: "https://example/extras"},
		},
		Config: Config{GitHubProxy: "https://mirror/", DownloadWorkers: &workers, BucketSyncMode: "auto"},
	}

	plan, err := Diff(current, target, ApplyOptions{Mode: ApplyModeInstallMissing})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.BucketsToAdd) != 1 || plan.BucketsToAdd[0].Name != "extras" {
		t.Fatalf("bucketsToAdd = %#v", plan.BucketsToAdd)
	}
	if len(plan.PackagesToInstall) != 1 || plan.PackagesToInstall[0].Name != "nodejs" {
		t.Fatalf("packagesToInstall = %#v", plan.PackagesToInstall)
	}
	if len(plan.PackagesToRemove) != 0 || len(plan.BucketsToRemove) != 0 {
		t.Fatalf("unexpected removals: %#v", plan)
	}
	if len(plan.ConfigChanges) < 2 {
		t.Fatalf("configChanges = %#v", plan.ConfigChanges)
	}
}

func TestDiffInstallsMissingVersion(t *testing.T) {
	current := CoreState{Packages: []Package{{Name: "vivaldi", Version: "7.0", Current: true}}}
	target := CoreState{Packages: []Package{
		{Name: "vivaldi", Version: "6.0"},
		{Name: "vivaldi", Version: "7.0", Current: true},
	}}
	plan, err := Diff(current, target, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PackagesToInstall) != 1 || plan.PackagesToInstall[0].Version != "6.0" {
		t.Fatalf("packagesToInstall = %#v", plan.PackagesToInstall)
	}
	if len(plan.PackagesToActivate) != 0 {
		t.Fatalf("packagesToActivate = %#v", plan.PackagesToActivate)
	}
}

func TestDiffActivatesCurrentVersion(t *testing.T) {
	current := CoreState{Packages: []Package{
		{Name: "vivaldi", Version: "6.0", Current: true},
		{Name: "vivaldi", Version: "7.0"},
	}}
	target := CoreState{Packages: []Package{
		{Name: "vivaldi", Version: "6.0"},
		{Name: "vivaldi", Version: "7.0", Current: true},
	}}
	plan, err := Diff(current, target, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PackagesToInstall) != 0 {
		t.Fatalf("packagesToInstall = %#v", plan.PackagesToInstall)
	}
	if len(plan.PackagesToActivate) != 1 || plan.PackagesToActivate[0].Version != "7.0" {
		t.Fatalf("packagesToActivate = %#v", plan.PackagesToActivate)
	}
}

func TestDiffLegacyNameOnlySkipsWhenAnyVersionPresent(t *testing.T) {
	current := CoreState{Packages: []Package{{Name: "git", Version: "1.0"}}}
	target := CoreState{Packages: []Package{{Name: "git"}}}
	plan, err := Diff(current, target, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PackagesToInstall) != 0 {
		t.Fatalf("expected no install, got %#v", plan.PackagesToInstall)
	}
}

func TestDiffReconcileRemovesExtras(t *testing.T) {
	current := CoreState{
		Packages: []Package{{Name: "git"}, {Name: "orphaned"}},
		Buckets:  []Bucket{{Name: "main"}, {Name: "old"}},
	}
	target := CoreState{
		Packages: []Package{{Name: "git"}},
		Buckets:  []Bucket{{Name: "main"}},
	}
	plan, err := Diff(current, target, ApplyOptions{Mode: ApplyModeReconcile})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.PackagesToRemove) != 1 || plan.PackagesToRemove[0] != "orphaned" {
		t.Fatalf("packagesToRemove = %#v", plan.PackagesToRemove)
	}
	if len(plan.BucketsToRemove) != 1 || plan.BucketsToRemove[0] != "old" {
		t.Fatalf("bucketsToRemove = %#v", plan.BucketsToRemove)
	}
}

func TestInstallRef(t *testing.T) {
	tests := []struct {
		pkg  Package
		want string
	}{
		{Package{Name: "git"}, "git"},
		{Package{Name: "git", Bucket: "main"}, "git"},
		{Package{Name: "nodejs", Bucket: "extras", Version: "22.0"}, "extras/nodejs@22.0"},
		{Package{Name: "git", Version: "2.40"}, "git@2.40"},
	}
	for _, tc := range tests {
		if got := InstallRef(tc.pkg); got != tc.want {
			t.Fatalf("InstallRef(%#v) = %q, want %q", tc.pkg, got, tc.want)
		}
	}
}

func TestNewIDFormat(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(id, "snap_") {
		t.Fatalf("id still has snap_ prefix: %q", id)
	}
	if len(id) != 32 {
		t.Fatalf("id length = %d, want 32: %q", len(id), id)
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("id is not lowercase hex: %q", id)
		}
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.json")
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	src := &Snapshot{
		SchemaVersion: SchemaVersion,
		Kind:          Kind,
		ID:            id,
		CreatedAt:     NowRFC3339(),
		Source:        SourceManual,
		Device: Device{
			DeviceID: "aabbccddeeff00112233445566778899",
			Hostname: "HOST",
			OS:       "windows",
			Arch:     "arm64",
		},
		Core: CoreState{
			Packages: []Package{{Name: "git", Bucket: "main", Version: "1"}},
			Buckets:  []Bucket{{Name: "main", URL: "https://example"}},
			Config:   Config{GitHubProxy: "https://mirror/"},
		},
	}
	if err := WriteFile(path, src); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Device.DeviceID != src.Device.DeviceID {
		t.Fatalf("deviceId = %q", got.Device.DeviceID)
	}
	if len(got.Core.Packages) != 1 || got.Core.Packages[0].Name != "git" {
		t.Fatalf("packages = %#v", got.Core.Packages)
	}
}

func TestValidateRejectsBadKind(t *testing.T) {
	err := Validate(&Snapshot{
		Kind:   "other",
		Device: Device{DeviceID: "aabbccddeeff00112233445566778899"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("err = %v, want ErrInvalidFormat", err)
	}
}

func TestReadFileRejectsArbitraryJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wails.json")
	if err := os.WriteFile(path, []byte(`{"name":"desktop","outputfilename":"app"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFile(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("err = %v, want ErrInvalidFormat", err)
	}
	if !strings.Contains(err.Error(), ErrInvalidFormat.Error()) {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveBucketURL(t *testing.T) {
	known := func(name string) (string, bool) {
		if name == "extras" {
			return "https://known/extras", true
		}
		return "", false
	}
	u, err := ResolveBucketURL("extras", "", known)
	if err != nil || u != "https://known/extras" {
		t.Fatalf("got %q %v", u, err)
	}
	u, err = ResolveBucketURL("custom", "https://custom", known)
	if err != nil || u != "https://custom" {
		t.Fatalf("got %q %v", u, err)
	}
	if _, err := ResolveBucketURL("missing", "", known); err == nil {
		t.Fatal("expected error")
	}
}

func TestReadMissingFile(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "nope.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v", err)
	}
}
