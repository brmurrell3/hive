[← Back to Documentation](README.md)

# Testing Hive Before Hardware

For build instructions, see the [main README](../README.md).

Two tiers of testing, from easiest to most realistic.

## Prerequisites

```bash
# Build all binaries
make build

# Run existing test suite (should be green before proceeding)
make test
```

---

## Tier 1: Mock Mode (macOS / any OS, no KVM)

Tests the full control plane — NATS, capability routing, health, reconciliation,
scheduling, RBAC, dashboard, CLI — without starting real VMs.

### 1. Create a test cluster

```bash
mkdir -p /tmp/hive-test
cd /tmp/hive-test
```

Create `cluster.yaml`:

```yaml
apiVersion: hive/v1
kind: Cluster
metadata:
  name: smoke-test
spec:
  nats:
    port: 4222
    jetstream:
      enabled: true
      maxMemory: "256MB"
      maxStorage: "1GB"
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 1
    health:
      interval: "5s"
      timeout: "3s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 3
      backoff: "2s"
```

Create `teams/demo.yaml`:

```yaml
apiVersion: hive/v1
kind: Team
metadata:
  id: demo
spec:
  communication:
    namespace: demo
```

Create `agents/echo/manifest.yaml`:

```yaml
apiVersion: hive/v1
kind: Agent
metadata:
  id: echo
  team: demo
spec:
  tier: vm
  resources:
    memory: "512Mi"
    vcpus: 1
  capabilities:
    - name: echo
      description: "Echoes input back"
      inputs:
        - name: message
          type: string
          required: true
      outputs:
        - name: message
          type: string
```

### 2. Start hived in mock mode

```bash
HIVE_TEST_FIRECRACKER=mock /path/to/hived --cluster-root /tmp/hive-test
```

hived will:
- Start embedded NATS on port 4222
- Load the cluster config and agent manifests
- Use MockHypervisor (no real VMs, no KVM needed)
- Write state to `/tmp/hive-test/.state/`
- Write NATS auth token to `/tmp/hive-test/.state/nats-auth-token`

Leave this running in a terminal.

### 3. Exercise with hivectl

In a second terminal:

```bash
export HIVE_CONFIG=/tmp/hive-test

# Validate the cluster config
hivectl validate

# Check cluster status
hivectl status

# List agents (should show echo agent)
hivectl agents list

# Check agent status
hivectl agents status echo

# Start the agent (mock VM will "boot")
hivectl agents start echo

# Verify it's running
hivectl agents status echo
# Expected: status=RUNNING

# Test health (wait ~10s for heartbeats in mock mode)
hivectl agents status echo
# Expected: healthy=true (mock sidecar reports healthy)

# Invoke a capability
hivectl capabilities invoke echo echo --inputs '{"message":"hello"}'

# Send a NATS message
hivectl messages send echo --payload '{"test": true}'

# Subscribe to messages (Ctrl+C to stop)
hivectl messages subscribe "hive.agent.>"

# Restart the agent
hivectl agents restart echo

# Check status after restart
hivectl agents status echo

# Destroy the agent
hivectl agents destroy echo

# Verify it's gone
hivectl agents list
```

### 4. Test the dashboard

If dashboard is enabled in cluster.yaml (`spec.dashboard.enabled: true`):

```bash
# Dashboard API
curl http://localhost:8080/api/agents
curl http://localhost:8080/api/status

# WebSocket (use wscat or similar)
wscat -c ws://localhost:8080/ws
```

### 5. Test RBAC

Add users to `cluster.yaml`:

```yaml
spec:
  users:
    - id: admin-user
      name: Admin
      role: admin
      token: "test-admin-token"
      teams: ["all"]
    - id: viewer-user
      name: Viewer
      role: viewer
      token: "test-viewer-token"
      teams: ["demo"]
```

Restart hived, then test authorization:

```bash
# Admin can do everything
hivectl agents list --user admin-user --token test-admin-token

# Viewer can read but not mutate
hivectl agents list --user viewer-user --token test-viewer-token    # works
hivectl agents destroy echo --user viewer-user --token test-viewer-token  # should fail
```

### What Tier 1 validates

- Cluster config parsing and validation
- NATS embedded server startup and messaging
- Agent lifecycle state machine (PENDING → CREATING → RUNNING → STOPPED → DESTROYED)
- Reconciler drift detection and action generation
- Scheduler bin-packing and placement
- Health monitor heartbeat tracking
- Restart manager policy enforcement
- Capability routing (NATS request/reply)
- Cross-team capability routing
- RBAC authorization
- Dashboard REST + WebSocket API
- Prometheus metrics endpoint
- Log aggregation
- CLI command coverage

### What Tier 1 does NOT validate

- Firecracker VM boot
- Virtio-vsock host↔guest communication
- Rootfs image correctness
- Sidecar boot inside a real VM
- Kernel boot parameters
- Agent drive mounting
- Network connectivity inside VMs

---

## Tier 2: Real Firecracker (Linux with KVM)

Tests the full stack including VM boot, vsock, sidecar, and agent runtime.

### Requirements

- Linux x86_64 host with `/dev/kvm` access
- Firecracker binary (v1.6.0+) in `$PATH`
- Root or `kvm` group membership
- Docker (for Alpine rootfs build) or Nix (for NixOS rootfs)

Options for getting a Linux environment from macOS:
- **Cloud VM**: Any x86 instance with nested virt (e.g., GCP `n2-standard-4` with `--enable-nested-virtualization`, AWS `.metal` instances)
- **Local**: UTM/QEMU with an Ubuntu VM and KVM passthrough
- **Codespaces**: GitHub Codespaces with a Linux devcontainer (check KVM support)

