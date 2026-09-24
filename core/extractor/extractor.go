// Package extractor runs 7-Zip and related tools to unpack archives into cache store or install dirs.
package extractor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/procutil"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
)

// UnsupportedFormatError is returned when the archive format is not supported
type UnsupportedFormatError struct {
	Format     string
	Suggestion string
}

// Error returns the formatted unsupported-format message and optional suggestion.
func (e *UnsupportedFormatError) Error() string {
	msg := fmt.Sprintf("unsupported format: %s", e.Format)
	if e.Suggestion != "" {
		msg += "\n" + e.Suggestion
	}
	return msg
}

// Extractor handles various archive formats
type Extractor struct {
	store    *store.Store
	tempDir  string
	use7z    bool   // Whether to use external 7z.exe
	sevenZip string // Path to 7z executable
	workers  int    // parallel 7z threads (-mmt) and CAS ingest workers

	ingestProgress IngestProgressFunc
	ctx            context.Context
}

// NewExtractor creates a new extractor
func NewExtractor(store *store.Store) *Extractor {
	return &Extractor{
		store:    store,
		tempDir:  os.TempDir(),
		use7z:    check7zAvailable(),
		sevenZip: "", // Will be set if needed
		workers:  4,
	}
}

// SetWorkers sets 7-Zip multithreading (-mmt) and parallel cache store ingest worker count.
func (e *Extractor) SetWorkers(n int) {
	if n < 1 {
		n = 1
	}
	e.workers = n
}

// SetContext attaches cancellation to 7z subprocesses and file-ingest progress lookups.
func (e *Extractor) SetContext(ctx context.Context) {
	e.ctx = ctx
}

func (e *Extractor) execContext() context.Context {
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// SetIngestProgress sets a callback for cache store ingest progress (UI clients).
// Pass nil to disable.
func (e *Extractor) SetIngestProgress(fn IngestProgressFunc) {
	e.ingestProgress = fn
}

// Set7zPath sets the path to 7z executable
func (e *Extractor) Set7zPath(path string) {
	e.sevenZip = path
	e.use7z = true
}

// SevenZipPath returns the configured 7z executable path, if any.
func (e *Extractor) SevenZipPath() string {
	return e.sevenZip
}

// check7zAvailable checks if 7z.exe is available in PATH
func check7zAvailable() bool {
	cmd := exec.Command("7z.exe", "help")
	procutil.HideWindow(cmd)
	_, err := cmd.CombinedOutput()
	// 7z "help" command should succeed even if it shows usage
	return err == nil
}

// ExtractFile extracts an archive file to the CAS store
// archiveType is optional; if empty, will be detected from file path
// Returns: (rootHash, map[relativePath]hash, error)
func (e *Extractor) ExtractFile(archivePath string, archiveType ...string) (string, map[string]string, error) {
	var at string
	if len(archiveType) > 0 && archiveType[0] != "" {
		at = archiveType[0]
	} else {
		// Detect archive type from file path
		var err error
		at, err = e.detectType(archivePath)
		if err != nil {
			return "", nil, fmt.Errorf("detect archive type: %w", err)
		}
	}

	switch at {
	case "zip":
		return e.extractWith7z(archivePath, "zip")
	case "7z", "xz", "gz", "bz2", "tar":
		return e.extractWith7z(archivePath, at)
	default:
		// Unknown format, try to extract with 7z anyway
		return e.extractWith7z(archivePath, at)
	}
}

// ExtractReader extracts from an io.Reader (for streaming)
func (e *Extractor) ExtractReader(r io.Reader, filename string) (string, map[string]string, error) {
	// For streaming, we need to buffer the data
	var buf bytes.Buffer
	_ = hashReader(r, &buf) // Hash the data

	data := buf.Bytes()

	// Detect type from content
	archiveType := detectTypeFromContent(data)

	switch archiveType {
	case "zip":
		return e.extractZIP(data)
	default:
		// For non-streaming formats, write to temp file first
		tmpPath := filepath.Join(e.tempDir, filename)
		if err := os.WriteFile(tmpPath, data, 0644); err != nil {
			return "", nil, err
		}
		defer os.Remove(tmpPath)
		return e.extractWith7z(tmpPath, archiveType)
	}
}

// DetectType detects the archive type from a filename or path extension.
func (e *Extractor) DetectType(path string) (string, error) {
	return e.detectType(path)
}

// detectType detects the archive type from file extension
func (e *Extractor) detectType(path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))

	// Check for compound extensions
	switch {
	case ext == ".zip", ext == ".nupkg":
		return "zip", nil
	case ext == ".7z":
		return "7z", nil
	case ext == ".gz" && strings.HasSuffix(base, ".tar.gz"):
		return "tar", nil
	case ext == ".gz":
		return "gz", nil
	case ext == ".bz2":
		return "bz2", nil
	case ext == ".xz":
		return "xz", nil
	case ext == ".tar":
		return "tar", nil
	case ext == ".msi", ext == ".msi_":
		return "msi", nil
	case ext == ".exe":
		return "exe", nil
	default:
		return "", fmt.Errorf("unknown archive type: %s", ext)
	}
}

