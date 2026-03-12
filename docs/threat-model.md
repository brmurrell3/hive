# Hive Threat Model

This document describes the trust boundaries, attack vectors, and security
mitigations implemented in the Hive agent orchestration platform.

## Architecture Overview

Hive orchestrates LLM agents across heterogeneous hardware using a
control-plane/data-plane architecture. The control plane (hived) manages
agent lifecycle, state, and communication. Agents run in isolated
environments and communicate via an embedded NATS message bus.

## Trust Boundaries

### 1. Control Plane (hived)

The control plane is the highest-trust component. It:

- Owns the state database (SQLite) and all agent lifecycle decisions
- Runs the embedded NATS server with authentication
- Manages RBAC enforcement for all control operations
- Coordinates cluster federation with peer nodes

**Trust level:** Full trust. Compromise of the control plane means
compromise of the entire cluster.

### 2. Agent VMs (Firecracker microVMs)

Tier 1 agents run inside Firecracker microVMs with:

- Minimal Linux kernel and rootfs
- No network access by default (nftables isolation)
- Communication only via vsock to the host sidecar
- Read-only rootfs with overlay for agent workspace

**Trust level:** Untrusted. Agents execute arbitrary LLM-generated code
and must be treated as potentially compromised. The VM boundary is the
primary isolation mechanism.

### 3. Tier 2 Agents (Native Processes)

Tier 2 agents run as native processes on the host:

- Process-level isolation only (no VM boundary)
- NATS authentication required for all communication
- Subject to OS-level resource limits

**Trust level:** Semi-trusted. Less isolation than Tier 1. Suitable for
agents that need host hardware access (GPIO, USB, etc.) on trusted
hardware.

### 4. Tier 3 Agents (Firmware/Embedded)

Tier 3 agents run on embedded devices (ESP32, STM32, etc.):

- Communicate via MQTT bridge to NATS
- Ed25519 firmware signing for OTA updates
- Limited compute and memory resources

**Trust level:** Semi-trusted. Physical access to devices is assumed
possible. Firmware signing prevents unauthorized code execution.

### 5. External Network

All external network traffic is untrusted:

- TLS required for all inter-node communication
- TLS required for MQTT bridge connections
- Dashboard API enforces CORS and CSP policies
- No NATS ports exposed externally by default

**Trust level:** Untrusted. All external input is validated.

### 6. CLI Tools (hivectl)

The CLI is a client-side tool that:

- Authenticates via RBAC tokens
- Communicates with hived via NATS
- Has no persistent state of its own

**Trust level:** Trusted to the level of the authenticated user's RBAC role.

## Attack Vectors and Mitigations

### A1: NATS Message Injection

**Vector:** An attacker gains access to the NATS bus and publishes
malicious control messages.

**Mitigations:**
- NATS server requires authentication (token-based)
- All control messages are wrapped in validated Envelope types
- Subject component validation prevents NATS subject injection
- Payload size limits (2MB) prevent resource exhaustion
- Correlation IDs are validated to prevent replay attacks

### A2: Agent VM Escape

**Vector:** Malicious code running inside an agent VM attempts to
escape the Firecracker sandbox.

**Mitigations:**
- Firecracker microVMs provide hardware-level isolation via KVM
- Minimal attack surface (no virtio-net, no PCI, vsock only)
- nftables rules prevent VM-to-host network access
- Read-only rootfs prevents persistent modification
- CID (vsock context ID) management prevents CID reuse attacks

### A3: Privilege Escalation via RBAC

**Vector:** A viewer-role user attempts to perform admin operations.

**Mitigations:**
- Three-tier RBAC (admin, operator, viewer) enforced on all control
  plane operations
- Token-based authentication with SHA-256 hashing
- Admin-only operations: user management, token creation, node approval
- Operator operations: agent start/stop/restart, capability invocation
- Viewer operations: read-only status and list queries
- Constant-time token comparison prevents timing attacks
- Failed authentication attempts are logged

