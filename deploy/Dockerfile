# Multi-stage production container for Voice Framework V1 matching spec §39
# Build stage
FROM golang:1.24-bookworm AS builder

WORKDIR /src

# Copy go work and modules
COPY go.work go.work.sum ./
COPY go.mod go.sum ./
COPY core/go.mod core/go.sum ./core/
COPY providers/google/go.mod providers/google/go.sum ./providers/google/
COPY providers/xai/go.mod ./providers/xai/
COPY server/go.mod server/go.sum ./server/

# Download dependencies
RUN go work sync

# Copy source code
COPY . .

# Build binaries
ENV CGO_ENABLED=1
RUN go build -ldflags="-s -w" -o /src/bin/vadworker ./cmd/vadworker && \
    go build -ldflags="-s -w" -o /src/bin/server ./cmd/server

# Verification stage for artifacts
RUN sha256sum models/silero_vad.onnx | grep 1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3

# Final runtime stage
FROM debian:bookworm-slim

# Install runtime dependencies: ca-certificates for TLS, tini for PID 1 child reaping
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tini curl libgomp1 && \
    rm -rf /var/lib/apt/lists/*

# Create unprivileged service user and group
RUN groupadd -g 10001 govoice && \
    useradd -u 10001 -g 10001 -m -s /sbin/nologin govoice

WORKDIR /opt/govoice

# Install binaries and assets
COPY --from=builder /src/bin/server /opt/govoice/bin/server
COPY --from=builder /src/bin/vadworker /opt/govoice/bin/vadworker
COPY --from=builder /src/models/silero_vad.onnx /opt/govoice/models/silero_vad.onnx
COPY --from=builder /src/config/production.yaml /etc/govoice/production.yaml

# Set ownership and permissions
RUN chown -R govoice:govoice /opt/govoice /etc/govoice && \
    chmod 755 /opt/govoice/bin/server /opt/govoice/bin/vadworker && \
    chmod 644 /opt/govoice/models/silero_vad.onnx /etc/govoice/production.yaml

# Set up writable scratch areas for read-only root filesystems
RUN mkdir -p /tmp /var/log/govoice && \
    chown -R govoice:govoice /tmp /var/log/govoice && \
    chmod 1777 /tmp

USER govoice:govoice

# Public WebSocket/Health listener
EXPOSE 8080
# Admin Metrics/Readiness listener
EXPOSE 9090

# Environment defaults
ENV PATH="/opt/govoice/bin:${PATH}"

# PID 1 init process reaps VAD worker child processes and propagates signals cleanly
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/opt/govoice/bin/server", "-config", "/etc/govoice/production.yaml"]
