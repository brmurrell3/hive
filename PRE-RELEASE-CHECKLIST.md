# Hive v1.0 Pre-Release Checklist

Everything that must be done before rigorous local hardware testing and a public v1.0 release.

---

## 1. CRITICAL: Network Stack Completion

The Firecracker network path is incomplete. VMs with network policies will fail on real hardware.

### 1.1 TAP Device Lifecycle
- [x] Implement TAP device creation (`ip tuntap add`) in `internal/vm/tap.go` before Firecracker VM boot
- [x] Assign IP address to TAP device (host side: 172.16.x.1/30, guest side: 172.16.x.2)
- [x] Bridge TAP device to host network (or configure IP masquerade/NAT)
- [x] Delete TAP device on VM destroy (cleanup in `DestroyAgent` and `cleanupAgentNetworkPolicy`)
- [x] Handle TAP device cleanup on crash recovery (`ReconcileOnStartup`)

### 1.2 Firecracker Network Device Configuration
- [x] Pass TAP device name + guest MAC address in Firecracker API `network-interfaces` config
- [x] Allocate unique MAC addresses per VM (deterministic from agent ID)
- [x] Verify guest-side network comes up (sidecar.conf already sets IP, but host side is missing)

### 1.3 Privilege Model
- [x] Document required Linux capabilities: `CAP_NET_ADMIN` (nftables, TAP), `CAP_SYS_ADMIN` (KVM)
- [x] Add startup pre-flight check for capabilities — clear error messages when missing
- [x] Add `--privileged` or `--skip-network` flag to run without network policy enforcement

### 1.4 nftables Verification
- [x] Verify nftables rules actually apply on Ubuntu 22.04/24.04, Debian 12, Amazon Linux 2023
- [x] Test `nft` binary detection at startup — clear error if missing
- [x] Test egress=none (no TAP device at all, vsock only)
- [x] Test egress=restricted (dnsmasq allowlist, iptables DROP default)
- [x] Test egress=full (TAP with NAT, no restrictions)

---

## 2. CRITICAL: Rootfs & Kernel Bootstrapping

Users need a single command to go from zero to bootable VM.

### 2.1 Automated Kernel Download
- [x] Add `make download-kernel` as a dependency of `make rootfs` (or integrate into hived startup)
- [x] Support both x86_64 and arm64 kernels (detect host arch)
- [x] Verify SHA-256 checksum of downloaded kernel
- [x] Add `--kernel-path` flag to hived for custom kernel location
- [x] Default kernel search: `{clusterRoot}/rootfs/vmlinux`, then `~/.hive/kernels/vmlinux-{arch}`

### 2.2 Rootfs Build Reliability
- [x] Test `build-rootfs.sh` on Ubuntu 22.04, Ubuntu 24.04, Debian 12 (CI matrix)
- [x] Add version check for `mkfs.ext4` (requires e2fsprogs >= 1.43 for `-d` flag)
- [x] Add clear error when Docker is not running
- [x] Add `make rootfs-openclaw` target to main Makefile

### 2.3 Pre-Built Release Artifacts
- [x] Create GitHub Actions workflow to build rootfs images on release tags
- [x] Publish: `hive-rootfs-amd64.ext4.gz`, `hive-rootfs-arm64.ext4.gz` (+ OpenClaw variants)
- [x] Publish SHA-256 checksum files alongside images
- [x] Publish `vmlinux-amd64` and `vmlinux-arm64` kernel binaries
- [x] Test auto-download path in `internal/vm/images.go` against real release artifacts

### 2.4 Image URL Configuration
- [x] Add `--image-url` flag or `cluster.yaml` field to override GitHub Releases URL
- [x] Support air-gapped environments (local file path, internal mirror)

---

## 3. CRITICAL: Firecracker Integration Testing on Real Hardware

### 3.1 Real KVM Test Environment
- [x] Set up CI runner with KVM access (self-hosted GitHub Actions runner, or use nested virt)
- [x] Add `vm` build tag CI job that runs on KVM-capable runner
- [x] Test full lifecycle: boot VM → sidecar connects → capability invocation → response → shutdown

### 3.2 Firecracker Binary Management
- [x] Add `--firecracker-bin` flag to hived
- [x] Add Firecracker version check at startup (minimum supported version)
- [x] Document Firecracker installation steps per distro
- [x] Consider bundling Firecracker binary in releases (or providing download script)

### 3.3 vsock Verification
- [x] Verify `vhost_vsock` kernel module is loaded at startup — clear error if not
- [x] Test vsock forwarding: guest NATS client → vsock → host NATS server
- [x] Test with multiple concurrent VMs (CID allocation, vsock port multiplexing)

---

## 4. HIGH: Operational Tooling

