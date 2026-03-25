#!/usr/bin/env bash

registry="ketidevit2"
image_name="orchestration-policy-engine"
version="latest"
dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_dir="$dir/.."

IMG="$registry/$image_name:$version"

echo "================================"
echo "Building Orchestration Policy Engine"
echo "Registry: $registry"
echo "Image: $IMG"
echo "================================"

cd "$project_dir" || exit 1

# Build Docker image using Makefile
echo "Building Docker image..."
make docker-build IMG="$IMG"

if [ $? -ne 0 ]; then
    echo "Error: Docker build failed"
    exit 1
fi

echo "Docker image built successfully"

# Push image
echo "Pushing image to registry..."
make docker-push IMG="$IMG"

if [ $? -ne 0 ]; then
    echo "Error: Docker push failed"
    exit 1
fi

echo "================================"
echo "Build and push completed successfully!"
echo "Image: $IMG"
echo "================================"
