# Credentials Epic: Provider Catalog and Environment Credentials Design

**Date:** 2026-10-11
**Status:** direction approved in conversation (live env credentials); awaiting written-spec review.

## Intent

An operator sets the standard variables (`XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_API_KEY`, ...) and the server works out, from data packaged with it, which providers are available and which services (STT, LLM, TTS) each one unlocks. The admin UI offers only what was detected, with curated models and voices to pick from instead of free text. Adding a provider, a model or a voice is a data change, not a code change (a brand-new provider still needs its adapter). Success:
- with only `XAI_API_KEY` set, the UI offers xAI (and no other provider), with its curated models and voices, and an agent built from it takes a call without anyone visiting the Providers page;
- a model or voice can be added by editing a YAML file (packaged, or an operator overlay), with startup validation that catches mistakes;
- credentials added through the UI keep working next to env credentials;
- every credential is labelled, so two Google credentials are no longer indistinguishable.

Decision made with the user on 2026-10-11: **env keys are live, read-only credentials.** They are never written to the database. The server exposes them with stable ids (`env:<provider>:<kind>`) and reads the variables at call time. Agents reference those ids. If a variable is removed or changed, the next call sees it (a removed one makes the agent not ready, with a clear reason). Rotating a key means restarting the server.

Assumptions to confirm at review (marked **A1** to **A5** below).

## Current behaviour (main at 6207869)

- The server never reads provider keys from the environment. It uses credentials stored through the admin API (`server/credential`): encrypted rows with `provider`, a masked suffix, a validation state and (always empty) capabilities. There is no label.
- The only env-to-credential mapping is in `tools/livecheck/creds.go` (`credSpecs`): one key per vendor, OpenAI key plus optional `OPENAI_BASE_URL` as JSON, Google's two roles (`GOOGLE_API_KEY` for the LLM, `GOOGLE_APPLICATION_CREDENTIALS` file content for speech), NVIDIA key plus per-stage function ids as JSON. `scripts/dev-seed.mjs` does the Google case for `make dev`.
- `provider.Definitions()` is a hard-coded list (id, display name, capabilities). xAI's STT/TTS capability depends on the `xaispeech` build tag through a package variable. There is no model or voice list anywhere on the server; the UI uses free-text fields. The package comment mentions a "model/voice discovery boundary" that does not exist.
- An agent stage names `provider`, `model`/`voice` and an explicit `credential_id`. `agent.ResolveCredential` checks it against stored rows; `Server.resolveStageSecrets` decrypts it at call time (and again when a graph node is activated).

## Design

### 1. The catalog: one YAML file per provider, packaged with the server

`server/catalog/data/<provider>.yaml`, embedded with `go:embed`. One file per provider, so adding a provider is adding a file. Schema (all keys required unless marked optional):

```yaml
id: xai
name: xAI
credentials:                       # the kinds of credential this provider has
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
    languages: [en]                # optional: language choices
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
      - {id: ara, label: Ara}
```

