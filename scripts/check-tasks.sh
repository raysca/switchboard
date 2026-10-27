#!/usr/bin/env bash
# Validates the ./dev front door: the justfile parses, every public recipe has a
# description, the required journeys exist and the help runbooks are reachable.
# Run after `make init`; CI runs it in the test job.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
DEV="$ROOT/dev"

fail=0
require() {
  local name="$1"
  if grep -qE "^    ${name}( |$)" <<<"$list"; then
    return 0
  fi
  echo "check-tasks: missing recipe '$name'" >&2
  fail=1
}

[[ -x "$DEV" ]] || { echo "check-tasks: ./dev is missing or not executable" >&2; exit 1; }
[[ -x "$ROOT/.tools/bin/just" ]] || { echo "check-tasks: run 'make init' first (.tools/bin/just is missing)" >&2; exit 1; }

list="$("$DEV" --list)"
[[ -n "$list" ]] || { echo "check-tasks: ./dev --list printed nothing" >&2; exit 1; }

probe="$(mktemp -d)"
trap 'rm -rf "$probe"' EXIT

# Every public recipe must carry a doc comment; that is what makes ./dev --list a menu.
while IFS= read -r line; do
  [[ "$line" =~ ^\ \ \ \ [a-z] ]] || continue
  if [[ "$line" != *" # "* ]]; then
    echo "check-tasks: recipe has no description: $line" >&2
    fail=1
  fi
done <<<"$list"

for recipe in \
  default help up up-fresh down status doctor admin-link example-web example-cli example-go \
  build ui-embed ort proto-gen test test-go test-e2e test-browser test-oneport test-postgres \
  verify fmt check-tasks deadcode demo-boundary release-security mcp-test e2e e2e-xai \
  demo-list demo-start demo-seed demo-reset demo-wipe seed \
  deploy-server deploy-demo deploy-status \
  eval-agents eval-agents-echo eval-agents-live eval-pipecat eval-turn parity-regen \
  live-list matrix-plan live-check agent-matrix live-critical live-selftest \
  provider-env provider-dry provider-matrix bench-latency loadtest-smoke loadtest-soak \
  perf-gate perf-baseline perf-selftest tts-sim offline-up offline-models offline-down offline-test; do
  require "$recipe"
done

count="$(grep -cE '^    [a-z]' <<<"$list")"
if (( count < 50 )); then
  echo "check-tasks: only $count recipes listed; expected the full developer surface" >&2
  fail=1
fi

# Wrapper bodies must reach real Make targets and executable scripts.
while IFS= read -r target; do
  grep -qE "^${target}:" Makefile || { echo "check-tasks: recipe calls missing Make target '$target'" >&2; fail=1; }
done < <(grep -oE 'make [a-z][a-z0-9-]*' justfile | awk '{print $2}' | sort -u)
while IFS= read -r script; do
  [[ -x "$ROOT/$script" ]] || { echo "check-tasks: recipe uses missing script '$script'" >&2; fail=1; }
done < <(grep -oE '\./scripts/[a-z-]+\.sh' justfile | sort -u)

# The pinned runner and the no-runner error path.
[[ "$("$ROOT/.tools/bin/just" --version)" == "just 1.59.0" ]] || { echo "check-tasks: just is not 1.59.0" >&2; fail=1; }
mv "$ROOT/.tools/bin/just" "$ROOT/.tools/bin/just.check-tasks"
restore_just() { if [[ -e "$ROOT/.tools/bin/just.check-tasks" ]]; then mv -f "$ROOT/.tools/bin/just.check-tasks" "$ROOT/.tools/bin/just"; fi; return 0; }
trap 'restore_just; rm -rf "$probe"' EXIT
if "$DEV" --list >/dev/null 2>&1; then
  echo "check-tasks: ./dev worked without .tools/bin/just" >&2
  fail=1
fi
restore_just

# Demo safety: without a fixture, seed must refuse before it can wipe anything.
demo_out="$(GOVOICE_DEV_DIR="$probe/demo-guard" GOVOICE_DEMO_FIXTURE_PORT=1 "$DEV" demo-seed atlas 2>&1)" && demo_rc=0 || demo_rc=$?
if [[ "$demo_rc" -ne 2 || "$demo_out" != *"MCP fixture is not running"* ]]; then
  echo "check-tasks: demo-seed did not refuse an absent fixture (exit $demo_rc): $demo_out" >&2
  fail=1
