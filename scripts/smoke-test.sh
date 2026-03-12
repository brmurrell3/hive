#!/usr/bin/env bash
#
# smoke-test.sh — Fast local smoke test for Hive.
#
# Builds binaries, starts a mock cluster, runs lifecycle operations,
# and verifies everything works. Catches most integration issues in ~30s.
#
# Usage:
#   ./scripts/smoke-test.sh          # Full smoke test
#   make smoke                       # Same thing via Makefile
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$ROOT_DIR/bin"
SMOKE_DIR=""
HIVED_PID=""
NATS_PORT=""
PASSED=0
FAILED=0
TOTAL=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

cleanup() {
    if [ -n "${HIVED_PID:-}" ]; then
        kill "$HIVED_PID" 2>/dev/null || true
        wait "$HIVED_PID" 2>/dev/null || true
    fi
    if [ -n "${SMOKE_DIR:-}" ] && [ -d "$SMOKE_DIR" ]; then
        rm -rf "$SMOKE_DIR"
    fi
}
trap cleanup EXIT INT TERM

log()  { printf "\033[0;34m[SMOKE]\033[0m %s\n" "$1"; }
pass() { printf "\033[0;32m[PASS]\033[0m  %s\n" "$1"; PASSED=$((PASSED + 1)); TOTAL=$((TOTAL + 1)); }
fail() { printf "\033[0;31m[FAIL]\033[0m  %s\n" "$1"; FAILED=$((FAILED + 1)); TOTAL=$((TOTAL + 1)); }

# Run a check: check "description" command args...
check() {
    local name="$1"
    shift
    if output=$("$@" 2>&1); then
        pass "$name"
    else
        fail "$name"
        printf "        %s\n" "$output" | head -5
    fi
}

# Run a check that expects the output to contain a string.
check_contains() {
    local name="$1"
    local expected="$2"
    shift 2
    local output
    if output=$("$@" 2>&1) && echo "$output" | grep -q "$expected"; then
        pass "$name"
    else
        fail "$name"
        printf "        expected to contain: %s\n" "$expected"
        printf "        got: %s\n" "$output" | head -5
    fi
}

find_free_port() {
    python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()" 2>/dev/null \
        || ruby -e "require 'socket'; s=TCPServer.new('',0); puts s.addr[1]; s.close" 2>/dev/null \
        || echo "14222"
}

wait_for_port() {
    local port="$1"
    local timeout="${2:-15}"
    local i=0
    while [ "$i" -lt "$((timeout * 2))" ]; do
        if (echo >/dev/tcp/127.0.0.1/"$port") 2>/dev/null; then
            return 0
        fi
        sleep 0.5
        i=$((i + 1))
    done
    return 1
}

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------

log "Building binaries..."
(cd "$ROOT_DIR" && mkdir -p bin && \
    go build -o bin/hived ./cmd/hived && \
    go build -o bin/hivectl ./cmd/hivectl && \
    go build -o bin/hive-agent ./cmd/hive-agent && \
    go build -o bin/hive-sidecar ./cmd/hive-sidecar)
log "Build complete."

NATS_PORT=$(find_free_port)
SMOKE_DIR="$(mktemp -d)"
log "Cluster root: $SMOKE_DIR (NATS port: $NATS_PORT)"

# Write cluster.yaml
cat > "$SMOKE_DIR/cluster.yaml" <<EOF
apiVersion: hive/v1
kind: Cluster
metadata:
  name: smoke-test
spec:
  nats:
    port: $NATS_PORT
    clusterPort: -1
    jetstream:
      enabled: true
  defaults:
    resources:
      memory: "256Mi"
    health:
      interval: "5s"
      timeout: "3s"
    restart:
      maxRetries: 2
      backoffSeconds: 1
EOF

# Create rootfs dir with dummy files
mkdir -p "$SMOKE_DIR/rootfs"
echo "fake" > "$SMOKE_DIR/rootfs/vmlinux"
echo "fake" > "$SMOKE_DIR/rootfs/rootfs.ext4"

