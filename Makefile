export SDKROOT := /Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
export CGO_LDFLAGS := -Wl,-w
export GOVOICE_ROOT := $(CURDIR)

build:
	go build -o bin/vadworker ./cmd/vadworker
	go build -o bin/server ./cmd/server
	go build -o bin/bench ./cmd/bench

test: build
	go vet ./...
	go test -race -count=1 ./voice/
	go test -count=1 ./vad/

.PHONY: build test
