import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { SessionDetailView } from "./SessionDetailView";
import type { ApiClient, SessionDTO, TurnDTO, SessionTracesDTO } from "../../api/client";

function renderWithRoute(ui: React.ReactElement, initialPath: string = "/sessions/sess-123") {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[initialPath]}>
        <Routes>
          <Route path="/sessions/:sessionId" element={ui} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function createMockSession(overrides: Partial<SessionDTO> = {}): SessionDTO {
  return {
    id: "sess-123",
    agent_id: "agent-alpha",
    agent_version_id: "ver-456",
    config_hash: "hashabcdef123456",
    started_at: "2026-10-05T12:00:00Z",
    ended_at: "2026-10-05T12:02:00Z",
    status: "completed",
    turn_count: 2,
    safe_summary: "Inquiry about billing",
    runtime_tools: [
      {
        mcp_server_id: "mcp-billing",
        remote_name: "get_invoice",
        schema_hash: "sh-inv-1",
        required: true,
      },
    ],
    ...overrides,
  };
}

function createMockTurns(): TurnDTO[] {
  return [
    {
      id: "turn-1",
      session_id: "sess-123",
      ordinal: 1,
      started_at: "2026-10-05T12:00:00Z",
      transcript: "Can you check my invoice?",
      assistant_text: "Checking your latest invoice.",
      interrupted: false,
    },
  ];
}

function createMockTraces(): SessionTracesDTO {
  return {
    session_id: "sess-123",
    spans: [],
    events: [],
    tool_calls: [],
  };
}

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    getSession: vi.fn().mockResolvedValue(createMockSession()),
    getSessionTurns: vi.fn().mockResolvedValue(createMockTurns()),
    getSessionTraces: vi.fn().mockResolvedValue(createMockTraces()),
    ...overrides,
  } as ApiClient;
}

describe("SessionDetailView", () => {
  it("renders prompt when key is absent", () => {
    const client = createMockClient();
    renderWithRoute(<SessionDetailView client={client} keyValue={null} />);
    expect(screen.getByText("Connect to inspect session")).toBeVisible();
  });

  it("renders session metadata, agent version reference, and config hash", async () => {
    const client = createMockClient();
    renderWithRoute(<SessionDetailView client={client} keyValue="test-key" />);

    expect(await screen.findByText("agent-alpha")).toBeVisible();
    expect(screen.getByText("ver-456")).toBeVisible();
    expect(screen.getByText(/hashabcdef123456/)).toBeVisible();
    expect(screen.getByText("Inquiry about billing")).toBeVisible();
    expect(screen.getByText(/get_invoice/)).toBeVisible();
    expect(screen.getByText("Required")).toBeVisible();
  });

  it("renders conversational turns timeline in detail view", async () => {
    const client = createMockClient();
    renderWithRoute(<SessionDetailView client={client} keyValue="test-key" />);

    expect(await screen.findByText("Can you check my invoice?")).toBeVisible();
    expect(screen.getByText("Checking your latest invoice.")).toBeVisible();
  });
});
