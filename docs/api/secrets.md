# Secrets API

Credentials that tools and MCP servers send to other services. A tool or server refers to a secret by name; the value is looked up when it is used and is never returned by any endpoint. How the names are found, how the store is sealed and how to rotate the master key is in [Tool and MCP secrets](../security/credentials.md#tool-and-mcp-secrets).

All endpoints need the management key (`Authorization: Bearer <key>`) and use the common envelope of [v1.md](v1.md).

| Method and path | Purpose |
|---|---|
| `GET /api/v1/secrets` | List the secrets the server can resolve, from every backend, without values |
| `PUT /api/v1/secrets/{name}` | Create or replace a stored secret |
| `DELETE /api/v1/secrets/{name}` | Delete a stored secret that nothing uses |

There is no `GET /api/v1/secrets/{name}`: a value cannot be read back.

## List

```json
{ "data": { "store_enabled": true, "secrets": [
  { "name": "MCP_TOKEN_CRM", "source": "environment", "writable": false },
  { "name": "billing-api",   "source": "directory",   "writable": false },
  { "name": "crm-api",       "source": "store",       "writable": true,
    "updated_at": "2026-10-24T09:12:00Z", "last_used_at": "2026-10-24T09:30:11Z" }
], "recent_changes": [
  { "at": "2026-10-24T09:12:00Z", "action": "replace", "name": "crm-api", "request_id": "0b1c…" }
] } }
```

`recent_changes` is the audit trail of the encrypted store, newest first (the last 20): `action` is `create`, `replace` or `delete`, `name` the secret, `request_id` the management request that did it (the `X-Request-ID` of its response, also in the server log). It never holds a value. Each entry is written in the same transaction as the change, so a change without an entry cannot happen; re-sealing under a new master key at start-up is not a change and is not recorded. A caller cannot choose the id: the server generates it. Under concurrent first writes of one name on Postgres, `create` versus `replace` is best effort; the secret itself is always right. Entries are kept until the retention policy covers them (Phase 8 of the roadmap). `store_enabled` is false when `SWITCHBOARD_SECRETS_KEY` is not set; writes then fail with 503. `source` is `environment`, `directory` or `store`. Only environment variables named `MCP_TOKEN_*` or `HTTP_TOKEN_*` and files in `secrets.dir` appear; nothing else in the environment is listed.

## Create or replace

```
PUT /api/v1/secrets/crm-api
{ "value": "…" }
```

The name is 1 to 63 characters: lower-case letters, digits, `-` and `_`, starting with a letter or digit. The value is 1 to 4096 characters with no spaces or control characters. The response carries the name and source, not the value.

| Status and code | Meaning |
|---|---|
| 400 `invalid_secret` | the name or the value is not allowed |
| 409 `secret_managed_externally` | a file of that name exists in `secrets.dir`; the directory is consulted before the store |
| 503 `secrets_store_disabled` | `SWITCHBOARD_SECRETS_KEY` is not set |

## Delete

`204` on success, `404 not_found` when no stored secret has that name, and `409 secret_in_use` with the referencing servers in `error.details` (`kind`, `id`, `name`) while anything uses it (a best-effort check). Environment and directory secrets are changed where they live, not here.

## Using a secret

Set an MCP server's `auth_env` to the secret name (`MCP_TOKEN_CRM`, `billing-api` or `crm-api`). `auth_env_set` in the server view says whether the name currently resolves.
