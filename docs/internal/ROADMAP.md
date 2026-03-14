# Hive Implementation Roadmap

Detailed step-by-step plan with verifiable goals. Each step has a concrete "done when" criteria that can be tested without subjective judgment.

---

## Phase 1: The Demo (Weeks 1-4)

### Step 1.1: Sidecar invoke-remote endpoint
**What:** Add `POST /capabilities/{name}/invoke-remote` to the sidecar HTTP API. This endpoint calls `router.Invoke(targetAgentID, capName, inputs, timeout)` over NATS, enabling one agent to invoke another agent's capability via HTTP.

**Files:**
- `internal/sidecar/health.go` — add route + handler
- `internal/sidecar/sidecar.go` — wire router reference if not already available

**Done when:**
- [ ] `curl -X POST http://localhost:9100/capabilities/review-code/invoke-remote -d '{"target": "agent-b", "inputs": {"file_path": "/tmp/test.go"}}' -H "Authorization: Bearer $TOKEN"` returns a capability response envelope
- [ ] Request without `target` field returns 400
- [ ] Request to offline agent returns error with code `AGENT_OFFLINE`
- [ ] Unit test covers success, timeout, and error paths

---

### Step 1.2: Process backend callback URL support
**What:** Extend the process backend to pass `HIVE_CALLBACK_PORT` and `HIVE_SIDECAR_URL` environment variables to agent processes. Add `HIVE_AGENT_CALLBACK_URL` support so the sidecar knows where to forward capability invocations.

**Files:**
- `internal/backend/process/backend.go` — add env vars to `exec.Command`
- `internal/sidecar/sidecar.go` — read `HIVE_AGENT_CALLBACK_URL`, implement `executeCapabilityLocally()` to HTTP POST to agent process

**Done when:**
- [ ] Agent process receives `HIVE_AGENT_ID`, `HIVE_TEAM_ID`, `HIVE_SIDECAR_URL`, `HIVE_CALLBACK_PORT` in its environment
- [ ] When a capability invocation arrives via NATS, the sidecar forwards it to `http://localhost:{CALLBACK_PORT}/handle/{capability}` on the agent process
- [ ] Agent process returning `{"outputs": {...}}` with status 200 results in a successful capability response
- [ ] Agent process returning status 500 results in a `CAPABILITY_FAILED` error response
- [ ] Unit test with a mock HTTP agent process

---

### Step 1.3: CI pipeline agent scripts
**What:** Create the three demo agent entrypoint scripts that handle capability invocations via HTTP callback.

**Files (new):**
- `internal/templates/ci-pipeline/agents/code-reviewer/entrypoint.sh` — starts HTTP server on `$HIVE_CALLBACK_PORT`, handles `POST /handle/review-code` by calling Claude API, returns structured review JSON
- `internal/templates/ci-pipeline/agents/test-runner/entrypoint.sh` — handles `POST /handle/run-tests` by running the test command as a subprocess, parses output, returns structured results
- `internal/templates/ci-pipeline/agents/security-scanner/entrypoint.sh` — handles `POST /handle/scan-security` by calling Claude API, returns vulnerability JSON

**Done when:**
- [ ] Each entrypoint.sh is executable and starts an HTTP server on `$HIVE_CALLBACK_PORT`
- [ ] `code-reviewer` responds to `POST /handle/review-code` with `{"outputs": {"review": "...", "severity": "info|warning|critical", "findings_count": N}}`
- [ ] `test-runner` responds to `POST /handle/run-tests` — actually executes the provided test command and parses results
- [ ] `security-scanner` responds to `POST /handle/scan-security` with `{"outputs": {"vulnerabilities": "[...]", "risk_level": "...", "findings_count": N}}`
- [ ] Each script works without LLM keys (test-runner always, code-reviewer/security-scanner return mock results when `ANTHROPIC_API_KEY` is unset)

---

### Step 1.4: CI pipeline manifests and team config
**What:** Create the manifest YAML files and team config for the ci-pipeline template.

**Files (new):**
- `internal/templates/ci-pipeline/cluster.yaml`
- `internal/templates/ci-pipeline/agents/code-reviewer/manifest.yaml`
- `internal/templates/ci-pipeline/agents/test-runner/manifest.yaml`
- `internal/templates/ci-pipeline/agents/security-scanner/manifest.yaml`
- `internal/templates/ci-pipeline/teams/ci-pipeline.yaml`
- `internal/templates/ci-pipeline/README.md`

