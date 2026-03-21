# Hive

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![CI](https://github.com/brmurrell3/hive/actions/workflows/ci.yml/badge.svg)](https://github.com/brmurrell3/hive/actions/workflows/ci.yml)

**Declarative orchestration for AI agent teams.**
Define agents in YAML, connect them over an embedded NATS message bus, and deploy anywhere -- from a laptop to an air-gapped data center. One binary, no Docker, no Python dependencies.

## Quickstart

```bash
brew install brmurrell3/tap/hive        # or: see docs/install.md for Linux, NixOS, source builds

hivectl init --template ci-pipeline my-pipeline
hivectl dev --cluster-root my-pipeline
```

In a second terminal:

```bash
hivectl trigger --cluster-root my-pipeline --team ci-pipeline \
  --payload '{"file_path": "main.go", "test_command": "go test ./..."}'
```

Three AI agents start, collaborate on a CI pipeline, and print a JSON report. No API key needed (mock responses by default). Set `ANTHROPIC_API_KEY` for real LLM-powered results.

Clean up: `rm -rf my-pipeline`

## How It Works

```
                    hivectl trigger
                         |
                         v
               +---------+---------+
               |   code-reviewer   |  <-- lead agent, orchestrates the pipeline
               +----+--------+----+
                    |        |
          invoke-remote   invoke-remote
            (NATS)          (NATS)
                    |        |
            +-------+    +-------+
            | test- |    |securi-|
            | runner|    |ty-    |
            |       |    |scanner|
            +-------+    +-------+
```

Agents are defined in YAML with typed capabilities. Each agent gets a sidecar that handles heartbeats, capability registration, and message routing -- agents just implement HTTP handlers. The sidecar auto-generates LLM tool definitions from capability schemas, so agents discover and invoke each other without hard-coded integrations.

## Supported Platforms

| OS | Architecture | Install | Agent Isolation |
|----|-------------|---------|-----------------|
| macOS | Apple Silicon (arm64) | `brew install brmurrell3/tap/hive` | Process backend |
| macOS | Intel (x86_64) | `brew install brmurrell3/tap/hive` | Process backend |
| Linux | x86_64 | [Install script](docs/install.md) or [release binary](https://github.com/brmurrell3/hive/releases/latest) | Firecracker microVMs (with KVM) or process backend |
| Linux | arm64 | [Install script](docs/install.md) or [release binary](https://github.com/brmurrell3/hive/releases/latest) | Firecracker microVMs (with KVM) or process backend |
| NixOS | x86_64 / arm64 | [Flake module](docs/install.md#nixos) | Firecracker microVMs (with KVM) or process backend |

The **process backend** runs agents as OS processes -- full orchestration, no VM overhead. On Linux with KVM, **Firecracker microVMs** add per-agent kernel, memory, and network isolation.

## Features

- **Declarative YAML** -- agents, teams, clusters, all version-controlled
- **Firecracker microVMs** -- per-agent isolation on Linux (process backend on macOS)
- **Capability routing** -- typed request/reply over NATS, auto-generated LLM tools
- **Model-agnostic** -- Anthropic, OpenAI, Ollama, Google, Mistral, Cohere
- **Hot-reload** -- edit a manifest, agents restart automatically
- **Built-in evals** -- measure agent accuracy across models and prompts
- **RBAC, metrics, log aggregation** -- production-ready out of the box
- **SDKs** -- Python, C, MicroPython (for IoT/firmware agents)
- **Self-hosted** -- single Go binary, runs air-gapped

## Why Hive?

| | LangGraph / CrewAI | E2B | AWS AgentCore | **Hive** |
|---|---|---|---|---|
| Multi-agent orchestration | Yes | No | Yes | **Yes** |
| Per-agent VM isolation | No | Yes (sandbox only) | Yes | **Yes** |
| Self-hosted / air-gapped | Difficult | No | No | **Yes** |
| Declarative config | Python code | API calls | Console + SDK | **YAML** |
| Open source | Partially | Partially | No | **Apache 2.0** |

## Documentation

| | |
|---|---|
| **[Install](docs/install.md)** | Homebrew, Linux script, NixOS, source, air-gapped |
| **[Getting Started](docs/getting-started.md)** | From zero to running agents |
| **[Architecture](docs/architecture.md)** | Agent design, capability routing, SOUL.md, MEMORY.md |
| **[Operations](docs/operations.md)** | Configuration, networking, backup, troubleshooting |
| **[API Reference](docs/api-reference.md)** | HTTP APIs, NATS subjects, YAML schemas, SDKs |
| **[CLI Reference](docs/cli-reference.md)** | All hivectl commands |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and guidelines.

## License

Apache License 2.0. See [LICENSE](LICENSE).
