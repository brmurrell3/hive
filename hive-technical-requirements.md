# Hive: Technical Requirements & Design

---

## What Already Exists

The Hive repository (github.com/brmurrell3/hive) has ~20,000 lines of Go implementation and ~16,000 lines of tests across 20 packages. This is not a prototype -- it's a working system missing the last-mile features needed for product-market fit.

**Working today:**
- YAML manifest parsing and validation (15 rules, all tested)
- Embedded NATS message bus with JetStream persistence
- SQLite state persistence with schema migrations
- Firecracker VM lifecycle management (8-state machine, vsock, virtiofs)
- Process backend for running agents as child processes
- Sidecar with HTTP API, heartbeats, and NATS connectivity
- Capability routing with worker pools, circuit breakers, and dedup
- Reconciler (desired state vs actual state, 5-second cycle)
- Scheduler with bin-packing, resource filtering, and team co-location
- Health monitoring with exponential backoff restart
- File watcher with hot-reload on manifest changes
- RBAC (admin/operator/viewer roles)
- REST + WebSocket dashboard API
- Prometheus metrics
- End-to-end test suite (634 lines, covers full lifecycle)

**What's missing (organized by phase below):**
- No compelling demo or example agent team
- No `hivectl dev` command for local development
- No template library beyond a single generic agent
- Rootfs images aren't published -- users must build manually
- No deployment API for pushing manifests to a remote control plane
- No multi-tenancy support
- No autoscaling
- No GitHub Action for CI/CD integration

---

## Phase 1 Requirements: The Demo

**Goal:** A developer clones the repo, runs one command, and sees three AI agents collaborating on a real task.

### REQ-1.1: CI Pipeline Agent Team

Build three agents that ship with the repo as a template.

**code-reviewer agent:**
- Accepts a file path or git diff as input
- Calls an LLM (configurable, defaults to Claude) to produce a structured code review
- Returns JSON with findings and severity rating (info/warning/critical)
- Exposes one capability: `review-code`

**test-runner agent:**
- Accepts a repository path and test command
- Runs the test suite as a subprocess (no LLM needed)
- Parses stdout/stderr for pass/fail counts
- Returns JSON with test results and pass rate
- Exposes one capability: `run-tests`

**security-scanner agent:**
- Accepts a file path
- Calls an LLM to identify security vulnerabilities (injection, hardcoded secrets, auth issues)
- Returns JSON with findings and risk levels
- Exposes one capability: `scan-security`

**ci-pipeline team:**
- The code-reviewer is the lead agent
- On trigger, the lead invokes test-runner and security-scanner capabilities in parallel via the sidecar HTTP API
- Aggregates results and publishes a combined report

All agents run on the process backend (no KVM needed). Works on macOS and Linux.

### REQ-1.2: hivectl init --template

Extend the existing `hivectl init` command to accept a `--template` flag.

| Template | What it creates |
|---|---|
| `default` | Current behavior (single example agent) |
| `ci-pipeline` | Three agents + team manifest for CI/CD |
| `research-team` | Two agents (researcher + synthesizer) |

Templates are stored as embedded files in the Go binary. `hivectl init --template ci-pipeline my-pipeline` copies the template files into a new directory.

### REQ-1.3: hivectl dev

New command that starts the full local development environment in one step.

```
hivectl dev --cluster-root ./my-pipeline
```

This command:
- Validates the manifests
- Forces process backend (ignores `tier: vm` settings)
- Starts the embedded NATS server
- Starts all agents
- Enables hot-reload (edit a manifest, agent restarts automatically)
- Logs in human-readable format (not JSON) at debug level
- Runs in the foreground until Ctrl+C

### REQ-1.4: README Rewrite

Replace the current README Quick Start with:

```
git clone https://github.com/brmurrell3/hive && cd hive
make build
./bin/hivectl init --template ci-pipeline demo
export ANTHROPIC_API_KEY=sk-...
./bin/hivectl dev --cluster-root demo
```

The README must answer three questions in the first 30 seconds of reading: what is Hive, why should I care, how do I try it.

### Phase 1 Acceptance Tests

- [ ] `git clone && make build && hivectl init --template ci-pipeline demo && hivectl dev --cluster-root demo` produces three running agents with health heartbeats in under 60 seconds
- [ ] Triggering the pipeline produces a JSON result with code review, test results, and security findings
- [ ] All existing e2e tests pass
- [ ] Works on macOS Apple Silicon and Linux amd64/arm64 without KVM

---

## Phase 2 Requirements: Developer Experience

**Goal:** Developers can build and iterate on custom agent teams for their own projects.

### REQ-2.1: OpenClaw Runtime

The manifest schema already supports `runtime.type: openclaw`. Make it actually work.

