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
	"text/template"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/token"
	"github.com/spf13/cobra"
)

var nonInteractive bool

func initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init PATH",
		Short: "Scaffold a new cluster root directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetDir := args[0]

			if nonInteractive || !isTTY() {
				return scaffoldCluster(targetDir)
			}
			return scaffoldClusterInteractive(targetDir)
		},
	}

	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "Skip interactive prompts (legacy behavior)")

	return cmd
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
	dirs := []string{
		dir,
		filepath.Join(dir, "agents", "example-agent"),
		filepath.Join(dir, "teams"),
	}

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", d, err)
		}
	}

	files := map[string]string{
		filepath.Join(dir, "cluster.yaml"):                             clusterTemplate,
		filepath.Join(dir, "agents", "example-agent", "manifest.yaml"): agentTemplate,
		filepath.Join(dir, "teams", "default.yaml"):                    teamTemplate,
	}

	// Skip existing files to make init idempotent.
	for path, content := range files {
		if _, err := os.Stat(path); err == nil {
			fmt.Printf("  skipping %s (already exists)\n", path)
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}

	fmt.Printf("Cluster root scaffolded at %s\n", dir)
	return nil
}

// wizardValues holds the values collected from interactive prompts.
type wizardValues struct {
	ClusterName    string
	AgentID        string
	OpenRouterKey  string
	TelegramToken  string
	TelegramUserID string
	DesktopIP      string
	JoinToken      string
	Dir            string
	AbsDir         string
	User           string
}

// scaffoldClusterInteractive runs the interactive wizard.
func scaffoldClusterInteractive(dir string) error {
	scanner := bufio.NewScanner(os.Stdin)

	fmt.Println("Hive Cluster Setup Wizard")
	fmt.Println("─────────────────────────")
	fmt.Println()

	v := wizardValues{Dir: dir}

	v.ClusterName = promptWithDefault(scanner, "Cluster name", "my-cluster")
	v.AgentID = promptWithDefault(scanner, "Agent ID", "assistant")
	v.OpenRouterKey = promptRequired(scanner, "OpenRouter API key")
	v.TelegramToken = promptRequired(scanner, "Telegram bot token")
	v.TelegramUserID = promptRequired(scanner, "Telegram user ID")
	v.DesktopIP = promptWithDefault(scanner, "Desktop IP address", detectDesktopIP())

	fmt.Println()

	// Create directories.
	agentDir := filepath.Join(dir, "agents", v.AgentID)
	for _, d := range []string{dir, agentDir, filepath.Join(dir, "teams")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", d, err)
		}
	}

	// Generate join token.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	statePath := filepath.Join(dir, "state.db")
	store, err := state.NewStore(statePath, logger)
	if err != nil {
		return fmt.Errorf("creating state store: %w", err)
	}

	rawToken, err := token.Generate(store, 0) // no expiry
	if err != nil {
		return fmt.Errorf("generating join token: %w", err)
	}
	v.JoinToken = rawToken

	// Render and write each templated file.
	type fileEntry struct {
		path     string
		tmplName string
		tmplText string
		perm     os.FileMode
	}

	entries := []fileEntry{
		{filepath.Join(dir, "cluster.yaml"), "cluster", wizardClusterTmpl, 0644},
		{filepath.Join(agentDir, "manifest.yaml"), "manifest", wizardManifestTmpl, 0644},
		{filepath.Join(dir, "teams", "default.yaml"), "team", wizardTeamTmpl, 0644},
		{filepath.Join(agentDir, "openclaw.json"), "openclaw", wizardOpenClawTmpl, 0600},
		{filepath.Join(dir, "setup-pi.sh"), "setup-pi", setupPiTmpl, 0755},
	}

	for _, e := range entries {
		if _, err := os.Stat(e.path); err == nil {
			fmt.Printf("  skipping %s (already exists)\n", e.path)
			continue
		}

		tmpl, err := template.New(e.tmplName).Parse(e.tmplText)
		if err != nil {
			return fmt.Errorf("parsing template %s: %w", e.tmplName, err)
		}

		f, err := os.OpenFile(e.path, os.O_CREATE|os.O_WRONLY, e.perm)
		if err != nil {
			return fmt.Errorf("creating %s: %w", e.path, err)
		}

		if err := tmpl.Execute(f, v); err != nil {
			f.Close()
			return fmt.Errorf("writing %s: %w", e.path, err)
		}
		f.Close()
	}

	v.AbsDir, _ = filepath.Abs(dir)
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

	scanner.Scan()
	val := strings.TrimSpace(scanner.Text())
	if val == "" {
		return defaultVal
	}
	return val
}

