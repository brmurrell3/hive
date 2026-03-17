# Hive

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![CI](https://github.com/brmurrell3/hive/actions/workflows/ci.yml/badge.svg)](https://github.com/brmurrell3/hive/actions/workflows/ci.yml)

**Hive is a declarative orchestration framework for AI agent teams running on lightweight VMs.**
Define agents in YAML, connect them over an embedded NATS message bus, and deploy anywhere -- from a laptop to an air-gapped data center.
One Go binary, no Docker, no Python dependencies.

## Quickstart (any OS)

```bash
git clone https://github.com/brmurrell3/hive && cd hive
make build
./bin/hivectl init --template ci-pipeline my-pipeline
./bin/hivectl dev --cluster-root my-pipeline
# In a second terminal:
./bin/hivectl trigger --cluster-root my-pipeline --team ci-pipeline \
  --payload '{"file_path": "main.go", "test_command": "go test ./..."}'
```

Requires [Go 1.25+](https://go.dev/dl/). This uses the **process backend** -- no KVM or Linux required. Three AI agents start, collaborate on a CI pipeline, and print a JSON report. No API key needed (mock responses by default). Set `ANTHROPIC_API_KEY` for real LLM-powered code review and security scanning.

Clean up when done:

```bash
rm -rf my-pipeline
```

## What just happened?

Three AI agents started, each running in its own process with a dedicated sidecar for messaging:

```
                    hivectl trigger
                         |
                         v
               +---------+---------+
               |   code-reviewer   |  <-- lead agent, orchestrates the pipeline
               |  (reviews code)   |
               +----+--------+----+
                    |        |
          invoke-remote   invoke-remote
            (NATS)          (NATS)
                    |        |
            +-------+    +-------+
            |test-  |    |securi-|
            |runner |    |ty-    |
            |(runs  |    |scanner|
            | tests)|    |(scans |
            +-------+    | code) |
                         +-------+
```

1. `hivectl trigger` published a task to the team's NATS broadcast subject.
2. The **code-reviewer** (lead agent) received the broadcast and kicked off orchestration.
3. It invoked **test-runner** and **security-scanner** capabilities in parallel via NATS request-reply.
4. Each agent processed its task and returned structured JSON results.
5. The lead agent aggregated everything into a single pipeline report.

All communication goes through an embedded NATS message bus. Each agent has a sidecar that handles heartbeats, capability registration, and message routing -- agents just implement HTTP handlers.

## Supported Platforms

| Platform | Architecture | VM Isolation | Dev Experience | Notes |
|----------|-------------|-------------|----------------|-------|
| macOS | Apple Silicon (arm64) | Process backend only | Full | All CLI, orchestration, SDK development |
| macOS | Intel (x86_64) | Process backend only | Full | All CLI, orchestration, SDK development |
| Linux | x86_64 + KVM | Firecracker microVMs | Full | Per-agent kernel, memory, network isolation |
| Linux | arm64 + KVM | Firecracker microVMs | Full | Per-agent kernel, memory, network isolation |

On macOS and Linux without KVM, Hive uses the **process backend**: agents run as OS processes with sidecar messaging, giving you the full orchestration experience without VM isolation. On Linux with KVM, Hive uses **Firecracker microVMs** for production-grade per-agent isolation.

## Real Hardware (Linux + KVM + Firecracker)

For full VM isolation on Linux with KVM:

```bash
# 1. Verify KVM is available
ls /dev/kvm

# 2. Build all binaries including the sidecar
make build-linux-amd64

# 3. Download a Firecracker-compatible kernel
make download-kernel

# 4. Build the rootfs image
make rootfs

# 5. Create a cluster configured for real VMs
./bin/hivectl init my-cluster
# Edit my-cluster/cluster.yaml to set:
#   spec.vm.kernelPath: rootfs/vmlinux
#   spec.vm.rootfsPath: rootfs/rootfs.ext4

# 6. Start the control plane (no HIVE_TEST_FIRECRACKER env var)
sudo ./bin/hived --cluster-root my-cluster

# 7. Start agents (each boots a Firecracker microVM)
./bin/hivectl agents start my-agent --cluster-root my-cluster
```

See the [Operations Guide](docs/operations.md) for full prerequisites, configuration, and troubleshooting.

## Why Hive?

| | LangGraph / CrewAI | E2B | AWS AgentCore | **Hive** |
|---|---|---|---|---|
| Multi-agent orchestration | Yes | No | Yes | **Yes** |
| Per-agent VM isolation | No | Yes (sandbox only) | Yes | **Yes** |
| Single integrated platform | Orchestration only | Sandbox only | Yes, but AWS-locked | **Yes, vendor-neutral** |
| Self-hosted / air-gapped | Difficult (Python deps) | No | No | **Yes (single Go binary)** |
| Declarative config files | Python code | API calls | Console + SDK | **YAML in your repo** |
| Open source | Partially | Partially | No | **Fully (Apache 2.0)** |

**The key insight:** Nobody else ships orchestration + isolation + declarative config + self-hosted deployment in one package.

## Features

- **Declarative YAML manifests** for agents, teams, and clusters
- **Firecracker microVMs** for production isolation (per-agent kernel, memory, network)
- **Process backend** for local development (no KVM needed, works on macOS)
- **Capability routing** over NATS with request/reply invocation
- **Health monitoring** with configurable heartbeats and auto-restart
- **Hot-reload** -- edit a manifest, agents restart automatically
- **Bin-packing scheduler** with team co-location
- **RBAC** with admin, operator, and viewer roles
- **Dashboard API** with REST and WebSocket endpoints
- **Prometheus metrics** and structured log aggregation
- **Reconciliation loop** that converges actual state to desired state
- **SDKs** for Python, Go, and TypeScript (zero external dependencies)

## Agent Design

Hive agents are defined by three layers:

**Identity (SOUL.md)** — Each agent has an optional SOUL.md that defines its
role, behavioral constraints, and output expectations. This acts as the agent's
system prompt and is injected by the sidecar when the agent starts.

**Memory (MEMORY.md)** — Agents maintain persistent memory across restarts via
MEMORY.md. The workspace state model uses last-modified-wins sync, allowing
agents to accumulate knowledge over time while operators can seed or reset
memory from the cluster root.

**Capabilities** — Declared in the manifest with typed inputs and outputs.
The sidecar auto-generates LLM tool definitions from capability schemas,
enabling agents to discover and invoke each other's functions without
hard-coded integrations.

```yaml
# Example: test-runner exposes a "run-tests" capability
capabilities:
  - name: run-tests
    description: "Run test suite for a given file and return results"
    inputs:
      - name: file_path
        type: string
        description: "Path to the file under test"
      - name: test_command
        type: string
        description: "Test command to execute"
    outputs:
      - name: passed
        type: bool
        description: "Whether all tests passed"
      - name: output
        type: string
        description: "Test runner stdout/stderr"
```

When the code-reviewer agent (backed by Claude) needs test results, it sees
`run-tests` as an available tool with typed parameters. The sidecar handles
routing the invocation over NATS to the test-runner agent and returning the
structured result. See [Architecture](docs/architecture.md#agent-identity-and-memory)
for details on SOUL.md, MEMORY.md, and capability-to-tool generation.

## Model Support

Hive is model-agnostic. The cluster manifest includes a model registry, and
each agent specifies its provider and model:

```yaml
# cluster.yaml
spec:
  models:
    - name: local-llama
      provider: ollama
      endpoint: http://localhost:11434

# agent manifest
spec:
  runtime:
    type: openclaw
    model:
      provider: anthropic
      name: claude-sonnet-4-5
```

Supported providers: Anthropic, OpenAI, Ollama, Google, Mistral, Cohere.
Swap models per-agent without code changes -- useful for cost optimization
(fast model for triage, strong model for analysis) and A/B evaluation.

## Evaluation

Hive includes a built-in eval framework for measuring agent and team
performance across models and prompt strategies.

```bash
# Run the CI pipeline eval across two models
hivectl eval run \
  --eval-root evals/ci-pipeline-accuracy \
  --cluster-root my-pipeline

# Example output:
# Eval: ci-pipeline-accuracy
# Time: 2026-03-16 14:30:00 UTC
#
# Model                      Accuracy  p50 Latency  p99 Latency  Passed  Total
# -----                      --------  -----------  -----------  ------  -----
# anthropic/claude-sonnet-4-5  90%     4.2s         8.1s         9       10
# openai/gpt-4o               80%     3.8s         7.4s         8       10
```

Define eval manifests with test cases, expected outputs, and scoring
metrics. The eval dataset includes known-buggy code (nil pointer derefs,
SQL injection, race conditions), clean code, and security-relevant files
(path traversal, command injection, weak crypto). See
[evals/ci-pipeline-accuracy/](evals/ci-pipeline-accuracy/) for the starter
eval.

## Cluster Layout

```
my-cluster/
+-- cluster.yaml           # NATS config, defaults, health settings
+-- agents/
|   +-- code-reviewer/
|   |   +-- manifest.yaml  # Runtime, capabilities, resources
|   |   +-- entrypoint.sh  # Agent logic (any language)
|   +-- test-runner/
|   |   +-- manifest.yaml
|   |   +-- entrypoint.sh
|   +-- security-scanner/
|       +-- manifest.yaml
|       +-- entrypoint.sh
+-- teams/
    +-- ci-pipeline.yaml   # Team lead, communication settings
```

## Requirements

- [Go 1.25](https://go.dev/dl/) or later
- macOS or Linux for building and local development
- Linux with KVM for Firecracker VM isolation (production)

## Templates

```bash
# List available templates
./bin/hivectl init --list-templates

# Scaffold a CI pipeline team
./bin/hivectl init --template ci-pipeline my-pipeline

# Start local dev environment
./bin/hivectl dev --cluster-root my-pipeline
```

## NixOS Deployment

```nix
# /etc/nixos/flake.nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    hive.url = "github:brmurrell3/hive";
  };

  outputs = { nixpkgs, hive, ... }: {
    nixosConfigurations.nixos = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        ./configuration.nix
        hive.nixosModules.default
        {
          services.hived = {
            enable = true;
            clusterRoot = "/home/deploy/hive-cluster";
            user = "deploy";
            group = "users";
            openFirewall = true;
          };
        }
      ];
    };
  };
}
```

## Project Layout

```
cmd/
  hived/           Control plane daemon (embedded NATS, state, reconciler)
  hivectl/         CLI tool (validate, init, dev, trigger, agents, tokens)
  hive-agent/      Tier 2 native agent join binary
  hive-sidecar/    Sidecar runtime for agent VMs
internal/
  config/          YAML parsing + validation
  sidecar/         Agent runtime, HTTP API, heartbeats, capability routing
  capability/      NATS capability routing with cross-team support
  nats/            Embedded NATS server wrapper
  vm/              Firecracker VM lifecycle + nftables networking
  state/           SQLite state persistence
  health/          Heartbeat monitor + auto-restart
  reconciler/      Desired-state reconciliation loop
  scheduler/       Bin-packing node scheduler
  auth/            RBAC (admin, operator, viewer)
  templates/       Embedded agent team templates
sdk/
  python/          Python SDK (zero dependencies)
  go/              Go SDK
  typescript/      TypeScript SDK (zero dependencies)
```

## Documentation

| Document | Description |
|----------|-------------|
| [Getting Started](docs/getting-started.md) | From zero to running agents |
| [Operations](docs/operations.md) | Installation, configuration, troubleshooting |
| [Architecture](docs/architecture.md) | Design, agent identity, capability routing, vol research deployment |
| [API & Schema Reference](docs/api-reference.md) | HTTP APIs, NATS subjects, YAML manifests, SDKs |
| [CLI Reference](docs/cli-reference.md) | All hivectl commands |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and contribution guidelines.

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
