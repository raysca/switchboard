# Rehearse the Cloud Run deployment locally

The production image and an nginx that behaves like Cloud Run's front end
(`X-Forwarded-Proto: https`, `X-Forwarded-For`), on `http://localhost:8080`. The server is deployed
clean-room exactly as on Cloud Run: it starts with an empty store and installs no demo.

```sh
cp deploy/rehearsal/.env.example deploy/rehearsal/.env      # fill in; placeholders are enough for a plumbing check
cp /path/to/google-service-account.json deploy/rehearsal/google.json && chmod 644 deploy/rehearsal/google.json
docker compose -f deploy/rehearsal/docker-compose.yaml up --build -d
GOVOICE_MANAGEMENT_API_KEY=<the value in .env> SMOKE_LOCAL=1 deploy/smoke-cloudrun.sh http://127.0.0.1:8080
```

The smoke test makes no demo expectation: the store may be empty. Open <http://localhost:8080>,
enter the management key, and use the first-run wizard (or the API) to create an agent.

Its Postgres lives in memory and is thrown away with the container (Cloud SQL keeps its data).

## Rehearsing a demo (optional)

A demo is deployed and seeded separately from the server, exactly as in production. Deploy its MCP
service to any public HTTPS endpoint (the Cloud Run assets under
[`demos/dental/deploy/cloudrun`](../../demos/dental/deploy/cloudrun) and
[`demos/atlas/deploy/cloudrun`](../../demos/atlas/deploy/cloudrun) do this), then run the seed client
against the rehearsal server through its public listener. The management API and the embedded admin
UI are on the one public port (`public_admin`), so the key is the only thing needed:

```sh
set -a; . deploy/rehearsal/.env; set +a
DENTAL_MCP_TOKEN="<the token the dental MCP service expects>" \
go run ./demos/seed --server http://127.0.0.1:8080 --demo dental \
  --mcp scheduling=https://<dental-mcp-url> --yes   # base URL: seed appends /mcp
```

The `SWITCHBOARD_SECRETS_KEY` in `.env` enables the encrypted secret store that seed writes the demo
token into, and the demo token variable (`DENTAL_MCP_TOKEN` or `ATLAS_MCP_TOKEN`) must match the
token the deployed MCP service checks; without either, seed fails before writing anything. The rehearsal runs the development
profile (so a local MCP fixture in the same network namespace may be reached over loopback HTTP;
`server.yaml` differs from `config/cloudrun.yaml` only in profile, tls and mcp, enforced by a test),
and it does not prove Cloud Run's real proxy chain. On Docker Desktop the client address the server
sees is a bridge address, so `SMOKE_LOCAL=1` downgrades that one check to a warning.
