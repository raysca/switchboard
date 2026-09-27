#!/usr/bin/env bash
# Start the development voice server and the browser client that talks to it.
# Stops anything already listening on the server or client ports first.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLIENT="$ROOT/examples/rtvi_client"
SERVER_PID=""

stop_port() {
  local port="$1"
  local pids
  pids="$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi
  echo "Stopping listener on port ${port} (pid ${pids//$'\n'/, })"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  sleep 0.4
  pids="$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -n "$pids" ]]; then
    # shellcheck disable=SC2086
    kill -9 $pids 2>/dev/null || true
  fi
}

cleanup() {
  if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
    echo "Stopping voice server ${SERVER_PID}"
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

stop_port 8080
stop_port 9090
stop_port 3000

cd "${ROOT}"
make build
./bin/server -config config/development.yaml &
SERVER_PID=$!

ready=0
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:8080/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    echo "Voice server exited before /health responded"
    exit 1
  fi
  sleep 0.2
done
if [[ "${ready}" -ne 1 ]]; then
  echo "Voice server did not become healthy on 127.0.0.1:8080"
  exit 1
fi

echo "Voice server is up at ws://127.0.0.1:8080/ws"
echo "Admin readiness: curl -s http://127.0.0.1:9090/ready"
echo "Opening the browser client at http://localhost:3000"

cd "${CLIENT}"
if [[ ! -d node_modules ]]; then
  npm install
fi
npm start