When the backend starts an OpenClaw agent:
- Copy the agent directory's SOUL.md, AGENTS.md, skills/, etc. into the OpenClaw workspace path
- Generate an `openclaw.json` from the manifest's model config and capabilities
- Start the OpenClaw binary as the child process
- The sidecar translates between Hive capability invocations and OpenClaw skill calls

This lets the 150K+ OpenClaw community use Hive for team orchestration and isolation without rewriting their agents.

### REQ-2.2: Agent SDK

Thin wrapper libraries (Python, Go, TypeScript) around the sidecar HTTP API. Not required -- agents can call the HTTP API directly -- but significantly lowers the barrier for custom agents.

Python example:
```python
from hive_sdk import HiveAgent

agent = HiveAgent()  # reads config from env vars

@agent.capability("review-code")
def review_code(file_path: str, diff: str = None):
    # your logic here
    return {"review": "...", "severity": "info"}

agent.run()  # blocks, handles heartbeats automatically
```

The SDK must handle: reading HIVE_AGENT_ID, HIVE_NATS_URL, HIVE_SIDECAR_URL from environment; registering capabilities with the sidecar at startup; heartbeat responses; graceful shutdown on SIGTERM.

### REQ-2.3: Template Library

Five templates that scaffold working agent teams:

| Template | Agents | Use Case |
|---|---|---|
| ci-pipeline | code-reviewer, test-runner, security-scanner | Code review and testing |
| research-team | researcher, synthesizer | Parallel research with synthesis |
| content-pipeline | drafter, editor, fact-checker | Content generation with review |
| data-processor | ingestor, transformer, validator | ETL-style data processing |
| monitor | watcher, alerter | System monitoring with escalation |

Each template must include working agent scripts, valid manifests, and a README explaining how to customize it.

### REQ-2.4: Cross-Team Capability Discovery

Currently agents can only invoke capabilities within their team. Add:
- Agents can discover capabilities from other teams (subject to RBAC)
- The capability router resolves cross-team invocations by routing through the NATS bus
- RBAC controls which teams can invoke which capabilities

### Phase 2 Acceptance Tests

- [ ] An OpenClaw agent starts on the process backend with workspace files injected
- [ ] A Python agent using the SDK registers a capability and responds to invocations
- [ ] All five templates pass `hivectl validate`
- [ ] Cross-team capability invocation works in e2e tests

---

## Phase 3 Requirements: Production Security

**Goal:** Each agent runs in its own Firecracker microVM with enforced resource and network boundaries. Zero manual setup.

### REQ-3.1: Pre-Built VM Images

Publish rootfs images as GitHub Release assets:
- `hive-rootfs-amd64.ext4.gz` -- base image with sidecar
- `hive-rootfs-arm64.ext4.gz` -- same for ARM
- `hive-rootfs-openclaw-amd64.ext4.gz` -- includes Node.js + OpenClaw

GitHub Actions workflow builds images on each release tag.

`hived` downloads the appropriate image on first use if not present. Stored in `~/.hive/images/`. No manual `build-rootfs.sh` required.

### REQ-3.2: Same Config, Different Backend

The same manifest runs on process backend (dev) and Firecracker (prod) with zero changes.

- `hivectl dev` always uses the process backend, even for `tier: vm` agents
- `hived` on a Linux host with KVM uses Firecracker for `tier: vm` agents
- The sidecar HTTP API, NATS subjects, and capability routing behave identically on both backends
- Agents cannot detect whether they're in a VM or a process

### REQ-3.3: Network Policies

Enforce the existing `spec.network.egress` field at the VM level:

| Setting | What it does |
|---|---|
| `egress: none` | No network interface. Agent can only communicate via the NATS bus (vsock). |
| `egress: restricted` | Agent can only reach domains listed in `egress_allowlist`. DNS queries for unlisted domains return NXDOMAIN. |
| `egress: full` | Standard internet access via NAT. |

### REQ-3.4: Resource Limits

Enforce the existing `spec.resources` fields at the hypervisor level:

| Resource | Enforcement |
|---|---|
| `memory` | Firecracker `mem_size_mib`. Agent OOM'd inside VM triggers health monitor restart. |
| `vcpus` | Firecracker `vcpu_count`. CPU-bound agents throttled by hypervisor. |
| `disk` | ext4 filesystem sized to `spec.resources.disk`. Writes beyond quota fail with ENOSPC. |

### REQ-3.5: Shared Volumes

Agents on the same team can share a directory (e.g., a git checkout). Implemented via virtiofs:
- Team manifest defines `shared_volumes` with host path
- Agent manifest references volumes by name with mount path and access mode (read-only or read-write)
- Firecracker backend configures virtiofs device per volume

### Phase 3 Acceptance Tests

