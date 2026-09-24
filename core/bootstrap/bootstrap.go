// Package bootstrap downloads runtime tools into ~/.glue/bin (MinGit, 7-Zip, WiX, innounp).
package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/procutil"
)

// Bootstrap downloads essential tools that glue needs
type Bootstrap struct {
	rootDir   string
	sevenZip  string   // Path to 7z executable
	ghProxies []string // GitHub proxy URLs to try
}

func (b *Bootstrap) bootstrappedGitPath() string {
	return filepath.Join(b.binDir(), "git", "mingw64", "bin", "git.exe")
}

func (b *Bootstrap) minGitZipPath() string {
	return filepath.Join(b.binDir(), "mingit.zip")
}

func (b *Bootstrap) cleanupMinGitZip() {
	_ = os.Remove(b.minGitZipPath())
}

func (b *Bootstrap) binDir() string {
	return filepath.Join(b.rootDir, "bin")
}

// NewBootstrap creates a new bootstrap manager for the given glue root.
// Empty rootDir uses ~/.glue.
func NewBootstrap(rootDir string) *Bootstrap {
	if rootDir == "" {
		home, _ := os.UserHomeDir()
		rootDir = filepath.Join(home, ".glue")
	}
	b := &Bootstrap{
		rootDir:   rootDir,
		ghProxies: config.LoadProxies(rootDir),
	}

	return b
}

// SetGitHubProxies replaces GitHub mirror prefixes used for bootstrap downloads.
func (b *Bootstrap) SetGitHubProxies(proxies []string) {
	if len(proxies) == 0 {
		b.ghProxies = nil
		return
	}
	b.ghProxies = append([]string(nil), proxies...)
}

// downloadWithFallback tries to download from multiple URLs with fallback.
func (b *Bootstrap) downloadWithFallback(ctx context.Context, urls []string) ([]byte, error) {
	return b.downloadWithFallbackTimeout(ctx, urls, 5*time.Minute)
}

func (b *Bootstrap) downloadWithFallbackTimeout(ctx context.Context, urls []string, timeout time.Duration) ([]byte, error) {
	var lastErr error
	ctx = contextOrBackground(ctx)

	for i, dlURL := range urls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i > 0 {
			// Show a shortened URL for display
			displayURL := dlURL
			if len(displayURL) > 60 {
				displayURL = displayURL[:57] + "..."
			}
			verbose.Progressf("  Trying mirror %d/%d: %s\n", i, len(urls), displayURL)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
		if err != nil {
			lastErr = err
			continue
		}

		client := &http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		// Success! Read the response
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		return data, nil
	}

	return nil, fmt.Errorf("all mirrors failed, last error: %w", lastErr)
}

func verify7zRunnable(path string) error {
	cmd := exec.Command(path, "i")
	procutil.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("7z not runnable: %w\n%s", err, stderr.String())
	}
	return nil
}

// ensure7zExtractor returns a 7-Zip binary for bootstrap extraction.
// Prefers bin/7z.exe when present; otherwise downloads minimal 7zr.exe.
func (b *Bootstrap) ensure7zExtractor(destDir string) (string, error) {
	sevenZ := filepath.Join(destDir, "7z.exe")
	if _, err := os.Stat(sevenZ); err == nil {
		if err := verify7zRunnable(sevenZ); err == nil {
			return sevenZ, nil
		}
	}
	return b.ensure7zr(destDir)
}

