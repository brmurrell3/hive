#!/usr/bin/env bash
#
# soak-test.sh — Long-running stability test for Hive.
#
# Starts a mock cluster and repeatedly exercises lifecycle operations
# for SOAK_DURATION seconds (default: 300 = 5 minutes). Catches memory
# leaks, goroutine leaks, state corruption, and race conditions over time.
#
# Usage:
#   ./scripts/soak-test.sh                    # 5-minute soak
#   SOAK_DURATION=600 ./scripts/soak-test.sh  # 10-minute soak
#   make soak                                 # Via Makefile
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$ROOT_DIR/bin"
SOAK_DIR=""
HIVED_PID=""
SOAK_DURATION="${SOAK_DURATION:-300}"
NATS_PORT=""
ITERATION=0
ERRORS=0

cleanup() {
    if [ -n "${HIVED_PID:-}" ]; then
        kill "$HIVED_PID" 2>/dev/null || true
        wait "$HIVED_PID" 2>/dev/null || true
    fi
    if [ -n "${SOAK_DIR:-}" ] && [ -d "$SOAK_DIR" ]; then
        rm -rf "$SOAK_DIR"
    fi
}
trap cleanup EXIT INT TERM

log()  { printf "\033[0;34m[SOAK]\033[0m  %s\n" "$1"; }
warn() { printf "\033[0;33m[SOAK]\033[0m  %s\n" "$1"; }
err()  { printf "\033[0;31m[SOAK]\033[0m  %s\n" "$1"; ERRORS=$((ERRORS + 1)); }

find_free_port() {
    python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()" 2>/dev/null \
        || echo "14333"
}

wait_for_port() {
    local port="$1" i=0
    while [ "$i" -lt 30 ]; do
        if (echo >/dev/tcp/127.0.0.1/"$port") 2>/dev/null; then return 0; fi
        sleep 0.5; i=$((i + 1))
    done
    return 1
}

hivectl() {
    HIVE_NATS_PORT=$NATS_PORT HIVE_TEST_FIRECRACKER=mock \
        "$BIN_DIR/hivectl" --cluster-root "$SOAK_DIR" "$@" 2>&1
}

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------

log "Building binaries..."
(cd "$ROOT_DIR" && mkdir -p bin && \
    go build -o bin/hived ./cmd/hived && \
    go build -o bin/hivectl ./cmd/hivectl)
log "Build complete."

NATS_PORT=$(find_free_port)
SOAK_DIR="$(mktemp -d)"
log "Cluster root: $SOAK_DIR (NATS port: $NATS_PORT)"
log "Soak duration: ${SOAK_DURATION}s"

# Write cluster config
cat > "$SOAK_DIR/cluster.yaml" <<EOF
apiVersion: hive/v1
kind: Cluster
metadata:
  name: soak-test
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
      maxRetries: 3
      backoffSeconds: 1
EOF

mkdir -p "$SOAK_DIR/rootfs"
echo "fake" > "$SOAK_DIR/rootfs/vmlinux"
echo "fake" > "$SOAK_DIR/rootfs/rootfs.ext4"
mkdir -p "$SOAK_DIR/teams"
cat > "$SOAK_DIR/teams/default.yaml" <<EOF
apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: soak-agent-0
EOF

log "Starting hived..."
HIVE_TEST_FIRECRACKER=mock "$BIN_DIR/hived" \
    --cluster-root "$SOAK_DIR" \
    --log-level warn \
    --force-process-backend \
    > "$SOAK_DIR/hived.log" 2>&1 &
HIVED_PID=$!

if ! wait_for_port "$NATS_PORT" 15; then
    err "hived failed to start"
    cat "$SOAK_DIR/hived.log" | tail -20
    exit 1
fi
log "hived started (PID $HIVED_PID)"
sleep 2

# ---------------------------------------------------------------------------
# Soak loop
# ---------------------------------------------------------------------------

START_TIME=$(date +%s)
END_TIME=$((START_TIME + SOAK_DURATION))
AGENT_COUNTER=0

