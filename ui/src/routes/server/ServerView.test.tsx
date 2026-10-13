import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ServerView } from "./ServerView";
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
    uptime_seconds: 3665,
    started_at: "2026-10-05T12:00:00Z",
    profile: "production",
    active_sessions: 10,
    max_sessions: 100,
    capacity_utilization: 0.1,
    sessions_total: 80,
    turns_total: 320,
    errors_total: 0,
    session_rate: 0.02,
    max_call_duration_seconds: 300,
    voice_token_max_lifetime_seconds: 60,
    database: {
      dialect: "postgres",
      status: "connected",
      open_connections: 5,
      in_use: 2,
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
    ...overrides,
  } as unknown as ApiClient;
}

describe("ServerView", () => {
  it("renders connect prompt when management key is absent", () => {
    const client = createMockClient();
    renderWithProviders(<ServerView client={client} keyValue={null} />);
    expect(screen.getByText("Connect to inspect the server")).toBeVisible();
    expect(
      screen.getByText("Enter a management key to load the public configuration projection."),
    ).toBeVisible();
  });

  it("renders loading state when server configuration is pending", () => {
    const client = createMockClient({
      getOperationsStatus: () => new Promise(() => {}),
    });
    renderWithProviders(<ServerView client={client} keyValue="test-key" />);
    expect(screen.getByText("Loading server configuration…")).toBeVisible();
  });

  it("renders error state with retry on failure", async () => {
    const retryFn = vi.fn().mockResolvedValue(createMockOperations());
    const client = createMockClient({
      getOperationsStatus: vi
        .fn()
        .mockRejectedValueOnce(new ApiError("Server diagnostics unavailable", "service_unavailable", 503, "req-diag-1"))
        .mockImplementation(retryFn),
    });

    renderWithProviders(<ServerView client={client} keyValue="test-key" />);
    expect(await screen.findByText("Server diagnostics unavailable")).toBeVisible();
    expect(screen.getByText("req-diag-1")).toBeVisible();

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(retryFn).toHaveBeenCalled();
  });

  it("renders complete read-only diagnostics across all four operational sections", async () => {
    const client = createMockClient();
    renderWithProviders(<ServerView client={client} keyValue="test-key" />);

    // Section 1: System Overview
    expect(await screen.findByText("production")).toBeVisible();
    expect(screen.getByText("1 hours, 1 minutes")).toBeVisible();
    expect(screen.getByText("Configuration available • System healthy")).toBeVisible();

    // Section 2: Operational Limits
    expect(screen.getByText("Maximum concurrent sessions")).toBeVisible();
    expect(screen.getByText("100")).toBeVisible();
    expect(screen.getByText(/10.0% \(10 active \/ 100 max\)/)).toBeVisible();
    expect(screen.getByText("300 seconds")).toBeVisible();
    expect(screen.getByText("60 seconds")).toBeVisible();

    // Section 3: Storage Engine
    expect(screen.getByText("Database dialect")).toBeVisible();
    expect(screen.getByText("postgres")).toBeVisible();
    expect(screen.getByText("connected")).toBeVisible();
    expect(screen.getByText("Version 5")).toBeVisible();
    expect(screen.getByText("5")).toBeVisible(); // open connections
    expect(screen.getByText("2")).toBeVisible(); // in use

    // Section 4: VAD Diagnostics
    expect(screen.getByText("Worker status")).toBeVisible();
    expect(screen.getByText("ready")).toBeVisible();
    expect(screen.getByText("16000 Hz")).toBeVisible();
    expect(screen.getByText("512 samples")).toBeVisible();
    expect(screen.getByText("Ready")).toBeVisible();

    // Read-only protection: no forms or edit inputs
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button", { name: /save|edit|submit/i })).toBeNull();
  });

  it("renders degraded and unhealthy status badges accurately", async () => {
    const client = createMockClient({
      getOperationsStatus: vi.fn().mockResolvedValue(
        createMockOperations({
          health: "unhealthy",
          database: {
            dialect: "postgres",
            status: "error",
            open_connections: 0,
            in_use: 0,
          },
          vad: {
            worker_status: "unavailable",
            sample_rate: 16000,
            frame_size: 512,
          },
        }),
      ),
    });

    renderWithProviders(<ServerView client={client} keyValue="test-key" />);

    expect(await screen.findByText("Configuration available • System unhealthy")).toBeVisible();
    expect(screen.getByText("error")).toBeVisible();
    expect(screen.getByText("unavailable")).toBeVisible();
    expect(screen.getByText("Unavailable")).toBeVisible();
  });
});
