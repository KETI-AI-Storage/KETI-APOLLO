# ============================================
# APOLLO Policy Server Dockerfile
# Multi-stage build for minimal image size
# ============================================

# Build stage
FROM golang:1.23-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Set working directory
WORKDIR /app

# Copy go mod files first for caching
COPY go.mod go.sum* ./
RUN go mod download || true

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o /apollo-policy-server \
    cmd/main.go

# Runtime stage
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -S apollo && adduser -S apollo -G apollo

# Copy binary from builder
COPY --from=builder /apollo-policy-server /usr/local/bin/apollo-policy-server

# Set permissions
RUN chmod +x /usr/local/bin/apollo-policy-server

# Use non-root user
USER apollo

# Expose ports
EXPOSE 50051 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Run
ENTRYPOINT ["/usr/local/bin/apollo-policy-server"]
