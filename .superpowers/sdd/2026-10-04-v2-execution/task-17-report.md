# Task 17: MCP discovery and validation

Status: implementation, independent review remediations, and final race/vet verification complete.

## Delivered

- Added `server/mcp` domain service, narrow server-owned `RemoteClient` adapter, and official Streamable HTTP SDK implementation.
- Added persistent descriptive metadata and validation timestamps for MCP servers and discovered tools, plus repository reads, latest-snapshot lookup, and validation-state updates. Discovery/tool rows remain immutable under existing storage triggers; refresh appends snapshots and updates only the latest pointer.
- Added schema validation with local `$ref` resolution only, deterministic sorting/canonical hashing through the existing storage canonicalizer, and bounded discovery/argument/result policies.
- Added bearer authentication through opaque server-side credential references, safe error categories, result redaction, per-operation deadlines/cancellation, and metadata-only MCP traces.
- Added a management-authenticated, host-injected HTTP handler for list/create/detail/validate/tool-test routes. `Server.SetMCPManagementAPI` mounts it through the admin listener after outer auth and propagates the trusted request ID. The config-driven root server does not instantiate MCP automatically because it has no persistent Store composition.
- Added API and threat-policy docs. Local/process transport is explicitly rejected. Remote endpoint configuration requires HTTPS and forbids user-info and query parameters.

## SDK decision and adapter boundary

The server had no MCP SDK dependency. The official `github.com/modelcontextprotocol/go-sdk` repository names `mcp` as its primary package and lists v1.7+ as compatible with the 2026-07-28 MCP protocol. v1.8.0 was the current stable release when implemented, and its Go 1.25 minimum is below the server module's Go 1.27.1 baseline. This updates the frozen v1.7.0 planning pin to current stable; the isolated `RemoteClient` interface limits SDK-specific churn. References: [official SDK repository](https://github.com/modelcontextprotocol/go-sdk), [v1.8.0 package docs](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk@v1.8.0/mcp).

## Assumptions and costs

- v2 Task 17 supports remote Streamable HTTP only. `stdio` is rejected pending a separate executable allowlist, sandbox, filesystem policy, and threat review.
- Authentication currently supports a resolved Bearer token only. OAuth refresh and arbitrary auth headers are deferred behind `CredentialResolver`.
- Remote `$ref` schemas are rejected unless all references resolve within the supplied schema. This prevents schema-triggered network fetches but excludes remote schema catalogs.
- Schema preflight accepts only the three explicit dialect URIs supported by `jsonschema-go` v0.4.3 (or an omitted dialect). It rejects dynamic/recursive references, anchors, resource IDs, every `$ref` cycle, and dollar-prefixed keys outside `$schema`/`$ref`/`$defs`/`$comment`. Root-local JSON Pointers may target object or boolean schemas. This intentionally narrows compatibility, including within annotations.
- Stored schema JSON is capped at 4,096 values, 128 references, and depth 64. Memoized expansion costs charge every repeated reference/combinator branch against 16,384 values and depth 64 before validator resolution; unused definitions and annotations count conservatively. This rejects exponential acyclic DAGs without expanding them. Duplicate JSON fields use the same decoded tree for preflight and validation; canonical schema rechecking catches typed-decoder keyword alias merging. Context deadlines do not interrupt synchronous validation mid-call.
- The SDK client blocks ambient proxies and redirects and rejects private/reserved destination addresses at DNS resolution and dial time. This prevents internal-network requests and credential forwarding but requires reachable public hosts without proxy-only routing.
- Server/tool names in trace metadata are sanitized; a generated opaque execution ID is shared across the trace event and response. Caller span context is preserved. Results are parsed and reserialized after recursive secret-key and decoded-credential redaction.
- API handler mounting into the root management listener is owned by the server composition task. Until wired, the MCP handler is an injectable, independently authenticated route set and not reachable from the default listener.

## Tests and verification

- TDD red confirmed: initial `go test ./mcp` failed because the MCP service/client contract was absent.
- Focused: `go test ./mcp ./storage` passed.
- Simulated Streamable HTTP integration uses the official Go MCP SDK server/client path and verifies bearer forwarding, schema discovery, and structured tool execution.
- Service tests cover sorted stable schema hashes, refresh immutability, malformed/oversized schemas, argument validation, oversized results, credential/error redaction, timeout/cancellation, local transport rejection, and unsafe endpoint rejection.
- API tests cover management auth, absent-auth fail-closed behavior, and rejecting endpoint query secrets without returning them.
- Full server: `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS='-Wl,-w' go test -race -count=1 ./...` passed, including agent, credential, provider, MCP, storage, and server packages.
- Full server vet with the same SDK environment passed; `git diff --check` passed.
- Review remediation verification: `go test -race -count=10 ./mcp` passed; final focused `go test -race -count=1 ./mcp` and full-server `go vet ./...` passed with the same SDK environment. The default Command Line Tools SDK link failed against its macOS 27 TBD stubs; the required Xcode macOS 26 SDK workaround passed.
- Additional correction RED tests reproduced accepted dynamic references and exponential acyclic reference graphs, typed connection/initialization failures overwritten by discovery fallback, mismatched dialect admission, and execution duration missing from ended asynchronous spans. Further regressions exposed duplicate-field and mixed-case keyword graph mismatches. Subprocess tests reject recursive/dynamic schemas and `allOf`/`anyOf`/`oneOf` exponential DAGs; accepted dialects execute `{}`, bounded shared refs and empty-schema refs remain usable, and rejected refreshes retain the last successful snapshot.
- Additional correction final verification passed with the Xcode SDK environment above: `go test -race -count=10 ./mcp` (57.877s), `go test -race -count=1 ./...` (all server packages), `go vet ./...`, and `go build ./...`. `git diff --check` also passed. Generated/unowned `go.work.sum` changes were excluded from the correction commit.

## Review

Independent review findings were remediated: bounded/cycle-safe schema preflight, recursive result redaction, wire DTO/integer preservation, typed SDK failures, authenticated root-listener mounting and request-ID propagation, bounded session DELETE cleanup, common DNS/dial public-egress enforcement, and correlation-safe execution identity/tracing. The additional correction bounds expanded reference/combinator work, excludes unsupported reference semantics, matches the validator's actual dialect support, preserves typed connection/initialization state priority, and records trace duration before every span end.

## Independent closeout

The final independent security review APPROVED `480deff` after adversarial
schema probes confirmed canonical parser/preflight alignment, rejection of
dynamic and noncanonical reference semantics, conservative expanded-work
bounds, exact supported-dialect admission, corrected failure-state priority,
and duration-before-End trace export.

The final independent tester passed MCP race tests five times, the full Server
race suite, vet/build, dependency isolation, and clean exact-commit checks. A
first detached checkout under `/tmp` exposed only the pre-existing macOS
`/tmp`→`/private/tmp` storage-path assertion; the identical full suite passed
from a checkout under `/Users/raymondottun/Projects`. Task 17 is complete.
