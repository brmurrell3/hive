// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package vm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/brmurrell3/hive/internal/state"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---------------------------------------------------------------------------
// NewImageManager defaults
// ---------------------------------------------------------------------------

func TestNewImageManager_DefaultCacheDir(t *testing.T) {
	mgr := NewImageManager("", "v1.0.0", testLogger())

	home, err := os.UserHomeDir()
	if err != nil {
		// If home dir is not available, it should fall back to temp dir.
		expected := filepath.Join(os.TempDir(), DefaultImageCacheDir)
		if mgr.CacheDir() != expected {
			t.Errorf("CacheDir() = %q, want %q", mgr.CacheDir(), expected)
		}
		return
	}

	expected := filepath.Join(home, DefaultImageCacheDir)
	if mgr.CacheDir() != expected {
		t.Errorf("CacheDir() = %q, want %q", mgr.CacheDir(), expected)
	}
}

func TestNewImageManager_ExplicitCacheDir(t *testing.T) {
	dir := t.TempDir()
	mgr := NewImageManager(dir, "v1.0.0", testLogger())

	if mgr.CacheDir() != dir {
		t.Errorf("CacheDir() = %q, want %q", mgr.CacheDir(), dir)
	}
}

func TestNewImageManager_EmptyVersion(t *testing.T) {
	mgr := NewImageManager(t.TempDir(), "", testLogger())
	// Should not panic or fail with empty version.
	if mgr.version != "" {
		t.Errorf("version = %q, want empty", mgr.version)
	}
}

func TestNewImageManager_DevVersion(t *testing.T) {
	mgr := NewImageManager(t.TempDir(), "dev", testLogger())
	if mgr.version != "dev" {
		t.Errorf("version = %q, want %q", mgr.version, "dev")
	}
}

// ---------------------------------------------------------------------------
// CacheDir
// ---------------------------------------------------------------------------

func TestCacheDir_ReturnsCorrectPath(t *testing.T) {
	dir := t.TempDir()
	mgr := NewImageManager(dir, "v1.0.0", testLogger())

	if got := mgr.CacheDir(); got != dir {
		t.Errorf("CacheDir() = %q, want %q", got, dir)
	}
}

// ---------------------------------------------------------------------------
// EnsureImage: cached image
// ---------------------------------------------------------------------------

func TestEnsureImage_ReturnsCachedPath(t *testing.T) {
	dir := t.TempDir()
	mgr := NewImageManager(dir, "v1.0.0", testLogger())

	// Pre-create a cached image file.
	arch := runtime.GOARCH
	filename := fmt.Sprintf("hive-rootfs-base-%s.ext4", arch)
	cachedPath := filepath.Join(dir, filename)
	if err := os.WriteFile(cachedPath, []byte("fake-rootfs-image"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := mgr.EnsureImage(context.Background(), "base")
	if err != nil {
		t.Fatalf("EnsureImage: unexpected error: %v", err)
	}
	if got != cachedPath {
		t.Errorf("EnsureImage() = %q, want %q", got, cachedPath)
	}
}

func TestEnsureImage_CachedOpenclawVariant(t *testing.T) {
	dir := t.TempDir()
	mgr := NewImageManager(dir, "v1.0.0", testLogger())

	// Pre-create a cached openclaw image file.
	arch := runtime.GOARCH
	filename := fmt.Sprintf("hive-rootfs-openclaw-%s.ext4", arch)
	cachedPath := filepath.Join(dir, filename)
	if err := os.WriteFile(cachedPath, []byte("fake-openclaw-rootfs"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := mgr.EnsureImage(context.Background(), "openclaw")
	if err != nil {
		t.Fatalf("EnsureImage: unexpected error: %v", err)
	}
	if got != cachedPath {
		t.Errorf("EnsureImage() = %q, want %q", got, cachedPath)
	}
}

// ---------------------------------------------------------------------------
// EnsureImage: download required (will fail because no real server)
// ---------------------------------------------------------------------------

func TestEnsureImage_DownloadFailsGracefully(t *testing.T) {
	dir := t.TempDir()
	// Use an unreachable URL by specifying a non-existent version.
	mgr := NewImageManager(dir, "v0.0.0-nonexistent", testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately so the HTTP request fails fast.

	_, err := mgr.EnsureImage(ctx, "base")
	if err == nil {
		t.Fatal("expected error when download fails, got nil")
	}
}

// ---------------------------------------------------------------------------
// EnsureImage: context cancellation
// ---------------------------------------------------------------------------

func TestEnsureImage_ContextCancelled(t *testing.T) {
	dir := t.TempDir()
	mgr := NewImageManager(dir, "v1.0.0", testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancelled before call.

	_, err := mgr.EnsureImage(ctx, "base")
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

// ---------------------------------------------------------------------------
// resolveBaseRootfs: Manager integration
// ---------------------------------------------------------------------------

func TestManager_ResolveBaseRootfs_Default(t *testing.T) {
	dir := t.TempDir()
	logger := testLogger()
	store, err := newTestStore(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	mock := NewMockHypervisor()
	mgr := NewManager(dir, store, logger, mock, 0, "", 0, 0)

	expected := filepath.Join(dir, "rootfs", "rootfs.ext4")
	if got := mgr.resolveBaseRootfs(); got != expected {
		t.Errorf("resolveBaseRootfs() = %q, want %q", got, expected)
	}
}

func TestManager_ResolveBaseRootfs_Override(t *testing.T) {
	dir := t.TempDir()
	logger := testLogger()
	store, err := newTestStore(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	mock := NewMockHypervisor()
	mgr := NewManager(dir, store, logger, mock, 0, "", 0, 0)

	customPath := filepath.Join(dir, "custom-rootfs.ext4")
	mgr.SetRootfsOverride(customPath)

	if got := mgr.resolveBaseRootfs(); got != customPath {
		t.Errorf("resolveBaseRootfs() = %q, want %q", got, customPath)
	}
}

func TestManager_SetRootfsOverride_Empty(t *testing.T) {
	dir := t.TempDir()
	logger := testLogger()
	store, err := newTestStore(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	mock := NewMockHypervisor()
	mgr := NewManager(dir, store, logger, mock, 0, "", 0, 0)

	// Setting empty override should use default path.
	mgr.SetRootfsOverride("")
	expected := filepath.Join(dir, "rootfs", "rootfs.ext4")
	if got := mgr.resolveBaseRootfs(); got != expected {
		t.Errorf("resolveBaseRootfs() = %q, want %q", got, expected)
	}
}

// newTestStore creates a test state.Store in the given directory.
func newTestStore(t *testing.T, dir string) (*state.Store, error) {
	t.Helper()
	return state.NewStore(filepath.Join(dir, "state.db"), testLogger())
}
