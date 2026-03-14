// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	// DefaultKernelCacheDir is the default directory for cached kernel images,
	// relative to the user's home directory.
	DefaultKernelCacheDir = ".hive/kernels"

	// FirecrackerKernelBaseURL is the default base URL for downloading
	// pre-built Firecracker-compatible Linux kernels.
	FirecrackerKernelBaseURL = "https://github.com/firecracker-microvm/firecracker/releases"

	// DefaultFirecrackerVersion is the Firecracker release version used when
	// no explicit version is provided. This determines which kernel build is
	// downloaded from GitHub releases.
	DefaultFirecrackerVersion = "v1.6.0"

	// DefaultKernelLinuxVersion is the Linux kernel version suffix used in
	// the Firecracker release artifact filename.
	DefaultKernelLinuxVersion = "5.10"

	// maxKernelDownloadSize is the maximum size for a downloaded kernel file (256 MB).
	// Firecracker kernels are typically 20-30 MB; this limit prevents unbounded
	// disk usage from a malicious or corrupted download.
	maxKernelDownloadSize int64 = 256 * 1024 * 1024
)

// archToFirecrackerArch maps Go's runtime.GOARCH to the Firecracker release
// artifact architecture suffix.
var archToFirecrackerArch = map[string]string{
	"amd64": "x86_64",
	"arm64": "aarch64",
}

// KernelManager handles kernel image resolution, caching, and download.
// It follows the same patterns as ImageManager for rootfs images.
type KernelManager struct {
	cacheDir           string
	firecrackerVersion string
	kernelVersion      string
	imageURL           string // custom download URL (for air-gapped / mirror deployments)
	logger             *slog.Logger
	mu                 sync.Mutex
}

// KernelManagerConfig holds configuration for creating a KernelManager.
type KernelManagerConfig struct {
	// CacheDir overrides the default kernel cache directory (~/.hive/kernels).
	CacheDir string

	// FirecrackerVersion is the Firecracker release version (e.g., "v1.6.0").
	// Defaults to DefaultFirecrackerVersion if empty.
	FirecrackerVersion string

	// KernelVersion is the Linux kernel version suffix (e.g., "5.10").
	// Defaults to DefaultKernelLinuxVersion if empty.
	KernelVersion string

	// ImageURL overrides the download URL entirely. When set, the manager
	// downloads the kernel from this URL instead of constructing one from
	// the Firecracker release page. This supports air-gapped environments
	// with local file servers or internal mirrors.
	//
	// The URL must use the "https" scheme unless it is a "file://" URL for
	// local paths. Examples:
	//   - https://internal-mirror.corp.example/kernels/vmlinux-5.10-x86_64.bin
	//   - file:///opt/hive/kernels/vmlinux
	ImageURL string

	Logger *slog.Logger
}

// NewKernelManager creates a new KernelManager with the given configuration.
func NewKernelManager(cfg KernelManagerConfig) *KernelManager {
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			cacheDir = filepath.Join(os.TempDir(), DefaultKernelCacheDir)
		} else {
			cacheDir = filepath.Join(home, DefaultKernelCacheDir)
		}
	}

	fcVersion := cfg.FirecrackerVersion
	if fcVersion == "" {
		fcVersion = DefaultFirecrackerVersion
	}

	kernelVersion := cfg.KernelVersion
	if kernelVersion == "" {
		kernelVersion = DefaultKernelLinuxVersion
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &KernelManager{
		cacheDir:           cacheDir,
		firecrackerVersion: fcVersion,
		kernelVersion:      kernelVersion,
		imageURL:           cfg.ImageURL,
		logger:             logger,
	}
}

