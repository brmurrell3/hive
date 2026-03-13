// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// DefaultImageCacheDir is the default directory for cached rootfs images,
	// relative to the user's home directory.
	DefaultImageCacheDir = ".hive/images"

	// GitHubReleasesBaseURL is the base URL for downloading rootfs images.
	GitHubReleasesBaseURL = "https://github.com/brmurrell3/hive/releases"

	// maxImageDownloadSize is the maximum size for a downloaded image file (4 GB).
	// This prevents unbounded memory/disk usage from a malicious or corrupted download.
	maxImageDownloadSize = 4 * 1024 * 1024 * 1024

	// progressLogInterval controls how often download progress is logged.
	// Progress is logged every 10% of total content length.
	progressLogInterval = 10
)

// ImageManager handles rootfs image caching and download.
type ImageManager struct {
	cacheDir string
	version  string
	logger   *slog.Logger
}

// NewImageManager creates a new ImageManager.
// cacheDir defaults to ~/.hive/images if empty.
// version is the hived version (used to select the correct release).
func NewImageManager(cacheDir, version string, logger *slog.Logger) *ImageManager {
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			cacheDir = filepath.Join(os.TempDir(), DefaultImageCacheDir)
		} else {
			cacheDir = filepath.Join(home, DefaultImageCacheDir)
		}
	}
	return &ImageManager{
		cacheDir: cacheDir,
		version:  version,
		logger:   logger,
	}
}

// EnsureImage returns the path to a cached rootfs image, downloading it if necessary.
// variant is typically "base" or "openclaw". The architecture is determined
// automatically from runtime.GOARCH.
func (m *ImageManager) EnsureImage(ctx context.Context, variant string) (string, error) {
	arch := runtime.GOARCH
	filename := fmt.Sprintf("hive-rootfs-%s-%s.ext4", variant, arch)
	cachedPath := filepath.Join(m.cacheDir, filename)

	// Check cache first.
	if _, err := os.Stat(cachedPath); err == nil {
		m.logger.Info("using cached rootfs image", "path", cachedPath)
		return cachedPath, nil
	}

	// Ensure cache directory exists.
	if err := os.MkdirAll(m.cacheDir, 0755); err != nil {
		return "", fmt.Errorf("creating image cache directory: %w", err)
	}

	// Build download URL.
	tag := m.version
	if tag == "" || tag == "dev" {
		tag = "latest"
	}
	compressedFilename := filename + ".gz"
	downloadURL := fmt.Sprintf("%s/download/%s/%s", GitHubReleasesBaseURL, tag, compressedFilename)
	checksumURL := fmt.Sprintf("%s/download/%s/%s.sha256", GitHubReleasesBaseURL, tag, compressedFilename)

	m.logger.Info("downloading rootfs image",
		"url", downloadURL,
		"variant", variant,
		"arch", arch,
	)

	// Download to a temp file first, then rename for atomicity.
	tmpPath := cachedPath + ".tmp"
	defer os.Remove(tmpPath)

	if err := m.downloadFile(ctx, downloadURL, tmpPath); err != nil {
		return "", fmt.Errorf("downloading rootfs image: %w", err)
	}

	// Try to validate checksum (non-fatal if checksum file doesn't exist).
	if err := m.validateChecksum(ctx, checksumURL, tmpPath); err != nil {
		m.logger.Warn("checksum validation skipped or failed", "error", err)
	}

	// Decompress gzip.
	decompressedPath := cachedPath + ".decompressing"
	defer os.Remove(decompressedPath)
	if err := m.decompressGzip(tmpPath, decompressedPath); err != nil {
		return "", fmt.Errorf("decompressing rootfs image: %w", err)
	}

	// Atomic rename into place.
	if err := os.Rename(decompressedPath, cachedPath); err != nil {
		return "", fmt.Errorf("moving rootfs image to cache: %w", err)
	}

	m.logger.Info("rootfs image cached", "path", cachedPath)
	return cachedPath, nil
}

// CacheDir returns the image cache directory.
func (m *ImageManager) CacheDir() string {
	return m.cacheDir
}

// downloadFile performs an HTTP GET to url and writes the response body to destPath.
// It respects context cancellation and logs progress at 10% intervals when the
// server provides a Content-Length header.
func (m *ImageManager) downloadFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP GET %s: status %d", url, resp.StatusCode)
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", destPath, err)
	}
	defer out.Close()

	// Use a limited reader to prevent writing unbounded data.
	body := io.LimitReader(resp.Body, maxImageDownloadSize)

	totalSize := resp.ContentLength
	if totalSize > 0 {
		// Wrap reader to log progress at 10% intervals.
		pr := &progressReader{
			reader:    body,
			totalSize: totalSize,
			logger:    m.logger,
			url:       url,
		}
		if _, err := io.Copy(out, pr); err != nil {
			return fmt.Errorf("writing to %s: %w", destPath, err)
		}
	} else {
		if _, err := io.Copy(out, body); err != nil {
			return fmt.Errorf("writing to %s: %w", destPath, err)
		}
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", destPath, err)
	}

	return nil
}

// progressReader wraps an io.Reader and logs download progress at 10% intervals.
type progressReader struct {
	reader       io.Reader
	totalSize    int64
	bytesRead    int64
	lastLoggedPc int // last logged percentage (in increments of progressLogInterval)
	logger       *slog.Logger
	url          string
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.bytesRead += int64(n)

	if pr.totalSize > 0 {
		pct := int(pr.bytesRead * 100 / pr.totalSize)
		logAt := (pct / progressLogInterval) * progressLogInterval
		if logAt > pr.lastLoggedPc && logAt > 0 {
			pr.lastLoggedPc = logAt
			pr.logger.Info("download progress",
				"url", pr.url,
				"percent", logAt,
				"bytes", pr.bytesRead,
				"total", pr.totalSize,
			)
		}
	}

	return n, err
}

// validateChecksum downloads a SHA-256 checksum file and compares it against
// the computed hash of the given file. Returns an error if the checksum does
// not match or if the checksum file cannot be retrieved.
func (m *ImageManager) validateChecksum(ctx context.Context, checksumURL, filePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return fmt.Errorf("creating checksum request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching checksum: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum file not available (HTTP %d)", resp.StatusCode)
	}

	// Read the checksum file (typically "<hash>  <filename>\n").
	// Limit to 1 KB to prevent abuse.
	checksumData, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return fmt.Errorf("reading checksum: %w", err)
	}

	// Parse the expected hash: take the first whitespace-delimited field.
	checksumStr := strings.TrimSpace(string(checksumData))
	fields := strings.Fields(checksumStr)
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum file")
	}
	expectedHash := strings.ToLower(fields[0])

	// Validate hex format.
	if len(expectedHash) != 64 {
		return fmt.Errorf("invalid checksum format (expected 64 hex chars, got %d)", len(expectedHash))
	}

	// Compute SHA-256 of the downloaded file.
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening file for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("computing checksum: %w", err)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	m.logger.Info("checksum verified", "file", filePath, "sha256", actualHash)
	return nil
}

// decompressGzip decompresses a gzip-compressed file from src to dest.
func (m *ImageManager) decompressGzip(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening compressed file: %w", err)
	}
	defer in.Close()

	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gz.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("creating decompressed file: %w", err)
	}
	defer out.Close()

	// Limit decompressed size to prevent zip bombs.
	if _, err := io.Copy(out, io.LimitReader(gz, maxImageDownloadSize)); err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("closing decompressed file: %w", err)
	}

	return nil
}
