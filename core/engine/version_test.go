package engine

import "testing"

func TestUpdateAvailable(t *testing.T) {
	tests := []struct {
		installed, latest string
		want              bool
	}{
		{"11.3.0", "11.4.0", true},
		{"11.4.0", "11.4.0", false},
		{"11.4.0", "11.3.0", false},
		{"v1.0.0", "1.0.1", true},
	}
	for _, tt := range tests {
		if got := UpdateAvailable(tt.installed, tt.latest); got != tt.want {
			t.Errorf("UpdateAvailable(%q, %q) = %v, want %v", tt.installed, tt.latest, got, tt.want)
		}
	}
}
