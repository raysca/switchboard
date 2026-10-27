#!/usr/bin/env bash
# Curated help for ./dev. The recipe list is the complete index; this script explains the
# journeys a new developer needs: start the stack, test, seed a demo, deploy one.
set -euo pipefail

overview() {
  cat <<'EOF'
Switchboard developer commands

  ./dev up                      start the server and admin UI (foreground; Ctrl-C stops)
  ./dev up-fresh                same stack on an empty store, for the first-run wizard
  ./dev status                  is the stack running? prints the UI/API/voice URLs
  ./dev down                    stop this checkout's stack
  ./dev doctor                  check Go/Node/Docker/ONNX/ports before starting

  ./dev test                    build + vet + race tests in every Go module
  ./dev test-e2e                Go e2e + agent-eval self-test + browser suites (echo)
  ./dev verify                  the full PR gate
  ./dev fmt                     format Go and typecheck the UI

  ./dev demo-seed atlas         wipe and recreate a demo in the running stack
  ./dev demo-start atlas        run the demo's MCP fixture (foreground)
  ./dev demo-list               the available demos and their commands

  ./dev deploy-server cloudrun  deploy the server (or: cloudflare)
  ./dev deploy-demo atlas cloudrun --server URL --environment ENV
  ./dev deploy-status           recent deployment runs

  ./dev help dev|test|demo|deploy|eval|live
  ./dev --list                  every recipe with its one-line description
EOF
}

dev_help() {
  cat <<'EOF'
Local development

  ./dev up
      Starts the server, the test MCP servers and the admin UI; seeds the Dev Assistant
      and Dealership desk agents when provider keys are present. State is in
      .govoice-dev/ (SQLite store, generated keys and remembered ports). Ctrl-C stops it.

  ./dev up-fresh
      A separate .govoice-dev-fresh/ store wiped on every start, with no seeded agents,
      so /onboarding shows the first-run wizard.

  ./dev status / ./dev down
      Work on the current checkout's stack only. A port collision is reported with the
      process holding it; nothing outside this checkout is killed.

  ./dev doctor
      Reports missing prerequisites (Go, Node, Docker, ONNX Runtime, just), the provider
      key file and the ports the stack uses.

  ./dev admin-link
      Prints a magic link that signs the admin UI in.
  ./dev offline-up | offline-models | offline-down | offline-test
      The offline speech stack (models are a separate one-time download).
  ./dev example-web | example-cli | example-go NAME=...
      Browser/CLI/standalone examples against the running stack.

Provider keys: examples/.env (template examples/.env.example). Without keys the echo
provider runs the free checks. Ports and more: docs/quickstart.md section 8.
EOF
}

test_help() {
  cat <<'EOF'
Testing

  ./dev test          build, vet, race-test every module and run the Go e2e
  ./dev test-go       Go end-to-end tests only (real server + WebSocket, echo provider)
  ./dev test-e2e      Go e2e + agent-eval self-test + browser suites (echo; free)
  ./dev test-browser  Playwright agents + one-port suites only
  ./dev test-oneport  Playwright one-port suite only
  ./dev test-postgres Postgres storage/retention/eval-worker tests (Docker or TEST_POSTGRES_DSN)
  ./dev verify        the PR gate: tests, formatting, manifest/fixtures, docs, vocabulary
  ./dev check-tasks   the task contract: recipe descriptions, help topics and safety probes
  ./dev fmt           gofmt + UI typecheck
  ./dev check-tasks   validate this justfile and the help runbooks (after make init)
  ./dev deadcode      unreachable functions outside the allowlist

Paid/live checks never run by accident: the underlying targets refuse without
GOVOICE_LIVE=1. Preview live runs with ./dev live-list and ./dev matrix-plan.
EOF
}

demo_help() {
  cat <<'EOF'
Demos (Smile Dental and Atlas Travel Concierge)

  ./dev demo-list
      List the demos, then start a stack and seed one:

  ./dev up                              # terminal 1: server + admin UI
  ./dev demo-start atlas                # terminal 2: run the demo MCP fixture (foreground)
  ./dev demo-seed atlas                 # terminal 3: wipe + create the demo

  ./dev demo-reset dental               # same as demo-seed: wipe and recreate
  ./dev demo-wipe dental                # archive the agent, delete its server/secret
  ./dev seed dental                     # alias for demo-seed

demo-start reads the demo token from the environment (the dev defaults are
dev-dental-token/dev-atlas-token). demo-seed needs the running stack, the
management key in .govoice-dev/dev.env and the fixture answering /health; it
refuses before wiping anything if the fixture is down. It writes the token into
the server's encrypted secret store. demo-seed/demo-wipe always target the
loopback dev server, never an exported production admin URL.

Seed into a non-local server manually:
  export GOVOICE_MANAGEMENT_API_KEY=...
  DENTAL_MCP_TOKEN=... go run ./demos/seed \
    --server https://admin.example --demo dental \
    --mcp scheduling=https://mcp.example --yes
  ATLAS_MCP_TOKEN=... go run ./demos/seed \
    --server https://admin.example --demo atlas \
    --mcp inventory=https://mcp.example --http-base https://mcp.example --yes

Full docs: demos/README.md, demos/dental/README.md, demos/atlas/README.md.
EOF
}

