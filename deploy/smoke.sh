#!/bin/bash
# Keyless deploy smoke test: costs nothing, needs no provider key.
#
# Builds the production image (or uses SMOKE_IMAGE), starts it read-only with all capabilities dropped
# next to a throwaway Postgres, and probes it FROM OUTSIDE the container, the way a kubelet or
# Prometheus would:
#   - /ready answers 200 {"status":"ready"} on the published probe port (admin stays loopback-only),
#   - /metrics serves the build info and the circuit gauges,
#   - the probe port does not serve the management API or /debug/timeline,
#   - xAI speech and the other launch providers are in the build (management API via docker exec),
#   - SIGTERM drains and exits 0.
# Containers and the network are removed on exit. For the full flow with a Google key (credential,
# agent, voice token through nginx) see deploy/smoke-google.sh.
#
# Required: docker, curl, openssl. Optional: SMOKE_IMAGE=<already built image>.
set -euo pipefail
cd "$(dirname "$0")/.."
command -v docker >/dev/null
command -v openssl >/dev/null
run="govoice-smoke-$$"
image="${SMOKE_IMAGE:-govoice:smoke}"
fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
cleanup() {
  docker rm -f "$run-app" "$run-pg" >/dev/null 2>&1 || true
  docker network rm "$run-net" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ -z "${SMOKE_IMAGE:-}" ]]; then
  docker build -q -f deploy/Dockerfile -t "$image" . >/dev/null
fi

pgpass="$(openssl rand -hex 24)"
mgmt="$(openssl rand -hex 32)"
docker network create "$run-net" >/dev/null
docker run -d --name "$run-pg" --network "$run-net" \
  -e POSTGRES_USER=govoice -e POSTGRES_DB=govoice -e POSTGRES_PASSWORD="$pgpass" \
  postgres:16-alpine >/dev/null
for _ in $(seq 1 60); do
  docker exec "$run-pg" pg_isready -U govoice -d govoice >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$run-pg" pg_isready -U govoice -d govoice >/dev/null || fail "postgres did not come up"

# Secrets travel through the environment of this one process, never argv: --env-file reads a 0600 file.
envfile="$(mktemp)"
chmod 600 "$envfile"
trap 'rm -f "$envfile"; cleanup' EXIT
cat >"$envfile" <<EOF
TOKEN_SECRET=$(openssl rand -hex 32)
GOVOICE_MANAGEMENT_API_KEY=$mgmt
GOVOICE_CREDENTIAL_KEYS=smoke:$(openssl rand -base64 32)
GOVOICE_DATABASE_DSN=postgres://govoice:$pgpass@$run-pg:5432/govoice?sslmode=disable
EOF
docker run -d --name "$run-app" --network "$run-net" --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --tmpfs /tmp:rw,noexec,nosuid,size=256M \
  --env-file "$envfile" -p 127.0.0.1:0:9092 "$image" >/dev/null
rm -f "$envfile"

port="$(docker port "$run-app" 9092/tcp | head -1 | sed 's/.*://')"
[[ -n "$port" ]] || fail "probe port not published"
base="http://127.0.0.1:$port"

for _ in $(seq 1 60); do
  [[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/ready" || true)" == 200 ]] && break
  sleep 1
done
ready="$(curl -sS "$base/ready")" || { docker logs "$run-app" 2>&1 | tail -20 >&2; fail "/ready unreachable from outside the container"; }
grep -q '"status":"ready"' <<<"$ready" || fail "/ready not ready: $ready"
echo "ready: $ready"

metrics="$(curl -fsS "$base/metrics")" || fail "/metrics unreachable from outside the container"
for want in voice_build_info voice_active_sessions voice_llm_circuit_state; do
  grep -q "^$want" <<<"$metrics" || fail "/metrics missing $want"
done
curl -fsS "$base/health" >/dev/null || fail "/health unreachable"

for path in /debug/timeline /api/v1/agents; do
  code="$(curl -s -o /dev/null -w '%{http_code}' "$base$path")"
  [[ "$code" == 404 ]] || fail "probe listener serves $path ($code); it must only serve /health /ready /metrics"
done

# The management API is loopback-only inside the container: reach it the supported way.
providers="$(docker exec "$run-app" sh -c 'curl -fsS -H "Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY" http://127.0.0.1:9090/api/v1/providers')" \
  || fail "management API unreachable inside the container"
for p in google xai nvidia; do
  grep -q "\"id\":\"$p\"" <<<"$providers" || fail "provider $p missing from the build"
done
# xAI speech is compiled in (-tags xaispeech): the xai provider advertises stt and tts, not just llm.
jq_xai="$(tr -d ' \n' <<<"$providers" | sed 's/.*{"id":"xai"//; s/}.*//')"
[[ "$jq_xai" == *'"stt"'* && "$jq_xai" == *'"tts"'* ]] || fail "xAI STT/TTS missing from the image (built without -tags xaispeech?): $jq_xai"

docker kill -s TERM "$run-app" >/dev/null
docker wait "$run-app" >/dev/null
code="$(docker inspect -f '{{.State.ExitCode}}' "$run-app")"
[[ "$code" == 0 ]] || fail "graceful shutdown exit code $code"
echo "SMOKE OK"
