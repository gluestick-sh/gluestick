package bootstrap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// contextOrBackground returns ctx, or context.Background() when ctx is nil, so
// callers can pass a nil context without panicking.
func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// httpGetAll performs a GET and reads the full response body into memory. It
// honors ctx (checking cancellation up front) and applies timeout as the client
// deadline. A non-200 status is returned as an error. It returns the body and
// the reported Content-Length.
func httpGetAll(ctx context.Context, url string, timeout time.Duration) ([]byte, int64, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return data, resp.ContentLength, nil
}
