# Hive v1.0 Pre-Release Checklist

Everything that must be done before rigorous local hardware testing and a public v1.0 release.

---

## 1. CRITICAL: Network Stack Completion

The Firecracker network path is incomplete. VMs with network policies will fail on real hardware.

### 1.1 TAP Device Lifecycle
- [ ] Implement TAP device creation (`ip tuntap add`) in `internal/vm/manager.go` before Firecracker VM boot
- [ ] Assign IP address to TAP device (host side: 172.16.x.1/30, guest side: 172.16.x.2)
- [ ] Bridge TAP device to host network (or configure IP masquerade/NAT)
- [ ] Delete TAP device on VM destroy (cleanup in `DestroyAgent` and `cleanupAgentNetworkPolicy`)
- [ ] Handle TAP device cleanup on crash recovery (`ReconcileOnStartup`)

### 1.2 Firecracker Network Device Configuration
- [ ] Pass TAP device name + guest MAC address in Firecracker API `network-interfaces` config
- [ ] Allocate unique MAC addresses per VM (deterministic from agent ID)
- [ ] Verify guest-side network comes up (sidecar.conf already sets IP, but host side is missing)

### 1.3 Privilege Model
- [ ] Document required Linux capabilities: `CAP_NET_ADMIN` (nftables, TAP), `CAP_SYS_ADMIN` (KVM)
- [ ] Add startup pre-flight check for capabilities — clear error messages when missing
- [ ] Add `--privileged` or `--skip-network` flag to run without network policy enforcement

### 1.4 nftables Verification
- [ ] Verify nftables rules actually apply on Ubuntu 22.04/24.04, Debian 12, Amazon Linux 2023
- [ ] Test `nft` binary detection at startup — clear error if missing
- [ ] Test egress=none (no TAP device at all, vsock only)
- [ ] Test egress=restricted (dnsmasq allowlist, iptables DROP default)
- [ ] Test egress=full (TAP with NAT, no restrictions)

---

## 2. CRITICAL: Rootfs & Kernel Bootstrapping

Users need a single command to go from zero to bootable VM.

### 2.1 Automated Kernel Download
- [ ] Add `make download-kernel` as a dependency of `make rootfs` (or integrate into hived startup)
- [ ] Support both x86_64 and arm64 kernels (detect host arch)
- [ ] Verify SHA-256 checksum of downloaded kernel
- [ ] Add `--kernel-path` flag to hived for custom kernel location
- [ ] Default kernel search: `{clusterRoot}/rootfs/vmlinux`, then `~/.hive/kernels/vmlinux-{arch}`

### 2.2 Rootfs Build Reliability
- [ ] Test `build-rootfs.sh` on Ubuntu 22.04, Ubuntu 24.04, Debian 12 (CI matrix)
- [ ] Add version check for `mkfs.ext4` (requires e2fsprogs >= 1.43 for `-d` flag)
- [ ] Add clear error when Docker is not running
- [ ] Add `make rootfs-openclaw` target to main Makefile

### 2.3 Pre-Built Release Artifacts
- [ ] Create GitHub Actions workflow to build rootfs images on release tags
- [ ] Publish: `hive-rootfs-amd64.ext4.gz`, `hive-rootfs-arm64.ext4.gz` (+ OpenClaw variants)
- [ ] Publish SHA-256 checksum files alongside images
- [ ] Publish `vmlinux-amd64` and `vmlinux-arm64` kernel binaries
- [ ] Test auto-download path in `internal/vm/images.go` against real release artifacts

### 2.4 Image URL Configuration
- [ ] Add `--image-url` flag or `cluster.yaml` field to override GitHub Releases URL
- [ ] Support air-gapped environments (local file path, internal mirror)

---

## 3. CRITICAL: Firecracker Integration Testing on Real Hardware

### 3.1 Real KVM Test Environment
- [ ] Set up CI runner with KVM access (self-hosted GitHub Actions runner, or use nested virt)
- [ ] Add `vm` build tag CI job that runs on KVM-capable runner
- [ ] Test full lifecycle: boot VM → sidecar connects → capability invocation → response → shutdown

### 3.2 Firecracker Binary Management
- [ ] Add `--firecracker-bin` flag to hived
- [ ] Add Firecracker version check at startup (minimum supported version)
- [ ] Document Firecracker installation steps per distro
- [ ] Consider bundling Firecracker binary in releases (or providing download script)

### 3.3 vsock Verification
- [ ] Verify `vhost_vsock` kernel module is loaded at startup — clear error if not
- [ ] Test vsock forwarding: guest NATS client → vsock → host NATS server
- [ ] Test with multiple concurrent VMs (CID allocation, vsock port multiplexing)

