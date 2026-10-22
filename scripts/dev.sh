#!/usr/bin/env bash
# Local development stack and examples. Run through make:
#   make dev                                   server + admin UI, seeded with "Dev Assistant", a "Dealership desk" graph and test MCP servers
#   make dev-fresh                             the same stack in .govoice-dev-fresh/, wiped on every start and not seeded: no flows, so /onboarding is the first-run wizard
#   make example-web                           custom web client (needs `make dev` running)
#   make example-cli [TEXT="..."]              CLI call to the dev flow (voice clip, or a text turn)
#   make admin-link                            a magic link that signs the admin UI in (no pasting)
#   make example-go NAME=google_pipeline       standalone Go example (reads examples/.env)
#
# Provider keys come from examples/.env (see examples/.env.example): the development
# server loads it itself at start (the environment wins; a relative
# GOOGLE_APPLICATION_CREDENTIALS resolves against examples/), so nothing is stored.
# State lives in .govoice-dev/ (git-ignored): the SQLite flow store (created by the
# server), plus dev.env with the generated management key, token secret, ports and
# dev flow id. Delete the directory to start over.
# Ports: PUBLIC_PORT (8080), ADMIN_PORT (9090), UI_PORT (5173), CLIENT_PORT (3000).
#   public = the voice WebSocket and /health; admin = management API, metrics and debug, kept off the
#   public listener on purpose; UI = the vite dev server (it proxies /api and /ws); CLIENT = only
#   `make example-web`. dev.env remembers the last public and admin ports.
# A second `make dev` from the same checkout stops the first one (and its server and vite) before it
# starts. A default port held by anything else is skipped: the next free port is used and printed. A
# port you name (PUBLIC_PORT=... make dev) is exact: if held, the error names the process. Nothing
# outside this checkout's own stack is ever killed. If this script is killed without running its
# trap, a small guard stops the server and vite within a second, so no stack is left orphaned.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV_DIR="${GOVOICE_DEV_DIR:-$ROOT/.govoice-dev}"
DEV_ENV="$DEV_DIR/dev.env"
cd "$ROOT"

