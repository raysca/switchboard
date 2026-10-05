# Task 16 — Provider credentials, capability registry, and discovery

## Status

Implementation and focused verification are complete. The full server race suite and `go vet` pass. An independent security review is still pending before this task's commit.

## Delivered

- `server/credential` stores provider secrets with AES-256-GCM, random nonces, explicit key IDs and associated data binding credential ID/provider/key ID. Decryption rejects tampering, wrong keys and unavailable key IDs. Rotation re-encrypts using the active key and compare-and-replace to avoid overwriting a concurrent secret update.
- Credential create, update, delete, list and masked read APIs use persistent storage. The DTO contains no plaintext field; short secrets are fully masked. Validation has explicit `unvalidated`, `valid`, `invalid` and `error` states and never exposes validator error text.
- `server/provider` has an explicit, sorted registry for Google and xAI capabilities, plus context-bounded, sorted, length-limited and sanitized model/voice discovery. Provider adapter failures and panics become a generic safe error.
- Authenticated `/api/v1/providers` routes cover provider metadata, credential lifecycle, explicit validation, key rotation, and model/voice discovery. Handler injection is host-configured; no automatic live key probe is assumed.
- Per-capability agent readiness is computed separately from server liveness/readiness.
- Credential encryption, key rotation and operating guidance are documented in `docs/security/credentials.md`; management routes are documented in `docs/api/v1.md`.

## Test-first evidence

Before implementation, `go test ./credential ./provider` failed to build because `NewRegistry`, capability definitions, discovery types and credential service symbols did not exist. After implementation, the same suites passed. Additional tests cover ciphertext tampering, wrong key, authenticated metadata tampering, rotation, redacted logging/API DTOs, invalid/valid/unsupported validation results, capability readiness, deterministic bounded discovery, sanitized provider failures, and persistent repository compare-and-replace/delete behavior.

## Verification

From `server/`, with `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk` and `CGO_LDFLAGS=-Wl,-w`:

- `go test -race -count=1 ./...` — PASS (including agent/MCP sibling packages present in the shared workspace)
- `go vet ./...` — PASS
- `git diff --check` — PASS

## Assumptions and boundaries

- The host injects storage, encryption keys, validators and discovery adapters before serving. This task does not add a deployment-specific KMS/environment key-loading mechanism; the security document specifies the boundary and rotation procedure.
- Google and xAI are the initial registry entries, each statically advertising LLM/STT/TTS support as selectable capabilities. This catalog does not assert account-level entitlement.
- There is no built-in live provider key validation. Without an explicitly configured adapter, validation/discovery is unavailable; creating a credential does not silently make it valid.
- Discovery adapters must honor context cancellation. Result counts and output fields are bounded/sanitized after return; a Go adapter that ignores cancellation can still hold its calling goroutine.
- Shared files `server/storage/repositories.go` and `docs/api/v1.md` also contain concurrent Task15/17 material in this checkout. No files have been staged by this worker; commit ownership must be reconciled with the parent before staging shared paths.

## Review risks to check

- Confirm AES-GCM key provisioning and rotation procedure are sufficient for deployment operations, since key loading intentionally remains host-owned.
- Confirm the initial static Google/xAI capability set matches the target provider integrations.
- Consider whether provider credential deletion should map database foreign-key restrictions to a dedicated `409 in_use` response when credentials are associated with MCP servers.
