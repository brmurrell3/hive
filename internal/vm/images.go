// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
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

// errChecksumUnavailable is a sentinel error indicating the checksum file
// could not be fetched (e.g. 404). This is non-fatal — the download can
// proceed without verification.
var errChecksumUnavailable = errors.New("checksum file unavailable")

// variantPattern validates image variant names (alphanumeric and hyphens only).
var variantPattern = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)

// httpClient is used for all image downloads. It rejects HTTP-downgrade
// redirects (HTTPS -> HTTP) to prevent man-in-the-middle attacks.
var httpClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-HTTPS URL: %s", req.URL.Redacted())
		}
		return nil
	},
}

// ImageManager handles rootfs image caching and download.
type ImageManager struct {
	cacheDir string
	version  string
	logger   *slog.Logger
	mu       sync.Mutex
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
	// CRITICAL-3: Validate variant to prevent path traversal.
	if !variantPattern.MatchString(variant) {
		return "", fmt.Errorf("invalid image variant %q: must match %s", variant, variantPattern.String())
	}

	// HIGH-5: Serialize concurrent calls to prevent races on the cache.
	m.mu.Lock()
	defer m.mu.Unlock()

	arch := runtime.GOARCH
	filename := fmt.Sprintf("hive-rootfs-%s-%s.ext4", variant, arch)
	cachedPath := filepath.Join(m.cacheDir, filename)

	// Check cache first.
	if _, err := os.Stat(cachedPath); err == nil {
		m.logger.Info("using cached rootfs image", "path", cachedPath)
		return cachedPath, nil
	}

	// Ensure cache directory exists.
	// MEDIUM-2: Use 0700 for cache directory permissions.
	if err := os.MkdirAll(m.cacheDir, 0700); err != nil {
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

	// MEDIUM-5: Use os.CreateTemp for unpredictable temp file names.
	tmpFile, err := os.CreateTemp(m.cacheDir, ".download-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close() // downloadFile will reopen it
	defer os.Remove(tmpPath)

	if err := m.downloadFile(ctx, downloadURL, tmpPath); err != nil {
		return "", fmt.Errorf("downloading rootfs image: %w", err)
	}

	// CRITICAL-1: Distinguish between "checksum unavailable" and "checksum mismatch".
	if err := m.validateChecksum(ctx, checksumURL, tmpPath); err != nil {
		if errors.Is(err, errChecksumUnavailable) {
			m.logger.Warn("checksum validation skipped: checksum file not available", "error", err)
		} else {
			// Checksum mismatch or other verification error is fatal.
			return "", fmt.Errorf("checksum verification failed: %w", err)
		}
	}

	// Decompress gzip.
	// MEDIUM-5: Use os.CreateTemp for unpredictable decompression temp file names.
	decompFile, err := os.CreateTemp(m.cacheDir, ".decompress-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating decompression temp file: %w", err)
	}
	decompressedPath := decompFile.Name()
	decompFile.Close() // decompressGzip will reopen it
	defer os.Remove(decompressedPath)
	if err := m.decompressGzip(ctx, tmpPath, decompressedPath); err != nil {
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
func (m *ImageManager) downloadFile(ctx context.Context, rawURL, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	// CRITICAL-2: Validate the initial URL scheme before making the request.
	if parsedURL, parseErr := url.Parse(rawURL); parseErr == nil && parsedURL.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS download URL: %s", rawURL)
	}

	// CRITICAL-2: Use httpClient which rejects HTTPS->HTTP redirect downgrades.
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP GET %s: status %d", rawURL, resp.StatusCode)
	}

	// HIGH-2: Check Content-Length against max before starting the download.
	if resp.ContentLength > maxImageDownloadSize {
		return fmt.Errorf("HTTP GET %s: Content-Length %d exceeds maximum %d", rawURL, resp.ContentLength, maxImageDownloadSize)
	}

	// MEDIUM-1: Use 0600 for file permissions.
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
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
			url:       rawURL,
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
// the computed hash of the given file.
//
// Returns errChecksumUnavailable if the checksum file cannot be fetched (non-fatal).
// Returns a regular error if the checksum is fetched but does not match (fatal).
func (m *ImageManager) validateChecksum(ctx context.Context, checksumURL, filePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return fmt.Errorf("%w: creating checksum request: %v", errChecksumUnavailable, err)
	}

	// CRITICAL-2: Use httpClient for checksum downloads too.
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetching checksum: %v", errChecksumUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", errChecksumUnavailable, resp.StatusCode)
	}

	// Read the checksum file (typically "<hash>  <filename>\n").
	// Limit to 1 KB to prevent abuse.
	checksumData, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return fmt.Errorf("%w: reading checksum: %v", errChecksumUnavailable, err)
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
		// CRITICAL-1: This is a hard error — the file is corrupted or tampered with.
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	m.logger.Info("checksum verified", "file", filePath, "sha256", actualHash)
	return nil
}

// contextReader wraps a reader and checks for context cancellation periodically.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (cr *contextReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.reader.Read(p)
}

// decompressGzip decompresses a gzip-compressed file from src to dest.
// HIGH-4: Accepts context.Context for cancellation support.
func (m *ImageManager) decompressGzip(ctx context.Context, src, dest string) error {
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

	// MEDIUM-1: Use 0600 for file permissions.
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating decompressed file: %w", err)
	}
	defer out.Close()

	// Wrap the gzip reader with context awareness and a size limit.
	// HIGH-4: Use contextReader to check for cancellation during decompression.
	ctxReader := &contextReader{ctx: ctx, reader: gz}
	limitedReader := io.LimitReader(ctxReader, maxImageDownloadSize)

	if _, err := io.Copy(out, limitedReader); err != nil {
		return fmt.Errorf("decompressing: %w", err)
	}

	// HIGH-1: Detect zip bomb — if there is more data after the limit, the
	// output was truncated and the archive is suspiciously large.
	probe := make([]byte, 1)
	ctxProbe := &contextReader{ctx: ctx, reader: gz}
	if n, probeErr := ctxProbe.Read(probe); n > 0 {
		return fmt.Errorf("decompressed data exceeds maximum size (%d bytes): possible zip bomb", maxImageDownloadSize)
	} else if probeErr != nil && probeErr != io.EOF {
		// Read error that isn't EOF — could indicate corruption.
		return fmt.Errorf("checking for truncated decompression: %w", probeErr)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("closing decompressed file: %w", err)
	}

	return nil
}
