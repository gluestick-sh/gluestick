package downloader

import (
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

const (
	downloadRetryAttempts = 4
	downloadRetryBase     = 500 * time.Millisecond
)

// isRetryableDownloadErr checks if an error is retryable during download.
// Returns true for network timeouts, connection errors, EOF, and similar transient failures.
func isRetryableDownloadErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "eof") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "forcibly closed") ||
		strings.Contains(msg, "connection aborted")
}

// retryDownloadDelay calculates exponential backoff delay for download retries.
// Returns delay duration: 500ms * 2^(attempt-1) for attempt > 0, else 0.
func retryDownloadDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return downloadRetryBase * time.Duration(1<<(attempt-1))
}
