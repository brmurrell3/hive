# Hive Documentation

[← Back to Project](../README.md)

## Guides

| Document | Description |
|----------|-------------|
| [Getting Started](getting-started.md) | Scaffold a cluster, validate, start the control plane, and manage agents |
| [Operations Guide](operations.md) | Full operational reference covering all Hive features |
| [Testing Guide](testing.md) | Test strategy, mock mode, and real Firecracker testing |

## Specifications

| Document | Description |
|----------|-------------|
| [Architecture](architecture.md) | Agent tiers, node types, component map, execution model |
| [Schemas](schemas.md) | YAML manifest specification for clusters, agents, and teams |
| [Communication](communication.md) | NATS subject hierarchy, message envelope, capability invocation protocol |
| [Control Plane](control-plane.md) | hived internals: reconciler, scheduler, node registry, VM manager |
| [Execution](execution.md) | Sidecar architecture, runtime lifecycle, boot sequences per tier |
| [Firmware](firmware.md) | C and MicroPython SDKs, build toolchain, OTA update protocol |
| [CLI Reference](cli-reference.md) | All hivectl commands, interaction model, director/lead tools |
| [Deployment](deployment.md) | NixOS modules, bootstrap, node joining, pre-built images |

## SDK Documentation

| SDK | Location | Target |
|-----|----------|--------|
| C SDK | [`sdk/firmware-c/`](../sdk/firmware-c/) | ESP-IDF, Arduino, Pico SDK, Zephyr, bare metal |
| MicroPython SDK | [`sdk/firmware-micropython/`](../sdk/firmware-micropython/) | ESP32, Pi Pico W |