### 4.1 Systemd Integration
- [x] Create `hived.service` unit file with:
  - `User=hive`, `Group=hive`
  - `AmbientCapabilities=CAP_NET_ADMIN CAP_SYS_ADMIN`
  - `Restart=on-failure`, `RestartSec=5`
  - `LimitNOFILE=65536`
  - `Environment=` for cluster root, log level
- [x] Create `hive-setup.sh` script: creates user, directories, installs unit file

### 4.2 Install Script
- [x] `scripts/install.sh` that:
  - Downloads latest release binaries (hived, hivectl, hive-agent, hive-sidecar)
  - Downloads kernel + rootfs images
  - Installs systemd unit
  - Creates hive user/group
  - Sets up `/var/lib/hive/` directory structure
  - Runs pre-flight checks (KVM, nftables, Docker for rootfs builds)
- [x] Test on Ubuntu 22.04, Ubuntu 24.04, Debian 12

### 4.3 Health & Readiness
- [x] Add `GET /healthz` endpoint to hived (returns 200 when ready to accept agents)
- [x] Add `GET /readyz` endpoint (returns 200 when NATS, state store, reconciler all healthy)
- [x] Wire into systemd `Type=notify` or `ExecStartPost=` health check

### 4.4 CLI Diagnostics
- [x] `hivectl doctor` command that checks:
  - Go version, Firecracker binary + version, KVM access, nftables, Docker
  - Rootfs image availability, kernel availability
  - Disk space, memory availability
  - NATS connectivity (if hived running)
  - Prints pass/fail checklist with remediation hints

---

## 5. HIGH: v1.0 Release Engineering

### 5.1 Release Workflow
- [x] Create `.github/workflows/release.yml`:
  - Triggered on `v*` tags
  - Builds binaries for darwin/arm64, linux/amd64, linux/arm64
  - Builds rootfs images (base + OpenClaw) for amd64 and arm64
  - Creates GitHub Release with all artifacts + checksums
  - Generates changelog from commits since last tag
- [x] Test release workflow on a `v0.1.0-rc1` tag first

### 5.2 Version Embedding
- [x] Verify `-ldflags "-X main.version=${VERSION}"` works for all 4 binaries
- [x] Add `hivectl version` command showing version, commit, build date, Go version
- [x] Add version to hived startup log and `/healthz` response

### 5.3 Cross-Platform Builds
- [x] Fix `make build-linux-arm64` to include ALL binaries (currently missing hived, hive-sidecar)
- [x] Add darwin/amd64 build target (Intel Macs)
- [x] Verify all binaries work with CGO_ENABLED=0

### 5.4 Go Module Hygiene
- [x] Verify `go mod tidy` produces no changes
- [x] Verify all dependencies are vendored and up to date
- [x] Run `govulncheck` and address any findings
- [x] Pin Go version in `go.mod` to stable release

---

## 6. HIGH: Documentation for v1.0

### 6.1 README Rewrite
- [x] Clear value proposition in first 3 lines
- [x] Quickstart: 5 commands from clone to running demo (process backend, any OS)
- [x] "Real Hardware" section: steps for Linux + KVM + Firecracker
- [x] Supported platforms matrix:
  - macOS (Apple Silicon): process backend only, full dev experience
  - Linux x86_64 + KVM: full Firecracker isolation
  - Linux arm64 + KVM: full Firecracker isolation
- [x] Link to detailed docs for each section

### 6.2 Getting Started Guide (Rewrite)
- [x] Part 1: Local development (any OS) — `hivectl init`, `hivectl dev`, `hivectl trigger`
- [x] Part 2: Writing agents — Python SDK, Go SDK, TypeScript SDK, shell scripts
- [x] Part 3: Real hardware — prerequisites, install, boot first VM, verify isolation
- [x] Part 4: Production deployment — systemd, monitoring, log aggregation

### 6.3 Operations Guide (New/Rewrite)
- [x] Prerequisites checklist (hardware, software, kernel modules)
- [x] Installation (from release, from source)
- [x] Configuration reference (cluster.yaml, agent manifest, team manifest)
- [x] Network policy guide with diagrams
- [x] Troubleshooting section (common errors, diagnostic commands)
- [x] Backup and recovery (state.db, agent workspaces)

### 6.4 Architecture Guide (Update)
- [x] Firecracker VM lifecycle diagram
- [x] Network topology diagram (TAP, bridge, vsock, NATS)
- [x] Capability routing flow diagram
- [x] State machine diagrams for agent lifecycle

### 6.5 API Reference
- [x] Sidecar HTTP API (all endpoints with request/response examples)
- [x] NATS subject hierarchy (all subjects with payload schemas)
- [x] SDK API reference (Python, Go, TypeScript)
- [x] CLI reference (all commands with flags and examples)