func (b *Bootstrap) ensure7zr(destDir string) (string, error) {
	sevenZipR := filepath.Join(destDir, "7zr.exe")
	if _, err := os.Stat(sevenZipR); err == nil {
		if err := verify7zRunnable(sevenZipR); err == nil {
			return sevenZipR, nil
		}
	}

	verbose.Progressf("Bootstrapping: downloading 7zr.exe...\n")
	urls := []string{
		"https://www.7-zip.org/a/7zr.exe",
		"https://mirror.7-zip.org/a/7zr.exe",
	}
	data, err := b.downloadWithFallback(context.Background(), urls)
	if err != nil {
		return "", fmt.Errorf("download 7zr: %w", err)
	}
	if err := os.WriteFile(sevenZipR, data, 0755); err != nil {
		return "", fmt.Errorf("write 7zr.exe: %w", err)
	}
	verbose.Progressf("  Downloaded 7zr.exe\n")
	return sevenZipR, nil
}

func extractWith7z(extractor, archive, outDir string) error {
	cmd := exec.Command(extractor, "x", "-o"+outDir, "-y", archive)
	procutil.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract %s: %w\n%s", filepath.Base(archive), err, stderr.String())
	}
	return nil
}

func removeBootstrapFile(path string) {
	for i := 0; i < 5; i++ {
		err := os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(path)
}

// CleanupSevenZipSeeds removes bootstrap-only files when a usable 7z.exe is present.
func (b *Bootstrap) CleanupSevenZipSeeds() {
	b.cleanupFull7zipArtifacts()
	b.cleanupBootstrapArtifacts()
}

// cleanupBootstrapArtifacts removes seed files after minimal 7z.exe (7za) is ready.
func (b *Bootstrap) cleanupBootstrapArtifacts() {
	bin := b.binDir()
	sevenZ := filepath.Join(bin, "7z.exe")
	if _, err := os.Stat(sevenZ); err != nil {
		return
	}
	if err := verify7zRunnable(sevenZ); err != nil {
		return
	}
	for _, name := range []string{"7zr.exe", "7z_extra.7z"} {
		removeBootstrapFile(filepath.Join(bin, name))
	}
}

// cleanupFull7zipArtifacts removes bootstrap intermediates once 7z.exe + 7z.dll are present.
func (b *Bootstrap) cleanupFull7zipArtifacts() {
	if !b.hasFull7zip() {
		return
	}
	bin := b.binDir()
	for _, name := range []string{"7zr.exe", "7z_extra.7z", "7z-installer.exe"} {
		removeBootstrapFile(filepath.Join(bin, name))
	}
}

// Ensure7z ensures 7z.exe is available in glue's bin directory
// Downloads the full 7-Zip console version (7za.exe) which supports .zip, .7z, .rar, .tar, .gz, etc.
func (b *Bootstrap) Ensure7z(ctx context.Context) (string, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mu := sevenZipLock(b.rootDir)
	mu.Lock()
	defer mu.Unlock()

	// Check bootstrapped version in glue bin directory
	bootstrappedPath := filepath.Join(b.binDir(), "7z.exe")
	if _, err := os.Stat(bootstrappedPath); err == nil {
		b.cleanupBootstrapArtifacts()
		return bootstrappedPath, nil
	}

	// Download full 7-Zip from 7-Zip Extra package
	return b.download7z(ctx)
}

// download7z downloads the full 7-Zip (7za.exe) which supports .zip and other formats
// Process: download 7zr.exe -> download 7z_extra.7z -> extract 7za.exe with 7zr.exe
func (b *Bootstrap) download7z(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	destDir := b.binDir()
	os.MkdirAll(destDir, 0755)

	destPath := filepath.Join(destDir, "7z.exe")

	// Step 1: Download 7-Zip Extra .7z package (contains 7za.exe that supports .zip)
	extra7z := filepath.Join(destDir, "7z_extra.7z")
	urls := []string{
		"https://www.7-zip.org/a/7z2601-extra.7z",
		"https://mirror.7-zip.org/a/7z2601-extra.7z",
	}

	if _, err := os.Stat(extra7z); os.IsNotExist(err) {
		verbose.Progressf("Bootstrapping: downloading 7-Zip Extra...\n")

		data, err := b.downloadWithFallback(ctx, urls)
		if err != nil {
			return "", fmt.Errorf("download 7z extra: %w", err)
		}
		if err := os.WriteFile(extra7z, data, 0755); err != nil {
			return "", fmt.Errorf("write 7z_extra.7z: %w", err)
		}
		verbose.Progressf("  Downloaded 7-Zip Extra\n")
	}

	// Step 2: Extract 7za.exe from the Extra package
	verbose.Progressf("Bootstrapping: extracting 7za.exe...\n")

	tempDir := filepath.Join(destDir, "temp_extract")
	os.MkdirAll(tempDir, 0755)
	defer os.RemoveAll(tempDir)

	extractor, err := b.ensure7zExtractor(destDir)
	if err != nil {
		return "", err
	}
	if err := extractWith7z(extractor, extra7z, tempDir); err != nil {
		return "", fmt.Errorf("extract 7z_extra: %w", err)
	}

	// Find 7za.exe in the extracted directory
	var extracted7z string
	for _, pattern := range []string{
		filepath.Join(tempDir, "7za.exe"),
		filepath.Join(tempDir, "*", "7za.exe"),
	} {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			extracted7z = matches[0]
			break
		}
	}

	if extracted7z == "" {
		return "", fmt.Errorf("7za.exe not found in extracted archive")
	}

	// Copy 7za.exe as 7z.exe
	data, err := os.ReadFile(extracted7z)
	if err != nil {
		return "", fmt.Errorf("read 7za.exe: %w", err)
	}
	if err := os.WriteFile(destPath, data, 0755); err != nil {
		return "", fmt.Errorf("write 7z.exe: %w", err)
	}

	verbose.Progressf("  Bootstrapped 7z.exe\n")
	b.cleanupBootstrapArtifacts()
	return destPath, nil
}

