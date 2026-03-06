# Hive Manifest Schema Specification

Single source of truth for all YAML schemas in Hive. Deterministic, structured, optimized for Claude Code.

This document resolves review issues #1, #2, #7, #9, #10, #11, #14, and #17.

## CLUSTER CONFIG (cluster.yaml)

```yaml
apiVersion: hive/v1  # REQUIRED, literal
kind: Cluster  # REQUIRED, literal

metadata:
  name: string  # REQUIRED, cluster name

spec:
  nats:
    port: int  # default 4222, NATS client port
    clusterPort: int  # default 6222, NATS cluster peering (Phase 3)
    jetstream:
      enabled: bool  # default true
      storePath: string  # JetStream persistence path, default $statePath/jetstream
      maxMemory: string  # default "1GB"
      maxStorage: string  # default "10GB"
    mqtt:
      enabled: bool  # default true, MQTT bridge for Tier 3
      port: int  # default 1883

  defaults:  # agent defaults, overridable per-agent
    resources:
      memory: string  # e.g. "512Mi"
      vcpus: int  # VM tier only
      disk: string  # default "5GB", VM tier only
    network:
      egress: enum(none|restricted|full)  # default "restricted", VM tier only
    health:
      enabled: bool  # default true
      interval: duration  # default "30s", Go duration format
      timeout: duration  # default "5s"
      maxFailures: int  # default 3
    restart:
      policy: enum(always|on-failure|never)  # default "on-failure"
      maxRestarts: int  # default 5
      backoff: duration  # default "10s"

  secrets: map[string]string  # OPTIONAL, secret name to value. Empty value = read from env HIVE_SECRET_{KEYNAME}

  models: list  # OPTIONAL, model registry
    - name: string  # REQUIRED, unique registry name. MUST NOT shadow provider names (see VALIDATION rule 13)
      endpoint: string  # OPTIONAL, URL for local models (e.g. http://localhost:11434)
      provider: string  # REQUIRED, e.g. "anthropic", "ollama", "openai"

  nodes:
    autoApprove: bool  # default true, auto-approve join requests

  vm:
    kernelPath: string  # OPTIONAL, custom Linux kernel path, default: bundled
    rootfsPath: string  # OPTIONAL, custom rootfs path, default: Nix-built

  director:  # OPTIONAL, top-level orchestrator
    agentId: string  # REQUIRED, references agent in agents/, agent MUST NOT have metadata.team

  users: list  # OPTIONAL, enables multi-user mode when non-empty
    - id: string  # REQUIRED, unique user identifier
      name: string  # OPTIONAL, display name
      role: enum(operator|viewer)  # REQUIRED
      token: string  # REQUIRED, SHA-256 hash of auth token
      teams: list[string]  # OPTIONAL, team IDs. "all" = all teams
      agents: list[string]  # OPTIONAL, additional agent IDs beyond team membership

  communication:  # OPTIONAL, cluster-level communication settings
    crossTeamCapabilities: list[string] or "all"  # OPTIONAL, cluster-wide default for cross-team capability exposure. Teams can override.
```

## AGENT MANIFEST (agents/AGENT_ID/manifest.yaml)

