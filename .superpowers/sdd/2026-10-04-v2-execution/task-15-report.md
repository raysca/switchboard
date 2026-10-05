# Task 15 — Persistent agents and immutable versions

## Result

Implemented persistent agent services and management handlers in `server/agent` and `server/api/agents.go`. The service supports create/list/get/update, archive, duplicate, V1 pipeline import, readiness, current runtime resolution, session pinning, and historical session snapshot resolution. `storage.AgentRepository` now lists and updates mutable agent fields and enforces contiguous monotonic version numbers while serializing allocation per agent. Config versions continue to use the existing storage canonical JSON/SHA-256 implementation and database immutability triggers.

The handler exposes `GET/POST /api/v1/agents`, `GET/PUT/PATCH /api/v1/agents/{id}`, archive and duplicate actions, and `POST /api/v1/agents/import-v1`. It is dependency-injected with `agent.Service`; the existing V1 server configuration does not create a product database, so a V2 composition layer must mount the handler behind the management auth boundary.

## Test evidence

Focused RED/GREEN coverage includes CRUD and paged listing, canonical config hash stability across key order, version increment and historical immutability, V1 import, incomplete-config readiness with safe bounded reasons, duplicate/archive behavior, session snapshot stability after agent update, invalid update atomicity, and disabled-state preservation during metadata-only updates. API tests cover the HTTP CRUD/import/actions and unknown-field/not-found behavior.

Passed:

- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race ./agent ./api -count=1`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race ./storage -run 'TestRepositoryAgentVersion|TestRepositoryAgentVersionsAreMonotonic' -count=1`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go vet ./agent ./api ./storage`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race -count=1 ./...`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go vet ./...`
- `gofmt` and `git diff --check`

The full server gate was first attempted while concurrent Task 16/17 files were in flight and failed on their incomplete module/import/migration edits. After those parallel tasks settled, the full race suite and `go vet ./...` both passed.

## Decisions and assumptions

- Agent config is a JSON object and can be incomplete when saved; incomplete voice stack state becomes `not_ready` rather than rejecting a useful draft. Canonical JSON key ordering and hashing reuse the storage foundation.
- Initial readiness checks only for nonempty `stt`, `llm`, and `tts` identifiers plus disabled/archive state. Safe reason codes are bounded to eight and never echo configuration values. Credential/model checks and MCP tool readiness are owned by later services.
- A config replacement publishes the next contiguous immutable version even if its canonical hash matches an earlier version. Mutable metadata-only updates preserve the version pointer.
- V1 import copies pipeline fields (`stt`, `llm`, `tts`, `max_messages`, `max_tokens`) into runtime config; the legacy pipeline key is only validated and omitted from the hashed runtime object.
- The existing V1 server has no configured persistent Store, so this task provides the management handler through constructor injection. Runtime composition will mount it only after authenticating management requests.
- Session snapshot persistence stores the immutable version ID and config hash already supported by the session schema; resolution verifies the session's agent/version/hash tuple before returning config.

## Review notes

Independent review remains a parent/orchestration gate after this implementation commit.
