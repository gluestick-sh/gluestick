package progress

import (
	"context"
	"testing"
)

func TestWithHandlerRoundTrip(t *testing.T) {
	var bytesCalled bool
	h := &Handler{
		Bytes: func(downloaded, total int64, message string) {
			bytesCalled = true
			if downloaded != 1 || total != 10 || message != "ok" {
				t.Fatalf("Bytes(%d, %d, %q)", downloaded, total, message)
			}
		},
		Files: func(processed, total int64) {
			if processed != 2 || total != 5 {
				t.Fatalf("Files(%d, %d)", processed, total)
			}
		},
		Extract: func(percent int) {
			if percent != 42 {
				t.Fatalf("Extract(%d)", percent)
			}
		},
	}

	ctx := WithHandler(context.Background(), h)
	got := HandlerFrom(ctx)
	if got != h {
		t.Fatal("HandlerFrom returned different handler")
	}
	got.Bytes(1, 10, "ok")
	got.Files(2, 5)
	got.Extract(42)
	if !bytesCalled {
		t.Fatal("Bytes callback was not invoked")
	}
}

func TestHandlerFrom_nilContext(t *testing.T) {
	if HandlerFrom(nil) != nil {
		t.Fatal("expected nil handler for nil context")
	}
}

func TestWithHandler_nilHandler(t *testing.T) {
	ctx := WithHandler(context.Background(), nil)
	if HandlerFrom(ctx) != nil {
		t.Fatal("expected nil handler when attaching nil")
	}
}

func TestWithHandler_nilContextUsesBackground(t *testing.T) {
	h := &Handler{}
	ctx := WithHandler(nil, h)
	if ctx == nil {
		t.Fatal("expected non-nil context")
	}
	if HandlerFrom(ctx) != h {
		t.Fatal("handler not stored on derived context")
	}
}
