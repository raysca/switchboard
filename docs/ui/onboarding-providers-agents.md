# Onboarding, Providers, and Agent Configuration UI

This document details the admin interface for first-run onboarding, the provider status page, and persistent agent configuration implemented in Task 21 (Phase 6).

## 1. Architecture Overview

The UI layer communicates exclusively with `/api/v1/*` endpoints exposed on the Server's private admin listener, using bearer authentication kept in browser memory. Plaintext secrets are never stored or logged in browser state.

```
┌─────────────────────────────────────────────────────────────┐
│                       Admin UI Shell                        │
├───────────────┬─────────────────────────────┬───────────────┤
│  Onboarding   │          Providers          │    Agents     │
│  /onboarding  │         /providers          │    /agents    │
│  Guided setup │   Status (env, catalog)     │ CRUD, Versions│
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
- **First-run Detection:** Queries `/api/v1/providers` and `/api/v1/agents`.
- **Step 1 — Set a provider key:** If no provider is detected, lists each provider's environment variables (from `GET /api/v1/providers`) and tells the operator to set one on the server and restart it. There is no secret form: the UI never handles a provider key.
- **Step 2 — Create First Agent:** Once any provider is detected, guides the operator through the primary voice agent: name, description, and the STT / LLM / TTS provider, model, voice and prompt (agent config schema v2, see `docs/api/v1.md`). Only available providers are offered, with the catalog's models and voices and an "Other…" free-text entry.
- **Completion State:** When a provider is detected and an agent exists, presents a system-ready confirmation with links to providers and agents.

## 3. Provider Status (`/providers`)

A read-only view of `GET /api/v1/providers`: per provider whether it is detected, the variables it reads, and per service (STT, LLM, TTS) whether it is available or which variables are missing (`missing_env`) or that the build lacks it (`not_in_build`). Variable names only, never values. Keys are set and rotated in the server's environment ([docs/providers.md](../providers.md)).

## 4. Agent Configuration (`/agents`)

The agent configuration view provides full lifecycle management with immutable versioning:
- **Catalog View:** Lists all agents with readiness badges (`ready`, `ready_with_tool_warning`, `needs_tool_review`, `tool_dependency_unavailable`, `not_ready`) and reason tags (e.g., `missing_stt`, `missing_llm`, `missing_tts`, `missing_credential_llm`, with the missing variable names).
- **Create & Edit Forms:** Captures agent name (1–128 chars), description (≤1024 chars), enabled status, and voice stack settings (STT, LLM, TTS providers, models, voice and prompt). A stage names a provider only; its key comes from the server's environment.
- **Immutable Version History:** In accordance with V2 specifications, updating an agent's configuration automatically publishes the next contiguous version number (`v1`, `v2`, etc.) along with a canonical SHA-256 configuration hash. Historical sessions stay pinned to specific version IDs and configuration hashes, preventing runtime drift.
- **Duplication & Archiving:** One-click duplication clones an agent and its configuration; archiving marks the agent as archived and updates its readiness status.

## 5. Security & Accessibility Invariants

- **No Secrets in the UI:** The UI never accepts, holds or displays a provider key or MCP token; it shows variable names only.
- **Request IDs:** Every error banner extracts and displays the `X-Request-ID` header for deterministic correlation with server diagnostics.
- **Accessibility:** Form fields use explicit labels with semantic `for`/`id` bindings. Error alerts use `role="alert"`, status announcements use `role="status"`, and tab ordering respects skip links.
