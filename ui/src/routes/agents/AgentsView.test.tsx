import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { AgentsView } from "./AgentsView";
import { ApiError, type ApiClient, type AgentDTO } from "../../api/client";

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

function createMockAgent(overrides: Partial<AgentDTO> = {}): AgentDTO {
  return {
    id: "agent-1",
    name: "Customer Support",
    description: "Helps users with questions",
    enabled: true,
    archived: false,
    current_version: {
      id: "ver-101",
      number: 1,
      schema_version: 1,
      config: { stt: "google", llm: "google", tts: "google" },
      config_hash: "hash-alpha-1",
      created_at: "2026-10-05T12:00:00Z",
      mcp_tools: [],
    },
    readiness: {
      state: "ready",
      reasons: [],
    },
    created_at: "2026-10-05T12:00:00Z",
    updated_at: "2026-10-05T12:00:00Z",
    ...overrides,
  };
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
    getAgents: vi.fn().mockResolvedValue([createMockAgent()]),
    createAgent: vi.fn(),
    getAgent: vi.fn().mockImplementation((id) => Promise.resolve(createMockAgent({ id }))),
    updateAgent: vi.fn(),
    archiveAgent: vi.fn(),
    duplicateAgent: vi.fn(),
    updateAgentMCPTools: vi.fn(),
    ...overrides,
  };
}