---

## 4. HIGH: Operational Tooling

### 4.1 Systemd Integration
- [ ] Create `hived.service` unit file with:
  - `User=hive`, `Group=hive`
  - `AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN`
  - `Restart=on-failure`, `RestartSec=5`
  - `LimitNOFILE=65536`
  - `Environment=` for cluster root, log level
- [ ] Create `hive-setup.sh` script: creates user, directories, installs unit file

### 4.2 Install Script
- [ ] `scripts/install.sh` that:
  - Downloads latest release binaries (hived, hivectl, hive-agent, hive-sidecar)
  - Downloads kernel + rootfs images
  - Installs systemd unit
  - Creates hive user/group
  - Sets up `/var/lib/hive/` directory structure
  - Runs pre-flight checks (KVM, nftables, Docker for rootfs builds)
- [ ] Test on Ubuntu 22.04, Ubuntu 24.04, Debian 12

### 4.3 Health & Readiness
- [ ] Add `GET /healthz` endpoint to hived (returns 200 when ready to accept agents)
- [ ] Add `GET /readyz` endpoint (returns 200 when NATS, state store, reconciler all healthy)
- [ ] Wire into systemd `Type=notify` or `ExecStartPost=` health check

### 4.4 CLI Diagnostics
- [ ] `hivectl doctor` command that checks:
  - Go version, Firecracker binary + version, KVM access, nftables, Docker
  - Rootfs image availability, kernel availability
  - Disk space, memory availability
  - NATS connectivity (if hived running)
  - Prints pass/fail checklist with remediation hints

---

## 5. HIGH: v1.0 Release Engineering

### 5.1 Release Workflow
- [ ] Create `.github/workflows/release.yml`:
  - Triggered on `v*` tags
  - Builds binaries for darwin/arm64, linux/amd64, linux/arm64
  - Builds rootfs images (base + OpenClaw) for amd64 and arm64
  - Creates GitHub Release with all artifacts + checksums
  - Generates changelog from commits since last tag
- [ ] Test release workflow on a `v0.1.0-rc1` tag first

### 5.2 Version Embedding
- [ ] Verify `-ldflags "-X main.version=${VERSION}"` works for all 4 binaries
- [ ] Add `hivectl version` command showing version, commit, build date, Go version
- [ ] Add version to hived startup log and `/healthz` response

### 5.3 Cross-Platform Builds
- [ ] Fix `make build-linux-arm64` to include ALL binaries (currently missing hived, hive-sidecar)
- [ ] Add darwin/amd64 build target (Intel Macs)
- [ ] Verify all binaries work with CGO_ENABLED=0

### 5.4 Go Module Hygiene
- [ ] Verify `go mod tidy` produces no changes
- [ ] Verify all dependencies are vendored and up to date
- [ ] Run `govulncheck` and address any findings
- [ ] Pin Go version in `go.mod` to stable release

---

## 6. HIGH: Documentation for v1.0

### 6.1 README Rewrite
- [ ] Clear value proposition in first 3 lines
- [ ] Quickstart: 5 commands from clone to running demo (process backend, any OS)
- [ ] "Real Hardware" section: steps for Linux + KVM + Firecracker
- [ ] Supported platforms matrix:
  - macOS (Apple Silicon): process backend only, full dev experience
  - Linux x86_64 + KVM: full Firecracker isolation
  - Linux arm64 + KVM: full Firecracker isolation
- [ ] Link to detailed docs for each section

### 6.2 Getting Started Guide (Rewrite)
- [ ] Part 1: Local development (any OS) — `hivectl init`, `hivectl dev`, `hivectl trigger`
- [ ] Part 2: Writing agents — Python SDK, Go SDK, TypeScript SDK, shell scripts
- [ ] Part 3: Real hardware — prerequisites, install, boot first VM, verify isolation
- [ ] Part 4: Production deployment — systemd, monitoring, log aggregation

### 6.3 Operations Guide (New/Rewrite)
- [ ] Prerequisites checklist (hardware, software, kernel modules)
- [ ] Installation (from release, from source)
- [ ] Configuration reference (cluster.yaml, agent manifest, team manifest)
- [ ] Network policy guide with diagrams
- [ ] Troubleshooting section (common errors, diagnostic commands)
- [ ] Backup and recovery (state.db, agent workspaces)

### 6.4 Architecture Guide (Update)
- [ ] Firecracker VM lifecycle diagram
- [ ] Network topology diagram (TAP, bridge, vsock, NATS)
- [ ] Capability routing flow diagram
- [ ] State machine diagrams for agent lifecycle

