#!/bin/bash
# ============================================
# APOLLO Protobuf Code Generation Script
# ============================================

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$SCRIPT_DIR/.."
PROTO_DIR="$PROJECT_ROOT/api/proto"
OUTPUT_DIR="$PROJECT_ROOT/api/proto"

echo "=== APOLLO Protobuf Code Generation ==="
echo ""

# Check if protoc is installed
if ! command -v protoc &> /dev/null; then
    echo "Error: protoc is not installed"
    echo "Install with: apt-get install -y protobuf-compiler"
    exit 1
fi

# Check if Go plugins are installed
if ! command -v protoc-gen-go &> /dev/null; then
    echo "Installing protoc-gen-go..."
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
fi

if ! command -v protoc-gen-go-grpc &> /dev/null; then
    echo "Installing protoc-gen-go-grpc..."
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
fi

# Ensure PATH includes Go bin
export PATH="$PATH:$(go env GOPATH)/bin"

echo "Generating Go code from proto files..."
echo ""

# Generate Go code
protoc \
    --proto_path="$PROTO_DIR" \
    --go_out="$OUTPUT_DIR" \
    --go_opt=paths=source_relative \
    --go-grpc_out="$OUTPUT_DIR" \
    --go-grpc_opt=paths=source_relative \
    "$PROTO_DIR/apollo.proto"

echo "Generated files:"
ls -la "$OUTPUT_DIR"/*.go 2>/dev/null || echo "(No .go files yet)"

echo ""
echo "Done!"
