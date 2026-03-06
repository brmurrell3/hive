# Hive Deployment Guide

> From bare hardware to a working AI agent you can talk to on Telegram.

---

## What is Hive?

Hive is a framework for running AI agents across multiple machines. It has three
parts:

- **hived** — the control plane. Runs on one machine (your desktop). Manages
  agent state, routes messages between agents, monitors health, and auto-restarts
  failed agents. It embeds its own NATS message bus and SQLite database — no
  external services needed.

- **hive-agent** — runs on each worker machine (your Pi). Joins the cluster,
  starts an AI runtime (OpenClaw), and reports health back to the control plane.

- **hivectl** — CLI tool for managing the cluster. Create tokens, list agents,
  start/stop agents.

Agents communicate over NATS (a lightweight message bus). The control plane
watches agent manifests on disk and automatically reconciles desired state with
actual state — if you define an agent, hived makes sure it's running.

---

## What you're building

```
Desktop (NixOS)                        Raspberry Pi 3
┌──────────────────────┐               ┌──────────────────────┐
│ hived (control plane)│               │ hive-agent           │
│  ├ Embedded NATS     │◄──TCP:4222───►│  ├ Sidecar           │
│  ├ SQLite state      │               │  ├ OpenClaw runtime  │
│  ├ Health monitor    │               │  │  ├ Kimi K2.5 LLM  │
│  └ Reconciler        │               │  │  └ Telegram bot    │
└──────────────────────┘               └──────────────────────┘
         you ──── Telegram ────────────────────► OpenClaw
```

You message your Telegram bot. OpenClaw (running on the Pi) receives the
message, sends it to Kimi K2.5 via OpenRouter, and replies. The control plane
on your desktop monitors the agent, restarts it if it crashes, and lets you
manage it with `hivectl`.

---

## What you need

| Item | Notes |
|------|-------|
| Desktop PC | Any x86_64 machine. Old is fine — hived uses <100MB RAM. |
| Raspberry Pi 3 | Model B or B+. A Pi 4/5 also works. |
| microSD card | 16GB+, for the Pi |
| Ethernet or WiFi | Both machines on the same LAN |
| USB flash drive | 2GB+, for the NixOS installer |
| A monitor + keyboard | Temporarily, for NixOS install. Not needed after. |

You'll also create accounts on these free services during the guide:
- OpenRouter (LLM API gateway)
- Telegram (messaging)

---

## Part 1: Set up the desktop (NixOS)

### 1.1 Download NixOS

Go to **https://nixos.org/download/** and download the **Minimal ISO** for
x86_64. It's ~1GB.

### 1.2 Write the ISO to a USB drive

On whatever machine you have now (Mac, Windows, Linux):

**macOS/Linux:**
```bash
# Find your USB device
diskutil list          # macOS
lsblk                  # Linux

# Write the ISO (REPLACE /dev/sdX with your USB device)
sudo dd if=nixos-minimal-*.iso of=/dev/sdX bs=4M status=progress
sync
```

