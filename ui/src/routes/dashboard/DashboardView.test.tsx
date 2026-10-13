import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { DashboardView } from "./DashboardView";
import { ApiError, type ApiClient, type OperationsStatus } from "../../api/client";

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

function createMockOperations(overrides: Partial<OperationsStatus> = {}): OperationsStatus {
  return {
    uptime_seconds: 7200,
    started_at: "2026-10-05T10:00:00Z",
    profile: "development",
    active_sessions: 4,
    max_sessions: 50,
    capacity_utilization: 0.08,
    sessions_total: 120,
    turns_total: 480,
    errors_total: 1,
    session_rate: 0.016,
    database: {
      dialect: "sqlite",
      status: "connected",
      open_connections: 1,
      in_use: 0,
      migration_version: 5,
    },
    vad: {
      worker_status: "ready",
      sample_rate: 16000,
      frame_size: 512,
    },
    health: "healthy",
    ...overrides,
  };
}

function createMockClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    getOperationsStatus: vi.fn().mockResolvedValue(createMockOperations()),
    getFlows: vi.fn().mockResolvedValue([
      { id: "agent-1", name: "Support Agent", readiness: { state: "ready" } },
      { id: "agent-2", name: "Sales Agent", readiness: { state: "not_ready" } },
    ]),
    getProviders: vi.fn().mockResolvedValue([
      { id: "google", display_name: "Google Gemini", capabilities: ["llm", "stt", "tts"], detected: true, env: [], services: {} },
      { id: "xai", display_name: "xAI", capabilities: [], detected: false, env: [], services: {} },
    ]),
    getSessions: vi.fn().mockResolvedValue([
      {
        id: "sess-abc-1234",
        status: "active",
        turn_count: 3,
        started_at: new Date(Date.now() - 30000).toISOString(),
      },
    ]),
    ...overrides,
  } as unknown as ApiClient;
}

describe("DashboardView", () => {
  it("renders connect prompt when management key is absent", () => {
    const client = createMockClient();
    renderWithProviders(<DashboardView client={client} keyValue={null} />);
    expect(screen.getByText("Connect to view operations")).toBeVisible();
    expect(screen.getByRole("link", { name: "Start first run" })).toBeVisible();
  });

  it("renders loading state when operational signals are pending", () => {
    const client = createMockClient({
      getOperationsStatus: () => new Promise(() => {}),
    });
    renderWithProviders(<DashboardView client={client} keyValue="test-key" />);
    expect(screen.getByText("Loading operational signals…")).toBeVisible();
  });

  it("renders error state with retry on failure", async () => {
    const retryFn = vi.fn().mockResolvedValue(createMockOperations());
    const client = createMockClient({
      getOperationsStatus: vi
        .fn()
        .mockRejectedValueOnce(new ApiError("Failed to fetch operations", "internal_error", 500, "req-err-1"))
        .mockImplementation(retryFn),
    });

    renderWithProviders(<DashboardView client={client} keyValue="test-key" />);
    expect(await screen.findByText("Failed to fetch operations")).toBeVisible();
    expect(screen.getByText("req-err-1")).toBeVisible();

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(retryFn).toHaveBeenCalled();
  });

  it("renders healthy status banner, metrics grid, readiness cards, and recent sessions", async () => {
    const client = createMockClient();
    renderWithProviders(<DashboardView client={client} keyValue="test-key" />);

    // Health Banner
    expect(await screen.findByText("System healthy. 4 of 50 sessions active.")).toBeVisible();
    expect(screen.getByText("2h 0m")).toBeVisible();
    expect(screen.getByText("All core components and voice runtime pipelines are operating normally.")).toBeVisible();

    // Operational Metrics Grid
    expect(screen.getByText("8.0%")).toBeVisible();
    expect(screen.getByText("120")).toBeVisible();

    // Readiness Summary
    expect(screen.getByRole("link", { name: "View flows" })).toBeVisible();
    expect(await screen.findByText("1 of 2 ready")).toBeVisible();
    expect(screen.getByRole("link", { name: "View providers" })).toBeVisible();
    expect(await screen.findByText("1 of 2 detected")).toBeVisible();

    // Recent Sessions
    expect(screen.getByRole("heading", { name: "Recent calls" })).toBeVisible();
    expect(screen.getByText("sess-abc…")).toBeVisible();
    expect(screen.getByRole("link", { name: "Inspect" })).toHaveAttribute("href", "/sessions/sess-abc-1234");
    expect(screen.getByRole("link", { name: "View all calls" })).toHaveAttribute("href", "/sessions");
  });

  it("renders capacity warning alert box and degraded status when utilization is high", async () => {
    const client = createMockClient({
      getOperationsStatus: vi.fn().mockResolvedValue(
        createMockOperations({
          health: "degraded",
          active_sessions: 48,
          max_sessions: 50,
          capacity_utilization: 0.96,
        }),
      ),
    });

    renderWithProviders(<DashboardView client={client} keyValue="test-key" />);

    expect(await screen.findByText(/System degraded\./)).toBeVisible();
    expect(screen.getByRole("alert", { name: "Capacity warning" })).toBeVisible();
    expect(screen.getByText(/Active sessions are at 96% capacity/)).toBeVisible();
    expect(screen.getByText(/48 of 50 sessions active/)).toBeVisible();
  });

  it("renders empty sessions message when no sessions exist", async () => {
    const client = createMockClient({
      getSessions: vi.fn().mockResolvedValue([]),
    });

    renderWithProviders(<DashboardView client={client} keyValue="test-key" />);

    expect(await screen.findByText("No recent calls")).toBeVisible();
    expect(screen.getByText(/No calls recorded yet/)).toBeVisible();
  });
});

describe("DashboardView setup checklist", () => {
  const prov = (detected: boolean) => [{ id: "google", display_name: "Google", capabilities: [], detected, env: ["GOOGLE_API_KEY"], services: {} }];
  const show = (overrides: Partial<ApiClient>) => renderWithProviders(<DashboardView client={createMockClient(overrides)} keyValue="test-key" />);

  it("shows two steps to a first call when nothing is set up", async () => {
    show({ getProviders: vi.fn().mockResolvedValue(prov(false)), getFlows: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByRole("heading", { name: "Two steps to your first call." })).toBeVisible();
    expect(within(screen.getByRole("list", { name: "Setup checklist" })).getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByRole("link", { name: "See providers" })).toHaveAttribute("href", "/providers");
    expect(screen.getByRole("link", { name: "Start first run" })).toHaveAttribute("href", "/onboarding");
  });

  it("says one step when a provider is already detected", async () => {
    show({ getProviders: vi.fn().mockResolvedValue(prov(true)), getFlows: vi.fn().mockResolvedValue([]) });
    expect(await screen.findByRole("heading", { name: "One step to your first call." })).toBeVisible();
    expect(screen.getByText("Google is ready.")).toBeVisible();
  });

  it("shows no checklist once a provider is detected and a flow exists", async () => {
    show({});
    await screen.findByRole("heading", { name: /Recent calls/ });
    expect(screen.queryByRole("heading", { name: /to your first call/ })).toBeNull();
  });

  it("does not claim anything is missing when the provider list failed", async () => {
    show({ getProviders: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")), getFlows: vi.fn().mockResolvedValue([]) });
    await screen.findByRole("heading", { name: /Recent calls/ });
    expect(screen.queryByRole("heading", { name: /to your first call/ })).toBeNull();
  });
});