die() { echo "dev: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required ($2)"; }
port_free() { ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
port_holder() { lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {printf "%s (pid %s)", $1, $2}'; }
# A free port: <wanted> if nothing listens there, else (unless the user asked for exactly that port)
# the next free one above it, skipping the ports in <taken...>. Notes go to stderr: stdout is the port.
pick_port() { # <label> <wanted> <explicit 1|0> [taken...]
  local label="$1" want="$2" explicit="$3" p t busy; shift 3
  p="$want"
  while :; do
    busy=0
    for t in "$@"; do [[ "$t" == "$p" ]] && busy=1; done
    (( busy )) || port_free "$p" || busy=1
    if (( ! busy )); then
      [[ "$p" == "$want" ]] || echo "dev: $label port $want is in use by $(port_holder "$want"); using $p" >&2
      echo "$p"; return 0
    fi
    (( ! explicit )) || die "$label port $p is in use by $(port_holder "$p"); stop it or pick another (e.g. $(echo "$label" | tr a-z A-Z)_PORT=$((p + 10000)) make dev)"
    p=$((p + 1))
    (( p - want <= 100 )) || die "no free $label port within 100 of $want"
  done
}
any_alive() { local p; for p in "$@"; do kill -0 "$p" 2>/dev/null && return 0; done; return 1; }

# Pids of a dev stack started from this checkout: the dev.sh that owns it, its server and its
# vite. Matched by command line AND working directory, so another checkout's stack is left alone.
# This script, the make that started it and their descendants are never matched.
own_stack_pids() {
  local pid cwd
  # awk picks the candidates and drops this script, the make that started it and every process
  # descended from either (its own subshells match the same command line).
  for pid in $(ps -axo pid=,ppid=,command= | awk -v me="$$" -v mom="$PPID" -v dd="$DEV_DIR" '
      { par[$1] = $2; pid[NR] = $1; cmd[NR] = $0; sub(/^ *[0-9]+ +[0-9]+ +/, "", cmd[NR]) }
      function mine(p,  n) { while (p > 1 && n++ < 64) { if (p == me || p == mom) return 1; p = par[p] } return 0 }
      END {
        for (i = 1; i <= NR; i++) {
          c = cmd[i]
          # Whole-command-line matches only: a shell whose long -c text merely mentions these names
          # (an editor, a grep) is not a dev stack.
          srv = "/server.yaml"; want = dd srv
          if (c ~ /^(\/[^ ]*\/)?(bash|sh)( -c)? [^ ]*scripts\/dev\.sh up$/ \
              || (c ~ /^[^ ]*bin\/server -config / && substr(c, length(c) - length(want) + 1) == want) \
              || c ~ /^([^ ]*\/)?node [^ ]*vite\/bin\/vite\.js ui --host [^ ]+ --port [0-9]+ --strictPort$/)
            if (!mine(pid[i])) print pid[i]
        }
      }'); do
    cwd="$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p')"
    [[ "$cwd" == "$ROOT" ]] && echo "$pid"
  done
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
  export_urls
}
export_urls() {
  export GOVOICE_ADMIN_URL="http://127.0.0.1:$ADMIN_PORT"
  export GOVOICE_PUBLIC_URL="http://127.0.0.1:$PUBLIC_PORT"
  export GOVOICE_WS_URL="ws://127.0.0.1:$PUBLIC_PORT/ws"
}

server_up() { curl -fsS "$GOVOICE_ADMIN_URL/ready" >/dev/null 2>&1; }
require_server() { server_up || die "no dev server at $GOVOICE_ADMIN_URL; start it with 'make dev'"; }
require_flow() { [[ -n "${GOVOICE_DEV_FLOW_ID:-}" ]] || die "no dev flow yet: add provider keys to examples/.env and rerun 'make dev', or create one in the admin UI and set GOVOICE_DEV_FLOW_ID in ${DEV_ENV#"$ROOT"/}"; }

# Starts a background process with prefixed output; remembers its pid.
PIDS=()
spawn() { # <label> <cmd...>
  local label="$1"; shift
  "$@" > >(while IFS= read -r line; do echo "[$label] $line"; done) 2>&1 &
  PIDS+=("$!")
}
GUARD=""
# If this script dies without running its trap (kill -9, a vanished terminal), the server and vite
# would live on holding the ports. The guard outlives it just long enough to stop them, then exits;
# cleanup kills the guard first, so a normal stop leaves nothing behind.
start_guard() {
  local parent=$$
  local -a pids=("${PIDS[@]}")
  (
    while kill -0 "$parent" 2>/dev/null; do any_alive "${pids[@]}" || exit 0; sleep 1; done
    kill "${pids[@]}" 2>/dev/null || true
    for _ in 1 2 3 4 5; do any_alive "${pids[@]}" || exit 0; sleep 1; done
    kill -9 "${pids[@]}" 2>/dev/null || true
  ) </dev/null >/dev/null 2>&1 &
  GUARD=$!
}
cleanup() {
  [[ -z "$GUARD" ]] || { kill "$GUARD" 2>/dev/null || true; GUARD=""; }
  ((${#PIDS[@]})) || return 0
  echo; echo "dev: stopping"
  kill "${PIDS[@]}" 2>/dev/null || true
  wait "${PIDS[@]}" 2>/dev/null || true
  PIDS=()
}

cmd_up() {
  need go "https://go.dev/dl"; need node "Node 20+"; need curl "health checks"
  # Ports named on the command line are exact; otherwise 8080 / 9090 / 5173, moved up to the next
  # free port when something else holds them (dev.env's remembered ports are not preferences).
  local want_public="${PUBLIC_PORT:-}" want_admin="${ADMIN_PORT:-}" want_ui="${UI_PORT:-}"
  PUBLIC_PORT_OVERRIDE="$want_public" ADMIN_PORT_OVERRIDE="$want_admin" load_env
  stop_previous_stack
  PUBLIC_PORT="$(pick_port public "${want_public:-8080}" "$([[ -n "$want_public" ]] && echo 1 || echo 0)")"
  ADMIN_PORT="$(pick_port admin "${want_admin:-9090}" "$([[ -n "$want_admin" ]] && echo 1 || echo 0)" "$PUBLIC_PORT")"
  UI_PORT="$(pick_port ui "${want_ui:-5173}" "$([[ -n "$want_ui" ]] && echo 1 || echo 0)" "$PUBLIC_PORT" "$ADMIN_PORT")"
  export_urls
  set_env PUBLIC_PORT "$PUBLIC_PORT"; set_env ADMIN_PORT "$ADMIN_PORT"

  make build >/dev/null || die "build failed (run 'make build' to see why)"
  go build -o bin/mcptest ./cmd/mcptest || die "mcptest build failed"
  MCPTEST_PORT="$(pick_port mcptest "${MCPTEST_PORT:-18765}" 0 "$PUBLIC_PORT" "$ADMIN_PORT" "$UI_PORT")"
  MCPTEST_HOLE_PORT="$(pick_port mcptest-blackhole "$((MCPTEST_PORT + 1))" 0 "$PUBLIC_PORT" "$ADMIN_PORT" "$UI_PORT" "$MCPTEST_PORT")"
  export MCPTEST_URL="http://127.0.0.1:$MCPTEST_PORT"
  # The test MCP servers' token; the dev server reads it like any MCP_TOKEN_* variable.
  export MCPTEST_TOKEN="${MCPTEST_TOKEN:-test-token}" MCP_TOKEN_TEST="${MCP_TOKEN_TEST:-test-token}"
  npm_ready "$ROOT/ui"
  sed -e "s|^public_listen_addr:.*|public_listen_addr: \"127.0.0.1:$PUBLIC_PORT\"|" \
      -e "s|^admin_listen_addr:.*|admin_listen_addr: \"127.0.0.1:$ADMIN_PORT\"|" \
      config/development.yaml > "$DEV_DIR/server.yaml"

  trap cleanup EXIT
  trap 'cleanup; exit 0' INT TERM HUP # Ctrl-C is the normal way to stop; HUP is a closed terminal
  GOVOICE_DEV_DIR="$DEV_DIR" spawn server ./bin/server -config "$DEV_DIR/server.yaml"
  for _ in $(seq 1 100); do server_up && break; kill -0 "${PIDS[0]}" 2>/dev/null || die "server exited (see [server] output)"; sleep 0.2; done
  server_up || die "server did not become ready"
  spawn mcptest ./bin/mcptest -addr "127.0.0.1:$MCPTEST_PORT" -blackhole-addr "127.0.0.1:$MCPTEST_HOLE_PORT"
  for _ in $(seq 1 50); do curl -fsS "$MCPTEST_URL/_record" >/dev/null 2>&1 && break; sleep 0.1; done

  # GOVOICE_DEV_SEED=0 (make dev-fresh) starts with no flows, so /onboarding opens the first-run wizard.
  if [[ "${GOVOICE_DEV_SEED:-1}" == "0" ]]; then
    echo "dev: not seeding (GOVOICE_DEV_SEED=0): open /onboarding for the first-run wizard"
  elif flow_id="$(node scripts/dev-seed.mjs)"; then
    [[ -n "$flow_id" ]] && set_env GOVOICE_DEV_FLOW_ID "$flow_id" && GOVOICE_DEV_FLOW_ID="$flow_id"
  else
    echo "dev: seeding failed (see [seed] output); the stack still runs" >&2
  fi

  spawn ui node ui/node_modules/vite/bin/vite.js ui --host 127.0.0.1 --port "$UI_PORT" --strictPort
  start_guard

  cat <<EOF

  Switchboard dev stack
    Admin UI        http://127.0.0.1:$UI_PORT   (signed in by the dev proxy: nothing to paste)
    Management key  $GOVOICE_MANAGEMENT_API_KEY   (for curl and the API; the admin UI does not need it)
    Admin API       $GOVOICE_ADMIN_URL
    Voice WebSocket $GOVOICE_WS_URL
    Dev flow        ${GOVOICE_DEV_FLOW_ID:-none (see [seed] above)}
    Data            ${DEV_DIR#"$ROOT"/}/  (SQLite store; the key is in dev.env)
    Test MCP        $MCPTEST_URL (/ok /open /fail/tools /fail/auth /fail/list /slow; black hole :$MCPTEST_HOLE_PORT)

  In another terminal: make example-web | make example-cli TEXT="Hello" | make example-go NAME=google_pipeline
  Ctrl-C stops everything.

EOF
  wait "${PIDS[0]}"
}

cmd_web() {
  local want_client="${CLIENT_PORT:-}"
  load_env; require_server; need node "Node 20+"
  CLIENT_PORT="$(pick_port client "${want_client:-3000}" "$([[ -n "$want_client" ]] && echo 1 || echo 0)")"
  npm_ready "$ROOT/examples/rtvi_client"
  echo "dev: custom web client at http://127.0.0.1:$CLIENT_PORT (flow and keys prefilled by the server)"
  cd examples/rtvi_client
  PORT="$CLIENT_PORT" HOST=127.0.0.1 GOVOICE_FLOW_ID="${GOVOICE_DEV_FLOW_ID:-}" exec node server.js
}

cmd_cli() {
  load_env; require_server; require_flow; need node "Node 20+"
  npm_ready "$ROOT/examples/rtvi_client"
  local args=(--flow "$GOVOICE_DEV_FLOW_ID" --admin "$GOVOICE_ADMIN_URL" --ws "$GOVOICE_WS_URL")
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
  link) load_env; "$ROOT/scripts/admin-link.sh" "http://127.0.0.1:$UI_PORT" ;;
  go) cmd_go "${2:-}" ;;
  *) die "unknown command '$1' (up | web | cli | link | go <name>)" ;;
esac