### A4: State Database Tampering

**Vector:** Direct modification of the SQLite state database to alter
agent states or inject malicious data.

**Mitigations:**
- State database written with restrictive file permissions (0600)
- Write-ahead logging (WAL) mode with immediate transaction locking
- DB-before-memory write ordering prevents inconsistent state
- Schema versioning prevents downgrade attacks
- Input validation on all state mutations

### A5: Join Token Theft

**Vector:** An attacker obtains a valid join token and registers a
rogue node.

**Mitigations:**
- Join tokens are SHA-256 hashed in storage (never stored in plaintext)
- Tokens support MaxUses to limit registration count
- Token revocation available via hivectl
- Node approval required before a joined node becomes active
- Rate limiting on join requests prevents brute force

### A6: Man-in-the-Middle on Cluster Communication

**Vector:** An attacker intercepts inter-node cluster traffic.

**Mitigations:**
- TLS required for all cluster communication with SNI validation
- Hardened cipher suite (TLS 1.2 minimum, no weak ciphers)
- Certificate hostname validation enforced
- Federation peer allowlist (PeerURLs) prevents unauthorized peers
- Rate-limited peer announcements prevent amplification

### A7: Denial of Service

**Vector:** Resource exhaustion through excessive requests or large payloads.

**Mitigations:**
- NATS MaxConn and MaxSubs limits prevent connection exhaustion
- JetStream resource limits prevent storage exhaustion
- Rate limiting on control handlers and node joins
- Payload size limits on all message types
- Bounded cardinality on Prometheus metrics labels
- Log entry size limit (64KB) prevents log flooding
- Maximum follower limits prevent WebSocket resource exhaustion
- Scheduler resource accounting prevents memory/CPU overcommit

### A8: Supply Chain Attacks on Firmware

**Vector:** Malicious firmware pushed to Tier 3 devices via OTA.

**Mitigations:**
- Ed25519 firmware signing required for all OTA updates
- Signature verification on device before applying update
- Streaming OTA transfer (no full binary buffered in memory)
- Build provenance tracking in firmware metadata

### A9: Dashboard and API Attacks

**Vector:** XSS, CSRF, or unauthorized access via the dashboard.

**Mitigations:**
- Strict Content Security Policy (no unsafe-inline)
- CORS validation (wildcard origins rejected)
- WebSocket origin validation
- Per-user RBAC on all API endpoints
- MaxHeaderBytes limit prevents header-based attacks
- RBAC filtering on all query responses (users only see their scope)
- JSON marshaling before WriteHeader prevents partial responses

### A10: Log Injection

**Vector:** Malicious agents publish crafted log entries to inject
false information or exploit log viewers.

**Mitigations:**
- Log level whitelist validation
- Control character stripping from log messages
- Log entry size limit (64KB)
- Agent ID validation on all log entries
- Bounded follower channels prevent memory exhaustion

## Security Features Summary

| Feature | Implementation |
|---------|---------------|
| Agent Isolation | Firecracker microVMs (Tier 1), process isolation (Tier 2) |
| Transport Encryption | TLS 1.2+ on NATS, cluster, MQTT, sidecar HTTP |
| Authentication | NATS token auth, RBAC tokens, join tokens (SHA-256) |
| Authorization | Three-tier RBAC (admin/operator/viewer) |
| Network Isolation | nftables rules per VM, no default external access |
| Communication | vsock (VM-to-host), NATS (inter-agent), MQTT bridge (Tier 3) |
| Firmware Security | Ed25519 signing and verification for OTA |
| Input Validation | Subject injection prevention, payload size limits, path traversal checks |
| Audit Logging | Failed auth logging, operation logging |
| Resource Limits | Connection limits, rate limiting, bounded metrics cardinality |
| State Protection | File permissions, WAL mode, schema versioning, crash-safe writes |
