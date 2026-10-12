# Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat Providers table with the Switchboard **Providers** screen (canvas board 15): a filterable list of catalog providers (lamp, services, where the credential comes from, how many flows use it) beside a detail panel for the selected provider (credential source, catalog, which flows use it).

**Architecture:** Pure helpers (`providerStats.ts`) turn `GET /providers` and `GET /agents` into display facts. `ProvidersView` renders the list and the detail panel; the selected provider and the service filter live in the URL. No server or API change. The page stays **read-only**: the server reads provider keys from its environment and has no endpoint to store them (spec S3), so the canvas's key field, "Save to keyring", "Test connection" and the "Last hour" figures are left out and recorded as server prerequisites.

**Tech Stack:** React 19, TypeScript 7, react-router 7, @tanstack/react-query 5, vitest 5 + Testing Library, Playwright (`e2e/tests-graph`, `e2e/tests`).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (Sections 3, 4, 5; this is "plan 4"). Visual rules: `docs/superpowers/specs/2026-10-12-ui-switchboard-design.md`. Canvas board 15 (`Providers.dc.html`). MCP tools (board 16) is plan 5.

## What the server really offers (verified in `server/api/providers.go` and `ui/src/api/client.ts`)

- `GET /api/v1/providers` returns, per provider: `id`, `display_name`, `capabilities`, `detected` (its credential variable is set in the server's environment), `env` (the variable names), and `services` for `stt`, `llm`, `tts` (only the ones the catalog lists): `available`, `reason` (`missing_env` | `not_in_build`), `missing_env[]`, `models[]`, `voices[]`, `languages[]` (each `{ id, label, default? }`), `custom`.
- There is **no** endpoint to write a key, test a connection, or read per-provider requests, errors, p50 latency or circuit-breaker state (those exist only in the Prometheus `/metrics` text, `server/tts_failover.go`, `server/metrics.go`).
- "Which flows use it" is derivable: each flow version's config names a `provider` per stage, in a flat config or in every step of a graph (`GET /agents`, capped at 100).

## Global Constraints

- UI copy says Flow / Step. Never "agent" or "node" in new copy. API names stay (`ProviderStatus`, `client.getProviders`, `["providers"]`, `["agents"]`).
- **The page never shows an input for a secret and never sends one** (the server would not store it). Existing guard: no `input`, `textarea` or `form` on this page. Filter pills and provider rows are buttons.
- Colours only through tokens (`--paper`, `--surface`, `--ink`, `--ink-2`, `--rule`, `--caller`, `--lamp`, `--ok`, `--bad`). No hex or `rgb(` in TSX or below `/* end tokens */` in `ui/src/styles.css`. Add every new TSX file to `CONVERTED` in `ui/src/test/noHardcodedColors.test.ts`. Radius is `var(--radius)`, `50%` for dots, or `999px` for filter pills only.
- Lamp amber is for the live call only; provider status uses `.lamp[data-tone="ok"]` or the neutral lamp.
- Sentence-case labels; no all-caps, no drop shadows, no monospace labels (variable names such as `GOOGLE_API_KEY` are data and may be monospace); 44px touch targets; the global focus ring.
- Never offer a control the server cannot honour. Left out on purpose (add these ids to spec Section 5 in Task 4): the key field, Save to keyring and the sealed-keyring wording (S3, already listed), Test connection (S16), p50 and the "Last hour" requests, errors and breaker (S15).
- `GET /agents` returns at most 100 flows: if the flow list fails or is capped, the counts say so instead of showing a wrong number.
- Keep working: the `Providers` nav link and `/providers` route; the paid e2e suite's check that no provider secret appears in the page.
- Gate for every task: `cd ui && npx vitest run && npx tsc --noEmit`.
- Commits are conventional (`feat(ui): …`) and end with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Nothing detected, or the provider list is empty.** Expect the existing "No providers detected" message naming every variable to set, a usable (empty) list, and no crash when there is no selected provider. Pinned in Task 2.
2. **A failing or capped flow list.** `GET /agents` can fail or return exactly 100. Expect the providers still list and open; counts show "—" and the detail says the flows could not be loaded (or "at least" when capped). Pinned in Task 2.
3. **Unknown `?provider=` or `?service=` in the URL.** Expect a sensible default (first detected provider, all services), never a blank panel. Pinned in Task 2.
4. **A provider with a service missing in one stage but not another.** Expect each service says which of "available", "needs VAR" or "not in this build" it is, and the missing variable is named once even if two services need it. Pinned in Tasks 1 and 2.
5. **Flows that name a provider in only one step.** Expect a graph flow counts for a provider used by any one step, once; archived flows never count. Pinned in Tasks 1 and 2.

---

### Task 1: Provider figures

**Files:**
- Create: `ui/src/routes/providers/providerStats.ts`
- Test: `ui/src/routes/providers/providerStats.test.ts`

**Interfaces:**
- Produces (all pure):
  - `SERVICES: readonly ["stt", "llm", "tts"]`, `type Service`, `SERVICE_NAMES: Record<Service, string>` (`Speech-to-text`, `Language models`, `Text-to-speech`).
  - `serviceList(p: ProviderStatus): string` — `"STT · LLM · TTS"`, only the services the catalog lists, in that order.
  - `hasService(p: ProviderStatus, s: Service): boolean`.
  - `credential(p: ProviderStatus): { text: string; ok: boolean }` — `Environment variable` when detected, else `Not configured`.
  - `missingEnv(p: ProviderStatus): string[]` — the variables still to set: those named by its services' `missing_env` without repeats, else (when not detected and none named) `p.env`, else `[]`.
  - `serviceState(p: ProviderStatus, s: Service): { state: "ok" | "missing" | "absent"; text: string } | null` — `null` when the catalog has no such service; `ok` → `available`; `missing_env` → `needs A, B`; otherwise → `not in this build`.
  - `catalogLines(p: ProviderStatus): { service: Service; text: string }[]` — per listed service, model (or, for TTS, voice) labels, then language labels, joined with ` · `; `No catalog entries` when both are empty.
  - `flowsUsing(flows: AgentDTO[], providerId: string): AgentDTO[]` — non-archived flows whose current version names that provider in any stage of the flat config or of any step.

- [ ] **Step 1: Write the failing tests**

`ui/src/routes/providers/providerStats.test.ts`:

```ts
import { describe, expect, it } from "vitest";
import type { AgentDTO, ProviderStatus } from "../../api/client";
import { catalogLines, credential, flowsUsing, hasService, missingEnv, serviceList, serviceState } from "./providerStats";

const google: ProviderStatus = {
  id: "google",
  display_name: "Google",
  capabilities: ["stt", "llm", "tts"],
  detected: true,
  env: ["GOOGLE_API_KEY"],
  services: {
    stt: { available: true, custom: true, models: [{ id: "chirp_2", label: "Chirp 2" }, { id: "long", label: "Latest long" }], languages: [{ id: "en-US", label: "English (US)" }] },
    llm: { available: true, custom: true, models: [{ id: "gemini", label: "Gemini 2.5 Flash", default: true }] },
    tts: { available: false, reason: "not_in_build", custom: true, voices: [{ id: "aoede", label: "Chirp 3 HD Aoede" }] },
  },
};
const xai: ProviderStatus = {
  id: "xai",
  display_name: "xAI",
  capabilities: [],
  detected: false,
  env: ["XAI_API_KEY"],
  services: {
    llm: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true },
    stt: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true },
  },
};

const flow = (id: string, config: Record<string, unknown>, archived = false): AgentDTO => ({
  id,
  name: id,
  description: "",
  enabled: true,
  archived,
  current_version: { id: `${id}-v`, number: 1, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});

describe("serviceList and hasService", () => {
  it("lists only the services the catalog has, in flow order", () => {
    expect(serviceList(google)).toBe("STT · LLM · TTS");
    expect(serviceList(xai)).toBe("STT · LLM");
    expect(hasService(xai, "tts")).toBe(false);
    expect(hasService(xai, "llm")).toBe(true);
  });
});

describe("credential", () => {
  it("says where the credential comes from: the server's environment, or nowhere", () => {
    expect(credential(google)).toEqual({ text: "Environment variable", ok: true });
    expect(credential(xai)).toEqual({ text: "Not configured", ok: false });
  });
});

describe("missingEnv", () => {
  it("names each missing variable once, even when two services need it", () => {
    expect(missingEnv(xai)).toEqual(["XAI_API_KEY"]);
  });
  it("falls back to the provider's own variables when nothing is named, and is empty once detected", () => {
    expect(missingEnv({ ...xai, services: {} })).toEqual(["XAI_API_KEY"]);
    expect(missingEnv(google)).toEqual([]);
  });
});

describe("serviceState", () => {
  it("says available, what is needed, or that the build lacks it", () => {
    expect(serviceState(google, "llm")).toEqual({ state: "ok", text: "available" });
    expect(serviceState(google, "tts")).toEqual({ state: "absent", text: "not in this build" });
    expect(serviceState(xai, "llm")).toEqual({ state: "missing", text: "needs XAI_API_KEY" });
    expect(serviceState(xai, "tts")).toBeNull();
  });
});

describe("catalogLines", () => {
  it("lists models (voices for TTS), then languages", () => {
    expect(catalogLines(google)).toEqual([
      { service: "stt", text: "Chirp 2, Latest long · English (US)" },
      { service: "llm", text: "Gemini 2.5 Flash" },
      { service: "tts", text: "Chirp 3 HD Aoede" },
    ]);
  });
  it("says so when the catalog has nothing for a service", () => {
    expect(catalogLines(xai)).toEqual([
      { service: "stt", text: "No catalog entries" },
      { service: "llm", text: "No catalog entries" },
    ]);
  });
});

describe("flowsUsing", () => {
  const flat = flow("flat", { stt: { provider: "google" }, llm: { provider: "google" }, tts: { provider: "google" } });
  const graph = flow("graph", {
    entry: "a",
    nodes: [
      { id: "a", config: { stt: { provider: "google" }, llm: { provider: "google" }, tts: { provider: "google" } } },
      { id: "b", config: { llm: { provider: "xai" }, tts: { provider: "google" } } },
    ],
    edges: [],
  });
  it("counts a flow once, whichever stage or step uses the provider", () => {
    expect(flowsUsing([flat, graph], "google").map((f) => f.id)).toEqual(["flat", "graph"]);
    expect(flowsUsing([flat, graph], "xai").map((f) => f.id)).toEqual(["graph"]);
  });
  it("never counts an archived flow, a flow with no version, or a malformed config", () => {
    const gone = flow("gone", flat.current_version!.config, true);
    const bare = { ...flow("bare", {}), current_version: undefined } as AgentDTO;
    const odd = flow("odd", { nodes: "nope", llm: "google" });
    expect(flowsUsing([gone, bare, odd], "google")).toEqual([]);
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/providers/providerStats.test.ts`
Expected: FAIL — `Failed to resolve import "./providerStats"`.

- [ ] **Step 3: Implement**

`ui/src/routes/providers/providerStats.ts`:

```ts
import type { AgentDTO, ProviderStatus } from "../../api/client";

export const SERVICES = ["stt", "llm", "tts"] as const;
export type Service = (typeof SERVICES)[number];
export const SERVICE_NAMES: Record<Service, string> = { stt: "Speech-to-text", llm: "Language models", tts: "Text-to-speech" };

export function hasService(p: ProviderStatus, s: Service): boolean {
  return !!p.services[s];
}

/** "STT · LLM · TTS": the services the catalog lists for the provider. */
export function serviceList(p: ProviderStatus): string {
  return SERVICES.filter((s) => hasService(p, s))
    .map((s) => s.toUpperCase())
    .join(" · ");
}

/** The server reads keys from its environment only; there is no keyring to store one in yet (spec S3). */
export function credential(p: ProviderStatus): { text: string; ok: boolean } {
  return p.detected ? { text: "Environment variable", ok: true } : { text: "Not configured", ok: false };
}

/** The variables still to set: named by the provider's services (once each), else the provider's own. */
export function missingEnv(p: ProviderStatus): string[] {
  const named = [...new Set(SERVICES.flatMap((s) => p.services[s]?.missing_env ?? []))];
  if (named.length > 0) return named;
  return p.detected ? [] : p.env;
}

/** What one service can do now, or what it still needs. `null` when the catalog has no such service. */
export function serviceState(p: ProviderStatus, s: Service): { state: "ok" | "missing" | "absent"; text: string } | null {
  const svc = p.services[s];
  if (!svc) return null;
  if (svc.available) return { state: "ok", text: "available" };
  if (svc.reason === "missing_env") return { state: "missing", text: `needs ${(svc.missing_env ?? []).join(", ")}` };
  return { state: "absent", text: "not in this build" };
}

/** Per listed service: the models (voices for TTS), then the languages. */
export function catalogLines(p: ProviderStatus): { service: Service; text: string }[] {
  return SERVICES.flatMap((s) => {
    const svc = p.services[s];
    if (!svc) return [];
    const names = (s === "tts" ? svc.voices : svc.models) ?? [];
    const parts = [names.map((n) => n.label).join(", "), (svc.languages ?? []).map((l) => l.label).join(", ")].filter(Boolean);
    return [{ service: s, text: parts.join(" · ") || "No catalog entries" }];
  });
}

type Stage = { provider?: unknown };
type Block = Partial<Record<Service, Stage>>;

/** The provider each stage names: in a flat config, or in every step of a graph. */
function stageProviders(config: unknown): string[] {
  const root = (config ?? {}) as { nodes?: { config?: Block }[] } & Block;
  const blocks: Block[] = Array.isArray(root.nodes) ? root.nodes.map((n) => n.config ?? {}) : [root];
  return blocks
    .flatMap((b) => SERVICES.map((s) => (b[s] as Stage | undefined)?.provider))
    .filter((p): p is string => typeof p === "string" && p !== "");
}

/** Flows whose current version uses the provider in any stage or step. Archived flows never count. */
export function flowsUsing(flows: AgentDTO[], providerId: string): AgentDTO[] {
  return flows.filter((f) => !f.archived && stageProviders(f.current_version?.config).includes(providerId));
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run src/routes/providers/providerStats.test.ts && npx tsc --noEmit`
Expected: PASS. If the `odd` case throws, `stageProviders` must treat a non-array `nodes` as a flat config and a non-object stage (`llm: "google"`) as "no provider" (the `?.provider` on a string is `undefined`).

- [ ] **Step 5: Commit**

```bash
git add ui/src/routes/providers/providerStats.ts ui/src/routes/providers/providerStats.test.ts
git commit -m "feat(ui): provider figures — services, credential source, catalog and the flows using each

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `ProvidersView`, list and detail

**Files:**
- Modify: `ui/src/routes/providers/ProvidersView.tsx` (rewrite), `ui/src/routes/providers/ProvidersView.test.tsx` (rewrite), `ui/src/routes/sessions/CallsView.tsx` (class names only), `ui/src/styles.css`, `ui/src/test/noHardcodedColors.test.ts` (already lists `routes/providers/ProvidersView.tsx`; nothing to add)

**Interfaces:**
- Consumes: `providerStats`, `client.getProviders`, `client.getAgents`.
- Produces: `ProvidersView(props: { client: ApiClient; keyValue: string | null })`. State in the URL: `?provider=<id>` (the selected provider) and `?service=stt|llm|tts` (the filter). CSS: `.page-head` and `.chips` replace `.calls-head` and `.calls-chips` (renamed, shared with the Calls list).

- [ ] **Step 1: Write the failing tests** — replace `ui/src/routes/providers/ProvidersView.test.tsx` with:

```tsx
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { ProvidersView } from "./ProvidersView";
import type { AgentDTO, ApiClient, ProviderStatus } from "../../api/client";
import { ApiError } from "../../api/client";

const google: ProviderStatus = {
  id: "google",
  display_name: "Google",
  capabilities: ["stt", "llm"],
  detected: true,
  env: ["GOOGLE_API_KEY"],
  services: {
    stt: { available: true, custom: true, models: [{ id: "chirp_2", label: "Chirp 2" }], languages: [{ id: "en-US", label: "English (US)" }] },
    llm: { available: true, custom: true, models: [{ id: "gemini", label: "Gemini 2.5 Flash" }] },
    tts: { available: false, reason: "not_in_build", custom: true },
  },
};
const xai: ProviderStatus = {
  id: "xai",
  display_name: "xAI",
  capabilities: [],
  detected: false,
  env: ["XAI_API_KEY"],
  services: { llm: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true, models: [{ id: "grok", label: "Grok 4" }] } },
};
const flow = (id: string, name: string, config: Record<string, unknown>, archived = false): AgentDTO => ({
  id,
  name,
  description: "",
  enabled: true,
  archived,
  current_version: { id: `${id}-v`, number: 1, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});
const front = flow("f1", "Front desk", { stt: { provider: "google" }, llm: { provider: "google" }, tts: { provider: "google" } });
const graph = flow("f2", "Support triage", { entry: "a", nodes: [{ id: "a", config: { llm: { provider: "xai" } } }], edges: [] });

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}
function setup(client: Partial<ApiClient>, path = "/providers", key: string | null = "k") {
  const full = {
    getProviders: vi.fn().mockResolvedValue([google, xai]),
    getAgents: vi.fn().mockResolvedValue([front, graph]),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/providers"
            element={
              <>
                <ProvidersView client={full} keyValue={key} />
                <Where />
              </>
            }
          />
          <Route path="*" element={<Where />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}
const rowOf = (name: string) => screen.getByRole("button", { name }).closest("tr") as HTMLElement;

describe("ProvidersView", () => {
  it("prompts for a management key when absent", () => {
    setup({}, "/providers", null);
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("lists each provider with its services, where the credential comes from, and how many flows use it", async () => {
    setup({});
    await screen.findByRole("table");
    const g = within(rowOf("Google"));
    expect(g.getByText("STT · LLM · TTS")).toBeVisible();
    expect(g.getByText("Environment variable")).toBeVisible();
    expect(g.getByText("1")).toBeVisible();
    const x = within(rowOf("xAI"));
    expect(x.getByText("LLM")).toBeVisible();
    expect(x.getByText("Not configured")).toBeVisible();
    expect(x.getByText("1")).toBeVisible();
    expect(screen.getByText("1 of 2 providers detected.", { exact: false })).toBeVisible();
  });

  it("opens the first detected provider in the panel, and another when its row is chosen", async () => {
    const { user } = setup({});
    expect(await screen.findByRole("heading", { name: "Google", level: 2 })).toBeVisible();
    expect(screen.getByRole("button", { name: "Google" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "xAI" }));
    expect(screen.getByRole("heading", { name: "xAI", level: 2 })).toBeVisible();
    expect(screen.getByRole("button", { name: "xAI" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("No credential. Flows cannot use it yet.")).toBeVisible();
    expect(screen.getByTestId("where")).toHaveTextContent("/providers?provider=xai");
  });

  it("shows where the credential comes from, per variable, and never offers a field for a secret", async () => {
    const { user } = setup({});
    const panel = await screen.findByRole("complementary");
    expect(within(panel).getByText("GOOGLE_API_KEY")).toBeVisible();
    expect(within(panel).getByText("Set on the server")).toBeVisible();
    expect(within(panel).getByText(/never stored or shown here/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: "xAI" }));
    expect(within(screen.getByRole("complementary")).getByText("Not set")).toBeVisible();
    // The server cannot store a key (spec S3): nothing on the page may collect one.
    expect(document.querySelector("input, textarea, select, form")).toBeNull();
  });

  it("shows the catalog and what each service needs", async () => {
    setup({});
    const panel = await screen.findByRole("complementary");
    const stt = within(panel).getByText("Speech-to-text", { selector: "dt" }).closest("div") as HTMLElement;
    expect(within(stt).getByText("Chirp 2 · English (US)")).toBeVisible();
    expect(within(stt).getByText("available")).toBeVisible();
    const tts = within(panel).getByText("Text-to-speech", { selector: "dt" }).closest("div") as HTMLElement;
    expect(within(tts).getByText("not in this build")).toBeVisible();
  });

  it("names the variable that unlocks a service that needs one", async () => {
    const { user } = setup({});
    await screen.findByRole("table");
    await user.click(screen.getByRole("button", { name: "xAI" }));
    expect(within(screen.getByRole("complementary")).getByText("needs XAI_API_KEY")).toBeVisible();
  });

  it("filters by service, and keeps the filter in the URL", async () => {
    const { user } = setup({});
    await screen.findByRole("table");
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "Speech-to-text" }));
    expect(screen.getByRole("button", { name: "Speech-to-text" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("button", { name: "xAI" })).toBeNull();
    expect(screen.getByRole("button", { name: "Google" })).toBeVisible();
    expect(screen.getByTestId("where")).toHaveTextContent("service=stt");
  });

  it("ignores an unknown service or provider in the URL", async () => {
    setup({}, "/providers?service=bogus&provider=nope");
    expect(await screen.findByRole("heading", { name: "Google", level: 2 })).toBeVisible();
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "xAI" })).toBeVisible();
  });

  it("says which flows use the provider, once each, and links to them", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([front, graph, flow("old", "Old", front.current_version!.config, true)]) });
    const panel = await screen.findByRole("complementary");
    const used = within(panel).getByRole("region", { name: "Used by" });
    expect(within(used).getAllByRole("link").map((a) => a.textContent)).toEqual(["Front desk"]);
    expect(within(used).getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/flows/f1");
  });

  it("says when no flow uses the provider yet", async () => {
    setup({ getAgents: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByText("No flow uses this provider yet.")).toBeVisible();
  });

  it("still lists providers when the flow list fails, with the counts left out", async () => {
    setup({ getAgents: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    await screen.findByRole("table");
    expect(within(rowOf("Google")).getByText("—")).toBeVisible();
    expect(screen.getByText("The flows could not be loaded, so which flows use this provider is unknown.")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("says 'at least' when the flow list was cut at the API's page size", async () => {
    const many = Array.from({ length: 100 }, (_, i) => flow(`f${i}`, `Flow ${i}`, { llm: { provider: "google" } }));
    setup({ getAgents: vi.fn().mockResolvedValue(many) });
    await screen.findByRole("table");
    expect(within(rowOf("Google")).getByText("100+")).toBeVisible();
  });

  it("names the variables to set when nothing is detected", async () => {
    setup({ getProviders: vi.fn().mockResolvedValue([{ ...xai }, { ...google, detected: false, capabilities: [], env: ["GOOGLE_API_KEY"] }]) });
    expect(await screen.findByText(/no providers detected/i)).toBeVisible();
    expect(screen.getAllByText(/XAI_API_KEY/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/GOOGLE_API_KEY/).length).toBeGreaterThan(0);
    // Nothing detected: the panel opens the first provider anyway.
    expect(screen.getByRole("heading", { level: 2, name: "xAI" })).toBeVisible();
  });

  it("copes with a server that lists no providers at all", async () => {
    setup({ getProviders: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByText(/no providers detected/i)).toBeVisible();
    expect(screen.queryByRole("complementary")).toBeNull();
  });

  it("shows a load failure with a retry", async () => {
    setup({ getProviders: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });
});
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd ui && npx vitest run src/routes/providers/ProvidersView.test.tsx`
Expected: FAIL — the old table has no buttons, no panel, no `service` filter.

- [ ] **Step 3: Implement**

Replace `ui/src/routes/providers/ProvidersView.tsx`:

```tsx
import { Link, useSearchParams } from "react-router";
import { useQuery } from "@tanstack/react-query";
import type { AgentDTO, ApiClient, ApiError, ProviderStatus } from "../../api/client";
import { EmptyState, ErrorState, LoadingState } from "../../components/primitives";
import {
  SERVICES,
  SERVICE_NAMES,
  catalogLines,
  credential,
  flowsUsing,
  hasService,
  missingEnv,
  serviceList,
  serviceState,
  type Service,
} from "./providerStats";

export type ProvidersViewProps = { client: ApiClient; keyValue: string | null };

// GET /agents returns at most this many flows; a count that reaches it is a floor.
const FLOW_CAP = 100;

function Panel({ p, flows, flowsCapped }: { p: ProviderStatus; flows: AgentDTO[] | undefined; flowsCapped: boolean }) {
  const cred = credential(p);
  const missing = new Set(missingEnv(p));
  const using = flows ? flowsUsing(flows, p.id) : undefined;
  return (
    <aside aria-labelledby="provider-title" className="provider-panel">
      <div>
        <p className="subtext">Provider</p>
        <h2 id="provider-title">{p.display_name}</h2>
        <p className="provider-panel__health">
          <span className="lamp" data-tone={cred.ok ? "ok" : undefined} aria-hidden="true" />
          {cred.ok ? "Credential found in the server environment." : "No credential. Flows cannot use it yet."}
        </p>
      </div>

      <section aria-labelledby="provider-credential">
        <h3 id="provider-credential">Credential</h3>
        <ul className="provider-env">
          {p.env.map((name) => (
            <li key={name}>
              <code>{name}</code>
              <span className="subtext">{missing.has(name) ? "Not set" : "Set on the server"}</span>
            </li>
          ))}
        </ul>
        <p className="subtext">Set it in the server's environment and restart the server. Keys are never stored or shown here.</p>
      </section>

      <section aria-labelledby="provider-catalog">
        <h3 id="provider-catalog">Catalog</h3>
        <dl className="provider-catalog">
          {catalogLines(p).map((c) => {
            const state = serviceState(p, c.service);
            return (
              <div key={c.service}>
                <dt>{SERVICE_NAMES[c.service]}</dt>
                <dd>
                  {c.text}
                  {state && (
                    <span className="pill" data-state={state.state}>
                      {state.text}
                    </span>
                  )}
                </dd>
              </div>
            );
          })}
        </dl>
      </section>

      <section aria-labelledby="provider-used-by">
        <h3 id="provider-used-by">Used by</h3>
        {using === undefined ? (
          <p className="subtext">The flows could not be loaded, so which flows use this provider is unknown.</p>
        ) : using.length === 0 ? (
          <p className="subtext">No flow uses this provider yet.</p>
        ) : (
          <>
            <ul className="provider-flows">
              {using.map((f) => (
                <li key={f.id}>
                  <Link to={`/flows/${encodeURIComponent(f.id)}`}>{f.name}</Link>
                </li>
              ))}
            </ul>
            {flowsCapped && <p className="subtext">Only the first {FLOW_CAP} flows were checked.</p>}
          </>
        )}
      </section>
    </aside>
  );
}

/** Catalog providers, and what each one needs to be usable. Read-only: the server reads keys from its environment. */
export function ProvidersView({ client, keyValue }: ProvidersViewProps) {
  const [params, setParams] = useSearchParams();
  const providersQuery = useQuery({ queryKey: ["providers"], queryFn: client.getProviders, enabled: !!keyValue, retry: false });
  const flowsQuery = useQuery({ queryKey: ["agents"], queryFn: () => client.getAgents(), enabled: !!keyValue, retry: false });

  const rawService = params.get("service");
  const service: Service | "" = SERVICES.find((s) => s === rawService) ?? "";
  const setParam = (next: { provider?: string; service?: Service | "" }) => {
    const p = new URLSearchParams(params);
    if (next.provider !== undefined) p.set("provider", next.provider);
    if (next.service !== undefined) {
      if (next.service) p.set("service", next.service);
      else p.delete("service");
    }
    setParams(p, { replace: true });
  };

  const head = (summary?: string) => (
    <header className="page-head">
      <h1 tabIndex={-1}>Providers</h1>
      {summary && <span className="subtext">{summary}</span>}
    </header>
  );

  if (!keyValue) {
    return (
      <>
        {head()}
        <EmptyState title="Connect to view providers">Enter a management key above to see which providers the server detected.</EmptyState>
      </>
    );
  }
  if (providersQuery.isPending) return <>{head()}<LoadingState label="Loading providers" /></>;
  if (providersQuery.isError) {
    return (
      <>
        {head()}
        <ErrorState title="Failed to load providers" error={providersQuery.error as ApiError} onRetry={() => void providersQuery.refetch()} />
      </>
    );
  }

  const providers = providersQuery.data;
  const shown = service ? providers.filter((p) => hasService(p, service)) : providers;
  const selected = providers.find((p) => p.id === params.get("provider")) ?? providers.find((p) => p.detected) ?? providers[0];
  const flows = flowsQuery.data;
  const capped = (flows?.length ?? 0) >= FLOW_CAP;
  const detected = providers.filter((p) => p.detected).length;
  const countFor = (p: ProviderStatus) => {
    if (!flows) return "—";
    const n = flowsUsing(flows, p.id).length;
    return capped ? `${n}+` : String(n);
  };

  return (
    <>
      {head(`${detected} of ${providers.length} providers detected. Keys come from the server's environment and are never stored or shown here.`)}
      {detected === 0 && (
        <EmptyState title="No providers detected">
          Set at least one of these environment variables on the server and restart it: {[...new Set(providers.flatMap((p) => p.env))].join(", ")}.
        </EmptyState>
      )}
      <div className="providers-layout">
        <section aria-label="Providers" className="providers-main">
          <div role="group" aria-label="Filter by service" className="chips">
            <button type="button" className="chip-button" aria-pressed={service === ""} onClick={() => setParam({ service: "" })}>
              All
            </button>
            {SERVICES.map((s) => (
              <button key={s} type="button" className="chip-button" aria-pressed={service === s} onClick={() => setParam({ service: s })}>
                {SERVICE_NAMES[s]}
              </button>
            ))}
          </div>
          <div className="calls-table">
            <table>
              <caption className="visually-hidden">Providers and the services they unlock</caption>
              <thead>
                <tr>
                  <th scope="col">Provider</th>
                  <th scope="col">Services</th>
                  <th scope="col">Credential</th>
                  <th scope="col" className="num">
                    Flows
                  </th>
                </tr>
              </thead>
              <tbody>
                {shown.map((p) => {
                  const on = selected?.id === p.id;
                  const cred = credential(p);
                  return (
                    <tr key={p.id} data-selected={on || undefined}>
                      <td>
                        <button type="button" className="provider-pick" aria-pressed={on} onClick={() => setParam({ provider: p.id })}>
                          <span className="lamp" data-tone={cred.ok ? "ok" : undefined} aria-hidden="true" />
                          {p.display_name}
                        </button>
                      </td>
                      <td>{serviceList(p)}</td>
                      <td>{cred.text}</td>
                      <td className="num">{countFor(p)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </section>
        {selected && <Panel p={selected} flows={flows} flowsCapped={capped} />}
      </div>
    </>
  );
}
```

CSS in `ui/src/styles.css`:

1. Rename `.calls-head` to `.page-head` and `.calls-chips` to `.chips` (the rules and every use in `ui/src/routes/sessions/CallsView.tsx`).
2. Delete the now-unused `.provider-views` rule if present (`grep -rn "provider-views" ui/src`), keep `.pills` and `.pill` (the panel uses `.pill`).
3. Append:

```css
.providers-layout {
  display: flex;
  flex-wrap: wrap;
  gap: 28px 32px;
  align-items: flex-start;
}
.providers-main {
  flex: 999 1 520px;
  min-width: 0;
}
.provider-pick {
  display: inline-flex;
  align-items: center;
  gap: 10px;
  min-height: 44px;
  padding: 0;
  border: 0;
  background: none;
  color: var(--ink);
  font: inherit;
  font-weight: 600;
  text-align: left;
}
.calls-table tr[data-selected] td {
  background: var(--paper);
}
.calls-table tr[data-selected] td:first-child {
  outline: 2px solid var(--caller);
  outline-offset: -2px;
}
.provider-panel {
  flex: 1 1 340px;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 24px;
  padding: 24px;
  border: 1px solid var(--rule);
  border-radius: var(--radius);
  background: var(--surface);
}
.provider-panel p {
  margin: 0;
}
.provider-panel h2 {
  margin: 4px 0 0;
  font-size: 1.5rem;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.provider-panel h3 {
  margin: 0 0 8px;
  padding-bottom: 8px;
  border-bottom: 1px solid var(--ink);
  font-size: 0.8rem;
  font-weight: 600;
  color: var(--ink-2);
}
.provider-panel__health {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 6px;
  color: var(--ink-2);
}
.provider-env,
.provider-flows {
  list-style: none;
  margin: 0 0 8px;
  padding: 0;
}
.provider-env li {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 0;
  border-bottom: 1px solid var(--rule);
}
.provider-flows li {
  padding: 8px 0;
  border-bottom: 1px solid var(--rule);
}
.provider-catalog {
  margin: 0;
}
.provider-catalog > div {
  padding: 10px 0;
  border-bottom: 1px solid var(--rule);
}
.provider-catalog dt {
  color: var(--ink-2);
  font-size: 0.8rem;
}
.provider-catalog dd {
  margin: 2px 0 0;
}
.provider-catalog .pill {
  margin-left: 8px;
}
```

- [ ] **Step 4: Run it to see it pass**

Run: `cd ui && npx vitest run && npx tsc --noEmit`
Expected: PASS (the whole suite: `CallsView` tests still pass after the class rename; the colour and system CSS guards pass). If `getByText("1")` is ambiguous in a row, the Flows count is the only bare number in a provider row; check the Services cell does not render digits.

- [ ] **Step 5: Commit**

```bash
git add ui/src
git commit -m "feat(ui): Providers screen — filterable list beside a panel with credential source, catalog and the flows using each

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: End-to-end suites follow the new page

**Files:**
- Modify: `e2e/tests-graph/10-agent-graph.spec.ts`, `e2e/tests/10-full-stack.spec.ts`

**Interfaces:**
- Consumes: the page from Task 2.
- Produces: one new graph-suite test; the paid suite follows the new markup (not run).

- [ ] **Step 1: Add a graph-suite test.** In `e2e/tests-graph/10-agent-graph.spec.ts`, after the `providers: with no STT provider available…` test, add:

```ts
test("providers page: the detected provider is listed with where its credential comes from, and no field for a key", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Providers", exact: true }).first().click();
  const name = providers.find((p) => p.id === PROVIDER)!.display_name;
  const row = page.getByRole("row").filter({ hasText: name });
  await expect(row).toContainText("Environment variable");
  await row.getByRole("button", { name }).click();
  await expect(page.getByRole("heading", { name, level: 2 })).toBeVisible();
  const panel = page.getByRole("complementary");
  await expect(panel).toContainText("Set on the server");
  await expect(panel).toContainText("never stored or shown here");
  await expect(page.locator("input, textarea, form")).toHaveCount(0);
  await step(page, "providers page");
});
```

  and in the `layout:` test, after the `noSideways("providers")` check, add `await expect(page.getByRole("heading", { name: "Providers", level: 1 })).toBeVisible();` is already there; no change needed except that the page must still not scroll sideways (it is the existing assertion).

- [ ] **Step 2: Run the fake-provider suite**

Run: `make e2e-fake`
Expected: every graph spec passes, including the new one and the 390px layout check on the Providers page. If the 390px check fails, the provider panel is wider than the viewport: give `.providers-layout` `min-width: 0` children and let `.calls-table` scroll (it has `overflow-x: auto`).

- [ ] **Step 3: Update the paid suite** (`e2e/tests/10-full-stack.spec.ts`; costs money, do not run it as part of this task). Replace

```ts
  await expect(page.getByRole("row").filter({ hasText: "Google" })).toContainText("Detected");
```

with

```ts
  await expect(page.getByRole("row").filter({ hasText: "Google" })).toContainText("Environment variable");
```

  The check right after it (a provider secret is never in the page) stays as it is.

- [ ] **Step 4: List the paid suite's specs, to catch a compile error**

Run: `cd e2e && npx playwright test --list -c playwright.config.ts`
Expected: both suites list their specs without error.

- [ ] **Step 5: Commit**

```bash
git add e2e/tests-graph/10-agent-graph.spec.ts e2e/tests/10-full-stack.spec.ts
git commit -m "test(e2e): suites follow the Providers screen

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Gate, spec update, a look beside the canvas, and independent review

**Files:**
- Modify: `docs/superpowers/specs/2026-10-12-switchboard-console-design.md`

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Record what plan 4 left out, in the spec.** Append to Section 5's table:

```
| S15 | Provider health in the management API: requests, errors, median latency and circuit-breaker state per provider (today only in Prometheus `/metrics`) | The p50 column and the panel's "Last hour" figures (canvas board 15) |
| S16 | Test a provider's connection (`POST /providers/{id}/test`) | The panel's "Test connection" button |
```

In Section 3's table replace the row for boards 15–16 with two rows:

```
| 15 | Providers | `/providers` | **Done (plan 4).** A list filtered by service (kept in the URL) beside a panel with the credential source (which variables are set on the server), the catalog, and the flows using the provider. Read-only: no key field until S3; p50, Last hour and Test connection wait on S15, S16. |
| 16 | MCP tools | `/mcp-servers` | MCP schema-change review. Plan 5. |
```

and in Section 6 replace plan item 4 with `4. **Done:** Providers (board 15).`, add `5. MCP tools (board 16).` and renumber First run and empty/error states to `6.`. Add to Section 4: `- The Providers panel shows the credential's source and which variables are set, not the canvas's key input, "Save to keyring" or "Test connection" (S3, S16), and no p50 or "Last hour" (S15). Selected rows are outlined in --caller, not boxed with a shadow.`

- [ ] **Step 2: Full gate**

Run:

```bash
cd ui && npx vitest run && npx tsc --noEmit
cd .. && make e2e-fake
git status --short
```

Expected: all green; the tree is clean apart from the spec file.

- [ ] **Step 3: Look at it, next to the canvas.** The graph suite's `step(page, "providers page")` leaves `e2e/evidence/*-providers-page.png`. Compare it with canvas board 15 (`scratchpad/canvas/project/Providers.dc.html`, served over HTTP): the heading with its inline summary, the service pills, the bordered list with a selected-row outline, the panel's section rules. Fix any difference that does not need data the server lacks, each as a commit with the difference named.

- [ ] **Step 4: Independent review (AGENTS.md "Mandatory Code Review Workflow").** Dispatch a fresh reviewer on `main..HEAD` with this brief: YAGNI and bloat (any unused helper, export or CSS; did the rewrite drop anything a user needs, such as the variable names to set); correctness (URL state, defaulting of the selected provider, `flowsUsing` over flat and graph configs, the cap handling); verify `providerStats` against the server's `server/api/providers.go` (field names, `reason` values, `detected`); the five Review Focus items; accessibility (a table with a button per row, `aria-pressed`, the panel as a labelled `aside`, one `h1`); the "never show or accept a secret" rule; token-only colours; tests that pass for the wrong reason. Fix confirmed findings, each with a test that fails first.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-10-12-switchboard-console-design.md
git commit -m "docs: record what the Providers screen leaves for the server (S15, S16) and mark plan 4 done

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
