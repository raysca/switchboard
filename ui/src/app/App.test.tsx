import { describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { App } from "./App";
import { QueryClient } from "@tanstack/react-query";

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
    render(<App initialPath="/agents" />);
    const strip = screen.getByRole("region", { name: "Management access" });
    expect(within(strip).getByLabelText("Management key")).toHaveAttribute("type", "password");
    expect(within(strip).getByRole("button", { name: "Connect" })).toBeVisible();
    expect(within(strip).getByText(/The key stays in this tab only, until you sign out or reload\./)).toBeVisible();
    expect(within(strip).getByText("/api/v1")).toBeVisible();
  });

  it("renders the agents list at /agents and an agent at /agents/:id", async () => {
    for (const path of ["/agents", "/agents/abc"]) {
      const { unmount } = render(<App initialPath={path} />);
      const nav = within(screen.getByRole("navigation", { name: "Main navigation" }));
      const agentsLink = await nav.findByRole("link", { name: "Agents" });
      await waitFor(() => expect(agentsLink).toHaveAttribute("aria-current", "page"));
      expect(screen.queryByText("Area not found")).toBeNull();
      unmount();
    }
  });

  it("has no /flows route", () => {
    render(<App initialPath="/flows" />);
    expect(screen.getByText("Area not found")).toBeVisible();
  });

  it("shows all six sections and keeps keyboard navigation visible", async () => {
    render(<App initialPath="/agents" />);
    for (const name of [
      "Dashboard",
      "Agents",
      "Providers",
      "MCP tools",
      "Calls",
      "Server",
    ]) {
      expect(screen.getByRole("link", { name })).toBeVisible();
    }
    expect(screen.getByRole("link", { name: "Agents" })).toHaveAttribute(
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

  it("puts the key entry above the page content, and names every nav link by its text", () => {
    render(<App initialPath="/dashboard" />);
    const field = screen.getByLabelText(/management key/i);
    const heading = screen.getByRole("heading", { name: "Dashboard" });
    // Entry comes first in the document so a demo never starts by scrolling to find it.
    expect(heading.compareDocumentPosition(field) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
    expect(field).toHaveAttribute("type", "password");
    expect(screen.getByRole("link", { name: "MCP tools" }).textContent).toContain("MCP tools");
  });

  async function signedInApp(fetcher: typeof fetch) {
    vi.stubGlobal("fetch", fetcher);
    render(<App initialPath="/dashboard" />);
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/management key/i), "k");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(await screen.findByText("Key in memory")).toBeVisible();
    return user;
  }

  // A server that requires the key: only requests carrying one are answered.
  const keyedServer = () =>
    vi.fn<typeof fetch>(async (_input, init) => {
      const headers = (init?.headers ?? {}) as Record<string, string>;
      return headers.Authorization
        ? new Response(JSON.stringify({ data: operationsStatus }), { status: 200 })
        : new Response("{}", { status: 401 });
    });

  it("shows the key status in the top line once connected", async () => {
    await signedInApp(keyedServer());
    expect(screen.getByText("Key in memory").closest(".topbar")).not.toBeNull();
    expect(screen.queryByLabelText(/management key/i)).toBeNull();
    vi.unstubAllGlobals();
  });

  it("TestSignOutClearsTheKey", async () => {
    const fetcher = keyedServer();
    const user = await signedInApp(fetcher);
    // The cache that held the protected data is emptied: observe the one client the app owns.
    let cache: QueryClient | undefined;
    const clear = QueryClient.prototype.clear;
    const spy = vi.spyOn(QueryClient.prototype, "clear").mockImplementation(function (this: QueryClient) {
      cache = this;
      return clear.call(this);
    });
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(cache).toBeDefined();
    // Disabled observers may re-register empty queries; none may still hold the old data.
    expect(cache!.getQueryCache().getAll().filter((q) => q.state.data !== undefined)).toEqual([]);
    spy.mockRestore();
    expect(screen.getByLabelText(/management key/i)).toBeVisible();
    expect(screen.getByText("No management key")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
    // Protected data is gone with the key, and nothing is asked of the server afterwards.
    expect(screen.queryByText(/System \w+ · up /)).toBeNull();
    const calls = fetcher.mock.calls.length;
    await new Promise((r) => setTimeout(r, 50));
    expect(fetcher.mock.calls.length).toBe(calls);
    vi.unstubAllGlobals();
  });

  it("returns to key entry when the key is rejected", async () => {
    // Every request is refused, including the keyless probe at startup and the call made with the key.
    const fetcher = vi.fn<typeof fetch>(
      async () =>
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

  it("renders the call page for a direct link /sessions/:sessionId", async () => {
    render(<App initialPath="/sessions/test-session-123" />);
    expect(await screen.findByRole("heading", { name: "Connect to see this call" })).toBeVisible();
  });
});

describe("access without pasting a key", () => {
  const seen: { url: string; auth: string | null }[] = [];
  // signedIn: the server accepts keyless dashboard requests (a dev proxy adds the credentials).
  // Otherwise it requires a key: a request is answered only if it carries one.
  const stubFetch = (signedIn: boolean) =>
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const headers = (init?.headers ?? {}) as Record<string, string>;
        seen.push({ url: String(input), auth: headers.Authorization ?? null });
        const allowed = signedIn || Boolean(headers.Authorization);
        return new Response(JSON.stringify({ data: {} }), { status: allowed ? 200 : 401 });
      }),
    );
  const cleanup = () => {
    seen.length = 0;
    vi.unstubAllGlobals();
    window.history.replaceState(null, "", "/");
  };

  it("takes the key from a magic link, uses it, and removes it from the address bar", async () => {
    stubFetch(false);
    window.history.replaceState(null, "", "/dashboard#key=link-key-123");
    try {
      render(<App />);
      expect(screen.queryByRole("region", { name: "Management access" })).toBeNull();
      expect(screen.getByText("Key in memory")).toBeVisible();
      expect(screen.getByRole("button", { name: "Sign out" })).toBeVisible(); // a link key can be dropped too
      await waitFor(() => expect(seen.some((c) => c.auth === "Bearer link-key-123")).toBe(true));
      expect(window.location.hash).toBe("");
      expect(window.location.href).not.toContain("link-key-123");
    } finally {
      cleanup();
    }
  });

  it("never probes for server sign-in in a production build (a keyless request is a failed login to a hosted server)", async () => {
    stubFetch(true);
    vi.stubEnv("PROD", true);
    vi.stubEnv("DEV", false);
    try {
      render(<App initialPath="/dashboard" />);
      expect(screen.getByRole("region", { name: "Management access" })).toBeVisible();
      await new Promise((resolve) => setTimeout(resolve, 150));
      expect(seen.some((c) => c.url.includes("server-config"))).toBe(false);
      expect(screen.getByRole("region", { name: "Management access" })).toBeVisible();
    } finally {
      vi.unstubAllEnvs();
      cleanup();
    }
  });

  it("decodes a magic link key that contains URL metacharacters", async () => {
    stubFetch(false);
    window.history.replaceState(null, "", "/dashboard#key=a%2Bb%26c%3Dd%25e%20f");
    try {
      render(<App />);
      await waitFor(() => expect(seen.some((c) => c.auth === "Bearer a+b&c=d%e f")).toBe(true));
    } finally {
      cleanup();
    }
  });

  it("skips the key form when the server signs the dashboard in", async () => {
    stubFetch(true);
    try {
      render(<App initialPath="/dashboard" />);
      expect(await screen.findByText("Signed in by the server")).toBeVisible();
      expect(screen.queryByRole("region", { name: "Management access" })).toBeNull();
      expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull(); // the proxy's key cannot be dropped
      await waitFor(() => expect(seen.filter((c) => !c.url.includes("server-config")).length).toBeGreaterThan(0));
      expect(seen.every((c) => c.auth === null)).toBe(true);
    } finally {
      cleanup();
    }
  });

  it("still asks for the key when the server does not sign the dashboard in", async () => {
    stubFetch(false);
    try {
      render(<App initialPath="/dashboard" />);
      await waitFor(() => expect(seen.some((c) => c.url.includes("server-config"))).toBe(true));
      expect(screen.getByRole("region", { name: "Management access" })).toBeVisible();
      expect(screen.getByLabelText("Management key")).toBeVisible();
    } finally {
      cleanup();
    }
  });
});
