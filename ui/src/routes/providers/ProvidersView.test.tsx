import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { ProvidersView } from "./ProvidersView";
import type { FlowDTO, ApiClient, ProviderStatus, ProviderTest } from "../../api/client";
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
const flow = (id: string, name: string, config: Record<string, unknown>, archived = false): FlowDTO => ({
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
const graph = flow("f2", "Support triage", { start: "a", steps: [{ id: "a", config: { llm: { provider: "xai" } } }], routes: [] });

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}
function setup(client: Partial<ApiClient>, path = "/providers", key: string | null = "k") {
  const full = {
    getProviders: vi.fn().mockResolvedValue([google, xai]),
    getFlows: vi.fn().mockResolvedValue([front, graph]),
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
    // The API names only the variables still missing; it cannot say a variable is set, so the page does not.
    expect(within(panel).queryByText("Set on the server")).toBeNull();
    expect(within(panel).queryByText("Not set")).toBeNull();
    expect(within(panel).getByText("Credential found in the server environment.")).toBeVisible();
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
    setup({ getFlows: vi.fn().mockResolvedValue([front, graph, flow("old", "Old", front.current_version!.config, true)]) });
    const panel = await screen.findByRole("complementary");
    const used = within(panel).getByRole("region", { name: "Used by" });
    expect(within(used).getAllByRole("link").map((a) => a.textContent)).toEqual(["Front desk"]);
    expect(within(used).getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/flows/f1");
  });

  it("says when no flow uses the provider yet", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByText("No flow uses this provider yet.")).toBeVisible();
  });

  it("still lists providers when the flow list fails, with the counts left out", async () => {
    setup({ getFlows: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    await screen.findByRole("table");
    expect(within(rowOf("Google")).getByText("—")).toBeVisible();
    expect(screen.getByText("The flows could not be loaded, so which flows use this provider is unknown.")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("says 'at least' when the flow list was cut at the API's page size", async () => {
    const many = Array.from({ length: 100 }, (_, i) => flow(`f${i}`, `Flow ${i}`, { llm: { provider: "google" } }));
    setup({ getFlows: vi.fn().mockResolvedValue(many) });
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

  it("says a provider with no match is unchecked beyond the first 100 flows, instead of 'none use it'", async () => {
    const many = Array.from({ length: 100 }, (_, i) => flow(`f${i}`, `Flow ${i}`, { llm: { provider: "google" } }));
    const { user } = setup({ getFlows: vi.fn().mockResolvedValue(many) });
    await screen.findByRole("table");
    await user.click(screen.getByRole("button", { name: "xAI" }));
    expect(screen.getByText("No flow among the first 100 uses this provider; later flows were not checked.")).toBeVisible();
    expect(screen.queryByText("No flow uses this provider yet.")).toBeNull();
  });

  it("counts a provider that a flow names only as a fallback", async () => {
    const withFallback = flow("fb", "Failover", { llm: { provider: "google" }, tts: { provider: "google", fallbacks: [{ provider: "cartesia" }] } });
    const cartesia: ProviderStatus = { id: "cartesia", display_name: "Cartesia", capabilities: [], detected: false, env: ["CARTESIA_API_KEY"], services: { tts: { available: false, reason: "missing_env", missing_env: ["CARTESIA_API_KEY"], custom: true } } };
    const { user } = setup({ getProviders: vi.fn().mockResolvedValue([google, cartesia]), getFlows: vi.fn().mockResolvedValue([withFallback]) });
    await screen.findByRole("table");
    expect(within(rowOf("Cartesia")).getByText("1")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Cartesia" }));
    expect(within(screen.getByRole("region", { name: "Used by" })).getByRole("link", { name: "Failover" })).toBeVisible();
  });

  it("explains where the catalog comes from, and how one provider covers many endpoints", async () => {
    setup({});
    await screen.findByRole("table");
    expect(screen.getByText(/Catalog providers are defined in YAML and can be overlaid per deployment\./)).toBeVisible();
    expect(screen.getByText(/OPENAI_BASE_URL/, { selector: "p" })).toBeVisible();
  });

  describe("Test connection", () => {
    const tested: ProviderTest = {
      provider: "google",
      checked_at: "2026-10-14T10:00:00Z",
      results: [
        { stage: "stt", status: "ok", circuit: "closed" },
        { stage: "llm", status: "ok", latency_ms: 410, circuit: "closed" },
        { stage: "tts", status: "error", detail: "client_error: bad voice", circuit: "open" },
      ],
    };

    it("asks nothing until the button is pressed, then lists each service", async () => {
      const testProvider = vi.fn().mockResolvedValue(tested);
      const { user } = setup({ testProvider });
      await screen.findByRole("heading", { name: "Google", level: 2 });
      expect(testProvider).not.toHaveBeenCalled();
      await user.click(screen.getByRole("button", { name: "Test connection" }));
      expect(testProvider).toHaveBeenCalledWith("google");
      const list = await screen.findByRole("list", { name: "Connection test results" });
      const items = within(list).getAllByRole("listitem").map((li) => li.textContent);
      expect(items).toEqual([
        "Speech-to-text — OK",
        "Language models — OK, 410 ms",
        "Text-to-speech — Error: client_error: bad voice · Circuit open",
      ]);
      expect(screen.getByText(/^Checked /)).toBeVisible();
    });

    it("announces results through a live region that exists before they arrive", async () => {
      const { user } = setup({ testProvider: vi.fn().mockResolvedValue(tested) });
      await screen.findByRole("heading", { name: "Google", level: 2 });
      const region = screen.getByRole("status");
      expect(region).toHaveAttribute("aria-live", "polite");
      expect(within(region).queryByRole("list")).toBeNull();
      await user.click(screen.getByRole("button", { name: "Test connection" }));
      expect(await within(region).findByRole("list", { name: "Connection test results" })).toBeVisible();
    });

    it("cannot be pressed for a provider with no credential, and says why", async () => {
      const { user } = setup({}, "/providers?provider=xai");
      await screen.findByRole("heading", { name: "xAI", level: 2 });
      expect(screen.getByRole("button", { name: "Test connection" })).toBeDisabled();
      expect(screen.getByText("No credential. Set the variable first.")).toBeVisible();
      await user.click(screen.getByRole("button", { name: "Test connection" }));
    });

    it("shows the server's try-again message and lets the button be pressed again", async () => {
      const testProvider = vi.fn().mockRejectedValue(new ApiError("this provider was checked a moment ago; try again in 5 s", "probe_too_soon", 429, "r1"));
      const { user } = setup({ testProvider });
      await user.click(await screen.findByRole("button", { name: "Test connection" }));
      expect(await screen.findByRole("alert")).toHaveTextContent("try again in 5 s");
      expect(screen.getByRole("button", { name: "Test connection" })).toBeEnabled();
    });

    it("forgets the results when another provider is chosen", async () => {
      const { user } = setup({ testProvider: vi.fn().mockResolvedValue(tested) });
      await user.click(await screen.findByRole("button", { name: "Test connection" }));
      await screen.findByRole("list", { name: "Connection test results" });
      await user.click(screen.getByRole("button", { name: "xAI" }));
      expect(screen.queryByRole("list", { name: "Connection test results" })).toBeNull();
    });
  });
});
