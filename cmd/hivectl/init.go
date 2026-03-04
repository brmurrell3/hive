// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/templates"
	"github.com/brmurrell3/hive/internal/token"
	"github.com/spf13/cobra"
)

var (
	nonInteractive bool
	templateName   string
	listTemplates  bool
)

func initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init PATH",
		Short: "Scaffold a new cluster root directory",
		Long: `Scaffold a new Hive cluster root at PATH, creating cluster.yaml, an example
agent manifest, and a default team configuration. When run interactively it
launches a setup wizard that collects cluster name, runtime type, and
credentials; pass --non-interactive to skip prompts and write plain defaults.

Use --template to scaffold from a pre-built template instead of the wizard.

Examples:
  # Interactive setup wizard
  hivectl init ./my-cluster

  # Scaffold from a template
  hivectl init --template ci-pipeline ./demo

  # List available templates
  hivectl init --list-templates

  # Non-interactive scaffolding (CI-friendly)
  hivectl init ./my-cluster --non-interactive
  # Output: Cluster root scaffolded at ./my-cluster`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if listTemplates {
				tmplList := templates.ListTemplates()
				if len(tmplList) == 0 {
					fmt.Println("No templates available.")
					return nil
				}
				fmt.Println("Available templates:")
				for name, desc := range tmplList {
					fmt.Printf("  %-20s %s\n", name, desc)
				}
				return nil
			}

			if len(args) == 0 {
				return fmt.Errorf("PATH argument is required (use 'hivectl init PATH')")
			}
			targetDir := args[0]

			if templateName != "" {
				return scaffoldFromTemplate(templateName, targetDir)
			}

			if nonInteractive || !isTTY() {
				return scaffoldCluster(targetDir)
			}
			return scaffoldClusterInteractive(targetDir)
		},
	}

	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Skip interactive prompts (legacy behavior)")
	cmd.Flags().StringVar(&templateName, "template", "", "Scaffold from a pre-built template (e.g., ci-pipeline)")
	cmd.Flags().BoolVar(&listTemplates, "list-templates", false, "List available templates")

	return cmd
}

// scaffoldFromTemplate copies a pre-built template to the target directory.
func scaffoldFromTemplate(name, dir string) error {
	if !templates.TemplateExists(name) {
		tmplList := templates.ListTemplates()
		available := make([]string, 0, len(tmplList))
		for n := range tmplList {
			available = append(available, n)
		}
		return fmt.Errorf("unknown template %q; available templates: %v", name, available)
	}

	// Check that target directory doesn't exist or is empty.
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("target directory %q is not empty; refusing to overwrite", dir)
	}

	if err := templates.CopyTemplate(name, dir); err != nil {
		return fmt.Errorf("copying template %q: %w", name, err)
	}

	fmt.Printf("Template '%s' scaffolded at %s\n", name, dir)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  export ANTHROPIC_API_KEY=sk-...  # optional\n")
	fmt.Printf("  hivectl dev --cluster-root %s\n", dir)
	return nil
}

// isTTY returns true if stdin is a terminal.
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// scaffoldCluster creates a minimal cluster root with example files (non-interactive).
func scaffoldCluster(dir string) error {
	v := wizardValues{
		Dir:         dir,
		ClusterName: "my-cluster",
		AgentID:     "assistant",
		RuntimeType: runtimeCustom,
	}

	agentDir := filepath.Join(dir, "agents", v.AgentID)
	for _, d := range []string{dir, agentDir, filepath.Join(dir, "teams")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", d, err)
		}
	}

	reg := templates.DefaultRegistry()

	type fileEntry struct {
		path string
		tmpl string
	}
	entries := []fileEntry{
		{filepath.Join(dir, "cluster.yaml"), "init/cluster.yaml.tmpl"},
		{filepath.Join(agentDir, "manifest.yaml"), "init/agent-manifest.yaml.tmpl"},
		{filepath.Join(dir, "teams", "default.yaml"), "init/team.yaml.tmpl"},
	}

	for _, e := range entries {
		if _, err := os.Stat(e.path); err == nil {
			fmt.Printf("  skipping %s (already exists)\n", e.path)
			continue
		}
		content, err := reg.Render(e.tmpl, v)
		if err != nil {
			return fmt.Errorf("rendering %s: %w", e.tmpl, err)
		}
		if err := os.WriteFile(e.path, content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", e.path, err)
		}
	}

	fmt.Printf("Cluster root scaffolded at %s\n", dir)
	return nil
}

// wizardValues holds the values collected from interactive prompts.
type wizardValues struct {
	ClusterName    string
	AgentID        string
	RuntimeType    string // runtimeOpenclaw or runtimeCustom
	RuntimeCommand string // command for custom runtimes
	RuntimeEnv     map[string]string
	OpenRouterKey  string
	TelegramToken  string
	TelegramUserID string
	DesktopIP      string
	JoinToken      string
	Dir            string
	AbsDir         string
	User           string
	Model          string
	TeamTemplate   string // e.g. "quant" for multi-agent team templates
}

