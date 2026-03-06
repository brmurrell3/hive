# DEPLOYMENT SPEC v3

Consolidated deployment spec for Hive covering NixOS configuration, bootstrap/installation, pre-built images, node discovery/join, and agent-to-node assignment.

Format: optimized for Claude Code. Terse, structured, deterministic. Minimal prose.

Resolves review issue #4 (agent-to-node assignment friction).

---

## NIXOS MODULE (services.hive)

```
enable: bool
clusterRoot: string, path to cluster root directory
role: enum(control-plane|worker|hybrid), default hybrid
  control-plane: runs hived, does not accept external scheduling
  worker: joins existing cluster, accepts VM scheduling
  hybrid: runs hived AND accepts scheduling (single-node default)
joinToken: string, OPTIONAL, for worker nodes joining cluster
controlPlane: string, OPTIONAL, NATS address of control plane (for worker nodes)

nats:
  mode: enum(embedded|external), default embedded
    embedded: hived manages NATS in-process (Phase 1 single-node)
    external: separate nats.service (Phase 3 multi-node)
  port: int, default 4222
  clusterPort: int, default 6222
  clusterPeers: list of NATS URLs (gossip handles discovery after initial contact)
  mqtt:
    enabled: bool, default true
    port: int, default 1883

network:
  bridge: string, default hivebr0
  subnet: string CIDR, default 172.30.0.0/16

firecracker:
  kernelPackage: derivation
  rootfsDerivation: derivation

storage:
  statePath: string, default /var/lib/hive
```

### Module Effects When Enabled

1. Installs: firecracker, hived, hivectl (nats-server only if nats.mode=external)
2. Creates user hive with KVM access
3. Creates directories: statePath, /run/hive
4. Creates bridge hivebr0 with subnet
5. Enables IP forwarding and NAT masquerade
6. If nats.mode=external: creates systemd nats.service with MQTT listener
7. Creates systemd hived.service (depends on nats.service if external mode)
8. Opens firewall: NATS port, NATS cluster port, MQTT port

---

## FLAKE STRUCTURE

```
nix/flake.nix
nix/host/hive.nix                    # Tier 1 host NixOS module
nix/host/nats.nix                    # NATS server config (external mode only)
nix/host/network.nix                 # bridge and firewall
nix/vm/default.nix                   # base VM rootfs (per arch)
nix/vm/sidecar.nix                   # sidecar binary for VM
nix/vm/kernel.nix                    # VM kernel
nix/images/tier1-worker.nix          # Tier 1 worker image
nix/images/tier2-pi3.nix             # Pi 3 image (armv7)
nix/images/tier2-pi4.nix             # Pi 4 image (arm64)
nix/images/tier2-pi5.nix             # Pi 5 image (arm64)
nix/images/tier2-pizero2w.nix        # Pi Zero 2 W image (arm64)
nix/images/tier2-generic-arm64.nix   # generic arm64
nix/images/tier2-generic-amd64.nix   # generic amd64
nix/packages/hived.nix               # control plane binary
nix/packages/hivectl.nix             # CLI binary
nix/packages/hive-agent.nix          # Tier 2 agent host binary
nix/devshells/firmware-esp-idf.nix   # ESP-IDF dev environment
nix/devshells/firmware-arduino.nix   # arduino-cli dev environment
nix/devshells/firmware-pico.nix      # Pico SDK dev environment
nix/devshells/firmware-zephyr.nix    # Zephyr dev environment
```

---

## BOOTSTRAP: CONTROL PLANE SETUP (Tier 1 NixOS)

1. Install NixOS on KVM-capable machine (4GB+ RAM)
2. Enable flakes in nix.conf
3. Add Hive flake input to system flake.nix
4. Import hive.nixosModules.default
5. Set services.hive.enable = true, services.hive.role = hybrid
6. nixos-rebuild switch (installs Firecracker, NATS, hived, hivectl, builds VM base images)
7. hivectl init /path/to/cluster-root (creates cluster.yaml, agents/, teams/, .state/)
8. hivectl tokens create (save the token)
9. Edit cluster.yaml: set secrets, model registry, adjust defaults
10. hivectl status: verify 1 node (self), 0 agents, NATS connected

