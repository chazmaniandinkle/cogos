# CogOS Kernel — Multi-stage Docker build
# Produces a minimal Alpine image with the kernel binary.
#
# Build:
#   docker build -t cogos/kernel:latest .
#
# Run:
#   docker run -v /path/to/.cog:/workspace/.cog -p 5100:5100 cogos/kernel:latest
#
# Multi-platform:
#   docker buildx build --platform linux/amd64,linux/arm64 -t cogos/kernel:latest .

# ── Stage 1: Build ───────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /build

# Copy module files first for layer caching
COPY go.mod go.sum ./
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY harness/go.mod harness/go.sum ./harness/
COPY envspec/go.mod ./envspec/

# Download dependencies
RUN go mod download

# Copy source
COPY . .

# Build with CGO for SQLite FTS5 support
RUN CGO_ENABLED=1 go build \
    -tags "fts5" \
    -ldflags="-s -w -X main.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /cog .

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────────
FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    sqlite-libs \
    git \
    curl \
    nodejs \
    npm

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force

# Create non-root user
RUN addgroup -S cogos && adduser -S cogos -G cogos

WORKDIR /workspace

# Copy kernel binary
COPY --from=builder /cog /usr/local/bin/cog

# Create workspace structure
RUN mkdir -p .cog/mem .cog/config .cog/run .cog/logs \
    && chown -R cogos:cogos /workspace

USER cogos

# Kernel API port
EXPOSE 5100

# Health check
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -f http://localhost:5100/health || exit 1

ENTRYPOINT ["cog"]
CMD ["serve", "--port", "5100"]
