package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseBasicManifest(t *testing.T) {
	json := `{
		"version": "1.0.0",
		"description": "Test package",
		"homepage": "https://example.com",
		"license": "MIT",
		"url": "https://example.com/download/test-1.0.0.zip",
		"hash": "abc123",
		"extractor": "zip"
	}`

	m, err := Parse(strings.NewReader(json))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if m.Version != "1.0.0" {
		t.Errorf("version = %s, want 1.0.0", m.Version)
	}

	if m.GetURL() != "https://example.com/download/test-1.0.0.zip" {
		t.Errorf("url = %s", m.GetURL())
	}

	if m.GetHash() != "abc123" {
		t.Errorf("hash = %s, want abc123", m.GetHash())
	}
}

func TestParseManifestWithArrayURL(t *testing.T) {
	json := `{
		"version": "1.0.0",
		"url": [
			"https://example.com/test-1.0.0-x64.zip",
			"https://example.com/test-1.0.0-arm64.zip"
		],
		"hash": [
			"abc123",
			"def456"
		]
	}`

	m, err := Parse(strings.NewReader(json))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// Should return first URL
	if m.GetURL() != "https://example.com/test-1.0.0-x64.zip" {
		t.Errorf("url = %s", m.GetURL())
	}

	if m.GetHash() != "abc123" {
		t.Errorf("hash = %s, want abc123", m.GetHash())
	}
}

