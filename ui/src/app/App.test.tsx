import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
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
  max_call_duration_seconds: 300,
  voice_token_max_lifetime_seconds: 60,
  database: { dialect: "sqlite", status: "connected", open_connections: 1, in_use: 0 },
  vad: { worker_status: "ready", sample_rate: 16000, frame_size: 512 },
  health: "healthy",
};

describe("admin shell", () => {
  it("explains what the management key is for and where it goes", () => {
    render(<App initialPath="/flows" />);
    const strip = screen.getByRole("region", { name: "Management access" });
    expect(within(strip).getByLabelText("Management key")).toHaveAttribute("type", "password");
    expect(within(strip).getByRole("button", { name: "Connect" })).toBeVisible();
    expect(within(strip).getByText(/The key stays in this tab only \(reloading clears it\)/)).toBeVisible();
    expect(within(strip).getByText("/api/v1")).toBeVisible();
  });

  it("keeps old /agents bookmarks working", async () => {
    render(<App initialPath="/agents/abc" />);
    expect(await screen.findByRole("link", { name: "Flows" })).toHaveAttribute("aria-current", "page");
    expect(screen.queryByText("Area not found")).toBeNull();
  });

  it("shows all six routes and keeps keyboard navigation visible", async () => {
    render(<App initialPath="/flows" />);
    for (const name of [
      "Dashboard",
      "Flows",
      "Providers",
      "MCP tools",
      "Calls",
      "Server",
    ]) {
      expect(screen.getByRole("link", { name })).toBeVisible();
    }
    expect(screen.getByRole("link", { name: "Flows" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("link", { name: "MCP tools" })).toBeVisible();
    const user = userEvent.setup();
    await user.tab();
    expect(
      screen.getByRole("link", { name: /skip to content/i }),
    ).toHaveFocus();
    await user.click(screen.getByRole("link", { name: "Calls" }));
    expect(screen.getByRole("heading", { name: "Calls" })).toBeVisible();
    expect(screen.getByRole("heading", { name: "Calls" })).toHaveFocus();
  });

  it("asks for management access and never requests microphone access at boot", () => {
    render(<App initialPath="/dashboard" />);
    expect(screen.getByLabelText(/management key/i)).toBeVisible();
    expect(screen.getByText(/The key stays in this tab only/i)).toBeVisible();
  });

  it("puts the key entry above the page content, and names every route link by its text", () => {
    render(<App initialPath="/dashboard" />);
    const field = screen.getByLabelText(/management key/i);
    const heading = screen.getByRole("heading", { name: "Dashboard" });
    // Entry comes first in the document so a demo never starts by scrolling to find it.
    expect(heading.compareDocumentPosition(field) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
    expect(field).toHaveAttribute("type", "password");
    expect(screen.getByRole("link", { name: "MCP tools" }).textContent).toContain("MCP tools");
  });

  it("shows the key status in the top line once connected, with no way to clear it but reloading", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(new Response(JSON.stringify({ data: operationsStatus }), { status: 200 }));
    vi.stubGlobal("fetch", fetcher);
    render(<App initialPath="/dashboard" />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/management key/i), "k");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(await screen.findByText("Key in memory")).toBeVisible();
    expect(screen.getByText("Key in memory").closest(".topbar")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Clear key" })).toBeNull();
    expect(screen.queryByLabelText(/management key/i)).toBeNull();
    vi.unstubAllGlobals();
  });

  it("returns to key entry when the key is rejected", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(
      new Response(
        JSON.stringify({ error: { code: "unauthorized", message: "Unauthorized", request_id: "req-3" } }),
        { status: 401, headers: { "X-Request-ID": "req-3" } },
      ),
    );
    vi.stubGlobal("fetch", fetcher);
    try {
      const user = userEvent.setup();
      render(<App initialPath="/server" />);
      await user.type(screen.getByLabelText(/management key/i), "expired-key");
      await user.click(screen.getByRole("button", { name: "Connect" }));
      expect(await screen.findByRole("button", { name: "Connect" })).toBeVisible();
      expect(screen.getByText("No management key")).toBeVisible();
      expect(screen.getByText("Management key was rejected. Enter a key to reconnect.")).toBeVisible();
      expect(screen.getByText("req-3")).toBeVisible();
      expect(screen.getByLabelText(/management key/i)).toHaveFocus();
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("renders the call page for a direct link /sessions/:sessionId", () => {
    render(<App initialPath="/sessions/test-session-123" />);
    expect(screen.getByRole("heading", { name: "Connect to see this call" })).toBeVisible();
  });
});
