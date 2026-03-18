# Hive Agent Protocol Reference

This document describes how agents communicate within a Hive cluster.
Every agent has a **sidecar** process that handles NATS messaging,
capability routing, and health reporting. Agents interact with the
cluster exclusively through the sidecar's local HTTP API.

## Sidecar HTTP API

The sidecar listens on `http://localhost:9100` by default (configurable
via `--http-addr`). If a Bearer token is configured, all endpoints
except `/health` require an `Authorization: Bearer <token>` header.

### Discovery

**GET /health** (no auth required)

Returns sidecar and runtime health status.

```json
{
  "sidecar": "healthy",
  "runtime": "healthy",
  "uptime_seconds": 3600
}
```

**GET /capabilities**

Returns the capabilities registered by this agent.

```json
[
  {
    "name": "code-review",
    "description": "Reviews code changes",
    "inputs": [{"name": "diff", "type": "string", "required": true}],
    "outputs": [{"name": "review", "type": "string"}],
    "async": false
  }
]
```

**GET /team/capabilities**

Returns all capabilities across the cluster (queries the control plane).

```json
[
  {
    "name": "summarize",
    "agent_id": "researcher",
    "team_id": "research",
    "description": "Summarizes documents"
  }
]
```

### Invoking Capabilities

**POST /capabilities/{name}/invoke**

Invoke one of your own capabilities locally.

```json
{
  "inputs": {"key": "value"},
  "timeout": "30s"
}
```

**POST /capabilities/{name}/invoke-remote**

Invoke a capability on another agent via NATS.

```json
{
  "target": "other-agent-id",
  "inputs": {"key": "value"},
  "timeout": "30s"
}
```

Both return:

```json
{
  "status": "success",
  "outputs": {"result": "..."},
  "duration_ms": 150,
  "error": null
}
```

Error responses include:

```json
{
  "status": "error",
  "error": {
    "code": "HANDLER_ERROR",
    "message": "description",
    "retryable": false
  }
}
```

Error codes: `NOT_FOUND`, `HANDLER_ERROR`, `HANDLER_TIMEOUT`,
`SERVICE_OVERLOADED`, `INVALID_REQUEST`, `AGENT_OFFLINE`.

## Workflow Pattern

1. Call `GET /team/capabilities` to discover available agents and tools
2. Call `POST /capabilities/{cap}/invoke-remote` with target agent ID
3. Collect outputs and chain into the next invocation

## Environment Variables

The sidecar injects these into the runtime process:

| Variable | Description |
|----------|-------------|
| `HIVE_AGENT_ID` | This agent's unique identifier |
| `HIVE_TEAM_ID` | Team this agent belongs to |
| `HIVE_NATS_URL` | NATS server URL |
| `HIVE_SIDECAR_URL` | Sidecar HTTP API base URL |
| `HIVE_WORKSPACE` | Agent workspace directory path |
| `HIVE_SOUL` | Contents of SOUL.md (system prompt) |
| `HIVE_MEMORY` | Contents of MEMORY.md (context) |
| `HIVE_PROTOCOL` | Contents of HIVE.md (this protocol doc) |

## Workspace Layout

```
workspace/
  SOUL.md              # Agent system prompt / personality
  MEMORY.md            # Agent memory (hot-reloaded via NATS)
  HIVE.md              # Protocol reference (auto-generated)
  .hive-metadata.json  # Agent identity and startup info
  .hive/requests/      # File-based IPC (if not using callbacks)
```

## Capability Execution Modes

**HTTP Callback** (recommended): Sidecar forwards requests to
`{CallbackURL}/handle/{capability}` via HTTP POST. The agent's HTTP
server responds with `{"outputs": {...}}`.

**File-based IPC**: Sidecar writes request JSON to
`.hive/requests/{id}.json`. Agent writes response to
`.hive/requests/{id}.response.json`.

**No-op**: If no runtime is configured, the sidecar echoes inputs back.

## NATS Subjects

Agents do not need to interact with NATS directly (use the HTTP API
instead), but for reference:

| Subject | Purpose |
|---------|---------|
| `hive.capabilities.{agentID}.{cap}.request` | Capability invocations |
| `hive.control.{agentID}` | Control commands (shutdown, restart, status) |
| `hive.health.{agentID}` | Health heartbeats |
| `hive.agent.{agentID}.memory` | Memory updates |
| `hive.team.{teamID}.broadcast` | Team broadcasts |
| `hive.capabilities.register` | Capability registration |