# Create team manifest
mkdir -p "$SMOKE_DIR/teams"
cat > "$SMOKE_DIR/teams/default.yaml" <<EOF
apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: smoke-agent
EOF

# Agent manifest is written AFTER hived starts (to avoid reconciler auto-start).

# ---------------------------------------------------------------------------
# Start hived
# ---------------------------------------------------------------------------

log "Starting hived (mock Firecracker)..."
HIVE_TEST_FIRECRACKER=mock "$BIN_DIR/hived" \
    --cluster-root "$SMOKE_DIR" \
    --log-level warn \
    --force-process-backend \
    > "$SMOKE_DIR/hived.log" 2>&1 &
HIVED_PID=$!

if ! wait_for_port "$NATS_PORT" 15; then
    fail "hived failed to start within 15s"
    cat "$SMOKE_DIR/hived.log" | tail -20
    exit 1
fi
pass "hived started (PID $HIVED_PID)"

# Let hived finish initialization
sleep 2

# ---------------------------------------------------------------------------
# Run tests
# ---------------------------------------------------------------------------

HIVECTL="$BIN_DIR/hivectl"
HIVECTL_ARGS="--cluster-root $SMOKE_DIR"
export HIVE_TEST_FIRECRACKER=mock
export HIVE_NATS_PORT=$NATS_PORT

log "Running lifecycle tests..."

# Basic commands
check "hivectl version"          "$HIVECTL" version

# Agent lifecycle: write manifest after hived starts to control lifecycle manually
check_contains "agents list (empty)" "AGENT_ID" \
    "$HIVECTL" $HIVECTL_ARGS agents list

# Write agent manifest
mkdir -p "$SMOKE_DIR/agents/smoke-agent"
cat > "$SMOKE_DIR/agents/smoke-agent/manifest.yaml" <<AGENT_EOF
apiVersion: hive/v1
kind: Agent
metadata:
  id: smoke-agent
  team: default
spec:
  runtime:
    type: process
    command: "sleep 3600"
  capabilities:
    - name: echo
      description: "Echo capability for smoke test"
  resources:
    memory: "512Mi"
    vcpus: 2
AGENT_EOF

check "hivectl validate"         "$HIVECTL" $HIVECTL_ARGS validate

check "hivectl agents start" \
    "$HIVECTL" $HIVECTL_ARGS agents start smoke-agent

sleep 2

check_contains "agent shows RUNNING" "RUNNING" \
    "$HIVECTL" $HIVECTL_ARGS agents status smoke-agent

check "hivectl agents stop" \
    "$HIVECTL" $HIVECTL_ARGS agents stop smoke-agent

sleep 1

check "hivectl agents destroy" \
    "$HIVECTL" $HIVECTL_ARGS agents destroy smoke-agent

check_contains "agents list (after destroy)" "AGENT_ID" \
    "$HIVECTL" $HIVECTL_ARGS agents list

# Token operations
check_contains "hivectl tokens generate" "token" \
    "$HIVECTL" $HIVECTL_ARGS tokens generate

# Node listing
check "hivectl nodes" \
    "$HIVECTL" $HIVECTL_ARGS nodes

# hived version
check_contains "hived --version" "hived" \
    "$BIN_DIR/hived" --version

# hive-agent version
check_contains "hive-agent version" "hive-agent" \
    "$BIN_DIR/hive-agent" version

# hive-sidecar version
check_contains "hive-sidecar --version" "hive-sidecar" \
    "$BIN_DIR/hive-sidecar" --version

# ---------------------------------------------------------------------------
# Results
# ---------------------------------------------------------------------------

echo ""
if [ "$FAILED" -eq 0 ]; then
    printf "\033[0;32m[SMOKE] All %d tests passed.\033[0m\n" "$TOTAL"
    exit 0
else
    printf "\033[0;31m[SMOKE] %d/%d tests failed.\033[0m\n" "$FAILED" "$TOTAL"
    echo ""
    echo "hived log (last 30 lines):"
    tail -30 "$SMOKE_DIR/hived.log" 2>/dev/null || true
    exit 1
fi
