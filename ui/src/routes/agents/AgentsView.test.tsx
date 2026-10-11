import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Link, MemoryRouter, Route, Routes } from "react-router";
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

const v2Config = {
  stt: { provider: "google", model: "chirp_2", language: "en-US", credential_id: "cred-g1" },
  llm: { provider: "google", model: "gemini-2.0-flash", system_prompt: "Be brief.", credential_id: "cred-g1" },
  tts: { provider: "google", voice: "en-US-Journey-F", credential_id: "cred-g1" },
};

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
      schema_version: 2,
      config: v2Config,
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
    issueVoiceToken: vi.fn(),
    getProviders: vi.fn().mockResolvedValue([
      { id: "google", display_name: "Google", capabilities: ["llm", "stt", "tts"], auth_required: true },
      { id: "deepgram", display_name: "Deepgram", capabilities: ["stt"], auth_required: true },
      { id: "mcp", display_name: "MCP", capabilities: ["mcp", "stt", "llm", "tts"], auth_required: true },
    ]),
    getCredentials: vi.fn().mockResolvedValue([
      {
        id: "cred-g1",
        provider: "google",
        masked_value: "••••1111",
        validation_state: "valid",
        capabilities: ["llm", "stt", "tts"],
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      },
      {
        id: "cred-dg",
        provider: "deepgram",
        masked_value: "••••2222",
        validation_state: "valid",
        capabilities: ["stt"],
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      },
    ]),
    createCredential: vi.fn(),
    updateCredential: vi.fn(),
    deleteCredential: vi.fn(),
    validateCredential: vi.fn(),
    rotateCredential: vi.fn(),
    getProviderDiscovery: vi.fn(),
    getAgents: vi.fn().mockResolvedValue([createMockAgent()]),
    createAgent: vi.fn(),
    updateAgent: vi.fn(),
    archiveAgent: vi.fn(),
    duplicateAgent: vi.fn(),
    ...overrides,
  } as ApiClient;
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
    // Providers load asynchronously; MCP is never offered for a voice stage.
    const sttProvider = await screen.findByLabelText("STT provider");
    await screen.findByRole("option", { name: "Deepgram" });
    expect(screen.queryByRole("option", { name: "MCP" })).toBeNull();
    expect(
      Array.from((screen.getByLabelText("LLM provider") as HTMLSelectElement).options).map((o) => o.value),
    ).toEqual(["", "google"]);

    await user.selectOptions(sttProvider, "deepgram");
    await user.type(screen.getByLabelText("STT model"), "nova-3");
    // Credential choices are filtered to the stage's provider.
    const sttCred = screen.getByLabelText("STT credential") as HTMLSelectElement;
    expect(Array.from(sttCred.options).map((o) => o.value)).toEqual(["", "cred-dg"]);
    await user.selectOptions(sttCred, "cred-dg");

    await user.selectOptions(screen.getByLabelText("LLM provider"), "google");
    await user.type(screen.getByLabelText("LLM model"), "gemini-2.0-flash");
    await user.type(screen.getByLabelText("System prompt"), "Be helpful.");
    await user.selectOptions(screen.getByLabelText("LLM credential"), "cred-g1");
    await user.selectOptions(screen.getByLabelText("TTS provider"), "google");
    await user.type(screen.getByLabelText("TTS voice"), "en-US-Journey-F");
    await user.selectOptions(screen.getByLabelText("TTS credential"), "cred-g1");
    await user.type(screen.getByLabelText("Max messages"), "64");

    await user.click(screen.getByRole("button", { name: /save agent/i }));

    expect(client.createAgent).toHaveBeenCalledWith({
      name: "Billing Bot",
      description: "Handles invoice questions",
      enabled: true,
      config: {
        stt: { provider: "deepgram", model: "nova-3", language: "en-US", credential_id: "cred-dg" },
        llm: { provider: "google", model: "gemini-2.0-flash", system_prompt: "Be helpful.", credential_id: "cred-g1" },
        tts: { provider: "google", voice: "en-US-Journey-F", credential_id: "cred-g1" },
        context: { max_messages: 64 },
      },
    });

    expect(await screen.findByText("Billing Bot")).toBeVisible();
  });

  it("edits an agent and publishes the next immutable version with new hash", async () => {
    let current = createMockAgent({
      id: "agent-1",
      name: "Support Bot",
      current_version: {
        id: "ver-1",
        number: 1,
        schema_version: 2,
        config: v2Config,
        config_hash: "hash-v1",
        created_at: "2026-10-05T12:00:00Z",
        mcp_tools: [],
      },
    });

    const client = createMockClient({
      getAgents: vi.fn().mockImplementation(() => Promise.resolve([current])),
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

    // The existing v2 config round-trips unchanged.
    expect(client.updateAgent).toHaveBeenCalledWith(
      "agent-1",
      expect.objectContaining({
        name: "Revised Support Bot",
        config: v2Config,
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

  it("opens the agent named in /agents/:agentId", async () => {
    const client = createMockClient({
      getAgents: vi.fn().mockResolvedValue([createMockAgent({ id: "agent-7", name: "Linked Agent" })]),
    });
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter initialEntries={["/agents/agent-7"]}>
          <Routes>
            <Route path="/agents/:agentId" element={<AgentsView client={client} keyValue="key-1" />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("heading", { name: /linked agent/i })).toBeVisible();
  });

  it("follows the URL: back to the list, and an unknown id falls back to it", async () => {
    const client = createMockClient({
      getAgents: vi.fn().mockResolvedValue([createMockAgent({ id: "agent-7", name: "Linked Agent" })]),
    });
    const view = <AgentsView client={client} keyValue="key-1" />;
    render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter initialEntries={["/agents/agent-7"]}>
          <Link to="/agents">All agents</Link>
          <Link to="/agents/nope">Missing agent</Link>
          <Routes>
            <Route path="/agents" element={view} />
            <Route path="/agents/:agentId" element={view} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    const user = userEvent.setup();
    expect(await screen.findByRole("heading", { name: /linked agent/i })).toBeVisible();
    await user.click(screen.getByRole("link", { name: "All agents" }));
    expect(await screen.findByRole("button", { name: "+ Create agent" })).toBeVisible();
    await user.click(screen.getByRole("link", { name: "Missing agent" }));
    expect(await screen.findByText("That agent was not found.")).toBeVisible();
    expect(screen.getByRole("button", { name: "+ Create agent" })).toBeVisible();

    // Opening an agent from the list keeps the URL; re-clicking "All agents" still returns to the list.
    await user.click(screen.getByRole("link", { name: "All agents" }));
    await user.click(await screen.findByRole("button", { name: "View detail" }));
    expect(await screen.findByRole("heading", { name: /linked agent/i })).toBeVisible();
    await user.click(screen.getByRole("link", { name: "All agents" }));
    expect(await screen.findByRole("button", { name: "+ Create agent" })).toBeVisible();
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

  it("deletes (archives) an agent after confirmation and hides it from the list", async () => {
    let agents = [
      createMockAgent({ id: "agent-1", name: "Doomed Agent" }),
      createMockAgent({ id: "agent-2", name: "Keeper Agent" }),
    ];
    const client = createMockClient({
      getAgents: vi.fn().mockImplementation(() => Promise.resolve(agents)),
      archiveAgent: vi.fn().mockImplementation(async (id) => {
        agents = agents.map((a) => (a.id === id ? { ...a, archived: true } : a));
        return { archived: true };
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Doomed Agent")).toBeVisible();
    await user.click(screen.getAllByRole("button", { name: "View detail" })[0]);
    await user.click(screen.getByRole("button", { name: "Delete agent" }));

    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveTextContent(/history .*kept/i);
    expect(client.archiveAgent).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Confirm delete" }));

    expect(client.archiveAgent).toHaveBeenCalledWith("agent-1");
    expect(await screen.findByRole("heading", { name: /configured agents \(1\)/i })).toBeVisible();
    expect(screen.queryByText("Doomed Agent")).toBeNull();
    expect(screen.getByText("Keeper Agent")).toBeVisible();
  });

  it("cancels agent deletion", async () => {
    const client = createMockClient({
      getAgents: vi.fn().mockResolvedValue([createMockAgent({ id: "agent-1", name: "Safe Agent" })]),
    });
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    await user.click(await screen.findByRole("button", { name: "View detail" }));
    await user.click(screen.getByRole("button", { name: "Delete agent" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(client.archiveAgent).not.toHaveBeenCalled();
  });

  it("blocks save and lists missing required stage fields", async () => {
    const client = createMockClient();
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Customer Support")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /create agent/i }));
    await user.type(screen.getByLabelText(/agent name/i), "Half Done");
    await user.type(screen.getByLabelText("Max tokens"), "-5");
    await user.click(screen.getByRole("button", { name: /save agent/i }));

    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("STT provider is required.");
    expect(alert).toHaveTextContent("LLM model is required.");
    expect(alert).toHaveTextContent("STT credential is required.");
    expect(alert).toHaveTextContent("LLM credential is required.");
    expect(alert).toHaveTextContent("TTS credential is required.");
    // Matches the server: STT model and TTS voice fall back to provider defaults.
    expect(alert).not.toHaveTextContent("STT model is required.");
    expect(alert).not.toHaveTextContent("TTS voice is required.");
    expect(alert).toHaveTextContent("Max tokens must be a positive whole number.");
    expect(client.createAgent).not.toHaveBeenCalled();
  });

  it("flags agents with a pre-v2 config as needing reconfiguration", async () => {
    const legacy = createMockAgent({
      id: "agent-old",
      name: "Legacy Bot",
      current_version: {
        id: "ver-old",
        number: 4,
        schema_version: 1,
        config: { stt: "google", llm: "gemini-2.0-flash", tts: "journey" },
        config_hash: "hash-old",
        created_at: "2026-10-05T12:00:00Z",
        mcp_tools: [],
      },
      readiness: { state: "not_ready", reasons: ["invalid_config"] },
    });
    const client = createMockClient({ getAgents: vi.fn().mockResolvedValue([legacy]) });
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Legacy Bot")).toBeVisible();
    expect(screen.getByText("Needs reconfiguration")).toBeVisible();
    expect(screen.getByText("invalid_config")).toBeVisible();

    await user.click(screen.getByRole("button", { name: /^edit$/i }));
    expect(screen.getByText(/uses an old format/i)).toBeVisible();
    // Stage fields start empty rather than carrying the old flat strings.
    expect(screen.getByLabelText("STT provider")).toHaveValue("");
    expect(screen.getByLabelText("LLM model")).toHaveValue("");
  });

  it("requires a credential even when the provider has exactly one", async () => {
    const client = createMockClient();
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Customer Support")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /create agent/i }));
    await user.type(screen.getByLabelText(/agent name/i), "No Creds");
    await screen.findByRole("option", { name: "Deepgram" });
    for (const cap of ["STT", "LLM", "TTS"]) {
      await user.selectOptions(screen.getByLabelText(`${cap} provider`), "google");
    }
    await user.type(screen.getByLabelText("LLM model"), "gemini-2.0-flash");
    expect(screen.queryByRole("option", { name: /automatic/i })).toBeNull();
    await user.click(screen.getByRole("button", { name: /save agent/i }));

    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("STT credential is required.");
    expect(alert).toHaveTextContent("TTS credential is required.");
    expect(alert).not.toHaveTextContent("provider is required");
    expect(client.createAgent).not.toHaveBeenCalled();
  });

  it("clears the stage credential when its provider changes", async () => {
    const client = createMockClient();
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Customer Support")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /create agent/i }));
    await screen.findByRole("option", { name: "Deepgram" });
    await user.selectOptions(screen.getByLabelText("STT provider"), "google");
    await user.selectOptions(screen.getByLabelText("STT credential"), "cred-g1");
    expect(screen.getByLabelText("STT credential")).toHaveValue("cred-g1");

    await user.selectOptions(screen.getByLabelText("STT provider"), "deepgram");
    const sttCred = screen.getByLabelText("STT credential") as HTMLSelectElement;
    expect(sttCred).toHaveValue("");
    expect(Array.from(sttCred.options).map((o) => o.value)).toEqual(["", "cred-dg"]);
  });

  it("links to Providers when the selected provider has no credentials", async () => {
    const client = createMockClient({
      getCredentials: vi.fn().mockResolvedValue([
        {
          id: "cred-g1",
          provider: "google",
          masked_value: "••••1111",
          validation_state: "valid",
          capabilities: ["llm", "stt", "tts"],
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:00:00Z",
        },
      ]),
    });
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Customer Support")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /create agent/i }));
    await screen.findByRole("option", { name: "Deepgram" });
    expect(screen.queryByText(/no stt credentials/i)).toBeNull();

    await user.selectOptions(screen.getByLabelText("STT provider"), "deepgram");
    expect(screen.getByText(/no stt credentials for this provider/i)).toBeVisible();
    expect(screen.getByRole("link", { name: /providers page/i })).toHaveAttribute("href", "/providers");
  });

  it("flags agents whose stored config lacks a stage credential as needing reconfiguration", async () => {
    const noCred = createMockAgent({
      id: "agent-nocred",
      name: "Implicit Cred Bot",
      current_version: {
        id: "ver-nc",
        number: 2,
        schema_version: 2,
        config: { ...v2Config, tts: { provider: "google", voice: "en-US-Journey-F" } },
        config_hash: "hash-nc",
        created_at: "2026-10-05T12:00:00Z",
        mcp_tools: [],
      },
    });
    const client = createMockClient({ getAgents: vi.fn().mockResolvedValue([noCred]) });
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    expect(await screen.findByText("Implicit Cred Bot")).toBeVisible();
    expect(screen.getByText("Needs reconfiguration")).toBeVisible();
  });

  it("opens the designer to create a graph agent and to edit a stored graph", async () => {
    const graph = createMockAgent({
      id: "agent-graph",
      name: "Two Node Bot",
      current_version: {
        id: "ver-g",
        number: 3,
        schema_version: 1,
        config: {
          entry: "front",
          nodes: [
            { id: "front", name: "Front", config: v2Config },
            { id: "sales", name: "Sales", config: { llm: v2Config.llm, tts: v2Config.tts } },
          ],
          edges: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
        },
        config_hash: "hash-g",
        created_at: "2026-10-05T12:00:00Z",
        mcp_tools: [],
      },
    });
    const client = createMockClient({ getAgents: vi.fn().mockResolvedValue([createMockAgent(), graph]) });
    const user = userEvent.setup();
    renderWithProviders(<AgentsView client={client} keyValue="key-1" />);

    await screen.findByText("Two Node Bot");
    await user.click(screen.getByRole("button", { name: /create graph agent/i }));
    expect(await screen.findByRole("heading", { name: /design a graph agent/i })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Cancel" }));

    const row = screen.getByText("Two Node Bot").closest("tr") as HTMLElement;
    await user.click(within(row).getByRole("button", { name: /edit/i }));
    expect(await screen.findByRole("heading", { name: /design two node bot/i })).toBeVisible();
    expect(screen.getByRole("button", { name: /front → sales/i })).toBeVisible();
  });
});