**Done when:**
- [ ] `hivectl validate --cluster-root internal/templates/ci-pipeline/` passes with no errors
- [ ] All three agents have correct capability definitions matching the entrypoint scripts
- [ ] Team manifest sets `code-reviewer` as lead
- [ ] Manifests use `tier: native` and `runtime.type: custom`

---

### Step 1.5: hivectl init --template
**What:** Extend `hivectl init` to accept `--template NAME` which copies a pre-built template directory into the target path instead of running the interactive wizard.

**Files:**
- `cmd/hivectl/init.go` — add `--template` flag, template copy logic
- `internal/templates/embed.go` — embed ci-pipeline and research-team directories

**Done when:**
- [ ] `hivectl init --template ci-pipeline ./demo` creates `./demo/` with cluster.yaml, agents/, teams/ matching the template
- [ ] `hivectl init --template ci-pipeline ./demo` on an existing non-empty directory returns an error (no silent overwrite)
- [ ] `hivectl init --template nonexistent ./demo` returns error listing available templates
- [ ] `hivectl init --list-templates` prints available template names with one-line descriptions
- [ ] Embedded files include entrypoint.sh with executable permission preserved

---

### Step 1.6: hivectl dev
**What:** New subcommand that starts hived with process backend forced, debug logging, and human-readable output.

**Files (new):**
- `cmd/hivectl/dev.go` — implements the `dev` subcommand

**Done when:**
- [ ] `hivectl dev --cluster-root ./demo` starts hived, embedded NATS, and all agents from the manifests
- [ ] All agents use the process backend regardless of `spec.tier` setting
- [ ] Log output is human-readable text (not JSON) at debug level
- [ ] Editing a manifest file triggers agent restart (visible in logs)
- [ ] Ctrl+C (SIGINT) gracefully stops all agents and exits cleanly
- [ ] Invalid `--cluster-root` path exits with a clear error message
- [ ] Exit code is 0 on clean shutdown, 1 on error

---

### Step 1.7: hivectl trigger
**What:** New subcommand that sends a task message to a team's broadcast subject, triggering the lead agent's orchestration.

**Files (new):**
- `cmd/hivectl/trigger.go` — implements the `trigger` subcommand

**Done when:**
- [ ] `hivectl trigger --cluster-root ./demo --team ci-pipeline --payload '{"repo_path": ".", "test_command": "go test ./..."}'` publishes an envelope with type `task` to `hive.team.ci-pipeline.broadcast`
- [ ] The command connects to the embedded NATS using the cluster config
- [ ] Missing `--team` flag returns an error
- [ ] Invalid JSON payload returns an error before sending
- [ ] Command exits after publishing (fire-and-forget)

---

### Step 1.8: Lead agent orchestration logic
**What:** The code-reviewer entrypoint must handle team broadcast messages: when it receives a trigger, it invokes test-runner and security-scanner in parallel via the sidecar's invoke-remote endpoint, then aggregates results.

**Files:**
- `internal/templates/ci-pipeline/agents/code-reviewer/entrypoint.sh` — add broadcast listener/orchestration logic (or replace with a small Go/Python program for reliability)

**Done when:**
- [ ] When a team broadcast arrives, the lead agent calls `POST /capabilities/run-tests/invoke-remote` and `POST /capabilities/scan-security/invoke-remote` on the sidecar
- [ ] Both invocations run in parallel (wall time < sum of individual times)
- [ ] Results are aggregated into a single JSON pipeline report printed to stdout
- [ ] Pipeline report includes: review findings, test results, security findings, overall pass/fail, total duration
- [ ] If one agent times out, the report includes partial results with the error noted

---

### Step 1.9: End-to-end demo integration test
**What:** Automated test that validates the full demo flow.

**Files (new):**
- `test/e2e/demo_test.go` (or extend existing e2e test file)

**Done when:**
- [ ] Test starts `hivectl dev` with the ci-pipeline template
- [ ] Waits for all three agents to report healthy heartbeats
- [ ] Sends trigger via `hivectl trigger`
- [ ] Receives pipeline result within 60 seconds
- [ ] Pipeline result contains non-empty review, test results, and security findings
- [ ] All existing e2e tests still pass
- [ ] Test passes on macOS (Apple Silicon) without KVM
- [ ] Test runs in CI (GitHub Actions)

---