// Ensure7zip ensures the full 7-Zip console build is available (7z.exe + 7z.dll).
// 7-Zip 26+ bundles NSIS/ISO/RAR codecs in 7z.dll; older installers may ship codecs/*.dll.
func (b *Bootstrap) Ensure7zip(ctx context.Context) (string, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mu := sevenZipLock(b.rootDir)
	mu.Lock()
	defer mu.Unlock()

	bootstrappedPath := filepath.Join(b.binDir(), "7z.exe")
	if b.hasFull7zip() {
		b.cleanupFull7zipArtifacts()
		return bootstrappedPath, nil
	}
	return b.downloadFull7zip(ctx)
}

// hasFull7zip reports whether the full installer build is present (not the minimal 7za bootstrap).
func (b *Bootstrap) hasFull7zip() bool {
	bin := b.binDir()
	if _, err := os.Stat(filepath.Join(bin, "7z.exe")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(bin, "7z.dll")); err != nil {
		return false
	}
	return true
}

// downloadFull7zip downloads the full 7-Zip installer and extracts it with all codecs
func (b *Bootstrap) downloadFull7zip(ctx context.Context) (string, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return "", err
	}
	destDir := b.binDir()
	os.MkdirAll(destDir, 0755)

	destPath := filepath.Join(destDir, "7z.exe")
	installerPath := filepath.Join(destDir, "7z-installer.exe")
	codecsDir := filepath.Join(destDir, "codecs")

	// Download 7-Zip installer (x64 version with all codecs)
	if _, err := os.Stat(installerPath); os.IsNotExist(err) {
		verbose.Progressf("Bootstrapping: downloading full 7-Zip installer...\n")

		urls := []string{
			"https://www.7-zip.org/a/7z2601-x64.exe",
			"https://mirror.7-zip.org/a/7z2601-x64.exe",
		}
		data, err := b.downloadWithFallback(ctx, urls)
		if err != nil {
			return "", fmt.Errorf("download 7z installer: %w", err)
		}

		if err := os.WriteFile(installerPath, data, 0755); err != nil {
			return "", fmt.Errorf("write 7z installer: %w", err)
		}
		verbose.Progressf("  Downloaded 7-Zip installer\n")
	}

	// Extract 7z.exe from the installer
	// 7-Zip installer is a self-extracting archive
	verbose.Progressf("Bootstrapping: extracting 7-Zip...\n")

	tempDir := filepath.Join(destDir, "temp_7zip_extract")
	os.MkdirAll(tempDir, 0755)
	defer os.RemoveAll(tempDir)

	extractor, err := b.ensure7zExtractor(destDir)
	if err != nil {
		return "", err
	}
	if err := extractWith7z(extractor, installerPath, tempDir); err != nil {
		return "", fmt.Errorf("extract 7z installer: %w", err)
	}

	// Find 7z.exe and related files in the extracted directory
	var found7z string
	var foundDir string
	for _, pattern := range []string{
		filepath.Join(tempDir, "7z.exe"),
		filepath.Join(tempDir, "*", "7z.exe"),
	} {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			found7z = matches[0]
			foundDir = filepath.Dir(found7z)
			break
		}
	}

	if found7z == "" {
		return "", fmt.Errorf("7z.exe not found in extracted installer")
	}

	// Copy 7z.exe
	data, err := os.ReadFile(found7z)
	if err != nil {
		return "", fmt.Errorf("read 7z.exe: %w", err)
	}
	if err := os.WriteFile(destPath, data, 0755); err != nil {
		return "", fmt.Errorf("write 7z.exe: %w", err)
	}

	sevenZipDll := filepath.Join(foundDir, "7z.dll")
	if _, err := os.Stat(sevenZipDll); err != nil {
		return "", fmt.Errorf("7z.dll not found in extracted installer")
	}
	dllData, err := os.ReadFile(sevenZipDll)
	if err != nil {
		return "", fmt.Errorf("read 7z.dll: %w", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, "7z.dll"), dllData, 0755); err != nil {
		return "", fmt.Errorf("write 7z.dll: %w", err)
	}

	codecCount := copyCodecDLLs(filepath.Join(foundDir, "codecs"), codecsDir)
	if codecCount == 0 {
		// Older layouts may nest codecs elsewhere in the SFX payload.
		_ = filepath.WalkDir(tempDir, func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || path == tempDir {
				return err
			}
			if strings.EqualFold(d.Name(), "codecs") {
				codecCount += copyCodecDLLs(path, codecsDir)
			}
			return nil
		})
	}
	if codecCount == 0 {
		_ = os.RemoveAll(codecsDir)
	}

	verbose.Progressf("  Bootstrapped full 7-Zip\n")
	b.cleanupFull7zipArtifacts()
	return destPath, nil
}

