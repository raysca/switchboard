#!/usr/bin/env bash
# Local development stack and examples. Run through make:
#   make dev                                   server + admin UI, seeded with "Dev Assistant" and a "Dealership desk" graph
#   make example-web                           custom web client (needs `make dev` running)
#   make example-cli [TEXT="..."]              CLI call to the dev agent (voice clip, or a text turn)
#   make example-go NAME=google_pipeline       standalone Go example (reads examples/.env)
#
# Provider keys come from examples/.env (see examples/.env.example): the development
# server loads it itself at start (the environment wins; a relative
# GOOGLE_APPLICATION_CREDENTIALS resolves against examples/), so nothing is stored.
# State lives in .govoice-dev/ (git-ignored): the SQLite agent store (created by the
# server), plus dev.env with the generated management key, token secret, ports and
# dev agent id. Delete the directory to start over.
# Ports: PUBLIC_PORT (8080), ADMIN_PORT (9090), UI_PORT (5173), CLIENT_PORT (3000).
#   public = the voice WebSocket and /health; admin = management API, metrics and debug, kept off the
#   public listener on purpose; UI = the vite dev server (it proxies /api and /ws); CLIENT = only
#   `make example-web`. dev.env remembers the last public and admin ports.
# A second `make dev` from the same checkout stops the first one (and its server and vite) before it
# starts. A port held by anything else is never killed: the error names the process instead.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV_DIR="${GOVOICE_DEV_DIR:-$ROOT/.govoice-dev}"
DEV_ENV="$DEV_DIR/dev.env"
cd "$ROOT"

die() { echo "dev: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required ($2)"; }
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
port_holder() { lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {printf "%s (pid %s)", $1, $2}'; }
any_alive() { local p; for p in "$@"; do kill -0 "$p" 2>/dev/null && return 0; done; return 1; }

# Pids of a dev stack started from this checkout: the dev.sh that owns it, its server and its
# vite. Matched by command line AND working directory, so another checkout's stack is left alone.
# This script, its subshells and the make that started it are never matched.
own_stack_pids() {
  local pid ppid cmd cwd
  while read -r pid ppid cmd; do
    [[ "$pid" != "$$" && "$ppid" != "$$" && "$pid" != "$PPID" ]] || continue
    case "$cmd" in
      *"scripts/dev.sh up"*|*"bin/server -config $DEV_DIR/server.yaml"*|*"vite/bin/vite.js ui --host"*) ;;
      *) continue ;;
    esac
    cwd="$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p')"
    [[ "$cwd" == "$ROOT" ]] && echo "$pid"
  done < <(ps -axo pid=,ppid=,command=)
  return 0
}

# A second `make dev` replaces the first instead of failing on its ports. dev.sh traps TERM and
# stops its own server and vite; anything still alive after a few seconds is killed.
stop_previous_stack() {
  local pids
  pids="$(own_stack_pids | sort -un | tr '\n' ' ')"
  [[ -n "${pids// }" ]] || return 0
  echo "dev: stopping the previous dev stack (pids ${pids% })"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  local _
  for _ in $(seq 1 30); do
    # shellcheck disable=SC2086
    any_alive $pids || return 0
    sleep 0.2
  done
  # shellcheck disable=SC2086
  kill -9 $pids 2>/dev/null || true
  sleep 0.3
}
set_env() { # <name> <value>: write or replace one line in dev.env (kept 0600)
  (umask 077
   grep -v "^$1=" "$DEV_ENV" > "$DEV_ENV.tmp" 2>/dev/null || true
   echo "$1=$2" >> "$DEV_ENV.tmp" && mv "$DEV_ENV.tmp" "$DEV_ENV")
}
npm_ready() { [[ -d "$1/node_modules" ]] || (echo "dev: npm install in ${1#"$ROOT"/}" && cd "$1" && npm install --no-audit --no-fund >/dev/null); }

load_env() {
  mkdir -p "$DEV_DIR" && chmod 700 "$DEV_DIR"
  if [[ ! -f "$DEV_ENV" ]]; then
    need openssl "to generate dev secrets"
    (umask 077
     echo "GOVOICE_MANAGEMENT_API_KEY=$(openssl rand -hex 32)" > "$DEV_ENV"
     echo "TOKEN_SECRET=$(openssl rand -hex 32)" >> "$DEV_ENV")
    echo "dev: generated secrets in ${DEV_ENV#"$ROOT"/}"
  fi
  set -a; source "$DEV_ENV"; set +a
  # Environment wins over dev.env; dev.env remembers the last ports used.
  PUBLIC_PORT="${PUBLIC_PORT_OVERRIDE:-${PUBLIC_PORT:-8080}}"
  ADMIN_PORT="${ADMIN_PORT_OVERRIDE:-${ADMIN_PORT:-9090}}"
  UI_PORT="${UI_PORT:-5173}"
  CLIENT_PORT="${CLIENT_PORT:-3000}"
  export GOVOICE_ADMIN_URL="http://127.0.0.1:$ADMIN_PORT"
  export GOVOICE_PUBLIC_URL="http://127.0.0.1:$PUBLIC_PORT"
  export GOVOICE_WS_URL="ws://127.0.0.1:$PUBLIC_PORT/ws"
}

