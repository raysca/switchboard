# Task 17: MCP discovery and validation

Status: implementation and focused/full server verification complete; independent MCP/security review pending.

## Delivered

- Added `server/mcp` domain service, narrow server-owned `RemoteClient` adapter, and official Streamable HTTP SDK implementation.
- Added persistent descriptive metadata and validation timestamps for MCP servers and discovered tools, plus repository reads, latest-snapshot lookup, and validation-state updates. Discovery/tool rows remain immutable under existing storage triggers; refresh appends snapshots and updates only the latest pointer.
- Added schema validation with local `$ref` resolution only, deterministic sorting/canonical hashing through the existing storage canonicalizer, and bounded discovery/argument/result policies.
- Added bearer authentication through opaque server-side credential references, safe error categories, result redaction, per-operation deadlines/cancellation, and metadata-only MCP traces.
- Added a management-authenticated, host-injected HTTP handler for list/create/detail/validate/tool-test routes. The current config-driven root server has no persistent Store composition; the handler therefore requires its owning management layer to mount it.
- Added API and threat-policy docs. Local/process transport is explicitly rejected. Remote endpoint configuration requires HTTPS and forbids user-info and query parameters.

## SDK decision and adapter boundary

The server had no MCP SDK dependency. The official `github.com/modelcontextprotocol/go-sdk` repository names `mcp` as its primary package and lists v1.7+ as compatible with the 2026-07-28 MCP protocol. v1.8.0 was the current stable release when implemented, and its Go 1.25 minimum is below the server module's Go 1.27.1 baseline. This updates the frozen v1.7.0 planning pin to current stable; the isolated `RemoteClient` interface limits SDK-specific churn. References: [official SDK repository](https://github.com/modelcontextprotocol/go-sdk), [v1.8.0 package docs](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk@v1.8.0/mcp).

## Assumptions and costs

- v2 Task 17 supports remote Streamable HTTP only. `stdio` is rejected pending a separate executable allowlist, sandbox, filesystem policy, and threat review.
- Authentication currently supports a resolved Bearer token only. OAuth refresh and arbitrary auth headers are deferred behind `CredentialResolver`.
- Remote `$ref` schemas are rejected unless all references resolve within the supplied schema. This prevents schema-triggered network fetches but excludes remote schema catalogs.
- The SDK client blocks ambient proxies and redirects and rejects private/reserved destination addresses at DNS resolution and dial time. This prevents internal-network requests and credential forwarding but requires reachable public hosts without proxy-only routing.
- API handler mounting into the root management listener is owned by the server composition task. Until wired, the MCP handler is an injectable, independently authenticated route set and not reachable from the default listener.

## Tests and verification

- TDD red confirmed: initial `go test ./mcp` failed because the MCP service/client contract was absent.
- Focused: `go test ./mcp ./storage` passed.
- Simulated Streamable HTTP integration uses the official Go MCP SDK server/client path and verifies bearer forwarding, schema discovery, and structured tool execution.
- Service tests cover sorted stable schema hashes, refresh immutability, malformed/oversized schemas, argument validation, oversized results, credential/error redaction, timeout/cancellation, local transport rejection, and unsafe endpoint rejection.
- API tests cover management auth, absent-auth fail-closed behavior, and rejecting endpoint query secrets without returning them.
- Full server: `SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS='-Wl,-w' go test -race -count=1 ./...` passed, including agent, credential, provider, MCP, storage, and server packages.
- Full server vet with the same SDK environment passed; `git diff --check` passed.

## Review

Independent MCP/security review is pending. Apply any findings before the Task 17 commit.
