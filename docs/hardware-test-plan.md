# Hardware Test Plan

Tests that require a real Linux host with KVM and cannot be validated in CI with mocks.

These tests run automatically on the self-hosted NixOS runner via the
`Self-Hosted (KVM)` workflow (`.github/workflows/self-hosted.yml`), specifically
the `vm-tests` job. They can also be run manually on any KVM-capable Linux host.

## Environment Requirements

All of these are provisioned automatically by `nix/ci-runner.nix` on the NixOS runner.
For manual testing on a non-NixOS host:

- Linux host (bare metal or nested-virt VM)
- `/dev/kvm` access
- `firecracker` binary installed and in PATH
- `vhost_vsock` kernel module loaded
- `nft` and `ip` binaries installed
- `CAP_NET_ADMIN` + `CAP_SYS_ADMIN` (or root)
- Valid kernel + rootfs image pair

## 1. Firecracker VM Lifecycle

**Source:** `internal/vm/firecracker.go`

- [ ] Firecracker binary spawns successfully via unix socket API
- [ ] `/dev/kvm` opened and VM boots with provided kernel + rootfs
- [ ] VM responds to API calls over unix socket (machine config, drives, boot source, network, vsock)
- [ ] SIGTERM gracefully shuts down the Firecracker process
- [ ] SIGKILL fallback fires when SIGTERM times out
- [ ] PID verification via `/proc/<pid>/cmdline` correctly identifies Firecracker processes
- [ ] PID recycling detection works (stale PID pointing to a different process)
- [ ] Zombie reaping via `syscall.Wait4` in sidecar reaper (`cmd/hive-sidecar/reaper_linux.go`)
- [ ] Multiple VMs can run concurrently without resource conflicts
- [ ] VM destruction cleans up all resources (socket, state dir, drive images)

## 2. Vsock Communication

**Source:** `internal/vm/vsock_linux.go`

- [ ] `vhost_vsock` module is detected via `/sys/module/vhost_vsock` or `/proc/modules`
- [ ] `AF_VSOCK` socket connects between host and guest
- [ ] VsockForwarder proxies UDS ↔ TCP bidirectionally
- [ ] CID allocation assigns unique IDs (≥ 3) per VM
- [ ] CID is reclaimed after VM destruction and reusable
- [ ] Connection tracking correctly counts active connections
- [ ] Forced closure terminates all proxied connections on shutdown
- [ ] Exponential backoff on accept errors prevents tight loop
- [ ] Sidecar HTTP API (:9100) is reachable from host via vsock forwarding

## 3. TAP Devices and NAT

**Source:** `internal/vm/tap.go`

- [ ] `ip tuntap add dev <name> mode tap` creates TAP device
- [ ] `ip addr add` assigns gateway IP to TAP device
- [ ] `ip link set <tap> up` activates the device
- [ ] iptables masquerade rule enables guest outbound NAT
- [ ] `/proc/sys/net/ipv4/ip_forward` is set to 1
- [ ] Guest VM can reach the host via TAP gateway IP
- [ ] Guest VM can reach external networks (when egress policy allows)
- [ ] TAP device is deleted on agent destruction
- [ ] iptables rules are cleaned up on agent destruction
- [ ] Multiple TAP devices coexist without IP conflicts

## 4. Nftables Network Isolation

**Source:** `internal/vm/network.go`

- [ ] `nft -f` applies generated ruleset without errors
- [ ] Egress allowlist permits only specified destinations
- [ ] Egress to non-allowed destinations is blocked (verified with curl/ping from guest)
- [ ] DNS traffic scoped to gateway IP only
- [ ] Ingress rules restrict traffic to expected protocols/ports
- [ ] IPv6 traffic is dropped when only IPv4 rules are configured
- [ ] Rules are agent-scoped (one VM's policy doesn't affect another)
- [ ] nftables table and chains are cleaned up on agent destruction
- [ ] Policy changes take effect without VM restart (rule replacement)

## 5. Preflight Checks

**Source:** `internal/vm/preflight.go`

