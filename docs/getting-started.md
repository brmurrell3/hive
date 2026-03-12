# Getting Started with Hive

A hands-on guide from zero to running AI agent teams. Four parts, each building on the last.

---

## Part 1: Local Development (Any OS)

Works on macOS and Linux. Uses the process backend -- no KVM or Firecracker needed.

### Prerequisites

- [Go 1.25+](https://go.dev/dl/) installed
- A terminal with `git` and `make`

### Step 1: Build

```bash
git clone https://github.com/brmurrell3/hive && cd hive
make build
```

This produces three binaries in `./bin/`: `hived` (control plane), `hivectl` (CLI), and `hive-agent` (Tier 2 agent host).

### Step 2: Scaffold a cluster

```bash
./bin/hivectl init my-cluster
```

This creates:

```
my-cluster/
+-- cluster.yaml
+-- agents/
|   +-- example-agent/
|       +-- manifest.yaml
+-- teams/
    +-- default.yaml
```

### Step 3: Validate

```bash
./bin/hivectl validate --cluster-root my-cluster
# Validation passed.
```

### Step 4: Start the control plane

```bash
./bin/hived --cluster-root my-cluster
```

hived runs in the foreground with an embedded NATS server. Leave it running and open a second terminal.

### Step 5: Manage agents

```bash
# List agents
./bin/hivectl agents list --cluster-root my-cluster

# Start an agent
./bin/hivectl agents start example-agent --cluster-root my-cluster

# Check status
./bin/hivectl agents status example-agent --cluster-root my-cluster

# Stop
./bin/hivectl agents stop example-agent --cluster-root my-cluster

# Destroy (removes from state)
./bin/hivectl agents destroy example-agent --cluster-root my-cluster
```

### Using a template

Templates scaffold complete multi-agent teams with working pipelines:

```bash
# List available templates
./bin/hivectl init --list-templates

# Scaffold a CI pipeline team
./bin/hivectl init --template ci-pipeline my-pipeline

# Start the dev environment (builds + starts hived + starts all agents)
./bin/hivectl dev --cluster-root my-pipeline

# In a second terminal, trigger the pipeline
./bin/hivectl trigger --cluster-root my-pipeline --team ci-pipeline \
  --payload '{"file_path": "main.go", "test_command": "go test ./..."}'
```

Clean up:

```bash
rm -rf my-pipeline
```

---

## Part 2: Writing Agents

Agents are programs that expose capabilities over HTTP. The Hive sidecar calls your agent's HTTP handlers when a capability is invoked. You can write agents in any language -- Hive provides SDKs for Python, Go, and TypeScript.

### Agent structure

Each agent lives in `agents/<agent-id>/` within the cluster root:

```
agents/my-agent/
+-- manifest.yaml    # Required: identity, capabilities, resources
+-- entrypoint.sh    # Optional: startup script for custom runtimes
+-- AGENTS.md        # Optional: instructions for OpenClaw runtimes
```

### Python SDK

Zero external dependencies. Single file: `sdk/python/hive_sdk.py`.

```python
from hive_sdk import HiveAgent

agent = HiveAgent()

@agent.capability("summarize")
def summarize(text: str):
    # Your logic here (call an LLM, run a model, etc.)
    return {"summary": f"Summary of: {text[:100]}..."}

@agent.capability("translate")
def translate(text: str, target_language: str = "es"):
    return {"translated": f"[{target_language}] {text}"}

agent.run()
```

The SDK reads configuration from environment variables set by the Hive runtime:

| Variable | Description |
|----------|-------------|
| `HIVE_AGENT_ID` | Agent identifier |
| `HIVE_TEAM_ID` | Team identifier |
| `HIVE_SIDECAR_URL` | Sidecar API URL (default: `http://127.0.0.1:9100`) |
| `HIVE_CALLBACK_PORT` | Port for the agent's HTTP callback server |
| `HIVE_WORKSPACE` | Workspace directory |

### Go SDK

Located at `sdk/go/hive/`.

```go
package main

import (
    "context"
    "github.com/brmurrell3/hive/sdk/go/hive"
)

func main() {
    agent := hive.NewAgent()

    agent.HandleCapability("summarize", func(inputs map[string]any) (map[string]any, error) {
        text := inputs["text"].(string)
        return map[string]any{"summary": "Summary of: " + text[:100]}, nil
    })

    agent.Run(context.Background())
}
```

### TypeScript SDK

Located at `sdk/typescript/`. Zero external runtime dependencies.

```typescript
import { HiveAgent } from "./src/index";

const agent = new HiveAgent();

agent.capability("summarize", async (inputs) => {
    const text = inputs.text as string;
    return { summary: `Summary of: ${text.substring(0, 100)}` };
});

agent.run();
```

### Shell script agents

For simple agents, use a shell script with the `custom` runtime type:

```bash
#!/bin/bash
# agents/my-agent/entrypoint.sh
# The sidecar handles all NATS communication.
# This script is called by the sidecar when capabilities are invoked.

echo "Agent started, workspace: ${HIVE_WORKSPACE}"
# Agent logic here -- the sidecar manages lifecycle
exec sleep infinity
```

### Agent manifest

Every agent needs a `manifest.yaml`. Here is a complete example:

```yaml
apiVersion: hive/v1
kind: Agent
metadata:
  id: my-agent
  team: default
  labels:
    role: worker
spec:
  runtime:
    type: process    # process, openclaw, or custom
  capabilities:
    - name: summarize
      description: Summarize text input
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
    vcpus: 1
  health:
    interval: "30s"
    timeout: "5s"
    maxFailures: 3
  restart:
    policy: on-failure
    maxRestarts: 5
```

See [Schemas](schemas.md) for the full manifest specification.

---

## Part 3: Real Hardware (Linux + KVM + Firecracker)

For production deployments with per-agent VM isolation.

### Prerequisites

- Linux x86_64 or arm64 host
- `/dev/kvm` accessible (kernel modules `kvm` and `kvm_intel` or `kvm_amd` loaded)
- At least 4 GB RAM (for Tier 1 classification)
- User in the `kvm` group, or run as root

```bash
# Verify KVM
ls -la /dev/kvm

# If not present, load kernel modules
sudo modprobe kvm
sudo modprobe kvm_intel  # or kvm_amd

# Add your user to the kvm group
sudo usermod -aG kvm $USER
# Log out and back in for group change to take effect
```

### Install Firecracker

Download from the [Firecracker releases](https://github.com/firecracker-microvm/firecracker/releases):

```bash
FC_VERSION=v1.6.0
ARCH=$(uname -m)
curl -fSL -o firecracker \
  "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}"
chmod +x firecracker
sudo mv firecracker /usr/local/bin/
```

### Build the VM images

```bash
# Build all Linux binaries (includes hive-sidecar for VMs)
make build-linux-amd64   # or build-linux-arm64

# Download a Firecracker-compatible kernel
make download-kernel

# Build the rootfs image (requires Docker for Alpine, or Nix for NixOS)
make rootfs
```

Output files:
- `rootfs/vmlinux` -- Linux kernel for Firecracker direct boot
- `rootfs/rootfs.ext4` -- ext4 filesystem with sidecar and init

### Boot your first VM

```bash
# Create a cluster with VM configuration
./bin/hivectl init my-cluster

# Edit cluster.yaml to point to the kernel and rootfs
```

Add to `my-cluster/cluster.yaml`:

```yaml
spec:
  vm:
    kernelPath: /absolute/path/to/rootfs/vmlinux
    rootfsPath: /absolute/path/to/rootfs/rootfs.ext4
```

Start the control plane (without `HIVE_TEST_FIRECRACKER`):

```bash
sudo ./bin/hived --cluster-root my-cluster
```

Start an agent -- this boots a real Firecracker microVM:

```bash
./bin/hivectl agents start example-agent --cluster-root my-cluster
```

### Verify isolation

```bash
# Check agent status (should show RUNNING with a real VM PID)
./bin/hivectl agents status example-agent --cluster-root my-cluster

# View serial console output
cat my-cluster/.state/agents/example-agent/console.log

# Monitor heartbeats from inside the VM
nats sub 'hive.health.>'
```

Each VM gets its own kernel, memory ceiling, dedicated network namespace, and vsock channel to the host NATS bus. See [Architecture](architecture.md) for details on the VM lifecycle and network topology.

---

## Part 4: Production Deployment

### systemd service

Create `/etc/systemd/system/hived.service`:

```ini
[Unit]
Description=Hive Control Plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=hive
Group=hive
ExecStart=/usr/local/bin/hived --cluster-root /var/lib/hive/cluster --log-level info
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

# Security hardening
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/hive
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now hived
sudo journalctl -u hived -f
```

### Monitoring with Prometheus

hived exposes a `/metrics` endpoint in Prometheus text format on the dashboard port (default `:8080`).

Add to `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'hive'
    static_configs:
      - targets: ['localhost:8080']
    scrape_interval: 15s
```

Key metrics:
- `hive_agents_total{status}` -- agent count by state
- `hive_heartbeat_healthy{agent_id}` -- per-agent health (1=healthy, 0=unhealthy)
- `hive_capability_invocation_duration_ms` -- capability latency distribution
- `hive_node_memory_usage_percent` / `hive_node_cpu_usage_percent` -- node resource usage
- `hive_nats_messages_total{subject}` -- NATS message throughput

### Log aggregation

Agent logs are streamed via NATS and persisted to local JSONL files:

```
<cluster-root>/logs/
+-- <agent-id>/
    +-- 2026-03-14.jsonl        # One file per day
    +-- 2026-03-14.1.jsonl      # Rotated when >100MB
```

Query logs via CLI:

```bash
# Recent logs
./bin/hivectl agents logs my-agent --cluster-root my-cluster

# Tail with follow
./bin/hivectl agents logs my-agent --follow --cluster-root my-cluster

# Last 100 lines
./bin/hivectl agents logs my-agent --tail 100 --cluster-root my-cluster
```

Or via the dashboard REST API:

```bash
curl http://localhost:8080/api/logs/my-agent?limit=50
```

### TLS configuration

For production, enable TLS on the NATS server and cluster communications. Configure TLS cert/key paths in `cluster.yaml` under `spec.nats`. The sidecar HTTP server also supports TLS via `TLSCertFile` and `TLSKeyFile` configuration.

---

## Next steps

- [Operations Guide](operations.md) -- full configuration reference, networking, backup, troubleshooting
- [Architecture](architecture.md) -- VM lifecycle, capability routing, state machine
- [API Reference](api-reference.md) -- sidecar HTTP API, NATS subjects, SDK details
- [Schemas](schemas.md) -- complete YAML manifest specification
- [CLI Reference](cli-reference.md) -- all hivectl commands
