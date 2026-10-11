# Credentials Epic: Provider Catalog and Environment-Only Credentials Design

**Date:** 2026-10-11
**Status:** direction approved in conversation (env-only, nothing stored); awaiting written-spec review.

## Intent

Provider secrets are never stored by go-voice. They live in the process environment (`XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_API_KEY`, ...) and are read when a call needs them, so a new deployment rotates them and nothing secret sits in a database. From the variables that are present, the server works out which providers are available and which services (STT, LLM, TTS) each one unlocks, using a catalog packaged with the binary. The catalog also holds the curated models and voices. The admin UI offers only what was detected, as dropdowns. Adding a provider, a model or a voice is a data change (a brand-new provider still needs its adapter). Success:
- with only `XAI_API_KEY` set, the UI offers xAI and nothing else, with its curated models and voices, and an agent built from it takes a call without anyone entering a secret anywhere;
- there is no credential table, no encryption key, no credential API and no credential screen: the code that stored secrets is deleted;
- an agent never names a credential: it names a provider, a model and a voice, and the secret is found by provider and service;
- a model or voice is added by editing a YAML file, with startup validation that catches mistakes.

Decisions made with the user on 2026-10-11:
1. **Env only, never stored.** Secrets stay in the live environment, because of risk and because a new deployment can rotate them.
2. **MCP bearer tokens also come from the environment**, from variables with a required prefix (`MCP_TOKEN_`).
3. **No data migration: the database is assumed clean** (nothing has been deployed). The initial schema is edited in place; developers delete their dev directory (`.govoice-dev`).

## Current behaviour (main at 6207869)

- Provider credentials are encrypted rows in `provider_credentials` (`server/credential`: AES-GCM keyring from `GOVOICE_CREDENTIAL_KEYS` or a generated dev key), managed through `/api/v1/providers/credentials` and the Providers page (add, rotate, update, delete, validate). Each stored row has a provider, a masked suffix and a validation state; there is no label.
- An agent stage carries `provider`, `model`/`voice` and an explicit `credential_id`. `agent.ResolveCredential` checks the id against stored rows; `Server.resolveStageSecrets` decrypts it per call and again when a graph node is activated; failover candidates carry their own credential ids.
- MCP servers have `auth_credential_id` (a stored credential of the `mcp` provider); its plaintext is sent to the MCP endpoint as a bearer token.
- The providers list was a hard-coded `provider.Definitions()` (id, display name, capabilities); xAI's STT/TTS capability depended on the `xaispeech` build tag through a package variable. There is no model or voice list on the server; the UI uses free-text fields.
- The only env-to-credential mapping is `tools/livecheck/creds.go` (`credSpecs`) and `scripts/dev-seed.mjs`; the harnesses store credentials through the API and strip provider variables from the server's environment.

## Design

### 1. The catalog: one YAML file per provider, packaged with the server

`server/catalog/data/<provider>.yaml`, embedded with `go:embed`. Adding a provider is adding a file.

```yaml
id: xai
name: xAI
credentials:                       # the kinds of secret this provider uses
  - id: key
    label: xAI API key
    fields:
      api_key: {env: XAI_API_KEY}  # required unless `optional: true`
    assemble: token                # how the fields become the secret the adapter parses
services:
  llm:
    credential: [key]              # kinds that satisfy this service, in order of preference
    models:
      - {id: grok-2-1212, label: Grok 2, default: true}
  stt:
    credential: [key]
    models:
      - {id: grok-voice-transcribe-2.0, label: Grok voice transcribe 2.0, default: true}
    languages: [{id: en, label: English, default: true}]   # optional
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
      - {id: ara, label: Ara}
```

