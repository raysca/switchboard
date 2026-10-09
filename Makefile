UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  SDKROOT_CANDIDATE := /Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
  # Only when present and not set by the caller; otherwise cgo asks xcrun (CI runners).
  ifneq ($(wildcard $(SDKROOT_CANDIDATE)),)
    export SDKROOT ?= $(SDKROOT_CANDIDATE)
  endif
  export CGO_LDFLAGS := -Wl,-w
endif
export GOVOICE_ROOT := $(CURDIR)
export PATH := $(PATH):$(HOME)/go/bin:/opt/homebrew/bin

# `make` with no target shows the help (this used to build). `make --help` is GNU make's own usage.
.DEFAULT_GOAL := help

help: ## build|Show this help
	@scripts/make-help.sh $(firstword $(MAKEFILE_LIST))

# xAI STT/TTS are in launch scope: every build, vet and test of the shipped server uses -tags xaispeech
# (the production Dockerfile does the same). Override with GOTAGS= to build without them.
GOTAGS ?= xaispeech
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS_VERSION := -X govoice/server.Version=$(VERSION) -X govoice/server.Commit=$(COMMIT)

build: ort-check ## build|Build bin/server, bin/vadworker, bin/turnworker and the examples
	@mkdir -p bin
	go build -tags "$(GOTAGS)" -o bin/vadworker ./cmd/vadworker
	go build -tags "$(GOTAGS)" -o bin/turnworker ./cmd/turnworker
	go build -tags "$(GOTAGS)" -ldflags "$(LDFLAGS_VERSION)" -o bin/server ./cmd/server
	go build -tags "$(GOTAGS)" ./examples/...

# ONNX Runtime is git-ignored (*.dylib / *.so) and needed by the vad and smartturn tests and the server.
# Fresh clone: `make ort` downloads the pinned release and verifies its sha256.
ort-check: ## build|Fail with instructions when the ONNX Runtime library is missing
	@scripts/fetch-ort.sh check

ort: ## build|Download the pinned ONNX Runtime for this OS/arch into third_party/ (sha256-verified)
	@scripts/fetch-ort.sh fetch

# Every module in go.work (root also covers vad, smartturn, transport, tools/*, cmd, examples).
MODULES := . core providers/cartesia providers/deepgram providers/elevenlabs providers/google providers/nvidia providers/openai providers/xai server

test: build ## build|Vet (with and without xaispeech) + race-test every module with -tags xaispeech (about 3 min)
	@failed=""; for m in $(MODULES); do \
		echo "=== Vetting and testing module $$m ==="; \
		(cd $$m && go vet ./... && go vet -tags "$(GOTAGS)" ./... && go test -race -count=1 -tags "$(GOTAGS)" ./...) || failed="$$failed $$m"; \
	done; \
	if [ -n "$$failed" ]; then echo "=== FAILED modules:$$failed ==="; exit 1; fi; \
	echo "=== All modules passed ==="

# Black-box pipecat.evals run against the Go server with fake providers (docs/decisions/pipecat-evals.md).
eval-pipecat: ## free|pipecat.evals black-box run against the server (fake providers)
	@./tools/eval/pipecat/run.sh

# Turn-quality gate (docs/benchmarks/turn-quality.md): fake oracles, no cost; fails on threshold regression.
eval-turn: ## free|Turn-quality gate (fake oracles; fails on threshold regression)
	@go run ./tools/eval/cmd/govoice-turn-eval

# Paired Pipecat fixtures (docs/pipecat-parity-matrix.md): re-executes the pinned pipecat-impl components
# from its venv (`cd pipecat-impl && uv sync` once) and rewrites tools/compat/testdata/parity/*.json.
# Offline and deterministic; `go test ./tools/compat/...` compares go-voice against the committed goldens.
parity-regen: ## free|Regenerate the paired Pipecat goldens (needs pipecat-impl venv)
	@test -x pipecat-impl/.venv/bin/python || { echo "pipecat-impl/.venv missing: cd pipecat-impl && uv sync"; exit 1; }
	cd pipecat-impl && .venv/bin/python ../tools/compat/parity/gen_goldens.py

verify: test ## build|test + gofmt + manifest/fixture checks: run before a PR
	@echo "=== Checking code formatting ==="
	@unformatted=$$(gofmt -l core/ transport/ tools/ proto/ providers/ vad/ smartturn/ cmd/ server/ examples/); \
	if [ -n "$$unformatted" ]; then \
		echo "Files need formatting:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "=== Validating Manifest ==="
	node tools/compat/validate_manifest.js
	@echo "=== Verifying Protobuf Fixtures ==="
	node tools/compat/verify_fixtures.js
	@echo "=== Playback-clear probe (clear failure stays a failure unless the recorded decision matches) ==="
	node tools/compat/probe_playback_clear.js
	@echo "=== All verify checks passed ==="

