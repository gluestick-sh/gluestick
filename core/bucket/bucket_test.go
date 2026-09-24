package bucket

import (
	"testing"
)

func TestKnownBuckets(t *testing.T) {
	buckets := KnownBuckets()

	if len(buckets) == 0 {
		t.Error("no known buckets")
	}

	// Check for expected buckets
	expected := []string{"main", "extras", "versions", "java", "php", "games"}
	for _, name := range expected {
		if _, ok := buckets[name]; !ok {
			t.Errorf("missing known bucket: %s", name)
		}
	}

	// Check URLs are valid
	for name, url := range buckets {
		if url == "" {
			t.Errorf("bucket %s has empty URL", name)
		}
	}
}

func TestKnownBucketDescriptions(t *testing.T) {
	descs := KnownBucketDescriptions()
	for name := range KnownBuckets() {
		if descs[name] == "" {
			t.Errorf("missing description for known bucket %q", name)
		}
	}
	if got := GetKnownBucketDescription("main"); got == "" {
		t.Fatal("expected main description")
	}
	if got := GetKnownBucketDescription("custom"); got != "" {
		t.Fatalf("custom bucket description = %q, want empty", got)
	}
}

func TestIsKnownBucket(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"main", true},
		{"extras", true},
		{"versions", true},
		{"java", true},
		{"php", true},
		{"games", true},
		{"unknown", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsKnownBucket(tt.name); got != tt.expected {
				t.Errorf("IsKnownBucket(%s) = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}
