# Hive

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![CI](https://github.com/brmurrell3/hive/actions/workflows/ci.yml/badge.svg)](https://github.com/brmurrell3/hive/actions/workflows/ci.yml)

**Hive is a declarative framework for orchestrating AI agent teams.** Define agents in YAML, run them in secure sandboxes, and deploy anywhere -- from a laptop to an air-gapped data center. One Go binary, no Docker, no Python dependencies.

## Try it in 60 seconds

```bash
git clone https://github.com/brmurrell3/hive && cd hive
make demo
```

Three AI agents start, collaborate on a CI pipeline, and print a JSON report. No API key needed (mock responses by default). Set `ANTHROPIC_API_KEY` for real LLM-powered code review and security scanning.

Requires [Go 1.25+](https://go.dev/dl/).

<details>
<summary>Step-by-step (two terminals)</summary>

```bash
make build
./bin/hivectl init --template ci-pipeline my-pipeline
./bin/hivectl dev --cluster-root my-pipeline
```

In a second terminal:

```bash
./bin/hivectl trigger --cluster-root my-pipeline --team ci-pipeline \
  --payload '{"file_path": "main.go", "test_command": "go test ./..."}'
```

Clean up when done:

```bash
rm -rf my-pipeline
```

</details>

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

## Cluster Layout

```
my-cluster/
├── cluster.yaml           # NATS config, defaults, health settings
├── agents/
│   ├── code-reviewer/
│   │   ├── manifest.yaml  # Runtime, capabilities, resources
│   │   └── entrypoint.sh  # Agent logic (any language)
│   ├── test-runner/
│   │   ├── manifest.yaml
│   │   └── entrypoint.sh
│   └── security-scanner/
│       ├── manifest.yaml
│       └── entrypoint.sh
└── teams/
    └── ci-pipeline.yaml   # Team lead, communication settings
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
```

## Documentation

| Document | Description |
|----------|-------------|
| [Getting Started](docs/getting-started.md) | Scaffold, validate, start, and manage agents |
| [Operations](docs/operations.md) | Full operational reference |
| [Architecture](docs/architecture.md) | Tiers, components, execution model |
| [Schemas](docs/schemas.md) | YAML manifest specification |
| [Communication](docs/communication.md) | NATS subjects, envelope format, capability protocol |
| [CLI Reference](docs/cli-reference.md) | All hivectl commands |

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