```yaml
apiVersion: hive/v1  # REQUIRED
kind: Agent  # REQUIRED

metadata:
  id: string  # REQUIRED, regex [a-z0-9][a-z0-9-]{0,62}, globally unique
  team: string  # OPTIONAL, references team ID. Empty = teamless.
  labels: map[string]string  # OPTIONAL

spec:
  tier: enum(vm|native|firmware)  # OPTIONAL, auto-inferred if omitted
    # vm: requires Tier 1 node
    # native: requires Tier 1 or Tier 2 node. On Tier 1: runs as process managed by hived, direct host hardware access, no VM isolation. On Tier 2: runs as process managed by hive-agent.
    # firmware: requires Tier 3 node

  mode: enum(tool|peer)  # OPTIONAL, default "tool", firmware tier only
    # tool: responds to capability invocations only, no initiative
    # peer: receives tasks, initiates messages, runs autonomous logic loop
    # Ignored for vm and native tiers (they are always full peers)

  resources:
    memory: string  # OPTIONAL. vm: Firecracker ceiling. native: scheduler hint. firmware: ignored.
    vcpus: int  # OPTIONAL, vm tier only
    disk: string  # OPTIONAL, default "5GB", vm tier only

  runtime:
    type: enum(openclaw|custom|firmware-c|firmware-micropython)  # REQUIRED
      # openclaw: OpenClaw agent runtime. Valid for vm, native.
      # custom: user-provided entrypoint.sh. Valid for vm, native.
      # firmware-c: C firmware via Hive SDK. firmware tier only.
      # firmware-micropython: MicroPython firmware. firmware tier only.
    model:  # OPTIONAL, for LLM-backed agents
      provider: string  # provider name or cluster model registry name. Resolution order: (1) Check cluster.yaml spec.models for matching name, (2) If no match, treat as cloud provider identifier, (3) VALIDATION rule 13 applies: spec.models entries MUST NOT use reserved provider names: anthropic, openai, ollama, google, mistral, cohere
      name: string  # model identifier, e.g. "claude-sonnet-4-5", "llama-70b"
      env: map[string]string  # env vars. Supports ${SECRET_NAME} interpolation from spec.secrets

  capabilities: list  # OPTIONAL
    - name: string  # REQUIRED, unique within agent. Becomes tool name for invokers. Validation rule 7 applies.
      description: string  # REQUIRED, natural language. Used in auto-generated tool docs.
      inputs: list  # OPTIONAL
        - name: string  # REQUIRED
          type: enum(string|int|float|bool|bytes)  # REQUIRED. bytes encoding: see BINARY DATA HANDLING section
          description: string  # REQUIRED
          required: bool  # OPTIONAL, default true
      outputs: list  # OPTIONAL, same schema as inputs
      async: bool  # OPTIONAL, default false. true = returns task_id, result delivered later.

  network:  # vm tier only
    egress: enum(none|restricted|full)  # default from cluster defaults
    egress_allowlist: list[string]  # OPTIONAL, when egress=restricted, list of allowed domains/IPs

  volumes: list  # OPTIONAL, vm tier only
    - name: string  # REQUIRED, MUST reference team shared_volumes entry
      mountPath: string  # REQUIRED, absolute path in VM
      access: enum(read-only|read-write)  # REQUIRED

  health:
    enabled: bool  # OPTIONAL, default true, all tiers
    interval: duration  # OPTIONAL, default "30s"
    timeout: duration  # OPTIONAL, default "5s"
    maxFailures: int  # OPTIONAL, default 3
    # BEHAVIOR BY TIER:
    # vm: maxFailures consecutive failures -> restart VM per restart.policy
    # native: maxFailures consecutive failures -> restart process per restart.policy
    # firmware: maxFailures consecutive failures -> mark agent OFFLINE, no restart (device self-manages)

  restart:  # vm and native tiers only. firmware ignores (devices self-restart).
    policy: enum(always|on-failure|never)  # OPTIONAL, default "on-failure"
    maxRestarts: int  # OPTIONAL, default 5
    backoff: duration  # OPTIONAL, default "10s"

  hardware: # OPTIONAL, declares hardware peripherals
    gpio: bool  # OPTIONAL, default false
    camera: bool  # OPTIONAL, default false
    sensors: list[string]  # OPTIONAL, e.g. ["dht22", "bme280"]
    actuators: list[string]  # OPTIONAL, e.g. ["relay", "servo"]
    gpu: string  # OPTIONAL, e.g. "nvidia-4090"
    custom: map[string]string  # OPTIONAL

  placement:
    nodeId: string  # OPTIONAL, pin to specific node. Warning if node not registered (validation rule 16).
    nodeLabels: map[string]string  # OPTIONAL, selector
    arch: string  # OPTIONAL, enum: amd64, arm64, armv7, armv6, rp2040, esp32

  firmware:  # OPTIONAL, firmware tier only
    platform: enum(esp-idf|arduino|pico-sdk|zephyr|bare-metal)  # REQUIRED for firmware tier
    board: string  # REQUIRED for firmware tier, e.g. "esp32-devkitc", "pico_w", "nrf52840dk"
    partitionTable: string  # OPTIONAL, path relative to firmware/, default: platform default
    extraLibs: list[string]  # OPTIONAL, additional libraries
    buildFlags: map[string]string  # OPTIONAL, passed to build system
    flashMethod: enum(serial|ota)  # OPTIONAL, default "serial"
    flashBaud: int  # OPTIONAL, default 460800, serial only
```

## AGENT DIRECTORY LAYOUT

Structure in agents/AGENT_ID/:

  manifest.yaml  # REQUIRED
  AGENTS.md  # OPTIONAL, OpenClaw instructions
  SOUL.md  # OPTIONAL, OpenClaw personality
  MEMORY.md  # OPTIONAL, OpenClaw memory (see WORKSPACE STATE MODEL section)
  entrypoint.sh  # OPTIONAL, custom runtime entry
  firmware/  # OPTIONAL, firmware source directory
  skills/  # OPTIONAL, OpenClaw skills
  files/  # OPTIONAL, additional files

