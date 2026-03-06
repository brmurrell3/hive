# Quickstart

> Three commands. Desktop, wizard, Pi.

This guide gets you from zero to a working Telegram AI agent in about 10
minutes. You'll run the control plane on your desktop and the agent on a
Raspberry Pi.

**Prerequisites:** A NixOS desktop and a Raspberry Pi (with Raspberry Pi OS)
on the same network, both reachable via SSH. If you need to set up the hardware
first, see the [deployment guide](deployment-guide.md) (Parts 1-2).

---

## Before you start

You need three things from external services. Get these first — the wizard will
ask for them.

### OpenRouter API key

1. Sign in at [openrouter.ai](https://openrouter.ai/).
2. Add credits at [openrouter.ai/credits](https://openrouter.ai/credits) — $5
   is plenty.
3. Create a key at
   [openrouter.ai/settings/keys](https://openrouter.ai/settings/keys).
4. Copy the key (`sk-or-v1-...`). You won't see it again.

### Telegram bot token

1. Open Telegram. Search for **@BotFather** and send `/newbot`.
2. Pick a display name and a username (must end in `bot`).
3. Copy the bot token BotFather gives you
   (`7123456789:AAH1bGci...`).

### Telegram user ID

1. In Telegram, search for **@userinfobot**.
2. It replies with your numeric user ID. Copy the number.

---

## Step 1: Install Hive on your desktop

### NixOS (recommended)

Add the Hive flake to your system configuration. Create or edit
`/etc/nixos/flake.nix`:

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    hive.url = "github:brmurrell3/hive";
  };

  outputs = { self, nixpkgs, hive, ... }: {
    nixosConfigurations.my-host = nixpkgs.lib.nixosSystem {
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

Apply it:

```bash
sudo nixos-rebuild switch
```

This installs `hived` as a systemd service, puts `hivectl` on your PATH, and
opens port 4222 in the firewall. hived won't start until the cluster root
directory exists — that's the next step.

### From source (any Linux)

```bash
git clone https://github.com/brmurrell3/hive.git ~/hive
cd ~/hive && make build
```

Binaries go to `~/hive/bin/`. Use `bin/hivectl` and `bin/hived` below instead
of the bare command names.

---

## Step 2: Run the wizard

```bash
hivectl init ~/hive-cluster
```

The wizard prompts for six values:

```
Cluster name [my-cluster]:  home
Agent ID [assistant]:
OpenRouter API key:         sk-or-v1-abc123...
Telegram bot token:         7123456789:AAH1bGci...
Telegram user ID:           123456789
Desktop IP address [auto]:
```

Press Enter to accept the defaults shown in brackets. Paste your keys when
prompted — they are not echoed.

When it finishes, `~/hive-cluster/` contains:

```
hive-cluster/
  cluster.yaml              # NATS config, bound to 0.0.0.0
  agents/assistant/
    manifest.yaml            # agent definition
    openclaw.json            # API key + Telegram config
  teams/default.yaml         # team with assistant as lead
  setup-pi.sh                # Pi bootstrap script (all values baked in)
  state.db                   # pre-seeded join token
```

If you used the NixOS flake, restart hived so it picks up the new cluster root:

```bash
sudo systemctl restart hived
```

If you built from source, start hived manually:

```bash
~/hive/bin/hived --cluster-root ~/hive-cluster
```

Confirm it's listening:

```
{"level":"INFO","msg":"hived is ready","nats_url":"nats://0.0.0.0:4222"}
```

---

## Step 3: Bootstrap the Pi

Copy the generated script to the Pi and run it:

```bash
scp ~/hive-cluster/setup-pi.sh pi@<PI_IP>:~/setup-pi.sh
ssh pi@<PI_IP> "bash ~/setup-pi.sh"
```

Replace `<PI_IP>` with your Pi's IP address (e.g., `192.168.1.200`).

The script installs Node.js 22, OpenClaw, the hive-agent binary, writes all
config files, and creates a systemd service. Every step is idempotent — safe to
re-run if something fails partway.

Output looks like:

```
=== Hive Pi Bootstrap ===
Control plane: 192.168.1.100:4222
Agent ID:      assistant

[1/6] Installing Node.js 22...
[2/6] Installing OpenClaw...
[3/6] Writing OpenClaw config...
[4/6] Installing hive-agent...
[5/6] Writing agent manifest...
[6/6] Creating systemd service...

=== Done ===
```

---

## Verify

On the desktop:

```bash
hivectl --cluster-root ~/hive-cluster nodes list
hivectl --cluster-root ~/hive-cluster agents list
```

You should see your Pi as a registered node and the assistant agent running.

Open Telegram and send a message to your bot. It should reply.

---

## What just happened

```
  You                    Telegram              Raspberry Pi            Desktop
   |                        |                      |                      |
   |-- message ------------>|                      |                      |
   |                        |-- webhook ---------->|                      |
   |                        |                      | OpenClaw receives    |
   |                        |                      | msg, calls Kimi K2.5 |
   |                        |                      | via OpenRouter API   |
   |                        |<-- reply ------------|                      |
   |<-- reply --------------|                      |                      |
   |                        |                      |-- heartbeat -------->|
   |                        |                      |                  hived
   |                        |                      |              monitors
   |                        |                      |              health +
   |                        |                      |              restarts
```

- **hived** (desktop) monitors the agent and restarts it if it crashes.
- **hive-agent** (Pi) runs OpenClaw, handles Telegram messages, and reports
  health to the control plane every 30 seconds.
- All communication between the Pi and desktop goes over NATS on port 4222.

---

## Next steps

### Give your agent instructions

Create `~/hive-cluster/agents/assistant/AGENTS.md` on the desktop:

```markdown
# Assistant

You are a helpful personal assistant.

## Rules
- Be concise — people read your replies on phones
- Use MEMORY.md to remember things across conversations
```

The reconciler syncs changes to the Pi within 5 seconds. OpenClaw picks up the
new instructions on the next message — no restart needed.

### Give your agent memory

Create `~/hive-cluster/agents/assistant/MEMORY.md`:

```markdown
# Memory

(The agent reads and writes this file to remember things between conversations.)
```

### Add more agents

Run the wizard again with a different path, or create agent directories
manually:

```bash
mkdir -p ~/hive-cluster/agents/researcher
# write manifest.yaml, openclaw.json
# update teams/default.yaml
```

### Manage the cluster

```bash
hivectl --cluster-root ~/hive-cluster agents status assistant
hivectl --cluster-root ~/hive-cluster agents restart assistant
hivectl --cluster-root ~/hive-cluster tokens list
hivectl --cluster-root ~/hive-cluster tokens create --ttl 24h
```

### Set `HIVE_CONFIG` to skip `--cluster-root` everywhere

```bash
export HIVE_CONFIG=~/hive-cluster
hivectl agents list    # much shorter
```

Add this to your `~/.bashrc` or `~/.zshrc`.

---

## Troubleshooting

| Problem | Check |
|---------|-------|
| Pi can't connect to desktop | `ping <DESKTOP_IP>` and `nc -zv <DESKTOP_IP> 4222` from the Pi. Is the firewall open? |
| Agent joins but OpenClaw won't start | SSH to Pi, run `openclaw start` manually to see errors. Check `~/.openclaw/openclaw.json` has correct keys. |
| Telegram bot doesn't respond | Verify bot token in `openclaw.json`. Verify `allowFrom` has `tg:` + your numeric ID (not @username). Send `/start` to the bot first. |
| Agent shows FAILED | `journalctl -u hive-agent --no-pager \| tail -50` on the Pi. Fix the issue, then `sudo systemctl restart hive-agent`. |
| NATS auth error | The join token may be expired. Create a new one: `hivectl tokens create`. Update the systemd service on the Pi with the new token and restart. |

For detailed hardware setup (NixOS install, Pi OS flashing, static IPs), see
the full [deployment guide](deployment-guide.md).
