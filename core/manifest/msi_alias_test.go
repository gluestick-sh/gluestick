package manifest

import "testing"

func TestIsScoopMsiAlias(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"dl.msi_", true},
		{"DL.MSI_", true},
		{"cache/dl.msi_", true},
		{"dl.msi", false},
		{"setup.msi_", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsScoopMsiAlias(tc.name); got != tc.want {
				t.Fatalf("IsScoopMsiAlias(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsScoopMsiHookInstall(t *testing.T) {
	tests := []struct {
		localName string
		fileExt   string
		want      bool
	}{
		{"dl.msi_", "", true},
		{"app.zip", ".msi_", true},
		{"app.zip", ".MSI_", true},
		{"setup.exe", ".msi", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.localName+"_"+tc.fileExt, func(t *testing.T) {
			if got := IsScoopMsiHookInstall(tc.localName, tc.fileExt); got != tc.want {
				t.Fatalf("IsScoopMsiHookInstall(%q, %q) = %v, want %v",
					tc.localName, tc.fileExt, got, tc.want)
			}
		})
	}
}