func copyCodecDLLs(srcDir, destDir string) int {
	codecInfos, err := os.ReadDir(srcDir)
	if err != nil {
		return 0
	}
	var copied int
	for _, codecInfo := range codecInfos {
		if codecInfo.IsDir() || !strings.HasSuffix(strings.ToLower(codecInfo.Name()), ".dll") {
			continue
		}
		if err := os.MkdirAll(destDir, 0755); err != nil {
			continue
		}
		srcCodec := filepath.Join(srcDir, codecInfo.Name())
		codecData, err := os.ReadFile(srcCodec)
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(destDir, codecInfo.Name()), codecData, 0755); err != nil {
			continue
		}
		copied++
	}
	return copied
}

// EnsureGit ensures git is available in glue's bin directory
func (b *Bootstrap) EnsureGit(ctx context.Context) (string, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mu := gitLock(b.rootDir)
	mu.Lock()
	defer mu.Unlock()

	gitExe := b.bootstrappedGitPath()
	if GitExecutableReady(gitExe) {
		b.cleanupMinGitZip()
		return gitExe, nil
	}

	return b.downloadMinGit(ctx)
}

// downloadMinGit downloads a minimal Git for Windows distribution
func (b *Bootstrap) downloadMinGit(ctx context.Context) (string, error) {
	binDir := b.binDir()
	os.MkdirAll(binDir, 0755)

	// MinGit URL (small, portable Git for Windows) ??try CDN, GitHub mirrors, then direct.
	npmmirrorURL := "https://registry.npmmirror.com/-/binary/git-for-windows/v2.54.0.windows.1/MinGit-2.54.0-64-bit.zip"
	githubURL := "https://github.com/git-for-windows/git/releases/download/v2.54.0.windows.1/MinGit-2.54.0-64-bit.zip"
	destZip := b.minGitZipPath()
	gitDir := filepath.Join(binDir, "git")
	stagingDir := filepath.Join(binDir, "git.staging")
	gitExe := b.bootstrappedGitPath()

	// Download MinGit with mirror fallback
	verbose.Progressf("Bootstrapping: downloading MinGit...\n")

	urls := []string{npmmirrorURL}
	urls = append(urls, config.MirrorURLs(githubURL, b.ghProxies)...)
	data, err := b.downloadWithFallback(ctx, urls)
	if err != nil {
		return "", fmt.Errorf("download mingit: %w", err)
	}

	if err := os.WriteFile(destZip, data, 0644); err != nil {
		return "", fmt.Errorf("write mingit.zip: %w", err)
	}
	defer b.cleanupMinGitZip()
	verbose.Progressf("  Downloaded MinGit\n")

	verbose.Progressf("Bootstrapping: extracting MinGit...\n")

	sevenZip, err := b.Ensure7z(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure 7z: %w", err)
	}
	b.sevenZip = sevenZip

	os.RemoveAll(stagingDir)
	os.MkdirAll(stagingDir, 0755)
	defer os.RemoveAll(stagingDir)

	cmd := exec.CommandContext(ctx, sevenZip, "x", "-o"+stagingDir, "-y", destZip)
	procutil.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("extract mingit: %w\n%s", err, stderr.String())
	}

	stagingGitExe := filepath.Join(stagingDir, "mingw64", "bin", "git.exe")
	if _, err := os.Stat(stagingGitExe); err != nil {
		return "", fmt.Errorf("git.exe not found in staging extract")
	}

	if err := os.RemoveAll(gitDir); err != nil {
		return "", fmt.Errorf("remove old git dir: %w", err)
	}
	if err := os.Rename(stagingDir, gitDir); err != nil {
		return "", fmt.Errorf("install git: %w", err)
	}

	if !GitExecutableReady(gitExe) {
		return "", fmt.Errorf("git.exe not runnable after extraction")
	}

	verbose.Progressf("  Bootstrapped MinGit to %s\n", gitDir)
	return gitExe, nil
}