**Windows:** Download [Rufus](https://rufus.ie/), select the ISO, select your
USB drive, click Start.

### 1.3 Boot from USB and install NixOS

1. Plug the USB into the desktop. Connect a monitor and keyboard.
2. Boot the machine. Enter BIOS/UEFI (usually F2, F12, DEL, or ESC during
   startup). Disable Secure Boot. Set USB as first boot device.
3. Boot into the NixOS installer. You'll get a root shell.

**Partition the disk** (replace `/dev/sda` with your actual disk — run `lsblk`
to check):

```bash
parted /dev/sda -- mklabel gpt
parted /dev/sda -- mkpart root ext4 512MiB -8GiB
parted /dev/sda -- mkpart swap linux-swap -8GiB 100%
parted /dev/sda -- mkpart ESP fat32 1MiB 512MiB
parted /dev/sda -- set 3 esp on
```

**Format:**
```bash
mkfs.ext4 -L nixos /dev/sda1
mkswap -L swap /dev/sda2
mkfs.fat -F 32 -n boot /dev/sda3
```

**Mount:**
```bash
mount /dev/disk/by-label/nixos /mnt
mkdir -p /mnt/boot
mount /dev/disk/by-label/boot /mnt/boot
swapon /dev/sda2
```

**Generate config:**
```bash
nixos-generate-config --root /mnt
```

**Edit** `/mnt/etc/nixos/configuration.nix`. Replace its contents with:

```nix
{ config, pkgs, ... }:
{
  imports = [ ./hardware-configuration.nix ];

  # Boot
  boot.loader.systemd-boot.enable = true;
  boot.loader.efi.canTouchEfiVariables = true;

  # Network — REPLACE enp1s0 with your interface (run: ip link show)
  networking.hostName = "hive-desktop";
  networking.useDHCP = false;
  networking.interfaces.enp1s0.useDHCP = true;  # DHCP for now; we set static later

  # Firewall — open NATS port so the Pi can connect
  networking.firewall.allowedTCPPorts = [ 22 4222 ];

  # SSH
  services.openssh.enable = true;

  # Enable flakes
  nix.settings.experimental-features = [ "nix-command" "flakes" ];

  # Packages
  environment.systemPackages = with pkgs; [
    git
    go
    gnumake
    vim
    nmap         # to find the Pi later
  ];

  # User account — REPLACE with your username
  users.users.deploy = {
    isNormalUser = true;
    extraGroups = [ "wheel" ];
    initialPassword = "changeme";
  };

  system.stateVersion = "25.05";
}
```

**Install and reboot:**
```bash
nixos-install    # Sets root password when prompted
reboot
```

Remove the USB drive. Log in as your user (e.g., `deploy` / `changeme`).

### 1.4 Set a static IP (recommended)

After the first boot, find your interface name:

```bash
ip link show
# Look for enp1s0, eno1, eth0, etc.
```

Edit `/etc/nixos/configuration.nix`. Replace the DHCP line with a static IP:

```nix
networking.interfaces.enp1s0 = {
  useDHCP = false;
  ipv4.addresses = [{
    address = "192.168.1.100";     # Pick an IP outside your router's DHCP range
    prefixLength = 24;
  }];
};
networking.defaultGateway = {
  address = "192.168.1.1";         # Your router's IP
  interface = "enp1s0";
};
networking.nameservers = [ "1.1.1.1" "8.8.8.8" ];
```

Apply:
```bash
sudo nixos-rebuild switch
```

From this point, you can SSH in from another machine and disconnect the monitor:
```bash
ssh deploy@192.168.1.100
```

---

## Part 2: Set up the Raspberry Pi

### 2.1 Flash Raspberry Pi OS

1. On any computer, download **Raspberry Pi Imager** from
   https://www.raspberrypi.com/software/
2. Insert your microSD card.
3. Open the Imager. Click **Choose Device** and select Raspberry Pi 3.
4. Click **Choose OS** > **Raspberry Pi OS (other)** > **Raspberry Pi OS Lite
   (64-bit)**. This is a headless (no desktop) image.
5. Click **Choose Storage** and select your microSD card.
6. Click **Next**. When asked "Would you like to apply OS customisation
   settings?", click **Edit Settings**.

**In the General tab:**
- Set hostname: `hive-pi`
- Set username: `pi`
- Set password: (pick something)
- If using WiFi: enter your SSID and password

**In the Services tab:**
- Check **Enable SSH**
- Select **Use password authentication**

Click **Save**, then **Yes**. Wait for it to finish writing.

### 2.2 Boot the Pi and find its IP

1. Insert the microSD into the Pi.
2. Connect Ethernet (or rely on the WiFi you configured).
3. Power it on. Wait ~60 seconds for it to boot.

From your NixOS desktop, find the Pi:

```bash
# Scan your subnet
sudo nmap -sn 192.168.1.0/24 | grep -B 2 -i raspberry
```

Or check your router's DHCP client list for a device named `hive-pi`.

Or try mDNS:
```bash
ping hive-pi.local
```

### 2.3 SSH into the Pi

```bash
ssh pi@192.168.1.XXX     # Replace with the Pi's IP
```

### 2.4 Set a static IP on the Pi (recommended)

Raspberry Pi OS uses NetworkManager:

```bash
# See your connection name
nmcli connection show

# Set static IP (replace "Wired connection 1" if yours is different)
sudo nmcli connection modify "Wired connection 1" \
  ipv4.addresses 192.168.1.200/24 \
  ipv4.gateway 192.168.1.1 \
  ipv4.dns "1.1.1.1,8.8.8.8" \
  ipv4.method manual

# Activate (your SSH session will drop — reconnect at the new IP)
sudo nmcli connection down "Wired connection 1"
sudo nmcli connection up "Wired connection 1"
```

Reconnect:
```bash
ssh pi@192.168.1.200
```

### 2.5 Install Node.js 22 on the Pi

OpenClaw requires Node.js 22+.

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg

sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg

echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
  | sudo tee /etc/apt/sources.list.d/nodesource.list

sudo apt-get update
sudo apt-get install -y nodejs

node --version   # Should print v22.x.x
```

---

## Part 3: Create accounts (OpenRouter + Telegram)

### 3.1 Get an OpenRouter API key

OpenRouter is an API gateway that lets you use many LLM models (including Kimi
K2.5) through one API key.

1. Go to **https://openrouter.ai/** and sign in (Google, GitHub, or email).
2. Go to **https://openrouter.ai/credits** and add $5-10 in credits. Kimi K2.5
   costs ~$0.38 per million input tokens — $5 goes a long way.
3. Go to **https://openrouter.ai/settings/keys** and click **Create Key**.
4. Name it (e.g., `hive`), optionally set a spending limit, click **Create**.
5. **Copy the key immediately** — it looks like `sk-or-v1-abc123...` and is
   shown only once. Save it somewhere safe.

### 3.2 Create a Telegram bot

1. Open Telegram on your phone or desktop.
2. Search for **@BotFather** and start a conversation.
3. Send `/newbot`.
4. Enter a display name (e.g., `Hive Assistant`).
5. Enter a username ending in `bot` (e.g., `hive_assistant_bot`).
6. BotFather replies with your bot token. It looks like:
   ```
   7123456789:AAH1bGciOiJSUzI1NiIsInR5...
   ```
   **Copy and save this token.**

### 3.3 Get your Telegram user ID

Your user ID is a number (not your @username). You need it to restrict the bot
so only you can talk to it.

1. In Telegram, search for **@userinfobot** and start a conversation.
2. It immediately replies with your info:
   ```
   Id: 123456789
   First: Brendan
   ```
3. **Copy the Id number.**

---

## Part 4: Build and configure Hive

All of this happens on your **NixOS desktop**.

### Option A: NixOS flake (recommended)

If your desktop runs NixOS, add Hive to your system configuration:

Edit `/etc/nixos/flake.nix` (or create one):

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    hive.url = "github:brmurrell3/hive";
  };

  outputs = { self, nixpkgs, hive, ... }: {
    nixosConfigurations.hive-desktop = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        hive.nixosModules.default
        {
          services.hived = {
            enable = true;
            clusterRoot = "/home/deploy/hive-cluster";
            openFirewall = true;
          };
        }
      ];
    };
  };
}
```

This gives you:
- `hived` running as a systemd service (auto-starts on boot)
- `hivectl` on your PATH
- Port 4222 opened in the firewall
- A dedicated `hive` system user

Apply:
```bash
sudo nixos-rebuild switch
```

Then skip to step 4.2.

### Option B: Build from source

```bash
cd ~
git clone https://github.com/brmurrell3/hive.git
cd hive

# Build the control plane and CLI for your desktop
make build
```

### 4.2 Run the setup wizard

The wizard prompts for your API keys and generates everything — cluster config,
agent manifest, OpenClaw config, join token, and a Pi bootstrap script:

```bash
hivectl init ~/hive-cluster
# or, if built from source:
# bin/hivectl init ~/hive-cluster
```

The wizard asks:
```
Cluster name [my-cluster]:
Agent ID [assistant]:
OpenRouter API key: sk-or-v1-YOUR-KEY
Telegram bot token: 7123456789:AAH1bGci...
Telegram user ID: 123456789
Desktop IP address [192.168.1.100]:
```

It generates:

| File | Purpose |
|------|---------|
| `cluster.yaml` | Cluster config (NATS bound to 0.0.0.0, clusterPort disabled) |
| `agents/assistant/manifest.yaml` | Agent with chat capability |
| `agents/assistant/openclaw.json` | OpenClaw config (API key + Telegram) |
| `teams/default.yaml` | Team with your agent as lead |
| `setup-pi.sh` | Pi bootstrap script with all values baked in |
| `state.db` | Pre-seeded with a join token |

> **CI/scripts:** Use `--non-interactive` to get the old behavior (example files,
> no prompts). Piped input also triggers non-interactive mode automatically.

### 4.3 Start the control plane

```bash
# If using NixOS flake, hived is already running as a service.
# If built from source:
cd ~/hive
bin/hived --cluster-root ~/hive-cluster
```

You should see:
```
{"level":"INFO","msg":"hived is ready","nats_url":"nats://0.0.0.0:4222"}
```

---

## Part 5: Set up the Pi (one command)

The wizard generated `setup-pi.sh` with all your keys and tokens baked in.
Copy it to the Pi and run it:

```bash
# From your desktop
scp ~/hive-cluster/setup-pi.sh pi@192.168.1.200:~/setup-pi.sh

# On the Pi
ssh pi@192.168.1.200 "bash ~/setup-pi.sh"
```

The script does everything automatically:
1. Installs Node.js 22 (via NodeSource)
2. Installs OpenClaw (`npm install -g openclaw@latest`)
3. Writes `~/.openclaw/openclaw.json` with your API key and Telegram config
4. Downloads the `hive-agent` binary (or tells you how to SCP it)
5. Writes the agent manifest
6. Creates and starts a systemd service

Each step is idempotent — safe to re-run if something fails partway through.

After it finishes:
```bash
# Check it's running
sudo systemctl status hive-agent

# Watch logs
journalctl -u hive-agent -f
```

### Manual setup (if you prefer)

<details>
<summary>Click to expand manual Pi setup steps</summary>

If you'd rather set things up by hand, the wizard-generated `setup-pi.sh` is
a readable bash script — open it to see exactly what it does, or follow the
steps below.

#### Copy the agent binary

```bash
# Cross-compile on your desktop
cd ~/hive
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/hive-agent-arm64 ./cmd/hive-agent
scp bin/hive-agent-arm64 pi@192.168.1.200:/home/pi/hive-agent
ssh pi@192.168.1.200 "chmod +x /home/pi/hive-agent"
```

#### Install Node.js 22 and OpenClaw

```bash
# On the Pi
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
  | sudo gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" \
  | sudo tee /etc/apt/sources.list.d/nodesource.list
sudo apt-get update
sudo apt-get install -y nodejs
sudo npm install -g openclaw@latest
```

#### Configure OpenClaw

Create `~/.openclaw/openclaw.json` with your keys (see the generated
`agents/assistant/openclaw.json` in your cluster root for the exact format).

#### Join the cluster

```bash
/home/pi/hive-agent join \
  --token PASTE_TOKEN_HERE \
  --control-plane 192.168.1.100:4222 \
  --agent-id assistant \
  --manifest /home/pi/manifest.yaml \
  --runtime-cmd openclaw \
  --runtime-args start \
  --work-dir /home/pi/hive-workspace \
  --http-addr :9100
```

</details>

---

## Part 6: Verify everything works

On the desktop:

```bash
# Is the Pi registered as a node?
hivectl --cluster-root ~/hive-cluster nodes list

# Is the agent running?
hivectl --cluster-root ~/hive-cluster agents list

# Detailed agent status
hivectl --cluster-root ~/hive-cluster agents status assistant
```

Open Telegram and message your bot. It should reply using Kimi K2.5.

Both services survive reboots automatically:
- **Desktop:** If using the NixOS flake, hived runs as a systemd service. If
  built from source, see the manual systemd setup in the collapsed section below.
- **Pi:** `setup-pi.sh` already created and enabled a systemd service.

<details>
<summary>Manual hived systemd setup (if not using the NixOS flake)</summary>

Edit `/etc/nixos/configuration.nix` and add:

```nix
systemd.services.hived = {
  description = "Hive Control Plane";
  after = [ "network.target" ];
  wantedBy = [ "multi-user.target" ];
  serviceConfig = {
    User = "deploy";
    ExecStart = "/home/deploy/hive/bin/hived --cluster-root /home/deploy/hive-cluster";
    Restart = "on-failure";
    RestartSec = 5;
  };
};
```

Apply:
```bash
sudo nixos-rebuild switch
```

</details>

---

## Part 7: Day-to-day usage

### Managing the agent from the desktop

```bash
# Stop the agent (sends graceful shutdown to the Pi)
hivectl --cluster-root ~/hive-cluster agents stop assistant

# Start it again
hivectl --cluster-root ~/hive-cluster agents start assistant

# Restart it
hivectl --cluster-root ~/hive-cluster agents restart assistant

# Check status
hivectl --cluster-root ~/hive-cluster agents status assistant

# List all agents
hivectl --cluster-root ~/hive-cluster agents list

# List all nodes
hivectl --cluster-root ~/hive-cluster nodes list
```

### Changing agent behavior (hot reload)

You can change how the agent behaves without restarting it. Create or edit these
files in `~/hive-cluster/agents/assistant/` on the desktop:

**`AGENTS.md`** — instructions for the agent:
```bash
cat > ~/hive-cluster/agents/assistant/AGENTS.md << 'EOF'
# Assistant

You are a helpful personal assistant running on a Raspberry Pi.

## Rules
- Be concise in Telegram messages (people read them on phones)
- Use MEMORY.md to remember things across conversations
- When asked about the weather, say you don't have internet access yet

## Personality
- Friendly but not chatty
- Technical when needed, simple when not
EOF
```

**`MEMORY.md`** — persistent memory the agent reads and writes:
```bash
cat > ~/hive-cluster/agents/assistant/MEMORY.md << 'EOF'
# Memory

## About the user
- (Agent will fill this in as it learns)

## Ongoing tasks
- (Agent will track tasks here)
EOF
```

The reconciler detects changes within 5 seconds and syncs them to the Pi's
workspace. OpenClaw picks up the new instructions on the next message.

Changes to `manifest.yaml` (capabilities, resources, runtime type) require an
agent restart.

### Token management

The wizard pre-seeds a join token during `hivectl init`. You can also manage
tokens manually:

```bash
# Create a new token
hivectl --cluster-root ~/hive-cluster tokens create

# Create a token that expires in 24 hours
hivectl --cluster-root ~/hive-cluster tokens create --ttl 24h

# List all tokens
hivectl --cluster-root ~/hive-cluster tokens list

# Revoke a token (use the prefix shown in tokens list)
hivectl --cluster-root ~/hive-cluster tokens revoke a1b2c3d4
```

---

## Troubleshooting

### "connection refused" when the Pi tries to join

1. Is hived running on the desktop?
   ```bash
   sudo systemctl status hived
   ```
2. Is port 4222 open?
   ```bash
   # On the desktop
   ss -tlnp | grep 4222
   ```
3. Can the Pi reach the desktop?
   ```bash
   # On the Pi
   ping 192.168.1.100
   nc -zv 192.168.1.100 4222
   ```
4. Is the NixOS firewall blocking it? Check that
   `networking.firewall.allowedTCPPorts` includes `4222`.

### Agent joins but OpenClaw doesn't start

1. Is Node.js installed?
   ```bash
   node --version    # needs 22+
   ```
2. Is OpenClaw installed?
   ```bash
   which openclaw
   ```
3. Is the API key set?
   ```bash
   echo $OPENROUTER_API_KEY
   ```
4. Try running OpenClaw directly to see errors:
   ```bash
   openclaw start
   ```

### Telegram bot doesn't respond

1. Check the bot token is correct in `~/.openclaw/openclaw.json`.
2. Check your user ID is in `allowFrom` (it must be `tg:` followed by the
   numeric ID, not your @username).
3. Make sure you started a conversation with the bot (search for it in Telegram
   and press Start).
4. Check logs:
   ```bash
   journalctl -u hive-agent -f
   ```

### Agent shows FAILED

The agent crashed too many times (5 by default). Check what went wrong:
```bash
journalctl -u hive-agent --no-pager | tail -100
```

Fix the issue, then restart manually:
```bash
# From the desktop
hivectl --cluster-root ~/hive-cluster agents restart assistant

# Or on the Pi
sudo systemctl restart hive-agent
```

### NATS authentication error

The `--token` flag on `hive-agent join` is the **join token** (created by
`hivectl tokens create`), not the NATS auth token. The agent receives the NATS
auth token automatically during the join handshake. If you see auth errors, the
join token may be expired or revoked — create a new one.

### Pi runs out of memory

Kimi K2.5 runs remotely (via OpenRouter API), not on the Pi. The Pi only runs
OpenClaw (~100-200MB) and the Hive agent (~10MB). If you're running out of the
Pi 3's 1GB RAM, check for other processes:
```bash
free -h
top
```

---

## Reference: what lives where

### Desktop

| Path | What |
|------|------|
| `~/hive/` | Hive source code (if built from source) |
| `~/hive-cluster/cluster.yaml` | Cluster config (generated by wizard) |
| `~/hive-cluster/agents/assistant/manifest.yaml` | Agent definition |
| `~/hive-cluster/agents/assistant/openclaw.json` | OpenClaw config (API keys) |
| `~/hive-cluster/agents/assistant/AGENTS.md` | Agent instructions |
| `~/hive-cluster/agents/assistant/MEMORY.md` | Agent memory |
| `~/hive-cluster/teams/default.yaml` | Team definition |
| `~/hive-cluster/setup-pi.sh` | Pi bootstrap script (generated by wizard) |
| `~/hive-cluster/state.db` | SQLite state (pre-seeded by wizard) |
| `~/hive-cluster/.state/nats-auth-token` | NATS auth token (auto-generated) |

### Raspberry Pi

| Path | What |
|------|------|
| `/home/pi/hive-agent` | Agent binary |
| `/home/pi/manifest.yaml` | Agent manifest (copy from desktop) |
| `/home/pi/hive-workspace/` | Agent working directory |
| `~/.openclaw/openclaw.json` | OpenClaw config (LLM + Telegram) |
