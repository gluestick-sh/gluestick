package humanize

import (
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
	}
	for _, tt := range tests {
		if got := FormatBytes(tt.bytes); got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestFormatCacheDate(t *testing.T) {
	if got := FormatCacheDate(""); got != "-" {
		t.Fatalf("empty: got %q", got)
	}
	got := FormatCacheDate("2024-06-01T12:30:00Z")
	if got == "" || got == "2024-06-01T12:30:00Z" {
		t.Fatalf("RFC3339: got %q", got)
	}
	if got := FormatCacheDate("not-a-date"); got != "not-a-date" {
		t.Fatalf("passthrough: got %q", got)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "500 ms"},
		{1500 * time.Millisecond, "1.5 s"},
		{90 * time.Second, "1m 30s"},
		{125 * time.Second, "2m 5s"},
	}
	for _, tt := range tests {
		if got := FormatDuration(tt.d); got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