deploy_help() {
  cat <<'EOF'
Deployments

Two independent deployments:
  - the server pipeline (.github/workflows/deploy.yml), clean-room: server only;
  - the optional per-demo pipeline (.github/workflows/deploy-demo.yml), which deploys
    one demo MCP service and can seed it into a running server.

Server
  ./dev deploy-server cloudrun
      Dispatches deploy.yml on main with gh. It builds/pushes the image, deploys, and
      smoke-tests before taking traffic. Optional: --min-instances N.
  ./dev deploy-server cloudflare
      Runs `make deploy-cloudflare` locally; requires CLOUDFLARE_API_TOKEN and a
      Postgres-backed server (the /tmp SQLite profile is not a persistent demo target).

Demo
  ./dev deploy-demo atlas cloudrun --server https://switchboard.example --environment demo
  ./dev deploy-demo dental cloudflare --server https://switchboard.example --environment demo \
    --mcp-base-url https://dental-mcp.example

  Required flags:
    --server URL             admin URL of the clean-room server to seed
    --environment NAME       GitHub environment holding the demo token secret and
                             the management key (and CLOUDFLARE_API_TOKEN on Cloudflare)
    --mcp-base-url URL       Cloudflare only: the deployed demo Worker URL
    --management-key-secret  optional; defaults to SWITCHBOARD_MANAGEMENT_API_KEY
    --no-seed                deploy without seeding
    --image-tag TAG          optional image tag (defaults to the commit SHA)
  The demo must already have a token secret (DENTAL_MCP_TOKEN/ATLAS_MCP_TOKEN) in the
  environment; the workflow never generates one. Both values are masked.

  ./dev deploy-status      recent server and demo deployment runs
  ./dev deploy-demo --help goes to this topic

Docs: docs/deployment.md, docs/deployment-cloudflare-plan.md, demos/README.md.
EOF
}

eval_help() {
  cat <<'EOF'
Agent evals

  ./dev eval-agents                    offline model against ./dev up; tools run for real
  ./dev eval-agents CASES=demos/dental/eval
      Run the dental case (seed the dental demo and start its fixture first).
  ./dev eval-agents-live               agents' own models (GOVOICE_LIVE=1; paid)
  ./dev eval-agents-echo               harness self-test on the echo provider (free)
  ./dev eval-pipecat                   black-box pipecat.evals run (echo)
  ./dev eval-turn                      turn-quality gate (fake oracles)
  ./dev parity-regen                   regenerate paired Pipecat goldens

The targets never read or print the management key; source it first:
  set -a; . .govoice-dev/dev.env; set +a

Docs: docs/operations/agent-evals.md.
EOF
}

live_help() {
  cat <<'EOF'
Live and paid checks

The live/matrix commands spend real provider credit and refuse without GOVOICE_LIVE=1.
The free checks below (live-list, matrix-plan, live-selftest, provider-*, bench, loadtest,
perf, tts-sim) spend nothing; preview a paid run first.

  ./dev live-list                      profiles this machine can run (env names only)
  ./dev matrix-plan                    upper-bound estimate for an agent-matrix run
  ./dev live-check                     Google live run (LIVE_ARGS=...)
  ./dev agent-matrix                   profiles x scenarios x modes (MAX_CALLS cap)
  ./dev live-critical                  nightly critical-path subset
  ./dev live-selftest                  scenario engine on the echo provider (free)
  ./dev provider-env                   ready provider/stage rows and env names
  ./dev provider-dry                   stage conformance against fakes (free)
  ./dev provider-matrix                real STT/TTS/LLM matrix (paid)
  ./dev bench-latency                  framework overhead benchmark (free)
  ./dev loadtest-smoke                 real WebSocket calls on echo (free)
  ./dev loadtest-soak                  longer soak (SOAK_DURATION=...)
  ./dev perf-gate | perf-baseline | perf-selftest
  ./dev tts-sim                        TTS unit-sizing simulator (free)

Docs: docs/benchmarks/README.md. Paid examples:
  GOVOICE_LIVE=1 ./dev live-check LIVE_ARGS="-scenarios S1 -modes off"
  GOVOICE_LIVE=1 ./dev agent-matrix PROFILES=google SCENARIOS=S1 MAX_CALLS=40
EOF
}

topic="${1:-}"
case "$topic" in
  "") overview ;;
  dev) dev_help ;;
  test) test_help ;;
  demo) demo_help ;;
  deploy) deploy_help ;;
  eval) eval_help ;;
  live) live_help ;;
  *)
    echo "dev: unknown help topic '$topic'" >&2
    echo "available topics: dev test demo deploy eval live" >&2
    exit 2
    ;;
esac

# The recipe index follows every runbook, so the answer to "what else can I run?" is
# always one screen away. The runner path is passed by the justfile.
if [[ -n "${JUST_BIN:-}" && -x "$JUST_BIN" ]]; then
  echo
  if "$JUST_BIN" --list; then
    :
  else
    status=$?
    [[ "$status" -eq 141 ]] || exit "$status"
  fi
elif command -v just >/dev/null 2>&1; then
  echo
  just --list
fi