describe("AgentsView", () => {
  it("renders prompt when management key is missing", () => {
    const client = createMockClient();
    renderWithProviders(<AgentsView client={client} keyValue={null} />);
    expect(screen.getByRole("heading", { level: 1, name: /agents/i })).toBeVisible();
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("lists agents with readiness status badges and version numbers", async () => {
    const client = createMockClient({
      getAgents: vi.fn().mockResolvedValue([
        createMockAgent({
          id: "agent-1",
          name: "Front Desk",
          readiness: { state: "ready", reasons: [] },
        }),
        createMockAgent({
          id: "agent-2",
          name: "Unconfigured Bot",
          readiness: { state: "not_ready", reasons: ["missing_stt", "missing_tts"] },
        }),
      ]),
    });

    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Front Desk")).toBeVisible();
    expect(screen.getByText("Unconfigured Bot")).toBeVisible();
    expect(screen.getByText("Ready")).toBeVisible();
    expect(screen.getByText("Not ready")).toBeVisible();
    expect(screen.getByText("missing_stt")).toBeVisible();
    expect(screen.getByText("missing_tts")).toBeVisible();
  });

  it("creates a new agent with STT, LLM, and TTS settings", async () => {
    let agents = [createMockAgent()];
    const client = createMockClient({
      getAgents: vi.fn().mockImplementation(() => Promise.resolve(agents)),
      createAgent: vi.fn().mockImplementation(async (payload) => {
        const created = createMockAgent({
          id: "agent-new",
          name: payload.name,
          description: payload.description,
          current_version: {
            id: "ver-new",
            number: 1,
            schema_version: 1,
            config: payload.config,
            config_hash: "hash-new",
            created_at: "2026-10-05T13:00:00Z",
            mcp_tools: [],
          },
        });
        agents = [...agents, created];
        return created;
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Customer Support")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /create agent/i }));

    await user.type(screen.getByLabelText(/agent name/i), "Billing Bot");
    await user.type(screen.getByLabelText(/description/i), "Handles invoice questions");
    await user.type(screen.getByLabelText(/stt|speech-to-text/i), "google");
    await user.type(screen.getByLabelText(/llm/i), "gemini-2.0-flash");
    await user.type(screen.getByLabelText(/tts|text-to-speech/i), "en-US-Journey-F");

    await user.click(screen.getByRole("button", { name: /save agent/i }));

    expect(client.createAgent).toHaveBeenCalledWith(
      expect.objectContaining({
        name: "Billing Bot",
        description: "Handles invoice questions",
        config: expect.objectContaining({
          stt: "google",
          llm: "gemini-2.0-flash",
          tts: "en-US-Journey-F",
        }),
      }),
    );

    expect(await screen.findByText("Billing Bot")).toBeVisible();
  });

  it("edits an agent and publishes the next immutable version with new hash", async () => {
    let current = createMockAgent({
      id: "agent-1",
      name: "Support Bot",
      current_version: {
        id: "ver-1",
        number: 1,
        schema_version: 1,
        config: { stt: "google", llm: "google", tts: "google" },
        config_hash: "hash-v1",
        created_at: "2026-10-05T12:00:00Z",
        mcp_tools: [],
      },
    });

    const client = createMockClient({
      getAgents: vi.fn().mockImplementation(() => Promise.resolve([current])),
      getAgent: vi.fn().mockImplementation((id) => Promise.resolve(current)),
      updateAgent: vi.fn().mockImplementation(async (id, payload) => {
        current = {
          ...current,
          name: payload.name ?? current.name,
          current_version: {
            id: "ver-2",
            number: 2,
            schema_version: 1,
            config: payload.config ?? current.current_version!.config,
            config_hash: "hash-v2-published",
            created_at: "2026-10-05T13:30:00Z",
            mcp_tools: [],
          },
        };
        return current;
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Support Bot")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /edit/i }));

    expect(screen.getByText(/publish a new immutable version/i)).toBeVisible();

    const nameInput = screen.getByLabelText(/agent name/i);
    await user.clear(nameInput);
    await user.type(nameInput, "Revised Support Bot");

    await user.click(screen.getByRole("button", { name: /save changes|save agent/i }));

    expect(client.updateAgent).toHaveBeenCalledWith(
      "agent-1",
      expect.objectContaining({
        name: "Revised Support Bot",
      }),
    );

    // Displays updated version v2 and config hash
    expect(await screen.findByText("Revised Support Bot")).toBeVisible();
    expect(screen.getAllByText(/v2/)[0]).toBeVisible();
    expect(screen.getAllByText(/hash-v2-published/)[0]).toBeVisible();
  });

  it("views agent detail with immutable version history metadata", async () => {
    const client = createMockClient({
      getAgents: vi.fn().mockResolvedValue([
        createMockAgent({
          id: "agent-99",
          name: "Detailed Agent",
          current_version: {
            id: "ver-snap-99",
            number: 3,
            schema_version: 1,
            config: { stt: "google", llm: "gemini-2.0-flash", tts: "journey" },
            config_hash: "sha256-abcdef012345",
            created_at: "2026-10-05T12:00:00Z",
            mcp_tools: [
              {
                mcp_server_id: "mcp-server-1",
                remote_name: "weather_lookup",
                schema_hash: "sh-999",
                required: true,
              },
            ],
          },
        }),
      ]),
    });

    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Detailed Agent")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /view detail|view/i }));

    expect(await screen.findByRole("heading", { name: /detailed agent/i })).toBeVisible();
    expect(screen.getByText("ver-snap-99")).toBeVisible();
    expect(screen.getByText("sha256-abcdef012345")).toBeVisible();
    expect(screen.getByText("weather_lookup")).toBeVisible();
    expect(screen.getByRole("heading", { name: /immutable snapshot architecture/i })).toBeVisible();
  });

  it("duplicates an agent", async () => {
    let agents = [createMockAgent({ id: "agent-1", name: "Original Agent" })];
    const client = createMockClient({
      getAgents: vi.fn().mockImplementation(() => Promise.resolve(agents)),
      duplicateAgent: vi.fn().mockImplementation(async (id) => {
        const duplicate = createMockAgent({
          id: "agent-dup",
          name: "Original Agent (Copy)",
        });
        agents = [...agents, duplicate];
        return duplicate;
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Original Agent")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /duplicate/i }));

    expect(client.duplicateAgent).toHaveBeenCalledWith("agent-1");
    expect(await screen.findByText("Original Agent (Copy)")).toBeVisible();
  });

  it("archives an agent", async () => {
    let agents = [createMockAgent({ id: "agent-1", name: "Archivable Agent" })];
    const client = createMockClient({
      getAgents: vi.fn().mockImplementation(() => Promise.resolve(agents)),
      archiveAgent: vi.fn().mockImplementation(async (id) => {
        agents = agents.map((a) => (a.id === id ? { ...a, archived: true } : a));
        return { archived: true };
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Archivable Agent")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /archive/i }));

    expect(client.archiveAgent).toHaveBeenCalledWith("agent-1");
    expect(await screen.findByText(/archived/i)).toBeVisible();
  });
});
