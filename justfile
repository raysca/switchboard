# Switchboard developer commands. The public entry point is ./dev, which runs this
# justfile with the pinned runner installed by `make init`.
#
#   ./dev                 common commands and the recipe list
#   ./dev --list          every task with a description
#   ./dev help <topic>    dev | test | demo | deploy | eval | live
#
# Make remains the implementation and the CI path; these recipes are the discoverable
# front door. Add a doc comment above every public recipe: ./dev --list shows it.
set shell := ["bash", "-euo", "pipefail", "-c"]
set minimum-version := "1.59.0"
set positional-arguments := true

# Show common commands and the full recipe list
default:
    @JUST_BIN="{{just_executable()}}" ./scripts/help.sh

# Show a runbook: dev | test | demo | deploy | eval | live
help topic="":
    @JUST_BIN="{{just_executable()}}" ./scripts/help.sh "${1:-}"

# ---------------------------------------------------------------------------
# Local development

# Start the server, test MCP servers and admin UI (foreground; Ctrl-C stops)
up:
    @./scripts/dev.sh up

# Start the same stack on an empty store, for the first-run wizard at /onboarding
up-fresh:
    @make dev-fresh

# Stop this checkout's dev stack
down:
    @./scripts/dev.sh down

# Show whether the dev stack is running, with the UI, API and voice URLs
status:
    @./scripts/dev.sh status

# Check Go, Node, Docker, ONNX, keys and ports before starting
doctor:
    @./scripts/doctor.sh

# Print a magic link that signs the admin UI in (URL=https://host for a hosted server)
admin-link *args:
    @make admin-link "$@"

# Browser client example against the running dev stack
example-web:
    @make example-web

# CLI call against the dev agent (TEXT="hello" for a text turn)
example-cli *args:
    @make example-cli "$@"

# Standalone Go example (NAME=google_pipeline)
example-go *args:
    @make example-go "$@"

# Run the MCP test servers on 127.0.0.1:18765 (/ok /open /fail/* /slow)
mcp-test:
    @make mcp-test

# ---------------------------------------------------------------------------
# Build

# Build bin/server, bin/vadworker, bin/turnworker and the examples
build:
    @make build

# Build the admin UI into server/adminui/static
ui-embed:
    @make ui-embed

# Download the pinned ONNX Runtime for this OS/arch into third_party/
ort:
    @make ort

# Regenerate the protobuf frame code
proto-gen:
    @make proto-gen

# ---------------------------------------------------------------------------
# Tests and checks

# Build, vet and race-test every Go module with the production tags
test:
    @make test

# Go end-to-end tests only (real server + WebSocket, echo provider)
test-go:
    @make test-e2e-go

# Every free end-to-end test: Go e2e, agent-eval self-test, then browser
test-e2e:
    @make e2e-echo

# Playwright browser suites only (agents + one-port, echo provider)
test-browser:
    @make e2e-fake

# Playwright one-port suite only (embedded UI, same origin)
test-oneport:
    @make e2e-oneport

# Postgres storage, retention and eval-worker tests (Docker or TEST_POSTGRES_DSN)
test-postgres:
    @make test-postgres

# The full PR gate: tests, formatting, manifest/fixtures, docs, vocabulary and the task contract
verify:
    @make verify
    @make check-tasks

# Format Go sources and typecheck the admin UI
fmt:
    @make fmt

# Unreachable functions outside scripts/deadcode-allow.txt
deadcode:
    @make deadcode

# Fail when the server module depends on demos/
demo-boundary:
    @make demo-boundary

# Validate this justfile, recipe descriptions and help runbooks (after make init)
check-tasks:
    @make check-tasks

# Release hardening: pinned images, govulncheck, SBOM and image scan (Docker)
release-security:
    @make release-security

# ---------------------------------------------------------------------------
# Demos

# List the demos and their commands
demo-list:
    @./scripts/demo.sh list

# Run a demo MCP fixture in the foreground (dental or atlas)
demo-start name="":
    @./scripts/demo.sh start "$1"

# Wipe and create a demo in the running dev stack (dental or atlas)
demo-seed name="":
    @./scripts/demo.sh seed "$1"

# Alias of demo-seed: wipe and recreate a demo
demo-reset name="":
    @./scripts/demo.sh reset "$1"

# Archive the demo agent and delete its MCP server/secret (call history stays)
demo-wipe name="":
    @./scripts/demo.sh wipe "$1"

