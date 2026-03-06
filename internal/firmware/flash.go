package firmware

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// FlashConfig holds firmware flashing configuration.
type FlashConfig struct {
	AgentID    string
	Platform   Platform
	BinaryPath string
	Port       string
	Baud       int
	Logger     *slog.Logger
}

// Flash writes firmware to a connected device via serial port.
func Flash(cfg FlashConfig) error {
	if cfg.Port == "" {
		return fmt.Errorf("--port is required for flashing")
	}
	if cfg.BinaryPath == "" {
		return fmt.Errorf("no binary path specified")
	}

	if _, err := os.Stat(cfg.BinaryPath); os.IsNotExist(err) {
		return fmt.Errorf("firmware binary not found: %s", cfg.BinaryPath)
	}

	baud := cfg.Baud
	if baud == 0 {
		baud = 460800
	}

	cfg.Logger.Info("flashing firmware",
		"agent_id", cfg.AgentID,
		"platform", cfg.Platform,
		"port", cfg.Port,
		"baud", baud,
		"binary", cfg.BinaryPath,
	)

	switch cfg.Platform {
	case PlatformESPIDF:
		return flashESPIDF(cfg, baud)
	case PlatformArduino:
		return flashArduino(cfg, baud)
	case PlatformPicoSDK:
		return flashPicoSDK(cfg)
	case PlatformZephyr:
		return flashZephyr(cfg)
	case PlatformBareMetal:
		return flashBareMetal(cfg)
	default:
		return fmt.Errorf("unsupported platform for flashing: %s", cfg.Platform)
	}
}

func flashESPIDF(cfg FlashConfig, baud int) error {
	cmd := exec.Command("esptool.py",
		"--port", cfg.Port,
		"--baud", fmt.Sprintf("%d", baud),
		"write_flash", "0x10000", cfg.BinaryPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func flashArduino(cfg FlashConfig, baud int) error {
	cmd := exec.Command("arduino-cli", "upload",
		"-p", cfg.Port,
		"--input-file", cfg.BinaryPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func flashPicoSDK(cfg FlashConfig) error {
	if err := checkToolchain("picotool"); err != nil {
		return fmt.Errorf("picotool not found: %w", err)
	}

	cmd := exec.Command("picotool", "load",
		"-f", cfg.BinaryPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func flashZephyr(cfg FlashConfig) error {
	cmd := exec.Command("west", "flash",
		"--build-dir", filepath.Dir(cfg.BinaryPath),
		"--bin-file", cfg.BinaryPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func flashBareMetal(cfg FlashConfig) error {
	cmd := exec.Command("make",
		"flash",
		"PORT="+cfg.Port,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// MonitorConfig holds serial monitor configuration.
type MonitorConfig struct {
	AgentID  string
	Port     string
	Baud     int
	Platform Platform
	Logger   *slog.Logger
}

// Monitor opens a serial monitor for firmware debugging.
func Monitor(cfg MonitorConfig) error {
	if cfg.Port == "" {
		return fmt.Errorf("--port is required for monitoring")
	}

	baud := cfg.Baud
	if baud == 0 {
		baud = 115200
	}

	cfg.Logger.Info("starting serial monitor",
		"agent_id", cfg.AgentID,
		"port", cfg.Port,
		"baud", baud,
	)

	switch cfg.Platform {
	case PlatformESPIDF:
		return monitorESPIDF(cfg, baud)
	default:
		return monitorGeneric(cfg, baud)
	}
}

func monitorESPIDF(cfg MonitorConfig, baud int) error {
	cmd := exec.Command("idf.py",
		"-p", cfg.Port,
		"-b", fmt.Sprintf("%d", baud),
		"monitor",
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func monitorGeneric(cfg MonitorConfig, baud int) error {
	// Try screen, then minicom, then cat
	if _, err := exec.LookPath("screen"); err == nil {
		cmd := exec.Command("screen", cfg.Port, fmt.Sprintf("%d", baud))
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	if _, err := exec.LookPath("minicom"); err == nil {
		cmd := exec.Command("minicom",
			"-D", cfg.Port,
			"-b", fmt.Sprintf("%d", baud),
		)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// Fallback: cat the serial device
	cmd := exec.Command("cat", cfg.Port)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
