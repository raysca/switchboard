import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { OnboardingView } from "./OnboardingView";
import { ApiError, type ApiClient, type FlowDTO, type ProviderStatus } from "../../api/client";

vi.mock("../flows/FlowVoiceTest", () => ({ FlowVoiceTest: ({ flow }: { flow: { name: string } }) => <div data-testid="voice-test">{flow.name}</div> }));

const svc = (extra: object = {}) => ({ available: true, custom: true, ...extra });
const google: ProviderStatus = {
  id: "google",
  display_name: "Google",
  capabilities: ["stt", "llm", "tts"],
  detected: true,
  env: ["GOOGLE_API_KEY"],
  services: {
    stt: svc({ models: [{ id: "chirp_2", label: "Chirp 2", default: true }], languages: [{ id: "en-US", label: "English", default: true }] }),
    llm: svc({ models: [{ id: "gemini-2.5-flash", label: "Gemini 2.5 Flash", default: true }] }),
    tts: svc({ voices: [{ id: "aoede", label: "Aoede", default: true }, { id: "puck", label: "Puck" }] }),
  },
};
const xai: ProviderStatus = {
  id: "xai",
  display_name: "xAI",
  capabilities: [],
  detected: false,
  env: ["XAI_API_KEY"],
  services: {
    stt: svc({ available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], models: [{ id: "scribe", label: "Scribe" }] }),
    llm: svc({ available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], models: [{ id: "grok", label: "Grok" }] }),
    tts: svc({ available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], voices: [{ id: "ara", label: "Ara" }] }),
  },
};
const flow: FlowDTO = {
  id: "flow-1",
  name: "Front Desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: null,
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
};

