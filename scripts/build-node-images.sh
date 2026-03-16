#!/usr/bin/env bash
# Build CogOS node container images for local testing.
#
# Usage:
#   ./scripts/build-node-images.sh           # Build both kernel + gateway
#   ./scripts/build-node-images.sh kernel    # Build kernel only
#   ./scripts/build-node-images.sh gateway   # Build gateway only

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COGOS_DIR="$(dirname "$SCRIPT_DIR")"
GATEWAY_DIR="$COGOS_DIR/../moltbot-gateway"

build_kernel() {
    echo "=== Building cogos/kernel:latest ==="
    docker build -t cogos/kernel:latest "$COGOS_DIR"
    echo "  Done. Image: cogos/kernel:latest"
    docker images cogos/kernel:latest --format "  Size: {{.Size}}"
}

build_gateway() {
    echo "=== Building cogos/gateway:latest ==="
    if [ ! -d "$GATEWAY_DIR" ]; then
        echo "  Error: Gateway not found at $GATEWAY_DIR"
        exit 1
    fi
    docker build -t cogos/gateway:latest "$GATEWAY_DIR"
    echo "  Done. Image: cogos/gateway:latest"
    docker images cogos/gateway:latest --format "  Size: {{.Size}}"
}

case "${1:-all}" in
    kernel)  build_kernel ;;
    gateway) build_gateway ;;
    all)
        build_kernel
        echo
        build_gateway
        ;;
    *)
        echo "Usage: $0 [kernel|gateway|all]"
        exit 1
        ;;
esac

echo
echo "=== Ready ==="
echo "Run local multi-node test:"
echo "  cd $COGOS_DIR && docker compose -f docker-compose.node.yml up -d"