const darkBootstrapURL = "https://raw.githubusercontent.com/ScoopInstaller/Binary/master/dark/dark-3.14.1.zip"

func (b *Bootstrap) bootstrappedDarkPath() string {
	return filepath.Join(b.binDir(), "wix", "dark.exe")
}

func (b *Bootstrap) hasBootstrappedDark() bool {
	wixDir := filepath.Join(b.binDir(), "wix")
	darkPath := filepath.Join(wixDir, "dark.exe")
	if _, err := os.Stat(darkPath); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(wixDir, "wix.dll")); err != nil {
		return false
	}
	return true
}

// EnsureDark ensures the WiX 3 toolset (dark.exe + wix.dll) is available under glue/bin/wix.
func (b *Bootstrap) EnsureDark(ctx context.Context) (string, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mu := darkLock(b.rootDir)
	mu.Lock()
	defer mu.Unlock()

	if b.hasBootstrappedDark() {
		return b.bootstrappedDarkPath(), nil
	}
	return b.downloadDark(ctx)
}

func (b *Bootstrap) downloadDark(ctx context.Context) (string, error) {
	binDir := b.binDir()
	wixDir := filepath.Join(binDir, "wix")
	os.MkdirAll(binDir, 0755)

	destPath := b.bootstrappedDarkPath()
	destZip := filepath.Join(binDir, "dark.zip")

	verbose.Progressf("Bootstrapping: downloading WiX toolset (dark)...\n")

	urls := config.MirrorURLs(darkBootstrapURL, b.ghProxies)
	data, err := b.downloadWithFallback(ctx, urls)
	if err != nil {
		return "", fmt.Errorf("download dark: %w", err)
	}

	if err := os.WriteFile(destZip, data, 0644); err != nil {
		return "", fmt.Errorf("write dark.zip: %w", err)
	}
	verbose.Progressf("  Downloaded WiX dark\n")

	verbose.Progressf("Bootstrapping: extracting WiX toolset to bin/wix...\n")

	sevenZip, err := b.Ensure7z(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure 7z: %w", err)
	}

	_ = os.Remove(filepath.Join(binDir, "dark.exe"))
	os.RemoveAll(wixDir)
	os.MkdirAll(wixDir, 0755)
	defer os.Remove(destZip)

	if err := extractWith7z(sevenZip, destZip, wixDir); err != nil {
		return "", err
	}

	if !b.hasBootstrappedDark() {
		return "", fmt.Errorf("dark.exe or wix.dll missing after extraction")
	}

	verbose.Progressf("  Bootstrapped WiX toolset to %s\n", wixDir)
	return destPath, nil
}

