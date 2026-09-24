package install

import (
	"github.com/gluestick-sh/core/downloader"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/progress"
	"github.com/gluestick-sh/core/verbose"
)

// attachExtractProgressHandlers wires file-ingest and 7z decompress callbacks for deploy/fetch.
func attachExtractProgressHandlers(
	prog *progress.Handler,
	report func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64),
) {
	ingestProgress := downloader.NewThrottledProgress(func(processed, total int64, _ string) {
		pct := 0.0
		if total > 0 {
			pct = (float64(processed) / float64(total)) * 100
		}
		args := map[string]any{
			"current": processed,
			"total":   total,
		}
		report(etypes.PhaseExtract, etypes.StatusRunning, pct, message.ProgressExtractProcessing, args, processed, total)
		printCLIExtractProgress(message.FormatEN(message.ProgressExtractProcessing, args))
	})
	prog.Files = func(processed, total int64) { ingestProgress.Report(processed, total, "") }

	decompressReport := func(percent int) {
		pct := float64(percent)
		args := map[string]any{"percent": percent}
		report(etypes.PhaseExtract, etypes.StatusRunning, pct, message.ProgressExtractDecompress, args, int64(percent), 100)
		printCLIExtractProgress(message.FormatEN(message.ProgressExtractDecompress, args))
	}
	prog.Extract = decompressReport
}

func printCLIExtractProgress(line string) {
	if line == "" {
		return
	}
	verbose.Progressf("\r  %s   ", line)
}
