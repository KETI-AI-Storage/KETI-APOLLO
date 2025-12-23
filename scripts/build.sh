#!/bin/bash
# ============================================
# APOLLO Policy Server Build Script
# ============================================

set -e

# Configuration
IMAGE_NAME="ketidevit2/apollo-policy-server"
TAG="${1:-latest}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "============================================"
echo "Building APOLLO Policy Server"
echo "Image: ${IMAGE_NAME}:${TAG}"
echo "============================================"

cd "$PROJECT_DIR"

# Build Go binary (for local testing)
echo "[1/4] Building Go binary..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/apollo-policy-server cmd/main.go
echo "Binary built: bin/apollo-policy-server"

# Build Docker image
echo "[2/4] Building Docker image..."
docker build -t "${IMAGE_NAME}:${TAG}" .

# Tag as latest if not already
if [ "$TAG" != "latest" ]; then
    docker tag "${IMAGE_NAME}:${TAG}" "${IMAGE_NAME}:latest"
fi

echo "[3/4] Pushing to Docker Hub..."
docker push "${IMAGE_NAME}:${TAG}"
if [ "$TAG" != "latest" ]; then
    docker push "${IMAGE_NAME}:latest"
fi

# Import to containerd (for local k8s)
echo "[4/4] Importing to containerd..."
docker save "${IMAGE_NAME}:${TAG}" | ctr -n k8s.io images import - 2>/dev/null || true

echo "============================================"
echo "Build complete!"
echo "Image: ${IMAGE_NAME}:${TAG}"
echo "============================================"
