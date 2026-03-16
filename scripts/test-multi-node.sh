#!/usr/bin/env bash
# Test CogOS multi-node deployment locally.
#
# This script:
#   1. Builds container images (kernel + gateway)
#   2. Starts a primary + secondary node via docker-compose
#   3. Verifies health endpoints
#   4. Checks container labels
#   5. Tears down
#
# Usage:
#   ./scripts/test-multi-node.sh              # Full test
#   ./scripts/test-multi-node.sh --no-build   # Skip image build
#   ./scripts/test-multi-node.sh --keep       # Don't tear down after test

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COGOS_DIR="$(dirname "$SCRIPT_DIR")"
COMPOSE_FILE="$COGOS_DIR/docker-compose.node.yml"
BUILD=true
KEEP=false

for arg in "$@"; do
    case "$arg" in
        --no-build) BUILD=false ;;
        --keep)     KEEP=true ;;
    esac
done

cleanup() {
    if [ "$KEEP" = false ]; then
        echo
        echo "=== Cleanup ==="
        docker compose -f "$COMPOSE_FILE" down -v 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo "=== CogOS Multi-Node Test ==="
echo

# Step 1: Build images
if [ "$BUILD" = true ]; then
    echo "--- Building images ---"
    "$SCRIPT_DIR/build-node-images.sh"
    echo
fi

# Step 2: Start nodes
echo "--- Starting nodes ---"
docker compose -f "$COMPOSE_FILE" up -d
echo
sleep 5  # Give services time to start

# Step 3: Check container status
echo "--- Container Status ---"
docker compose -f "$COMPOSE_FILE" ps
echo

# Step 4: Health checks
echo "--- Health Checks ---"
PASS=0
FAIL=0

check_health() {
    local name=$1
    local url=$2
    if curl -sf "$url" > /dev/null 2>&1; then
        echo "  $name: OK"
        PASS=$((PASS + 1))
    else
        echo "  $name: FAIL"
        FAIL=$((FAIL + 1))
    fi
}

check_health "Primary Kernel"    "http://localhost:5100/health"
check_health "Secondary Kernel"  "http://localhost:5101/health"
check_health "Vaultwarden"       "http://localhost:8222/"

echo

# Step 5: Check CogOS labels
echo "--- CogOS Managed Containers ---"
docker ps --filter "label=com.cogos.managed=true" \
    --format "  {{.Names}}\tnode={{index .Labels \"com.cogos.node\"}}\tservice={{index .Labels \"com.cogos.service\"}}\t{{.Status}}"
echo

# Step 6: Check volumes
echo "--- Volumes ---"
docker volume ls --filter "name=cogos" --format "  {{.Name}}"
echo

# Results
echo "=== Results ==="
echo "  Passed: $PASS"
echo "  Failed: $FAIL"

if [ "$FAIL" -gt 0 ]; then
    echo
    echo "--- Logs (last 20 lines per service) ---"
    docker compose -f "$COMPOSE_FILE" logs --tail=20
    exit 1
fi

if [ "$KEEP" = true ]; then
    echo
    echo "Nodes still running. To stop:"
    echo "  docker compose -f $COMPOSE_FILE down -v"
fi

echo
echo "=== All tests passed ==="
