package engine

import "testing"

func TestRecordBucketUpdateActivity(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if err := eng.RecordBucketAddActivity("extras", "success", ""); err != nil {
		t.Fatalf("RecordBucketAddActivity: %v", err)
	}
	if err := eng.RecordBucketRemoveActivity("old", "success", ""); err != nil {
		t.Fatalf("RecordBucketRemoveActivity: %v", err)
	}
	if err := eng.RecordBucketUpdateActivity("main", "success", ""); err != nil {
		t.Fatalf("RecordBucketUpdateActivity single: %v", err)
	}
	if err := eng.RecordBucketUpdateActivity("*", "success", ""); err != nil {
		t.Fatalf("RecordBucketUpdateActivity all: %v", err)
	}
	if err := eng.RecordBucketUpdateActivity("main,extras", "failed", "git pull failed"); err != nil {
		t.Fatalf("RecordBucketUpdateActivity failed: %v", err)
	}
	if err := eng.RecordBucketCheckActivity(2, []string{"main", "extras"}, "success", ""); err != nil {
		t.Fatalf("RecordBucketCheckActivity: %v", err)
	}
	if err := eng.RecordBucketCheckActivity(0, nil, "success", ""); err != nil {
		t.Fatalf("RecordBucketCheckActivity none: %v", err)
	}

	rows, err := eng.QueryActivityLog("", 10, 0)
	if err != nil {
		t.Fatalf("QueryActivityLog: %v", err)
	}
	if len(rows) < 7 {
		t.Fatalf("expected at least 7 activity rows, got %d", len(rows))
	}
	if op, _ := rows[0]["operation"].(string); op != "bucket_check" {
		t.Fatalf("latest operation = %q, want bucket_check", op)
	}
}

func TestBucketUpdateActivityLabel(t *testing.T) {
	tests := []struct {
		taskName string
		want     string
	}{
		{"*", "*"},
		{"main", "main"},
		{"main,extras", "main (+1)"},
	}
	for _, tc := range tests {
		if got := bucketUpdateActivityLabel(tc.taskName); got != tc.want {
			t.Fatalf("bucketUpdateActivityLabel(%q) = %q, want %q", tc.taskName, got, tc.want)
		}
	}
}
