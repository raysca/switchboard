import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ProvidersView } from "./ProvidersView";
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
      { id: "xai", display_name: "xAI", capabilities: ["llm", "stt", "tts"], auth_required: true },
    ]),
    getCredentials: vi.fn().mockResolvedValue([
      {
        id: "cred-1",
        provider: "google",
        masked_value: "••••••••9876",
        validation_state: "unvalidated",
        capabilities: ["llm", "stt", "tts"],
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      },
    ]),
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
  };
}

describe("ProvidersView", () => {
  it("renders prompt when management key is absent", () => {
    const client = createMockClient();
    renderWithProviders(<ProvidersView client={client} keyValue={null} />);
    expect(screen.getByRole("heading", { level: 1, name: /providers/i })).toBeVisible();
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("lists credentials with masked values and status badges", async () => {
    const client = createMockClient();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);

    expect(await screen.findByText("••••••••9876")).toBeVisible();
    expect(screen.getByRole("cell", { name: /Google/i })).toBeVisible();
    expect(screen.getByText("unvalidated")).toBeVisible();
  });

  it("adds a credential with secret masking and clears secret input", async () => {
    let credentials = [
      {
        id: "cred-1",
        provider: "google",
        masked_value: "••••••••9876",
        validation_state: "unvalidated",
        capabilities: ["llm", "stt", "tts"],
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      },
    ];
    const client = createMockClient({
      getCredentials: vi.fn().mockImplementation(() => Promise.resolve(credentials)),
      createCredential: vi.fn().mockImplementation(async ({ provider, secret }) => {
        expect(secret).toBe("xai-super-secret-key-321");
        const newCred = {
          id: "cred-2",
          provider,
          masked_value: "••••••••0321",
          validation_state: "unvalidated",
          capabilities: ["llm"],
          created_at: "2026-10-05T12:30:00Z",
          updated_at: "2026-10-05T12:30:00Z",
        };
        credentials = [...credentials, newCred];
        return newCred;
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);

    expect(await screen.findByText("••••••••9876")).toBeVisible();

    const providerSelect = screen.getByLabelText("Provider service");
    await user.selectOptions(providerSelect, "xai");

    const secretInput = screen.getByLabelText(/secret|api key/i);
    expect(secretInput).toHaveAttribute("type", "password");
    await user.type(secretInput, "xai-super-secret-key-321");

    await user.click(screen.getByRole("button", { name: /add credential/i }));

    expect(client.createCredential).toHaveBeenCalledWith({
      provider: "xai",
      secret: "xai-super-secret-key-321",
    });

    expect(await screen.findByText("••••••••0321")).toBeVisible();
    // Verify input was cleared
    expect(secretInput).toHaveValue("");
    // Plaintext secret is never anywhere in the rendered document
    expect(screen.queryByText("xai-super-secret-key-321")).toBeNull();
  });

  it("validates credential and updates status badge", async () => {
    let credState = "unvalidated";
    const client = createMockClient({
      getCredentials: vi.fn().mockImplementation(() =>
        Promise.resolve([
          {
            id: "cred-1",
            provider: "google",
            masked_value: "••••••••9876",
            validation_state: credState,
            capabilities: ["llm", "stt", "tts"],
            created_at: "2026-10-05T12:00:00Z",
            updated_at: "2026-10-05T12:00:00Z",
          },
        ]),
      ),
      validateCredential: vi.fn().mockImplementation(async (id) => {
        expect(id).toBe("cred-1");
        credState = "valid";
        return {
          id: "cred-1",
          provider: "google",
          masked_value: "••••••••9876",
          validation_state: "valid",
          validated_at: "2026-10-05T12:35:00Z",
          capabilities: ["llm", "stt", "tts"],
          created_at: "2026-10-05T12:00:00Z",
          updated_at: "2026-10-05T12:35:00Z",
        };
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);

    expect(await screen.findByText("unvalidated")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /validate/i }));

    expect(client.validateCredential).toHaveBeenCalledWith("cred-1");
    expect(await screen.findByText("valid")).toBeVisible();
  });

  it("displays error banner with request ID when credential validation fails", async () => {
    const client = createMockClient({
      validateCredential: vi.fn().mockRejectedValue(
        new ApiError("Provider validation rejected key", "validation_failed", 400, "req-val-fail-88"),
      ),
    });

    const user = userEvent.setup();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);

    expect(await screen.findByRole("button", { name: /validate/i })).toBeVisible();
    await user.click(screen.getByRole("button", { name: /validate/i }));

    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByText("Provider validation rejected key")).toBeVisible();
    expect(screen.getByText(/req-val-fail-88/)).toBeVisible();
  });

  it("tests capability discovery and displays models and voices", async () => {
    const client = createMockClient({
      getProviderDiscovery: vi.fn().mockResolvedValue({
        models: [
          { id: "gemini-2.0-flash", display_name: "Gemini 2.0 Flash" },
          { id: "gemini-1.5-pro", display_name: "Gemini 1.5 Pro" },
        ],
        voices: [],
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);

    expect(await screen.findByRole("heading", { name: /test capability discovery/i })).toBeVisible();

    const providerSelect = screen.getByLabelText("Discovery provider");
    await user.selectOptions(providerSelect, "google");

    const capabilitySelect = screen.getByLabelText("Discovery capability");
    await user.selectOptions(capabilitySelect, "llm");

    await user.click(screen.getByRole("button", { name: /discover/i }));

    expect(client.getProviderDiscovery).toHaveBeenCalledWith("google", "llm");
    expect(await screen.findByText("Gemini 2.0 Flash")).toBeVisible();
    expect(screen.getByText("gemini-2.0-flash")).toBeVisible();
    expect(screen.getByText("Gemini 1.5 Pro")).toBeVisible();
  });

  it("deletes a credential and removes it from the list", async () => {
    let credentials = [
      {
        id: "cred-1",
        provider: "google",
        masked_value: "••••••••9876",
        validation_state: "unvalidated",
        capabilities: ["llm", "stt", "tts"],
        created_at: "2026-10-05T12:00:00Z",
        updated_at: "2026-10-05T12:00:00Z",
      },
    ];
    const client = createMockClient({
      getCredentials: vi.fn().mockImplementation(() => Promise.resolve(credentials)),
      deleteCredential: vi.fn().mockImplementation(async (id) => {
        credentials = credentials.filter((c) => c.id !== id);
        return { deleted: true };
      }),
    });

    const user = userEvent.setup();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);

    expect(await screen.findByText("••••••••9876")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /delete/i }));

    expect(client.deleteCredential).toHaveBeenCalledWith("cred-1");
    expect(await screen.findByText(/no provider credentials configured/i)).toBeVisible();
  });
});