---

## ADDING TIER 1 WORKER NODE

Option A: NixOS with module
1. Install NixOS on KVM-capable machine
2. Add Hive flake, import module
3. Set services.hive.enable = true, role = worker
4. Set services.hive.joinToken = token, services.hive.controlPlane = address
5. If multi-node NATS: set services.hive.nats.mode = external, nats.clusterPeers = [control_plane_nats_url]
6. nixos-rebuild switch
7. Node joins cluster automatically

Option B: Pre-built image
1. Flash pre-built Tier 1 worker image
2. Write hive-config.yaml to boot partition
3. Boot and join

---

## ADDING TIER 2 DEVICE

### Path A: Pre-built Image

1. Download image for board (e.g., hive-tier2-pi4-arm64.img)
2. Flash to SD card
3. Mount boot partition, create hive-config.yaml:
   ```
   joinToken: TOKEN
   controlPlane: CONTROL_PLANE_ADDRESS:4222
   labels:
     location: greenhouse
     hardware: camera-pi
   agentId: greenhouse-camera  # OPTIONAL, self-declare agent identity
   ```
4. Power on. Device joins cluster.

### Path B: Existing Linux

1. Download hive-agent binary for target arch
2. Place at /usr/local/bin/hive-agent
3. Create /etc/hive/config.yaml:
   ```
   joinToken: TOKEN
   controlPlane: CONTROL_PLANE_ADDRESS:4222
   labels: {}
   agentId: ""  # OPTIONAL
   ```
4. Install systemd unit: systemctl enable --now hive-agent
5. Device joins cluster

---

## ADDING TIER 3 DEVICE

1. Write agent manifest: agents/{AGENT_ID}/manifest.yaml with tier: firmware, capabilities, firmware config
2. Write firmware source in agents/{AGENT_ID}/firmware/
3. Build: hivectl firmware build {AGENT_ID} --target {PLATFORM}
4. Flash: hivectl firmware flash {AGENT_ID} --port /dev/ttyUSB0
   OR for deployed devices: hivectl firmware update {AGENT_ID} --binary path/to/firmware.bin
5. Device boots, connects WiFi, joins via MQTT

WiFi credentials and join token embedded at build time (from cluster secrets and build config).

---

## NODE DISCOVERY AND JOIN PROTOCOL

### Join Token

- hivectl tokens create: 256-bit random, print to stdout, store SHA-256 hash in .state/cluster/tokens.json
- Tokens do not expire by default. --ttl flag for expiry.
- Control plane NEVER stores raw token.

### Per-Tier Join Mechanism

**Tier 1 NixOS:**
- hived starts, connects to cluster NATS (if worker role), presents token
- Collects hardware inventory, publishes join request

**Tier 2 Linux:**
- hive-agent reads config, connects to NATS, presents token
- Collects hardware inventory, publishes join request

**Tier 3 Microcontroller:**
- Firmware connects MQTT, presents token as password
- Sends capability list and device info in join request

### Hardware Inventory (Tier 1 and Tier 2)

Collected at join time:
```
arch: uname -m -> amd64|arm64|armv7|armv6
memory_total: /proc/meminfo
cpu_count: nproc
disk_total: root fs size
kvm_available: bool, /dev/kvm exists
gpus: lspci scan
peripherals: USB device list, I2C bus scan, GPIO chip detection
hostname: system hostname
mac_address: primary interface MAC (stable identification)
```

### Device Inventory (Tier 3)

```
arch: rp2040|esp32|esp8266|stm32|etc
free_heap: bytes
flash_size: total flash
capabilities: from firmware SDK registration
firmware_version: string
mode: tool|peer
message_format: json|msgpack
```

### Approval

- autoApprove=true (default): immediate registration
- autoApprove=false: node enters PENDING. hivectl nodes approve NODE_ID activates.

### Post-Approval

Control plane sends to node:
- NATS credentials (unique per node)
- Assigned node ID
- Tier 1: pending VM agent assignments
- Tier 2: pending agent assignment if any
- Tier 3: confirmation and subscription instructions