// EnsureKernel returns the path to a usable kernel image. It searches the
// following locations in order:
//
//  1. {clusterRoot}/rootfs/vmlinux        — local build / manual placement
//  2. ~/.hive/kernels/vmlinux-{arch}      — cached download
//  3. Download from GitHub releases       — auto-download with SHA-256 verification
//
// The clusterRoot parameter is the absolute path to the cluster root directory.
// If empty, step 1 is skipped.
func (km *KernelManager) EnsureKernel(ctx context.Context, clusterRoot string) (string, error) {
	// Serialize concurrent calls to prevent races on the cache.
	km.mu.Lock()
	defer km.mu.Unlock()

	arch := runtime.GOARCH

	// Step 1: Check local cluster root.
	if clusterRoot != "" {
		localKernel := filepath.Join(clusterRoot, "rootfs", "vmlinux")
		if _, err := os.Stat(localKernel); err == nil {
			km.logger.Info("using local kernel image", "path", localKernel)
			return localKernel, nil
		}
	}

	// Step 2: Check cache.
	cachedFilename := fmt.Sprintf("vmlinux-%s", arch)
	cachedPath := filepath.Join(km.cacheDir, cachedFilename)
	checksumSidecar := cachedPath + ".sha256"

	if _, err := os.Stat(cachedPath); err == nil {
		if err := km.verifyCachedChecksum(cachedPath, checksumSidecar); err != nil {
			km.logger.Warn("cached kernel integrity check failed, re-downloading",
				"path", cachedPath, "error", err)
			os.Remove(cachedPath)
			os.Remove(checksumSidecar)
		} else {
			km.logger.Info("using cached kernel image", "path", cachedPath)
			return cachedPath, nil
		}
	}

	// Step 3: Download from GitHub releases (or custom URL).
	km.logger.Info("no local or cached kernel found, attempting download",
		"arch", arch)

	// Ensure cache directory exists.
	if err := os.MkdirAll(km.cacheDir, 0700); err != nil {
		return "", fmt.Errorf("creating kernel cache directory: %w", err)
	}

	downloadURL, checksumURL, err := km.buildDownloadURLs(arch)
	if err != nil {
		return "", fmt.Errorf("building kernel download URLs: %w", err)
	}

	km.logger.Info("downloading kernel image",
		"url", downloadURL,
		"arch", arch,
	)

	// Download to a temp file first.
	tmpFile, err := os.CreateTemp(km.cacheDir, ".kernel-download-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	if err := km.downloadKernel(ctx, downloadURL, tmpPath); err != nil {
		return "", fmt.Errorf("downloading kernel image: %w", err)
	}

	// Validate checksum if available.
	if checksumURL != "" {
		if err := km.validateKernelChecksum(ctx, checksumURL, tmpPath); err != nil {
			// Checksum validation failure is fatal for kernels — a tampered
			// kernel is a critical security risk.
			return "", fmt.Errorf("kernel checksum verification failed: %w", err)
		}
	} else {
		km.logger.Warn("no checksum URL available for kernel; skipping verification")
	}

	// Validate that the downloaded file looks like an ELF binary.
	if err := validateELFMagic(tmpPath); err != nil {
		return "", fmt.Errorf("downloaded file is not a valid kernel image: %w", err)
	}

	// Compute SHA-256 of the kernel for cache integrity verification.
	kernelHash, err := computeFileSHA256(tmpPath)
	if err != nil {
		return "", fmt.Errorf("computing kernel checksum: %w", err)
	}

	// Atomic rename into cache.
	if err := os.Rename(tmpPath, cachedPath); err != nil {
		return "", fmt.Errorf("moving kernel to cache: %w", err)
	}

	// Write the SHA-256 sidecar file for future cache-hit verification.
	if err := os.WriteFile(checksumSidecar, []byte(kernelHash+"\n"), 0600); err != nil {
		km.logger.Warn("failed to write kernel checksum sidecar", "path", checksumSidecar, "error", err)
	}

	km.logger.Info("kernel image cached", "path", cachedPath, "sha256", kernelHash)
	return cachedPath, nil
}

// CacheDir returns the kernel cache directory.
func (km *KernelManager) CacheDir() string {
	return km.cacheDir
}

