package extractor

import (
	"strings"
	"testing"
)

func TestProgress7zParser_parses7zPlusFormat(t *testing.T) {
	var got []int
	p := newProgress7zParser(func(percent int) { got = append(got, percent) })
	if _, err := p.Write([]byte("  45% 2 + inkscape.7z\n100% 2 + inkscape.7z\n")); err != nil {
		t.Fatal(err)
	}
	want := []int{45, 100}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestProgress7zParser_parsesLeadingSpaceAndChunks(t *testing.T) {
	var got []int
	p := newProgress7zParser(func(percent int) { got = append(got, percent) })

	chunks := []string{"  12% 1/2 - Extract", "ing\n100% 2/2 - Everything is Ok\n"}
	for _, chunk := range chunks {
		if _, err := p.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}

	want := []int{12, 100}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestProgress7zParser_dedupesRepeatedPercent(t *testing.T) {
	var got []int
	p := newProgress7zParser(func(percent int) { got = append(got, percent) })
	if _, err := p.Write([]byte("45% 1/2 - a\n45% 1/2 - b\n")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 45 {
		t.Fatalf("got %v, want [45]", got)
	}
}

func TestScaleExtractPercent_twoStages(t *testing.T) {
	if got := scaleExtractPercent(100, 1, 2); got != 50 {
		t.Fatalf("stage1 100%% = %d, want 50", got)
	}
	if got := scaleExtractPercent(100, 2, 2); got != 100 {
		t.Fatalf("stage2 100%% = %d, want 100", got)
	}
}

func TestTrimProgressLine(t *testing.T) {
	line := trimProgressLine("\r  12% 1/2 - file\r")
	if !strings.HasPrefix(line, "12%") {
		t.Fatalf("trimProgressLine = %q", line)
	}
}