function Where() {
  const l = useLocation();
  return <p data-testid="where">{l.pathname + l.search}</p>;
}
function setup(client: Partial<ApiClient> = {}, path = "/onboarding", key: string | null = "k") {
  const full = {
    getProviders: vi.fn().mockResolvedValue([google]),
    getFlows: vi.fn().mockResolvedValue([]),
    createFlow: vi.fn().mockResolvedValue(flow),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route
            path="/onboarding"
            element={
              <>
                <OnboardingView client={full} keyValue={key} />
                <Where />
              </>
            }
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}
const stepNav = () => within(screen.getByRole("navigation", { name: "Setup steps" }));

describe("OnboardingView", () => {
  it("prompts for a management key when absent", () => {
    setup({}, "/onboarding", null);
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("shows the four steps, with the current one marked", async () => {
    setup();
    await screen.findByRole("heading", { name: "Choose how your flow hears, thinks and speaks." });
    expect(stepNav().getAllByRole("listitem").map((li) => li.textContent)).toEqual(["1Voice stackProviders for each stage", "2FlowName, job, voice", "3Test callTalk to it in the browser", "4ConnectToken and web client"]);
    expect(stepNav().getByText("Voice stack").closest("li")).toHaveAttribute("aria-current", "step");
  });

  it("step 1 offers the stacks the catalog allows, with the ready one chosen, and no field for a key", async () => {
    setup({ getProviders: vi.fn().mockResolvedValue([xai, google]) });
    const group = await screen.findByRole("radiogroup", { name: "Voice stack" });
    const radios = within(group).getAllByRole("radio");
    expect(radios.map((r) => r.getAttribute("aria-label") ?? r.closest("label")?.textContent)).toHaveLength(2);
    expect(within(group).getByRole("radio", { name: /Google/ })).toBeChecked();
    expect(within(group).getByText("Chirp 2 → Gemini 2.5 Flash → Aoede")).toBeVisible();
    expect(within(group).getByRole("radio", { name: /xAI/ })).toBeDisabled();
    expect(within(group).getByText("Set XAI_API_KEY on the server and restart it")).toBeVisible();
    expect(document.querySelector('input[type="password"], input[type="url"], textarea')).toBeNull();
  });

  it("keeps Continue off, and says what to set, when no stack is ready", async () => {
    setup({ getProviders: vi.fn().mockResolvedValue([xai]) });
    expect(await screen.findByRole("button", { name: "Continue" })).toBeDisabled();
    expect(screen.getByText("Set a provider key on the server to continue")).toBeVisible();
  });

  it("says so when no provider covers every stage, naming the variables to set", async () => {
    const deepgram: ProviderStatus = { id: "deepgram", display_name: "Deepgram", capabilities: ["stt"], detected: true, env: ["DEEPGRAM_API_KEY"], services: { stt: svc({ models: [{ id: "nova-3", label: "Nova 3" }] }) } };
    setup({ getProviders: vi.fn().mockResolvedValue([deepgram]) });
    expect(await screen.findByText(/No provider on this server covers speech-to-text, a language model and text-to-speech yet/)).toBeVisible();
    expect(screen.getByText(/DEEPGRAM_API_KEY/)).toBeVisible();
    expect(screen.getByRole("link", { name: "See providers" })).toHaveAttribute("href", "/providers");
  });

  it("continues to step 2 and creates the flow from the catalog's defaults and the typed job", async () => {
    const { client, user } = setup();
    await user.click(await screen.findByRole("button", { name: "Continue" }));
    expect(screen.getByTestId("where")).toHaveTextContent("step=2");
    await user.type(screen.getByLabelText("Name"), "Front desk");
    await user.selectOptions(screen.getByLabelText("Voice"), "puck");
    await user.clear(screen.getByLabelText("What should it do?"));
    await user.type(screen.getByLabelText("What should it do?"), "Answer for Northline.");
    await user.click(screen.getByRole("button", { name: "Create and call it" }));
    await waitFor(() => expect(client.createFlow).toHaveBeenCalledOnce());
    expect(client.createFlow).toHaveBeenCalledWith({
      name: "Front desk",
      description: "",
      enabled: true,
      config: {
        stt: { provider: "google", model: "chirp_2", language: "en-US" },
        llm: { provider: "google", model: "gemini-2.5-flash", system_prompt: "Answer for Northline." },
        tts: { provider: "google", voice: "puck" },
      },
    });
    await waitFor(() => expect(screen.getByTestId("where")).toHaveTextContent("step=3&flow=flow-1"));
  });

  it("needs a name before it creates anything", async () => {
    const { client, user } = setup({}, "/onboarding?step=2");
    await user.click(await screen.findByRole("button", { name: "Create and call it" }));
    expect(client.createFlow).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Name")).toBeInvalid();
  });

  it("shows a failed create and stays on step 2", async () => {
    const { user } = setup({ createFlow: vi.fn().mockRejectedValue(new ApiError("name already in use", "conflict", 409, "req-1")) }, "/onboarding?step=2");
    await user.type(await screen.findByLabelText("Name"), "Front desk");
    await user.click(screen.getByRole("button", { name: "Create and call it" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("name already in use");
    expect(screen.getByTestId("where")).toHaveTextContent("step=2");
  });

  it("goes back from step 2 to step 1", async () => {
    const { user } = setup({}, "/onboarding?step=2");
    await user.click(await screen.findByRole("button", { name: "Back" }));
    expect(screen.getByTestId("where")).toHaveTextContent("step=1");
  });

  it("treats a garbage step as step 1, and step 2 without a ready stack as step 1", async () => {
    setup({}, "/onboarding?step=banana");
    expect(await screen.findByRole("heading", { name: "Choose how your flow hears, thinks and speaks." })).toBeVisible();
  });

  it("sends step 2 without a ready stack back to step 1", async () => {
    setup({ getProviders: vi.fn().mockResolvedValue([xai]) }, "/onboarding?step=2");
    expect(await screen.findByRole("heading", { name: "Choose how your flow hears, thinks and speaks." })).toBeVisible();
  });

  it("sends step 3 or 4 without a known flow back to step 2", async () => {
    setup({}, "/onboarding?step=3");
    expect(await screen.findByRole("heading", { name: "Tell it what job it's doing." })).toBeVisible();
  });

  it("shows the configured summary when a provider is detected and a flow exists, with ways back in", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([flow]) });
    expect(await screen.findByText(/system configured and ready/i)).toBeVisible();
    expect(screen.getByText("Front Desk")).toBeVisible();
    expect(screen.getByRole("link", { name: "Manage flows" })).toHaveAttribute("href", "/flows");
    expect(screen.getByRole("link", { name: "View providers" })).toHaveAttribute("href", "/providers");
    expect(screen.getByRole("link", { name: "Test a flow" })).toHaveAttribute("href", "/onboarding?step=3&flow=flow-1");
    expect(screen.getByRole("link", { name: "Connect a client" })).toHaveAttribute("href", "/onboarding?step=4&flow=flow-1");
  });

  it("treats an install whose only flow is archived as having no flows", async () => {
    setup({ getFlows: vi.fn().mockResolvedValue([{ ...flow, archived: true }]) });
    expect(await screen.findByRole("heading", { name: "Choose how your flow hears, thinks and speaks." })).toBeVisible();
  });

  it("shows a load failure with a retry, and says live calls are fine", async () => {
    setup({ getProviders: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("alert")).toHaveTextContent("Live calls are not affected");
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });
});