Injection by tier:

  vm: injected via virtiofs at /workspace/
  native: available at agent working directory
  firmware: firmware/ compiled and flashed via SDK toolchain

## WORKSPACE STATE MODEL

Definition files: agents/AGENT_ID/ in cluster root (user-authored)
Workspace state: .state/agents/AGENT_ID/workspace/ (runtime state)

On first creation: definition files copied to workspace.

On hot reload (MD, skills, files changes): definition files synced TO workspace, OVERWRITING workspace copies.
  EXCEPTION: MEMORY.md - if workspace MEMORY.md is newer than cluster root MEMORY.md, workspace version preserved.

On cold reload (manifest changes requiring restart): workspace preserved, definition files re-synced with same MEMORY.md exception.

On destroy --purge: workspace deleted.

On destroy (no --purge): workspace preserved for potential reuse.

Rule: cluster root is authoritative for all files EXCEPT MEMORY.md which uses last-modified-wins between cluster root and workspace.

## TEAM MANIFEST (teams/TEAM_ID.yaml)

```yaml
apiVersion: hive/v1  # REQUIRED
kind: Team  # REQUIRED

metadata:
  id: string  # REQUIRED, globally unique. Validation rule 5 applies.
  labels: map[string]string  # OPTIONAL

spec:
  lead: string  # OPTIONAL, agent ID. Must reference agent with matching team. Validation rule 3 applies.

  resources:
    maxMemory: string  # OPTIONAL, VM agents total ceiling
    maxAgents: int  # OPTIONAL

  communication:
    namespace: string  # OPTIONAL, default "team.{TEAM_ID}"
    persistent: bool  # OPTIONAL, default false, enables JetStream for team subjects
    historyDepth: int  # OPTIONAL, default 100, messages retained when persistent=true
    crossTeamCapabilities: list[string] or "all"  # OPTIONAL, capability names exposed to other teams. Default: empty (no exposure). Naming: {CAPABILITY_NAME}-{TEAM_ID}. Collision resolution applies (see below).

  shared_volumes: list  # OPTIONAL, vm tier only
    - name: string  # REQUIRED
      hostPath: string  # REQUIRED
      access: enum(read-only|read-write)  # REQUIRED
```

Cross-team capability collision resolution:

If agent has local capability with same name as cross-team tool: local wins, cross-team tool gets full prefix {CAPABILITY_NAME}-{TEAM_ID}.

If two teams expose same capability name: both get team suffix, no ambiguity.

Local capabilities NEVER get a suffix.

## BINARY DATA HANDLING

Encoding for bytes type in capabilities:

transport: base64-encoded string in JSON payload
field_name: same as declared output name
max_inline_size: 512KB after base64 encoding (corresponds to ~384KB raw)

large_payload_strategy: for payloads exceeding max_inline_size, use reference mode:

  agent stores data locally (filesystem for vm/native, flash for firmware)
  returns JSON: {"ref": "hive://AGENT_ID/blob/UUID", "size": int, "mime": string}
  consumer fetches via: sidecar GET /blobs/AGENT_ID/UUID (vm/native) or MQTT chunked transfer (firmware)

NATS max_payload: configure to 2MB in cluster.yaml spec.nats (default NATS is 1MB, Hive sets 2MB)

firmware compact format: for constrained Tier 3 devices, MQTT bridge translates between compact binary (MessagePack) and JSON. Compact format defined in firmware SDK headers.

## VALIDATION RULES

1. agent metadata.team MUST reference existing team ID or be empty
2. agent volumes MUST reference team shared_volumes by name
3. team lead MUST reference agent with matching team
4. agent IDs globally unique across all agents/
5. team IDs globally unique across all teams/
6. secret refs (${SECRET_NAME}) MUST resolve to spec.secrets entry
7. capability names unique within agent
8. tier must be compatible with runtime type: vm -> openclaw, custom | native -> openclaw, custom | firmware -> firmware-c, firmware-micropython
9. volumes only valid for vm tier
10. network egress only valid for vm tier
11. director agentId MUST reference existing agent with NO metadata.team
12. director agentId agent MUST have tier vm or native (not firmware)
13. spec.models entries MUST NOT use reserved provider names: anthropic, openai, ollama, google, mistral, cohere
14. firmware tier agents MUST have spec.firmware.platform and spec.firmware.board set
15. mode field only valid for firmware tier
16. placement.nodeId if set must reference a registered node (warning, not error - node may join later)
17. user IDs in spec.users must be unique
18. user teams references must be valid team IDs or "all"
19. user agents references must be valid agent IDs