- [ ] `hived` starts a Firecracker VM for a vm-tier agent. VM boots, sidecar connects, agent registers capabilities, responds to invocations -- all within 2 seconds
- [ ] Agent with `egress: none` cannot reach any IP. Agent with `egress: restricted` can only reach whitelisted domains. Validated by e2e test.
- [ ] Agent exceeding memory ceiling is killed, detected by health monitor, restarted per policy
- [ ] Two agents share a file via team shared volume
- [ ] Pre-built images download automatically and work without manual building

---

## Phase 4 Requirements: Hive Cloud

**Goal:** Customers push config files. Hive handles everything else.

### REQ-4.1: Deployment API

New REST API on the control plane:

| Endpoint | What it does |
|---|---|
| `POST /api/v1/deployments` | Upload manifest bundle (tarball or git reference). Returns deployment ID. |
| `GET /api/v1/deployments/{id}` | Get deployment status and agent health. |
| `DELETE /api/v1/deployments/{id}` | Tear down all agents and clean up. |
| `POST /api/v1/deployments/{id}/trigger` | Trigger a pipeline run. Returns run ID. |

New CLI command: `hivectl deploy --cluster-root .hive/ --target https://cloud.hive.dev`

### REQ-4.2: Multi-Tenancy

Each customer is completely isolated:

| Layer | How isolation works |
|---|---|
| Messaging | Separate NATS accounts per tenant. Tenant A cannot see Tenant B's messages. |
| State | Separate SQLite database per tenant. |
| Compute | Separate Firecracker VMs. Resource quotas per tenant (max VMs, max memory, max vCPUs). |

### REQ-4.3: Autoscaling

| Behavior | How it works |
|---|---|
| Scale up | Pipeline trigger increases queue depth on NATS subjects. Scheduler starts new agent VMs from a pre-warmed pool. |
| Scale to zero | No capability invocations for configurable timeout (default 10 min). VMs stopped, state persisted. |
| Pre-warm | Pool of booted-but-idle VMs ready for fast assignment (~200ms vs ~2s cold start). |

### REQ-4.4: GitHub Integration

GitHub Action published to the Marketplace:

```yaml
# .github/workflows/hive.yml
on: [push, pull_request]
jobs:
  hive:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: brmurrell3/hive-action@v1
        with:
          cluster-root: .hive/
          command: validate    # on PR
          # command: deploy    # on merge
          hive-cloud-token: ${{ secrets.HIVE_TOKEN }}
```

On pull requests: validates manifests and reports errors. On merge to main: deploys the agent pipeline to Hive Cloud.

### REQ-4.5: Enterprise Self-Hosted Package

Single downloadable tarball containing:
- `hived` binary (statically compiled)
- `hivectl` binary
- Pre-built rootfs images
- All documentation

Requirements:
- Zero internet access after download (no package managers, no external dependencies)
- Offline license validation (signed JWT, checked locally at startup)
- Append-only audit log for all control plane operations (SOC 2 compliance)

### Phase 4 Acceptance Tests

- [ ] `hivectl deploy` pushes manifests to Hive Cloud API; agents running within 30 seconds
- [ ] Two tenants on the same instance cannot observe each other's data or agents
- [ ] Idle pipeline scales to zero VMs; new trigger starts agents within 5 seconds
- [ ] GitHub Action validates on PR and deploys on merge
- [ ] Enterprise tarball installs and runs on air-gapped Linux with no internet

---

## What We're Deferring

| Item | Why | When |
|---|---|---|
| Microcontroller/IoT support (Tier 3) | Market not ready for edge LLM agents | 12-24 months |
| Visual workflow builder | CLI-first product; dashboard API exists for future UI | After revenue |
| LLM proxy / model routing | Out of scope by design; agents manage their own models | Never |
| Multi-cloud (GCP, Azure) | Focus on AWS-compatible Firecracker first | After enterprise validation |
| Agent marketplace | Community feature after adoption | After Phase 4 |

---

## Dependency Chain

Phases overlap but have hard dependencies:

```
Phase 1 (Demo)
  └── Process backend e2e ✓ (exists)
  └── Template system (REQ-1.2)
  └── Dev mode (REQ-1.3)
        │
Phase 2 (DX)
  └── OpenClaw runtime (REQ-2.1) ──→ needed for Phase 3 OpenClaw rootfs
  └── SDK (REQ-2.2)
  └── Templates (REQ-2.3)
        │
Phase 3 (Isolation)
  └── Pre-built rootfs (REQ-3.1) ──→ needed for Phase 4 managed deploys
  └── Firecracker e2e validated (REQ-3.2) ──→ needed for Phase 4 production
        │
Phase 4 (Cloud)
  └── Deployment API (REQ-4.1)
  └── Multi-tenancy (REQ-4.2) ──→ needed before commercial launch
  └── GitHub Action (REQ-4.4)
```

Total timeline: ~18 weeks from Phase 1 start to first paying customer.