release-security: ## build|Release hardening: pinned images, govulncheck, SBOM, image scan (docker)
	@echo "=== Immutable base image digests ==="
	@if grep -E '^FROM ' deploy/Dockerfile | grep -v '@sha256:'; then \
		echo "Dockerfile FROM lines must pin immutable digests"; \
		exit 1; \
	fi
	@echo "=== govulncheck ==="
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	@echo "=== Module SBOM ==="
	go list -m -json all | python3 -c 'import json,sys; text=sys.stdin.read(); dec=json.JSONDecoder(); i=0; objs=[]; \
	exec("while i < len(text):\n    while i < len(text) and text[i].isspace():\n        i += 1\n    if i >= len(text):\n        break\n    obj, i = dec.raw_decode(text, i)\n    objs.append(obj)"); \
	open("deploy/sbom.json","w").write(json.dumps(objs, indent=2)+"\n")'
	@echo "=== Production image build and smoke ==="
	docker build -f deploy/Dockerfile -t govoice:release-security .
	docker run --rm --entrypoint /bin/sh govoice:release-security -c 'test -x /opt/govoice/bin/server && test -x /opt/govoice/bin/vadworker && test -f /opt/govoice/models/silero_vad.onnx'
	@echo "=== Container vulnerability scan ==="
	@if command -v trivy >/dev/null 2>&1; then \
		trivy image --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL govoice:release-security; \
	elif command -v grype >/dev/null 2>&1; then \
		grype govoice:release-security --only-fixed --fail-on high; \
	else \
		docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:0.69.3 image --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL govoice:release-security; \
	fi

proto-gen: ## build|Regenerate the protobuf frame code
	@mkdir -p core/transport/v1/framespb
	protoc --proto_path=proto --go_out=core/transport/v1/framespb --go_opt=Mframes.proto=govoice/core/transport/v1/framespb --go_opt=paths=source_relative frames.proto

.PHONY: build test verify release-security proto-gen

# Postgres storage tests (skipped by `make test`). Uses TEST_POSTGRES_DSN if set,
# otherwise starts a throwaway postgres container.
PG_IMAGE ?= postgres:16-alpine
test-postgres: ## build|Postgres storage tests (docker, or TEST_POSTGRES_DSN)
	@if [ -n "$$TEST_POSTGRES_DSN" ]; then \
		cd server && go test -race -count=1 ./storage/ ./retention/; \
	else \
		docker rm -f govoice-pg-test >/dev/null 2>&1; \
		docker run -d --name govoice-pg-test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=govoice -p 55432:5432 $(PG_IMAGE) >/dev/null && \
		for i in $$(seq 1 30); do docker exec govoice-pg-test pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done; \
		cd server && TEST_POSTGRES_DSN="postgres://postgres:test@127.0.0.1:55432/govoice?sslmode=disable" go test -race -count=1 ./storage/ ./retention/; \
		status=$$?; docker rm -f govoice-pg-test >/dev/null; exit $$status; \
	fi

.PHONY: test-postgres

# Full-stack browser e2e (Playwright) against real Google providers; see e2e/README.md.
# Skips every test when no Google secret is configured.
e2e: ## paid|Playwright browser e2e against REAL Google (GOVOICE_LIVE=1; the most expensive)
	@test "$$GOVOICE_LIVE" = 1 || { echo "e2e drives real Google providers and spends API credit: run GOVOICE_LIVE=1 make e2e"; exit 1; }
	cd e2e && npm install --no-audit --no-fund && npx playwright install chromium && npx playwright test

.PHONY: e2e

# Local development (scripts/dev.sh): `make dev` runs the server and admin UI with
# generated secrets and a local SQLite store in .govoice-dev/, seeded with a
# "Dev Assistant" agent from examples/.env. Examples call that agent.
dev: ## dev|Run the server and admin UI with generated secrets and a seeded Dev Assistant
	@./scripts/dev.sh up

example-web: ## dev|Browser client example against make dev
	@./scripts/dev.sh web

example-cli: ## dev|CLI call against make dev (TEXT="hello" for a text turn)
	@TEXT="$(TEXT)" ./scripts/dev.sh cli

example-go: ## dev|Standalone Go example (NAME=google_pipeline)
	@./scripts/dev.sh go "$(NAME)"

