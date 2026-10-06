# Onboarding, Providers, and Agent Configuration UI

This document details the admin interface for first-run onboarding, provider credentials management, and persistent agent configuration implemented in Task 21 (Phase 6).

## 1. Architecture Overview

The UI layer communicates exclusively with `/api/v1/*` endpoints exposed on the Server's private admin listener, using bearer authentication kept in browser memory. Plaintext secrets are never stored or logged in browser state.

```
┌─────────────────────────────────────────────────────────────┐
│                       Admin UI Shell                        │
├───────────────┬─────────────────────────────┬───────────────┤
│  Onboarding   │          Providers          │    Agents     │
│  /onboarding  │         /providers          │    /agents    │
│  Guided setup │  Credentials & Discovery    │ CRUD, Versions│
└───────┬───────┴──────────────┬──────────────┴───────┬───────┘
        │                      │                      │
        └──────────────────────┼──────────────────────┘
                               ▼
                    Typed API Client (`client.ts`)
                               │ (Bearer auth in memory)
                               ▼
               Server Admin Listener (`/api/v1/*`)
```

## 2. Onboarding (`/onboarding`)

The onboarding route provides a guided first-run experience:
- **First-run Detection:** Queries `/api/v1/providers/credentials` and `/api/v1/agents`.
- **Step 1 — Connect Provider:** If no credentials are configured, prompts the operator to select a provider that can run every stage (e.g., Google) and input the secret token (`type="password"`). Secrets are never echoed back; on submission, the field is cleared immediately.
- **Step 2 — Create First Agent:** Once a provider is registered, guides the operator through configuring the primary voice agent, including agent name, description, and the STT / LLM / TTS provider, model, voice and prompt (agent config schema v2, see `docs/api/v1.md`).
- **Completion State:** When at least one credential and one agent exist, presents a system-ready confirmation with links to manage providers and agents.

## 3. Providers Management (`/providers`)

The providers view manages external service integrations powering speech recognition, reasoning, and speech synthesis:
- **Credential List:** Displays registered credentials with masked keys (`••••••••XXXX`), validation state badges (`valid`, `invalid`, `unvalidated`), and timestamps.
- **Validation:** Operators can trigger live provider validation (`POST /api/v1/providers/credentials/{id}/validate`). Validation errors display actionable error banners with the server request ID.
- **Rotation & Secret Replacement:** In-place rotation (`POST .../rotate`) re-encrypts under active keyring keys. Operators can also submit replacement secrets.
- **Capability Discovery:** Direct inspection tool allowing operators to query supported LLM models, STT models, and TTS voices for registered providers (`GET /api/v1/providers/{provider}/discovery?capability={llm|stt|tts}`).

## 4. Agent Configuration (`/agents`)

The agent configuration view provides full lifecycle management with immutable versioning:
- **Catalog View:** Lists all agents with readiness badges (`ready`, `configured`, `ready_with_tool_warning`, `needs_tool_review`, `tool_dependency_unavailable`, `not_ready`) and reason tags (e.g., `missing_stt`, `missing_llm`, `missing_tts`, `missing_credential_llm`).
- **Create & Edit Forms:** Captures agent name (1–128 chars), description (≤1024 chars), enabled status, and voice stack settings (STT, LLM, TTS providers, models, voice, prompt, and a required credential per stage: agents are attached to their providers' credentials when created). The first agent created by onboarding is attached to the credential added in step 1.
- **Immutable Version History:** In accordance with V2 specifications, updating an agent's configuration automatically publishes the next contiguous version number (`v1`, `v2`, etc.) along with a canonical SHA-256 configuration hash. Historical sessions stay pinned to specific version IDs and configuration hashes, preventing runtime drift.
- **Duplication & Archiving:** One-click duplication clones an agent and its configuration; archiving marks the agent as archived and updates its readiness status.

## 5. Security & Accessibility Invariants

- **Secret Masking:** Plaintext API secrets are never preserved in component state longer than submission time. Server responses only return masked suffixes.
- **Request IDs:** Every error banner extracts and displays the `X-Request-ID` header for deterministic correlation with server diagnostics.
- **Accessibility:** Form fields use explicit labels with semantic `for`/`id` bindings. Error alerts use `role="alert"`, status announcements use `role="status"`, and tab ordering respects skip links.
