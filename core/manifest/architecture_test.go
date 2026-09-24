package manifest

import "testing"

func TestArchitectureCandidatesARM64Win11(t *testing.T) {
	got := architectureCandidates(ArchARM64, 22000)
	want := []string{ArchARM64, Arch64bit, Arch32bit}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestSelectedArchitecture(t *testing.T) {
	m := &Manifest{
		Version: "1.0.0",
		Architecture: map[string]interface{}{
			Arch64bit: map[string]interface{}{
				"url":  "https://example.com/x64.zip",
				"hash": "h64",
			},
			ArchARM64: map[string]interface{}{
				"url":  "https://example.com/arm64.zip",
				"hash": "harm",
			},
			Arch32bit: map[string]interface{}{
				"url":  "https://example.com/x86.zip",
				"hash": "h32",
			},
		},
	}

	tests := []struct {
		host    string
		build   int
		want    string
		wantURL string
	}{
		{ArchARM64, 22000, ArchARM64, "https://example.com/arm64.zip"},
		{ArchARM64, 19041, ArchARM64, "https://example.com/arm64.zip"},
		{Arch64bit, 22000, Arch64bit, "https://example.com/x64.zip"},
		{Arch32bit, 0, Arch32bit, "https://example.com/x86.zip"},
	}

	for _, tt := range tests {
		arch := m.selectArchitectureForHost(tt.host, tt.build)
		if arch != tt.want {
			t.Fatalf("host=%s build=%d arch=%q want %q", tt.host, tt.build, arch, tt.want)
		}
		urls := m.urlsForArchitecture(tt.want)
		if len(urls) != 1 || urls[0] != tt.wantURL {
			t.Fatalf("host=%s urls=%v want %q", tt.host, urls, tt.wantURL)
		}
	}
}

func TestArchitectureForInstall(t *testing.T) {
	m := &Manifest{
		Architecture: map[string]interface{}{
			Arch64bit: map[string]interface{}{
				"url": "https://example.com/x64.exe",
			},
			ArchARM64: map[string]interface{}{
				"url": "https://example.com/arm64.exe",
			},
		},
	}
	if got := m.ArchitectureForInstall(Arch64bit); got != Arch64bit {
		t.Fatalf("override 64bit = %q", got)
	}
	if urls := m.GetURLsForInstall(Arch64bit); len(urls) != 1 || urls[0] != "https://example.com/x64.exe" {
		t.Fatalf("urls = %v", urls)
	}
	if got := m.AvailableArchitectures(); len(got) != 2 {
		t.Fatalf("available = %v", got)
	}
}

func TestSelectedArchitecture_extractDirOnly(t *testing.T) {
	m := &Manifest{
		Version: "1.6.4.0",
		URL:     "https://example.com/ultravnc.zip",
		Architecture: map[string]interface{}{
			Arch64bit: map[string]interface{}{
				"extract_dir": "x64",
			},
			Arch32bit: map[string]interface{}{
				"extract_dir": "x86",
			},
		},
	}
	arch := m.selectArchitectureForHost(Arch64bit, 22000)
	if arch != Arch64bit {
		t.Fatalf("arch = %q want 64bit", arch)
	}
	if got := m.GetExtractDirForInstall(arch); got != "x64" {
		t.Fatalf("extract_dir = %q want x64", got)
	}
	if urls := m.GetURLsForInstall(arch); len(urls) != 1 || urls[0] != "https://example.com/ultravnc.zip" {
		t.Fatalf("urls = %v", urls)
	}
}

func TestSelectedArchitectureARM64Fallback64bit(t *testing.T) {
	m := &Manifest{
		Version: "1.0.0",
		Architecture: map[string]interface{}{
			Arch64bit: map[string]interface{}{
				"url":  "https://example.com/x64.zip",
				"hash": "h64",
			},
		},
	}
	arch := m.selectArchitectureForHost(ArchARM64, 22000)
	if arch != Arch64bit {
		t.Fatalf("arch = %q want 64bit fallback on Win11", arch)
	}
}

func TestFallbackArchitectureAfter404(t *testing.T) {
	m := &Manifest{
		Architecture: map[string]interface{}{
			ArchARM64: map[string]interface{}{
				"url": "https://example.com/arm64.exe",
			},
			Arch64bit: map[string]interface{}{
				"url": "https://example.com/x64.exe",
			},
			Arch32bit: map[string]interface{}{
				"url": "https://example.com/x86.exe",
			},
		},
	}
	if got := m.fallbackArchitectureForHost(ArchARM64, ArchARM64, 22000); got != Arch64bit {
		t.Fatalf("after arm64 = %q want 64bit", got)
	}
	if got := m.fallbackArchitectureForHost(Arch64bit, ArchARM64, 22000); got != Arch32bit {
		t.Fatalf("after 64bit = %q want 32bit", got)
	}
	if got := m.fallbackArchitectureForHost(Arch32bit, ArchARM64, 22000); got != "" {
		t.Fatalf("after 32bit = %q want empty", got)
	}
	if got := m.fallbackArchitectureForHost("", ArchARM64, 22000); got != "" {
		t.Fatalf("empty current = %q", got)
	}
}

func (m *Manifest) selectArchitectureForHost(hostArch string, winBuild int) string {
	if m.Architecture == nil {
		return ""
	}
	for _, arch := range architectureCandidates(hostArch, winBuild) {
		block, ok := m.Architecture[arch].(map[string]interface{})
		if !ok {
			continue
		}
		if archBlockIsSelectable(block) {
			return arch
		}
	}
	return ""
}

func (m *Manifest) urlsForArchitecture(arch string) []string {
	block, ok := m.Architecture[arch].(map[string]interface{})
	if !ok {
		return nil
	}
	return stringSliceFromField(block["url"])
}