- **Services** are `stt`, `llm`, `tts`. A service has `credential` (acceptable kinds), `models` (STT, LLM, and TTS models where the provider has them), `voices` (TTS), `languages` (optional, STT), and `needs` (optional: field ids that must be present for this service, for NVIDIA's per-stage function ids).
- **Every list has at most one `default`.** The default is preselected in the UI; a service with a list but no default preselects nothing.
- **Assemblers** are a closed set in code: `token` (the single field's value), `json` (a JSON object of the fields that are present), `file_json` (the field's value is a path; read the file, validate and compact its JSON). A new secret shape needs a new assembler in code; a new provider that fits an existing shape is data only.
- **Seed content** (verified against the adapters and `tools/livecheck/profiles.yaml`; the lists are deliberately short and are the main item to review, **A4**):

| Provider | Credential kinds (env) | STT | LLM | TTS |
|---|---|---|---|---|
| google | `api_key` (`GOOGLE_API_KEY`, token), `service_account` (`GOOGLE_APPLICATION_CREDENTIALS`, file_json) | `chirp_2` (default), `latest_long`; needs service_account | `gemini-2.5-flash`; api_key preferred, service_account accepted | `en-US-Chirp3-HD-Aoede`; needs service_account |
| xai | `key` (`XAI_API_KEY`) | `grok-voice-transcribe-2.0` | `grok-2-1212` | `eve`, `ara` |
| elevenlabs | `key` (`ELEVENLABS_API_KEY`) | `scribe_v2_realtime` | none | model `eleven_flash_v2_5`; voice `21m00Tcm4TlvDq8ikWAM` |
| deepgram | `key` (`DEEPGRAM_API_KEY`) | `nova-3` | none | none |
| cartesia | `key` (`CARTESIA_API_KEY`) | `ink-whisper` | none | model `sonic-3`; voice `694f9389-aac1-45b6-b726-9d9369183238` |
| openai | `key` (`OPENAI_API_KEY`, optional `OPENAI_BASE_URL`, json) | none | `gpt-4o-mini` | none |
| nvidia | `key` (`NVIDIA_API_KEY`, token), `riva` (`NVIDIA_API_KEY`, `NVIDIA_STT_FUNCTION_ID`, `NVIDIA_TTS_FUNCTION_ID`, json) | needs `function_id` | `openai/gpt-oss-20b` | `Magpie-Multilingual.EN-US.Aria`; needs `tts_function_id` |

`provider.Definitions()` is rebuilt from the catalog (display name, capabilities from the declared services), so there is one source of truth. The MCP token provider stays in code (it is not a voice provider).

**Operator overlay (A1).** An optional directory `providers.catalog_dir` in the server config. Files there are merged over the packaged ones by provider id: a service's `models`/`voices`/`languages` entries are added, and an entry with an existing id replaces the packaged one; `credentials` and `assemble` cannot be overridden (that is code-adjacent). An overlay may not introduce a provider the build has no adapter for; it is reported (see validation) and ignored.

**Validation at startup** (the server refuses to start on a packaged-catalog error, and logs and skips a bad overlay file): unique provider ids; known assembler names; every `credential:` kind exists; every `needs:` field exists on a referenced kind; at most one default per list; ids non-empty, at most 128 characters, unique within a list; env variable names match `^[A-Z][A-Z0-9_]*$`; a field marked `file_json` is the only field of its kind. A provider in the catalog with no registered adapter factory is not an error (a build without the `xaispeech` tag has no xAI STT/TTS): its services are simply unavailable (section 3).

### 2. Detection and live env credentials

- **Detection** reads the environment through an injected `func(string) string` (tests pass a map). For each provider and credential kind, the kind is *detected* when every non-optional field's variable is set and non-empty (for `file_json`: the file is readable and holds JSON; checked at startup and again at use). Detection runs at startup for the API listing, and again whenever a secret is resolved, so the answer never goes stale.
- **Credential ids** are `env:<provider>:<kind>`, for example `env:google:service_account`. They cannot be created by a client; they exist exactly when the kind is detected.
- **A detected env credential** is listed next to stored ones with `source: "env"`, `label` from the catalog (`xAI API key`), `env` (the variable names, never values), a masked suffix of the primary value, `validation_state: "unvalidated"`, and read-only: update, rotate, validate-with-new-secret and delete return 409 `read_only_credential`.
- **Resolving** an env credential id into a secret assembles the fields with the kind's assembler, fresh on every call (and on every graph node activation). The result is wiped after the stage is built, like a decrypted stored secret. Variable values are trimmed; a token-type value must be a single token without control characters; a Google API key must not start with `$` (the adapter already refuses it, the catalog check fails earlier with a clear reason).
- **Fit check.** A stage that names an env credential must name a kind the catalog lists for that provider's service: `env:google:api_key` as an STT credential is refused at publish time (`invalid_credential_stt`), because the catalog says streaming STT needs the service account. Stored credentials are unchanged (their kind is unknown), so they keep working for any service as today.
- **Switch (A2).** `providers.env_credentials: true|false` (default `true`). The e2e and livecheck harnesses already strip provider variables from the server's environment, so they keep stored-only behaviour; the switch also lets an operator turn the feature off.
- **Not a secret store.** The catalog holds variable *names*; the environment holds values; nothing about an env credential is written to the database, a log line, a trace attribute or an API response beyond the masked suffix.

### 3. What the UI may offer (API)

`GET /api/v1/providers` keeps `id`, `display_name`, `capabilities` and gains, per provider:

```json
{
  "id": "xai", "display_name": "xAI",
  "detected": true,
  "services": {
    "llm": {"available": true, "credentials": ["env:xai:key"], "models": [{"id": "grok-2-1212", "label": "Grok 2", "default": true}]},
    "stt": {"available": false, "reason": "not_in_build"},
    "tts": {"available": true, "credentials": ["env:xai:key"], "voices": [{"id": "eve", "label": "Eve", "default": true}]}
  }
}
```

A service is **available** when (a) the catalog declares it, (b) this build has an adapter factory for it (`registry.Supports`), and (c) at least one usable credential exists: a detected env credential of an accepted kind, or any stored credential of that provider. Otherwise `available: false` with a `reason` (`no_credential`, `not_in_build`, `missing_field:<id>`). The list always includes undetected providers (with `detected: false`) so the Providers page can show what *could* be added; the agent and designer forms offer only available ones.

### 4. Labels

Stored credentials gain a `label` (1 to 64 characters, `NOT NULL DEFAULT ''` in migration 006 for sqlite and postgres). Create and update accept it; the DTO returns it; an empty label displays as `<Provider> key ••••<suffix>`. Env credentials carry their catalog label. Every credential dropdown shows `label ••••suffix`. `scripts/dev-seed.mjs` names the two Google credentials it creates (`Google service account (speech)` and `Google API key (LLM)`) until `make dev` seeds env credentials instead (section 6).

### 5. UI

- **Providers page.** Credentials table: label, provider, source (Environment or Stored), masked key, validation, actions (none for environment credentials, with the variable name shown). Add form: provider (any supported provider), label, secret. A "Detected from environment" section lists env credentials and, greyed out, providers with no key and the variable that would enable each.
- **Agent form and graph designer.** One shared `StageFields` component (the two forms stop duplicating it). Provider options are the providers whose service is available. Choosing a provider fills the credential (preselected when exactly one is usable; env credentials sorted first), the model and the voice from catalog defaults. Model and voice are `<select>`s fed by the catalog, each with a final **Other…** option that reveals a text box (**A3**: a model released yesterday must be usable before the catalog is updated; the catalog marks a service `custom: false` to hide it). STT language is a select when the catalog lists languages, else hidden.
- Existing agents are untouched: a stored `model` or `voice` the catalog does not list shows as "Other" with its value.

### 6. Consumers and compatibility

- `agent.ResolveCredential`, `AttachmentProblems`, `FallbackProblems`, `Server.resolveStageSecrets`, the fallback resolvers and the graph node activation all go through one `credential.Source` interface (list, resolve) implemented by the stored service and the env resolver together, so there is no second code path.
- Agent configs and versions are unchanged in shape; `credential_id` may be an `env:` id. Duplicating or exporting an agent keeps the id. A version pins the credential *id*, not its secret value; rotating an env key is picked up by the next call.
- `tools/livecheck` keeps its own `credSpecs` for now; a consistency test fails if a vendor's env names there differ from the catalog, so the two cannot drift. Migrating livecheck onto the catalog is a follow-up.
- `make dev`: the server is started with the provider variables from `examples/.env` and the seed script references env credentials, so the dev flow needs no stored credential. Stored credentials still work.

### 7. Security

Environment values are server-wide (like stored credentials today; there is no per-tenant isolation in this server). Env secrets are never logged, traced, recorded in timelines, or returned; errors name the variable, not the value. `file_json` reads only the path an operator put in an env variable, caps the file at 64 KiB, and requires valid JSON. The overlay directory is operator-controlled and parsed strictly (unknown keys rejected, size and entry-count caps: 64 KiB per file, 500 entries per list). Read-only enforcement for `env:` ids is in the credential service, not only the UI.

## Testing

- **catalog:** schema parse and strict unknown-key rejection; each validation rule (table test); overlay merge (add, replace, ignored override of credentials); the packaged files validate; detection with an injected environment for every seed provider, including partial NVIDIA and an unreadable or non-JSON service-account file.
- **credential:** env ids list, resolve, mask and refuse updates; stored and env mixed; secrets wiped; assembled shapes equal what each adapter parses (a test per provider that passes the assembled secret to the adapter's own credential parser).
- **agent and call path:** an agent on `env:` ids is ready, takes a call (fake providers), and becomes not ready when the variable is removed; the fit check rejects `env:google:api_key` for STT; a graph node on a different env provider activates.
- **API:** golden response for `GET /api/v1/providers` with and without env keys and with a build that lacks xAI speech; 409 on writes to env credentials; labels round-trip on sqlite and postgres.
- **UI:** vitest for `StageFields` (only available providers, preselection, Other…), the Providers page sections, label display; Playwright against the fake stack with provider variables set (only those providers appear; an agent built from them takes a call), and without any (the empty state explains which variables enable which provider).
- Verification gate per AGENTS.md: `gofmt -w .`, `make verify`, independent adversarial review including the YAGNI audit.

## Out of scope

Live model discovery from provider APIs; per-tenant credentials; credentials fetched from a vault or secret manager (the `Source` interface is where that would plug in); moving livecheck onto the catalog; the designer's test panel; catalog entries for providers without an adapter.

## Open items for review

- **A1** Overlay semantics (add or replace entries; credentials and assemblers not overridable).
- **A2** `providers.env_credentials` defaults to on, in every profile including production.
- **A3** "Other…" free-text escape on model and voice.
- **A4** The seed model and voice lists (only values verifiable in this repo; you will want to add more, for example more Google, ElevenLabs and Cartesia voices).
- **A5** Credential labels are part of this epic.

## Risks

- **Per-call env reads** make behaviour depend on process state; mitigated by detection being re-checked at resolve time and by explicit not-ready reasons.
- **Catalog drift** from what adapters really accept (a listed model an adapter rejects); mitigated by validating the seed against adapter defaults in tests and by "Other…" for anything unlisted.
- **Two Google kinds** make the UI's credential choice less obvious; the fit check and preselection (prefer the kind the catalog lists first) carry that.