func promptRequired(scanner *bufio.Scanner, prompt string) string {
	for {
		fmt.Printf("%s: ", prompt)
		scanner.Scan()
		val := strings.TrimSpace(scanner.Text())
		if val != "" {
			return val
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
	tmpl, err := template.New("nixos-flake").Parse(nixosFlakeTmpl)
	if err != nil {
		return fmt.Errorf("parsing flake template: %w", err)
	}

	// Write to a temp file first.
	tmpFile, err := os.CreateTemp("", "hive-flake-*.nix")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := tmpl.Execute(tmpFile, v); err != nil {
		tmpFile.Close()
		return fmt.Errorf("rendering flake: %w", err)
	}
	tmpFile.Close()

	fmt.Println()
	fmt.Print("Install NixOS config to /etc/nixos/flake.nix? [Y/n]: ")
	scanner.Scan()
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	if answer != "" && answer != "y" && answer != "yes" {
		// Write to cluster root instead.
		fallback := filepath.Join(v.AbsDir, "flake.nix")
		data, _ := os.ReadFile(tmpPath)
		if err := os.WriteFile(fallback, data, 0644); err != nil {
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
	scanner.Scan()
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
		fmt.Println("Done! hived and hive-agent are running.")
		fmt.Println("  Check status: systemctl status hived hive-agent")
	} else {
		fmt.Println()
		fmt.Println("Run when ready: sudo nixos-rebuild switch")
	}

	return nil
}

// currentUser returns the current OS username.
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

	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}

// --- Non-interactive templates (legacy) ---

const clusterTemplate = `apiVersion: hive/v1
kind: Cluster
metadata:
  name: my-cluster
spec:
  nats:
    port: 4222
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 2
    health:
      interval: "30s"
      timeout: "5s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 5
      backoff: "10s"
`

const agentTemplate = `apiVersion: hive/v1
kind: Agent
metadata:
  id: example-agent
  team: default
spec:
  runtime:
    type: openclaw
    model:
      provider: anthropic
      name: claude-sonnet-4-5
  capabilities:
    - name: answer-questions
      description: Answers general knowledge questions
      inputs:
        - name: question
          type: string
          description: The question to answer
      outputs:
        - name: answer
          type: string
          description: The answer
  resources:
    memory: "512Mi"
    vcpus: 2
`

const teamTemplate = `apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: example-agent
`

// --- Interactive wizard templates ---

const wizardClusterTmpl = `apiVersion: hive/v1
kind: Cluster
metadata:
  name: {{.ClusterName}}
spec:
  nats:
    host: "0.0.0.0"
    port: 4222
    clusterPort: -1
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "256Mi"
      vcpus: 1
    health:
      interval: "30s"
      timeout: "5s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 5
      backoff: "10s"
`

const wizardManifestTmpl = `apiVersion: hive/v1
kind: Agent
metadata:
  id: {{.AgentID}}
  team: default
spec:
  tier: native
  runtime:
    type: openclaw
  capabilities:
    - name: chat
      description: General conversation and task execution
      inputs:
        - name: message
          type: string
          description: User message
      outputs:
        - name: response
          type: string
          description: Agent response
  resources:
    memory: "256Mi"
    vcpus: 1
`

const wizardTeamTmpl = `apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: {{.AgentID}}
`

const wizardOpenClawTmpl = `{
  "env": {
    "OPENROUTER_API_KEY": "{{.OpenRouterKey}}"
  },
  "agents": {
    "defaults": {
      "model": {
        "primary": "openrouter/moonshotai/kimi-k2.5"
      }
    }
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "{{.TelegramToken}}",
      "dmPolicy": "allowlist",
      "allowFrom": ["tg:{{.TelegramUserID}}"]
    }
  }
}
`

const setupPiTmpl = `#!/usr/bin/env bash
# Hive Pi Bootstrap Script
# Generated by: hivectl init
# Cluster: {{.ClusterName}} | Agent: {{.AgentID}}
#
# Usage: scp this file to the Pi, then run: bash setup-pi.sh

set -euo pipefail

CONTROL_PLANE="{{.DesktopIP}}:4222"
JOIN_TOKEN="{{.JoinToken}}"
AGENT_ID="{{.AgentID}}"
OPENROUTER_API_KEY="{{.OpenRouterKey}}"
TELEGRAM_BOT_TOKEN="{{.TelegramToken}}"
TELEGRAM_USER_ID="{{.TelegramUserID}}"

echo "=== Hive Pi Bootstrap ==="
echo "Control plane: $CONTROL_PLANE"
echo "Agent ID:      $AGENT_ID"
echo ""

# ── 1. Install Node.js 22 ──────────────────────────────────────────────────────
if command -v node &>/dev/null && [[ "$(node --version)" == v22.* ]]; then
    echo "[1/6] Node.js 22 already installed ($(node --version)), skipping."
else
    echo "[1/6] Installing Node.js 22..."
    sudo apt-get update -qq
    sudo apt-get install -y -qq ca-certificates curl gnupg

    sudo mkdir -p /etc/apt/keyrings
    curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | sudo gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg --yes

    echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
        | sudo tee /etc/apt/sources.list.d/nodesource.list >/dev/null

    sudo apt-get update -qq
    sudo apt-get install -y -qq nodejs
    echo "  Node.js $(node --version) installed."
fi

# ── 2. Install OpenClaw ─────────────────────────────────────────────────────────
if command -v openclaw &>/dev/null; then
    echo "[2/6] OpenClaw already installed, skipping."
else
    echo "[2/6] Installing OpenClaw..."
    sudo npm install -g openclaw@latest
    echo "  OpenClaw installed."
fi

# ── 3. Write OpenClaw config ────────────────────────────────────────────────────
OPENCLAW_DIR="$HOME/.openclaw"
OPENCLAW_CFG="$OPENCLAW_DIR/openclaw.json"

if [ -f "$OPENCLAW_CFG" ]; then
    echo "[3/6] OpenClaw config already exists ($OPENCLAW_CFG), skipping."
else
    echo "[3/6] Writing OpenClaw config..."
    mkdir -p "$OPENCLAW_DIR"
    cat > "$OPENCLAW_CFG" << 'OPENCLAW_EOF'
{
  "env": {
    "OPENROUTER_API_KEY": "OPENROUTER_API_KEY_PLACEHOLDER"
  },
  "agents": {
    "defaults": {
      "model": {
        "primary": "openrouter/moonshotai/kimi-k2.5"
      }
    }
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "botToken": "TELEGRAM_BOT_TOKEN_PLACEHOLDER",
      "dmPolicy": "allowlist",
      "allowFrom": ["tg:TELEGRAM_USER_ID_PLACEHOLDER"]
    }
  }
}
OPENCLAW_EOF

    # Substitute values (done this way to avoid heredoc quoting issues).
    sed -i "s|OPENROUTER_API_KEY_PLACEHOLDER|$OPENROUTER_API_KEY|g" "$OPENCLAW_CFG"
    sed -i "s|TELEGRAM_BOT_TOKEN_PLACEHOLDER|$TELEGRAM_BOT_TOKEN|g" "$OPENCLAW_CFG"
    sed -i "s|TELEGRAM_USER_ID_PLACEHOLDER|$TELEGRAM_USER_ID|g" "$OPENCLAW_CFG"
    chmod 600 "$OPENCLAW_CFG"
    echo "  Config written to $OPENCLAW_CFG"
fi

# ── 4. Install hive-agent ───────────────────────────────────────────────────────
AGENT_BIN="/home/$(whoami)/hive-agent"

if [ -x "$AGENT_BIN" ]; then
    echo "[4/6] hive-agent binary already exists, skipping."
else
    echo "[4/6] Installing hive-agent..."

    # Detect architecture.
    ARCH=$(dpkg --print-architecture 2>/dev/null || uname -m)
    case "$ARCH" in
        arm64|aarch64) ARCH="arm64" ;;
        amd64|x86_64)  ARCH="amd64" ;;
        *)             ARCH="arm64" ;; # default for Pi
    esac

    RELEASE_URL="https://github.com/brmurrell3/hive/releases/latest/download/hive-agent-linux-${ARCH}"

    if curl -fsSL -o "$AGENT_BIN" "$RELEASE_URL" 2>/dev/null; then
        chmod +x "$AGENT_BIN"
        echo "  Downloaded hive-agent from GitHub releases."
    else
        echo "  Could not download from GitHub releases."
        echo "  Please copy the binary manually:"
        echo "    From your desktop, run:"
        echo "    scp bin/linux-${ARCH}/hive-agent $(whoami)@$(hostname -I | awk '{print $1}'):$AGENT_BIN"
        echo ""
        echo "  Then re-run this script."
        exit 1
    fi
fi

# ── 5. Write agent manifest ─────────────────────────────────────────────────────
MANIFEST="$HOME/manifest.yaml"

if [ -f "$MANIFEST" ]; then
    echo "[5/6] Agent manifest already exists, skipping."
else
    echo "[5/6] Writing agent manifest..."
    cat > "$MANIFEST" << 'MANIFEST_EOF'
apiVersion: hive/v1
kind: Agent
metadata:
  id: {{.AgentID}}
  team: default
spec:
  tier: native
  runtime:
    type: openclaw
  capabilities:
    - name: chat
      description: General conversation and task execution
      inputs:
        - name: message
          type: string
          description: User message
      outputs:
        - name: response
          type: string
          description: Agent response
  resources:
    memory: "256Mi"
    vcpus: 1
MANIFEST_EOF
    echo "  Manifest written to $MANIFEST"
fi

# ── 6. Create and start systemd service ──────────────────────────────────────────
SERVICE_FILE="/etc/systemd/system/hive-agent.service"

if systemctl is-active --quiet hive-agent 2>/dev/null; then
    echo "[6/6] hive-agent service already running, skipping."
else
    echo "[6/6] Creating systemd service..."
    sudo tee "$SERVICE_FILE" > /dev/null << SERVICE_EOF
[Unit]
Description=Hive Agent ({{.AgentID}})
After=network-online.target
Wants=network-online.target

[Service]
User=$(whoami)
ExecStart=$AGENT_BIN join \\
    --token $JOIN_TOKEN \\
    --control-plane $CONTROL_PLANE \\
    --agent-id $AGENT_ID \\
    --manifest $MANIFEST \\
    --runtime-cmd openclaw \\
    --runtime-args gateway \\
    --work-dir $HOME/hive-workspace \\
    --http-addr :9100
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
SERVICE_EOF

    sudo systemctl daemon-reload
    sudo systemctl enable hive-agent
    sudo systemctl start hive-agent
    echo "  Service created and started."
fi

echo ""
echo "=== Done ==="
echo "Check status:  sudo systemctl status hive-agent"
echo "View logs:     journalctl -u hive-agent -f"
echo "Test Telegram: send a message to your bot"
`

const nixosFlakeTmpl = `{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    hive.url = "github:brmurrell3/hive";
  };

  outputs = { self, nixpkgs, hive, ... }: {
    nixosConfigurations.nixos = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        hive.nixosModules.default
        {
          services.hived = {
            enable = true;
            clusterRoot = "{{.AbsDir}}";
            user = "{{.User}}";
            group = "users";
            openFirewall = true;
            agent = {
              enable = true;
              id = "{{.AgentID}}";
              manifest = "{{.AbsDir}}/agents/{{.AgentID}}/manifest.yaml";
              openclawConfig = "{{.AbsDir}}/agents/{{.AgentID}}/openclaw.json";
              joinToken = "{{.JoinToken}}";
            };
          };
        }
      ];
    };
  };
}
`