// buildDownloadURLs constructs the kernel download URL and optional checksum
// URL based on the architecture and configuration.
func (km *KernelManager) buildDownloadURLs(arch string) (downloadURL, checksumURL string, err error) {
	// If a custom ImageURL is configured, use it directly.
	if km.imageURL != "" {
		parsed, parseErr := url.Parse(km.imageURL)
		if parseErr != nil {
			return "", "", fmt.Errorf("invalid custom image URL %q: %w", km.imageURL, parseErr)
		}
		// Allow file:// for local paths and https:// for remote.
		if parsed.Scheme != "https" && parsed.Scheme != "file" {
			return "", "", fmt.Errorf("custom image URL must use https:// or file:// scheme, got %q", parsed.Scheme)
		}
		// No checksum URL for custom URLs — the operator is responsible for
		// verifying integrity of custom kernel images.
		return km.imageURL, "", nil
	}

	// Map Go arch to Firecracker release artifact arch.
	fcArch, ok := archToFirecrackerArch[arch]
	if !ok {
		return "", "", fmt.Errorf("unsupported architecture %q: expected amd64 or arm64", arch)
	}

	// Build URL: e.g., https://github.com/firecracker-microvm/firecracker/releases/download/v1.6.0/vmlinux-5.10-x86_64.bin
	filename := fmt.Sprintf("vmlinux-%s-%s.bin", km.kernelVersion, fcArch)
	downloadURL = fmt.Sprintf("%s/download/%s/%s",
		FirecrackerKernelBaseURL, km.firecrackerVersion, filename)

	// Firecracker releases include SHA-256 checksum files with the same name + .sha256.txt suffix.
	checksumURL = downloadURL + ".sha256.txt"

	return downloadURL, checksumURL, nil
}

// downloadKernel downloads a kernel image from the given URL to destPath.
// It supports both https:// and file:// URLs.
func (km *KernelManager) downloadKernel(ctx context.Context, rawURL, destPath string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	if parsed.Scheme == "file" {
		return km.copyLocalFile(parsed.Path, destPath)
	}

	// HTTPS download — reuse the same httpClient as ImageManager for
	// consistent TLS and redirect policies.
	if parsed.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS download URL: %s", rawURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP GET %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP GET %s: status %d", rawURL, resp.StatusCode)
	}

	if resp.ContentLength > maxKernelDownloadSize {
		return fmt.Errorf("HTTP GET %s: Content-Length %d exceeds maximum %d",
			rawURL, resp.ContentLength, maxKernelDownloadSize)
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", destPath, err)
	}

	body := io.LimitReader(resp.Body, maxKernelDownloadSize)

	var written int64
	if resp.ContentLength > 0 {
		pr := &progressReader{
			reader:    body,
			totalSize: resp.ContentLength,
			logger:    km.logger,
			url:       rawURL,
		}
		written, err = io.Copy(out, pr)
		if err != nil {
			out.Close()
			return fmt.Errorf("writing to %s: %w", destPath, err)
		}
		if written != resp.ContentLength {
			out.Close()
			return fmt.Errorf("incomplete download of %s: wrote %d bytes, expected %d",
				rawURL, written, resp.ContentLength)
		}
	} else {
		if _, err := io.Copy(out, body); err != nil {
			out.Close()
			return fmt.Errorf("writing to %s: %w", destPath, err)
		}
	}

	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("syncing %s: %w", destPath, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", destPath, err)
	}

	return nil
}

// copyLocalFile copies a local file from src to dest. This supports the
// file:// URL scheme for air-gapped deployments where the kernel is stored
// on a local or mounted filesystem.
func (km *KernelManager) copyLocalFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening local kernel file %s: %w", src, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat local kernel file %s: %w", src, err)
	}
	if info.Size() > maxKernelDownloadSize {
		return fmt.Errorf("local kernel file %s (%d bytes) exceeds maximum size %d",
			src, info.Size(), maxKernelDownloadSize)
	}
	if info.Size() == 0 {
		return fmt.Errorf("local kernel file %s is empty", src)
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating destination file %s: %w", dest, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copying kernel file: %w", err)
	}

	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("syncing %s: %w", dest, err)
	}
	return out.Close()
}