### Step 1.10: README rewrite
**What:** Replace the current README quickstart with the 5-line demo flow.

**Files:**
- `README.md`

**Done when:**
- [ ] First section answers: what is Hive, why should I care, how do I try it
- [ ] Quickstart is exactly: `git clone`, `make build`, `hivectl init --template`, set API key, `hivectl dev`
- [ ] Time from clone to running demo is under 5 minutes on a fresh machine with Go installed
- [ ] README includes a "What just happened?" section explaining the three agents and their interaction

---

## Phase 2: Developer Experience (Weeks 3-8)

### Step 2.1: Python SDK
**What:** Single-file Python SDK (`sdk/python/hive_sdk.py`) that wraps the sidecar HTTP API with a decorator-based capability registration pattern.

**Files (new):**
- `sdk/python/hive_sdk.py`
- `sdk/python/setup.py` or `sdk/python/pyproject.toml`
- `sdk/python/test_hive_sdk.py`
- `sdk/python/README.md`

**Done when:**
- [ ] `from hive_sdk import HiveAgent` works after `pip install ./sdk/python`
- [ ] `@agent.capability("name")` decorator registers a capability handler
- [ ] `agent.run()` starts HTTP server on `HIVE_CALLBACK_PORT`, blocks until SIGTERM
- [ ] SDK reads `HIVE_AGENT_ID`, `HIVE_TEAM_ID`, `HIVE_SIDECAR_URL` from environment automatically
- [ ] Handler functions receive keyword arguments matching capability input names
- [ ] Return dict is wrapped as `{"outputs": {...}}` automatically
- [ ] Raised exceptions are caught and returned as `{"error": {"code": "CAPABILITY_FAILED", "message": "..."}}`
- [ ] Zero external dependencies (stdlib only: `http.server`, `json`, `signal`, `os`)
- [ ] Unit tests pass

---

### Step 2.2: Go SDK
**What:** Go SDK package at `sdk/go/hive/` mirroring the Python SDK's ergonomics.

**Files (new):**
- `sdk/go/hive/agent.go`
- `sdk/go/hive/agent_test.go`

**Done when:**
- [ ] `agent := hive.NewAgent()` reads config from environment
- [ ] `agent.HandleCapability("name", func(inputs map[string]any) (map[string]any, error))` registers a handler
- [ ] `agent.Run(ctx)` starts HTTP server and blocks until context cancellation
- [ ] Graceful shutdown on SIGTERM
- [ ] Unit tests pass

---

### Step 2.3: TypeScript SDK
**What:** Single-file TypeScript SDK using Node.js stdlib only.

**Files (new):**
- `sdk/typescript/src/index.ts`
- `sdk/typescript/package.json`
- `sdk/typescript/tsconfig.json`
- `sdk/typescript/test/agent.test.ts`

**Done when:**
- [ ] `import { HiveAgent } from '@hive/sdk'` works
- [ ] `agent.capability("name", async (inputs) => ({...}))` registers handler
- [ ] `agent.run()` starts HTTP server on callback port
- [ ] Zero runtime dependencies (Node.js `http` module only)
- [ ] Tests pass

---

### Step 2.4: OpenClaw runtime backend
**What:** When `runtime.type: openclaw` is set, the process backend generates `openclaw.json`, copies workspace files, and starts the OpenClaw binary.

**Files:**
- `internal/backend/process/backend.go` — add OpenClaw startup path
- `internal/backend/process/openclaw.go` (new) — workspace generation, config generation, binary detection

**Done when:**
- [ ] Agent with `runtime.type: openclaw` starts OpenClaw binary (found in PATH)
- [ ] `openclaw.json` is generated from manifest model config at `.state/agents/{id}/workspace/openclaw.json`
- [ ] SOUL.md, USER.md, IDENTITY.md, skills/ are copied from agent directory to workspace
- [ ] OpenClaw gateway port is unique per agent (auto-assigned from range or from manifest config)
- [ ] If `openclaw` binary not found, error message includes install instructions
- [ ] Sidecar bridges capability invocations to OpenClaw gateway HTTP calls
- [ ] Unit test with mock OpenClaw binary (shell script)

---

### Step 2.5: Cross-team capability discovery endpoint
**What:** Add `GET /team/capabilities` to the sidecar that queries the control plane for all capabilities across teams.

**Files:**
- `internal/sidecar/health.go` — add route
- `internal/sidecar/sidecar.go` — implement NATS request to `hive.ctl.capabilities.list`