func TestEnvAddPaths(t *testing.T) {
	tests := []struct {
		name string
		json string
		want []string
	}{
		{
			name: "string",
			json: `{"version":"1.0.0","url":"https://example.com/x.zip","hash":"abc","env_add_path":"cmd"}`,
			want: []string{"cmd"},
		},
		{
			name: "array",
			json: `{"version":"1.0.0","url":"https://example.com/x.zip","hash":"abc","env_add_path":["bin","usr\\bin"]}`,
			want: []string{"bin", "usr\\bin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Parse(strings.NewReader(tt.json))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := m.EnvAddPaths()
			if len(got) != len(tt.want) {
				t.Fatalf("EnvAddPaths() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("EnvAddPaths()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseManifestMissingRequired(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantErr string
	}{
		{
			name:    "missing version",
			json:    `{"url": "https://example.com/test.zip", "hash": "abc123"}`,
			wantErr: "version",
		},
		{
			name:    "missing url",
			json:    `{"version": "1.0.0", "hash": "abc123"}`,
			wantErr: "url",
		},
		{
			name:    "missing hash",
			json:    `{"version": "1.0.0", "url": "https://example.com/test.zip"}`,
			wantErr: "hash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.json))
			if err == nil {
				t.Error("expected error, got nil")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error should mention %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestBucketManagerGetManifest(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bucket-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	bm, err := NewBucketManager(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create a bucket with a manifest
	bucketDir := filepath.Join(tmpDir, "buckets", "main")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}

	manifestJSON := `{
		"version": "1.0.0",
		"description": "Test package",
		"url": "https://example.com/test.zip",
		"hash": "abc123"
	}`

	if err := os.WriteFile(filepath.Join(bucketDir, "test.json"), []byte(manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Add bucket
	if err := bm.AddBucket("main", ""); err != nil {
		t.Fatal(err)
	}

	// Get manifest
	path, m, err := bm.GetManifestPath("test")
	if err != nil {
		t.Fatalf("GetManifestPath failed: %v", err)
	}

	if m.Version != "1.0.0" {
		t.Errorf("version = %s, want 1.0.0", m.Version)
	}

	if filepath.Base(path) != "test.json" {
		t.Errorf("path = %s", path)
	}

	path, m, err = bm.GetManifestPath("test@2.0.0")
	if err != nil {
		t.Fatalf("GetManifestPath with @version failed: %v", err)
	}
	if m.Version != "1.0.0" {
		t.Errorf("version = %s, want 1.0.0", m.Version)
	}
}

func TestBucketManagerGetManifest_deprecated(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bucket-deprecated-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	bm, err := NewBucketManager(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	bucketDir := filepath.Join(tmpDir, "buckets", "lemon")
	deprecatedDir := filepath.Join(bucketDir, "deprecated")
	if err := os.MkdirAll(deprecatedDir, 0755); err != nil {
		t.Fatal(err)
	}

	manifestJSON := `{
		"version": "3.5.1",
		"description": "1key.run",
		"url": "https://example.com/1key.run.exe",
		"hash": "abc123"
	}`
	if err := os.WriteFile(filepath.Join(deprecatedDir, "1key.run.json"), []byte(manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := bm.AddBucket("lemon", ""); err != nil {
		t.Fatal(err)
	}

	path, m, err := bm.GetManifestPath("lemon/1key.run")
	if err != nil {
		t.Fatalf("GetManifestPath(lemon/1key.run) failed: %v", err)
	}
	if m.Version != "3.5.1" {
		t.Fatalf("version = %s", m.Version)
	}
	if !strings.HasSuffix(filepath.ToSlash(path), "deprecated/1key.run.json") {
		t.Fatalf("path = %s", path)
	}
}

func TestBucketManagerFindManifest_deprecated(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bucket-find-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	bm, err := NewBucketManager(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	bucketDir := filepath.Join(tmpDir, "buckets", "lemon")
	deprecatedDir := filepath.Join(bucketDir, "deprecated")
	if err := os.MkdirAll(deprecatedDir, 0755); err != nil {
		t.Fatal(err)
	}

	manifestJSON := `{
		"version": "3.5.1",
		"description": "1key.run",
		"url": "https://example.com/1key.run.exe",
		"hash": "abc123"
	}`
	if err := os.WriteFile(filepath.Join(deprecatedDir, "1key.run.json"), []byte(manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}
	if err := bm.AddBucket("lemon", ""); err != nil {
		t.Fatal(err)
	}

	bucketName, m := bm.FindManifest("1key.run")
	if m == nil {
		t.Fatal("FindManifest(1key.run) returned nil")
	}
	if bucketName != "lemon" {
		t.Fatalf("bucket = %q, want lemon", bucketName)
	}
	if m.Version != "3.5.1" {
		t.Fatalf("version = %s", m.Version)
	}
}

func TestIsDeprecatedLocation(t *testing.T) {
	root := filepath.Join("C:", "buckets", "lemon")
	deprecatedPath := filepath.Join(root, "deprecated", "1key.run.json")
	if !IsDeprecatedManifestPath(root, deprecatedPath) {
		t.Fatal("expected deprecated dir path")
	}
	activePath := filepath.Join(root, "bucket", "1key.run.json")
	if IsDeprecatedManifestPath(root, activePath) {
		t.Fatal("bucket path should not be deprecated")
	}
	m, err := Parse(strings.NewReader(`{
		"version": "1.0.0",
		"url": "https://example.com/x.zip",
		"hash": "abc",
		"deprecated": "use other-app instead"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !IsDeprecatedLocation(root, activePath, m) {
		t.Fatal("expected JSON-marked deprecated")
	}
}

func TestBinariesAlias(t *testing.T) {
	json := `{
		"version": "1.0.0",
		"url": "https://example.com/x.zip",
		"hash": "abc",
		"bin": [
			"plain.exe",
			["nested.exe", "nested-alias"],
			{"file": "map.exe", "alias": "map-alias"},
			{"name": "shim.exe", "shim": "shim-cmd"}
		]
	}`

	m, err := Parse(strings.NewReader(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := m.Binaries()
	want := []string{
		"plain.exe",
		"[nested.exe,nested-alias]",
		"[map.exe,map-alias]",
		"[shim.exe,shim-cmd]",
	}
	if len(got) != len(want) {
		t.Fatalf("Binaries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Binaries()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseInnoSetupFlag(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "4.3.3",
		"innosetup": true,
		"architecture": {
			"64bit": {
				"url": "https://example.com/setup.exe",
				"hash": "md5:abc"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !m.InnoSetup {
		t.Fatal("expected InnoSetup = true")
	}
}

func TestBinariesArchitectureBlock(t *testing.T) {
	json := `{
		"version": "4.3.3",
		"url": null,
		"hash": null,
		"bin": null,
		"architecture": {
			"64bit": {
				"url": "https://example.com/R-4.3.3-win.exe",
				"hash": "md5:abc",
				"bin": [
					"bin\\x64\\R.exe",
					"bin\\x64\\Rcmd.exe",
					["bin\\x64\\Rgui.exe", "R"]
				]
			}
		}
	}`

	m, err := Parse(strings.NewReader(json))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := m.Binaries()
	want := []string{
		"bin\\x64\\R.exe",
		"bin\\x64\\Rcmd.exe",
		"[bin\\x64\\Rgui.exe,R]",
	}
	if len(got) != len(want) {
		t.Fatalf("Binaries() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Binaries()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLaunchBinariesArchitectureWithoutURL(t *testing.T) {
	json := `{
		"version": "2026.1",
		"architecture": {
			"64bit": {
				"bin": [["IDE\\bin\\pycharm64.exe", "pycharm"]]
			}
		}
	}`
	m, err := Parse(strings.NewReader(json))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasLaunchDefinitions() {
		t.Fatal("expected HasLaunchDefinitions true")
	}
	got := m.LaunchBinaries()
	if len(got) != 1 || got[0] != "[IDE\\bin\\pycharm64.exe,pycharm]" {
		t.Fatalf("LaunchBinaries() = %v", got)
	}
	if len(m.Binaries()) != 0 {
		t.Fatalf("Binaries() = %v, want empty without arch URL", m.Binaries())
	}
}

func TestParseFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "manifest-file-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	manifestPath := filepath.Join(tmpDir, "test.json")
	manifestJSON := `{
		"version": "1.0.0",
		"description": "Test",
		"url": "https://example.com/test.zip",
		"hash": "abc123"
	}`

	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := ParseFile(manifestPath)
	if err != nil {
		t.Fatalf("ParseFile failed: %v", err)
	}

	if m.Version != "1.0.0" {
		t.Errorf("version = %s, want 1.0.0", m.Version)
	}
}

func TestParseManifestNotesStringOrArray(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		m, err := Parse(strings.NewReader(`{"version":"1","url":"https://example.com/x.zip","hash":"abc","notes":"run install.reg"}`))
		if err != nil {
			t.Fatal(err)
		}
		got := m.GetNotes()
		if len(got) != 1 || got[0] != "run install.reg" {
			t.Fatalf("GetNotes() = %v", got)
		}
	})

	t.Run("array", func(t *testing.T) {
		m, err := Parse(strings.NewReader(`{"version":"1","url":"https://example.com/x.zip","hash":"abc","notes":["a","b"]}`))
		if err != nil {
			t.Fatal(err)
		}
		got := m.GetNotes()
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("GetNotes() = %v", got)
		}
	})
}

func TestSuggestions(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "1.0.0",
		"url": "https://example.com/x.zip",
		"hash": "abc",
		"suggest": {
			"Java Runtime Environment": "java/temurin-jre"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got := m.Suggestions()
	if len(got) != 1 || got[0].Label != "Java Runtime Environment" || got[0].Ref != "java/temurin-jre" {
		t.Fatalf("Suggestions() = %#v", got)
	}
}

// TestParse_vimBucketSnippet uses a realistic Scoop vim manifest (notes + architecture url + suggest + bin).
func TestParse_vimBucketSnippet(t *testing.T) {
	json := `{
    "version": "9.2.0545",
    "description": "A highly configurable text editor",
    "homepage": "https://www.vim.org",
    "license": "Vim",
    "notes": "Add gVim as a context menu option by running: \"$dir\\install-context.reg\"",
    "suggest": { "vimtutor": "vimtutor" },
    "architecture": {
        "64bit": {
            "url": "https://example.com/gvim.zip",
            "hash": "45ce96a13044a82d2d698648fb1ea6c266f69f8f0b9add5d63b0b06b3bd49188"
        }
    },
    "extract_dir": "vim/vim92",
    "bin": ["vim.exe", "gvim.exe"]
}`
	m, err := Parse(strings.NewReader(json))
	if err != nil {
		t.Fatal(err)
	}
	notes := m.GetNotes()
	if len(notes) != 1 || notes[0] == "" {
		t.Fatalf("GetNotes() = %v", notes)
	}
	if m.GetURL() != "https://example.com/gvim.zip" {
		t.Fatalf("GetURL() = %q", m.GetURL())
	}
	suggest := m.Suggestions()
	if len(suggest) != 1 || suggest[0].Label != "vimtutor" || suggest[0].Ref != "vimtutor" {
		t.Fatalf("Suggestions() = %#v", suggest)
	}
	bins := m.Binaries()
	if len(bins) != 2 || bins[0] != "vim.exe" || bins[1] != "gvim.exe" {
		t.Fatalf("Binaries() = %v", bins)
	}
}

func TestPreInstallHooksForInstall_archBlock(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "26.01",
		"architecture": {
			"arm64": {
				"url": "https://example.com/7z-arm64.exe",
				"pre_install": [
					"Invoke-ExternalCommand $7zr @('x', \"$dir\\$fname\", \"-o$dir\", '-y')"
				]
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	hooks := m.PreInstallHooksForInstall("arm64")
	if len(hooks) != 1 || !strings.Contains(hooks[0], "Invoke-ExternalCommand") {
		t.Fatalf("PreInstallHooksForInstall(arm64) = %#v", hooks)
	}
	if len(m.PreInstallHooksForInstall("64bit")) != 0 {
		t.Fatal("expected no pre_install for 64bit")
	}
}

func TestPreUninstallHooksForInstall(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "1.98.4",
		"pre_uninstall": [
			"Stop-Service -Name 'Tailscale' -Force -ErrorAction SilentlyContinue",
			"tailscaled.exe uninstall-system-daemon"
		],
		"architecture": {
			"arm64": {
				"url": "https://example.com/tailscale-arm64.msi",
				"pre_uninstall": "Stop-Process -Name tailscale-ipn -Force"
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	root := m.PreUninstallHooksForInstall("64bit")
	if len(root) != 2 {
		t.Fatalf("PreUninstallHooksForInstall(64bit) = %#v", root)
	}
	arm := m.PreUninstallHooksForInstall("arm64")
	if len(arm) != 1 || !strings.Contains(arm[0], "tailscale-ipn") {
		t.Fatalf("PreUninstallHooksForInstall(arm64) = %#v", arm)
	}
}

func TestParseManifestPostInstallString(t *testing.T) {
	m, err := Parse(strings.NewReader(`{
		"version": "8.3.29",
		"homepage": "https://windows.php.net/",
		"architecture": {
			"64bit": {
				"url": "https://example.com/php.zip",
				"hash": "abc123"
			}
		},
		"post_install": "Write-Host 'done'"
	}`))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	hooks := m.PostInstallHooks()
	if len(hooks) != 1 || hooks[0] != "Write-Host 'done'" {
		t.Fatalf("PostInstallHooks() = %#v", hooks)
	}
}
