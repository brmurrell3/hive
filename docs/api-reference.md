# Hive API & Schema Reference

Comprehensive reference for the sidecar HTTP API, NATS subjects, dashboard REST API, SDKs, and YAML manifest schemas.

---

## Table of Contents

1. [Sidecar HTTP API](#sidecar-http-api)
2. [Dashboard REST API](#dashboard-rest-api)
3. [NATS Subject Hierarchy](#nats-subject-hierarchy)
4. [Message Envelope Format](#message-envelope-format)
5. [SDK Overview](#sdk-overview)
6. [CLI Reference Summary](#cli-reference-summary)

---

## Sidecar HTTP API

The sidecar runs inside each agent (VM or process) and exposes an HTTP API on port 9100 by default. TLS is supported via `TLSCertFile` and `TLSKeyFile` configuration.

Authentication: Bearer token in the `Authorization` header. The `/health` endpoint is unauthenticated.

### GET /health

Returns sidecar and runtime health status.

**Request:**
```
GET /health
```

**Response (200 OK):**
```json
{
  "sidecar": "healthy",
  "runtime": "healthy",
  "uptime_seconds": 3600
}
```

Sidecar status values:
- `healthy` -- NATS connected
- `degraded` -- NATS disconnected (reconnecting)
- `unhealthy` -- NATS never established

### GET /capabilities

Returns the list of capabilities registered on this agent.

**Request:**
```
GET /capabilities
Authorization: Bearer <token>
```

**Response (200 OK):**
```json
[
  {
    "name": "summarize",
    "description": "Summarize text input",
    "inputs": [
      {"name": "text", "type": "string", "description": "Text to summarize", "required": true}
    ],
    "outputs": [
      {"name": "summary", "type": "string", "description": "The summary"}
    ]
  }
]
```

### GET /team/capabilities

Returns all capabilities registered across teams, queried from the control plane via NATS.

**Request:**
```
GET /team/capabilities
Authorization: Bearer <token>
```

**Response (200 OK):**
```json
[
  {
    "name": "summarize",
    "agent_id": "agent-a",
    "team_id": "default",
    "description": "Summarize text input"
  },
  {
    "name": "translate",
    "agent_id": "agent-b",
    "team_id": "default",
    "description": "Translate text"
  }
]
```

### POST /capabilities/{name}/invoke

Invoke a capability locally on this agent.

**Request:**
```
POST /capabilities/summarize/invoke
Authorization: Bearer <token>
Content-Type: application/json

{
  "inputs": {
    "text": "Long document text here..."
  },
  "timeout": "30s"
}
```

**Response (200 OK):**
```json
{
  "status": "success",
  "outputs": {
    "summary": "A concise summary of the document."
  },
  "duration_ms": 1250
}
```

**Error response (422 Unprocessable Entity):**
```json
{
  "status": "error",
  "error": {
    "code": "CAPABILITY_FAILED",
    "message": "handler returned error: invalid input",
    "retryable": false
  },
  "duration_ms": 5
}
```

**Timeout response (504 Gateway Timeout):**
```json
{
  "status": "timeout",
  "duration_ms": 30000
}
```

### POST /capabilities/{name}/invoke-remote

Invoke a capability on a remote agent via NATS.

**Request:**
```
POST /capabilities/summarize/invoke-remote
Authorization: Bearer <token>
Content-Type: application/json

{
  "target": "agent-a",
  "inputs": {
    "text": "Long document text here..."
  },
  "timeout": "30s"
}
```

**Response (200 OK):**
```json
{
  "status": "success",
  "outputs": {
    "summary": "A concise summary of the document."
  },
  "duration_ms": 2500
}
```

**Error response (502 Bad Gateway -- target agent offline):**
```json
{
  "status": "error",
  "error": {
    "code": "AGENT_OFFLINE",
    "message": "nats: no responders",
    "retryable": true
  }
}
```

---

## Dashboard REST API

The dashboard API is served by hived on port 8080 by default. All `/api/*` endpoints enforce RBAC when users are configured.

### GET /api/cluster

Cluster overview.

**Response:**
```json
{
  "node_count": 2,
  "team_count": 1,
  "agent_count": 3,
  "uptime_seconds": 120,
  "agent_status": {
    "RUNNING": 2,
    "STOPPED": 1
  }
}
```

### GET /api/agents

List all agents (filtered by RBAC scope).

**Response:**
```json
[
  {
    "id": "code-reviewer",
    "team": "ci-pipeline",
    "status": "RUNNING",
    "uptime_seconds": 3600
  }
]
```

### GET /api/agents/{id}

Agent detail.

**Response:**
```json
{
  "id": "code-reviewer",
  "team": "ci-pipeline",
  "status": "RUNNING",
  "vm_pid": 12345,
  "vm_cid": 3,
  "restart_count": 0,
  "started_at": "2026-03-14T10:00:00Z"
}
```

### POST /api/agents/{id}/chat

Send a message to an agent (proxied via NATS, 10-second timeout).

**Request:**
```json
{
  "message": "Hello, what can you do?"
}
```

**Response:**
```json
{
  "agent_id": "code-reviewer",
  "response": "I can review code and run tests."
}
```

### GET /api/nodes

List all registered nodes.

**Response:**
```json
[
  {
    "id": "node-1",
    "tier": 1,
    "arch": "x86_64",
    "status": "online",
    "memory_total": "16Gi",
    "cpu_count": 8
  }
]
```

### GET /api/nodes/{id}

Node detail.

### GET /api/capabilities

All registered capabilities across all agents.

**Response:**
```json
{
  "agents": {
    "code-reviewer": {
      "team_id": "ci-pipeline",
      "capabilities": ["review-code", "generate-report"]
    }
  },
  "capabilities": {
    "review-code": ["code-reviewer"],
    "run-tests": ["test-runner"]
  }
}
```

### GET /api/logs/{agentID}

Query agent logs. Accepts `?limit=N` query parameter.

**Response:**
```json
[
  {
    "agent_id": "code-reviewer",
    "timestamp": "2026-03-14T10:00:00Z",
    "level": "info",
    "message": "Processing review request"
  }
]
```

### GET /api/users

List users (admin only).

### GET /healthz

Health check endpoint. Returns `200 OK` when the server is running.

### GET /readyz

Readiness check endpoint. Returns `200 OK` when the server is ready to accept requests.

### WebSocket: /ws

Connect for real-time events. RBAC filtering applies per-user.

**Event types:**

```json
{"type": "agent_state_change", "data": {"agent_id": "my-agent", "old_status": "RUNNING", "new_status": "STOPPED"}}
{"type": "heartbeat", "data": {"agent_id": "my-agent", "healthy": true}}
{"type": "log_entry", "data": {"agent_id": "my-agent", "message": "processing request..."}}
```

### GET /metrics

Prometheus metrics endpoint (text exposition format). See [Operations Guide](operations.md#prometheus-metrics) for metric details.

---

## NATS Subject Hierarchy

All subjects use the `hive.` prefix to avoid collisions with other NATS applications.

### Health and metrics

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.health.<agent_id>` | Agent -> hived | Health heartbeat |
| `hive.metrics.<agent_id>` | Agent -> hived | Metrics report |

**Health payload:**
```json
{
  "healthy": true,
  "uptime_seconds": 3600,
  "tier": "vm",
  "cpu_percent": 25,
  "memory_used_bytes": 268435456
}
```

### Join protocol

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.join.request` | Agent -> hived | Join request with token and hardware inventory |
| `hive.join.status.<node_name>` | hived -> Agent | Join response (accepted/rejected) |

### Control

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.control.<agent_id>` | hived -> Agent | Control commands (shutdown, restart, reload) |

### Capabilities

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.capabilities.registry` | Agent <-> All | Capability registration/deregistration announcements |
| `hive.capabilities.<agent_id>.<capability>.request` | Any -> Agent | Capability invocation request |
| `hive.capabilities.<agent_id>.<capability>.response` | Agent -> Requester | Capability invocation response |

**Capability request payload:**
```json
{
  "capability": "summarize",
  "inputs": {"text": "document content..."},
  "timeout": "30s"
}
```

**Capability response payload:**
```json
{
  "capability": "summarize",
  "status": "success",
  "outputs": {"summary": "A summary."},
  "duration_ms": 1250
}
```

### Agent messaging

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.agent.<agent_id>.inbox` | Any -> Agent | Direct messages to an agent |
| `hive.agent.state.<agent_id>` | hived -> All | Agent state change notifications |

### Team subjects

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.team.<team_id>.broadcast` | Lead -> Team | Team-wide broadcast |
| `hive.team.<team_id>.management` | Lead only | Lead agent control channel |
| `hive.team.<team_id>.tasks.<task_id>` | Lead -> Agent | Task assignment |
| `hive.team.<team_id>.results.<agent_id>` | Agent -> Lead | Task results |

### Logs

| Subject | Direction | Payload |
|---------|-----------|---------|
| `hive.logs.<agent_id>` | Agent -> hived | Log entries |

### Persistence

When `communication.persistent=true` in a team manifest, these subjects are backed by JetStream:
- `hive.team.<team_id>.broadcast`
- `hive.team.<team_id>.tasks.*`
- `hive.team.<team_id>.results.*`

Non-persistent (ephemeral) subjects: `hive.health.*`, `hive.metrics.*`, `hive.control.*`, `hive.join.*`, `hive.capabilities.registry`.

---

## Message Envelope Format

All NATS messages use a unified JSON envelope:

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "from": "agent-a",
  "to": "agent-b",
  "type": "capability-request",
  "timestamp": "2026-03-14T10:00:00Z",
  "payload": { },
  "reply_to": "hive.capabilities.agent-a.summarize.response",
  "correlation_id": "req-12345"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `id` | Yes | UUID v4, unique message identifier |
| `from` | Yes | Sender agent ID |
| `to` | Yes | Target agent, team, or broadcast |
| `type` | Yes | Message type (see below) |
| `timestamp` | Yes | RFC 3339 timestamp |
| `payload` | Yes | Message-type-specific data |
| `reply_to` | No | NATS subject for response delivery |
| `correlation_id` | No | Links request to response |

**Message types:** `task`, `result`, `broadcast`, `status`, `health`, `control`, `capability-request`, `capability-response`, `error`

**Limits:**
- Maximum payload size: 2 MB
- Maximum subject length: validated per component
- Timestamps must not be more than 5 minutes in the future

See [Architecture](architecture.md#communication-protocol) for error codes, retry behavior, and circuit breaker details.

---

## SDK Overview

Hive provides SDKs for building agents in three languages. All SDKs follow the same pattern: register capability handlers, and the SDK manages the HTTP callback server that the sidecar invokes.

### Python SDK

Location: `sdk/python/hive_sdk.py`

Zero external dependencies (stdlib only). Single file that can be copied into any project.

```python
from hive_sdk import HiveAgent

agent = HiveAgent()

@agent.capability("greet")
def greet(name: str, greeting: str = "Hello"):
    return {"message": f"{greeting}, {name}!"}

agent.run()
```

Features:
- Decorator-based capability registration
- Automatic sidecar registration on startup
- Signal handling (SIGTERM/SIGINT) for graceful shutdown
- Remote capability invocation via `agent.invoke_remote(target, capability, inputs)`
- Configuration from environment variables

### Go SDK

Location: `sdk/go/hive/`

Import path: `github.com/brmurrell3/hive/sdk/go/hive`

```go
agent := hive.NewAgent()

agent.HandleCapability("greet", func(inputs map[string]any) (map[string]any, error) {
    name := inputs["name"].(string)
    return map[string]any{"message": "Hello, " + name + "!"}, nil
})

agent.Run(context.Background())
```

Features:
- Handler-based capability registration
- Context-aware `Run()` with signal handling
- Remote capability invocation via `agent.InvokeRemote(target, capability, inputs)`
- HTTP client for sidecar communication
- Configuration from environment variables

### TypeScript SDK

Location: `sdk/typescript/src/index.ts`

Zero external runtime dependencies (Node.js stdlib only).

```typescript
import { HiveAgent } from "./src/index";

const agent = new HiveAgent();

agent.capability("greet", async (inputs) => {
    const name = inputs.name as string;
    return { message: `Hello, ${name}!` };
});

agent.run();
```

Features:
- Async capability handlers
- Automatic sidecar registration
- Remote capability invocation via `agent.invokeRemote(target, capability, inputs)`
- Configuration from environment variables

### Environment variables (all SDKs)

| Variable | Description | Default |
|----------|-------------|---------|
| `HIVE_AGENT_ID` | Agent identifier | Required |
| `HIVE_TEAM_ID` | Team identifier | Optional |
| `HIVE_SIDECAR_URL` | Sidecar API URL | `http://127.0.0.1:9100` |
| `HIVE_CALLBACK_PORT` | Agent HTTP callback port | Required |
| `HIVE_WORKSPACE` | Workspace directory | Optional |

---

## CLI Reference

See [CLI Reference](cli-reference.md) for all hivectl commands, flags, and examples.

---

## YAML Manifest Schemas

### Cluster Config (cluster.yaml)

```yaml
apiVersion: hive/v1
kind: Cluster
metadata:
  name: string  # REQUIRED
spec:
  nats:
    port: int              # default 4222
    clusterPort: int       # default 6222
    jetstream:
      enabled: bool        # default true
      maxMemory: string    # default "1GB"
      maxStorage: string   # default "10GB"
  defaults:
    resources:
      memory: string       # e.g. "512Mi"
      vcpus: int           # VM tier only
    health:
      interval: duration   # default "30s"
      timeout: duration    # default "5s"
      maxFailures: int     # default 3
    restart:
      policy: enum(always|on-failure|never)  # default "on-failure"
      maxRestarts: int     # default 5
      backoff: duration    # default "10s"
  secrets: map[string]string
  models:
    - name: string         # MUST NOT shadow provider names
      provider: string     # "anthropic", "ollama", "openai", etc.
      endpoint: string     # for local models
  users:
    - id: string
      role: enum(operator|viewer)
      token: string        # SHA-256 hash
      teams: list[string]  # "all" = all teams
```

### Agent Manifest (agents/AGENT_ID/manifest.yaml)

```yaml
apiVersion: hive/v1
kind: Agent
metadata:
  id: string               # [a-z0-9][a-z0-9-]{0,62}, globally unique
  team: string             # references team ID
spec:
  tier: enum(vm|native)    # auto-inferred if omitted
  resources:
    memory: string
    vcpus: int             # VM only
  runtime:
    type: enum(openclaw|custom|process)
    backend: enum(firecracker|process)  # inferred from tier
    model:
      provider: string     # cloud provider or model registry name
      name: string         # e.g. "claude-sonnet-4-5"
  capabilities:
    - name: string         # becomes tool name for invokers
      description: string  # used in auto-generated tool docs
      inputs:
        - name: string
          type: enum(string|int|float|bool|bytes)
          description: string
          required: bool   # default true
      outputs: [...]       # same schema as inputs
      async: bool          # default false
  network:                 # VM only
    egress: enum(none|restricted|full)
    egress_allowlist: list[string]
  health:
    interval: duration     # default "30s"
    maxFailures: int       # default 3
  restart:
    policy: enum(always|on-failure|never)
    maxRestarts: int       # default 5
  placement:
    nodeId: string
    nodeLabels: map[string]string
    arch: enum(amd64|arm64|armv7|armv6)
```

### Team Manifest (teams/TEAM_ID.yaml)

```yaml
apiVersion: hive/v1
kind: Team
metadata:
  id: string               # globally unique
spec:
  lead: string             # agent ID, must be team member
  communication:
    persistent: bool       # default false, enables JetStream
    historyDepth: int      # default 100
  shared_volumes:          # VM only
    - name: string
      hostPath: string
      access: enum(read-only|read-write)
```

### Eval Manifest (evals/EVAL_NAME/eval.yaml)

```yaml
apiVersion: hive/v1
kind: Eval
metadata:
  name: string             # e.g. "ci-pipeline-accuracy"
  team: string             # team under test
spec:
  dataset:
    - name: string
      payload: object
      expected:
        fields: map[string]any
        assertions:
          - type: enum(contains|equals|regex|json_path)
            path: string   # dot-separated JSON path
            value: any
  scoring:
    metrics:
      - name: string
        type: enum(accuracy|latency|cost|custom)
  runs:
    count: int             # default 1
    parallel: int          # default 1
    timeout: duration      # default "60s"
  models:
    - provider: string
      name: string         # e.g. "claude-sonnet-4-5", "gpt-4o"
```