**Done when:**
- [ ] `GET /team/capabilities` returns JSON array of all registered capabilities with agent ID, team ID, capability name, and description
- [ ] Response includes capabilities from other teams (not just the calling agent's team)
- [ ] Control plane handler for `hive.ctl.capabilities.list` responds with full registry
- [ ] Integration test with two agents on different teams

---

### Step 2.6: Additional templates (research-team, content-pipeline, data-processor, monitor)
**What:** Four more templates with working agent scripts and valid manifests.

**Files (new):**
- `internal/templates/research-team/` — researcher + synthesizer agents
- `internal/templates/content-pipeline/` — drafter + editor + fact-checker agents
- `internal/templates/data-processor/` — ingestor + transformer + validator agents
- `internal/templates/monitor/` — watcher + alerter agents

**Done when:**
- [ ] `hivectl validate` passes for each template
- [ ] `hivectl init --template NAME ./test-dir` works for all five templates
- [ ] Each template has a README explaining purpose and customization
- [ ] Each agent entrypoint starts, registers capabilities, and responds to invocations
- [ ] `hivectl init --list-templates` shows all five with descriptions

---

### Step 2.7: SDK integration test
**What:** End-to-end test using a Python SDK agent in a Hive cluster.

**Files (new):**
- `test/e2e/sdk_test.go`
- `test/e2e/testdata/python-agent/agent.py` — minimal Python agent using hive_sdk

**Done when:**
- [ ] Test starts a cluster with one Python SDK agent
- [ ] Agent registers a capability at startup
- [ ] Capability invocation via NATS returns correct response
- [ ] Agent shuts down cleanly on SIGTERM
- [ ] Cross-team capability invocation works between a Go agent and a Python agent

---

## Phase 3: Production Security (Weeks 6-12)

### Step 3.1: Rootfs image build pipeline
**What:** GitHub Actions workflow that builds base and OpenClaw rootfs images for amd64 and arm64, uploads as release assets.

**Files:**
- `.github/workflows/rootfs.yml` (new)
- `rootfs/nixos/` — update Nix expressions to produce both variants

**Done when:**
- [ ] `nix build` produces `hive-rootfs-amd64.ext4.gz` (< 50MB) and `hive-rootfs-arm64.ext4.gz`
- [ ] OpenClaw variant includes Node.js 20 and OpenClaw dist (< 150MB compressed)
- [ ] hive-sidecar binary is at `/usr/local/bin/hive-sidecar` in the image
- [ ] GitHub Actions builds on release tags and uploads artifacts
- [ ] Images boot in Firecracker and sidecar starts successfully

---

### Step 3.2: Automatic rootfs download
**What:** hived downloads the appropriate rootfs image from GitHub Releases on first use if not cached locally.

**Files:**
- `internal/vm/manager.go` or new `internal/vm/images.go`

**Done when:**
- [ ] `hived` starting a VM-tier agent checks `~/.hive/images/` for the rootfs
- [ ] If not found, downloads from the latest GitHub Release matching the hived version
- [ ] Download shows progress bar and validates checksum (SHA-256)
- [ ] Subsequent starts use the cached image (no re-download)
- [ ] `--rootfs-path` flag overrides auto-download for air-gapped environments
- [ ] Clear error message if download fails (network, disk space, etc.)

---

### Step 3.3: --force-process-backend flag
**What:** hived flag that forces all agents to use the process backend regardless of tier config. Used by `hivectl dev`.

**Files:**
- `cmd/hived/main.go` — add flag
- `internal/reconciler/` — pass flag to backend selection logic

**Done when:**
- [ ] `hived --force-process-backend --cluster-root ./demo` runs `tier: vm` agents via process backend
- [ ] `hivectl dev` passes this flag automatically
- [ ] Warning log emitted: "Running VM-tier agent {ID} via process backend (--force-process-backend)"
- [ ] Without the flag, automatic fallback occurs when `/dev/kvm` is absent (with warning)

---

### Step 3.4: Network policy enforcement in VM
**What:** Implement `egress: none`, `egress: restricted`, and `egress: full` at the Firecracker VM level.

**Files:**
- `internal/vm/manager.go` — pass network config to VM
- `rootfs/nixos/` — init script for iptables + dnsmasq configuration
- New: `rootfs/scripts/S10-network` — init.d script for network policy

**Done when:**
- [ ] `egress: none` — VM has no network device, only vsock. Agent cannot reach any IP. Verified by e2e test.
- [ ] `egress: restricted` — VM can resolve and reach only domains in `egress_allowlist`. DNS for other domains returns NXDOMAIN. Verified by e2e test with `curl` inside VM.
- [ ] `egress: full` — VM has full internet access via NAT. Verified by e2e test.
- [ ] Sidecar NATS communication works in all three modes (via vsock for `none`, TCP for others)

---

### Step 3.5: Resource limit enforcement
**What:** Map manifest `spec.resources` to Firecracker VM configuration.

**Files:**
- `internal/vm/manager.go` — set `mem_size_mib`, `vcpu_count`, disk size from manifest

**Done when:**
- [ ] Agent with `memory: 256Mi` gets a 256 MiB VM (Firecracker `mem_size_mib: 256`)
- [ ] Agent with `vcpus: 2` gets 2 vCPUs
- [ ] Agent exceeding memory limit is OOM-killed inside VM
- [ ] Health monitor detects the crash and restarts per restart policy
- [ ] Disk quota enforced: writes beyond `spec.resources.disk` fail with ENOSPC

---

### Step 3.6: Shared volumes via virtiofs
**What:** Team-level shared directories mounted into agent VMs.

**Files:**
- `internal/config/` — parse `shared_volumes` from team manifest
- `internal/vm/manager.go` — configure virtiofs device per volume

**Done when:**
- [ ] Team manifest with `shared_volumes: [{name: "repo", host_path: "/tmp/repo"}]` is valid
- [ ] Agent manifest referencing `volumes: [{name: "repo", mount_path: "/workspace/repo", access: "rw"}]` mounts the shared dir
- [ ] File written by agent A in the shared volume is readable by agent B
- [ ] Read-only access mode prevents writes (returns EROFS)
- [ ] Integration test with two VM agents sharing a directory

---

### Step 3.7: Firecracker end-to-end test
**What:** Full lifecycle test: VM boot, sidecar connect, capability registration, invocation, response.

**Files:**
- `test/e2e/firecracker_test.go` (new or extend existing)

**Done when:**
- [ ] Test requires Linux with KVM (skipped on macOS/no-KVM)
- [ ] Agent with `tier: vm` boots in Firecracker, sidecar connects to NATS, registers capabilities
- [ ] Capability invocation returns correct response
- [ ] Total time from agent start to first successful capability response < 3 seconds
- [ ] Agent with `egress: none` cannot reach external IPs (verified in test)
- [ ] Test tears down VM cleanly (no leaked processes, CIDs, or tap devices)

---

## Phase 4: Hive Cloud (Weeks 10-18)

### Step 4.1: hive-cloud binary scaffold
**What:** New binary at `cmd/hive-cloud/` that embeds hived and adds an HTTP API layer.

**Files (new):**
- `cmd/hive-cloud/main.go`
- `internal/cloud/server.go` — HTTP server, routing, middleware
- `internal/cloud/auth.go` — tenant authentication (Bearer token)

**Done when:**
- [ ] `hive-cloud` binary builds and starts
- [ ] Embeds full hived functionality (NATS, reconciler, scheduler, etc.)
- [ ] HTTP API listens on configurable port (default 8080)
- [ ] All endpoints require `Authorization: Bearer <token>` (except healthz)
- [ ] `GET /healthz` returns 200
- [ ] Invalid/missing token returns 401

---

### Step 4.2: Deployment API
**What:** REST endpoints for managing deployments.

**Files:**
- `internal/cloud/deployments.go` (new)
- `internal/cloud/store.go` (new) — deployment state persistence

**Done when:**
- [ ] `POST /api/v1/deployments` with tarball body creates a deployment, returns `{"deployment_id": "dep-xxx", "status": "validating"}`
- [ ] `POST /api/v1/deployments` with `{"git_url": "...", "git_ref": "main"}` clones and deploys
- [ ] `GET /api/v1/deployments/{id}` returns deployment status and agent health
- [ ] `DELETE /api/v1/deployments/{id}` tears down agents and returns 202
- [ ] `POST /api/v1/deployments/{id}/trigger` sends team broadcast, returns run ID
- [ ] `GET /api/v1/deployments/{id}/runs/{run_id}` returns run status and result
- [ ] Invalid manifests in tarball return 400 with validation errors
- [ ] Integration test covers create, status, trigger, and delete lifecycle

---

### Step 4.3: Multi-tenancy — NATS account isolation
**What:** Each tenant gets a NATS account with isolated subject namespace.

**Files:**
- `internal/cloud/tenants.go` (new)
- `internal/nats/server.go` — multi-account configuration

**Done when:**
- [ ] Tenant provisioning creates a NATS account with unique credentials
- [ ] Tenant A cannot subscribe to or publish on Tenant B's subjects
- [ ] Each tenant's agents publish to `{tenant-prefix}.hive.health.{agent}` etc.
- [ ] Integration test: two tenants, verify message isolation

---

### Step 4.4: Multi-tenancy — state isolation
**What:** Per-tenant SQLite databases.

**Files:**
- `internal/cloud/tenants.go` — tenant lifecycle
- `internal/state/store.go` — per-tenant store instantiation

**Done when:**
- [ ] Each tenant's state is at `.state/tenants/{tenant_id}/state.db`
- [ ] Tenant A's API calls never return Tenant B's agents or capabilities
- [ ] Tenant deletion cleans up the database file
- [ ] Integration test: two tenants with identically-named agents, no cross-contamination

---

### Step 4.5: Multi-tenancy — compute quotas
**What:** Per-tenant resource limits enforced at the scheduler level.

**Files:**
- `internal/cloud/tenants.go` — quota definitions
- `internal/scheduler/scheduler.go` — quota enforcement

**Done when:**
- [ ] Tenant config specifies `max_concurrent_vms`, `max_total_memory_mb`, `max_total_vcpus`
- [ ] Deployment exceeding quota returns HTTP 429 with `Retry-After` header
- [ ] Quota check happens before VM creation (no partial deployments that exceed quota)
- [ ] Integration test: tenant at quota limit, new deployment rejected

---

### Step 4.6: Autoscaler
**What:** Automatic scale-up on queue depth and scale-to-zero on idle.

**Files (new):**
- `internal/autoscaler/autoscaler.go`
- `internal/autoscaler/autoscaler_test.go`

**Done when:**
- [ ] Pending NATS messages > 0 for > 5 seconds triggers agent replica creation (up to `maxReplicas`)
- [ ] No capability invocations for configurable timeout (default 10 min) triggers VM stop
- [ ] Scaling decisions run on a 10-second loop
- [ ] Scale-up respects tenant compute quotas
- [ ] Integration test: send burst of requests, verify replicas increase, wait idle timeout, verify scale to zero

---

### Step 4.7: VM pool for fast cold start
**What:** Maintain a pool of pre-booted Firecracker VMs for sub-second agent starts.

**Files (new):**
- `internal/autoscaler/pool.go`

**Done when:**
- [ ] Pool maintains N pre-booted VMs (configurable, default 5)
- [ ] Assignment from pool: workspace injected, sidecar configured, agent started in < 500ms
- [ ] Pool replenishes automatically after assignment
- [ ] Pool VMs are cleaned up on hive-cloud shutdown
- [ ] Benchmark test: cold start from pool < 500ms (p50)

---

### Step 4.8: hivectl deploy
**What:** CLI command to push manifests to a remote Hive Cloud instance.

**Files (new):**
- `cmd/hivectl/deploy.go`

**Done when:**
- [ ] `hivectl deploy --cluster-root .hive/ --target https://cloud.hive.dev --token $TOKEN` creates a tarball of cluster root, POSTs to deployment API
- [ ] Waits for deployment to reach `running` status (timeout 120s, configurable)
- [ ] Prints agent status table on success
- [ ] Prints validation errors on failure
- [ ] `--dry-run` validates locally without uploading

---

### Step 4.9: GitHub Action
**What:** Publishable GitHub Action for CI/CD integration.

**Files (new):**
- `.github/actions/hive/action.yml`
- `.github/actions/hive/entrypoint.sh`

**Done when:**
- [ ] `uses: brmurrell3/hive-action@v1` works in a GitHub Actions workflow
- [ ] `command: validate` — validates manifests, exits 0/1, writes summary to `$GITHUB_STEP_SUMMARY`
- [ ] `command: deploy` — pushes to Hive Cloud, waits for running status
- [ ] `command: trigger` — sends trigger, waits for run completion
- [ ] Downloads hivectl from GitHub Releases (with caching via `actions/cache`)
- [ ] Integration test in a separate test repository

---

### Step 4.10: Enterprise license validation
**What:** Offline license check via signed JWT.

**Files (new):**
- `internal/license/license.go`
- `internal/license/license_test.go`

**Done when:**
- [ ] `hived --license-file /path/to/license.jwt` validates the license at startup
- [ ] License JWT contains: tenant name, expiry date, max nodes, max agents
- [ ] Signed with Ed25519 key pair. Public key embedded in binary.
- [ ] Expired license: hived starts in read-only mode (existing agents run, no new deployments)
- [ ] Invalid/missing license: hived starts in open-source mode (no cloud features)
- [ ] No network calls during validation
- [ ] Unit tests cover valid, expired, tampered, and missing license scenarios

---

### Step 4.11: Audit logging
**What:** Append-only JSON-lines audit log for all control plane operations.

**Files (new):**
- `internal/audit/log.go`
- `internal/audit/log_test.go`

**Done when:**
- [ ] All agent lifecycle operations (start, stop, destroy, restart) are logged
- [ ] All deployment operations (create, delete, trigger) are logged
- [ ] All config changes (manifest update, scale change) are logged
- [ ] Log file at `{cluster-root}/.state/audit.log`, opened with `O_APPEND | O_WRONLY`
- [ ] Each entry is a JSON line with: `timestamp`, `actor`, `action`, `resource`, `outcome`
- [ ] Write latency < 1ms p99 (verified by benchmark test)
- [ ] Log file cannot be truncated while hived holds the file descriptor

---

## Dependency Graph

```
Step 1.1 (invoke-remote) ──┐
Step 1.2 (callback URL)  ──┤
                            ├──→ Step 1.3 (agent scripts) ──→ Step 1.4 (manifests) ──→ Step 1.5 (init --template)
                            │                                                            │
                            │    Step 1.6 (hivectl dev) ←────────────────────────────────┘
                            │    Step 1.7 (hivectl trigger)
                            │         │
                            └────→ Step 1.8 (orchestration) ──→ Step 1.9 (e2e test) ──→ Step 1.10 (README)
                                                                       │
                ┌──────────────────────────────────────────────────────┘
                │
                ├──→ Step 2.1 (Python SDK) ──┐
                ├──→ Step 2.2 (Go SDK)       ├──→ Step 2.7 (SDK e2e test)
                ├──→ Step 2.3 (TS SDK)     ──┘
                ├──→ Step 2.4 (OpenClaw runtime)
                ├──→ Step 2.5 (cross-team discovery)
                └──→ Step 2.6 (templates)

Step 3.1 (rootfs build) ──→ Step 3.2 (auto-download) ──→ Step 3.7 (Firecracker e2e)
Step 3.3 (force-process)                                        ↑
Step 3.4 (network policy) ─────────────────────────────────────┤
Step 3.5 (resource limits) ────────────────────────────────────┤
Step 3.6 (shared volumes) ─────────────────────────────────────┘

Step 4.1 (hive-cloud scaffold) ──→ Step 4.2 (deployment API) ──→ Step 4.8 (hivectl deploy)
                                    │
                                    ├──→ Step 4.3 (NATS isolation) ──┐
                                    ├──→ Step 4.4 (state isolation)  ├──→ Step 4.5 (compute quotas)
                                    │                               ─┘
                                    ├──→ Step 4.6 (autoscaler) ──→ Step 4.7 (VM pool)
                                    ├──→ Step 4.9 (GitHub Action)
                                    ├──→ Step 4.10 (license)
                                    └──→ Step 4.11 (audit log)
```

## Parallelization Opportunities

**Within Phase 1:**
- Steps 1.1 + 1.2 can be built in parallel (independent sidecar/backend changes)
- Steps 1.5 + 1.6 + 1.7 can be built in parallel (independent hivectl subcommands)

**Within Phase 2:**
- Steps 2.1 + 2.2 + 2.3 can be built in parallel (independent SDKs)
- Steps 2.4 + 2.5 + 2.6 can be built in parallel (independent features)

**Phase overlap:**
- Phase 2 can start at week 3 (after Step 1.9 validates the core flow)
- Phase 3 rootfs work (3.1-3.2) can start at week 4 (independent of Phase 2)
- Phase 4 scaffold (4.1) can start at week 8 (after Phase 2 stabilizes)

**Within Phase 4:**
- Steps 4.3 + 4.4 can be built in parallel (independent isolation layers)
- Steps 4.9 + 4.10 + 4.11 can be built in parallel (independent features)