// scaffoldClusterInteractive runs the interactive wizard.
func scaffoldClusterInteractive(dir string) error {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("Hive Cluster Setup Wizard")
	fmt.Println("─────────────────────────")
	fmt.Println()

	v := wizardValues{Dir: dir, RuntimeEnv: make(map[string]string)}

	v.ClusterName = promptWithDefault(scanner, "Cluster name", "my-cluster")

	// Runtime type selection.
	fmt.Println()
	fmt.Println("Runtime:")
	fmt.Println("  1. openclaw  — OpenClaw AI agent framework (default)")
	fmt.Println("  2. custom    — Custom runtime command")
	fmt.Println()
	rtChoice := promptWithDefault(scanner, "Choose runtime [1/2]", "1")
	switch strings.TrimSpace(rtChoice) {
	case "2", runtimeCustom:
		v.RuntimeType = runtimeCustom
	default:
		v.RuntimeType = runtimeOpenclaw
	}

	if v.RuntimeType == runtimeCustom {
		val, err := promptRequired(scanner, "Runtime command (full path)")
		if err != nil {
			return err
		}
		v.RuntimeCommand = val
	}

	v.AgentID = promptWithDefault(scanner, "Agent ID", "assistant")

	// OpenClaw-specific prompts: only ask when runtime is openclaw.
	if v.RuntimeType == runtimeOpenclaw {
		var err error
		v.OpenRouterKey, err = promptRequired(scanner, "OpenRouter API key")
		if err != nil {
			return err
		}
		v.TelegramToken, err = promptRequired(scanner, "Telegram bot token")
		if err != nil {
			return err
		}
		v.TelegramUserID, err = promptRequired(scanner, "Telegram user ID")
		if err != nil {
			return err
		}
		v.Model = promptWithDefault(scanner, "Model", "openrouter/moonshotai/kimi-k2.5")
	}

	v.DesktopIP = promptWithDefault(scanner, "Desktop IP address", detectDesktopIP())

	fmt.Println()

	// Create directories.
	agentDir := filepath.Join(dir, "agents", v.AgentID)
	secretsDir := filepath.Join(dir, ".secrets")
	for _, d := range []string{dir, agentDir, filepath.Join(dir, "teams"), secretsDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", d, err)
		}
	}
	// Restrict secrets directory to owner only.
	if err := os.Chmod(secretsDir, 0700); err != nil {
		return fmt.Errorf("restricting %s permissions: %w", secretsDir, err)
	}

	// Generate join token.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	statePath := filepath.Join(dir, "state.db")
	store, err := state.NewStore(statePath, logger)
	if err != nil {
		return fmt.Errorf("creating state store: %w", err)
	}
	defer store.Close() //nolint:errcheck // best-effort cleanup on deferred close

	rawToken, err := token.Generate(store, 0, 0) // no expiry
	if err != nil {
		return fmt.Errorf("generating join token: %w", err)
	}
	v.JoinToken = rawToken

	// Write join token to secrets directory (readable only by owner).
	tokenPath := filepath.Join(secretsDir, "join-token")
	if err := os.WriteFile(tokenPath, []byte(rawToken), 0600); err != nil {
		return fmt.Errorf("writing join token to %s: %w", tokenPath, err)
	}

	// Write shared env file so runtime configs can use ${VAR} references
	// instead of hardcoding secrets.
	if v.OpenRouterKey != "" {
		envPath := filepath.Join(secretsDir, "env")
		envContent := "OPENROUTER_API_KEY=" + v.OpenRouterKey + "\n"
		if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
			return fmt.Errorf("writing env file to %s: %w", envPath, err)
		}
	}

	reg := templates.DefaultRegistry()

	// Render and write each templated file.
	type fileEntry struct {
		path string
		tmpl string
		perm os.FileMode
	}

	entries := []fileEntry{
		{filepath.Join(dir, "cluster.yaml"), "init/cluster.yaml.tmpl", 0644},
		{filepath.Join(agentDir, "manifest.yaml"), "init/agent-manifest.yaml.tmpl", 0644},
		{filepath.Join(dir, "teams", "default.yaml"), "init/team.yaml.tmpl", 0644},
		{filepath.Join(dir, "setup-pi.sh"), "init/setup-pi.sh.tmpl", 0700},
	}

	// Only render runtime config for openclaw.
	if v.RuntimeType == runtimeOpenclaw {
		entries = append(entries, fileEntry{
			filepath.Join(agentDir, "openclaw.json"), "init/runtime-config.json.tmpl", 0600,
		})
	}

	for _, e := range entries {
		if _, err := os.Stat(e.path); err == nil {
			fmt.Printf("  skipping %s (already exists)\n", e.path)
			continue
		}

		content, err := reg.Render(e.tmpl, v)
		if err != nil {
			return fmt.Errorf("rendering %s: %w", e.tmpl, err)
		}

		if err := os.WriteFile(e.path, content, e.perm); err != nil {
			return fmt.Errorf("writing %s: %w", e.path, err)
		}
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	v.AbsDir = absDir
	v.User = currentUser()

	fmt.Printf("\nCluster root created at %s\n", v.AbsDir)

	// On NixOS, write flake.nix and offer to install it.
	if isNixOS() {
		if err := writeNixOSFlake(v, scanner); err != nil {
			fmt.Printf("  (could not write NixOS config: %s)\n", err)
			fmt.Println("  You can manually configure /etc/nixos/flake.nix")
		}
	}

	fmt.Println()
	fmt.Println("For Pi deployment:")
	fmt.Printf("  scp %s/setup-pi.sh pi@<PI_IP>:~/setup-pi.sh\n", v.AbsDir)
	fmt.Println("  ssh pi@<PI_IP> 'bash ~/setup-pi.sh'")

	return nil
}

