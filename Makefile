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

build:
	@mkdir -p bin
	go build -o bin/vadworker ./cmd/vadworker
	go build -o bin/turnworker ./cmd/turnworker
	go build -o bin/server ./cmd/server
	go build ./examples/...

# Every module in go.work (root also covers vad, smartturn, transport, tools/*, cmd, examples).
MODULES := . core providers/cartesia providers/deepgram providers/elevenlabs providers/google providers/nvidia providers/openai providers/xai server

test: build
	@failed=""; for m in $(MODULES); do \
		echo "=== Vetting and testing module $$m ==="; \
		(cd $$m && go vet ./... && go test -race -count=1 ./...) || failed="$$failed $$m"; \
	done; \
	if [ -n "$$failed" ]; then echo "=== FAILED modules:$$failed ==="; exit 1; fi; \
	echo "=== All modules passed ==="

# Black-box pipecat.evals run against the Go server with fake providers (docs/decisions/pipecat-evals.md).
eval-pipecat:
	@./tools/eval/pipecat/run.sh

# Turn-quality gate (docs/benchmarks/turn-quality.md): fake oracles, no cost; fails on threshold regression.
eval-turn:
	@go run ./tools/eval/cmd/govoice-turn-eval

# Paired Pipecat fixtures (docs/pipecat-parity-matrix.md): re-executes the pinned pipecat-impl components
# from its venv (`cd pipecat-impl && uv sync` once) and rewrites tools/compat/testdata/parity/*.json.
# Offline and deterministic; `go test ./tools/compat/...` compares go-voice against the committed goldens.
parity-regen:
	@test -x pipecat-impl/.venv/bin/python || { echo "pipecat-impl/.venv missing: cd pipecat-impl && uv sync"; exit 1; }
	cd pipecat-impl && .venv/bin/python ../tools/compat/parity/gen_goldens.py

verify: test
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

release-security:
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

proto-gen:
	@mkdir -p core/transport/v1/framespb
	protoc --proto_path=proto --go_out=core/transport/v1/framespb --go_opt=Mframes.proto=govoice/core/transport/v1/framespb --go_opt=paths=source_relative frames.proto

.PHONY: build test verify release-security proto-gen

# Postgres storage tests (skipped by `make test`). Uses TEST_POSTGRES_DSN if set,
# otherwise starts a throwaway postgres container.
PG_IMAGE ?= postgres:16-alpine
test-postgres:
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
e2e:
	cd e2e && npm install --no-audit --no-fund && npx playwright install chromium && npx playwright test

.PHONY: e2e

# Local development (scripts/dev.sh): `make dev` runs the server and admin UI with
# generated secrets and a local SQLite store in .govoice-dev/, seeded with a
# "Dev Assistant" agent from examples/.env. Examples call that agent.
dev:
	@./scripts/dev.sh up

example-web:
	@./scripts/dev.sh web

example-cli:
	@TEXT="$(TEXT)" ./scripts/dev.sh cli

example-go:
	@./scripts/dev.sh go "$(NAME)"

# Opt-in LIVE check: real Google STT/LLM/TTS (spends API credit, ~60 provider calls) driving
# turn scenarios against a real server with turn.manager off/on/smart-turn
# (docs/benchmarks/turn-manager-live.md). Needs examples/.env; refuses without GOVOICE_LIVE=1.
# LIVE_ARGS e.g. "-only S1,S2 -variants off,plain -stt-model latest_long -trace".
live-check:
	@test "$$GOVOICE_LIVE" = 1 || { echo "live-check spends real Google API credit: run GOVOICE_LIVE=1 make live-check"; exit 1; }
	@test -f examples/.env || { echo "live-check needs examples/.env (see examples/.env.example)"; exit 1; }
	@go run ./tools/livecheck $(LIVE_ARGS)

.PHONY: live-check

# Framework latency baseline with fake providers (docs/benchmarks/baseline.md).
bench-latency:
	@go run ./tools/eval/cmd/govoice-bench $(BENCH_ARGS)

# Load/soak harness: real WebSocket calls against a -tags fakeproviders server child
# (docs/benchmarks/capacity.md). Fake providers, no network, no cost.
# loadtest-smoke: ~1 min, CI-friendly. Skips when the Silero model is missing.
loadtest-smoke:
	@test -f third_party/silero_vad.onnx || { echo "loadtest-smoke: SKIPPED: third_party/silero_vad.onnx missing"; exit 0; }
	@go run ./tools/loadtest -sessions 4 -duration 30s -ramp 3s -turns-per-session 3 -sample-interval 2s -max-p95-v2v 1500 $(LOADTEST_ARGS)

# loadtest-soak: SOAK_DURATION=24h for the full P5-4 acceptance run; default 5m.
# Sample interval is duration/60 clamped to 2s..60s; minimum duration ~1m (ramp + 13 intervals), shorter fails with a clear message.
SOAK_DURATION ?= 5m
SOAK_SESSIONS ?= 16
loadtest-soak:
	@go run ./tools/loadtest -soak -sessions $(SOAK_SESSIONS) -duration $(SOAK_DURATION) -ramp 30s \
		-max-p95-v2v 1500 -max-rss-growth-pct 10 -max-goroutine-growth 50 -json-out .govoice-loadtest-soak.json -md-out .govoice-loadtest-soak.md $(LOADTEST_ARGS)
	@cat .govoice-loadtest-soak.md

# Perf gate (P5-5, docs/benchmarks/README.md): framework-overhead bench + loadtest smoke vs the
# committed baseline for THIS machine class (docs/benchmarks/baselines/<label>*.json).
# PERF_BASELINE=<label> overrides the label; CI=true fails on a missing baseline unless PERF_ALLOW_MISSING=1.
perf-gate:
	@scripts/perf-gate.sh gate

# (Re)generate this machine's baseline; FORCE=1 to overwrite. Run on an otherwise idle host.
perf-baseline:
	@scripts/perf-gate.sh baseline

# Proves the gate fails on a 100 ms injected delay and passes without one (~4 min).
perf-gate-selftest:
	@scripts/perf-gate.sh selftest

.PHONY: parity-regen eval-turn eval-pipecat bench-latency loadtest-smoke loadtest-soak perf-gate perf-baseline perf-gate-selftest
.PHONY: dev example-web example-cli example-go
