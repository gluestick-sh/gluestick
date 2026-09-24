package downloader

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestThrottledProgress(t *testing.T) {
	var calls int
	var lastDownloaded int64
	tp := NewThrottledProgress(func(downloaded, total int64, _ string) {
		calls++
		lastDownloaded = downloaded
	})

	tp.Report(0, 100, "start")
	tp.Report(10, 100, "same pct soon")
	tp.Report(20, 100, "next")
	if calls < 2 {
		t.Fatalf("expected at least 2 calls, got %d (last=%d)", calls, lastDownloaded)
	}
	if lastDownloaded != 20 {
		t.Fatalf("last downloaded = %d, want 20", lastDownloaded)
	}
}

func TestThrottledProgressFlush(t *testing.T) {
	var calls int
	tp := NewThrottledProgress(func(downloaded, total int64, _ string) {
		calls++
		if downloaded != 99 || total != 100 {
			t.Fatalf("flush downloaded=%d total=%d", downloaded, total)
		}
	})
	tp.Report(99, 100, "almost")
	tp.Report(99, 100, "throttled")
	if calls != 1 {
		t.Fatalf("expected 1 call before flush, got %d", calls)
	}
	tp.Flush()
	if calls != 2 {
		t.Fatalf("expected flush to emit again, got %d calls", calls)
	}
}

func TestProgressCounterReader(t *testing.T) {
	var lastDownloaded int64
	ctx := ContextWithDownloadProgress(context.Background(), func(downloaded, total int64, _ string) {
		lastDownloaded = downloaded
	})
	pc := newProgressCounter(ctx, "test.bin", 10, 0, nil, nil)
	reader := pc.reader(&fixedReader{data: []byte("1234567890")})
	buf := make([]byte, 10)
	if _, err := reader.Read(buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(progressEmitInterval + 30*time.Millisecond)
	if lastDownloaded != 10 {
		t.Fatalf("downloaded = %d, want 10", lastDownloaded)
	}
}

type fixedReader struct {
	data []byte
	off  int
}

func (f *fixedReader) Read(p []byte) (int, error) {
	if f.off >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}
