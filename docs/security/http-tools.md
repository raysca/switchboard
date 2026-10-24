# HTTP tools: security

An HTTP tool makes the server call a URL that someone with the management key chose, with arguments chosen by a model. The controls below keep that from reaching internal services, leaking credentials or exhausting the server. Reference: [api/http-tools.md](../api/http-tools.md).

## Destinations

- **Public HTTPS only.** The URL must be `https` with a host and no user-info, query or fragment. Placeholders are allowed in the path only, so a model's argument can never change the host, the scheme or the port.
- **Checked twice.** The host is resolved and refused unless every address is public (no private, loopback, link-local, carrier-grade NAT, documentation or multicast ranges) when the tool is saved, and again at every connection. The dialer connects to the addresses it checked, so a name that answers differently later cannot reach a private address. This is the same code MCP servers use (`server/egress`).
- **No redirects, no proxy.** A `3xx` answer is a failed call, never followed, and the environment's proxy settings are ignored.
- **Development.** `mcp.allow_local_http` (refused by the production profile) also allows `http` to loopback so a local test service can be called. It is the one switch for MCP servers and HTTP tools.
- **Arguments in the path.** A path value must be a string or an integer, non-empty, not `.` or `..`, with no `/`, `\` or control characters, and is percent-encoded. It cannot add path segments.

## Secrets

- A definition holds a secret *name*. The value is resolved at each call from the environment (`HTTP_TOKEN_*`), the secrets directory or the encrypted store ([credentials](credentials.md#tool-and-mcp-secrets)), and sent in one header: `Authorization: Bearer <value>`, or the raw value in another header you name.
- `MCP_TOKEN_*` names are refused for HTTP tools, and `HTTP_TOKEN_*` names are refused for MCP servers, so one kind of credential is not sent to the other kind of endpoint. Provider keys and the management key can never be named.
- Static headers cannot carry `Authorization`, `Cookie` or other credentials, so a stored definition (which is readable through the API) holds no secret.
- The value never appears in an API response, log, span or error. It is removed from results (as MCP results are), and the test endpoint reports the same sanitised errors.
- Whoever holds the management key can create a tool that sends any resolvable secret to a public host they control. The secret store's controls do not change that: protect the management key, and keep `secrets.dir` for tool secrets only.

## Limits

| Limit | Value |
|---|---|
| Arguments from the model | 64 KiB, and the loop's own argument limit |
| Input schema | 16 KiB, with the MCP schema depth, node and reference limits |
| Time | 100 ms to 30 s per call (default 10 s), ended with the call's own cancellation |
| Response | 256 B to 256 KiB (default 64 KiB); a larger answer fails instead of being cut |
| Headers | 16 static headers, 512 bytes each |

A call reads at most the response limit plus one byte, so a hostile endpoint cannot make the server buffer more. Concurrency is the model loop's: a call runs at most `max_parallel` tool calls at once.

## What is recorded

A tool call stores its tool id and name, status, error category and duration, like an MCP call; the span `http_tool.execute` has the tool id, name and duration. Arguments, results, the URL and secrets are never stored or traced. The agent's runtime snapshot holds the URL template and the secret's name (needed to show which tool a call used), never a value.

## Known limits

- One secret header per tool. Several credentials or signed requests are not supported.
- The destination check at save time resolves DNS; a host that becomes private later is stopped by the dial-time check, not by the saved definition.
- An endpoint can still see the arguments the model sent (they are the request): do not give a tool arguments you would not send to that service.
