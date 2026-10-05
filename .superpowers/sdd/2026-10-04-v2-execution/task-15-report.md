# Task 15 — Persistent agents and immutable versions

## Result

Implemented persistent agent services and management handlers in `server/agent` and `server/api/agents.go`. The service supports create/list/get/update, archive, duplicate, V1 pipeline import, readiness, current runtime resolution, session pinning, and historical session snapshot resolution. `storage.AgentRepository` now lists and updates mutable agent fields and enforces contiguous monotonic version numbers while serializing allocation per agent. Config versions continue to use the existing storage canonical JSON/SHA-256 implementation and database immutability triggers.

The handler exposes `GET/POST /api/v1/agents`, `GET/PUT/PATCH /api/v1/agents/{id}`, archive and duplicate actions, and `POST /api/v1/agents/import-v1`. `Server.SetAgentManagementAPI` injects it and dispatches only after management authentication. The existing V1 server configuration does not create a product database; the host supplies the store-backed service before serving requests.

## Test evidence

Focused RED/GREEN coverage includes CRUD and paged listing, canonical config hash stability across key order, version increment and historical immutability, V1 import, incomplete-config readiness with safe bounded reasons, duplicate/archive behavior, session snapshot stability after agent update, publication-failure rollback for create and update, concurrent contiguous version allocation, concurrent partial updates preserving omitted fields, and disabled-state preservation during metadata-only updates. An injected readiness evaluator returns only safe fixed reason codes; without it, complete stack configuration is reported as `configured`, never as externally `ready`. The real-server test covers authenticated route dispatch, wrong/voice credential rejection, public-listener exclusion, and request-ID context propagation despite an attacker-supplied header.

Passed:

- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race ./agent ./api -count=1`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race ./ -run TestAgentRoutesUseManagementBoundaryAndTrustedRequestID -count=1`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race ./agent -run 'TestConcurrent(ConfigUpdatesAllocateContiguousVersions|PartialUpdatesPreserveOmittedFields)' -count=30`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race ./storage -run 'TestRepositoryAgentVersion|TestRepositoryAgentVersionsAreMonotonic' -count=1`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go vet ./agent ./api ./storage`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race -count=1 ./...`
- `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go vet ./...`
- `gofmt` and `git diff --check`

The full server race/vet gate passed after Task 16 settled. A later rerun during Task 17's active edits failed only on transient, uncommitted `server/mcp` compile issues (`net/netip` unused, missing `regexp`, `classifiedStatusTransport`, `errors`, and `isPublicEgressIP`). Those MCP files are outside this fix and were left untouched; the parent will rerun the complete gate after Task 17 settles.

## Decisions and assumptions

- Agent config is a JSON object and can be incomplete when saved; incomplete voice stack state becomes `not_ready` rather than rejecting a useful draft. Canonical JSON key ordering and hashing reuse the storage foundation.
- Initial readiness checks only for nonempty `stt`, `llm`, and `tts` identifiers plus disabled/archive state. Safe reason codes are bounded to eight and never echo configuration values. Complete config without an injected evaluator reports `configured`; only an evaluator may promote it to `ready`, and evaluator failure blocks new runtime resolution with fixed safe codes. Credential/model checks and MCP tool readiness are owned by later services.
- A config replacement publishes the next contiguous immutable version even if its canonical hash matches an earlier version. Mutable metadata-only updates preserve the version pointer.
- V1 import copies pipeline fields (`stt`, `llm`, `tts`, `max_messages`, `max_tokens`) into runtime config; the legacy pipeline key is only validated and omitted from the hashed runtime object.
- The existing V1 server has no configured persistent Store, so this task adds handler injection rather than implicit database setup. The management boundary authenticates first, then passes a generated request ID through context; the agent handler never trusts the request-ID header.
- Session snapshot persistence stores the immutable version ID and config hash already supported by the session schema; resolution verifies the session's agent/version/hash tuple before returning config.

## Review notes

The independent reviewer requested atomic create/update publication, strict draft readiness, a provider/MCP readiness seam, authenticated Server mounting and trusted request-ID propagation, and removal of a cross-task helper dependency. This fix implements those items. Independent re-review remains the parent/orchestration gate.
