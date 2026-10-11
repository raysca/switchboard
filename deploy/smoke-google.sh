#!/bin/bash
# Release smoke test: builds the image, starts deploy/docker-compose.yaml (server,
# postgres agent store, nginx), then adds a Google credential and an agent through
# the management API and issues a voice token for that agent.
#
# Required: TOKEN_SECRET, SMOKE_GOOGLE_SECRET_FILE (a file holding a Google API key
# or a service-account JSON key), docker, openssl, jq.
# GOVOICE_MANAGEMENT_API_KEY, GOVOICE_CREDENTIAL_KEYS and POSTGRES_PASSWORD are
# generated when unset.
set -euo pipefail
cd "$(dirname "$0")/.."
test -n "${TOKEN_SECRET:-}"
test -r "${SMOKE_GOOGLE_SECRET_FILE:-}"
command -v jq >/dev/null
export GOVOICE_MANAGEMENT_API_KEY="${GOVOICE_MANAGEMENT_API_KEY:-$(openssl rand -hex 32)}"
export GOVOICE_CREDENTIAL_KEYS="${GOVOICE_CREDENTIAL_KEYS:-smoke:$(openssl rand -base64 32)}"
export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-$(openssl rand -hex 24)}"
mkdir -p deploy/certs
if [[ ! -f deploy/certs/voice.example.com.crt ]]; then
  openssl req -x509 -newkey rsa:2048 -nodes -keyout deploy/certs/voice.example.com.key \
    -out deploy/certs/voice.example.com.crt -days 2 -subj '/CN=localhost'
  cp deploy/certs/voice.example.com.crt deploy/certs/admin.crt
  cp deploy/certs/voice.example.com.key deploy/certs/admin.key
fi
docker build -f deploy/Dockerfile -t govoice:release-security .
# -v: a kept postgres volume keeps the previous run's random password.
trap 'docker compose -f deploy/docker-compose.yaml down -v' EXIT
docker compose -f deploy/docker-compose.yaml up -d
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

# Service-account JSON is compacted (secrets may not contain control characters);
# anything else is treated as an API key. The secret only travels through pipes.
cred_id="$({ jq -c . "$SMOKE_GOOGLE_SECRET_FILE" 2>/dev/null || tr -d '\r\n' <"$SMOKE_GOOGLE_SECRET_FILE"; } |
  jq -Rs '{provider: "google", secret: rtrimstr("\n")}' |
  api POST /api/v1/providers/credentials | jq -er .data.id)"

# credential_id is explicit so credentials left by earlier runs cannot make it ambiguous.
agent="$(jq -n --arg c "$cred_id" '{name: "smoke", config: {
  stt: {provider: "google", model: "chirp_2", language: "en-US", credential_id: $c},
  llm: {provider: "google", model: "gemini-2.5-flash", system_prompt: "You are a release smoke test.", credential_id: $c},
  tts: {provider: "google", voice: "en-US-Chirp3-HD-Aoede", credential_id: $c}}}' |
  api POST /api/v1/agents)"
agent_id="$(jq -er .data.id <<<"$agent")"
test "$(jq -r .data.readiness.state <<<"$agent")" = "ready"

token="$(jq -n --arg a "$agent_id" '{principal: "smoke", agent_id: $a}' |
  api POST /api/v1/voice-tokens | jq -er .data.token)"
test -n "$token"

docker kill -s TERM govoice-server
docker wait govoice-server
test "$(docker inspect -f '{{.State.ExitCode}}' govoice-server)" = "0"
