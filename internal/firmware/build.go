package firmware

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hivehq/hive/internal/types"
)

// Platform represents a firmware build platform.
type Platform string

const (
	PlatformESPIDF    Platform = "esp-idf"
	PlatformArduino   Platform = "arduino"
	PlatformPicoSDK   Platform = "pico-sdk"
	PlatformZephyr    Platform = "zephyr"
	PlatformBareMetal Platform = "bare-metal"
)

// BuildConfig holds firmware build configuration.
type BuildConfig struct {
	AgentID     string
	Platform    Platform
	Board       string
	SourceDir   string
	OutputDir   string
	ClusterRoot string
	Logger      *slog.Logger
}

// BuildResult contains the output of a firmware build.
type BuildResult struct {
	BinaryPath string
	Platform   Platform
	Board      string
	Size       int64
}

// Build compiles firmware from the agent's source directory.
func Build(cfg BuildConfig) (*BuildResult, error) {
	if cfg.SourceDir == "" {
		cfg.SourceDir = filepath.Join(cfg.ClusterRoot, "agents", cfg.AgentID, "firmware")
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = filepath.Join(cfg.SourceDir, "build")
	}

	if _, err := os.Stat(cfg.SourceDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("firmware source directory not found: %s", cfg.SourceDir)
	}

	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("creating output directory: %w", err)
	}

	cfg.Logger.Info("building firmware",
		"agent_id", cfg.AgentID,
		"platform", cfg.Platform,
		"board", cfg.Board,
		"source_dir", cfg.SourceDir,
	)

	var binaryPath string
	var err error

	switch cfg.Platform {
	case PlatformESPIDF:
		binaryPath, err = buildESPIDF(cfg)
	case PlatformArduino:
		binaryPath, err = buildArduino(cfg)
	case PlatformPicoSDK:
		binaryPath, err = buildPicoSDK(cfg)
	case PlatformZephyr:
		binaryPath, err = buildZephyr(cfg)
	case PlatformBareMetal:
		binaryPath, err = buildBareMetal(cfg)
	default:
		return nil, fmt.Errorf("unsupported platform: %s", cfg.Platform)
	}

	if err != nil {
		return nil, fmt.Errorf("build failed: %w", err)
	}

	info, err := os.Stat(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("checking build output: %w", err)
	}

	cfg.Logger.Info("firmware build complete",
		"output", binaryPath,
		"size", info.Size(),
	)

	return &BuildResult{
		BinaryPath: binaryPath,
		Platform:   cfg.Platform,
		Board:      cfg.Board,
		Size:       info.Size(),
	}, nil
}

func buildESPIDF(cfg BuildConfig) (string, error) {
	if err := checkToolchain("idf.py"); err != nil {
		return "", fmt.Errorf("ESP-IDF toolchain not found: %w", err)
	}

	// Set IDF_TARGET from board name
	target := cfg.Board
	if target == "" {
		target = "esp32"
	}

	cmd := exec.Command("idf.py",
		"-B", cfg.OutputDir,
		"-DIDF_TARGET="+target,
		"build",
	)
	cmd.Dir = cfg.SourceDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return filepath.Join(cfg.OutputDir, "firmware.bin"), nil
}

func buildArduino(cfg BuildConfig) (string, error) {
	if err := checkToolchain("arduino-cli"); err != nil {
		return "", fmt.Errorf("Arduino CLI not found: %w", err)
	}

	fqbn := cfg.Board
	if fqbn == "" {
		fqbn = "esp32:esp32:esp32"
	}

	// Find .ino file
	inoFile := ""
	entries, _ := os.ReadDir(cfg.SourceDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".ino") {
			inoFile = filepath.Join(cfg.SourceDir, e.Name())
			break
		}
	}
	if inoFile == "" {
		return "", fmt.Errorf("no .ino file found in %s", cfg.SourceDir)
	}

	cmd := exec.Command("arduino-cli", "compile",
		"--fqbn", fqbn,
		"--output-dir", cfg.OutputDir,
		inoFile,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return filepath.Join(cfg.OutputDir, filepath.Base(inoFile)+".bin"), nil
}

func buildPicoSDK(cfg BuildConfig) (string, error) {
	if err := checkToolchain("cmake"); err != nil {
		return "", fmt.Errorf("CMake not found: %w", err)
	}

	board := cfg.Board
	if board == "" {
		board = "pico_w"
	}

	// cmake configure
	cmd := exec.Command("cmake",
		"-B", cfg.OutputDir,
		"-DPICO_BOARD="+board,
		cfg.SourceDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cmake configure: %w", err)
	}

	// cmake build
	cmd = exec.Command("cmake", "--build", cfg.OutputDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cmake build: %w", err)
	}

	return filepath.Join(cfg.OutputDir, "firmware.uf2"), nil
}

func buildZephyr(cfg BuildConfig) (string, error) {
	if err := checkToolchain("west"); err != nil {
		return "", fmt.Errorf("Zephyr west tool not found: %w", err)
	}

	board := cfg.Board
	if board == "" {
		return "", fmt.Errorf("board is required for Zephyr builds")
	}

	cmd := exec.Command("west", "build",
		"-b", board,
		"-d", cfg.OutputDir,
		cfg.SourceDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	return filepath.Join(cfg.OutputDir, "zephyr", "zephyr.bin"), nil
}

func buildBareMetal(cfg BuildConfig) (string, error) {
	makefile := filepath.Join(cfg.SourceDir, "Makefile")
	if _, err := os.Stat(makefile); os.IsNotExist(err) {
		return "", fmt.Errorf("no Makefile found in %s", cfg.SourceDir)
	}

	cmd := exec.Command("make",
		"-C", cfg.SourceDir,
		"BUILD_DIR="+cfg.OutputDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}

	return filepath.Join(cfg.OutputDir, "firmware.bin"), nil
}

func checkToolchain(name string) error {
	_, err := exec.LookPath(name)
	return err
}

// BuildFromManifest builds firmware using settings from the agent manifest.
func BuildFromManifest(manifest *types.AgentManifest, clusterRoot string, logger *slog.Logger) (*BuildResult, error) {
	platform := Platform(manifest.Spec.Firmware.Platform)
	if platform == "" {
		return nil, fmt.Errorf("manifest missing spec.firmware.platform")
	}

	return Build(BuildConfig{
		AgentID:     manifest.Metadata.ID,
		Platform:    platform,
		Board:       manifest.Spec.Firmware.Board,
		ClusterRoot: clusterRoot,
		Logger:      logger,
	})
}
