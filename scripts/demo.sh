#!/usr/bin/env bash
# Run the Hive CI pipeline demo end-to-end.
# Usage: ./scripts/demo.sh (or: make demo)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DEMO_DIR="$ROOT_DIR/.demo"
HIVECTL="$ROOT_DIR/bin/hivectl"
DEMO_PORT=14222

cleanup() {
    if [ -n "${DEV_PID:-}" ]; then
        kill "$DEV_PID" 2>/dev/null || true
        wait "$DEV_PID" 2>/dev/null || true
    fi
    rm -rf "$DEMO_DIR"
}
trap cleanup EXIT INT TERM

# Clean up any leftover processes from a previous demo run.
STALE_PIDS=""
for port in $DEMO_PORT 9100 9101 9102 9200 9201 9202; do
    STALE_PIDS="$STALE_PIDS $(lsof -ti:"$port" 2>/dev/null || true)"
done
STALE_PIDS=$(echo "$STALE_PIDS" | xargs)
if [ -n "$STALE_PIDS" ]; then
    echo "Cleaning up previous demo processes..."
    echo "$STALE_PIDS" | xargs kill -9 2>/dev/null || true
    sleep 1
fi

# Scaffold a fresh demo directory.
rm -rf "$DEMO_DIR"
"$HIVECTL" init --template ci-pipeline "$DEMO_DIR" >/dev/null 2>&1
sed -i.bak "s/port: 4222/port: $DEMO_PORT/" "$DEMO_DIR/cluster.yaml" && rm -f "$DEMO_DIR/cluster.yaml.bak"

echo "Starting 3 agents (code-reviewer, test-runner, security-scanner)..."
"$HIVECTL" dev --cluster-root "$DEMO_DIR" >"$DEMO_DIR/dev.log" 2>&1 &
DEV_PID=$!

# Wait for agents to be ready.
for i in $(seq 1 30); do
    if grep -q "all agents started" "$DEMO_DIR/dev.log" 2>/dev/null; then break; fi
    sleep 0.5
done

if ! grep -q "all agents started" "$DEMO_DIR/dev.log" 2>/dev/null; then
    echo "ERROR: agents failed to start within 15s."
    echo ""
    cat "$DEMO_DIR/dev.log"
    exit 1
fi

echo "Agents ready. Running CI pipeline on README.md..."
echo ""

"$HIVECTL" trigger --cluster-root "$DEMO_DIR" --team ci-pipeline --timeout 30 \
    --payload '{"file_path":"README.md","test_command":"echo PASS: all tests passed"}'
