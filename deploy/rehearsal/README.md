# Rehearse the Cloud Run deployment locally

The production image, the dentistry MCP server and an nginx that behaves like Cloud Run's front end
(`X-Forwarded-Proto: https`, `X-Forwarded-For`), on `http://localhost:8080`.

```sh
cp deploy/rehearsal/.env.example deploy/rehearsal/.env      # fill in; placeholders are enough for a plumbing check
cp /path/to/google-service-account.json deploy/rehearsal/google.json && chmod 644 deploy/rehearsal/google.json
docker compose -f deploy/rehearsal/docker-compose.yaml up --build -d
GOVOICE_MANAGEMENT_API_KEY=<the value in .env> SMOKE_LOCAL=1 deploy/smoke-cloudrun.sh http://127.0.0.1:8080
```

Open <http://localhost:8080>, enter the management key, and the "Smile Dental front desk" flow is there
(installed on start; the server retries for up to a minute while the MCP server comes up). With real keys,
make the two demo calls from the Test call panel (see `docs/demo/dentistry.md`).

What it does not prove: the production profile (the rehearsal runs the development profile so the MCP
server can be reached over loopback; `server.yaml` differs from `config/cloudrun.yaml` only in profile,
tls and mcp, enforced by a test) and Cloud Run's real proxy chain. On Docker Desktop the client address
the server sees is a bridge address, so `SMOKE_LOCAL=1` downgrades that one check to a warning.