---

## 7. MEDIUM: Testing Gaps

### 7.1 Real Hardware Test Suite
- [x] Test matrix: 1 VM, 5 VMs, 20 VMs (resource exhaustion)
- [x] Test long-running stability (24h soak test with periodic triggers)
- [x] Test agent crash + auto-restart via health monitor
- [x] Test network policy enforcement with actual DNS resolution and HTTP requests from inside VMs
- [x] Test shared volume read/write between two VMs
- [x] Test `hivectl dev` → edit manifest → hot-reload cycle
- [x] Test OOM behavior inside VMs (agent exceeds memory limit)
- [x] Test disk quota enforcement (writes beyond spec.resources.disk)

### 7.2 Failure Mode Testing
- [x] Kill hived while VMs running — verify ReconcileOnStartup recovers
- [x] Kill Firecracker process — verify health monitor detects and restarts
- [x] Network partition between agent and NATS — verify reconnection
- [x] Disk full on host — verify graceful failure
- [x] NATS server crash — verify agents reconnect

### 7.3 Performance Benchmarks
- [x] VM cold start time (target: < 3s from API call to capability response)
- [x] Capability invocation latency (target: < 50ms for simple echo)
- [x] Maximum concurrent VMs on 32GB host
- [x] NATS throughput under load

---

## 8. MEDIUM: Code Quality for Public Release

### 8.1 License & Headers
- [x] Verify Apache-2.0 header on ALL .go files
- [x] Add LICENSE file at repo root (if not present)
- [x] Add NOTICE file listing third-party dependencies and their licenses
- [x] Verify SDK license headers (Python, Go, TypeScript)

### 8.2 Code Cleanup
- [x] Remove any TODO/FIXME/HACK comments that reference internal context
- [x] Remove any hardcoded paths or credentials
- [x] Audit for any internal GitHub URLs, Slack links, or private references
- [x] Ensure no test files contain real API keys or tokens

### 8.3 CI Pipeline for v1.0
- [x] All existing CI jobs green (currently: yes)
- [x] Add golangci-lint with strict config (currently passing)
- [x] Add license header check
- [x] Add `go mod tidy` check (fail if go.mod/go.sum change)
- [x] Consider adding CodeQL or Semgrep for security scanning

---

## 9. LOW: Nice-to-Have Before v1.0

### 9.1 Developer Experience Polish
- [x] `hivectl logs <agent-id>` — tail agent logs in real-time
- [x] `hivectl exec <agent-id> -- <command>` — run command in agent context
- [x] Colorized CLI output (agent status table, health indicators)
- [x] `hivectl dev` progress indicator during startup (instead of raw log lines)

### 9.2 Observability
- [x] Grafana dashboard JSON for Prometheus metrics
- [x] Example Prometheus scrape config
- [x] Structured event log (agent started, capability invoked, error occurred)

### 9.3 Security Hardening Documentation
- [x] Threat model document
- [x] Security policy (SECURITY.md) with responsible disclosure process
- [x] Dependency audit report

---

## Priority Order for Execution

### Week 1: Make it work on real hardware
1. TAP device lifecycle (S1.1, S1.2) -- DONE
2. Kernel download automation (S2.1) -- DONE
3. Rootfs build verification on Linux (S2.2) -- DONE
4. Firecracker binary management (S3.2) -- DONE
5. vsock module check (S3.3) -- DONE
6. Privilege pre-flight checks (S1.3) -- DONE

### Week 2: Make it installable
7. Install script (S4.2) -- DONE
8. Systemd unit file (S4.1) -- DONE
9. `hivectl doctor` command (S4.4) -- DONE
10. Health endpoints (S4.3) -- DONE

### Week 3: Real hardware test campaign
11. Set up KVM test environment (S3.1) -- DONE
12. Run full test matrix (S7.1) -- DONE
13. Failure mode testing (S7.2) -- DONE
14. Fix everything that breaks -- DONE

### Week 4: Release engineering
15. Release workflow (S5.1) -- DONE
16. Pre-built release artifacts (S2.3) -- DONE
17. Cross-platform builds (S5.3) -- DONE
18. Version embedding (S5.2) -- DONE

### Week 5: Documentation
19. README rewrite (S6.1) -- DONE
20. Getting started guide (S6.2) -- DONE
21. Operations guide (S6.3) -- DONE
22. API reference (S6.5) -- DONE

### Week 6: Polish & ship
23. Code cleanup (S8.2) -- DONE
24. License audit (S8.1) -- DONE
25. Performance benchmarks (S7.3) -- DONE
26. Tag v1.0.0, publish release -- READY
