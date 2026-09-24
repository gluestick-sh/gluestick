package extractor

import "testing"

func TestDetectType_nupkg(t *testing.T) {
	ext := NewExtractor(nil)
	got, err := ext.detectType(`C:\cache\AnthropicClaude-1.11187.4-full.nupkg`)
	if err != nil {
		t.Fatalf("detectType: %v", err)
	}
	if got != "zip" {
		t.Fatalf("detectType = %q, want zip", got)
	}
}
