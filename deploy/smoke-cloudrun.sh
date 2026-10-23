#!/usr/bin/env bash
# Smoke test of a deployed Cloud Run revision. Costs nothing: no call is placed, no provider is called.
# Usage: GOVOICE_MANAGEMENT_API_KEY=... deploy/smoke-cloudrun.sh https://switchboard-xyz.a.run.app
# (the variable is the one the server reads; the deployed secret behind it is SWITCHBOARD_MANAGEMENT_API_KEY)
# SMOKE_LOCAL=1 downgrades the two checks that need a real public proxy chain (https seen, public client_ip)
# to warnings, for the local rehearsal (deploy/rehearsal).
set -euo pipefail
base="${1:?usage: smoke-cloudrun.sh <base-url>}"
base="${base%/}"
: "${GOVOICE_MANAGEMENT_API_KEY:?GOVOICE_MANAGEMENT_API_KEY is required}"
command -v jq >/dev/null || { echo "smoke: jq is required" >&2; exit 2; }
fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
warn() { echo "SMOKE WARN: $*" >&2; }
# soft is fail on a real deployment, a warning in the local rehearsal.
soft() { if [[ -n "${SMOKE_LOCAL:-}" ]]; then warn "$* (ignored: SMOKE_LOCAL)"; else fail "$*"; fi; }
api() { curl -fsS --max-time 20 -H "Authorization: Bearer ${GOVOICE_MANAGEMENT_API_KEY}" "${base}$1"; }

# 1. alive, and the UI is the embedded one
for _ in $(seq 1 30); do curl -fsS --max-time 5 "${base}/health" >/dev/null 2>&1 && break || sleep 2; done
curl -fsS "${base}/health" | jq -e '.status == "live"' >/dev/null || fail "/health is not live"
page="$(curl -fsS "${base}/")" || fail "GET / failed"
[[ "$page" == *'<div id="root"'* ]] || fail "the admin UI is not served at /"
page="$(curl -fsS "${base}/flows")" || fail "GET /flows failed"
[[ "$page" == *'<div id="root"'* ]] || fail "a client route does not fall back to the UI"

# 2. the operational surfaces are not on the public port
for p in /metrics /ready /debug/timeline; do
  code="$(curl -s -o /dev/null -w '%{http_code}' "${base}${p}")"
  [[ "$code" != 200 ]] || fail "${p} is served on the public port"
done

# 3. the management API is behind the key, and the proxy is trusted correctly
[[ "$(curl -s -o /dev/null -w '%{http_code}' "${base}/api/v1/server-config")" == 401 ]] || fail "the management API answers without a key"
cfg="$(api /api/v1/server-config)"
echo "$cfg" | jq -e '.data.secure == true' >/dev/null || soft "the server does not see the request as https: check tls.trusted_proxies against the real peer"
ip="$(echo "$cfg" | jq -r '.data.client_ip')"
if [[ ! "$ip" =~ ^[0-9a-fA-F:.]+$ ]] || [[ "$ip" =~ ^(10\.|127\.|169\.254\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.) ]]; then
  soft "client_ip is '${ip}': the proxy chain is trusted wrongly (the per-IP limits would see one shared client)"
fi
echo "client_ip ${ip}"

# 4. providers: the three the demo uses are detected
prov="$(api /api/v1/providers)"
for id in google xai elevenlabs; do
  echo "$prov" | jq -e --arg id "$id" '.data[] | select(.id == $id and .detected == true)' >/dev/null || fail "provider ${id} is not detected (missing secret?)"
done

# 5. the demo flow was installed by the server and is ready (the install runs in the background after start)
ready=""
for _ in $(seq 1 30); do
  ready="$(api /api/v1/agents | jq -r '.data[] | select(.name == "Smile Dental front desk") | .readiness.state' || true)"
  [[ "$ready" == "ready" ]] && break
  sleep 2
done
[[ "$ready" == "ready" ]] || fail "the flow 'Smile Dental front desk' is not ready (state: ${ready:-missing})"

# 6b. (after the wait: the same background install discovers the tools) the dentistry MCP server is registered, has its token, and its tools were discovered
mcp="$(api /api/v1/mcp-servers)"
echo "$mcp" | jq -e '.data[] | select(.name == "Smile Dental scheduling" and .auth_env_set == true and ((.latest_discovery_id // "") != ""))' >/dev/null ||
  fail "the dental MCP server is not registered with its token and discovered tools (demo.auto_install, DENTAL_MCP_URL, MCP_TOKEN_DENTAL)"

# 7. wrong keys are refused, then throttled (a correct key never is)
for _ in $(seq 1 10); do curl -s -o /dev/null -H "Authorization: Bearer wrong" "${base}/api/v1/server-config"; done
curl -s -H "Authorization: Bearer wrong" "${base}/api/v1/server-config" | grep -q too_many_attempts || fail "failed logins are not throttled"
api /api/v1/server-config >/dev/null || fail "the correct key was refused while throttled"
echo "SMOKE OK ${base}"
