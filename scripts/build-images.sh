#!/bin/bash
# APOLLO Docker Images Build Script

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
APOLLO_DIR="$(dirname "$SCRIPT_DIR")"

TAG="${1:-latest}"

echo "============================================"
echo "Building APOLLO Docker Images (tag: $TAG)"
echo "============================================"

# Build Scheduling Policy Engine
echo ""
echo "[1/3] Building scheduling-policy-engine..."
cd "$APOLLO_DIR/scheduling-policy-engine"
# forecaster.proto 재사용 (단일 소스: node-resource-forecaster)
cp "$APOLLO_DIR/node-resource-forecaster/proto/forecaster.proto" proto/
docker build -t scheduling-policy-engine:$TAG .
echo "✓ scheduling-policy-engine:$TAG built"

# Build Node Resource Forecaster
echo ""
echo "[2/3] Building node-resource-forecaster..."
cd "$APOLLO_DIR/node-resource-forecaster"
docker build -t node-resource-forecaster:$TAG .
echo "✓ node-resource-forecaster:$TAG built"

# Build Orchestration Policy Engine
echo ""
echo "[3/3] Building orchestration-policy-engine..."
cd "$APOLLO_DIR/orchestration-policy-engine"
docker build -t orchestration-policy-engine:$TAG .
echo "✓ orchestration-policy-engine:$TAG built"

echo ""
echo "============================================"
echo "All images built successfully!"
echo "============================================"
docker images | grep -E "(scheduling-policy|node-resource|orchestration-policy)"