### 6.5 API Reference
- [ ] Sidecar HTTP API (all endpoints with request/response examples)
- [ ] NATS subject hierarchy (all subjects with payload schemas)
- [ ] SDK API reference (Python, Go, TypeScript)
- [ ] CLI reference (all commands with flags and examples)

---

## 7. MEDIUM: Testing Gaps

### 7.1 Real Hardware Test Suite
- [ ] Test matrix: 1 VM, 5 VMs, 20 VMs (resource exhaustion)
- [ ] Test long-running stability (24h soak test with periodic triggers)
- [ ] Test agent crash + auto-restart via health monitor
- [ ] Test network policy enforcement with actual DNS resolution and HTTP requests from inside VMs
- [ ] Test shared volume read/write between two VMs
- [ ] Test `hivectl dev` → edit manifest → hot-reload cycle
- [ ] Test OOM behavior inside VMs (agent exceeds memory limit)
- [ ] Test disk quota enforcement (writes beyond spec.resources.disk)

### 7.2 Failure Mode Testing
- [ ] Kill hived while VMs running — verify ReconcileOnStartup recovers
- [ ] Kill Firecracker process — verify health monitor detects and restarts
- [ ] Network partition between agent and NATS — verify reconnection
- [ ] Disk full on host — verify graceful failure
- [ ] NATS server crash — verify agents reconnect

### 7.3 Performance Benchmarks
- [ ] VM cold start time (target: < 3s from API call to capability response)
- [ ] Capability invocation latency (target: < 50ms for simple echo)
- [ ] Maximum concurrent VMs on 32GB host
- [ ] NATS throughput under load

---

## 8. MEDIUM: Code Quality for Public Release

### 8.1 License & Headers
- [ ] Verify Apache-2.0 header on ALL .go files
- [ ] Add LICENSE file at repo root (if not present)
- [ ] Add NOTICE file listing third-party dependencies and their licenses
- [ ] Verify SDK license headers (Python, Go, TypeScript)

### 8.2 Code Cleanup
- [ ] Remove any TODO/FIXME/HACK comments that reference internal context
- [ ] Remove any hardcoded paths or credentials
- [ ] Audit for any internal GitHub URLs, Slack links, or private references
- [ ] Ensure no test files contain real API keys or tokens

### 8.3 CI Pipeline for v1.0
- [ ] All existing CI jobs green (currently: yes)
- [ ] Add golangci-lint with strict config (currently passing)
- [ ] Add license header check
- [ ] Add `go mod tidy` check (fail if go.mod/go.sum change)
- [ ] Consider adding CodeQL or Semgrep for security scanning

---

## 9. LOW: Nice-to-Have Before v1.0

### 9.1 Developer Experience Polish
- [ ] `hivectl logs <agent-id>` — tail agent logs in real-time
- [ ] `hivectl exec <agent-id> -- <command>` — run command in agent context
- [ ] Colorized CLI output (agent status table, health indicators)
- [ ] `hivectl dev` progress indicator during startup (instead of raw log lines)

### 9.2 Observability
- [ ] Grafana dashboard JSON for Prometheus metrics
- [ ] Example Prometheus scrape config
- [ ] Structured event log (agent started, capability invoked, error occurred)

### 9.3 Security Hardening Documentation
- [ ] Threat model document
- [ ] Security policy (SECURITY.md) with responsible disclosure process
- [ ] Dependency audit report

---

## Priority Order for Execution

### Week 1: Make it work on real hardware
1. TAP device lifecycle (§1.1, §1.2)
2. Kernel download automation (§2.1)
3. Rootfs build verification on Linux (§2.2)
4. Firecracker binary management (§3.2)
5. vsock module check (§3.3)
6. Privilege pre-flight checks (§1.3)

### Week 2: Make it installable
7. Install script (§4.2)
8. Systemd unit file (§4.1)
9. `hivectl doctor` command (§4.4)
10. Health endpoints (§4.3)

### Week 3: Real hardware test campaign
11. Set up KVM test environment (§3.1)
12. Run full test matrix (§7.1)
13. Failure mode testing (§7.2)
14. Fix everything that breaks

### Week 4: Release engineering
15. Release workflow (§5.1)
16. Pre-built release artifacts (§2.3)
17. Cross-platform builds (§5.3)
18. Version embedding (§5.2)

### Week 5: Documentation
19. README rewrite (§6.1)
20. Getting started guide (§6.2)
21. Operations guide (§6.3)
22. API reference (§6.5)

### Week 6: Polish & ship
23. Code cleanup (§8.2)
24. License audit (§8.1)
25. Performance benchmarks (§7.3)
26. Tag v1.0.0, publish release
