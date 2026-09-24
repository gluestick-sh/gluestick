package runtime

import "testing"

func TestSearchIndexFindExactName(t *testing.T) {
	idx := NewIndex()
	idx.entries = []Entry{
		{Name: "python", Bucket: "main"},
		{Name: "python", Bucket: "extras"},
		{Name: "node", Bucket: "main"},
	}

	matches := idx.FindExactName("python")
	if len(matches) != 2 {
		t.Fatalf("expected 2 exact matches, got %d", len(matches))
	}
}