# Opt-in LIVE check: the agent-level provider-matrix harness (tools/livecheck). Real providers,
# spends API credit; refuses without GOVOICE_LIVE=1 and prints a provider-request estimate first.
# live-check keeps the old shape (Google profile, turn modes off/on/smart-turn):
# LIVE_ARGS e.g. "-scenarios S1,S2 -modes off,on -stt-model latest_long -trace -profiles nvidia".
# Flags: go run ./tools/livecheck -h. Profiles: tools/livecheck/profiles.yaml (docs/benchmarks/README.md).
live-check: ## paid|Agent-level live run, Google profile (GOVOICE_LIVE=1; LIVE_ARGS=...)
	@test "$$GOVOICE_LIVE" = 1 || { echo "live-check spends real provider credit: run GOVOICE_LIVE=1 make live-check"; exit 1; }
	@test -f examples/.env || { echo "live-check needs examples/.env (see examples/.env.example)"; exit 1; }
	@go run ./tools/livecheck $(LIVE_ARGS)

# agent-matrix: standard agent x provider profiles x scenarios x turn modes
# (docs/benchmarks/agent-matrix.md). GOVOICE_LIVE=1 make agent-matrix PROFILES=google,nvidia SCENARIOS=S1 MODES=off,on
# COST CAP: -max-calls defaults to 40 (DEFAULT_MAX_CALLS). A plan whose upper-bound estimate exceeds it is refused before any
# call (see make agent-matrix-plan); during the run the MEASURED provider requests are checked every 250ms and the call ends
# when they pass the cap. Raise it on purpose: MAX_CALLS=120.
# Optional: REPEAT=n MAX_CALLS=n LIVE_ARGS="-write-docs -mcp-url https://... -trace"; PROFILES=all runs every profile whose env is set.
agent-matrix: ## paid|Standard agent x PROFILES x SCENARIOS x MODES (GOVOICE_LIVE=1; hard cap MAX_CALLS, default 40)
	@test "$$GOVOICE_LIVE" = 1 || { echo "agent-matrix spends real provider credit: run GOVOICE_LIVE=1 make agent-matrix PROFILES=google,nvidia"; exit 1; }
	@test -f examples/.env || { echo "agent-matrix needs examples/.env (see examples/.env.example)"; exit 1; }
	@go run ./tools/livecheck -profiles "$(or $(PROFILES),google)" -modes "$(or $(MODES),off$(comma)on)" $(if $(SCENARIOS),-scenarios "$(SCENARIOS)") $(if $(REPEAT),-repeat $(REPEAT)) -max-calls $(or $(MAX_CALLS),$(DEFAULT_MAX_CALLS)) $(LIVE_ARGS)

# Offline engine self-test: the same scenario engine against a -tags fakeproviders server child.
# No network, no keys, no cost (about 2 min: builds the server, S5/S8 wait on real timers).
live-check-selftest: ## free|The scenario engine against fake providers (about 2 min)
	@go run ./tools/livecheck -selftest $(LIVE_ARGS)

# Previews: read-only, no provider calls, no keys needed beyond reading which env names are set.
live-list: ## preview|Which agent-matrix profiles this machine can run (env check only)
	@go run ./tools/livecheck -list

agent-matrix-plan: ## preview|Per-profile upper-bound estimate + the hard cap (MAX_CALLS, default 40) for an agent-matrix run
	@go run ./tools/livecheck -dry-run -profiles "$(or $(PROFILES),google)" -modes "$(or $(MODES),off$(comma)on)" $(if $(SCENARIOS),-scenarios "$(SCENARIOS)") $(if $(REPEAT),-repeat $(REPEAT)) -max-calls $(or $(MAX_CALLS),$(DEFAULT_MAX_CALLS)) $(LIVE_ARGS)

provider-env: ## preview|Which provider/stage rows are ready and the env NAMES they need (no values)
	@go run $(if $(TAGS),-tags $(TAGS)) ./tools/provtest/cmd/provtest -list-env

provider-matrix-dry: ## free|Stage conformance suite against in-process fakes (no network, no keys)
	@go run ./tools/provtest/cmd/provtest -dry -out ""

# Default hard cap on provider requests for the paid targets (override per run with MAX_CALLS=n).
DEFAULT_MAX_CALLS ?= 40
# $(or $(MODES),off,on) would split on the comma and default to "off" only.
comma := ,

.PHONY: help live-check agent-matrix live-check-selftest live-list agent-matrix-plan provider-env provider-matrix-dry

