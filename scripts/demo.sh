#!/usr/bin/env bash
# Demo lifecycle for ./dev. Thin wrapper over the Makefile targets and demos/seed.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_DIR="${GOVOICE_DEV_DIR:-$ROOT/.govoice-dev}"
DEV_ENV="$DEV_DIR/dev.env"
cd "$ROOT"
name="${2:-}"

die() { echo "dev demo: $*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage: ./dev demo-<action> <demo>

actions:
  list              list the demos
  start <demo>      run the demo MCP fixture in the foreground
  seed <demo>       wipe and create the demo in the running dev stack
  reset <demo>      alias of seed (wipe and recreate)
  wipe <demo>       remove the demo from the running dev stack

demos: dental, atlas
EOF
}

case "${1:-}" in
  list)
    for d in "$ROOT"/demos/*/README.md; do
      [[ -f "$d" ]] || continue
      printf '  %-8s %s\n' "$(basename "$(dirname "$d")")" "$(sed -n 's/^# //p' "$d" | head -1)"
    done
    echo
    echo "start: ./dev demo-start <demo>   seed: ./dev demo-seed <demo>"
    exit 0
    ;;
  start|seed|reset|wipe) ;;
  *) usage; exit 2 ;;
esac

[[ -n "$name" ]] || die "missing demo name (dental or atlas)"
[[ -f "$ROOT/demos/$name/README.md" ]] || die "unknown demo '$name' (dental or atlas)"

fixture_port() {
  # Test hook: check-tasks uses a guaranteed-closed port so its probe cannot seed
  # (or wipe) a developer's real demo.
  if [[ -n "${GOVOICE_DEMO_FIXTURE_PORT:-}" ]]; then
    echo "$GOVOICE_DEMO_FIXTURE_PORT"
    return
  fi
  case "$1" in
    dental) echo 18767 ;;
    atlas) echo 18768 ;;
  esac
}

# The fixture must answer before seed starts: a seed whose MCP server is unreachable
# wipes the working demo and then fails during create.
require_fixture() {
  local port; port="$(fixture_port "$name")"
  curl -fsS --max-time 2 "http://127.0.0.1:${port}/health" >/dev/null 2>&1 ||
    die "the $name MCP fixture is not running on 127.0.0.1:${port}; start it with ./dev demo-start $name"
}

# The dev stack is loopback-only. Ignore an exported GOVOICE_ADMIN_URL so a production
# URL in the developer's shell can never receive a destructive demo command.
require_dev_env() {
  [[ -f "$DEV_ENV" ]] || die "no dev stack: $DEV_ENV is missing (run ./dev up first)"
  # shellcheck disable=SC1090
  set -a; . "$DEV_ENV"; set +a
  admin="http://127.0.0.1:${ADMIN_PORT:-9090}"
  curl -fsS --max-time 2 "$admin/ready" >/dev/null 2>&1 ||
    die "the dev server is not ready at $admin; start it with ./dev up"
}

case "$1" in
  start)
    exec make -C "$ROOT" "${name}-mcp"
    ;;
  seed|reset)
    require_fixture
    require_dev_env
    exec env GOVOICE_ADMIN_URL="$admin" \
      DENTAL_MCP_TOKEN="${DENTAL_MCP_TOKEN:-dev-dental-token}" \
      ATLAS_MCP_TOKEN="${ATLAS_MCP_TOKEN:-dev-atlas-token}" \
      make -C "$ROOT" seed DEMO="$name"
    ;;
  wipe)
    require_dev_env
    exec env GOVOICE_ADMIN_URL="$admin" \
      DENTAL_MCP_TOKEN="${DENTAL_MCP_TOKEN:-dev-dental-token}" \
      ATLAS_MCP_TOKEN="${ATLAS_MCP_TOKEN:-dev-atlas-token}" \
      go run ./demos/seed --server "$admin" --demo "$name" --wipe-only --yes
    ;;
esac
