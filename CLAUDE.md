# CLAUDE.md — Implementation Guide for Hive Framework

This file is the entry point for implementation sessions. Read this file first. It tells you what exists, what to build next, and when to read other files

## Project Summary

Hive is a declarative framework for orchestrating LLM agent teams across heterogeneous hardware (workstations, Raspberry Pis, microcontrollers). Agents are defined in YAML manifests and managed by a Go control plane (`hived`) that handles provisioning, communication (NATS), health management, and capability routing. The CLI (`hivectl`) provides all operator-facing commands.

The core differentiator is ease of use and frictionless deployment. Every design decision should favor simplicity for the operator. If a feature adds operator-facing complexity without proportional value, push back.

## Current State

**Active milestone:** All milestones complete (M1-M10)
**MVP target:** M5 (after which the system supports single-node, single-team, VM-based agent deployment with capability routing and auto-restart)

Update this section as milestones are completed:
- [x] M1: Project Skeleton + NATS Transport
- [x] M2: Manifest Parsing + Validation
- [x] M3: Firecracker VM Lifecycle
- [x] M4: Sidecar + Agent Runtime
- [x] M5: Capability Routing + Tool Gen + Health + MEMORY.md Hot-Reload
- [x] M6: Tier 2 Native Agents + Join Tokens + Capability Registry + MQTT Bridge
- [x] M7: Tier 3 Firmware Agents + Build/Flash Tooling
- [x] M8: Multi-Node Clustering + Scheduler + Structured State
- [x] M9: Multi-Team + Cross-Team Routing + Director + RBAC + OTA
- [x] M10: Dashboard + Metrics + Logs + NixOS Rootfs + Production Hardening

## Document Map

Only read spec files when you need them. Here's when:

| File | Read when... |
|---|---|
| `09-ROADMAP.md` | Starting any milestone. Contains deliverables, acceptance criteria, explicit exclusions, and test specs. This is the authoritative build plan. |
| `00-VISION-AND-PHASES.md` | You need to understand the "why" behind a design decision or need broader context on the product vision. Rarely needed during implementation. |
| `01-ARCHITECTURE.md` | Implementing agent execution models, tier classification, or node types. Key reference for M3, M4, M6, M7. |
| `02-SCHEMAS.md` | Implementing YAML parsing or validation. Contains exact field names, types, defaults, and validation rules. Key reference for M2 and anytime you touch manifest parsing. |
| `03-COMMUNICATION.md` | Implementing NATS messaging, subject hierarchies, or the message envelope format. Key reference for M1, M4, M5. |
| `04-CONTROL-PLANE.md` | Implementing hived internals: reconciliation, state management, VM manager, agent lifecycle. Key reference for M3, M5, M8. |
| `05-EXECUTION.md` | Implementing the sidecar, agent runtime lifecycle, or host-guest communication. Key reference for M4, M5, M6. |
| `06-FIRMWARE.md` | Implementing Tier 3 firmware SDKs, MQTT protocol, or OTA updates. Only needed for M7 and M9. |
| `07-CLI-AND-INTERACTION.md` | Implementing any `hivectl` command. Contains exact command signatures, flags, output formats, and exit codes. Referenced in every milestone. |
| `08-DEPLOYMENT.md` | Implementing NixOS modules, node discovery, join protocol, or rootfs image builds. Key reference for M6, M7, M8, M10. |

## Do NOT Build Yet

These items are explicitly deferred. If you find yourself reaching for one of these, stop and check the Deferred Complexity Register in `09-ROADMAP.md`.

**Before M5:** No hot-reload of manifests/skills/AGENTS.md (only MEMORY.md hot-reload, introduced in M5). No NixOS rootfs (use Alpine).
**Before M6:** No sidecar library mode (standalone binary only). No join tokens. No capability registry file (NATS subjects are the registry). No MQTT bridge. No node registration or hardware inventory.
**Before M7:** No firmware SDKs (C or MicroPython). No firmware build/flash tooling. No pre-built RPi images.
**Before M8:** No reconciliation polling loop (fsnotify + explicit CLI only). No scheduler (single node, direct placement). No structured `.state/` directory (single `state.json`). No external NATS (embedded only). No node drain/cordon. No hot-reload of manifests or skills (only MEMORY.md hot-reloads through M7; manifest hot-reload triggers restart starting in M8).
**Before M9:** No multi-user RBAC. No director agent. No OTA firmware updates. No cross-team capability routing. No mTLS between nodes.
**Before M10:** No web dashboard. No Prometheus metrics. No NixOS rootfs build. No log aggregation. No message flow visualization.