- **Services** are `stt`, `llm`, `tts`. A service has `credential` (acceptable kinds), `models`, `voices` (TTS), `languages` (optional), `needs` (optional: field ids that must be present for this service, for NVIDIA's per-stage function ids) and `custom` (default `true`: the UI may offer "Other…").
- **At most one `default` per list;** the default is preselected in the UI.
- **Assemblers** are a closed set in code: `token` (the single field's value), `json` (a JSON object of the fields that are present), `file_json` (the field's value is a path; read the file, validate and compact its JSON). A new secret shape needs a new assembler; a provider that fits an existing shape is data only.
- **Seed content,** verified against the adapters and `tools/livecheck/profiles.yaml`. The lists are deliberately short and are the main item to review (**A3**):

| Provider | Secret kinds (env) | STT | LLM | TTS |
|---|---|---|---|---|
| google | `api_key` (`GOOGLE_API_KEY`, token), `service_account` (`GOOGLE_APPLICATION_CREDENTIALS`, file_json) | `chirp_2` (default), `latest_long`; kind `service_account` | `gemini-2.5-flash`; kinds `api_key` then `service_account` | `en-US-Chirp3-HD-Aoede`; kind `service_account` |
| xai | `key` (`XAI_API_KEY`) | `grok-voice-transcribe-2.0` | `grok-2-1212` | voices `eve`, `ara` |
| elevenlabs | `key` (`ELEVENLABS_API_KEY`) | `scribe_v2_realtime` | none | voice `21m00Tcm4TlvDq8ikWAM` |
| deepgram | `key` (`DEEPGRAM_API_KEY`) | `nova-3` | none | none |
| cartesia | `key` (`CARTESIA_API_KEY`) | `ink-whisper` | none | voice `694f9389-aac1-45b6-b726-9d9369183238` |
| openai | `key` (`OPENAI_API_KEY`, optional `OPENAI_BASE_URL`; json) | none | `gpt-4o-mini` | none |
| nvidia | `key` (`NVIDIA_API_KEY`, token), `riva` (`NVIDIA_API_KEY`, `NVIDIA_STT_FUNCTION_ID`, `NVIDIA_TTS_FUNCTION_ID`; json) | kind `riva`, needs `function_id`; language | `openai/gpt-oss-20b`; kind `key` | `Magpie-Multilingual.EN-US.Aria`; kind `riva`, needs `tts_function_id` |

The providers API is built from the catalog (display name, capabilities from the declared services) and `server/provider` is deleted: one source of truth. The `mcp` pseudo-provider disappears (MCP tokens are not provider credentials any more, section 5).

**Operator overlay (A1).** An optional directory `providers.catalog_dir` in the server config. Files there are merged over the packaged ones by provider id: `models`/`voices`/`languages` entries are added, and an entry with an existing id replaces the packaged one. `credentials` and `assemble` cannot be overridden. An overlay may not introduce a provider the build has no adapter for; that is logged and the file ignored.

**Validation at startup.** A packaged-catalog error stops the server (it is a bug in the release); a bad overlay file is logged and skipped. Rules: unique provider ids; known assembler names; every `credential:` kind exists; every `needs:` field exists on a referenced kind; at most one default per list; ids non-empty, at most 128 characters, unique within a list; env names match `^[A-Z][A-Z0-9_]*$`; a `file_json` field is the only field of its kind. A catalog provider with no adapter factory in this build is not an error: its services are simply unavailable (section 4).

### 2. Detection and resolution (live environment)

- **Detection** reads the environment through an injected `func(string) string` (tests pass a map). A secret kind is *detected* when every non-optional field's variable is non-empty (for `file_json`: the file is readable and holds JSON). It is evaluated each time it is needed, never cached, so rotation by redeploy and removal of a variable are seen immediately.
- **Resolution:** `Resolve(provider, service) -> (secret []byte, err)` picks the first detected kind in the service's `credential` order whose `needs` fields are present, assembles it, and returns a fresh buffer that the caller wipes after the stage is built (as `wipeSecrets` does today). Values are trimmed; token-type values must be a single token without control characters; a Google API key must not start with `$` (the adapter refuses it too; the catalog error is clearer). `file_json` caps the file at 64 KiB.
- **Where it is used:** the call path (`resolveStageSecrets`, the failover candidate resolvers, graph node activation) and readiness, through one small interface `catalog.Resolver` (detect, resolve). That interface is also where a future vault would plug in.
- **Rotation:** running calls keep the stages they built; the next call, and the next graph-node activation, read the new value.

### 3. What is deleted

`server/provider` (the hard-coded definitions and the `xaiCapabilities` build-tag hook; the catalog replaces it, and adapter-factory presence, `registry.Supports`, already encodes the `xaispeech` tag); `server/credential` (service, keyring, encryption, validation against provider APIs); `storage` credential repository and `provider_credentials`; the credential half of `server/api/providers.go`; `credentials.keyring_env` and `GOVOICE_CREDENTIAL_KEYS` (and the dev-mode generated key); the Providers page's add, rotate, update, delete and validate controls; `credential_id` everywhere in agent config (a stored or posted `credential_id` is rejected by the strict decoder, loudly, so a stale client is noticed); the "validated keys" counters on the dashboard. The initial migrations (sqlite and postgres) are edited in place: `provider_credentials` is removed, and `mcp_servers.auth_credential_id` becomes `auth_env TEXT NOT NULL DEFAULT ''`. No data migration, no drop migration.

### 4. Agent config, readiness and the API

- **Stage config** is `{provider, model?, voice?, language?}` (plus fallbacks with the same shape). No credential field. A blank `model` or `voice` means the provider's adapter default, as today; the UI fills the catalog default.
- **Readiness** for a stage: the provider exists, this build has a factory for the service, the model and voice are non-empty or defaulted, and `Resolve(provider, service)` would succeed. The bounded reason codes keep their names (`missing_credential_<stage>` now means the required variable is not set) and readiness gains `missing_env: ["XAI_API_KEY"]` (names only, never values). `invalid_credential_*` goes away (there is no validation state).
- **`GET /api/v1/providers`** keeps `id`, `display_name`, `capabilities` and gains, per provider:

```json
{
  "id": "xai", "display_name": "xAI", "detected": true,
  "services": {
    "llm": {"available": true, "models": [{"id": "grok-2-1212", "label": "Grok 2", "default": true}], "custom": true},
    "stt": {"available": false, "reason": "not_in_build"},
    "tts": {"available": true, "voices": [{"id": "eve", "label": "Eve", "default": true}], "custom": true}
  },
  "env": ["XAI_API_KEY"]
}
```

A service is **available** when the catalog declares it, this build has its adapter factory (`registry.Supports`; xAI speech needs `-tags xaispeech`), and a kind that satisfies it is detected. Otherwise `available: false` with `reason` (`missing_env`, `not_in_build`). The response lists undetected providers too (with `detected: false` and `env`, the variables that would enable them) so the UI can say what is missing; forms offer only available ones. The response never contains a value from the environment.

### 5. MCP bearer tokens

An MCP server entry names `auth_env` (for example `MCP_TOKEN_CRM`) instead of a credential id. The name must match `^MCP_TOKEN_[A-Z0-9_]{1,48}$`: without the prefix, anyone with the management key could point a server at `TOKEN_SECRET` or `XAI_API_KEY` and have it sent to an endpoint they control. The value is read when the server connects or a tool is called, and sent only to that server's endpoint. API views show `auth_env` and `auth_env_set` (true or false), never the value.

### 6. UI

- **Providers page** becomes a read-only status page: per provider, detected or not, the services it unlocks, and the variables that enable what is missing (greyed out). No forms, no secrets on screen.
- **Agent form and graph designer.** One shared `StageFields` component replaces the duplicated stage inputs. Provider options are the providers whose service is available. Choosing a provider fills model and voice from the catalog defaults. Model and voice are `<select>`s fed by the catalog with a final **Other…** option that reveals a text box (**A2**: a model released yesterday must work before the catalog is updated; a service sets `custom: false` to hide it). STT language is a select when the catalog lists languages, else hidden. There are no credential selects.
- **MCP page:** the credential select becomes an `auth_env` field with the prefix shown and a "variable is set" indicator.
- If nothing is detected, the agent forms show an empty state that lists which variables enable which provider.

### 7. Harnesses and dev flow

- **`make dev`** starts the server with the provider variables loaded from `examples/.env`; `scripts/dev-seed.mjs` stops storing credentials and creates the "Dev Assistant" agent from the catalog defaults of whatever is detected.
- **e2e (Playwright and Go) and livecheck** pass the keys a profile needs to the server child as environment variables (still filtered to exactly those) instead of storing them through the API; the fake stack sets placeholder values for the providers it fakes. The paid suites' "secret never enters the browser" property gets stronger: it never even reaches the API.
- `tools/livecheck` keeps its own env-name table for now; a consistency test fails if a vendor's variable names there differ from the catalog. Moving livecheck onto the catalog is a follow-up.

### 8. Security

Secrets exist only in the process environment and, briefly, in per-call buffers that are wiped after use; nothing is persisted, returned by any API, logged, traced or put on a timeline. Errors and readiness name variables, never values. `file_json` reads only the path an operator put in a variable, capped at 64 KiB, JSON-validated. The MCP prefix rule (section 5) bounds what a management-key holder can make the server send out. The overlay directory is operator-controlled and parsed strictly (unknown keys rejected; 64 KiB per file; 500 entries per list). Environment values are server-wide, as stored credentials were (there is no per-tenant isolation in this server).

## Testing

- **catalog:** strict schema parse; every validation rule (table test); overlay merge (add, replace, refused credential override); the packaged files validate; detection with an injected environment for every seed provider, including partial NVIDIA, Google with only one of its two kinds, and an unreadable or non-JSON service-account file.
- **resolution:** assembled secrets equal what each adapter parses (a test per provider that gives the assembled secret to the adapter's own credential parser); preference order (Google LLM picks the API key, else the service account); secrets wiped; live behaviour (a variable removed or changed between two resolves).
- **agent and call path:** an agent with no credential is ready when the variable is set, takes a call with the fake providers, and is not ready (with `missing_env`) when it is removed; `credential_id` in a posted config is rejected; a graph node on another env provider activates; failover candidates resolve by provider.
- **API:** golden `GET /api/v1/providers` with no keys, one key, and a build lacking xAI speech; the credential routes no longer exist (404); MCP `auth_env` prefix validation and `auth_env_set`.
- **storage:** both dialects create the schema without `provider_credentials`.
- **UI:** vitest for `StageFields` (only available providers, defaults, Other…, language), the status page, the MCP field; Playwright on the fake stack with provider variables set (only those providers appear; an agent built from them takes a call) and with none (the empty state names the variables).
- Gate per AGENTS.md: `gofmt -w .`, `make verify`, independent adversarial review including the YAGNI audit.

## Out of scope

Live model discovery from provider APIs; per-tenant secrets; a vault or secret-manager source (the `catalog.Resolver` interface is where it would plug in); several keys for one provider at once; a "test connection" action; moving livecheck onto the catalog; the designer's test panel; catalog entries for providers without an adapter.

## Open items for review

- **A1** Overlay semantics (add or replace entries; credentials and assemblers not overridable).
- **A2** "Other…" free-text escape on model and voice.
- **A3** The seed model and voice lists (only values verifiable in this repo; you will want to add more, for example more Google, ElevenLabs and Cartesia voices).
- **A4** An agent that names a model or voice the catalog does not list still works (it shows as "Other…"); the catalog is guidance, not a gate.
- **A5** Strict rejection of `credential_id` (a stale client fails loudly) rather than ignoring it.

## Risks

- **Per-call env reads** make behaviour depend on process state; mitigated by evaluating detection at resolve time and by explicit not-ready reasons that name the variable.
- **Catalog drift** from what adapters accept (a listed model an adapter rejects): the seed is validated against adapter defaults in tests, and "Other…" covers anything unlisted.
- **Two Google secret kinds** (API key and service account) make provider setup less obvious: the catalog resolves them by service and preference, and the status page says which variable unlocks which service.
- **Rotation is a redeploy:** a leaked key is replaced by changing the variable and restarting; there is no in-place rotate button by design.
