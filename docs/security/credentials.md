# Provider credential security

Provider secrets exist only in the server's process environment. There is no credential store, no credential API and no encryption keyring: the database has no secret column, and `GOVOICE_CREDENTIAL_KEYS` is no longer read. [providers.md](../providers.md) lists every variable.

## What is read, and when

- The packaged provider catalog (`server/catalog`) maps each provider to the variable names it reads. The server reads only those names, plus `MCP_TOKEN_*` for MCP servers.
- Variables are read on demand and never cached: when flow readiness is computed, when `GET /api/v1/providers` is served, and when a call starts. A call assembles the secrets for its own stages (and failover candidates) only, hands them to the provider clients, and zeroes the buffers once the clients are built or the build fails.
- `GOOGLE_APPLICATION_CREDENTIALS` is a path. The server reads only the path an operator put in that variable, only if it is a regular file (a FIFO or device is refused before opening), at most 64 KiB, and accepts it only if it is a JSON object with `"type": "service_account"`.
- A value with whitespace or control characters inside it, or one starting with `$`, is treated as missing rather than used, for optional variables too (a bad `OPENAI_BASE_URL` makes OpenAI undetected instead of sending the key to the default endpoint).
- Overlay catalog files are opened only if they are regular files.

## Who can see it

Nobody, through the server. Management responses, readiness (`missing_env`), errors, logs, traces, the call timeline and metrics name variables, never values. `server/secret_leak_test.go` sets sentinel values for an API key, a service-account file and an MCP token, runs a call on the fake providers, and checks that no sentinel appears in any management route, `/metrics`, `/debug/timeline` or the server log. The VAD and Smart Turn worker processes receive an allowlisted environment (`vad.WorkerEnv`) that excludes every provider variable.

Environment values are server-wide: every flow on the server uses the same key for a provider. There is no per-tenant isolation.

## MCP bearer tokens

An MCP server names its token with `auth_env`, which must match `MCP_TOKEN_[A-Z0-9_]{1,48}`. The prefix is checked when the server is created and again every time the token is read, so a management-key holder can never make the server send an unrelated secret (a provider key, `GOVOICE_MANAGEMENT_API_KEY`) to an endpoint they choose, even through a row written straight into the database. Responses carry `auth_env` and `auth_env_set`, never the token.

## Rotation

Rotation is a redeploy: change the variable (or the service-account file) and restart. A call already running keeps the clients it built with the old key until it ends. There is no in-place rotate action by design.

## Catalog overlay

`providers.catalog_dir` lets an operator add models, voices and languages. Overlay files are parsed strictly (unknown keys rejected, 64 KiB per file, 500 entries per list) and cannot change which variables a provider reads; a bad file is skipped and logged.

## Upgrading from stored credentials

Earlier builds stored credentials encrypted in the flow store and flows carried `credential_id`. The schema changed in place and nothing is migrated: delete a development database (`.govoice-dev/`), set the provider variables, and recreate flows. A config that still sends `credential_id` is rejected with `400`.
