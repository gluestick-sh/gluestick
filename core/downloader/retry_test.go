package downloader

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIsRetryableDownloadErr(t *testing.T) {
	if !isRetryableDownloadErr(io.EOF) {
		t.Fatal("EOF should be retryable")
	}
	if isRetryableDownloadErr(io.ErrUnexpectedEOF) == false {
		t.Fatal("unexpected EOF should be retryable")
	}
}

type eofThenOKTransport struct {
	attempts atomic.Int32
}

func (t *eofThenOKTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.attempts.Add(1) < 3 {
		return nil, io.EOF
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestDoWithRetriesEOF(t *testing.T) {
	d := &Downloader{
		client:    &http.Client{Transport: &eofThenOKTransport{}},
		userAgent: "glue-test",
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.test/file.zip", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, _, err := d.doWithRetries(req)
	if err != nil {
		t.Fatalf("doWithRetries: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