server_up() { curl -fsS "$GOVOICE_ADMIN_URL/ready" >/dev/null 2>&1; }
require_server() { server_up || die "no dev server at $GOVOICE_ADMIN_URL; start it with 'make dev'"; }
require_agent() { [[ -n "${GOVOICE_DEV_AGENT_ID:-}" ]] || die "no dev agent yet: add provider keys to examples/.env and rerun 'make dev', or create one in the admin UI and set GOVOICE_DEV_AGENT_ID in ${DEV_ENV#"$ROOT"/}"; }

# Starts a background process with prefixed output; remembers its pid.
PIDS=()
spawn() { # <label> <cmd...>
  local label="$1"; shift
  "$@" > >(while IFS= read -r line; do echo "[$label] $line"; done) 2>&1 &
  PIDS+=("$!")
}
cleanup() {
  ((${#PIDS[@]})) || return 0
  echo; echo "dev: stopping"
  kill "${PIDS[@]}" 2>/dev/null || true
  wait "${PIDS[@]}" 2>/dev/null || true
  PIDS=()
}

cmd_up() {
  need go "https://go.dev/dl"; need node "Node 20+"; need curl "health checks"
  PUBLIC_PORT_OVERRIDE="${PUBLIC_PORT:-}" ADMIN_PORT_OVERRIDE="${ADMIN_PORT:-}" load_env
  stop_previous_stack
  for p in "$PUBLIC_PORT" "$ADMIN_PORT" "$UI_PORT"; do
    port_free "$p" || die "port $p is in use by $(port_holder "$p"), which is not a dev stack from this checkout; stop it or pick others, e.g. PUBLIC_PORT=18080 ADMIN_PORT=19090 UI_PORT=15173 make dev"
  done
  set_env PUBLIC_PORT "$PUBLIC_PORT"; set_env ADMIN_PORT "$ADMIN_PORT"

  make build >/dev/null || die "build failed (run 'make build' to see why)"
  npm_ready "$ROOT/ui"
  sed -e "s|^public_listen_addr:.*|public_listen_addr: \"127.0.0.1:$PUBLIC_PORT\"|" \
      -e "s|^admin_listen_addr:.*|admin_listen_addr: \"127.0.0.1:$ADMIN_PORT\"|" \
      config/development.yaml > "$DEV_DIR/server.yaml"

  trap cleanup EXIT
  trap 'cleanup; exit 0' INT TERM HUP # Ctrl-C is the normal way to stop; HUP is a closed terminal
  GOVOICE_DEV_DIR="$DEV_DIR" spawn server ./bin/server -config "$DEV_DIR/server.yaml"
  for _ in $(seq 1 100); do server_up && break; kill -0 "${PIDS[0]}" 2>/dev/null || die "server exited (see [server] output)"; sleep 0.2; done
  server_up || die "server did not become ready"

  if agent_id="$(node scripts/dev-seed.mjs)"; then
    [[ -n "$agent_id" ]] && set_env GOVOICE_DEV_AGENT_ID "$agent_id" && GOVOICE_DEV_AGENT_ID="$agent_id"
  else
    echo "dev: seeding failed (see [seed] output); the stack still runs" >&2
  fi

  spawn ui node ui/node_modules/vite/bin/vite.js ui --host 127.0.0.1 --port "$UI_PORT" --strictPort

  cat <<EOF

  Switchboard dev stack
    Admin UI        http://127.0.0.1:$UI_PORT   (management key: grep MANAGEMENT ${DEV_ENV#"$ROOT"/})
    Admin API       $GOVOICE_ADMIN_URL
    Voice WebSocket $GOVOICE_WS_URL
    Dev agent       ${GOVOICE_DEV_AGENT_ID:-none (see [seed] above)}
    Data            ${DEV_DIR#"$ROOT"/}/  (SQLite store, dev.env)

  In another terminal: make example-web | make example-cli TEXT="Hello" | make example-go NAME=google_pipeline
  Ctrl-C stops everything.

EOF
  wait "${PIDS[0]}"
}

cmd_web() {
  load_env; require_server; need node "Node 20+"
  port_free "$CLIENT_PORT" || die "port $CLIENT_PORT is in use; set CLIENT_PORT"
  npm_ready "$ROOT/examples/rtvi_client"
  echo "dev: custom web client at http://127.0.0.1:$CLIENT_PORT (agent and keys prefilled by the server)"
  cd examples/rtvi_client
  PORT="$CLIENT_PORT" HOST=127.0.0.1 GOVOICE_AGENT_ID="${GOVOICE_DEV_AGENT_ID:-}" exec node server.js
}

cmd_cli() {
  load_env; require_server; require_agent; need node "Node 20+"
  npm_ready "$ROOT/examples/rtvi_client"
  local args=(--agent "$GOVOICE_DEV_AGENT_ID" --admin "$GOVOICE_ADMIN_URL" --ws "$GOVOICE_WS_URL")
  [[ -n "${TEXT:-}" ]] && args+=(--text "$TEXT")
  cd examples/rtvi_client && exec node cli.js "${args[@]}"
}

cmd_go() {
  local name="${1:-}"
  local list; list="$(cd examples && for d in */; do [[ -f "$d/main.go" ]] && printf ' %s' "${d%/}"; done)"
  [[ -n "$name" && -f "examples/$name/main.go" ]] || die "usage: make example-go NAME=<example>; one of:$list"
  exec go run "./examples/$name"
}

case "${1:-up}" in
  up) cmd_up ;;
  web) cmd_web ;;
  cli) cmd_cli ;;
  go) cmd_go "${2:-}" ;;
  *) die "unknown command '$1' (up | web | cli | go <name>)" ;;
esac
