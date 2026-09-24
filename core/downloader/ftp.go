package downloader

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// isFTPURL checks if the given URL string is an FTP URL.
// Returns true if the URL scheme is "ftp" (case-insensitive).
func isFTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, "ftp")
}

// ftpDialAddress constructs the FTP server address from a URL.
// Returns hostname:port, using default FTP port 21 if not specified.
func ftpDialAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "21"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// ftpCredentials extracts FTP username and password from a URL.
// Returns anonymous credentials if no user info is present.
func ftpCredentials(u *url.URL) (user, pass string) {
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	if user == "" {
		user = "anonymous"
	}
	if pass == "" {
		pass = "anonymous@"
	}
	return user, pass
}

// downloadFTPToPartial downloads a file from an FTP server to a partial file.
// Handles FTP connection, authentication, file retrieval, and progress tracking.
// Returns total size downloaded and any error encountered.
func (d *Downloader) downloadFTPToPartial(ctx context.Context, task Task, partPath, metaPath string) (totalSize int64, err error) {
	u, err := url.Parse(task.URL)
	if err != nil {
		return 0, fmt.Errorf("parse ftp URL: %w", err)
	}

	conn, err := ftp.Dial(
		ftpDialAddress(u),
		ftp.DialWithContext(ctx),
		ftp.DialWithTimeout(60*time.Second),
	)
	if err != nil {
		return 0, fmt.Errorf("ftp connect: %w", err)
	}
	defer conn.Quit()

	user, pass := ftpCredentials(u)
	if err := conn.Login(user, pass); err != nil {
		return 0, fmt.Errorf("ftp login: %w", err)
	}

	remotePath := u.EscapedPath()
	if remotePath == "" || remotePath == "/" {
		return 0, fmt.Errorf("ftp URL has no file path")
	}

	var expectedSize int64
	if size, sizeErr := conn.FileSize(remotePath); sizeErr == nil && size > 0 {
		expectedSize = int64(size)
	}

	bar := makeDownloadBar(task.Filename, expectedSize, 0)
	defer completeDownloadBar(bar)
	setDownloadBarProgress(bar, expectedSize, 0)
	pc := newProgressCounter(ctx, task.Filename, expectedSize, 0, bar, nil)
	pc.emit()

	resp, err := conn.Retr(remotePath)
	if err != nil {
		return 0, fmt.Errorf("ftp retr: %w", err)
	}
	defer resp.Close()

	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return 0, fmt.Errorf("open partial file: %w", err)
	}
	defer file.Close()

	buf := d.acquireCopyBuf()
	written, err := io.CopyBuffer(file, pc.reader(resp), buf)
	d.releaseCopyBuf(buf)
	if err != nil {
		return 0, fmt.Errorf("write partial file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("sync partial file: %w", err)
	}

	finalSize := written
	if expectedSize > 0 {
		finalSize = expectedSize
	}
	if expectedSize > 0 && written != expectedSize {
		return 0, fmt.Errorf("incomplete ftp download: got %d bytes, expected %d", written, expectedSize)
	}

	meta := partialMeta{URL: task.URL, SourceURL: task.URL, TotalSize: finalSize}
	if err := savePartialMeta(metaPath, meta); err != nil {
		return 0, fmt.Errorf("save partial meta: %w", err)
	}
	return finalSize, nil
}