- [ ] `/dev/kvm` openability check passes on KVM-capable host
- [ ] `/dev/kvm` check fails gracefully on non-KVM host (warning, not crash)
- [ ] `CAP_NET_ADMIN` detected from `/proc/self/status` capability bitmask
- [ ] `CAP_SYS_ADMIN` detected from `/proc/self/status` capability bitmask
- [ ] Missing capabilities produce clear warning messages
- [ ] `nft list tables` succeeds with sufficient permissions
- [ ] `nft list tables` failure reports missing nftables
- [ ] `vhost_vsock` module state detected from `/sys/module/` or `/proc/modules`
- [ ] `ip` binary found in PATH
- [ ] All preflight failures are warnings (non-VM workloads still run)

## 6. Kernel and Rootfs Images

**Source:** `internal/vm/kernel.go`, `internal/vm/images.go`

- [ ] Kernel image downloads from GitHub releases and caches locally
- [ ] Rootfs image downloads and decompresses (gzip)
- [ ] ext4 superblock validation passes on valid rootfs
- [ ] ext4 superblock validation rejects corrupted images
- [ ] Per-agent rootfs copy creates independent writable image
- [ ] Kernel architecture mapping selects correct binary for host CPU (amd64/arm64)
- [ ] Cached images are reused on subsequent agent starts
- [ ] Disk size configuration is respected in drive image creation

## 7. System Resource Monitoring Under Load

**Source:** `internal/production/sysresources_linux.go`

- [ ] `/proc/meminfo` parsing returns accurate total/available memory
- [ ] `/proc/stat` parsing returns accurate CPU utilization
- [ ] Memory pressure threshold triggers warning when VMs consume significant memory
- [ ] CPU utilization threshold triggers warning under VM workload
- [ ] Resource monitoring doesn't degrade performance (sampling interval appropriate)
- [ ] Threshold clamping prevents false alerts at boundary values

## 8. End-to-End VM Agent Lifecycle

Combined test exercising the full stack:

- [ ] `hived` starts with Firecracker backend on KVM-capable host
- [ ] Agent start: creates rootfs copy → creates TAP → spawns Firecracker → boots VM → vsock connects → sidecar registers
- [ ] Agent health heartbeats flow from sidecar through vsock to control plane
- [ ] Capability invocation reaches agent inside VM and returns response
- [ ] Network policy restricts VM egress to allowed destinations only
- [ ] Agent stop: SIGTERM → graceful shutdown → resource cleanup
- [ ] Agent destroy: VM killed → TAP deleted → nftables cleaned → state dir removed → CID reclaimed
- [ ] Crash recovery: kill Firecracker process externally → health monitor detects → auto-restart with backoff
- [ ] Scale up: multiple VMs started concurrently without CID/TAP/IP conflicts
- [ ] Scale down: mass destroy respects safeguard limits

## Running the Tests

```bash
# VM-specific tests (requires KVM)
go test -tags vm -race -count=1 ./internal/vm/...

# E2E tests with real Firecracker (unset the mock env var)
unset HIVE_TEST_FIRECRACKER
go test -tags e2e -race -count=1 -timeout 10m ./test/e2e/...

# Full suite including hardware tests
go test -tags unit,integration,vm,e2e -race -count=1 -timeout 15m ./...
```

---

# Deployment and Testing Instructions

Step-by-step instructions for deploying Hive on real hardware and executing the hardware test plan above.

## Part 1: Provision a Linux Host

### Option A: Use the existing NixOS CI runner (recommended)

The NixOS box already running GitHub Actions has everything provisioned via
`nix/ci-runner.nix`: KVM modules, Firecracker, nftables, iproute2, e2fsprogs,
Go, and the correct capabilities (`CAP_NET_ADMIN`, `CAP_SYS_ADMIN`).

**To run the hardware tests via CI:** push to `main` or trigger the
`Self-Hosted (KVM)` workflow manually. The `vm-tests` job builds rootfs images,
runs VM tests with real KVM, runs E2E with real Firecracker, and runs
`VALIDATE.sh real`.

**To run manually on the NixOS box:** SSH in and:

