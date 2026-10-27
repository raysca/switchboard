#!/usr/bin/env bash
# Local prerequisite check for ./dev doctor. Read-only: it never starts anything or
# prints a secret value.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

fail=0
say() { printf '%-24s %s\n' "$1" "$2"; }
good() { say "$1" "ok${2:+ ($2)}"; }
bad() { say "$1" "MISSING${2:+ ($2)}"; fail=$((fail + 1)); }
warn() { say "$1" "warn${2:+ ($2)}"; }

check_cmd() { # <label> <command> <required 1|0> [version args...]
  local label="$1" cmd="$2" required="$3"; shift 3
  if command -v "$cmd" >/dev/null 2>&1; then
    local version
    version="$("$cmd" "$@" 2>/dev/null | head -1 || true)"
    good "$label" "${version:-installed}"
  elif [[ "$required" == 1 ]]; then
    bad "$label" "needed for the dev stack or build"
  else
    warn "$label" "optional for this checkout"
  fi
}

echo "Switchboard doctor (read-only)"
echo
check_cmd "just" "$ROOT/.tools/bin/just" 1 --version
check_cmd "Go" go 1 version
check_cmd "Node" node 1 --version
check_cmd "npm" npm 1 --version
check_cmd "make" make 1 --version
check_cmd "lsof" lsof 1 -v
check_cmd "Docker" docker 0 --version
check_cmd "GitHub CLI" gh 0 --version
check_cmd "curl" curl 1 --version
check_cmd "openssl" openssl 1 version

if "$ROOT/scripts/fetch-ort.sh" check >/dev/null 2>&1; then
  good "ONNX Runtime" "make ort already fetched the library"
else
  bad "ONNX Runtime" "run: make ort"
fi
if [[ -x "$ROOT/.tools/bin/just" ]]; then
  have="$("$ROOT/.tools/bin/just" --version 2>/dev/null || true)"
  if [[ "$have" == "just 1.59.0" ]]; then
    good "pinned just" "1.59.0"
  else
    bad "pinned just" "got '${have:-no version}'; run: make init"
  fi
fi
if [[ -f "$ROOT/third_party/silero_vad.onnx" ]]; then
  good "ONNX VAD model" "third_party/silero_vad.onnx"
else
  bad "ONNX VAD model" "tracked model is missing: run: make ort"
fi
if [[ -f "$ROOT/examples/.env" ]]; then
  good "provider config" "examples/.env"
else
  warn "provider config" "examples/.env missing; free echo checks still run"
fi
if [[ -d "$ROOT/ui/node_modules" ]]; then
  good "UI dependencies" "ui/node_modules"
else
  warn "UI dependencies" "run: npm ci --prefix ui"
fi
if [[ -d "$ROOT/e2e/node_modules" ]]; then
  good "e2e dependencies" "e2e/node_modules"
else
  warn "e2e dependencies" "make e2e-echo installs them"
fi

echo
echo "Ports (the dev stack moves to the next free port unless you name one):"
public_port=8080 admin_port=9090 ui_port=5173
if [[ -f "${GOVOICE_DEV_DIR:-$ROOT/.govoice-dev}/dev.env" ]]; then
  # The last stack's remembered ports; values only, never secrets.
  # shellcheck disable=SC1090
  set -a; . "${GOVOICE_DEV_DIR:-$ROOT/.govoice-dev}/dev.env"; set +a
  public_port="${PUBLIC_PORT:-$public_port}"
  admin_port="${ADMIN_PORT:-$admin_port}"
  ui_port="${UI_PORT:-$ui_port}"
fi
for spec in "public:$public_port" "admin:$admin_port" "ui:$ui_port"; do
  label="${spec%%:*}"; port="${spec##*:}"
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    upper="$(printf '%s' "$label" | tr '[:lower:]' '[:upper:]')"
    say "$label :$port" "in use (the stack will pick another port unless you set ${upper}_PORT)"
  else
    say "$label :$port" "free"
  fi
done

echo
if [[ "$fail" -gt 0 ]]; then
  echo "$fail required prerequisite(s) missing; fix them before ./dev up or ./dev test."
  exit 1
fi
echo "All required prerequisites are present."
