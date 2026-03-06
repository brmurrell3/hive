# Hive Operations Guide

Complete guide to building, running, and testing every feature of Hive end-to-end.

---

## Table of Contents

1. [Prerequisites](#1-prerequisites)
2. [Build Everything](#2-build-everything)
3. [Run the Test Suite](#3-run-the-test-suite)
4. [Scaffold a Cluster](#4-scaffold-a-cluster)
5. [Start the Control Plane](#5-start-the-control-plane)
6. [Validate Configuration](#6-validate-configuration)
7. [Manage Agents (VM Lifecycle)](#7-manage-agents-vm-lifecycle)
8. [Join Tokens](#8-join-tokens)
9. [Node Management](#9-node-management)
10. [Join a Tier 2 Native Agent](#10-join-a-tier-2-native-agent)
11. [RBAC and User Management](#11-rbac-and-user-management)
12. [Firmware Agents (Tier 3)](#12-firmware-agents-tier-3)
13. [Dashboard and Web UI](#13-dashboard-and-web-ui)
14. [Prometheus Metrics](#14-prometheus-metrics)
15. [Log Aggregation](#15-log-aggregation)
16. [NATS Messaging and Pub/Sub](#16-nats-messaging-and-pubsub)
17. [Build the Rootfs Images](#17-build-the-rootfs-images)
18. [Production Hardening](#18-production-hardening)
19. [Full End-to-End Walkthrough](#19-full-end-to-end-walkthrough)
20. [Troubleshooting](#20-troubleshooting)

---

## 1. Prerequisites

**Required:**
- Go 1.23+ (`go version`)
- Linux host for VM features (Firecracker requires `/dev/kvm`)
- macOS works for everything except VM lifecycle (use `HIVE_TEST_FIRECRACKER=mock`)

**Optional (per feature):**
- Docker — for building the Alpine rootfs (`rootfs/build-rootfs.sh`)
- Nix with flakes — for building the NixOS rootfs (`rootfs/nixos/`)
- Firecracker binary — for real VM lifecycle (not needed for mock mode)
- `nats` CLI — for manual pub/sub testing (`go install github.com/nats-io/natscli/nats@latest`)
- ESP-IDF / Arduino CLI / Pico SDK — only for firmware builds
- A serial device — only for firmware flash/monitor

---

## 2. Build Everything

```bash
# From the project root:
cd path/to/hive

# Build all three binaries
make build

# This produces:
#   ./hived        — control plane daemon
#   ./hivectl      — CLI tool
#   ./hive-agent   — tier 2 native agent
```

Verify the builds:

```bash
./hivectl version
# hivectl v0.1.0

./hive-agent version
# hive-agent dev darwin/arm64
```

---

## 3. Run the Test Suite

```bash
# Unit tests (no external dependencies, fast)
make test-unit

# Integration tests (starts embedded NATS, uses filesystem)
make test-integration

# Both at once
make test

# VM tests (requires /dev/kvm — Linux only)
make test-vm

# Full regression with race detection
go test -tags unit,integration -race -count=1 -timeout 5m ./...
```

Current test coverage: 22 packages, all passing.

---

## 4. Scaffold a Cluster

Create a fresh cluster directory with all the template files:

```bash
./hivectl init my-cluster
```

This creates:

```
my-cluster/
├── cluster.yaml                    # Cluster configuration
├── agents/
│   └── example-agent/
│       └── manifest.yaml           # Example agent manifest
└── teams/
    └── default.yaml                # Default team
```

Examine the generated files:

```bash
cat my-cluster/cluster.yaml
cat my-cluster/agents/example-agent/manifest.yaml
cat my-cluster/teams/default.yaml
```

---

## 5. Start the Control Plane

```bash
# Start hived pointing at the cluster directory
./hived --cluster-root my-cluster
```

Output (JSON structured logs to stdout):

```json
{"level":"INFO","msg":"starting hived","cluster_root":"/path/to/my-cluster"}
{"level":"INFO","msg":"cluster config loaded","name":"my-cluster","nats_port":4222,"jetstream":true}
{"level":"INFO","msg":"hived is ready","nats_url":"nats://127.0.0.1:4222"}
```

hived runs in the foreground. It:
- Embeds a NATS server (no external NATS needed)
- Listens on the port from `cluster.yaml` (default 4222)
- Enables JetStream for persistent messaging
- Stores JetStream data at `my-cluster/.state/jetstream/`
- Handles `SIGTERM`/`SIGINT` for graceful shutdown

**To stop:** `Ctrl+C` or `kill <pid>`

---

## 6. Validate Configuration

Validate all YAML manifests (cluster + agents + teams) without starting anything:

```bash
./hivectl validate --cluster-root my-cluster
# Validation passed.
```

If there are errors (e.g., missing required fields, invalid agent IDs, duplicate IDs):

```bash
# Example: invalid agent ID
./hivectl validate --cluster-root my-cluster
# Error: agent ID "-bad-id" is invalid: must match [a-z0-9][a-z0-9-]{0,62}
```

---

## 7. Manage Agents (VM Lifecycle)

Agents run inside Firecracker VMs on Linux. On macOS or without `/dev/kvm`, use mock mode:

```bash
export HIVE_TEST_FIRECRACKER=mock
```

### Create an Agent Manifest

```bash
mkdir -p my-cluster/agents/my-agent

cat > my-cluster/agents/my-agent/manifest.yaml << 'EOF'
apiVersion: hive/v1
kind: Agent
metadata:
  id: my-agent
  team: default
spec:
  runtime:
    type: openclaw
    model:
      provider: anthropic
      name: claude-sonnet-4-5
  capabilities:
    - name: summarize
      description: Summarize text
      inputs:
        - name: text
          type: string
          description: Text to summarize
      outputs:
        - name: summary
          type: string
          description: The summary
  resources:
    memory: "512Mi"
    vcpus: 2
EOF
```

### Start, Stop, Restart, Destroy

```bash
# Start an agent (provisions VM, transitions PENDING → CREATING → STARTING → RUNNING)
./hivectl agents start my-agent --cluster-root my-cluster
# Agent my-agent started

# List all agents
./hivectl agents list --cluster-root my-cluster
# AGENT_ID    TEAM     STATE    UPTIME
# my-agent    default  RUNNING  5s

# Detailed status (JSON output)
./hivectl agents status my-agent --cluster-root my-cluster

# Stop an agent (RUNNING → STOPPING → STOPPED)
./hivectl agents stop my-agent --cluster-root my-cluster
# Agent my-agent stopped

# Restart (stop + start, resets restart counter)
./hivectl agents restart my-agent --cluster-root my-cluster
# Agent my-agent restarted

# Destroy (force kill, delete rootfs copy, remove from state)
./hivectl agents destroy my-agent --cluster-root my-cluster
# Agent my-agent destroyed
```

### State File

All runtime state is persisted atomically to `my-cluster/state.db`:

```bash
cat my-cluster/state.db | python3 -m json.tool
```

```json
{
  "agents": {
    "my-agent": {
      "id": "my-agent",
      "team": "default",
      "status": "RUNNING",
      "vm_pid": 12345,
      "vm_cid": 3,
      "vm_socket_path": ".state/agents/my-agent/firecracker.sock",
      "rootfs_copy_path": ".state/agents/my-agent/rootfs.ext4",
      "restart_count": 0,
      "last_transition": "2026-03-03T10:00:00Z",
      "started_at": "2026-03-03T10:00:00Z"
    }
  }
}
```

### Agent State Machine

```
PENDING → CREATING → STARTING → RUNNING → STOPPING → STOPPED
                 ↘              ↘          ↘
                  ←←←← FAILED ←←←←←←←←←←←←←
STOPPED → CREATING  (restart)
FAILED  → CREATING  (restart)
```

---

## 8. Join Tokens

Tokens authenticate Tier 2/3 nodes joining the cluster. The raw token is shown once at creation time; only a SHA-256 hash is stored.

```bash
# Create a token (no expiry)
./hivectl tokens create --cluster-root my-cluster
# a1b2c3d4e5f6...  (64 hex chars — save this!)

# Create a token with TTL
./hivectl tokens create --ttl 24h --cluster-root my-cluster
./hivectl tokens create --ttl 168h --cluster-root my-cluster  # 7 days

# List tokens
./hivectl tokens list --cluster-root my-cluster
# PREFIX    CREATED                    EXPIRES                    LAST USED  STATUS
# a1b2c3d4  2026-03-03T10:00:00Z       -                          -          active

# Revoke a token by prefix
./hivectl tokens revoke a1b2c3d4 --cluster-root my-cluster
# Token a1b2c3d4 revoked
```

---

## 9. Node Management

Nodes are registered when agents join via `hive-agent join`. Once registered, you can manage them:

```bash
# List all nodes
./hivectl nodes list --cluster-root my-cluster
# NODE_ID       TIER  ARCH      STATUS  MEMORY  CPUS  AGENTS
# pi4-aarch64   2     aarch64   online  4.0Gi   4     1

# Detailed status (JSON)
./hivectl nodes status pi4-aarch64 --cluster-root my-cluster

# Cordon a node (prevents new agent scheduling, existing agents stay)
./hivectl nodes cordon pi4-aarch64 --cluster-root my-cluster
# Node pi4-aarch64 cordoned

# Drain a node (prevents scheduling, signals migration)
./hivectl nodes drain pi4-aarch64 --cluster-root my-cluster
# Node pi4-aarch64 marked as draining

# Uncordon (return to online)
./hivectl nodes uncordon pi4-aarch64 --cluster-root my-cluster
# Node pi4-aarch64 uncordoned (now online)

# Add labels
./hivectl nodes label pi4-aarch64 env=prod gpu=none --cluster-root my-cluster
# Node pi4-aarch64 labeled

# Remove labels
./hivectl nodes unlabel pi4-aarch64 gpu --cluster-root my-cluster
# Node pi4-aarch64 unlabeled
```

### Node Tiers

Tier classification is automatic based on hardware:
- **Tier 1:** KVM available AND >= 4GB RAM (can run Firecracker VMs)
- **Tier 2:** Everything else — RPis, workstations without KVM (native agents)
- **Tier 3:** Firmware devices (ESP32, Pico, etc.)

---

## 10. Join a Tier 2 Native Agent

Tier 2 agents run natively on hardware (no VM). They use `hive-agent join` to connect to the control plane.

**On the control plane host** (first create a join token):

```bash
./hivectl tokens create --cluster-root my-cluster
# Copy the output token: a1b2c3d4e5f6...
```

**On the Tier 2 node** (e.g., a Raspberry Pi):

```bash
# Cross-compile the agent binary for the target
GOOS=linux GOARCH=arm64 go build -o hive-agent ./cmd/hive-agent

# Copy to the target and run:
./hive-agent join \
    --token a1b2c3d4e5f6... \
    --control-plane 192.168.1.10:4222 \
    --agent-id my-pi-agent \
    --http-addr :9100 \
    --work-dir /var/lib/hive/workspace
```

The agent will:
1. Connect to NATS at the control plane address
2. Send a join request with hardware inventory (CPU count, memory, KVM availability)
3. Receive a join response (accepted/rejected)
4. Start the sidecar in library mode
5. Begin sending heartbeats on `hive.health.my-pi-agent`
6. Listen for tasks on `hive.agent.my-pi-agent.inbox`

**Verify on the control plane:**

```bash
./hivectl nodes list --cluster-root my-cluster
# Should show the new node

./hivectl agents list --cluster-root my-cluster
# Should show the agent in RUNNING state
```

---

## 11. RBAC and User Management

Three roles with different permissions:

| Role | Actions | Scope |
|------|---------|-------|
| `admin` | Everything | All resources |
| `operator` | start, stop, restart, destroy, list, status, logs | Assigned teams/agents |
| `viewer` | list, status, logs | Assigned teams/agents |

```bash
# Create an admin user
./hivectl users create alice --role admin --cluster-root my-cluster
# User alice created with role admin
# Token: hive-user-a1b2c3d4...  (save this!)

# Create an operator scoped to a team
./hivectl users create bob --role operator --teams default --cluster-root my-cluster

# Create a viewer scoped to specific agents
./hivectl users create carol --role viewer --agents my-agent,other-agent --cluster-root my-cluster

# List users
./hivectl users list --cluster-root my-cluster
# USER_ID  ROLE      TEAMS    AGENTS
# alice    admin     -        -
# bob      operator  default  -
# carol    viewer    -        my-agent,other-agent

# Update a user's role or scope
./hivectl users update bob --role admin --cluster-root my-cluster

# Clear a user's team scope
./hivectl users update bob --teams "" --cluster-root my-cluster

# Revoke a user (removes from RBAC entirely)
./hivectl users revoke carol --cluster-root my-cluster
# User carol revoked
```

---

## 12. Firmware Agents (Tier 3)

Firmware agents run on microcontrollers (ESP32, Raspberry Pi Pico, etc.) and communicate via MQTT.

### SDK Setup

Two SDKs are provided:

**C SDK** (for ESP-IDF, Arduino, Zephyr, bare metal):
```
sdk/firmware-c/
├── include/hive.h          # API header
├── src/hive.c              # Implementation (no malloc, hand-written JSON)
├── CMakeLists.txt           # Multi-platform CMake
└── examples/
    └── esp32-sensor/main.c  # ESP32 temperature sensor example
```

**MicroPython SDK** (for RPi Pico W, ESP32 with MicroPython):
```
sdk/firmware-micropython/
├── hive_agent.py            # HiveAgent class
└── examples/
    └── pico_gpio.py         # Pi Pico W GPIO example
```

### Create a Firmware Agent Manifest

```bash
mkdir -p my-cluster/agents/sensor-01/firmware

cat > my-cluster/agents/sensor-01/manifest.yaml << 'EOF'
apiVersion: hive/v1
kind: Agent
metadata:
  id: sensor-01
  team: default
spec:
  tier: firmware
  firmware:
    platform: esp-idf
    board: esp32
  capabilities:
    - name: read-temperature
      description: Read temperature from BME280 sensor
      outputs:
        - name: temperature
          type: string
  hardware:
    sensors: [bme280]
EOF
```

### Build, Flash, Monitor

```bash
# Build firmware (requires platform toolchain to be installed)
./hivectl firmware build sensor-01 --cluster-root my-cluster
# Firmware built successfully
#   Binary: /path/to/agents/sensor-01/firmware/build/firmware.bin
#   Size: 245760 bytes

# Override platform target
./hivectl firmware build sensor-01 --target arduino --cluster-root my-cluster

# Flash to device
./hivectl firmware flash sensor-01 --port /dev/ttyUSB0 --baud 460800 --cluster-root my-cluster
# Firmware flashed to /dev/ttyUSB0

# Open serial monitor for debugging
./hivectl firmware monitor sensor-01 --port /dev/ttyUSB0 --baud 115200 --cluster-root my-cluster
```

### Supported Platforms

| Platform | Toolchain | Build Command | Flash Command |
|----------|-----------|---------------|---------------|
| `esp-idf` | `idf.py` | `idf.py build` | `idf.py -p PORT flash` |
| `arduino` | `arduino-cli` | `arduino-cli compile` | `arduino-cli upload` |
| `pico-sdk` | `cmake` | `cmake --build` | `picotool load` |
| `zephyr` | `west` | `west build` | `west flash` |
| `bare-metal` | `make` | `make` | `make flash` |

### Firmware Device Communication

Firmware devices connect via MQTT (port 1883) using:
- Username: `agent_id` (e.g., `sensor-01`)
- Password: raw join token

The MQTT bridge translates between MQTT topics and NATS subjects:
- MQTT `hive/health/sensor-01` ↔ NATS `hive.health.sensor-01`
- MQTT `hive/control/sensor-01` ↔ NATS `hive.control.sensor-01`

---

## 13. Dashboard and Web UI

The dashboard is a single-page web application served by hived's HTTP server.

### Starting the Dashboard

The dashboard API server is at `internal/dashboard/api.go`. To integrate it with hived, it binds to `:8080` by default. It needs access to the state store and a NATS connection.

Currently the dashboard server is a library — you wire it into hived or run it standalone by writing a small main:

```go
// Example standalone dashboard runner
package main

import (
    "github.com/brmurrell3/hive/internal/dashboard"
    "github.com/brmurrell3/hive/internal/state"
)

func main() {
    store, _ := state.NewStore("my-cluster/state.db", logger)
    nc, _ := nats.Connect("nats://127.0.0.1:4222")
    srv := dashboard.NewServer(dashboard.Config{
        Store:    store,
        NATSConn: nc,
        Addr:     ":8080",
    })
    srv.Start()
}
```

### REST API Endpoints

Once running, the following endpoints are available:

```bash
# Cluster overview
curl http://localhost:8080/api/cluster
# {"node_count":2,"team_count":1,"agent_count":3,"uptime_seconds":120,"agent_status":{"RUNNING":2,"STOPPED":1}}

# List all agents
curl http://localhost:8080/api/agents
# [{"id":"my-agent","team":"default","status":"RUNNING",...},...]

# Agent detail
curl http://localhost:8080/api/agents/my-agent
# {"id":"my-agent","team":"default","status":"RUNNING","vm_pid":12345,...}

# List all nodes
curl http://localhost:8080/api/nodes
# [{"id":"node-1","tier":1,"arch":"x86_64","status":"online",...}]

# Node detail
curl http://localhost:8080/api/nodes/node-1

# All registered capabilities
curl http://localhost:8080/api/capabilities
# {"agents":{"my-agent":{"team_id":"default","capabilities":[...]}},"capabilities":{"summarize":["my-agent"]}}

# Chat with an agent (proxied via NATS, 10s timeout)
curl -X POST http://localhost:8080/api/agents/my-agent/chat \
  -H 'Content-Type: application/json' \
  -d '{"message":"Hello, what can you do?"}'
# {"agent_id":"my-agent","response":"I can summarize text..."}

# Query agent logs
curl http://localhost:8080/api/logs/my-agent?limit=50

# Static dashboard UI
open http://localhost:8080/
```

### WebSocket Live Events

Connect to `ws://localhost:8080/ws` for real-time events:

```bash
# Using websocat (install: cargo install websocat)
websocat ws://localhost:8080/ws
```

Events pushed by the server:

```json
{"type":"agent_state_change","data":{"agent_id":"my-agent","old_status":"RUNNING","new_status":"STOPPED"}}
{"type":"heartbeat","data":{"agent_id":"my-agent","healthy":true}}
{"type":"log_entry","data":{"agent_id":"my-agent","message":"processing request..."}}
```

### Dashboard UI Features

The embedded SPA at `http://localhost:8080/` provides:
- **Cluster Overview** — agent counts, node counts, status summary
- **Nodes** — table with tier, arch, status, labels
- **Agents** — table with click-through to detail view
- **Agent Detail** — full status, chat interface, live logs
- **Capabilities** — browse all registered capabilities by agent
- **Message Flow** — visual DAG of agent communication
- **Logs** — select agent, load/stream log entries

---

## 14. Prometheus Metrics

The metrics collector exposes a `/metrics` endpoint in Prometheus text exposition format.

### Testing Metrics

```bash
# After the dashboard/metrics server is running:
curl http://localhost:8080/metrics
```

Expected output:

```
# HELP hive_agents_total Number of agents by status
# TYPE hive_agents_total gauge
hive_agents_total{status="RUNNING"} 2
hive_agents_total{status="STOPPED"} 1

# HELP hive_nats_messages_total Total NATS messages by subject
# TYPE hive_nats_messages_total counter
hive_nats_messages_total{subject="hive.health"} 450

# HELP hive_capability_invocation_duration_ms Capability invocation latency
# TYPE hive_capability_invocation_duration_ms summary
hive_capability_invocation_duration_ms{capability="summarize",quantile="0.5"} 120.5
hive_capability_invocation_duration_ms{capability="summarize",quantile="0.9"} 350.2
hive_capability_invocation_duration_ms{capability="summarize",quantile="0.99"} 450.0
hive_capability_invocation_duration_ms_sum{capability="summarize"} 12050
hive_capability_invocation_duration_ms_count{capability="summarize"} 100

# HELP hive_heartbeat_healthy Agent heartbeat status (1=healthy, 0=unhealthy)
# TYPE hive_heartbeat_healthy gauge
hive_heartbeat_healthy{agent_id="my-agent"} 1

# HELP hive_node_memory_usage_percent Node memory usage percentage
# TYPE hive_node_memory_usage_percent gauge
hive_node_memory_usage_percent{node_id="node-1"} 65.2

# HELP hive_node_cpu_usage_percent Node CPU usage percentage
# TYPE hive_node_cpu_usage_percent gauge
hive_node_cpu_usage_percent{node_id="node-1"} 42.1
```

### Connecting to Grafana

Add to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'hive'
    static_configs:
      - targets: ['localhost:8080']
    scrape_interval: 15s
```

---

## 15. Log Aggregation

Agent logs are streamed via NATS and persisted to local JSONL files.

### Log Directory Structure

```
<cluster-root>/logs/
└── <agent-id>/
    ├── 2026-03-03.jsonl          # One file per day
    ├── 2026-03-03.1.jsonl        # Rotated when >100MB
    └── 2026-03-04.jsonl
```

Each line is a JSON log entry:

```json
{"agent_id":"my-agent","timestamp":"2026-03-03T10:00:00Z","level":"info","message":"processing request","fields":{"request_id":"abc123"}}
```

### Publishing Logs (from an agent/sidecar)

Agents publish logs as NATS envelopes on `hive.logs.<agent_id>`:

```bash
# Using nats CLI to simulate an agent log
nats pub hive.logs.my-agent '{
  "id": "log-1",
  "from": "my-agent",
  "to": "hived",
  "type": "status",
  "timestamp": "2026-03-03T10:00:00Z",
  "payload": {
    "agent_id": "my-agent",
    "timestamp": "2026-03-03T10:00:00Z",
    "level": "info",
    "message": "Hello from my-agent"
  }
}'
```

### Configuration

- **Retention:** 30 days by default (files older than this are deleted on startup)
- **Rotation:** Files exceeding 100MB are rotated to `{date}.1.jsonl`, `.2.jsonl`, etc.

---

## 16. NATS Messaging and Pub/Sub

With hived running, you can interact with the embedded NATS server using the `nats` CLI.

```bash
# Install nats CLI
go install github.com/nats-io/natscli/nats@latest

# Connect to the embedded server
export NATS_URL=nats://127.0.0.1:4222

# Check server info
nats server info

# Subscribe to health heartbeats
nats sub 'hive.health.>'

# Subscribe to all agent state changes
nats sub 'hive.agent.state.>'

# Subscribe to log messages
nats sub 'hive.logs.>'

# Publish a test heartbeat (simulating an agent)
nats pub hive.health.test-agent '{
  "id": "test-1",
  "from": "test-agent",
  "to": "hived",
  "type": "health",
  "timestamp": "2026-03-03T10:00:00Z",
  "payload": {
    "healthy": true,
    "uptime_seconds": 60,
    "tier": "vm"
  }
}'

# Publish a capability request
nats pub hive.capabilities.my-agent.summarize.request '{
  "id": "req-1",
  "from": "requester",
  "to": "my-agent",
  "type": "capability-request",
  "timestamp": "2026-03-03T10:00:00Z",
  "payload": {"text": "Summarize this document..."},
  "reply_to": "hive.capabilities.my-agent.summarize.response"
}'

# Listen for the response
nats sub hive.capabilities.my-agent.summarize.response

# Team broadcast
nats pub team.default.broadcast '{
  "id": "broadcast-1",
  "from": "team-lead",
  "to": "team.default",
  "type": "broadcast",
  "timestamp": "2026-03-03T10:00:00Z",
  "payload": {"message": "All agents: new task available"}
}'

# JetStream streams (check what exists)
nats stream list
```

### NATS Subject Reference

| Subject Pattern | Direction | Description |
|----------------|-----------|-------------|
| `hive.health.<agent_id>` | Agent → hived | Heartbeat from sidecar |
| `hive.control.<agent_id>` | hived → Agent | Control commands to sidecar |
| `hive.agent.<agent_id>.inbox` | Any → Agent | Agent message inbox |
| `hive.join.request` | Agent → hived | Tier 2 node join request |
| `hive.logs.<agent_id>` | Agent → hived | Log entries |
| `hive.agent.state.<agent_id>` | hived → All | State change notifications |
| `hive.capabilities.<agent>.<cap>.request` | Any → Agent | Capability invocation |
| `hive.capabilities.<agent>.<cap>.response` | Agent → Requester | Capability response |
| `team.<team_id>.broadcast` | Lead → Team | Team broadcast |
| `org.capabilities.<cap>` | Cross-team | Cross-team capability routing |
| `hive.director.<tool>.request` | Any → Director | Director agent tools |

---

## 17. Build the Rootfs Images

### Alpine Rootfs (current)

Requires Docker and sudo (for loop mounting):

```bash
cd rootfs

# Build the sidecar binary for Linux
make sidecar

# Build the rootfs image (512MB ext4)
make rootfs
# Output: rootfs/rootfs.ext4

# Or manually:
./build-rootfs.sh rootfs.ext4 512M hive-sidecar
```

The rootfs contains:
- Alpine 3.19 base
- `/usr/local/bin/hive-sidecar` — the sidecar binary
- `/init` — init script that mounts proc/sys/dev and exec's the sidecar
- `/workspace` — mount point for agent files

### NixOS Rootfs (production)

Requires Nix with flakes enabled:

```bash
cd rootfs/nixos

# Build the rootfs ext4 image
nix build .#rootfs
# Output: result/ (contains the ext4 image)

# Build the kernel (vmlinux for Firecracker direct boot)
nix build .#kernel
# Output: result/ (contains vmlinux)

# Build everything
nix build
# Default package is rootfs
```

The NixOS rootfs includes:
- Minimal NixOS with custom kernel (virtio, vsock, serial console)
- systemd service for hive-sidecar at `/opt/hive/sidecar`
- Directories: `/opt/hive/agent`, `/opt/hive/tools`, `/workspace`
- vsock device access for host-guest NATS bridge
- Serial console auto-login for debugging
- Packages: bash, coreutils, iproute2, curl, cacert, procps, strace

---

## 18. Production Hardening

### Graceful Shutdown

Sending `SIGTERM` or `SIGINT` to hived triggers graceful shutdown:

```bash
kill -TERM $(pgrep hived)
```

This will:
1. Stop accepting new connections
2. Stop all running agents (in parallel)
3. Wait up to 30 seconds for clean shutdown
4. Close all NATS connections
5. Exit

### Crash Recovery

If hived is killed with `SIGKILL` (unclean shutdown):

```bash
kill -9 $(pgrep hived)
```

On next startup, crash recovery runs automatically:
1. Reads `state.db` to find agents marked as RUNNING/STARTING
2. For each, checks if the VM PID is still alive (`kill -0 PID`)
3. If the process is dead, marks the agent as FAILED with error "process not found after crash recovery"
4. Agents can then be restarted normally

### Rate Limiting

The rate limiter uses a per-subject token bucket algorithm:
- Default: 100 messages/second per subject
- Burst: 100 messages
- When exceeded, messages are dropped and a warning is logged

### Resource Monitoring

The resource monitor checks node usage every 30 seconds:
- Logs a warning when memory usage exceeds 80%
- Logs a warning when CPU usage exceeds 80%
- Records metrics for Prometheus export

---

## 19. Full End-to-End Walkthrough

This walks through every feature in order, from zero to a running cluster.

```bash
# ── Step 1: Build ──
make build

# ── Step 2: Scaffold ──
./hivectl init demo-cluster

# ── Step 3: Add a second agent ──
mkdir -p demo-cluster/agents/summarizer
cat > demo-cluster/agents/summarizer/manifest.yaml << 'EOF'
apiVersion: hive/v1
kind: Agent
metadata:
  id: summarizer
  team: default
spec:
  runtime:
    type: openclaw
    model:
      provider: anthropic
      name: claude-sonnet-4-5
  capabilities:
    - name: summarize
      description: Summarize text
      inputs:
        - name: text
          type: string
      outputs:
        - name: summary
          type: string
  resources:
    memory: "512Mi"
    vcpus: 2
EOF

# ── Step 4: Validate ──
./hivectl validate --cluster-root demo-cluster
# Validation passed.

# ── Step 5: Start hived ──
./hived --cluster-root demo-cluster &
HIVED_PID=$!
sleep 2

# ── Step 6: Start agents (mock mode for macOS) ──
export HIVE_TEST_FIRECRACKER=mock
./hivectl agents start example-agent --cluster-root demo-cluster
./hivectl agents start summarizer --cluster-root demo-cluster

# ── Step 7: List agents ──
./hivectl agents list --cluster-root demo-cluster
# AGENT_ID        TEAM     STATE    UPTIME
# example-agent   default  RUNNING  3s
# summarizer      default  RUNNING  1s

# ── Step 8: Check detailed status ──
./hivectl agents status example-agent --cluster-root demo-cluster

# ── Step 9: Create a join token ──
TOKEN=$(./hivectl tokens create --ttl 24h --cluster-root demo-cluster)
echo "Token: $TOKEN"

# ── Step 10: List tokens ──
./hivectl tokens list --cluster-root demo-cluster

# ── Step 11: Create RBAC users ──
./hivectl users create admin-user --role admin --cluster-root demo-cluster
./hivectl users create team-operator --role operator --teams default --cluster-root demo-cluster
./hivectl users create viewer-user --role viewer --agents example-agent --cluster-root demo-cluster

# ── Step 12: List users ──
./hivectl users list --cluster-root demo-cluster

# ── Step 13: Test NATS pub/sub ──
# (Install nats CLI: go install github.com/nats-io/natscli/nats@latest)
# In another terminal:
nats sub 'hive.health.>' &
SUB_PID=$!
sleep 1

# Simulate a heartbeat
nats pub hive.health.test-device '{
  "id":"hb-1","from":"test-device","to":"hived",
  "type":"health","timestamp":"2026-03-03T10:00:00Z",
  "payload":{"healthy":true,"uptime_seconds":60,"tier":"firmware"}
}'
sleep 1
kill $SUB_PID 2>/dev/null

# ── Step 14: Test agent restart ──
./hivectl agents restart example-agent --cluster-root demo-cluster

# ── Step 15: Stop an agent ──
./hivectl agents stop summarizer --cluster-root demo-cluster

# ── Step 16: List again (one running, one stopped) ──
./hivectl agents list --cluster-root demo-cluster

# ── Step 17: Destroy the stopped agent ──
./hivectl agents destroy summarizer --cluster-root demo-cluster

# ── Step 18: Node management (simulate) ──
# Nodes appear via hive-agent join; for testing, inspect state.db directly
cat demo-cluster/state.db | python3 -m json.tool

# ── Step 19: Revoke the token ──
PREFIX=$(./hivectl tokens list --cluster-root demo-cluster 2>/dev/null | tail -1 | awk '{print $1}')
./hivectl tokens revoke "$PREFIX" --cluster-root demo-cluster 2>/dev/null || true

# ── Step 20: Update and revoke users ──
./hivectl users update team-operator --role admin --cluster-root demo-cluster
./hivectl users revoke viewer-user --cluster-root demo-cluster

# ── Step 21: Final status check ──
./hivectl agents list --cluster-root demo-cluster
./hivectl tokens list --cluster-root demo-cluster
./hivectl users list --cluster-root demo-cluster

# ── Step 22: Graceful shutdown ──
kill -TERM $HIVED_PID
wait $HIVED_PID 2>/dev/null

# ── Step 23: Verify state persisted ──
cat demo-cluster/state.db | python3 -m json.tool

# ── Step 24: Run the full test suite ──
make test

echo "All features tested successfully."
```

---

## 20. Troubleshooting

### Port conflicts

If NATS fails to start with "address already in use", change the port in `cluster.yaml`:

```yaml
spec:
  nats:
    port: 4223  # try a different port
```

### Firecracker not found

If `agents start` fails with "firecracker: command not found":

```bash
# Use mock mode
export HIVE_TEST_FIRECRACKER=mock

# Or install Firecracker (Linux only):
# https://github.com/firecracker-microvm/firecracker/releases
```

### /dev/kvm not available

On macOS or Linux without KVM:

```bash
export HIVE_TEST_FIRECRACKER=mock
```

On Linux, enable KVM:

```bash
sudo modprobe kvm
sudo modprobe kvm_intel  # or kvm_amd
sudo chmod 666 /dev/kvm
```

### State file corruption

If `state.db` becomes corrupted:

```bash
# Delete it and start fresh (loses runtime state, not config)
rm demo-cluster/state.db
```

### Tests fail with "timeout"

Integration tests use embedded NATS with random ports. If tests time out:

```bash
# Run with verbose output
go test -tags integration -race -count=1 -v -timeout 10m ./internal/...
```

### Agent stuck in CREATING or STARTING

```bash
# Force destroy
./hivectl agents destroy stuck-agent --cluster-root demo-cluster

# Then restart
./hivectl agents start stuck-agent --cluster-root demo-cluster
```

### NATS connection refused

Verify hived is running and the port matches:

```bash
# Check if NATS is listening
lsof -i :4222

# Check hived logs (they go to stdout)
```

### Firmware build fails

Ensure the platform toolchain is installed:

```bash
# ESP-IDF
idf.py --version

# Arduino
arduino-cli version

# Pico SDK
cmake --version
```

---

## Environment Variables Reference

| Variable | Default | Description |
|----------|---------|-------------|
| `HIVE_TEST_FIRECRACKER` | _(unset)_ | Set to `mock` to use mock VM manager |
| `NATS_URL` | _(unset)_ | Override NATS URL for `nats` CLI |

## File Reference

| File | Owned By | Purpose |
|------|----------|---------|
| `cluster.yaml` | Operator | Cluster configuration (read by hived and hivectl) |
| `agents/*/manifest.yaml` | Operator | Agent definitions |
| `teams/*.yaml` | Operator | Team definitions |
| `state.db` | hived | Runtime state (do not edit manually while hived runs) |
| `.state/jetstream/` | NATS | JetStream persistence |
| `.state/agents/*/` | hived | Per-agent VM artifacts (sockets, rootfs copies) |
