import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";

// A complete /operations/status payload, as the server always sends it.
const operationsStatus = {
  uptime_seconds: 60,
  started_at: "2026-10-05T12:00:00Z",
  profile: "production",
  active_sessions: 0,
  max_sessions: 50,
  capacity_utilization: 0,
  sessions_total: 0,
  turns_total: 0,
  errors_total: 0,
  p95_latency_ms: 0,
  max_call_duration_seconds: 300,
  voice_token_max_lifetime_seconds: 60,
  database: { dialect: "sqlite", status: "connected", open_connections: 1, in_use: 0 },
  vad: { worker_status: "ready", sample_rate: 16000, frame_size: 512 },
  health: "healthy",
};

describe("admin shell", () => {
  it("shows all six routes and keeps keyboard navigation visible", async () => {
    render(<App initialPath="/agents" />);
    for (const name of [
      "Dashboard",
      "Agents",
      "Providers",
      "MCP Servers",
      "Sessions",
      "Server",
    ]) {
      expect(screen.getByRole("link", { name })).toBeVisible();
    }
    expect(screen.getByRole("link", { name: "Agents" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("link", { name: "MCP Servers" })).toBeVisible();
    const user = userEvent.setup();
    await user.tab();
    expect(
      screen.getByRole("link", { name: /skip to content/i }),
    ).toHaveFocus();
    await user.click(screen.getByRole("link", { name: "Sessions" }));
    expect(screen.getByRole("heading", { name: "Sessions" })).toBeVisible();
    expect(screen.getByRole("heading", { name: "Sessions" })).toHaveFocus();
  });

  it("asks for management access and never requests microphone access at boot", () => {
    render(<App initialPath="/dashboard" />);
    expect(screen.getByLabelText(/management key/i)).toBeVisible();
    expect(screen.getByText(/The key stays in memory only/i)).toBeVisible();
  });

  it("loads the server projection after entry and returns to key entry on 401", async () => {
    const fetcher = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({ data: operationsStatus }),
          { status: 200 },
        ),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            error: {
              code: "unauthorized",
              message: "Unauthorized",
              request_id: "req-3",
            },
          }),
          { status: 401, headers: { "X-Request-ID": "req-3" } },
        ),
      );
    vi.stubGlobal("fetch", fetcher);
    try {
      const user = userEvent.setup();
      render(<App initialPath="/server" />);
      await user.type(screen.getByLabelText(/management key/i), "first-key");
      await user.click(screen.getByRole("button", { name: "Connect" }));
      expect(await screen.findByText("production")).toBeVisible();
      await user.click(screen.getByRole("button", { name: "Clear key" }));
      await user.type(screen.getByLabelText(/management key/i), "expired-key");
      await user.click(screen.getByRole("button", { name: "Connect" }));
      expect(
        await screen.findByRole("button", { name: "Connect" }),
      ).toBeVisible();
      expect(screen.getByText("No management key")).toBeVisible();
      expect(
        screen.getByText(
          "Management key was rejected. Enter a key to reconnect.",
        ),
      ).toBeVisible();
      expect(screen.getByText("Request ID: req-3")).toBeVisible();
      expect(screen.getByLabelText(/management key/i)).toHaveFocus();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("does not let a late 401 from key A clear successfully loaded key B", async () => {
    let respondToA!: (response: Response) => void;
    const pendingA = new Promise<Response>((resolve) => {
      respondToA = resolve;
    });
    const fetcher = vi
      .fn<typeof fetch>()
      .mockImplementationOnce(async () => pendingA)
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({ data: operationsStatus }),
          { status: 200 },
        ),
      );
    vi.stubGlobal("fetch", fetcher);
    try {
      const user = userEvent.setup();
      render(<App initialPath="/server" />);
      await user.type(screen.getByLabelText(/management key/i), "key-A");
      await user.click(screen.getByRole("button", { name: "Connect" }));
      await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
      await user.click(screen.getByRole("button", { name: "Clear key" }));
      await user.type(screen.getByLabelText(/management key/i), "key-B");
      await user.click(screen.getByRole("button", { name: "Connect" }));
      expect(await screen.findByText("production")).toBeVisible();
      await act(async () => {
        respondToA(
          new Response(
            JSON.stringify({
              error: {
                code: "unauthorized",
                message: "Unauthorized",
                request_id: "req-A",
              },
            }),
            { status: 401, headers: { "X-Request-ID": "req-A" } },
          ),
        );
      });
      expect(screen.getByRole("button", { name: "Clear key" })).toBeVisible();
      expect(screen.getByText("production")).toBeVisible();
      expect(screen.queryByText("Request ID: req-A")).not.toBeInTheDocument();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("renders sessions area for direct session link /sessions/:sessionId", () => {
    render(<App initialPath="/sessions/test-session-123" />);
    expect(screen.getByRole("heading", { name: "Sessions" })).toBeVisible();
  });
});
