#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
test -n "${TOKEN_SECRET:-}"
test -n "${GOOGLE_APPLICATION_CREDENTIALS_FILE:-}"
cred_copy="$(mktemp)"
cp "$GOOGLE_APPLICATION_CREDENTIALS_FILE" "$cred_copy"
chmod 600 "$cred_copy"
export GOOGLE_APPLICATION_CREDENTIALS_FILE="$cred_copy"
mkdir -p deploy/certs
if [[ ! -f deploy/certs/voice.example.com.crt ]]; then
  openssl req -x509 -newkey rsa:2048 -nodes -keyout deploy/certs/voice.example.com.key \
    -out deploy/certs/voice.example.com.crt -days 2 -subj '/CN=localhost'
  cp deploy/certs/voice.example.com.crt deploy/certs/admin.crt
  cp deploy/certs/voice.example.com.key deploy/certs/admin.key
fi
docker build -f deploy/Dockerfile -t govoice:release-security .
docker compose -f deploy/docker-compose.yaml up -d
trap 'docker compose -f deploy/docker-compose.yaml down; rm -f "$cred_copy"' EXIT
for _ in $(seq 1 30); do
  if curl -fsS --insecure https://127.0.0.1/health; then
    break
  fi
  sleep 1
done
curl -fsS --insecure https://127.0.0.1/health
docker kill -s TERM govoice-server
docker wait govoice-server
test "$(docker inspect -f '{{.State.ExitCode}}' govoice-server)" = "0"