// extractZIP extracts a ZIP archive
func (e *Extractor) extractZIP(data []byte) (string, map[string]string, error) {
	// This is implemented in the downloader package
	// For now, return an error
	return "", nil, fmt.Errorf("ZIP extraction not implemented in extractor")
}

// uniqueTempName generates a unique temp directory name
func uniqueTempName() string {
	data := make([]byte, 4)
	if _, err := rand.Read(data); err != nil {
		// Fallback to simple counter
		data = []byte{byte(os.Getpid() & 0xFF), byte(os.Getpid() >> 8), byte(os.Getpid() >> 16), byte(os.Getpid() >> 24)}
	}
	return hex.EncodeToString(data)
}

// extractWith7z uses external 7z.exe to extract archives
// Supports: 7z, XZ, GZIP, BZIP2, TAR, MSI, and more
// Returns: (archiveHash, map[relativePath]hash, error)
func (e *Extractor) extractWith7z(archivePath, archiveType string) (string, map[string]string, error) {
	if !e.use7z {
		return "", nil, fmt.Errorf("7z.exe not found in PATH")
	}

	// Determine which 7z executable to use
	sevenZip := "7z.exe"
	if e.sevenZip != "" {
		sevenZip = e.sevenZip
	}

	// Create temp extraction directory
	extractDir := filepath.Join(e.tempDir, fmt.Sprintf("extract-%s", uniqueTempName()))
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(extractDir)

	stages := extractStagesForName(filepath.Base(archivePath))
	if archiveType == "gz" || archiveType == "bz2" || archiveType == "xz" {
		stages = 2
	}

	decompressStart := time.Now()
	if err := e.run7zExtract(sevenZip, archivePath, extractDir, 1, stages); err != nil {
		return "", nil, err
	}
	decompressDur := time.Since(decompressStart)
	verbose.Progressf("  Decompressed archive in %s\n", humanize.FormatDuration(decompressDur))

	// Handle compound formats like tar.gz, tar.bz2, tar.xz
	// 7z extracts the outer layer (e.g., gzip), leaving .tar file
	// Need to extract the tar file as well
	var tarFile string
	entries, _ := os.ReadDir(extractDir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := filepath.Join(extractDir, entry.Name())
		// Check by .tar extension
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".tar") {
			tarFile = filePath
			break
		}
		// Check by content (for files without .tar extension, e.g., from CAS)
		if isTarFile(filePath) {
			tarFile = filePath
			break
		}
	}

	var intermediateTarHash string
	if tarFile != "" {
		// Write the tar file to CAS before extracting it (cached for reuse).
		tarData, err := os.ReadFile(tarFile)
		if err == nil {
			intermediateTarHash, err = e.store.Write(bytes.NewReader(tarData))
			if err != nil {
				intermediateTarHash = ""
			}
		}

		// This is a compound format, extract the tar file
		if err := e.run7zExtract(sevenZip, tarFile, extractDir, 2, 2); err != nil {
			return "", nil, err
		}
		// Remove the intermediate tar file
		os.Remove(tarFile)
	} else if stages > 1 {
		e.finishCompoundExtractProgress()
	}

	files, ingestDur, err := e.ingestExtractedDir(extractDir)
	if err != nil {
		return "", nil, err
	}
	verbose.Progressf("  Stored %d file(s) to cache in %s (%d workers)\n", len(files), humanize.FormatDuration(ingestDur), e.workers)

	// Leading dot marks intermediate blobs; install skips these when linking.
	if intermediateTarHash != "" {
		files[".intermediate.tar"] = intermediateTarHash
	}

	// Return the archive hash (in production, calculate this)
	return hashBytesFromFile(archivePath), files, nil
}

// ExtractMSI extracts an MSI installer
func (e *Extractor) ExtractMSI(msiPath string) (string, map[string]string, error) {
	if !e.use7z {
		return "", nil, &UnsupportedFormatError{
			Format: "MSI",
			Suggestion: "MSI extraction requires 7z.exe in PATH.\n" +
				"Install 7z with: glue install 7zip\n" +
				"Or use ZIP format packages instead.",
		}
	}

	// MSI files can be extracted with 7z
	return e.extractWith7z(msiPath, "msi")
}

