# Providers and the provider catalog

Provider secrets live in the server's environment and nowhere else. The server never stores them, never accepts them over the API and never returns them. A packaged **catalog** (`server/catalog/data/*.yaml`, compiled into the binary) says which variables each provider reads, which services (STT, LLM, TTS) they unlock, and which models, voices and languages the UI offers.

`GET /api/v1/providers` shows, for every catalog provider, whether its variables are set (`detected`), the variables it reads (`env`), and per service whether it is `available` or why not (`missing_env` with the variable names, or `not_in_build`). Names only, never values.

## Environment variables

| Provider | Variable | Unlocks |
|---|---|---|
| `google` | `GOOGLE_API_KEY` | LLM (Gemini API key; preferred for the LLM when both are set) |
| `google` | `GOOGLE_APPLICATION_CREDENTIALS` | STT, TTS, and the LLM through Vertex AI. A **path** to a service-account JSON key file (read at most 64 KiB, must be valid JSON) |
| `xai` | `XAI_API_KEY` | LLM; STT and TTS in builds tagged `xaispeech` (the default `make build` and the image) |
| `openai` | `OPENAI_API_KEY`, optional `OPENAI_BASE_URL` | LLM (OpenAI or any OpenAI-compatible server such as vLLM; private base URLs are refused unless `providers.openai_llm.allow_private_base_url`) |
| `nvidia` | `NVIDIA_API_KEY` | LLM (hosted NIM) |
| `nvidia` | `NVIDIA_API_KEY` + `NVIDIA_STT_FUNCTION_ID` | STT (hosted Riva on NVCF) |
| `nvidia` | `NVIDIA_API_KEY` + `NVIDIA_TTS_FUNCTION_ID` | TTS (Riva on NVCF) |
| `deepgram` | `DEEPGRAM_API_KEY` | STT |
| `cartesia` | `CARTESIA_API_KEY` | STT, TTS |
| `elevenlabs` | `ELEVENLABS_API_KEY` | STT, TTS |
| MCP servers | `MCP_TOKEN_<NAME>` | The bearer token of an MCP server whose `auth_env` names it (see [api/mcp.md](api/mcp.md)) |

A value must be a single token: surrounding whitespace is trimmed, and a value with inner whitespace or control characters, or one that starts with `$` (an unexpanded shell reference), counts as missing. This applies to optional variables too: a set but unusable `OPENAI_BASE_URL` makes OpenAI undetected (naming the variable) rather than sending the key to the default endpoint. `GOOGLE_APPLICATION_CREDENTIALS` must name a regular file holding a JSON object with `"type": "service_account"`; a user ADC file or any other JSON counts as missing.

Self-hosted NVIDIA NIM and Riva endpoints are not supported in this release: `nvidia` always uses the hosted NIM (`integrate.api.nvidia.com`) and NVCF Riva endpoints, and `providers.nvidia.allow_private` is removed (a config that still sets it fails to load). They come back as catalog environment fields when needed. Variables are read when they are needed (agent readiness, `GET /providers`, and the start of each call), never cached; in practice you change them by editing the environment file and restarting.

## Catalog file format

One YAML file per provider. Parsing is strict: unknown keys are errors, a file is at most 64 KiB and a list at most 500 entries.

```yaml
id: xai                     # lower-case, matches the adapter's provider id
name: xAI                   # display name
credentials:                # the kinds of secret the provider accepts
  - id: key
    label: xAI API key
    fields:
      api_key: {env: XAI_API_KEY}      # field -> environment variable (optional: true allowed)
    assemble: token         # token: the one value | json: an object of the fields | file_json: the JSON file the one field points at
services:
  llm:
    credential: [key]       # kinds that serve this service, in preference order
    models:
      - {id: grok-2-1212, label: Grok 2, default: true}
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
    custom: true            # default true: the UI also offers "Other..." free text
```

A service may list `needs: [field]`: the service is available only when that optional field is set (NVIDIA STT needs `function_id`). `models`, `voices` and `languages` are guidance for the UI, not a gate: an agent that names a model or voice the catalog does not list still works.

## Adding models, voices and languages: the overlay directory

Set `providers.catalog_dir` in the server YAML to a directory of overlay files. At start-up the server merges every `*.yaml` file in it over the packaged catalog:

```yaml
# /etc/govoice/catalog/google.yaml
id: google
services:
  tts:
    voices:
      - {id: en-US-Chirp3-HD-Charon, label: Chirp 3 HD Charon (en-US)}
      - {id: en-GB-Chirp3-HD-Aoede, label: Chirp 3 HD Aoede (en-GB), default: true}
```

An entry with an existing `id` replaces it, a new one is appended, and an overlay `default: true` takes over the default. An overlay can also set `custom`. It can **not** add a provider (a provider needs an adapter in the build), add a service, or change `credentials`, a service's `credential` or `needs`: the environment variables the server reads are fixed by the release. A file that breaks a rule is skipped and logged (`catalog overlay file skipped`), and the server starts with the rest; a missing directory is a start-up error.

## Adding a provider

A new provider needs code: an adapter under `providers/`, a factory registered in `server/registry.go`, and a packaged catalog file in `server/catalog/data/`. `server/catalog_adapters_test.go` checks that what the catalog assembles is what the adapter parses.
