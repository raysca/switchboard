#!/usr/bin/env bash
# Black-box eval: pipecat.evals (Python) drives the Go server over RTVI/WebSocket.
# Providers are the simulated echo provider (no keys, no network, no cost). See
# docs/decisions/pipecat-evals.md. Skips (exit 0) when prerequisites are missing.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"; cd "$ROOT"
skip() { echo "eval-pipecat: SKIPPED: $*" >&2; exit 0; }
command -v uv >/dev/null || skip "uv not installed (https://docs.astral.sh/uv/)"
command -v go >/dev/null || skip "go not installed"
command -v curl >/dev/null || skip "curl not installed"
PIPECAT="${PIPECAT_DIR:-$ROOT/pipecat-impl}"
[ -f "$PIPECAT/pyproject.toml" ] || skip "pipecat checkout not found at $PIPECAT (set PIPECAT_DIR)"
[ -f third_party/silero_vad.onnx ] || skip "third_party/silero_vad.onnx missing"
pc() { (cd "$PIPECAT" && uv run --quiet --extra cli python "$ROOT/tools/eval/pipecat/run_eval.py" "$@"); }
# Skip only when pipecat itself is unavailable; a broken run_eval.py must fail, not skip.
(cd "$PIPECAT" && uv run --quiet --extra cli python -c 'import pipecat.evals, pipecat.cli.commands.eval' >/dev/null 2>&1) \
  || skip "pipecat.evals not importable via 'uv run --extra cli' in $PIPECAT"

OUT="$ROOT/.govoice-eval-pipecat"; rm -rf "$OUT"; mkdir -p "$OUT"; chmod 700 "$OUT"
PUB="${EVAL_PUBLIC_PORT:-18680}"; ADM="${EVAL_ADMIN_PORT:-19690}"
for p in "$PUB" "$ADM"; do curl -s -o /dev/null "http://127.0.0.1:$p/" && { echo "eval-pipecat: port $p in use (set EVAL_PUBLIC_PORT/EVAL_ADMIN_PORT)" >&2; exit 1; }; done
go build -o "$OUT/server-echo" ./cmd/server
go build -o "$OUT/vadworker" ./cmd/vadworker   # server looks for vadworker next to itself
sed -e "s|^public_listen_addr:.*|public_listen_addr: \"127.0.0.1:$PUB\"|" \
    -e "s|^admin_listen_addr:.*|admin_listen_addr: \"127.0.0.1:$ADM\"|" \
    config/development.yaml | awk '{print} /^providers:$/ {print "  echo: {mimic: instant, stt: audio}"}' > "$OUT/server.yaml"
grep -q 'echo: {mimic: instant' "$OUT/server.yaml" || { echo "eval-pipecat: config/development.yaml has no top-level providers: line to add providers.echo to" >&2; exit 1; }

export GOVOICE_DEV_DIR="$OUT/data" GOVOICE_MANAGEMENT_API_KEY="$(openssl rand -hex 32)" TOKEN_SECRET="$(openssl rand -hex 32)"
# The flow uses the echo provider (no keys). ENV_FILE=/dev/null stops the development server loading examples/.env itself.
export ENV_FILE=/dev/null
mkdir -p "$GOVOICE_DEV_DIR"; chmod 700 "$GOVOICE_DEV_DIR"
"$OUT/server-echo" -config "$OUT/server.yaml" -vad-mode inprocess >"$OUT/server.log" 2>&1 &
SRV=$!; trap 'kill $SRV 2>/dev/null; wait $SRV 2>/dev/null || true' EXIT
ready=0
for _ in $(seq 1 100); do curl -fsS "http://127.0.0.1:$ADM/ready" >/dev/null 2>&1 && { ready=1; break; }; kill -0 $SRV 2>/dev/null || break; sleep 0.2; done
[ $ready = 1 ] || { cat "$OUT/server.log" >&2; echo "eval-pipecat: server did not become ready" >&2; exit 1; }

api() { curl -fsS -X POST "http://127.0.0.1:$ADM/api/v1/$1" -H "Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY" -H 'Content-Type: application/json' -d "$2"; }
jid() { python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["id"])'; }
FLOW=$(api flows '{"name":"Eval Echo","description":"echo provider","config":{"stt":{"provider":"echo","model":"chirp_2","language":"en-US"},"llm":{"provider":"echo","model":"fake","system_prompt":"fake"},"tts":{"provider":"echo","voice":"fake"}}}' | jid)

fail=0
for f in tools/eval/pipecat/scenarios/*.yaml; do
  # Voice tokens are single-use: one token (and one invocation) per scenario file.
  TOKEN=$(api voice-tokens "{\"principal\":\"pipecat-eval\",\"flow_id\":\"$FLOW\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')
  # The harness echoes the bot URL; mask the (single-use, 60 s) token.
  pc run "$ROOT/$f" --bot-url "ws://127.0.0.1:$PUB/ws?token=$TOKEN" --logs-dir "$OUT" 2>&1 | sed -E 's/token=[A-Za-z0-9._-]+/token=***/g' || fail=1
done
[ $fail = 0 ] && echo "eval-pipecat: all scenarios passed" || { echo "eval-pipecat: FAILED (server log: $OUT/server.log)" >&2; exit 1; }
