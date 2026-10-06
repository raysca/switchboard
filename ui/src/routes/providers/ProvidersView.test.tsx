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
    updateCredential: vi.fn(),
    deleteCredential: vi.fn(),
    rotateCredential: vi.fn(),
    getAgents: vi.fn().mockResolvedValue([]),
    createAgent: vi.fn(),
    updateAgent: vi.fn(),
    archiveAgent: vi.fn(),
    duplicateAgent: vi.fn(),
    ...overrides,
  } as ApiClient;
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

  it("explains a blocked delete and links the agents still attached to the credential", async () => {
    const client = createMockClient({
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
      deleteCredential: vi.fn().mockRejectedValue(
        new ApiError("credential is attached to agents", "credential_in_use", 409, "req-1", [
          { agent_id: "agent-1", agent_name: "Billing" },
        ]),
      ),
    });
    const user = userEvent.setup();
    renderWithProviders(<ProvidersView client={client} keyValue="key-1" />);
    expect(await screen.findByText("••••••••9876")).toBeVisible();
    await user.click(screen.getByRole("button", { name: /delete/i }));
    expect(await screen.findByRole("link", { name: "Billing" })).toHaveAttribute("href", "/agents/agent-1");
    expect(screen.getByText("••••••••9876")).toBeVisible();
  });
});