```bash
cd /path/to/hive   # or wherever the runner checks out the repo
nix develop --command bash

# Verify hardware:
ls -la /dev/kvm /dev/vhost-vsock
firecracker --version
nft list tables

# Build images:
make build
make download-kernel
cd rootfs && ./build-rootfs.sh test-rootfs.ext4 512M ../bin/hive-sidecar && cd ..

# Run hardware tests:
go test -tags vm -race -count=1 -timeout 10m -v ./internal/vm/...
go test -tags e2e -race -count=1 -timeout 10m -v ./test/e2e/
./VALIDATE.sh real
```

If Docker is also enabled on the NixOS box (`virtualisation.docker.enable = true`),
the rootfs-build CI job will also run. If not, the NixOS rootfs flake at
`rootfs/nixos/` can build images without Docker:

```bash
nix build ./rootfs/nixos#rootfs
nix build ./rootfs/nixos#kernel
```

### Option B: Separate Linux host

| Option | Spec | Notes |
|--------|------|-------|
| Bare metal | x86_64 or arm64, 4GB+ RAM, 20GB+ disk | Best for real perf numbers |
| Cloud VM with nested virt | AWS `.metal` or `i3.metal`, GCP with `--enable-nested-virtualization`, Hetzner dedicated | Must expose `/dev/kvm` |
| Local VM | QEMU/libvirt with `cpu host` passthrough | Works for functional testing |

**OS:** Ubuntu 22.04 or 24.04 (amd64 or arm64). Debian 12 also works.

## Part 2: Host Preparation (non-NixOS only)

Skip this section if using the NixOS runner — everything below is already provisioned.

SSH into the host and run these as root (or with sudo):

### 2.1 Install system dependencies

```bash
apt-get update
apt-get install -y \
    curl tar git make \
    nftables iproute2 \
    e2fsprogs docker.io
```

### 2.2 Load kernel modules

```bash
modprobe kvm
modprobe kvm_intel   # Intel — or kvm_amd for AMD
modprobe vhost_vsock

# Persist across reboots
cat > /etc/modules-load.d/hive.conf <<'EOF'
kvm
kvm_intel
vhost_vsock
EOF
```

### 2.3 Install Firecracker

```bash
FC_VERSION="v1.6.0"
ARCH=$(uname -m)  # x86_64 or aarch64

curl -fSL -o /tmp/firecracker.tgz \
    "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${ARCH}.tgz"
tar -xzf /tmp/firecracker.tgz -C /tmp
cp /tmp/release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH} /usr/local/bin/firecracker
chmod +x /usr/local/bin/firecracker
```

### 2.4 Verify prerequisites

```bash
ls -la /dev/kvm              # Should exist and be readable
ls -la /dev/vhost-vsock      # Should exist after modprobe
which firecracker nft ip     # All three in PATH
firecracker --version        # Should print version
```

## Part 3: Install Hive

### Option A: From release (recommended for testing)

```bash
sudo ./scripts/install.sh --yes
```

This creates the `hive` user, installs binaries to `/usr/local/bin/`, downloads kernel + rootfs images to `/var/lib/hive/rootfs/`, and installs the systemd unit.

### Option B: Build from source

```bash
# Install Go 1.25+ if not present
# https://go.dev/dl/

git clone https://github.com/brmurrell3/hive.git
cd hive
make build

# Build VM images
make rootfs          # Requires Docker for Alpine base extraction
# Output: rootfs/vmlinux, rootfs/rootfs.ext4
```

## Part 4: Configure the Cluster

### 4.1 Scaffold a cluster

```bash
# If installed via install.sh:
hivectl init /var/lib/hive

# If built from source:
./bin/hivectl init ./test-cluster
```

### 4.2 Edit cluster.yaml

Point kernel/rootfs to real images and configure for Firecracker:

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
  vm:
    kernelPath: /var/lib/hive/rootfs/vmlinux      # Adjust path
    rootfsPath: /var/lib/hive/rootfs/rootfs.ext4   # Adjust path
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 1
      disk: "5GB"
    network:
      egress: restricted
    health:
      enabled: true
      interval: "10s"
      timeout: "5s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 3
      backoff: "5s"
  dashboard:
    enabled: true
    addr: ":8080"
  metrics:
    enabled: true
    addr: ":9090"