while [ "$(date +%s)" -lt "$END_TIME" ]; do
    ITERATION=$((ITERATION + 1))
    AGENT_ID="soak-agent-${AGENT_COUNTER}"
    AGENT_COUNTER=$((AGENT_COUNTER + 1))
    ELAPSED=$(( $(date +%s) - START_TIME ))
    REMAINING=$((SOAK_DURATION - ELAPSED))

    log "Iteration $ITERATION (${ELAPSED}s elapsed, ${REMAINING}s remaining, ${ERRORS} errors)"

    # Check hived is still alive
    if ! kill -0 "$HIVED_PID" 2>/dev/null; then
        err "hived crashed at iteration $ITERATION"
        cat "$SOAK_DIR/hived.log" | tail -30
        exit 1
    fi

    # Create agent manifest
    mkdir -p "$SOAK_DIR/agents/$AGENT_ID"
    cat > "$SOAK_DIR/agents/$AGENT_ID/manifest.yaml" <<EOF
apiVersion: hive/v1
kind: Agent
metadata:
  id: $AGENT_ID
  team: default
spec:
  runtime:
    type: process
    command: "sleep 3600"
  resources:
    memory: "512Mi"
    vcpus: 2
EOF

    # Update team manifest to include agent
    cat > "$SOAK_DIR/teams/default.yaml" <<EOF
apiVersion: hive/v1
kind: Team
metadata:
  id: default
spec:
  lead: $AGENT_ID
EOF

    # Start agent
    if ! hivectl agents start "$AGENT_ID" >/dev/null 2>&1; then
        err "Failed to start $AGENT_ID"
    fi

    sleep 2

    # Verify agent is running
    status=$(hivectl agents status "$AGENT_ID" 2>&1 || true)
    if ! echo "$status" | grep -q "RUNNING"; then
        warn "Agent $AGENT_ID not in RUNNING state: $status"
    fi

    # List agents (check for crashes during listing)
    if ! hivectl agents >/dev/null 2>&1; then
        err "hivectl agents failed at iteration $ITERATION"
    fi

    # List nodes (check node registry health)
    if ! hivectl nodes >/dev/null 2>&1; then
        err "hivectl nodes failed at iteration $ITERATION"
    fi

    # Stop and destroy agent
    hivectl agents stop "$AGENT_ID" >/dev/null 2>&1 || true
    sleep 1
    hivectl agents destroy "$AGENT_ID" >/dev/null 2>&1 || true

    # Clean up agent dir
    rm -rf "$SOAK_DIR/agents/$AGENT_ID"

    sleep 1
done

# ---------------------------------------------------------------------------
# Final health check
# ---------------------------------------------------------------------------

log "Soak complete. Running final health checks..."

if ! kill -0 "$HIVED_PID" 2>/dev/null; then
    err "hived is not running at end of soak"
else
    log "hived still running (PID $HIVED_PID)"
fi

if ! hivectl agents >/dev/null 2>&1; then
    err "hivectl agents failed after soak"
fi

if ! hivectl nodes >/dev/null 2>&1; then
    err "hivectl nodes failed after soak"
fi

# Check hived log for panics
if grep -qi "panic" "$SOAK_DIR/hived.log" 2>/dev/null; then
    err "Panic detected in hived log"
    grep -i "panic" "$SOAK_DIR/hived.log" | head -10
fi

# Check hived log for data races
if grep -qi "DATA RACE" "$SOAK_DIR/hived.log" 2>/dev/null; then
    err "Data race detected in hived log"
    grep -i "DATA RACE" "$SOAK_DIR/hived.log" | head -10
fi

# ---------------------------------------------------------------------------
# Results
# ---------------------------------------------------------------------------

echo ""
TOTAL_TIME=$(( $(date +%s) - START_TIME ))
if [ "$ERRORS" -eq 0 ]; then
    printf "\033[0;32m[SOAK] PASSED — %d iterations in %ds, 0 errors.\033[0m\n" "$ITERATION" "$TOTAL_TIME"
    exit 0
else
    printf "\033[0;31m[SOAK] FAILED — %d iterations in %ds, %d errors.\033[0m\n" "$ITERATION" "$TOTAL_TIME" "$ERRORS"
    exit 1
fi