# Alias of demo-seed for a running dev stack
seed name="":
    @./scripts/demo.sh seed "$1"

# ---------------------------------------------------------------------------
# Deployments

# Deploy the server: cloudrun (GitHub Actions) or cloudflare (local wrangler)
deploy-server target="cloudrun" *args:
    @./scripts/deploy.sh server "$@"

# Deploy one demo and seed a server: deploy-demo <dental|atlas> <cloudrun|cloudflare> --server URL --environment NAME
deploy-demo name target="cloudrun" *args:
    @./scripts/deploy.sh demo "$@"

# Show recent server and demo deployment runs
deploy-status:
    @./scripts/deploy.sh status

# ---------------------------------------------------------------------------
# Evals and paid/live checks (the targets gate paid runs on GOVOICE_LIVE=1)

# Agent evals against the running dev stack on the offline model (CASES=...)
eval-agents *args:
    @make eval-agents "$@"

# Agent eval harness self-test on the echo provider (free)
eval-agents-echo:
    @make eval-agents-echo

# Agent evals on each agent's own model (GOVOICE_LIVE=1; paid)
eval-agents-live *args:
    @make eval-agents-live "$@"

# Black-box pipecat.evals run on the echo provider (free)
eval-pipecat:
    @make eval-pipecat

# Turn-quality gate with fake oracles (free)
eval-turn:
    @make eval-turn

# Regenerate the paired Pipecat goldens (needs the pipecat-impl venv)
parity-regen:
    @make parity-regen

# Playwright browser e2e against real Google providers (GOVOICE_LIVE=1; paid)
e2e:
    @make e2e

# Playwright browser e2e against real xAI (GOVOICE_LIVE=1; paid)
e2e-xai:
    @make e2e-xai

# List the live-check profiles this machine can run (environment names only)
live-list:
    @make live-list

# Upper-bound estimate and hard cap for an agent-matrix run
matrix-plan *args:
    @make agent-matrix-plan "$@"

# Agent-level live run, Google profile (GOVOICE_LIVE=1; LIVE_ARGS=...)
live-check *args:
    @make live-check "$@"

# Standard agent x profiles x scenarios x modes (GOVOICE_LIVE=1; MAX_CALLS cap)
agent-matrix *args:
    @make agent-matrix "$@"

# Nightly critical-path live subset (GOVOICE_LIVE=1 PROFILES=... SCENARIOS=...)
live-critical *args:
    @make live-critical "$@"

# Scenario engine against the echo provider (free)
live-selftest *args:
    @make live-check-selftest "$@"

# Provider/stage rows that are ready, with the env names they need (no values)
provider-env *args:
    @make provider-env "$@"

# Stage conformance against in-process fakes (free)
provider-dry *args:
    @make provider-matrix-dry "$@"

# Real STT/TTS/LLM provider matrix (GOVOICE_LIVE=1; paid)
provider-matrix *args:
    @make provider-matrix "$@"

# Framework overhead benchmark with fake providers (free)
bench-latency *args:
    @make bench-latency "$@"

# Real WebSocket calls against the echo provider (free, about 1 min)
loadtest-smoke *args:
    @make loadtest-smoke "$@"

# Soak run (SOAK_DURATION=5m by default; free)
loadtest-soak *args:
    @make loadtest-soak "$@"

# Latency/CPU regression check against this machine's baseline (free)
perf-gate *args:
    @make perf-gate "$@"

# Regenerate this machine's perf baseline (FORCE=1; idle host)
perf-baseline *args:
    @make perf-baseline "$@"

# Prove the perf gate fails on an injected delay (free, about 4 min)
perf-selftest *args:
    @make perf-gate-selftest "$@"

# TTS unit-sizing simulator for unary providers (free)
tts-sim *args:
    @make tts-sim "$@"

# ---------------------------------------------------------------------------
# Offline speech stack

# Start the offline stack (OFFLINE_PROFILE=container-llm|host-llm)
offline-up *args:
    @make offline-up "$@"

# Download Whisper, Kokoro and the LLM into the offline stack (once)
offline-models *args:
    @make offline-models "$@"

# Stop the offline stack (volumes are kept)
offline-down *args:
    @make offline-down "$@"

# Round trip through the running offline stack (after offline-up and offline-models)
offline-test *args:
    @make offline-test "$@"