// extractEXE extracts a self-extracting archive or single executable
func (e *Extractor) ExtractEXE(exePath string) (string, map[string]string, error) {
	// First, try to extract with 7z (some .exe are self-extracting archives)
	if e.use7z {
		// Determine which 7z executable to use
		sevenZip := "7z.exe"
		if e.sevenZip != "" {
			sevenZip = e.sevenZip
		}

		// Try to list contents first
		args := []string{"l", exePath}
		cmd := exec.CommandContext(e.execContext(), sevenZip, args...)
		procutil.HideWindow(cmd)
		listErr := cmd.Run()

		if listErr == nil {
			// It's an archive, extract it
			return e.extractWith7z(exePath, "exe")
		}

		// 7z list failed, try to extract directly (might still work)
		// Some SFX files can't be listed but can be extracted
		args = []string{"x", "-y", exePath}
		cmd = exec.CommandContext(e.execContext(), sevenZip, args...)
		procutil.HideWindow(cmd)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		// Create a unique temp directory for extraction
		tempDir := filepath.Join(e.tempDir, fmt.Sprintf("extract-sfx-%s", uniqueTempName()))
		if err := os.MkdirAll(tempDir, 0755); err != nil {
			return "", nil, fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tempDir)

		// Try extracting to temp dir
		// First, copy the file to a temporary location (7z may have issues with hardlinks)
		tempExePath := filepath.Join(e.tempDir, fmt.Sprintf("sfx-%s.exe", uniqueTempName()))
		data, err := os.ReadFile(exePath)
		if err != nil {
			return "", nil, fmt.Errorf("read exe for extraction: %w", err)
		}
		if err := os.WriteFile(tempExePath, data, 0755); err != nil {
			return "", nil, fmt.Errorf("write temp exe: %w", err)
		}
		defer os.Remove(tempExePath)

		extractArgs := e.build7zExtractArgs(tempDir, tempExePath, e.extractProgress() != nil)
		cmd = exec.CommandContext(e.execContext(), sevenZip, extractArgs...)
		procutil.HideWindow(cmd)
		cmd.Stdout = &stdout
		if parser := newProgress7zParser(e.stageExtractProgress(1, 1)); parser != nil {
			cmd.Stdout = io.MultiWriter(&stdout, parser)
			cmd.Stderr = &stderr
		} else {
			cmd.Stderr = &stderr
		}

		if err := cmd.Run(); err == nil {
			files, ingestDur, ingestErr := e.ingestExtractedDir(tempDir)
			if ingestErr != nil {
				return "", nil, ingestErr
			}
			verbose.Progressf("  Stored %d file(s) to cache in %s (%d workers)\n", len(files), humanize.FormatDuration(ingestDur), e.workers)
			return hashBytesFromFile(exePath), files, nil
		}
		return "", nil, fmt.Errorf("7z extract SFX failed: %w\nStderr: %s", err, stderr.String())
	}

	// Not an archive, it's a single executable
	// Read it and write to CAS as-is
	data, err := os.ReadFile(exePath)
	if err != nil {
		return "", nil, fmt.Errorf("read exe: %w", err)
	}

	hash, err := e.store.Write(bytes.NewReader(data))
	if err != nil {
		return "", nil, fmt.Errorf("write to CAS: %w", err)
	}

	// Return the exe filename with its hash
	filename := filepath.Base(exePath)
	return hash, map[string]string{filename: hash}, nil
}

// detectTypeFromContent detects archive type from magic bytes
func detectTypeFromContent(data []byte) string {
	if len(data) < 4 {
		return ""
	}

	// ZIP: PK\x03\x04 or PK\x05\x06
	if data[0] == 0x50 && data[1] == 0x4B {
		if data[2] == 0x03 || data[2] == 0x05 {
			return "zip"
		}
	}

	// 7z: 7z\xBC\xAF\x27\x1C
	if len(data) >= 6 &&
		data[0] == 0x37 && data[1] == 0x7A &&
		data[2] == 0xBC && data[3] == 0xAF &&
		data[4] == 0x27 && data[5] == 0x1C {
		return "7z"
	}

	return ""
}

// hashBytes returns SHA-256 hash of bytes
func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// hashReader hashes from io.Reader and optionally writes to writer
func hashReader(r io.Reader, w io.Writer) string {
	h := sha256.New()

	var writer io.Writer = h
	if w != nil {
		writer = io.MultiWriter(h, w)
	}

	io.Copy(writer, r)
	return hex.EncodeToString(h.Sum(nil))
}

// hashBytesFromFile hashes a file
func hashBytesFromFile(path string) string {
	data, _ := os.ReadFile(path)
	return hashBytes(data)
}

// isTarFile checks if a file is a tar archive by reading its header
func isTarFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 512 {
		return false
	}
	// Check if it could be a tar file by looking at the header
	// Tar files start with a 512-byte header. The checksum field is bytes 148-155.
	// It's 6 octal digits followed by null and space.
	// We check if bytes 148-153 are octal digits and byte 154 is null or space.
	for i := 148; i < 154; i++ {
		b := data[i]
		if b < '0' || b > '7' {
			// Not an octal digit
			return false
		}
	}
	typeflag := data[156]
	// Valid typeflags: 0 (regular), '0' (regular), 1 (hardlink), 2 (symlink),
	// 3 (chardev), 4 (blockdev), 5 (directory), 6 (fifo), 'x', 'g' (extended)
	return typeflag == 0 || typeflag == '0' || typeflag == 1 || typeflag == '2' ||
		typeflag == 3 || typeflag == 4 || typeflag == 5 || typeflag == 6 ||
		typeflag == 'x' || typeflag == 'g'
}