// validateKernelChecksum downloads a SHA-256 checksum and verifies the
// downloaded kernel file against it.
func (km *KernelManager) validateKernelChecksum(ctx context.Context, checksumURL, filePath string) error {
	parsed, err := url.Parse(checksumURL)
	if err != nil {
		return fmt.Errorf("invalid checksum URL %q: %w", checksumURL, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS checksum URL: %s", checksumURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return fmt.Errorf("creating checksum request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching checksum: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Checksum file not available — this may be expected for some
		// Firecracker releases. Log a warning and skip verification.
		km.logger.Warn("kernel checksum file not available",
			"url", checksumURL,
			"status", resp.StatusCode)
		return nil
	}

	// Read the checksum file (limit to 1 KB).
	checksumData, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return fmt.Errorf("reading checksum data: %w", err)
	}

	// Parse expected hash — support formats:
	//   1. "<hash>" (hash only)
	//   2. "<hash>  <filename>" (sha256sum format)
	checksumStr := strings.TrimSpace(string(checksumData))
	if checksumStr == "" {
		return fmt.Errorf("empty checksum file")
	}

	var expectedHash string
	fields := strings.Fields(checksumStr)
	if len(fields) >= 1 {
		expectedHash = strings.ToLower(fields[0])
	}

	if len(expectedHash) != 64 {
		return fmt.Errorf("invalid checksum format (expected 64 hex chars, got %d)", len(expectedHash))
	}
	if _, err := hex.DecodeString(expectedHash); err != nil {
		return fmt.Errorf("invalid checksum hex encoding: %w", err)
	}

	// Compute SHA-256 of the downloaded kernel file.
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening kernel for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("computing kernel checksum: %w", err)
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}

	km.logger.Info("kernel checksum verified", "file", filePath, "sha256", actualHash)
	return nil
}

// verifyCachedChecksum verifies that a cached kernel file matches the SHA-256
// hash stored in the sidecar checksum file.
func (km *KernelManager) verifyCachedChecksum(kernelPath, checksumPath string) error {
	expectedBytes, err := os.ReadFile(checksumPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No sidecar file — compute and store one for future verification.
			km.logger.Info("no checksum sidecar found for cached kernel, computing one",
				"path", kernelPath)
			hash, hashErr := computeFileSHA256(kernelPath)
			if hashErr != nil {
				return fmt.Errorf("computing checksum for cached kernel: %w", hashErr)
			}
			if writeErr := os.WriteFile(checksumPath, []byte(hash+"\n"), 0600); writeErr != nil {
				km.logger.Warn("failed to write kernel checksum sidecar",
					"path", checksumPath, "error", writeErr)
			}
			return nil // Trust the file this time.
		}
		return fmt.Errorf("reading kernel checksum sidecar: %w", err)
	}

	expectedHash := strings.TrimSpace(string(expectedBytes))
	if len(expectedHash) != 64 {
		return fmt.Errorf("invalid sidecar checksum format (expected 64 hex chars, got %d)",
			len(expectedHash))
	}

	actualHash, err := computeFileSHA256(kernelPath)
	if err != nil {
		return fmt.Errorf("computing cached kernel checksum: %w", err)
	}

	if actualHash != expectedHash {
		return fmt.Errorf("cached kernel integrity failure: expected %s, got %s",
			expectedHash, actualHash)
	}

	return nil
}

// validateELFMagic checks that the file at path begins with the ELF magic
// number (0x7f 'E' 'L' 'F'). This is a minimal sanity check to ensure the
// downloaded file is a valid ELF binary (Linux kernel) and not HTML, JSON,
// or other unexpected content.
func validateELFMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	magic := make([]byte, 4)
	n, err := f.Read(magic)
	if err != nil {
		return fmt.Errorf("reading file header: %w", err)
	}
	if n < 4 {
		return fmt.Errorf("file too small (%d bytes) to be a valid ELF binary", n)
	}

	// ELF magic: 0x7f, 'E', 'L', 'F'
	if magic[0] != 0x7f || magic[1] != 'E' || magic[2] != 'L' || magic[3] != 'F' {
		return fmt.Errorf("invalid ELF magic: got %02x %02x %02x %02x, expected 7f 45 4c 46",
			magic[0], magic[1], magic[2], magic[3])
	}

	return nil
}