### 1. Build the rootfs and kernel

**Option A: Alpine rootfs (simpler)**

```bash
# Cross-compile sidecar for Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar

# Build rootfs image
cd rootfs && make rootfs

# Download Firecracker-compatible kernel
make download-kernel

# Outputs:
#   rootfs/rootfs.ext4    — ext4 filesystem image
#   rootfs/vmlinux        — Linux kernel for Firecracker
```

**Option B: NixOS rootfs (reproducible)**

```bash
# Cross-compile sidecar first
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux-amd64/hive-sidecar ./cmd/hive-sidecar

# Build with Nix
cd rootfs/nixos
nix build .#rootfs    # produces result/rootfs.ext4
nix build .#kernel    # produces result/vmlinux
```

### 2. Configure the cluster for real VMs

Create `cluster.yaml` on your Linux host:

```yaml
apiVersion: hive/v1
kind: Cluster
metadata:
  name: hardware-test
spec:
  nats:
    port: 4222
    jetstream:
      enabled: true
      maxMemory: "256MB"
      maxStorage: "1GB"
  vm:
    kernelPath: /path/to/vmlinux
    rootfsPath: /path/to/rootfs.ext4
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 1
    health:
      interval: "5s"
      timeout: "3s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 3
      backoff: "5s"
```

Create the same team/agent manifests as Tier 1.

### 3. Start hived (real mode)

```bash
# No HIVE_TEST_FIRECRACKER env var — uses real Firecracker
sudo ./hived --cluster-root /path/to/cluster
```

`sudo` is needed for `/dev/kvm` access unless your user is in the `kvm` group.

### 4. Deploy and test

```bash
# Start an agent — this boots a real Firecracker VM
hivectl agents start echo

# Watch the logs for:
#   - "creating VM" — Firecracker process spawned
#   - "vsock forwarder started" — host-side vsock listener bound
#   - "agent started" — VM booted, sidecar connected to NATS

# Check agent status
hivectl agents status echo
# Expected: status=RUNNING, healthy=true (after sidecar heartbeat arrives)

# Invoke a capability through the VM
hivectl capabilities invoke echo echo --inputs '{"message":"hello from host"}'

# Check sidecar health directly (if network is routed)
# The sidecar listens on port 9100 inside the VM

# Test auto-restart: kill the VM process
# hived should detect unhealthy → restart automatically

# Destroy
hivectl agents destroy echo
```

### 5. Debugging VM issues

```bash
# Check Firecracker socket
ls -la /path/to/cluster/.state/agents/echo/

# Check VM logs (serial console output)
cat /path/to/cluster/.state/agents/echo/console.log

# Check sidecar config generated for the VM
cat /path/to/cluster/.state/agents/echo/agent-drive/sidecar.conf

# Check vsock UDS files
ls /path/to/cluster/.state/agents/echo/*.vsock*

# Monitor NATS subjects for sidecar heartbeats
hivectl messages subscribe "hive.health.>"

# Monitor agent logs
hivectl messages subscribe "hive.logs.>"
```

### What Tier 2 additionally validates

- Firecracker VM creation via API socket
- Kernel boot with correct parameters
- Rootfs mount and init script execution
- Agent drive (vdb) mounting with sidecar.conf
- Vsock forwarder (host) ↔ vsock proxy (guest) communication
- Sidecar NATS connection through vsock tunnel
- Sidecar heartbeat publishing from inside VM
- Capability request/response through vsock
- MEMORY.md hot-reload pushed to VM
- Auto-restart on VM crash
- Resource cleanup on destroy (rootfs copy, agent drive, vsock UDS)

---

## Test Matrix

| Feature | Tier 1 (Mock) | Tier 2 (Firecracker) |
|---------|:---:|:---:|
| Config parsing + validation | Y | Y |
| NATS embedded server | Y | Y |
| Agent state machine | Y | Y |
| Reconciler | Y | Y |
| Scheduler | Y | Y |
| Health monitoring | Y | Y |
| Auto-restart | Y | Y |
| Capability routing | Y | Y |
| Cross-team capabilities | Y | Y |
| RBAC | Y | Y |
| Dashboard + WebSocket | Y | Y |
| Metrics endpoint | Y | Y |
| Log aggregation | Y | Y |
| CLI commands | Y | Y |
| Firecracker VM boot | - | Y |
| Vsock communication | - | Y |
| Sidecar inside VM | - | Y |
| Rootfs + kernel | - | Y |
| Agent drive mounting | - | Y |
| VM crash recovery | - | Y |

---

## Troubleshooting

**hived won't start**: Check that the cluster-root has a valid `cluster.yaml` and the teams/agents directory structure is correct. Run `hivectl validate` first.

**NATS port conflict**: Change `spec.nats.port` in cluster.yaml or kill any existing NATS process on 4222.

**Mock mode not activating**: Ensure `HIVE_TEST_FIRECRACKER=mock` is exported, not just set inline.

**Firecracker "permission denied"**: Need `/dev/kvm` access. Run as root or add user to `kvm` group: `sudo usermod -aG kvm $USER`.

**VM boots but sidecar doesn't connect**: Check vsock UDS files exist, check serial console log for sidecar startup errors, verify NATS auth token matches.

**Capability invoke times out**: Sidecar may not have finished connecting. Wait for a heartbeat to appear (`hivectl messages subscribe "hive.health.>"`), then retry.

**Rootfs too small**: Increase the size in `rootfs/Makefile` (`SIZE` variable) or NixOS config.
