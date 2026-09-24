package extractor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/procutil"
	"github.com/gluestick-sh/core/verbose"
)

// ExtractToDir extracts an archive directly into destDir via 7z.exe.
// archiveName is the original download filename (e.g. pkg.tar.gz) and guides format detection.
func (e *Extractor) ExtractToDir(archivePath, destDir, archiveName string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	if fn := e.extractProgress(); fn != nil {
		fn(0)
	}

	start := time.Now()
	if !e.use7z {
		return fmt.Errorf("7z.exe not available")
	}

	stagePath, cleanup, err := e.stageArchiveFor7z(archivePath, archiveName)
	if err != nil {
		return err
	}
	defer cleanup()

	sevenZip := "7z.exe"
	if e.sevenZip != "" {
		sevenZip = e.sevenZip
	}

	if err := e.run7zExtract(sevenZip, stagePath, destDir, 1, extractStagesForName(archiveName)); err != nil {
		return err
	}

	if err := e.extractNestedTar(sevenZip, destDir, archiveName); err != nil {
		return err
	}

	verbose.Progressf("  Extracted archive to install dir in %s (7z -mmt=%d)\n", humanize.FormatDuration(time.Since(start)), e.workers)
	return nil
}

func (e *Extractor) stageArchiveFor7z(archivePath, archiveName string) (string, func(), error) {
	noop := func() {}
	if archiveName == "" {
		archiveName = filepath.Base(archivePath)
	}
	ext := archiveExtension(archiveName)
	if ext == "" || strings.EqualFold(filepath.Ext(archivePath), ext) {
		return archivePath, noop, nil
	}

	tmpPath, cleanup, err := stageArchiveWithExtension(e.tempDir, archivePath, ext)
	if err != nil {
		return "", noop, err
	}
	return tmpPath, cleanup, nil
}

func stageArchiveWithExtension(tempDir, archivePath, ext string) (string, func(), error) {
	noop := func() {}
	tmpPath := filepath.Join(tempDir, fmt.Sprintf("stage-%s%s", uniqueTempName(), ext))
	if err := os.MkdirAll(filepath.Dir(tmpPath), 0755); err != nil {
		return "", noop, err
	}
	if err := os.Link(archivePath, tmpPath); err != nil {
		if err := copyFileContents(archivePath, tmpPath); err != nil {
			return "", noop, fmt.Errorf("stage archive: %w", err)
		}
	}
	return tmpPath, func() { _ = os.Remove(tmpPath) }, nil
}

func archiveExtension(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return ".tar.gz"
	case strings.HasSuffix(lower, ".tar.bz2"):
		return ".tar.bz2"
	case strings.HasSuffix(lower, ".tar.xz"):
		return ".tar.xz"
	case strings.HasSuffix(lower, ".7z.exe"):
		return ".7z.exe"
	default:
		return filepath.Ext(name)
	}
}

func (e *Extractor) run7zExtract(sevenZip, archivePath, destDir string, stage, totalStages int) error {
	enableProgress := e.extractProgress() != nil
	args := e.build7zExtractArgs(destDir, archivePath, enableProgress)
	cmd := exec.CommandContext(e.execContext(), sevenZip, args...)
	procutil.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout

	if parser := newProgress7zParser(e.stageExtractProgress(stage, totalStages)); parser != nil {
		cmd.Stdout = io.MultiWriter(&stdout, parser)
		cmd.Stderr = &stderr
	} else {
		cmd.Stderr = &stderr
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("7z extract failed: %w\nStdout: %s\nStderr: %s", err, stdout.String(), stderr.String())
	}
	return nil
}

func (e *Extractor) build7zExtractArgs(destDir, archivePath string, enableProgress bool) []string {
	var prefix []string
	if e.workers > 1 {
		prefix = append(prefix, fmt.Sprintf("-mmt=%d", e.workers))
	}
	if enableProgress {
		// -bso0 hides file list; -bsp1 emits percentage progress on stdout.
		prefix = append(prefix, "-bso0", "-bsp1")
	}
	args := []string{"x", "-y", fmt.Sprintf("-o%s", destDir), archivePath}
	return append(prefix, args...)
}

// extractNestedTar handles compound formats (tar.gz, tar.bz2, tar.xz) where 7z only
// decompresses the outer layer on the first pass.
func (e *Extractor) extractNestedTar(sevenZip, destDir, archiveName string) error {
	tarFile, err := findNestedTar(destDir)
	if err != nil {
		return err
	}
	if tarFile == "" {
		if extractStagesForName(archiveName) > 1 {
			e.finishCompoundExtractProgress()
		}
		return nil
	}
	if err := e.run7zExtract(sevenZip, tarFile, destDir, 2, 2); err != nil {
		return err
	}
	return os.Remove(tarFile)
}

func extractStagesForName(archiveName string) int {
	if isCompoundArchiveName(archiveName) {
		return 2
	}
	return 1
}

func isCompoundArchiveName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".tar.bz2") ||
		strings.HasSuffix(lower, ".tar.xz") ||
		strings.HasSuffix(lower, ".tbz2") ||
		strings.HasSuffix(lower, ".txz")
}

func findNestedTar(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".tar") {
			return filePath, nil
		}
		if isTarFile(filePath) {
			return filePath, nil
		}
	}
	return "", nil
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