const innounpBootstrapURL = "https://raw.githubusercontent.com/jrathlev/InnoUnpacker-Windows-GUI/refs/heads/master/innounp-2/bin/innounp-2.zip"

func (b *Bootstrap) bootstrappedInnounpPath() string {
	return filepath.Join(b.binDir(), "innounp", "innounp.exe")
}

func (b *Bootstrap) hasBootstrappedInnounp() bool {
	path := b.bootstrappedInnounpPath()
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// EnsureInnounp ensures innounp.exe is available under glue/bin/innounp.
func (b *Bootstrap) EnsureInnounp(ctx context.Context) (string, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	mu := innounpLock(b.rootDir)
	mu.Lock()
	defer mu.Unlock()

	if b.hasBootstrappedInnounp() {
		return b.bootstrappedInnounpPath(), nil
	}
	return b.downloadInnounp(ctx)
}

func (b *Bootstrap) downloadInnounp(ctx context.Context) (string, error) {
	binDir := b.binDir()
	innounpDir := filepath.Join(binDir, "innounp")
	os.MkdirAll(binDir, 0755)

	destPath := b.bootstrappedInnounpPath()
	destZip := filepath.Join(binDir, "innounp.zip")

	verbose.Progressf("Bootstrapping: downloading innounp...\n")

	urls := config.MirrorURLs(innounpBootstrapURL, b.ghProxies)
	data, err := b.downloadWithFallback(ctx, urls)
	if err != nil {
		return "", fmt.Errorf("download innounp: %w", err)
	}

	if err := os.WriteFile(destZip, data, 0644); err != nil {
		return "", fmt.Errorf("write innounp.zip: %w", err)
	}
	verbose.Progressf("  Downloaded innounp\n")

	verbose.Progressf("Bootstrapping: extracting innounp to bin/innounp...\n")

	sevenZip, err := b.Ensure7z(ctx)
	if err != nil {
		return "", fmt.Errorf("ensure 7z: %w", err)
	}

	os.RemoveAll(innounpDir)
	os.MkdirAll(innounpDir, 0755)
	defer os.Remove(destZip)

	if err := extractWith7z(sevenZip, destZip, innounpDir); err != nil {
		return "", err
	}

	if !b.hasBootstrappedInnounp() {
		return "", fmt.Errorf("innounp.exe missing after extraction")
	}

	verbose.Progressf("  Bootstrapped innounp to %s\n", innounpDir)
	return destPath, nil
}