If a milestone's acceptance criteria do not mention a feature, that feature does not belong in that milestone.

## Project Structure

```
hive/
├── cmd/
│   ├── hived/
│   │   └── main.go              # control plane daemon
│   └── hivectl/
│       └── main.go              # CLI tool
├── internal/
│   ├── config/                  # cluster.yaml + manifest parsing
│   │   ├── loader.go            # filesystem loading
│   │   └── validate.go          # validation rules
│   ├── nats/                    # embedded NATS server management
│   │   └── server.go
│   ├── state/                   # state.json persistence
│   │   └── store.go
│   ├── vm/                      # Firecracker VM lifecycle
│   │   └── manager.go
│   ├── sidecar/                 # sidecar binary (standalone mode)
│   │   ├── main.go              # VM entry point
│   │   ├── health.go            # HTTP health + capabilities endpoints
│   │   ├── nats.go              # NATS bridge + heartbeat
│   │   └── runtime.go           # agent process management
│   ├── capability/              # capability routing (M5+)
│   │   └── router.go
│   ├── watcher/                 # fsnotify filesystem watching (M5+)
│   │   └── watcher.go
│   ├── types/                   # shared type definitions
│   │   ├── agent.go
│   │   ├── team.go
│   │   ├── cluster.go
│   │   ├── state.go
│   │   └── envelope.go          # NATS message envelope
│   └── testutil/                # test helpers
│       ├── cluster.go           # temp cluster root scaffolding
│       ├── nats.go              # test NATS server
│       └── firecracker.go       # VM test harness
├── testdata/                    # test fixtures
│   ├── valid-cluster/
│   └── invalid-manifests/
├── rootfs/                      # Alpine rootfs build scripts (M3+)
│   ├── Makefile
│   └── overlay/                 # files baked into rootfs
├── go.mod
├── go.sum
└── Makefile                     # top-level build targets
```

This structure is a starting point. Adapt it as needed but keep `cmd/` for binaries, `internal/` for library code, and `testdata/` for fixtures. Do not create packages until they're needed by the current milestone.

## Coding Conventions

**Error handling:** Return errors, don't panic. Wrap errors with context using `fmt.Errorf("doing X: %w", err)`. Every public function that can fail returns `error` as its last return value.

**Logging:** Use `log/slog` (structured logging, stdlib). Log level defaults to `info`. JSON format to stdout. Include relevant context fields (agent_id, node_id, etc.) on every log line. No `log.Fatal` except in `main()`.

**Testing:** Table-driven tests. Use `t.Helper()` in test helpers. Use build tags (`//go:build unit`, etc.) as specified in the roadmap testing strategy. Test file naming: `foo_test.go` next to `foo.go`. Integration tests that need NATS or filesystem go in the same package with the `integration` build tag.

**Naming:**
- Go packages: lowercase, single word where possible (`config`, `vm`, `state`, `nats`).
- Agent IDs, team IDs: lowercase alphanumeric with hyphens, validated by `[a-z0-9][a-z0-9-]{0,62}`.
- NATS subjects: dot-separated hierarchy as specified in `03-COMMUNICATION.md`.
- CLI flags: GNU-style long flags with `--`. No short flags except `-h` for help.

**Dependencies:** Minimize external dependencies. Prefer stdlib where reasonable. Known required dependencies:
- `github.com/nats-io/nats-server/v2` — embedded NATS server
- `github.com/nats-io/nats.go` — NATS client
- `gopkg.in/yaml.v3` — YAML parsing
- `github.com/fsnotify/fsnotify` — filesystem watching (M5+)
- `github.com/spf13/cobra` — CLI framework for hivectl

Do not add dependencies for things the stdlib handles (HTTP server, JSON, crypto, testing).

**State management (M1-M7):** Single `state.json` file at the cluster root. hived owns this file exclusively. Read on startup, write on every state change. Use a mutex to prevent concurrent writes. The file is the source of truth for runtime state (VM PIDs, health status, restart counts). Desired state comes from the YAML manifests, not from `state.json`.

