package install

import "testing"

func TestInnounpComponentArg(t *testing.T) {
	tests := []struct {
		extractDir string
		want       string
	}{
		{"", "-c{app}"},
		{"subdir", "-c{app}\\subdir"},
		{"{code}", "-c{code}"},
	}
	for _, tt := range tests {
		if got := innounpComponentArg(tt.extractDir); got != tt.want {
			t.Errorf("innounpComponentArg(%q) = %q, want %q", tt.extractDir, got, tt.want)
		}
	}
}
