package bootstrap

import (
	"io"
	"os"

	"github.com/schollz/progressbar/v3"
)

// bootstrapStderr clears each progress row before render (see downloader.progressBarOut).
var bootstrapStderr io.Writer = &bootstrapProgressBarWriter{w: &bootstrapFlushWriter{w: os.Stderr}}

type bootstrapFlushWriter struct {
	w io.Writer
}

func (f *bootstrapFlushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if file, ok := f.w.(*os.File); ok {
		_ = file.Sync()
	}
	return n, err
}

type bootstrapProgressBarWriter struct {
	w io.Writer
}

func (p *bootstrapProgressBarWriter) Write(b []byte) (int, error) {
	if len(b) > 0 && string(b) != "\r" {
		clearBootstrapProgressLine(p.w)
	}
	return p.w.Write(b)
}

func makeBootstrapDownloadBar(description string, maxSize int64) *progressbar.ProgressBar {
	return progressbar.NewOptions64(maxSize,
		progressbar.OptionSetDescription(description),
		progressbar.OptionSetWriter(bootstrapStderr),
		progressbar.OptionShowCount(),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionClearOnFinish(),
	)
}

// finishBootstrapDownloadBar erases the in-place progress row after a bootstrap download.
func finishBootstrapDownloadBar(bar *progressbar.ProgressBar) {
	if bar == nil {
		return
	}
	_ = bar.Clear()
	clearBootstrapProgressLine(bootstrapStderr)
}

func clearBootstrapProgressLine(w io.Writer) {
	_, _ = io.WriteString(w, "\r\033[2K")
}
