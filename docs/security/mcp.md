# MCP transport and security policy

The Server MCP package owns MCP protocol types and converts them into server storage records and API DTOs. Core and provider ToolLoop contracts contain no MCP types. Remote tools are identified by `(MCPServerID, remote tool name)` and every discovery refresh creates a new immutable snapshot with stable canonical schema and discovery hashes.

## Supported transport and authentication

Task 17 supports remote Streamable HTTP only. A configured endpoint must be an HTTPS URL with a host, no embedded URL credentials, and no query parameters. Rejecting queries prevents access tokens from being persisted as part of the endpoint; integrations must use a server-side credential reference instead. Local/process (`stdio`) is explicitly rejected; this avoids turning management access into arbitrary executable and filesystem access. Enabling it later requires a separate admin/config allowlist, process sandbox, working-directory policy, and executable provenance review.

Authentication is by a server-side credential reference. `CredentialResolver` resolves that opaque reference at operation time; the MCP HTTP client attaches the returned token as a Bearer header. The token is never persisted in the MCP endpoint, returned by management APIs, or included in trace attributes or service errors. This initial adapter does not implement OAuth discovery/refresh or arbitrary custom headers. A future credential service can implement token rotation behind the resolver without changing the MCP domain API.

The HTTPS policy rejects URL user-info and cleartext HTTP. The production SDK client also resolves the destination at dial time, rejects private, loopback, link-local, unspecified, and multicast addresses, and dials only one of the checked public addresses. Redirect following and ambient proxy routing are disabled to prevent credential forwarding across destinations. Deployments may inject a stricter `DestinationPolicy`; the default does not allow internal MCP destinations.

## Resource bounds and execution

Each remote operation receives a context deadline (15 seconds by default, capped at two minutes). Cancellation flows into the official SDK client. Tool catalogs are limited to 256 entries; tool descriptions to 4 KiB; schemas to 64 KiB; test arguments to 64 KiB; and returned structured JSON to 256 KiB by default. The SDK event reader has an independent body/event bound. Schemas are resolved without loading remote `$ref` documents, so a schema cannot make the Server fetch arbitrary URLs.

The validation console calls the same `Service.Execute` path as future agent wrappers. The service checks arguments against the discovered JSON Schema before sending a request, and normalizes remote failures to safe categories. Results are size-checked and redact the resolved credential and common secret-shaped fields before returning. Traces contain operation names, duration and tool counts only; arguments, results, endpoints, and credentials are excluded.

## Validation state

The persisted state is one of `not_validated`, `healthy`, `authentication_failed`, `connection_failed`, `discovery_failed`, `tool_schema_invalid`, or `disabled`. The API reports only these values and does not return remote error bodies. A failed refresh never replaces the last successful immutable discovery.

The standalone `mcp.Handler` exposes `/api/v1/mcp-servers` routes and requires the existing `auth.Management` object. It fails closed when management authentication is absent or disabled. Runtime composition should mount it behind the same authenticated management boundary; no public voice listener should mount it.

## SDK choice and adapter boundary

The Server uses the official maintained `github.com/modelcontextprotocol/go-sdk` at v1.8.0. The official SDK repository identifies `mcp` as its primary client/server API and lists v1.7.0+ as supporting protocol version 2026-07-28; v1.8.0 is the current stable SDK selected when Task 17 was implemented. v1.8.0 requires Go 1.25, below this module's Go 1.27.1 baseline. The Server-owned `RemoteClient` adapter isolates SDK churn and keeps MCP types out of core and public storage/API contracts.

The compatibility cost is the SDK's protocol implementation and Go-version dependency; upgrading SDK versions is an explicit Server integration change. The current adapter opens a request-scoped Streamable HTTP session for discovery and tool execution and intentionally does not expose server-initiated notifications or local process management.
