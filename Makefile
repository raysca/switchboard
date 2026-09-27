export SDKROOT := /Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
export CGO_LDFLAGS := -Wl,-w
export GOVOICE_ROOT := $(CURDIR)
export PATH := $(PATH):$(HOME)/go/bin:/opt/homebrew/bin

build:
	@mkdir -p bin
	go build -o bin/vadworker ./cmd/vadworker
	go build -o bin/server ./cmd/server
	go build -o bin/bench ./cmd/bench

test: build
	@echo "=== Vetting and testing root module ==="
	go vet ./...
	go test -race -count=1 ./voice/
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
	@unformatted=$$(gofmt -l core/ transport/ tools/compat/ proto/ providers/ vad/ cmd/ call/ server/); \
	if [ -n "$$unformatted" ]; then \
		echo "Files need formatting:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "=== Validating Manifest ==="
	node tools/compat/validate_manifest.js
	@echo "=== Verifying Protobuf Fixtures ==="
	node tools/compat/verify_fixtures.js
	@echo "=== Verifying Playback Clear Probe ==="
	node tools/compat/probe_playback_clear.js
	@echo "=== All verify checks passed ==="

proto-gen:
	@mkdir -p core/transport/v1/framespb
	protoc --proto_path=proto --go_out=core/transport/v1/framespb --go_opt=Mframes.proto=govoice/core/transport/v1/framespb --go_opt=paths=source_relative frames.proto

.PHONY: build test verify proto-gen
