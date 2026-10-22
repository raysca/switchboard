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
| `offline` | none | LLM; STT and TTS in builds tagged `xaispeech`. Local models behind `providers.offline.*` endpoints; see [Offline mode](#offline-mode-privacy) |
| MCP servers | `MCP_TOKEN_<NAME>` | The bearer token of an MCP server whose `auth_env` names it (see [api/mcp.md](api/mcp.md)) |

**LLM sampling.** A flow step's `llm.temperature`, `llm.top_p` and `llm.max_output_tokens` ([api/v1.md](api/v1.md)) reach each provider as: `google`, `generationConfig` `temperature`, `topP`, `maxOutputTokens` (reasoning models such as Gemini 2.5, GPT-OSS on NVIDIA and Grok reasoning models count their thinking toward the cap, so a low cap can leave an empty reply; leave it empty on these models, or use 1024 or more); `xai` and `offline` on the Responses endpoint, `temperature`, `top_p`, `max_output_tokens`, and on a chat-completions endpoint `max_tokens`; `openai` (and any OpenAI-compatible server) and `nvidia`, `temperature`, `top_p`, `max_tokens`. Unset values are left out of the request. `echo` shows them in its labelled replies. Wire names are from the providers' documentation and are not verified against the live APIs.

A value must be a single token: surrounding whitespace is trimmed, and a value with inner whitespace or control characters, or one that starts with `$` (an unexpanded shell reference), counts as missing. This applies to optional variables too: a set but unusable `OPENAI_BASE_URL` makes OpenAI undetected (naming the variable) rather than sending the key to the default endpoint. `GOOGLE_APPLICATION_CREDENTIALS` must name a regular file holding a JSON object with `"type": "service_account"`; a user ADC file or any other JSON counts as missing.

Self-hosted NVIDIA NIM and Riva endpoints are not supported in this release: `nvidia` always uses the hosted NIM (`integrate.api.nvidia.com`) and NVCF Riva endpoints, and `providers.nvidia.allow_private` is removed (a config that still sets it fails to load). They come back as catalog environment fields when needed. Variables are read when they are needed (flow readiness, `GET /providers`, and the start of each call), never cached; in practice you change them by editing the environment file and restarting.

## How the variables relate to the data files

The table above is derived from the data files; the files are the source of truth. The chain, for one provider:

```
environment variable ──▶ credentials[].fields ──▶ credentials[] kind ──▶ services.<stt|llm|tts>.credential ──▶ UI + readiness
 (GOOGLE_API_KEY)         (api_key: {env: ...})    (id: api_key)          (llm: [api_key, service_account])     (dropdowns)
```

1. **`fields`** bind a variable name to a field of a credential kind. A kind is *usable* when every non-`optional` field's variable is set and valid (see the rules above).
2. **`assemble`** turns the fields into the one secret the adapter receives: `token` (a single value), `json` (an object of the fields) or `file_json` (the contents of the file the variable points at). The environment is read here, at use time.
3. **A service's `credential` list** names the kinds that can serve it, in preference order. The service is `available` when any listed kind is usable (and `needs` fields are set, and the build has the adapter); the first usable kind wins. Google shows this: with only `GOOGLE_API_KEY` set, the LLM works and STT/TTS report `missing_env: [GOOGLE_APPLICATION_CREDENTIALS]`; with both set, the LLM uses the API key.
4. **`models`, `voices`, `languages`** are what the dropdowns offer for that service once it is available. They never gate anything.
5. **What is returned:** `GET /api/v1/providers` lists, per provider, `env` (every variable in its `fields`), `detected` (some kind is usable) and per service `available`, `missing_env` and the entries. Flows never name a variable or a kind; they name a provider.

Where to set the variables: `examples/.env` for `make dev` and the examples (see `examples/.env.example`, which lists them), the `EnvironmentFile` of `deploy/govoice.service` or the `env_file` of `deploy/docker-compose.yaml` in production. Nothing else reads a file of secrets.

Keeping things in step: adding a variable means editing the provider's data file (the `fields` entry); `tools/livecheck/creds.go` keeps its own list for choosing live-test profiles and a drift test (`TestCredSpecsMatchCatalog`) fails if the two disagree, so update both, then this table.

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
      - {id: grok-4.20-0309-non-reasoning, label: Grok 4.20, default: true}
  tts:
    credential: [key]
    voices:
      - {id: eve, label: Eve, default: true}
    custom: true            # default true: the UI also offers "Other..." free text
```

A service may list `needs: [field]`: the service is available only when that optional field is set (NVIDIA STT needs `function_id`). `models`, `voices` and `languages` are guidance for the UI, not a gate: a flow that names a model or voice the catalog does not list still works.

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

## Echo (simulated)

`echo` is a provider that costs nothing and needs no key: it is always detected and always offered, and the UI labels it "Echo (simulated)". Its STT, LLM and TTS are the **real Google processors** (retries, circuit breaker, usage, tracing, tool loop, unit splitting, streaming versus unary synthesis) running against simulated backends in `providers/echo`, so a call through echo exercises the real pipeline code. Use it to try the console, to test the UI, and to load-test or end-to-end test without spending money; never put real callers on it.

- **What it says.** The LLM repeats the caller ("You said: ..."), word by word, and calls the tool a phrase asks for ("transfer to sales", "end the call", "call tool <name> {json}"). The TTS makes audio as long as the text takes to say (silence, or a quiet tone with `audible`). The STT cannot recognise speech: in `audio` mode it turns voiced audio into placeholder words ("hello"); typed turns work as always.
- **What it imitates.** Its catalog (`server/catalog/data/echo.yaml`) mirrors Google's models, voices and languages (a test keeps them identical), so a flow built for Google runs on echo by changing the provider. With `mimic: google` the timing follows `docs/benchmarks/first-audio-latency.md`: `chirp_2` has no interim results and finalizes about 1.2 s after the caller stops, `latest_long` has interims and finalizes in about 0.5 s; the model's first word comes in about 0.7 s; Chirp 3 HD voices stream (first audio about 0.4 s) while every other voice (and any voice when `providers.google_tts.disable_streaming` is set) is unary (about 1 s plus 12 ms per character); speech is 15 characters per second at `providers.google_tts.sample_rate`.
- **Settings** (`providers.echo` in the server config):

| key | default | meaning |
|---|---|---|
| `mimic` | `google` | `google` imitates Google's timing; `instant` has no delays (framework measurements, tests); both stream the reply word by word |
| `stt` | `text` | `audio` turns voiced audio into placeholder words |
| `audible` | `false` | speak a quiet 220 Hz tone instead of silence |
| `label_replies` | `false` | prefix each reply with the step's prompt, so a test can tell which step answered; it also shows the sampling the step set, e.g. `{temperature=0.3 top_p=0.9 max_output_tokens=200}` |
| `llm_extra_delay` | `0` | added between reply pieces, after the first (the perf-gate self-test uses it) |

- **Failure injection.** A step whose TTS voice is `brokenvoice` cannot be built, which the end-to-end suites use to fail a route. Stage-level failure injection for core tests lives in `core/coretest`, not here.

## Silence (text only)

`silence` is the provider for flows that have no server-side speech: a client that listens and speaks in the browser (the Web Speech API, a chat widget, a screen reader) sends typed turns and renders the reply text. It needs no key and is always detected, like `echo`; the UI labels it "Silence (text only)". Give it to **both** speech stages and pick a real provider for the model: `stt` model `text` and `tts` voice `none` are the only choices, and it has no model service. Design and rationale: [decisions/silence-provider.md](decisions/silence-provider.md).

- **What a call does.** The client sends RTVI `send-text` turns and gets `user-transcription`, `bot-llm-started`, `bot-llm-text`, `bot-llm-stopped`, `bot-tts-text` and `bot-started-speaking`/`bot-stopped-speaking`. The speaking events open and close at once: the TTS makes one 20 ms frame of silence per unit with no delay, and the STT never transcribes (audio sent anyway is ignored).
- **Quiet callers.** A caller who types is not silent. When the call's STT provider is `silence` and the flow sets no `idle` settings, the quiet-caller prompts and hang-up are off (`max_call_duration` still ends the call and is the only bound on a tab left open, so set a short one for kiosks, or set `idle` on the flow). A flow that sets `idle` gets exactly what it sets.
- **Failover.** `silence` never fails, so it takes no fallbacks and cannot be one (a flow that says so is refused): a real voice must not fail over onto silence.
- **Resilience.** Its stages are the real Google processors on in-process backends, with their own circuit breakers (`voice_{stt,tts}_circuit_state{provider="silence",endpoint="text"}`), apart from `echo`'s.
- **Console.** The Test call panel does not ask for a microphone when the call's STT provider is `silence`; type in the message box.
- **Privacy.** The browser's own recognition and synthesis are often cloud services run by the browser vendor: text-only flows keep audio off Switchboard, not off the vendor.

## Adding a provider

A new provider needs code: an adapter under `providers/`, a factory registered in `server/registry.go`, and a packaged catalog file in `server/catalog/data/`. `server/catalog_adapters_test.go` checks that what the catalog assembles is what the adapter parses.

## Offline mode (privacy)

The `offline` provider runs a whole call on local models: no key, no network, and audio and text never leave the host. It reuses the `xai` processors against a local stack in [`offline/`](../offline/README.md): Ollama for the LLM, and an adapter in front of Speaches (faster-whisper and Kokoro) for STT and TTS.

```yaml
providers:
  offline:
    llm_endpoint: http://127.0.0.1:11434/v1/responses
    stt_endpoint: ws://127.0.0.1:8099/v1/stt
    tts_endpoint: http://127.0.0.1:8099/v1/tts
```

These are the defaults. Start the stack with `make offline-up` (`OFFLINE_PROFILE=host-llm` on Apple Silicon), fetch the models once with `make offline-models` (the only step that needs network), then choose provider `offline` for a stage. The step-by-step walkthrough, with a flow to create and a call to make, is in [offline/README.md](../offline/README.md#run-it). `offline` is a regular provider. The development profile points the three endpoints at the local stack; the **production profile ships them empty, so `offline` is off** (its services report unavailable) until an operator sets `providers.offline.llm_endpoint`, `stt_endpoint` and `tts_endpoint`, because it speaks plaintext http/ws to whatever listens there. An endpoint left empty drops that stage, and a stage can never fall back to xAI's cloud endpoints. A flow that names a cloud provider as a failover target for an `offline` stage sends that call to the cloud provider when the local stack fails. `GET /api/v1/providers` lists it (always detected, no variables), the console's **Test connection** button and `POST /api/v1/providers/offline/test` check all three stages, and any stage can name it as a failover primary or fallback. It keeps its own circuit breakers, so a local outage does not affect `xai`: they appear as `voice_{stt,llm,tts}_circuit_state{provider="offline",endpoint="local"}` on `/metrics` and as `offline_<stage>:local` under `circuits` on `/ready`. Without `-tags xaispeech` only its LLM stage exists. Traces and usage still say `provider: xai`.
