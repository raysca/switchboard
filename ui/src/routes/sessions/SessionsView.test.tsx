import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { SessionsView } from "./SessionsView";
import type { ApiClient, SessionDTO } from "../../api/client";

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

function createMockSession(overrides: Partial<SessionDTO> = {}): SessionDTO {
  return {
    id: "sess-uuid-1",
    agent_id: "agent-1",
    agent_version_id: "ver-1",
    config_hash: "hash123456789",
    started_at: "2026-10-05T12:00:00Z",
    ended_at: "2026-10-05T12:01:30Z",
    status: "completed",
    turn_count: 4,
    safe_summary: "Customer inquiry",
    ...overrides,
  };
}

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    getSessions: vi.fn().mockResolvedValue([createMockSession()]),
    getAgents: vi.fn().mockResolvedValue([
      { id: "agent-1", name: "Support Agent" },
    ]),
    ...overrides,
  } as ApiClient;
}

describe("SessionsView", () => {
  it("renders prompt when management key is absent", () => {
    const client = createMockClient();
    renderWithProviders(<SessionsView client={client} keyValue={null} />);
    expect(screen.getByText("Connect to inspect sessions")).toBeVisible();
  });

  it("renders paginated session table with status, agent name, turns, and duration", async () => {
    const client = createMockClient({
      getSessions: vi.fn().mockResolvedValue([
        createMockSession({
          id: "sess-alpha-1234",
          agent_id: "agent-1",
          status: "completed",
          turn_count: 5,
        }),
      ]),
    });

    renderWithProviders(<SessionsView client={client} keyValue="test-key" />);

    expect(await screen.findByText(/Support Agent/)).toBeVisible();
    expect(screen.getByText("completed")).toBeVisible();
    expect(screen.getByText("5")).toBeVisible();
    expect(screen.getByRole("link", { name: "Inspect Trace" })).toBeVisible();
  });

  it("handles filters for status and agent ID", async () => {
    const user = userEvent.setup();
    const getSessions = vi.fn().mockResolvedValue([createMockSession()]);
    const client = createMockClient({ getSessions });

    renderWithProviders(<SessionsView client={client} keyValue="test-key" />);

    await screen.findByText(/Support Agent/);

    const statusSelect = screen.getByLabelText("Status");
    await user.selectOptions(statusSelect, "completed");

    expect(getSessions).toHaveBeenCalledWith(
      expect.objectContaining({ status: "completed", offset: 0 }),
    );

    const agentInput = screen.getByLabelText(/Filter by Agent ID/i);
    await user.type(agentInput, "agent-2");

    expect(getSessions).toHaveBeenCalledWith(
      expect.objectContaining({ agent_id: "agent-2", offset: 0 }),
    );
  });

  it("supports pagination controls with Next and Previous", async () => {
    const user = userEvent.setup();
    // Return 10 sessions to enable the Next button
    const sessions = Array.from({ length: 10 }, (_, i) =>
      createMockSession({ id: `sess-${i + 1}` }),
    );
    const getSessions = vi.fn().mockResolvedValue(sessions);
    const client = createMockClient({ getSessions });

    renderWithProviders(<SessionsView client={client} keyValue="test-key" />);

    await screen.findByText("Showing 1–10 sessions");

    const prevButton = screen.getByRole("button", { name: "Previous" });
    const nextButton = screen.getByRole("button", { name: "Next" });

    expect(prevButton).toBeDisabled();
    expect(nextButton).not.toBeDisabled();

    await user.click(nextButton);

    expect(getSessions).toHaveBeenCalledWith(
      expect.objectContaining({ offset: 10, limit: 10 }),
    );
  });

  it("renders empty state when no sessions are found", async () => {
    const client = createMockClient({
      getSessions: vi.fn().mockResolvedValue([]),
    });

    renderWithProviders(<SessionsView client={client} keyValue="test-key" />);

    expect(await screen.findByText("No sessions found")).toBeVisible();
  });
});
