import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { OnboardingView } from "./OnboardingView";
import { ApiError, type ApiClient } from "../../api/client";

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

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    serverConfig: vi.fn(),
    issueVoiceToken: vi.fn(),
    getProviders: vi.fn().mockResolvedValue([
      { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], auth_required: true },
    ]),
    getCredentials: vi.fn().mockResolvedValue([]),
    createCredential: vi.fn(),
    getCredential: vi.fn(),
    updateCredential: vi.fn(),
    deleteCredential: vi.fn(),
    validateCredential: vi.fn(),
    rotateCredential: vi.fn(),
    getProviderDiscovery: vi.fn(),
    getAgents: vi.fn().mockResolvedValue([]),
    createAgent: vi.fn(),
    getAgent: vi.fn(),
    updateAgent: vi.fn(),
    archiveAgent: vi.fn(),
    duplicateAgent: vi.fn(),
    updateAgentMCPTools: vi.fn(),
    ...overrides,
  } as ApiClient;
}

describe("OnboardingView", () => {
  it("shows prompt to connect when no management key is entered", () => {
    const client = createMockClient();
    renderWithProviders(<OnboardingView client={client} keyValue={null} />);
    expect(screen.getByRole("heading", { name: /onboarding setup/i })).toBeVisible();
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("guides empty install (0 credentials, 0 agents) through step 1: connect provider", async () => {
    const client = createMockClient({
      getCredentials: vi.fn().mockResolvedValue([]),
      getAgents: vi.fn().mockResolvedValue([]),
    });
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);

    expect(await screen.findByRole("heading", { name: /step 1: connect your first provider/i })).toBeVisible();
    expect(screen.getByLabelText("Select provider")).toBeVisible();
    const secretInput = screen.getByLabelText(/secret|api key/i);
    expect(secretInput).toHaveAttribute("type", "password");
  });

  it("submits credential, masks value, clears secret input, and advances to step 2: create agent", async () => {
    let credentialsList: any[] = [];
    const client = createMockClient({
      getCredentials: vi.fn().mockImplementation(() => Promise.resolve(credentialsList)),
      getAgents: vi.fn().mockResolvedValue([]),
      createCredential: vi.fn().mockImplementation(async ({ provider, secret }) => {
        expect(secret).toBe("secret-api-token-12345");
        const newCred = {
          id: "cred-new",
          provider,
          masked_value: "••••••••2345",
          validation_state: "unvalidated",
          capabilities: ["llm", "stt", "tts"],
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:00:00Z",
        };
        credentialsList = [newCred];
        return newCred;
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);

    const secretInput = await screen.findByLabelText(/secret|api key/i);
    await user.type(secretInput, "secret-api-token-12345");
    await user.click(screen.getByRole("button", { name: /connect provider/i }));

    expect(client.createCredential).toHaveBeenCalledWith({
      provider: "google",
      secret: "secret-api-token-12345",
    });

    // Advances to step 2
    expect(await screen.findByRole("heading", { name: /step 2: create your first agent/i })).toBeVisible();
    expect(screen.getByText(/••••••••2345/)).toBeVisible();
    // Secret is never leaked into text
    expect(screen.queryByText("secret-api-token-12345")).toBeNull();
  });

  it("creates first agent, displays readiness badge, and confirms completion", async () => {
    let agentsList: any[] = [];
    const client = createMockClient({
      getCredentials: vi.fn().mockResolvedValue([
        {
          id: "cred-1",
          provider: "google",
          masked_value: "••••••••1234",
          validation_state: "valid",
          capabilities: ["llm", "stt", "tts"],
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:00:00Z",
        },
      ]),
      getAgents: vi.fn().mockImplementation(() => Promise.resolve(agentsList)),
      createAgent: vi.fn().mockImplementation(async (payload) => {
        const newAgent = {
          id: "agent-1",
          name: payload.name,
          description: payload.description,
          enabled: true,
          archived: false,
          current_version: {
            id: "ver-1",
            number: 1,
            schema_version: 1,
            config: payload.config,
            config_hash: "hash-001",
            created_at: "2026-10-05T12:00:00Z",
            mcp_tools: [],
          },
          readiness: {
            state: "ready",
            reasons: [],
          },
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:00:00Z",
        };
        agentsList = [newAgent];
        return newAgent;
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);

    expect(await screen.findByRole("heading", { name: /step 2: create your first agent/i })).toBeVisible();

    await user.type(screen.getByLabelText(/agent name/i), "Concierge");
    await user.type(screen.getByLabelText(/description/i), "Primary concierge agent");
    await user.click(screen.getByRole("button", { name: /create agent/i }));

    expect(client.createAgent).toHaveBeenCalledWith(
      expect.objectContaining({
        name: "Concierge",
        description: "Primary concierge agent",
        config: expect.objectContaining({
          stt: "google",
        }),
      }),
    );

    expect(await screen.findByText(/system configured and ready/i)).toBeVisible();
    expect(screen.getByText("Concierge")).toBeVisible();
    expect(screen.getByText("Ready")).toBeVisible();
  });

  it("displays error banner with request ID when credential creation fails", async () => {
    const client = createMockClient({
      getCredentials: vi.fn().mockResolvedValue([]),
      getAgents: vi.fn().mockResolvedValue([]),
      createCredential: vi.fn().mockRejectedValue(
        new ApiError("Invalid credential payload", "invalid_request", 400, "req-fail-42"),
      ),
    });

    const user = userEvent.setup();
    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);

    const secretInput = await screen.findByLabelText(/secret|api key/i);
    await user.type(secretInput, "invalid-key");
    await user.click(screen.getByRole("button", { name: /connect provider/i }));

    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByText("Invalid credential payload")).toBeVisible();
    expect(screen.getByText(/req-fail-42/)).toBeVisible();
  });

  it("shows system is already configured when credentials and agents exist", async () => {
    const client = createMockClient({
      getCredentials: vi.fn().mockResolvedValue([
        {
          id: "cred-1",
          provider: "google",
          masked_value: "••••••••1234",
          validation_state: "valid",
          capabilities: ["llm", "stt", "tts"],
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:00:00Z",
        },
      ]),
      getAgents: vi.fn().mockResolvedValue([
        {
          id: "agent-1",
          name: "Front Desk",
          description: "Greeting bot",
          enabled: true,
          archived: false,
          current_version: {
            id: "ver-1",
            number: 1,
            schema_version: 1,
            config: { stt: "google", llm: "google", tts: "google" },
            config_hash: "hash-001",
            created_at: "2026-10-05T12:00:00Z",
            mcp_tools: [],
          },
          readiness: { state: "ready", reasons: [] },
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:00:00Z",
        },
      ]),
    });

    renderWithProviders(<OnboardingView client={client} keyValue="key-1" />);
    expect(await screen.findByText(/system configured and ready/i)).toBeVisible();
    expect(screen.getByText("Front Desk")).toBeVisible();
    expect(screen.getByRole("link", { name: /manage agents/i })).toBeVisible();
    expect(screen.getByRole("link", { name: /manage providers/i })).toBeVisible();
  });
});