func promptWithDefault(scanner *bufio.Scanner, prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Printf("%s: ", prompt)
	}

	if !scanner.Scan() {
		return defaultVal
	}
	val := strings.TrimSpace(scanner.Text())
	if val == "" {
		return defaultVal
	}
	return val
}

func promptRequired(scanner *bufio.Scanner, prompt string) (string, error) {
	for {
		fmt.Printf("%s: ", prompt)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("reading input for %q: %w", prompt, err)
			}
			return "", fmt.Errorf("unexpected EOF while reading %q", prompt)
		}
		val := strings.TrimSpace(scanner.Text())
		if val != "" {
			return val, nil
		}
		fmt.Println("  (required)")
	}
}

// isNixOS returns true if the system is running NixOS.
func isNixOS() bool {
	_, err := os.Stat("/etc/nixos")
	return err == nil
}

// writeNixOSFlake renders the NixOS flake.nix and installs it to /etc/nixos/.
func writeNixOSFlake(v wizardValues, scanner *bufio.Scanner) error {
	reg := templates.DefaultRegistry()
	content, err := reg.Render("init/nixos-flake.nix.tmpl", v)
	if err != nil {
		return fmt.Errorf("rendering flake template: %w", err)
	}

	// Write to a temp file first.
	tmpFile, err := os.CreateTemp("", "hive-flake-*.nix")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(content); err != nil {
		return fmt.Errorf("writing temp flake: %w", err)
	}
	tmpFile.Close()

	fmt.Println()
	fmt.Print("Install NixOS config to /etc/nixos/flake.nix? [Y/n]: ")
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
		return fmt.Errorf("no input received")
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	if answer != "" && answer != "y" && answer != "yes" {
		// Write to cluster root instead.
		fallback := filepath.Join(v.AbsDir, "flake.nix")
		if err := os.WriteFile(fallback, content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", fallback, err)
		}
		fmt.Printf("  Written to %s (copy to /etc/nixos/flake.nix manually)\n", fallback)
		return nil
	}

	// Use sudo cp to install to /etc/nixos/.
	cmd := exec.Command("sudo", "cp", tmpPath, "/etc/nixos/flake.nix")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo cp failed: %w", err)
	}

	fmt.Println("  Installed /etc/nixos/flake.nix")
	fmt.Println()

	// Offer to rebuild.
	fmt.Print("Run 'sudo nixos-rebuild switch' now? [Y/n]: ")
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
		return fmt.Errorf("no input received")
	}
	answer = strings.TrimSpace(strings.ToLower(scanner.Text()))
	if answer == "" || answer == "y" || answer == "yes" {
		fmt.Println()
		rebuild := exec.Command("sudo", "nixos-rebuild", "switch")
		rebuild.Stdin = os.Stdin
		rebuild.Stdout = os.Stdout
		rebuild.Stderr = os.Stderr
		if err := rebuild.Run(); err != nil {
			return fmt.Errorf("nixos-rebuild failed: %w", err)
		}
		fmt.Println()
		fmt.Println("Done! hived and agents are running.")
		fmt.Println("  Check status: systemctl status hived 'hive-agent-*'")
	} else {
		fmt.Println()
		fmt.Println("Run when ready: sudo nixos-rebuild switch")
	}

	return nil
}

// currentUser returns the current OS username.
// readEnvFileKey reads a KEY=VALUE env file and returns the value for the given key.
func readEnvFileKey(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "deploy"
}

// detectDesktopIP returns the local IP used for outbound connections.
func detectDesktopIP() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:80", 2*time.Second)
	if err != nil {
		return "192.168.1.100"
	}
	defer conn.Close()

	udpAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "127.0.0.1"
	}
	return udpAddr.IP.String()
}