**NATS message envelope:** Every message on NATS uses the envelope format defined in `03-COMMUNICATION.md`. Never publish raw payloads. Always include `id` (UUID v4), `from`, `to`, `type`, `timestamp` (RFC3339). Validate incoming envelopes before processing.

## Milestone Workflow

When starting a new milestone:

1. Read the milestone section in `09-ROADMAP.md`. Note the deliverables, acceptance criteria, and explicit exclusions.
2. Read only the spec files listed as "Key reference" for that milestone in the document map above.
3. Implement deliverables in the order listed (they're dependency-ordered).
4. Write tests as you go, matching the test specifications in the roadmap's Testing Strategy section.
5. Run the full test suite for all completed milestones (regression) before considering the milestone done.
6. Update the checklist in the "Current State" section of this file.

## Key Technical Decisions

These decisions are final. Do not revisit them during implementation.

1. **NATS is embedded as a Go library in hived (M1-M7).** Import `github.com/nats-io/nats-server/v2/server`, call `server.New()` and `server.Start()`. Do not spawn a subprocess. Do not use Docker. The transition to external NATS happens in M8.

2. **Single `state.json` (M1-M7).** Not a database, not etcd, not multiple files. One JSON file. Migration to structured `.state/` directory happens in M8.

3. **Alpine rootfs for Firecracker (M3-M9).** Do not use NixOS for the rootfs until M10. Use a minimal Alpine Linux rootfs with the sidecar binary and agent runtime baked in via a shell script or Makefile. Priority is fast iteration, not reproducibility (that comes later).

4. **Sidecar is standalone binary only (M1-M5).** Compiled as a static Linux binary, placed in the rootfs, runs as PID 1 or init-launched inside the VM. Do not build the library/goroutine mode until M6.

5. **No capability registry file (M1-M5).** Agents subscribe to their NATS capability subjects directly. The NATS subject namespace is the registry. The explicit capability registry is introduced in M6.

6. **fsnotify only, no polling (M1-M7).** Filesystem changes are detected via fsnotify. No background timer reconciliation loop until M8. For changes that fsnotify misses, the operator runs `hivectl` commands explicitly.

7. **MEMORY.md is the only hot-reloadable file (M1-M5).** All other config changes require `hivectl agents restart`. Manifest hot-reload (triggering restart via reconciliation) is introduced in M8.

## Common Pitfalls

- **Firecracker requires KVM.** The host must have `/dev/kvm`. Tests tagged `vm` will fail without it. Use the `HIVE_TEST_FIRECRACKER=mock` env var to run with a mock VM manager in environments without KVM.
- **NATS port conflicts.** Tests must use random port allocation for the embedded NATS server. Never hardcode port 4222 in tests. The `testutil.NATSServer()` helper should handle this.
- **virtio-vsock CID allocation.** Each Firecracker VM needs a unique context ID (CID) for vsock. The VM manager must track allocated CIDs and assign unique values. CID 0, 1, and 2 are reserved.
- **Rootfs copy-on-write.** Each VM needs its own rootfs copy (Firecracker doesn't support shared rootfs). The VM manager must copy the base rootfs for each new VM and clean up on destroy.
- **Agent file mounting.** Agent-specific files (AGENTS.md, SOUL.md, etc.) need to be accessible inside the VM. This is done via a secondary drive attached to Firecracker, not via 9p or network filesystem.
- **NATS connection from inside VM.** The sidecar inside the VM connects to hived's NATS server via virtio-vsock, not via TCP on a bridge network. The sidecar needs a vsock-aware NATS dialer or a vsock-to-TCP proxy.
- **State file corruption.** If hived crashes mid-write to `state.json`, the file could be corrupted. Write to a temp file first, then atomic rename. Use `os.Rename()` which is atomic on Linux when source and dest are on the same filesystem.
- **Heartbeat timing.** The default health interval is 30s with `maxFailures: 3`, meaning an agent is marked UNHEALTHY after 90s of no heartbeats. Tests that validate health transitions need to use shorter intervals (e.g., 1s interval, 3 max failures = 3s detection time) to avoid slow test suites.
