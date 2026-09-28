# Production image for Voice Framework V1.
# The module graph requires Go 1.27.1. The runtime loads Silero and ONNX Runtime
# from the paths the process actually searches.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS builder

WORKDIR /src

COPY go.work go.work.sum ./
COPY go.mod go.sum ./
COPY core/go.mod ./core/
COPY providers/google/go.mod providers/google/go.sum ./providers/google/
COPY providers/xai/go.mod ./providers/xai/
COPY server/go.mod server/go.sum ./server/

RUN go work sync

COPY . .

ENV CGO_ENABLED=1
RUN go build -ldflags="-s -w" -o /src/bin/vadworker ./cmd/vadworker && \
    go build -ldflags="-s -w" -o /src/bin/server ./cmd/server

RUN test -f third_party/silero_vad.onnx && \
    sha256sum third_party/silero_vad.onnx | grep 1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tini curl libgomp1 && \
    rm -rf /var/lib/apt/lists/*

ARG ORT_VERSION=1.29.0
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) \
        ort_arch=x64; \
        expected_sha="c3fddc4f139a045b0c4902c57410f0694f1c2fdf9b6939fbe38b1aeae7cd14ba" ;; \
      arm64) \
        ort_arch=aarch64; \
        expected_sha="e1799098ebc054b370f6176a450f158720f297818c613e5dc99b92e2ec82346f" ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ort_arch}-${ORT_VERSION}.tgz" && \
    echo "${expected_sha}  /tmp/ort.tgz" | sha256sum -c - && \
    mkdir -p /opt/govoice/third_party /opt/govoice/lib && \
    tar -xzf /tmp/ort.tgz -C /opt/govoice/third_party && \
    cp "/opt/govoice/third_party/onnxruntime-linux-${ort_arch}-${ORT_VERSION}/lib/libonnxruntime.so" /opt/govoice/lib/libonnxruntime.so && \
    rm /tmp/ort.tgz

RUN groupadd -g 10001 govoice && \
    useradd -u 10001 -g 10001 -m -s /sbin/nologin govoice

WORKDIR /opt/govoice

COPY --from=builder /src/bin/server /opt/govoice/bin/server
COPY --from=builder /src/bin/vadworker /opt/govoice/bin/vadworker
COPY --from=builder /src/third_party/silero_vad.onnx /opt/govoice/third_party/silero_vad.onnx
COPY --from=builder /src/third_party/silero_vad.onnx /opt/govoice/models/silero_vad.onnx
COPY --from=builder /src/config/production.yaml /etc/govoice/production.yaml

RUN chown -R govoice:govoice /opt/govoice /etc/govoice && \
    chmod 755 /opt/govoice/bin/server /opt/govoice/bin/vadworker && \
    chmod 644 /opt/govoice/third_party/silero_vad.onnx /opt/govoice/models/silero_vad.onnx /etc/govoice/production.yaml /opt/govoice/lib/libonnxruntime.so

RUN mkdir -p /tmp /var/log/govoice && \
    chown -R govoice:govoice /tmp /var/log/govoice && \
    chmod 1777 /tmp

USER govoice:govoice

EXPOSE 8080
EXPOSE 9090

ENV PATH="/opt/govoice/bin:${PATH}" \
    GOVOICE_ROOT="/opt/govoice" \
    GOVOICE_MODEL_PATH="/opt/govoice/models/silero_vad.onnx" \
    GOVOICE_ORT_LIB="/opt/govoice/lib/libonnxruntime.so" \
    LD_LIBRARY_PATH="/opt/govoice/lib"

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/opt/govoice/bin/server", "-config", "/etc/govoice/production.yaml"]
