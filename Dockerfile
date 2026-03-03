# audio-dock Dockerfile — Go static binary (v2.0)
# Stage 1: Build — golang:1.26-alpine has musl libc + CA certs; produces statically-linked binary
# Stage 2: Runtime — FROM scratch (empty filesystem); only the binary and CA certs are included

# ── Stage 1: Build ───────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Download dependencies first — this layer caches unless go.mod/go.sum changes
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags="-s -w" \
    -trimpath \
    -o /audio-dock \
    ./cmd/audio-dock

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────
# FROM scratch = empty filesystem; only our binary and CA certs are present.
# No shell, no libc, no package manager — minimal attack surface.
FROM scratch

# Copy CA certificates from the Alpine builder stage.
# REQUIRED for Phase 6: wss:// WebSocket TLS connections will fail without this.
# FROM scratch has no /etc/ssl/ directory — we must copy it explicitly.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the statically-linked binary
COPY --from=builder /audio-dock /audio-dock

# SIP ports — documented for reference; not required with network_mode: host
# Note: DO NOT EXPOSE the RTP port range (10000-10099/udp) here —
# large port ranges stall Docker Desktop's port proxy on macOS.
# RTP EXPOSE is added in Phase 6 with the macOS override pattern.
EXPOSE 5060/udp
EXPOSE 5060/tcp

ENTRYPOINT ["/audio-dock"]
