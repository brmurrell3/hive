# Hive CLI & Interaction Reference

## COMMAND STRUCTURE

All commands accept:
- `--config PATH` or `HIVE_CONFIG` env var (local control plane)
- `--control-plane ADDRESS` (remote control plane)
- Multi-user: additionally `--user USER_ID --token TOKEN` or `HIVE_USER`/`HIVE_TOKEN` env vars

**Output format**: tables default. `--output json` available. Errors → stderr. **Exit codes**: 0 (success), 1 (error), 2 (usage).

---

## HIVECTL CLUSTER COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl init PATH` | Scaffold cluster root (cluster.yaml, agents/, .state/). Idempotent. | Path to cluster root |
| `hivectl status` | Cluster overview: name, nodes, agent count, NATS status | Table |
| `hivectl validate` | Validate all manifests | 0 or 1, errors to stderr |
| `hivectl version` | Print hivectl and control plane version | Version string |

---

## HIVECTL TOKEN COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl tokens create [--ttl DURATION]` | Generate 256-bit join token. Print token to stdout. Store SHA-256 hash. | Token string |
| `hivectl tokens list` | List active tokens (creation time, last-used) | Table |
| `hivectl tokens revoke PREFIX` | Revoke token by first 8 chars | Status message |

---

## HIVECTL NODE COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl nodes list` | All nodes: ID, arch, status, resources, agent count, labels | Table |
| `hivectl nodes approve NODE_ID` | Approve pending node | Status |
| `hivectl nodes remove NODE_ID` | Deregister node | Status |

---

## HIVECTL AGENT COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl agents list` | All agents: ID, node, status | Table |
| `hivectl agents status AGENT_ID` | Detail: capabilities, health, resources, node, uptime | JSON or formatted |
| `hivectl agents logs AGENT_ID [--follow] [--since DURATION] [--tail N]` | Stream logs. --follow = live, --since = from timestamp, --tail = last N lines | Text stream |
| `hivectl agents restart AGENT_ID` | Restart agent | Status |
| `hivectl agents stop AGENT_ID` | Stop agent | Status |
| `hivectl agents start AGENT_ID` | Start agent | Status |
| `hivectl agents destroy AGENT_ID` | Destroy agent | Status |

---

## HIVECTL CAPABILITIES COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl capabilities list` | All registered capabilities across all agents | Table |

---

## HIVECTL USER COMMANDS

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl users create USER_ID --role operator\|viewer` | Create user, output token | Token string |
| `hivectl users list` | List users with role | Table |
| `hivectl users update USER_ID --role\|--teams\|--agents` | Modify user | Status |
| `hivectl users delete USER_ID` | Remove user | Status |

---

## HIVECTL DASHBOARD COMMAND

| Command | Purpose | Output |
|---------|---------|--------|
| `hivectl dashboard` | Interactive TUI dashboard with live agent status | TUI |

---

## OBSERVABILITY

### Log Access

| Command | Behavior |
|---------|----------|
| `hivectl agents logs AGENT_ID` | Live stream from control plane |
| `hivectl agents logs AGENT_ID --since 1h` | Entries since timestamp |
| `hivectl agents logs AGENT_ID --tail 100` | Last N lines |
| `hivectl agents logs AGENT_ID --follow` | Live stream (continuous) |

### Metrics

**Health heartbeats** contain uptime, CPU, and memory for each agent.

**Access**:
- Point-in-time: `hivectl agents status AGENT_ID`

---

## EXIT CODES

| Code | Meaning |
|------|---------|
| 0 | Command succeeded |
| 1 | Runtime error (invalid cluster, agent not found, execution failure, etc.) |
| 2 | Usage error (missing required arg, invalid flag, etc.) |

---

## ENV VARS (Summary)

| Var | Purpose | Example |
|-----|---------|---------|
| `HIVE_CONFIG` | Path to local cluster root (default: ./cluster.yaml) | `/etc/hive/cluster.yaml` |
| `HIVE_CONTROL_PLANE` | Remote control plane address | `hive.example.com:9090` |
| `HIVE_USER` | User ID (multi-user) | `alice` |
| `HIVE_TOKEN` | Auth token (multi-user) | `hv_xxxxxxxxxxxxxxxx` |

---

## ERROR HANDLING

- **Invalid config**: Error to stderr, exit 1. Message: "config validation failed: ..."
- **Agent not found**: Error to stderr, exit 1. Message: "agent {ID} not found"
- **Network failure**: Error to stderr, exit 1. Message: "connection to control plane failed: ..."
- **Permission denied**: Error to stderr, exit 1. Message: "user {UID} lacks access to {resource}"
- **Usage error**: Error to stderr, exit 2. Message: "usage: hivectl {command} ..."