```

### 4.3 Create a test agent manifest

```bash
mkdir -p /var/lib/hive/agents/test-vm-agent
cat > /var/lib/hive/agents/test-vm-agent/manifest.yaml <<'EOF'
apiVersion: hive/v1
kind: Agent
metadata:
  id: test-vm-agent
  team: hardware-test
spec:
  tier: vm
  runtime:
    type: custom
    command: "/bin/sh -c 'echo agent running && sleep infinity'"
  capabilities:
    - name: echo
      description: Echo inputs back
  resources:
    memory: "512Mi"
    vcpus: 1
    disk: "2GB"
  network:
    egress: restricted
    egress_allowlist:
      - "1.1.1.1"
  health:
    interval: "10s"
    timeout: "5s"
    maxFailures: 3
  restart:
    policy: on-failure
    maxRestarts: 3
    backoff: "5s"
EOF

mkdir -p /var/lib/hive/teams
cat > /var/lib/hive/teams/hardware-test.yaml <<'EOF'
apiVersion: hive/v1
kind: Team
metadata:
  id: hardware-test
  name: Hardware Test Team
spec:
  agents:
    - test-vm-agent
EOF
```

### 4.4 Validate

```bash
hivectl validate --cluster-root /var/lib/hive
```

## Part 5: Start the Control Plane

### Option A: Systemd (install.sh path)

```bash
systemctl start hived
journalctl -u hived -f     # Watch logs in another terminal
```

### Option B: Foreground (build-from-source path)

```bash
# As root or with CAP_NET_ADMIN + CAP_SYS_ADMIN:
sudo ./bin/hived --cluster-root ./test-cluster --log-level debug
```

Verify startup:
```bash
hivectl status --cluster-root /var/lib/hive
# Should show cluster name, 0 agents, NATS port
```

## Part 6: Execute the Hardware Test Plan

### 6.1 Preflight checks (Section 5)

These run automatically on `hived` startup. Check the logs for:

```bash
# Look for preflight results in hived output:
journalctl -u hived | grep -i preflight
# Expected: all checks PASS on a properly configured host
```

Manually verify:
```bash
ls -la /dev/kvm /dev/vhost-vsock
cat /proc/modules | grep vhost_vsock
nft list tables     # Should succeed (may be empty)
```

### 6.2 VM lifecycle (Sections 1, 2, 3, 6)

```bash
# Start the test agent — this exercises:
#   rootfs copy, drive image creation, TAP device, CID allocation,
#   Firecracker spawn, vsock connect, sidecar boot, heartbeat
hivectl agents start test-vm-agent --cluster-root /var/lib/hive

# Watch it progress through states:
watch -n1 'hivectl agents status test-vm-agent --cluster-root /var/lib/hive'
# Expected: PENDING → CREATING → STARTING → RUNNING

# Verify the VM process exists:
pgrep -a firecracker
# Should show a firecracker process with --api-sock

# Verify TAP device was created:
ip link show | grep hive
# Should show a tap device like hive-test-vm-agent

# Verify vsock forwarding:
ss -xlp | grep vsock
# Should show listening unix sockets for vsock forwarding

# Verify heartbeats are arriving (in hived logs):
journalctl -u hived | grep "heartbeat.*test-vm-agent"
```

### 6.3 Network isolation (Section 4)

```bash
# Check nftables rules were applied:
nft list tables | grep hive
nft list table inet hive_test-vm-agent  # Exact name depends on implementation

# If you can exec into the VM (via sidecar exec):
hivectl agents exec test-vm-agent -- curl -s --connect-timeout 3 http://1.1.1.1
# Expected: succeeds (1.1.1.1 is in allowlist)

hivectl agents exec test-vm-agent -- curl -s --connect-timeout 3 http://8.8.8.8
# Expected: fails/times out (not in allowlist)
```

### 6.4 Stop and destroy (Sections 1, 8)

```bash
# Stop agent (SIGTERM → graceful shutdown):
hivectl agents stop test-vm-agent --cluster-root /var/lib/hive

# Verify state:
hivectl agents status test-vm-agent --cluster-root /var/lib/hive
# Expected: STOPPED

# Verify cleanup:
pgrep -a firecracker         # Should be gone
ip link show | grep hive      # TAP device should be gone
nft list tables | grep hive   # nftables rules should be gone

