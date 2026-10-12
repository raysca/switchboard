#!/bin/bash
# Release smoke test: builds the image, starts deploy/docker-compose.yaml (server,
# postgres flow store, nginx) with a Google service account mounted read-only, then
# creates a flow through the management API (it must be ready: the server found
# the account through GOOGLE_APPLICATION_CREDENTIALS) and issues a voice token for it.
#
# Required: TOKEN_SECRET, GOOGLE_APPLICATION_CREDENTIALS (host path of a Google
# service-account JSON key), docker, openssl, jq.
# GOVOICE_MANAGEMENT_API_KEY and POSTGRES_PASSWORD are generated when unset.
set -euo pipefail
cd "$(dirname "$0")/.."
test -n "${TOKEN_SECRET:-}"
test -r "${GOOGLE_APPLICATION_CREDENTIALS:-}"
command -v jq >/dev/null
jq -e '.type == "service_account"' "$GOOGLE_APPLICATION_CREDENTIALS" >/dev/null
export GOVOICE_MANAGEMENT_API_KEY="${GOVOICE_MANAGEMENT_API_KEY:-$(openssl rand -hex 32)}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-$(openssl rand -hex 24)}"
mkdir -p deploy/certs
if [[ ! -f deploy/certs/voice.example.com.crt ]]; then
  openssl req -x509 -newkey rsa:2048 -nodes -keyout deploy/certs/voice.example.com.key \
    -out deploy/certs/voice.example.com.crt -days 2 -subj '/CN=localhost'
  cp deploy/certs/voice.example.com.crt deploy/certs/admin.crt
  cp deploy/certs/voice.example.com.key deploy/certs/admin.key
fi
docker build -f deploy/Dockerfile -t govoice:release-security .
# The service account reaches the container as a read-only file, never through the API.
# A 0444 copy inside a 0700 directory: readable by the container user, not by other host users.
sadir="$(mktemp -d)"
cp "$GOOGLE_APPLICATION_CREDENTIALS" "$sadir/google-sa.json"
chmod 444 "$sadir/google-sa.json"
cat >"$sadir/override.yaml" <<EOF
services:
  govoice:
    environment:
      - GOOGLE_APPLICATION_CREDENTIALS=/run/secrets/google-sa.json
    volumes:
      - $sadir/google-sa.json:/run/secrets/google-sa.json:ro
EOF
compose() { docker compose -f deploy/docker-compose.yaml -f "$sadir/override.yaml" "$@"; }
# -v: a kept postgres volume keeps the previous run's random password.
trap 'compose down -v; rm -rf "$sadir"' EXIT
compose up -d
for _ in $(seq 1 30); do
  if curl -fsS --insecure https://127.0.0.1/health; then
    break
  fi
  sleep 1
done
curl -fsS --insecure https://127.0.0.1/health

# The admin listener binds 127.0.0.1:9090 inside the container, so management
# calls run there. api METHOD PATH reads a JSON body on stdin (keeping secrets
# out of argv) and prints the response body.
api() {
  docker exec -i govoice-server sh -c \
    'curl -sS --fail-with-body -X "$1" "http://127.0.0.1:9090$2" -H "Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY" -H "Content-Type: application/json" --data-binary @-' \
    sh "$1" "$2"
}

docker exec govoice-server curl -fsS http://127.0.0.1:9090/ready

# No credential is sent anywhere: the flow names providers, the server reads the environment.
flow="$(jq -n '{name: "smoke", config: {
  stt: {provider: "google", model: "chirp_2", language: "en-US"},
  llm: {provider: "google", model: "gemini-2.5-flash", system_prompt: "You are a release smoke test."},
  tts: {provider: "google", voice: "en-US-Chirp3-HD-Aoede"}}}' |
  api POST /api/v1/flows)"
flow_id="$(jq -er .data.id <<<"$flow")"
test "$(jq -r .data.readiness.state <<<"$flow")" = "ready"

token="$(jq -n --arg a "$flow_id" '{principal: "smoke", flow_id: $a}' |
  api POST /api/v1/voice-tokens | jq -er .data.token)"
test -n "$token"

docker kill -s TERM govoice-server
docker wait govoice-server
test "$(docker inspect -f '{{.State.ExitCode}}' govoice-server)" = "0"
