package downloader

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/config"
)

// urlsForTask returns the ordered list of URLs to try for a download: the
// canonical URL plus any configured GitHub mirror variants (tried as fallback).
func (d *Downloader) urlsForTask(canonical string) []string {
	if len(d.ghProxies) == 0 {
		return []string{canonical}
	}
	return config.MirrorURLs(canonical, d.ghProxies)
}

// urlsForResume returns the URL list for resuming a partial download. When
// resuming from a non-zero offset, it reuses the exact source URL the bytes came
// from (offsets are not portable across mirrors); otherwise it behaves like a
// fresh download and includes mirror fallbacks.
func urlsForResume(canonical string, proxies []string, sourceURL string, offset int64) []string {
	if offset > 0 && sourceURL != "" {
		return []string{sourceURL}
	}
	if len(proxies) == 0 {
		return []string{canonical}
	}
	return config.MirrorURLs(canonical, proxies)
}

// shortenURL truncates a URL for compact verbose log lines.
func shortenURL(url string) string {
	if len(url) <= 60 {
		return url
	}
	return url[:57] + "..."
}

// requestMutator customizes a request before it is sent (e.g. to set a Range
// header for resume).
type requestMutator func(*http.Request)

// doRequestWithFallback issues method requests across the given URLs in order,
// returning the first success. Each URL is retried per doWithRetries; any
// response with status >= 400 is treated as a failure and the next URL (mirror)
// is tried. It reports the URL that ultimately succeeded.
func (d *Downloader) doRequestWithFallback(
	ctx context.Context,
	method string,
	urls []string,
	mutate requestMutator,
) (*http.Response, string, error) {
	var lastErr error
	for i, u := range urls {
		if i > 0 {
			verbose.Fprintf("Trying mirror %d/%d: %s\n", i+1, len(urls), shortenURL(u))
		}
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", d.userAgent)
		if mutate != nil {
			mutate(req)
		}

		resp, usedURL, err := d.doWithRetries(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			body := resp.Status
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d: %s (%s)", resp.StatusCode, body, u)
			continue
		}
		return resp, usedURL, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no URLs to try")
	}
	return nil, "", fmt.Errorf("all download URLs failed, last error: %w", lastErr)
}

// doWithRetries sends a single request, retrying retryable failures up to
// downloadRetryAttempts with backoff. The request is cloned before each retry so
// its context and body remain valid. It returns the response and the URL used.
func (d *Downloader) doWithRetries(req *http.Request) (*http.Response, string, error) {
	var lastErr error
	url := req.URL.String()
	for attempt := 1; attempt <= downloadRetryAttempts; attempt++ {
		if attempt > 1 {
			delay := retryDownloadDelay(attempt - 1)
			verbose.Fprintf("Retry download %d/%d after %s: %v\n", attempt, downloadRetryAttempts, delay, lastErr)
			time.Sleep(delay)
			clone := req.Clone(req.Context())
			req = clone
		}
		resp, err := d.client.Do(req)
		if err == nil {
			return resp, url, nil
		}
		lastErr = err
		if !isRetryableDownloadErr(err) || attempt == downloadRetryAttempts {
			break
		}
	}
	return nil, url, lastErr
}

// acceptStatus reports whether a GET response status is usable: 200 OK (full
// body) or 206 Partial Content (successful range/resume request).
func acceptStatus(code int) bool {
	switch code {
	case http.StatusOK, http.StatusPartialContent:
		return true
	default:
		return false
	}
}

// doGETWithFallback is like doRequestWithFallback but for GET downloads: it
// accepts only 200/206 responses (via acceptStatus), so partial-content resume
// requests succeed while redirects and errors fall through to the next mirror.
func (d *Downloader) doGETWithFallback(
	ctx context.Context,
	urls []string,
	mutate requestMutator,
) (*http.Response, string, error) {
	var lastErr error
	for i, u := range urls {
		if i > 0 {
			verbose.Fprintf("Trying mirror %d/%d: %s\n", i+1, len(urls), shortenURL(u))
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", d.userAgent)
		if mutate != nil {
			mutate(req)
		}

		resp, usedURL, err := d.doWithRetries(req)
		if err != nil {
			lastErr = err
			continue
		}
		if !acceptStatus(resp.StatusCode) {
			body := resp.Status
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d: %s (%s)", resp.StatusCode, body, u)
			continue
		}
		return resp, usedURL, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no URLs to try")
	}
	return nil, "", fmt.Errorf("all download URLs failed, last error: %w", lastErr)
}
