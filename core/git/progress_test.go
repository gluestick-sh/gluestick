package git

import "testing"

func TestParseGitProgressLine(t *testing.T) {
	msg, pct := parseGitProgressLine("Receiving objects:  67% (1234/2740), 2.50 MiB | 1.20 MiB/s")
	if pct != 67 {
		t.Fatalf("percent = %v, want 67", pct)
	}
	if msg == "" {
		t.Fatal("expected message")
	}

	_, pct = parseGitProgressLine("Cloning into 'games'...")
	if pct != 0 {
		t.Fatalf("percent = %v, want 0", pct)
	}
}
