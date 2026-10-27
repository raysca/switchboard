#!/usr/bin/env bash
# Deployment helpers for ./dev. The real deployments are GitHub Actions; this script
# dispatches them, or calls the local Cloudflare deploy. It never reads a secret value.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

die() { echo "dev deploy: $*" >&2; exit 2; }
usage() {
  cat <<'EOF'
usage:
  ./dev deploy-server cloudrun [--min-instances N]
  ./dev deploy-server cloudflare
  ./dev deploy-demo <dental|atlas> [cloudrun|cloudflare] \
      --server URL --environment NAME [--mcp-base-url URL] \
      [--management-key-secret NAME] [--no-seed] [--image-tag TAG]
  ./dev deploy-status
EOF
}

dispatch() { # <gh args...>; prints the exact command when gh is unavailable
  if ! command -v gh >/dev/null 2>&1; then
    echo "dev deploy: GitHub CLI (gh) is not installed; install https://cli.github.com" >&2
    printf 'when it is installed, run:\n  gh' >&2
    printf ' %q' "$@" >&2
    printf '\n' >&2
    exit 2
  fi
  if ! gh auth status >/dev/null 2>&1; then
    echo "dev deploy: gh is not authenticated (run: gh auth login)" >&2
    printf 'then run:\n  gh' >&2
    printf ' %q' "$@" >&2
    printf '\n' >&2
    exit 2
  fi
  gh "$@"
}

server_deploy() {
  target="cloudrun"
  if [[ $# -gt 0 && "$1" != -* ]]; then
    target="$1"
    shift
  fi
  case "$target" in
    cloudrun)
      args=(workflow run deploy.yml --ref main)
      while [[ $# -gt 0 ]]; do
        case "$1" in
          --min-instances)
            [[ $# -ge 2 ]] || die "--min-instances needs a value"
            args+=(-f "min_instances=$2"); shift 2 ;;
          *) die "unknown server deploy option '$1'" ;;
        esac
      done
      dispatch "${args[@]}"
      echo "dispatched deploy.yml on main (watch: ./dev deploy-status)"
      ;;
    cloudflare)
      exec make -C "$ROOT" deploy-cloudflare
      ;;
    *) die "unknown target '$target' (cloudrun or cloudflare)" ;;
  esac
}

demo_deploy() {
  name="${1:-}"
  [[ -n "$name" ]] || { usage; exit 2; }
  shift
  target="cloudrun"
  if [[ $# -gt 0 && ( "$1" == "cloudrun" || "$1" == "cloudflare" ) ]]; then
    target="$1"
    shift
  fi
  [[ -f "$ROOT/demos/$name/README.md" ]] || die "unknown demo '$name' (dental or atlas)"

  server=""; environment=""; mcp_base=""; key_secret="SWITCHBOARD_MANAGEMENT_API_KEY"
  auto_seed=true; image_tag=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --server) [[ $# -ge 2 ]] || die "--server needs a URL"; server="$2"; shift 2 ;;
      --environment) [[ $# -ge 2 ]] || die "--environment needs a name"; environment="$2"; shift 2 ;;
      --mcp-base-url) [[ $# -ge 2 ]] || die "--mcp-base-url needs a URL"; mcp_base="$2"; shift 2 ;;
      --management-key-secret) [[ $# -ge 2 ]] || die "--management-key-secret needs a name"; key_secret="$2"; shift 2 ;;
      --no-seed) auto_seed=false; shift ;;
      --image-tag) [[ $# -ge 2 ]] || die "--image-tag needs a value"; image_tag="$2"; shift 2 ;;
      *) die "unknown demo deploy option '$1'" ;;
    esac
  done
  [[ -n "$server" ]] || die "--server is required (the clean-room server to seed)"
  [[ -n "$environment" ]] || die "--environment is required (the GitHub environment with the demo token)"
  if [[ "$target" == "cloudflare" && -z "$mcp_base" ]]; then
    die "--mcp-base-url is required for Cloudflare: the public URL of the deployed demo Worker"
  fi
  args=(workflow run deploy-demo-dispatch.yml --ref main
        -f "demo=$name" -f "target=$target" -f "environment=$environment"
        -f "server_admin_url=$server" -f "management_key_secret=$key_secret"
        -f "auto_seed=$auto_seed")
  [[ -z "$mcp_base" ]] || args+=(-f "mcp_base_url=$mcp_base")
  [[ -z "$image_tag" ]] || args+=(-f "image_tag=$image_tag")
  dispatch "${args[@]}"
  echo "dispatched deploy-demo-dispatch.yml for ${name}/${target} (watch: ./dev deploy-status)"
  echo "the demo token secret and management key are read by the workflow, never by this command"
}

deploy_status() {
  if ! command -v gh >/dev/null 2>&1; then
    die "gh is required to list deployment runs; install https://cli.github.com"
  fi
  gh auth status >/dev/null 2>&1 || die "gh is not authenticated (run: gh auth login)"
  echo "== server =="
  gh run list --workflow deploy.yml --limit 3
  echo
  echo "== demo =="
  gh run list --workflow deploy-demo-dispatch.yml --limit 3
}

case "${1:-}" in
  server) shift; server_deploy "$@" ;;
  demo) shift; demo_deploy "$@" ;;
  status) shift; deploy_status ;;
  ""|help|--help|-h) usage ;;
  *) usage; exit 2 ;;
esac
