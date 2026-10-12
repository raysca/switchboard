import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { AgentDTO, ApiClient, SessionDTO, SessionTracesDTO, TraceSpanDTO, TurnDTO } from "../../api/client";
import { ApiError } from "../../api/client";
import { CallView } from "./CallView";

const ID = "0192aaaa-0000-7000-8000-000000000001";
const graph = { entry: "r", nodes: [{ id: "r", name: "Reception" }, { id: "b", name: "Billing" }], edges: [] };
const flow = (over: Partial<AgentDTO> = {}): AgentDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v7", number: 7, schema_version: 1, config: graph, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
  ...over,
});
const session = (over: Partial<SessionDTO> = {}): SessionDTO => ({
  id: ID,
  agent_id: "a1",
  agent_version_id: "v7",
  config_hash: "abcdef1234567890",
  started_at: "2026-10-12T14:00:00Z",
  ended_at: "2026-10-12T14:04:12Z",
  status: "ended",
  turn_count: 3,
  safe_summary: "Billing question",
  ...over,
});
const turn = (n: number, started: string, ended: string, said: string, reply: string, interrupted = false): TurnDTO => ({
  id: `t${n}`,
  session_id: ID,
  ordinal: n,
  started_at: started,
  ended_at: ended,
  transcript: said,
  assistant_text: reply,
  interrupted,
});
const turns = [
  turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "", "Hi, you're through. How can I help?"),
  turn(2, "2026-10-12T14:00:06Z", "2026-10-12T14:00:09Z", "My bill went up.", "I can look into that."),
  turn(3, "2026-10-12T14:00:43Z", "2026-10-12T14:00:45Z", "Put it back on?", "Let me check", true),
];
const span = (name: string, at: string, attributes: object): TraceSpanDTO => ({ id: `${name}-${at}`, name, started_at: at, ended_at: at, attributes });
const traces = (...spans: TraceSpanDTO[]): SessionTracesDTO => ({ session_id: ID, spans, events: [], tool_calls: [] });
const toBilling = span("handoff.active", "2026-10-12T14:00:41Z", { from_node: "r", to_node: "b" });

function setup(client: Partial<ApiClient>, path = `/sessions/${ID}`, key: string | null = "k") {
  const full = {
    getSession: vi.fn().mockResolvedValue(session()),
    getSessionTurns: vi.fn().mockResolvedValue(turns),
    getSessionTraces: vi.fn().mockResolvedValue(traces(toBilling)),
    getAgents: vi.fn().mockResolvedValue([flow()]),
    ...client,
  } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/sessions/:sessionId" element={<CallView client={full} keyValue={key} />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}
const figure = (term: string) => screen.getByText(term, { selector: "dt" }).closest("div") as HTMLElement;

describe("CallView", () => {
  it("asks for a management key first", () => {
    setup({}, `/sessions/${ID}`, null);
    expect(screen.getByText(/Enter a management key/)).toBeVisible();
  });

  it("shows who the call was with, which flow and version, and how it went", async () => {
    setup({});
    expect(await screen.findByRole("heading", { name: "Call 0192aaaa", level: 1 })).toBeVisible();
    expect(screen.getByRole("link", { name: "Calls" })).toHaveAttribute("href", "/sessions");
    expect(screen.getByText("Ended · 04:12")).toBeVisible();
    expect(within(figure("Flow")).getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/flows/a1");
    expect(within(figure("Flow")).getByText("v7")).toBeVisible();
    expect(within(figure("Duration")).getByText("04:12")).toBeVisible();
    expect(within(figure("Turns")).getByText("3")).toBeVisible();
    expect(within(figure("Interrupted replies")).getByText("1")).toBeVisible();
    expect(within(figure("Path")).getByText("Reception → Billing")).toBeVisible();
  });

  it("marks a running call Live, with the amber lamp", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session({ status: "active", ended_at: null })) });
    const live = await screen.findByText(/^Live · /);
    expect(live.querySelector('.lamp[data-tone="warn"]')).not.toBeNull();
  });

  it("reads the call as a transcript labelled by step, with the route marked where it happened", async () => {
    setup({});
    const list = await screen.findByRole("list", { name: "Transcript" });
    const items = within(list).getAllByRole("listitem").map((li) => li.textContent);
    expect(items).toEqual([
      "Reception · 00:01Hi, you're through. How can I help?",
      "Caller · 00:06My bill went up.",
      "Reception · 00:09I can look into that.",
      "⇢ Routed Reception → Billing",
      "Caller · 00:43Put it back on?",
      "Billing · 00:45Let me check [interrupted]",
    ]);
  });

  it("says when no words were recorded, instead of empty bubbles", async () => {
    setup({ getSessionTurns: vi.fn().mockResolvedValue([turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:02Z", " ", "")]) });
    expect(await screen.findByText("No words were recorded for this call.")).toBeVisible();
  });

  it("names the failed route and who kept the caller", async () => {
    const failed = span("handoff.failed", "2026-10-12T14:00:30Z", { from_node: "r", to_node: "b" });
    setup({ getSessionTraces: vi.fn().mockResolvedValue(traces(failed)) });
    expect(await screen.findByRole("note")).toHaveTextContent("A route to Billing failed. The caller stayed with Reception.");
  });

  it("shows step ids the flow no longer has, and calls an old version 'earlier version'", async () => {
    const gone = span("handoff.active", "2026-10-12T14:00:41Z", { from_node: "r", to_node: "zz-deleted" });
    setup({ getSession: vi.fn().mockResolvedValue(session({ agent_version_id: "v6" })), getSessionTraces: vi.fn().mockResolvedValue(traces(gone)) });
    await screen.findByRole("list", { name: "Transcript" });
    expect(within(figure("Flow")).getByText("earlier version")).toBeVisible();
    expect(within(figure("Path")).getByText("Reception → zz-deleted")).toBeVisible();
    expect(screen.getByRole("list", { name: "Transcript" })).toHaveTextContent("Routed Reception → zz-deleted");
  });

  it("still shows the call when the flow list fails", async () => {
    setup({ getAgents: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")), getSessionTraces: vi.fn().mockResolvedValue(traces()) });
    expect(await screen.findByRole("list", { name: "Transcript" })).toHaveTextContent("Assistant · 00:01");
    expect(within(figure("Flow")).getByText("Unknown flow")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the transcript, unlabelled by step, when the traces fail", async () => {
    setup({ getSessionTraces: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("list", { name: "Transcript" })).toHaveTextContent("My bill went up.");
    expect(screen.getByRole("note")).toHaveTextContent("Steps and routes could not be loaded");
  });

  it("keeps the raw trace one click away", async () => {
    setup({});
    const summary = await screen.findByText("Technical trace", { selector: "summary" });
    expect(summary.closest("details")).not.toHaveAttribute("open");
    expect(summary.closest("details")).toHaveTextContent("Turn #1");
    expect(summary.closest("details")).toHaveTextContent("abcdef1234567890");
  });

  it("says a call is not found only when the server says so", async () => {
    setup({ getSession: vi.fn().mockRejectedValue(new ApiError("session not found", "not_found", 404, "r")) }, "/sessions/nope");
    expect(await screen.findByText("That call was not found.")).toBeVisible();
    expect(screen.getByRole("link", { name: "Back to calls" })).toHaveAttribute("href", "/sessions");
  });

  it("shows another failure as an error with a retry, never as not found", async () => {
    setup({ getSession: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")) });
    expect(await screen.findByRole("alert")).toHaveTextContent("down");
    expect(screen.queryByText("That call was not found.")).toBeNull();
    expect(screen.getByRole("button", { name: "Try again" })).toBeVisible();
  });
});
