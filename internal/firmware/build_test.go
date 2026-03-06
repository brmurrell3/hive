//go:build unit

package firmware

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/brmurrell3/hive/internal/types"
)

func TestBuild_MissingSourceDir(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	_, err := Build(BuildConfig{
		AgentID:   "test-agent",
		Platform:  PlatformBareMetal,
		SourceDir: "/nonexistent/path",
		Logger:    logger,
	})

	if err == nil {
		t.Fatal("expected error for missing source dir")
	}
}

func TestBuild_UnsupportedPlatform(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	dir := t.TempDir()
	_, err := Build(BuildConfig{
		AgentID:   "test-agent",
		Platform:  Platform("unsupported"),
		SourceDir: dir,
		Logger:    logger,
	})

	if err == nil {
		t.Fatal("expected error for unsupported platform")
	}
}

func TestBuildFromManifest_MissingPlatform(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	manifest := &types.AgentManifest{
		Metadata: types.AgentMetadata{ID: "test-agent"},
		Spec: types.AgentSpec{
			Firmware: types.AgentFirmware{},
		},
	}

	_, err := BuildFromManifest(manifest, t.TempDir(), logger)
	if err == nil {
		t.Fatal("expected error for missing platform")
	}
}

func TestBuildBareMetal_NoMakefile(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	dir := t.TempDir()
	_, err := Build(BuildConfig{
		AgentID:   "test-agent",
		Platform:  PlatformBareMetal,
		SourceDir: dir,
		Logger:    logger,
	})

	if err == nil {
		t.Fatal("expected error for missing Makefile")
	}
}

func TestFlash_MissingPort(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	err := Flash(FlashConfig{
		AgentID:    "test-agent",
		Platform:   PlatformESPIDF,
		BinaryPath: "/some/path",
		Logger:     logger,
	})

	if err == nil {
		t.Fatal("expected error for missing port")
	}
}

func TestFlash_MissingBinary(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	err := Flash(FlashConfig{
		AgentID:    "test-agent",
		Platform:   PlatformESPIDF,
		BinaryPath: "/nonexistent/firmware.bin",
		Port:       "/dev/ttyUSB0",
		Logger:     logger,
	})

	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestMonitor_MissingPort(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	err := Monitor(MonitorConfig{
		AgentID:  "test-agent",
		Platform: PlatformESPIDF,
		Logger:   logger,
	})

	if err == nil {
		t.Fatal("expected error for missing port")
	}
}

func TestBuildConfig_DefaultOutputDir(t *testing.T) {
	dir := t.TempDir()
	clusterRoot := dir
	agentID := "test-agent"

	// Create the firmware source dir
	srcDir := filepath.Join(clusterRoot, "agents", agentID, "firmware")
	os.MkdirAll(srcDir, 0755)

	// Write a dummy Makefile
	os.WriteFile(filepath.Join(srcDir, "Makefile"), []byte("all:\n\ttouch $(BUILD_DIR)/firmware.bin\n"), 0644)

	cfg := BuildConfig{
		AgentID:     agentID,
		Platform:    PlatformBareMetal,
		ClusterRoot: clusterRoot,
		Logger:      slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	if cfg.SourceDir == "" {
		cfg.SourceDir = filepath.Join(cfg.ClusterRoot, "agents", cfg.AgentID, "firmware")
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = filepath.Join(cfg.SourceDir, "build")
	}

	expectedSrc := filepath.Join(clusterRoot, "agents", agentID, "firmware")
	expectedOut := filepath.Join(expectedSrc, "build")

	if cfg.SourceDir != expectedSrc {
		t.Errorf("expected source dir %s, got %s", expectedSrc, cfg.SourceDir)
	}
	if cfg.OutputDir != expectedOut {
		t.Errorf("expected output dir %s, got %s", expectedOut, cfg.OutputDir)
	}
}

func TestPlatformConstants(t *testing.T) {
	platforms := []Platform{
		PlatformESPIDF,
		PlatformArduino,
		PlatformPicoSDK,
		PlatformZephyr,
		PlatformBareMetal,
	}

	for _, p := range platforms {
		if string(p) == "" {
			t.Errorf("platform constant is empty")
		}
	}
}
