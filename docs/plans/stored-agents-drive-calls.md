# Stored agents drive calls; YAML pipelines removed

Branch: `feature/stored-agents` (from `main` @ 79b15c9).

## Decisions (from the user)
- Voice tokens carry an `agent_id` claim; the `pipeline` claim is gone.
- YAML `pipelines:` is removed entirely. There is no import path: agents come only from storage.
- Calls use the stored, encrypted provider credentials. Storage and the credential keyring become mandatory.

## Target call path
```
POST /api/v1/voice-tokens {principal, agent_id}  -> agent must exist, be enabled and ready
WS connect with token(agent_id)
  -> runtime.StartSession(agent): pin current version + capture MCP tools + persist session row
  -> decrypt the credentials the agent's providers need (provider-scoped)
  -> registry builds VAD -> STT -> transcript -> user ctx -> LLM(+MCP tools) -> asst ctx -> TTS -> transport
  -> on teardown: end session row, release runtime session
```

## Agent config schema (v2, validated on create/update)
```json
{
  "stt":     {"provider": "google", "model": "chirp_2", "language": "en-US", "credential_id": "..."},
  "llm":     {"provider": "google", "model": "gemini-2.0-flash", "system_prompt": "...", "credential_id": "..."},
  "tts":     {"provider": "google", "voice": "en-US-Journey-F", "credential_id": "..."},
  "context": {"max_messages": 64, "max_tokens": 8000}
}
```
`credential_id` is optional. If omitted, the provider's single valid credential is used, and readiness fails if there is no credential or more than one.

## YAML keeps only infrastructure
Listeners, TLS, limits, VAD, token issuer, and provider *infrastructure* (Google project/location/quotas, rotation, xAI endpoint).
Removed from YAML: `pipelines`, per-provider model/voice/system prompt, `credentials_file`, `api_key_env`.

## Tasks
| ID | Task |
|---|---|
| S1 | Agent config schema v2: typed validation and readiness (providers known, capabilities, credentials resolvable) |
| S2 | Google provider accepts in-memory credentials (API key or service-account JSON) for STT/LLM/TTS |
| S3 | xAI provider takes its API key explicitly (no env lookup on the call path) |
| S4 | Credential service: provider-scoped secret access for the runtime (separate from MCP-only Resolve) |
| S5 | Registry builds the pipeline from an agent snapshot + decrypted secrets; YAML model/voice/credential fields removed |
| S6 | Call path: `agent_id` token claim; onSessionReady pins the agent version, builds the pipeline, persists/ends the session row |
| S7 | Agent's MCP tools wired into the call's LLM via runtime.Session |
| S8 | Storage mandatory; `pipelines:` rejected with a clear error; readiness no longer depends on pipelines |
| S9 | `/voice-tokens` takes `agent_id` and checks agent readiness |
| S10 | Remove ImportV1 / V1Pipeline and its API route |
| S11 | UI: agent form for schema v2; voice test and token use `agent_id` |
| S12 | Configs, deploy and docs updated |
| S13 | End-to-end test: credential + agent via API -> token -> WS call runs the agent's providers |
| S14 | Verification: make test, UI tests, independent review |
