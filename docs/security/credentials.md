# Provider credential security

Provider secrets exist only in the server's process environment. There is no provider credential store, no provider credential API and no provider encryption keyring: the agent tables have no secret column, and `GOVOICE_CREDENTIAL_KEYS` is no longer read. [providers.md](../providers.md) lists every variable.

Credentials for tools and MCP servers are a separate class: they belong to the people who author agents, change often, and must not need a redeploy. They are described in [Tool and MCP secrets](#tool-and-mcp-secrets) below and never mix with provider keys.

## What is read, and when

- The packaged provider catalog (`server/catalog`) maps each provider to the variable names it reads. The server reads only those names, plus `MCP_TOKEN_*` for MCP servers.
- Variables are read on demand and never cached: when agent readiness is computed, when `GET /api/v1/providers` is served, and when a call starts. A call assembles the secrets for its own stages (and failover candidates) only, hands them to the provider clients, and zeroes the buffers once the clients are built or the build fails.
- `GOOGLE_APPLICATION_CREDENTIALS` is a path. The server reads only the path an operator put in that variable, only if it is a regular file (a FIFO or device is refused before opening), at most 64 KiB, and accepts it only if it is a JSON object with `"type": "service_account"`.
- A value with whitespace or control characters inside it, or one starting with `$`, is treated as missing rather than used, for optional variables too (a bad `OPENAI_BASE_URL` makes OpenAI undetected instead of sending the key to the default endpoint).
- Overlay catalog files are opened only if they are regular files.

## Who can see it

Nobody, through the server. Management responses, readiness (`missing_env`), errors, logs, traces, the call timeline and metrics name variables, never values. `server/secret_leak_test.go` sets sentinel values for an API key, a service-account file and an MCP token, runs a call on the fake providers, and checks that no sentinel appears in any management route, `/metrics`, `/debug/timeline` or the server log. The VAD and Smart Turn worker processes receive an allowlisted environment (`vad.WorkerEnv`) that excludes every provider variable.

Environment values are server-wide: every agent on the server uses the same key for a provider. There is no per-tenant isolation.

## Tool and MCP secrets

An MCP server names its credential with `auth_env`. The value is a secret name, and the server finds it, in this order, in:

1. **The environment**: `MCP_TOKEN_[A-Z0-9_]{1,48}` and `HTTP_TOKEN_[A-Z0-9_]{1,48}`. Nothing else is ever read from the environment on behalf of a tool, so a management-key holder cannot make the server send a provider key or `GOVOICE_MANAGEMENT_API_KEY` to an endpoint they choose.
2. **A directory of files** (`secrets.dir`): one file per secret, named in lower case (`crm-api`). Point it at a directory used only for tool and MCP secrets: every readable file in it can be named by an MCP server, so a shared mount such as `/run/secrets` would expose its other files. It works on any platform that mounts secrets as files (Kubernetes, Docker and Compose, Nomad, systemd credentials, Cloud Run volumes). The file is read on every use, so a rotated file takes effect without a restart. Symbolic links are followed only while they stay inside the directory.
3. **The encrypted store** in the server's own database (SQLite or Postgres), written through [`/api/v1/secrets`](../api/secrets.md) or the dashboard. It is enabled by one master key in `SWITCHBOARD_SECRETS_KEY`, set once at deployment; adding a secret needs no redeploy.

The names are checked when a server is created and again at every read. Responses carry the name and whether it resolves (`auth_env_set`), never the value. No API reads a secret back.

### The encrypted store

- **Sealing.** Each value is encrypted with AES-256-GCM, a random nonce, and the secret name and key id as associated data, so a row copied under another name does not open. The database holds only the sealed bytes.
- **Master key.** `SWITCHBOARD_SECRETS_KEY` is `id:base64key` with a 32-byte key (for example `k1:` followed by the output of `openssl rand -base64 32`). With the variable unset the store is off: `PUT /secrets/{name}` answers 503 `secrets_store_disabled`, and the environment and directory backends still work. A malformed key stops the server at start.
- **Rotation.** List several keys, newest first: `k2:<new>,k1:<old>`, and restart. New writes use the first key, every key can still open old rows, and the server re-seals every row under the first key at start-up (it logs how many, and any row no configured key can open). Once nothing is left under the old key, drop it at the next restart. Losing every key that sealed a row makes that secret unrecoverable; set it again.
- **What it protects against.** A database dump or backup alone does not reveal a secret. It does not protect against someone who can read both the database and the process environment, or who can call the management API with a tool that sends the secret to a host they control: the master key, the management key and the destination checks (HTTPS, public addresses, no redirects) are the controls.
- **Audit.** Every write and delete is logged with the secret name and request id, never the value. `last_used_at` shows when a secret was last resolved.
- **Deletion.** A secret an MCP server uses cannot be deleted (409 `secret_in_use`, with the servers listed). The check is best effort: a server created in the instant between the check and the delete shows `auth_env_set: false` and the agents using it report it in readiness.
- **Names.** MCP servers may name `MCP_TOKEN_*` variables and lower-case secrets. `HTTP_TOKEN_*` variables are for HTTP tools only and are never sent to an MCP endpoint.

## Rotation

Provider keys: rotation is a redeploy: change the variable (or the service-account file) and restart. (Tool and MCP secrets in the directory or the store rotate without one.) A call already running keeps the clients it built with the old key until it ends. There is no in-place rotate action by design.

## Catalog overlay

`providers.catalog_dir` lets an operator add models, voices and languages. Overlay files are parsed strictly (unknown keys rejected, 64 KiB per file, 500 entries per list) and cannot change which variables a provider reads; a bad file is skipped and logged.

## Upgrading from stored credentials

Earlier builds stored credentials encrypted in the agent store and agents carried `credential_id`. The schema changed in place and nothing is migrated: delete a development database (`.govoice-dev/`), set the provider variables, and recreate agents. A config that still sends `credential_id` is rejected with `400`.
