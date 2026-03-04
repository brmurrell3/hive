#!/usr/bin/env bash
#
# Reproducible audit script for Hive.
# Runs all static analysis, security scanning, tests, and fuzz tests.
# Exits non-zero on any failure.
#
# Usage:
#   ./scripts/audit.sh          # full audit
#   FUZZ_TIME=10s ./scripts/audit.sh  # shorter fuzz runs
#
set -euo pipefail

FUZZ_TIME="${FUZZ_TIME:-30s}"

RED='\033[0;31m'
GREEN='\033[0;32m'
BOLD='\033[1m'
RESET='\033[0m'

pass() { echo -e "${GREEN}PASS${RESET} $1"; }
fail() { echo -e "${RED}FAIL${RESET} $1"; exit 1; }
step() { echo -e "\n${BOLD}=== $1 ===${RESET}"; }

# Ensure tools are installed
ensure_tool() {
    local cmd="$1"
    local pkg="$2"
    if ! command -v "$cmd" &>/dev/null; then
        echo "Installing $cmd..."
        go install "$pkg"
    fi
}

ensure_tool golangci-lint github.com/golangci/golangci-lint/cmd/golangci-lint@latest
ensure_tool gosec github.com/securego/gosec/v2/cmd/gosec@latest
ensure_tool govulncheck golang.org/x/vuln/cmd/govulncheck@latest

# 1. golangci-lint
step "golangci-lint (expanded linter set)"
golangci-lint run ./... && pass "golangci-lint" || fail "golangci-lint"

# 2. gosec
step "gosec (security scanner)"
gosec -quiet ./... && pass "gosec" || fail "gosec"

# 3. govulncheck
step "govulncheck (known CVEs)"
govulncheck ./... && pass "govulncheck" || fail "govulncheck"

# 4. Tests with race detector
step "go test -race -count=1"
go test -tags unit,integration -race -count=1 -timeout 10m ./... && pass "tests" || fail "tests"

# 5. Stress test for flaky races
step "go test -race -count=5 (flaky race detection)"
go test -tags unit,integration -race -count=5 -timeout 20m ./... && pass "race stress" || fail "race stress"

# 6. Fuzz tests
step "Fuzz tests (${FUZZ_TIME} each)"

FUZZ_TARGETS=(
    "internal/config FuzzParseMemory"
    "internal/config FuzzParseDiskSize"
    "internal/types FuzzValidateSubjectComponent"
    "internal/types FuzzEnvelopeValidate"
    "internal/auth FuzzAuthenticate"
)

for target in "${FUZZ_TARGETS[@]}"; do
    pkg=$(echo "$target" | cut -d' ' -f1)
    func=$(echo "$target" | cut -d' ' -f2)
    echo "  Fuzzing $pkg.$func for $FUZZ_TIME..."
    go test -tags unit -run=^$ -fuzz="^${func}$" -fuzztime="$FUZZ_TIME" "./${pkg}" \
        && pass "  $func" || fail "  $func"
done

echo ""
echo -e "${GREEN}${BOLD}All audit checks passed.${RESET}"
