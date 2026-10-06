UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
  SDKROOT_CANDIDATE := /Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
  ifneq ($(wildcard $(SDKROOT_CANDIDATE)),)
    export SDKROOT := $(SDKROOT_CANDIDATE)
  endif
  export CGO_LDFLAGS := -Wl,-w
endif
export GOVOICE_ROOT := $(CURDIR)
export PATH := $(PATH):$(HOME)/go/bin:/opt/homebrew/bin

build:
	@mkdir -p bin
	go build -o bin/vadworker ./cmd/vadworker
	go build -o bin/server ./cmd/server
	go build ./examples/...

test: build
	@echo "=== Vetting and testing root module ==="
	go vet ./...
	go test -race -count=1 ./vad/
	go test -race -count=1 ./tools/compat/...
	go test -race -count=1 ./transport/...
	@echo "=== Vetting and testing core module ==="
	cd core && go vet ./...
	cd core && go test -race -count=1 ./...
	@echo "=== Vetting and testing providers/google module ==="
	cd providers/google && go vet ./...
	cd providers/google && go test -race -count=1 ./...
	@echo "=== Vetting and testing providers/xai module ==="
	cd providers/xai && go vet ./...
	cd providers/xai && go test -race -count=1 ./...
	@echo "=== Vetting and testing server module ==="
	cd server && go vet ./...
	cd server && go test -race -count=1 ./...

verify: test
	@echo "=== Checking code formatting ==="
	@unformatted=$$(gofmt -l core/ transport/ tools/compat/ proto/ providers/ vad/ cmd/ server/ examples/); \
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