### Auto-Generated Labels

```
hive.io/arch: amd64|arm64|armv7|rp2040|etc
hive.io/tier: 1|2|3
hive.io/kvm: true|false
hive.io/gpu: GPU type if detected
```
Plus user-provided labels from config file.

### Re-registration

Device wiped and re-flashed: re-runs join. Control plane matches by hostname or MAC. Updates inventory. Previous agent state lost. Agents rescheduled.

### Node Removal

**hivectl nodes remove NODE_ID:**
- Tier 1: drains VMs to other nodes, deregisters
- Tier 2: stops agent, deregisters
- Tier 3: deregisters (device continues running but disconnected from cluster)

**Physical removal (unplug):** missed heartbeats → offline. Agents rescheduled. Node record remains until explicit remove.

---

## AGENT-TO-NODE ASSIGNMENT

**Issue #4 Resolution:** Tier 2 device joins → gets auto-generated node ID → user must find ID via hivectl nodes list → write into manifest placement.nodeId. Friction.

**Solution:** TWO mechanisms with priority-based matching.

### Mechanism 1: Self-Declaration (NEW, resolves #4)

Tier 2 device config (hive-config.yaml or /etc/hive/config.yaml) includes optional agentId field.

When agentId is set:
1. Device joins cluster
2. hive-agent reports agentId in join request
3. Control plane checks: does agents/{agentId}/manifest.yaml exist?
4. If yes AND manifest tier is native: auto-assign this agent to this node. No placement.nodeId needed.
5. If no manifest yet: node registered, waits. When manifest appears, auto-matched.
6. If manifest exists but tier is not native: error logged, assignment rejected.

Tier 3 devices always self-declare: firmware agent_id is baked into firmware at build time.

### Mechanism 2: Explicit Pinning (existing)

Agent manifest placement.nodeId: NODE_ID
- User finds node ID via hivectl nodes list
- Writes into manifest
- Control plane matches agent to node

### Mechanism 3: Label Matching (existing)

Agent manifest placement.nodeLabels: {location: greenhouse, hardware: camera}
- Node has matching labels (from config or hivectl nodes label)
- Scheduler matches agent to any node with matching labels
- Works for both Tier 1 and Tier 2

### Priority (evaluated in order)

1. Self-declaration (Mechanism 1)
2. Explicit nodeId (Mechanism 2)
3. Label matching (Mechanism 3)
4. Unconstrained scheduling

---

## PRE-BUILT IMAGES

### Tier 1 Worker Image (amd64, arm64)

Full NixOS, Hive module pre-configured in worker mode.
User writes hive-config.yaml to boot partition. Boots and joins.

### Tier 2 Device Images

Minimal NixOS with hive-agent:
- Raspberry Pi 3 (armv7)
- Raspberry Pi 4 (arm64)
- Raspberry Pi 5 (arm64)
- Raspberry Pi Zero 2 W (arm64)
- Generic arm64
- Generic amd64

All: write hive-config.yaml to boot partition, power on, done.

### Non-NixOS Tier 2

hive-agent is static Go binary. Runs on any Linux with systemd. Download binary + systemd unit. Supported but not primary path.

---

## UPGRADE PATH

**Control plane:** update Hive flake, nixos-rebuild switch. Rebuilds binaries and VM images. Running VMs NOT auto-restarted. hivectl agents restart AGENT_ID for individual. hivectl agents restart --all for rolling restart.

**Tier 2:** download new hive-agent binary, restart service. Or reflash NixOS image.

**Tier 3:** hivectl firmware update for OTA.

---

## UNINSTALL

**Control plane:** services.hive.enable = false, nixos-rebuild switch. Does not delete cluster root.

**Tier 2:** systemctl disable hive-agent. Or reflash.

**Tier 3:** reflash with non-Hive firmware. Or unplug.

---

## VERIFICATION

```
hivectl status                                          # nodes by tier, agents, teams, NATS/MQTT status
hivectl nodes list                                      # all registered nodes
hivectl validate                                        # manifest validation
hivectl capabilities invoke AGENT_ID CAPABILITY --inputs {}  # end-to-end capability test
```