# Stage-level provider conformance matrix (docs/benchmarks/provider-matrix.md, tools/provtest): the same
# STT/TTS/LLM checks against every provider built through the server registry. Spends real API credit
# (about 17 requests per provider); providers without credentials in examples/.env are skipped.
# PROVIDERS=google,nvidia STAGES=stt,tts,llm PROVTEST_ARGS="-repeat 2 -write-docs" MAX_CALLS=80 (default cap 40, checked against measured requests).
# TAGS=xaispeech includes xAI STT/TTS. Env names per row: go run ./tools/provtest/cmd/provtest -list-env
provider-matrix: ## paid|Stage-level STT/TTS/LLM conformance per provider (GOVOICE_LIVE=1; PROVIDERS= STAGES= TAGS=; cap MAX_CALLS, default 40)
	@test "$$GOVOICE_LIVE" = 1 || { echo "provider-matrix spends real provider API credit: run GOVOICE_LIVE=1 make provider-matrix"; exit 1; }
	@test -f examples/.env || { echo "provider-matrix needs examples/.env (see examples/.env.example)"; exit 1; }
	@go run $(if $(TAGS),-tags $(TAGS)) ./tools/provtest/cmd/provtest -providers "$(PROVIDERS)" -stages "$(STAGES)" -max-calls $(or $(MAX_CALLS),$(DEFAULT_MAX_CALLS)) $(PROVTEST_ARGS)

.PHONY: provider-matrix

# Framework latency baseline with fake providers (docs/benchmarks/baseline.md).
bench-latency: ## free|Framework overhead benchmark (fake providers)
	@go run ./tools/eval/cmd/govoice-bench $(BENCH_ARGS)

# Load/soak harness: real WebSocket calls against a -tags fakeproviders server child
# (docs/benchmarks/capacity.md). Fake providers, no network, no cost.
# loadtest-smoke: ~1 min, CI-friendly. Skips when the Silero model is missing.
loadtest-smoke: ## free|Real WebSocket calls against a fake-provider server (about 1 min)
	@test -f third_party/silero_vad.onnx || { echo "loadtest-smoke: SKIPPED: third_party/silero_vad.onnx missing"; exit 0; }
	@go run ./tools/loadtest -sessions 4 -duration 30s -ramp 3s -turns-per-session 3 -sample-interval 2s -max-p95-v2v 1500 $(LOADTEST_ARGS)

# loadtest-soak: SOAK_DURATION=24h for the full P5-4 acceptance run; default 5m.
# Sample interval is duration/60 clamped to 2s..60s; minimum duration ~1m (ramp + 13 intervals), shorter fails with a clear message.
SOAK_DURATION ?= 5m
SOAK_SESSIONS ?= 16
loadtest-soak: ## free|Soak run (SOAK_DURATION=5m default; SOAK_SESSIONS=16)
	@go run ./tools/loadtest -soak -sessions $(SOAK_SESSIONS) -duration $(SOAK_DURATION) -ramp 30s \
		-max-p95-v2v 1500 -max-rss-growth-pct 10 -max-goroutine-growth 50 -json-out .govoice-loadtest-soak.json -md-out .govoice-loadtest-soak.md $(LOADTEST_ARGS)
	@cat .govoice-loadtest-soak.md

# Perf gate (P5-5, docs/benchmarks/README.md): framework-overhead bench + loadtest smoke vs the
# committed baseline for THIS machine class (docs/benchmarks/baselines/<label>*.json).
# PERF_BASELINE=<label> overrides the label; CI=true fails on a missing baseline unless PERF_ALLOW_MISSING=1.
perf-gate: ## free|Latency/CPU regression check vs this machine's committed baseline
	@scripts/perf-gate.sh gate

# (Re)generate this machine's baseline; FORCE=1 to overwrite. Run on an otherwise idle host.
perf-baseline: ## free|Regenerate this machine's perf baseline (FORCE=1; idle host)
	@scripts/perf-gate.sh baseline

# Proves the gate fails on a 100 ms injected delay and passes without one (~4 min).
perf-gate-selftest: ## free|Prove the perf gate fails on an injected delay (about 4 min)
	@scripts/perf-gate.sh selftest

.PHONY: parity-regen eval-turn eval-pipecat bench-latency loadtest-smoke loadtest-soak perf-gate perf-baseline perf-gate-selftest
.PHONY: dev example-web example-cli example-go

# Offline TTS unit-sizing simulator for unary providers (docs/benchmarks/tts-unit-sizing.md). No network, no keys, no cost.
tts-sim: ## free|Simulate TTS unit sizing strategies for unary providers (TTSSIM_ARGS="-n 300 -cps 40,80,150 -overheads 300,800,1000")
	@go run ./tools/ttssim $(TTSSIM_ARGS)

.PHONY: tts-sim