fi

# The no-argument command and each runbook must be usable, and the runbooks must name
# the commands a developer is looking for.
"$DEV" >/dev/null
"$DEV" help >/dev/null
for topic in dev test demo deploy eval live; do
  "$DEV" help "$topic" >/dev/null
done
demo_help="$("$DEV" help demo)"
deploy_help="$("$DEV" help deploy)"
grep -q 'demo-seed' <<<"$demo_help" || { echo "check-tasks: demo help does not name demo-seed" >&2; fail=1; }
grep -q 'deploy-demo' <<<"$deploy_help" || { echo "check-tasks: deploy help does not name deploy-demo" >&2; fail=1; }
grep -q 'deploy-server' <<<"$deploy_help" || { echo "check-tasks: deploy help does not name deploy-server" >&2; fail=1; }

# `recipe --help` is a common first guess; it must reach help, not a Make target.
recipe_help="$("$DEV" deploy-demo --help)"
grep -q 'Deployments' <<<"$recipe_help" || { echo "check-tasks: deploy-demo --help did not reach the deploy runbook" >&2; fail=1; }

if ! (cd "$ROOT/docs" && ../dev --list >/dev/null); then
  echo "check-tasks: ./dev does not work from a subdirectory" >&2
  fail=1
fi

# Every ./dev <command> named in a runbook must resolve to a recipe.
while IFS= read -r cmd; do
  case "$cmd" in --*|help) continue ;; esac
  require "$cmd"
done < <(grep -oE '\./dev [a-z][a-z0-9-]*' "$ROOT/scripts/help.sh" | awk '{print $2}' | sort -u)

# Named/positional arguments must not be re-evaluated by the shell: a command
# substitution in a demo name or help topic is data, not code.
"$DEV" demo-seed "\$(touch "$probe/pwned")" >/dev/null 2>&1 || true
"$DEV" demo-seed "atlas\"; touch $probe/pwned; #" >/dev/null 2>&1 || true
"$DEV" demo-start "atlas\"; touch $probe/pwned; #" >/dev/null 2>&1 || true
"$DEV" help "\$(touch "$probe/pwned")" >/dev/null 2>&1 || true
if [[ -e "$probe/pwned" ]]; then
  echo "check-tasks: an argument was executed by the shell" >&2
  fail=1
fi

# Deployment recipes must pass the target exactly once. A fake gh records the dispatch.
mkdir -p "$probe/bin"
cat > "$probe/bin/gh" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${GH_LOG:?}"
if [[ "${1:-} ${2:-}" == "auth status" ]]; then exit 0; fi
exit 0
FAKE
chmod +x "$probe/bin/gh"
gh_log="$probe/gh.log"
: > "$gh_log"
if ! PATH="$probe/bin:$PATH" GH_LOG="$gh_log" "$DEV" deploy-server cloudrun --min-instances 0 >/dev/null 2>&1; then
  echo "check-tasks: deploy-server failed with a fake gh" >&2; fail=1
fi
grep -qx 'workflow run deploy.yml --ref main -f min_instances=0' "$gh_log" ||
  { echo "check-tasks: deploy-server dispatched the wrong command: $(tr '\n' ';' < "$gh_log")" >&2; fail=1; }
: > "$gh_log"
if ! PATH="$probe/bin:$PATH" GH_LOG="$gh_log" "$DEV" deploy-demo atlas --server https://server.example --environment demo >/dev/null 2>&1; then
  echo "check-tasks: deploy-demo failed with a fake gh" >&2; fail=1
fi
grep -q 'workflow run deploy-demo-dispatch.yml --ref main -f demo=atlas -f target=cloudrun' "$gh_log" ||
  { echo "check-tasks: deploy-demo dispatched the wrong command: $(tr '\n' ';' < "$gh_log")" >&2; fail=1; }
if grep -q -- ' -f target=atlas' "$gh_log"; then
  echo "check-tasks: deploy-demo passed the demo name as the target" >&2; fail=1
fi

"$ROOT/scripts/init-test.sh"

if (( fail != 0 )); then
  echo "check-tasks: failed" >&2
  exit 1
fi
echo "check-tasks: $count recipes, all described; help topics, demo/help argument safety, deploy dispatch, bootstrap failure paths and subdirectory invocation pass"