# Restart from stopped:
hivectl agents start test-vm-agent --cluster-root /var/lib/hive
# Should go back to RUNNING

# Full destroy:
hivectl agents destroy test-vm-agent --cluster-root /var/lib/hive
# Removes all state, drive images, CID freed
```

### 6.5 Crash recovery (Section 8)

```bash
# Start the agent:
hivectl agents start test-vm-agent --cluster-root /var/lib/hive

# Wait for RUNNING state, then kill the Firecracker process externally:
FC_PID=$(pgrep -f "firecracker.*test-vm-agent")
kill -9 $FC_PID

# Watch the health monitor detect the failure and auto-restart:
journalctl -u hived -f | grep -E "unhealthy|restart|test-vm-agent"
# Expected: agent marked FAILED, then auto-restarted with backoff
```

### 6.6 Concurrent VMs (Section 8)

Create a second agent manifest and start both:

```bash
# Copy and modify for a second agent:
mkdir -p /var/lib/hive/agents/test-vm-agent-2
sed 's/test-vm-agent/test-vm-agent-2/g' \
    /var/lib/hive/agents/test-vm-agent/manifest.yaml \
    > /var/lib/hive/agents/test-vm-agent-2/manifest.yaml

# Add to team manifest, then:
hivectl agents start test-vm-agent --cluster-root /var/lib/hive
hivectl agents start test-vm-agent-2 --cluster-root /var/lib/hive

# Verify both running with separate resources:
pgrep -a firecracker          # Two processes
ip link show | grep hive       # Two TAP devices
hivectl agents list --cluster-root /var/lib/hive  # Both RUNNING

# Verify no CID conflict:
journalctl -u hived | grep "CID"   # Each should have a unique CID

# Cleanup:
hivectl agents destroy test-vm-agent --cluster-root /var/lib/hive
hivectl agents destroy test-vm-agent-2 --cluster-root /var/lib/hive
```

### 6.7 Resource monitoring (Section 7)

```bash
# With VMs running, check metrics:
curl -s localhost:9090/metrics | grep hive_node

# Check resource monitoring logs:
journalctl -u hived | grep -E "memory|cpu|resource"
```

## Part 7: Run the Automated Test Suites

After manual verification, run the automated suites that require real hardware:

```bash
cd /path/to/hive

# Ensure mock is NOT set:
unset HIVE_TEST_FIRECRACKER

# VM-specific unit tests:
go test -tags vm -race -count=1 -timeout 10m ./internal/vm/...

# Full E2E with real Firecracker:
go test -tags e2e -race -count=1 -timeout 10m -v ./test/e2e/...

# Everything together:
go test -tags unit,integration,vm,e2e -race -count=1 -timeout 15m ./...

# VALIDATE.sh in real mode:
./VALIDATE.sh real

# Soak test (5 minutes of continuous operation):
make soak SOAK_DURATION=300
```

## Part 8: Tier 2 Agent Testing (Optional)

If testing with a Raspberry Pi or second Linux host:

### On the control plane host:

```bash
hivectl tokens create --cluster-root /var/lib/hive
# Save the output token
```

### On the Tier 2 device:

```bash
# Copy hive-agent binary (cross-compiled for arm64 if Pi):
scp user@build-host:hive/bin/linux-arm64/hive-agent ./

./hive-agent join \
    --token <token-from-above> \
    --control-plane <hive-host-ip>:4222 \
    --agent-id pi-agent-1 \
    --log-level debug
```

### Verify on control plane:

```bash
hivectl nodes list --cluster-root /var/lib/hive
# Should show the Pi node

hivectl nodes approve pi-agent-1 --cluster-root /var/lib/hive
# Node goes from PENDING → ONLINE
```

## Cleanup

```bash
# Stop everything:
systemctl stop hived              # or Ctrl+C if foreground

# Remove test state (optional):
rm -rf /var/lib/hive/state.db
rm -rf /var/lib/hive/.state/

# Uninstall (optional):
systemctl disable hived
rm /etc/systemd/system/hived.service
rm /usr/local/bin/{hived,hivectl,hive-agent,hive-sidecar}
userdel hive
rm -rf /var/lib/hive /var/log/hive
```
