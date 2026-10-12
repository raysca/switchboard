import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { OnboardingView } from "./OnboardingView";
import type { ApiClient, ProviderStatus } from "../../api/client";

function renderWithProviders(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

const full = { available: true, custom: true };
const google: ProviderStatus = {
  id: "google", display_name: "Google", capabilities: ["stt", "llm", "tts"], detected: true, env: ["GOOGLE_API_KEY"],
  services: { stt: full, llm: full, tts: full },
};
const xai: ProviderStatus = {
  id: "xai", display_name: "xAI", capabilities: ["llm"], detected: true, env: ["XAI_API_KEY"],
  services: { llm: full },
};
const undetected: ProviderStatus = { ...google, detected: false, capabilities: [] };
const flow = {
  id: "agent-1", name: "Front Desk", description: "", enabled: true, archived: false,
  current_version: null, readiness: { state: "ready", reasons: [] },
  created_at: "2026-10-05T12:00:00Z", updated_at: "2026-10-05T12:00:00Z",
};

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    getProviders: vi.fn().mockResolvedValue([google]),
    getFlows: vi.fn().mockResolvedValue([]),
    createFlow: vi.fn().mockResolvedValue(flow),
    ...overrides,
  } as unknown as ApiClient;
}

describe("OnboardingView", () => {
  it("prompts for a management key when absent", () => {
    renderWithProviders(<OnboardingView client={createMockClient()} keyValue={null} />);
    expect(screen.getByRole("heading", { name: /onboarding setup/i })).toBeVisible();
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("step 1 says which variables to set and has no credential form", async () => {
    const client = createMockClient({ getProviders: vi.fn().mockResolvedValue([undetected, { ...xai, detected: false }]) });
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);
    expect(await screen.findByRole("heading", { name: /step 1: set a provider key/i })).toBeVisible();
    expect(screen.getByText(/GOOGLE_API_KEY/)).toBeVisible();
    expect(screen.getByText(/XAI_API_KEY/)).toBeVisible();
    expect(screen.queryByLabelText(/secret|api key/i)).toBeNull();
    expect(screen.queryByText(/add a credential|connect provider/i)).toBeNull();
  });

  it("advances to step 2 once any provider is detected and creates a flow without credential ids", async () => {
    const client = createMockClient();
    const user = userEvent.setup();
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);
    await screen.findByRole("heading", { name: /step 2: create your first flow/i });
    await user.type(screen.getByLabelText(/flow name/i), "Concierge");
    await user.click(screen.getByRole("button", { name: /create flow/i }));
    await waitFor(() => expect(client.createFlow).toHaveBeenCalled());
    const cfg = (client.createFlow as ReturnType<typeof vi.fn>).mock.calls[0][0].config;
    expect(cfg.stt).toEqual({ provider: "google", model: "chirp_2", language: "en-US" });
    expect(cfg.llm).toEqual({ provider: "google", model: "gemini-2.5-flash" });
    expect(cfg.tts).toEqual({ provider: "google", voice: "en-US-Chirp3-HD-Aoede" });
  });

  it("explains why Create flow cannot run when no detected provider covers every stage", async () => {
    const client = createMockClient({ getProviders: vi.fn().mockResolvedValue([xai]) });
    const user = userEvent.setup();
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);
    await screen.findByRole("heading", { name: /step 2: create your first flow/i });
    await user.type(screen.getByLabelText(/flow name/i), "Concierge");
    await user.click(screen.getByRole("button", { name: /create flow/i }));
    expect(await screen.findByText(/no detected provider can run/i)).toBeVisible();
    expect(client.createFlow).not.toHaveBeenCalled();
  });

  it("shows the configured state when a provider is detected and a flow exists", async () => {
    const client = createMockClient({ getFlows: vi.fn().mockResolvedValue([flow]) });
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);
    expect(await screen.findByText(/system configured and ready/i)).toBeVisible();
    expect(screen.getByText("Front Desk")).toBeVisible();
    expect(screen.getByRole("link", { name: /manage flows/i })).toBeVisible();
    expect(screen.getByRole("link", { name: /view providers/i })).toBeVisible();
  });

  it("treats an install whose only flow is archived as having no flows", async () => {
    const client = createMockClient({ getFlows: vi.fn().mockResolvedValue([{ ...flow, archived: true }]) });
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);
    expect(await screen.findByRole("heading", { name: /step 2: create your first flow/i })).toBeVisible();
  });
});
