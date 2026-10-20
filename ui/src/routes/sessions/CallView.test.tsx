import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import type { FlowDTO, ApiClient, SessionDTO, SessionTracesDTO, TraceSpanDTO, TurnDTO } from "../../api/client";
import { ApiError } from "../../api/client";
import { CallView } from "./CallView";

const ID = "0192aaaa-0000-7000-8000-000000000001";
const graph = { start: "r", steps: [{ id: "r", name: "Reception" }, { id: "b", name: "Billing" }], routes: [] };
const flow = (over: Partial<FlowDTO> = {}): FlowDTO => ({
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
  draft: false,
  id: ID,
  flow_id: "a1",
  flow_version_id: "v7",
  config_hash: "abcdef1234567890",
  started_at: "2026-10-12T14:00:00Z",
  ended_at: "2026-10-12T14:04:12Z",
  status: "ended",
  turn_count: 3,
  safe_summary: "Billing question",
  caller: "",
  end_reason: "",
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
const toBilling = span("route.active", "2026-10-12T14:00:41Z", { from_step: "r", to_step: "b" });

function setup(client: Partial<ApiClient>, path = `/sessions/${ID}`, key: string | null = "k") {
  const full = {
    getSession: vi.fn().mockResolvedValue(session()),
    getSessionTurns: vi.fn().mockResolvedValue(turns),
    getSessionTraces: vi.fn().mockResolvedValue(traces(toBilling)),
    getFlows: vi.fn().mockResolvedValue([flow()]),
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
    expect(await screen.findByText("Ended · 04:12")).toBeVisible();
    expect(screen.getByRole("heading", { name: "call_0192aaaa", level: 1 })).toBeVisible();
    expect(screen.getByRole("link", { name: "Calls" })).toHaveAttribute("href", "/sessions");
    expect(within(figure("Flow")).getByRole("link", { name: "Front desk" })).toHaveAttribute("href", "/flows/a1");
    expect(within(figure("Flow")).getByText("v7")).toBeVisible();
    expect(within(figure("Duration")).getByText("04:12")).toBeVisible();
    expect(within(figure("Turns")).getByText("3")).toBeVisible();
    expect(within(figure("Interrupted replies")).getByText("1")).toBeVisible();
    expect(within(figure("Path")).getByText("Reception → Billing")).toBeVisible();
  });

  it("marks a draft test call with a Draft chip", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session({ draft: true })) });
    expect(await screen.findByText("Draft")).toBeVisible();
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
    const failed = span("route.failed", "2026-10-12T14:00:30Z", { from_step: "r", to_step: "b" });
    setup({ getSessionTraces: vi.fn().mockResolvedValue(traces(failed)) });
    expect(await screen.findByRole("note")).toHaveTextContent("A route to Billing failed. The caller stayed with Reception.");
  });

  it("shows step ids the flow no longer has, and calls an old version 'earlier version'", async () => {
    const gone = span("route.active", "2026-10-12T14:00:41Z", { from_step: "r", to_step: "zz-deleted" });
    setup({ getSession: vi.fn().mockResolvedValue(session({ flow_version_id: "v6" })), getSessionTraces: vi.fn().mockResolvedValue(traces(gone)) });
    await screen.findByRole("list", { name: "Transcript" });
    expect(within(figure("Flow")).getByText("earlier version")).toBeVisible();
    expect(within(figure("Path")).getByText("Reception → zz-deleted")).toBeVisible();
    expect(screen.getByRole("list", { name: "Transcript" })).toHaveTextContent("Routed Reception → zz-deleted");
  });

  it("still shows the call when the flow list fails", async () => {
    setup({ getFlows: vi.fn().mockRejectedValue(new ApiError("down", "internal", 500, "r")), getSessionTraces: vi.fn().mockResolvedValue(traces()) });
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

  it("has its heading from the first moment, and in every state, so focus has somewhere to go", async () => {
    setup({ getSession: vi.fn().mockReturnValue(new Promise(() => {})) });
    expect(screen.getByRole("heading", { name: "call_0192aaaa", level: 1 })).toBeVisible();
  });

  it("keeps the heading when the call is not found, and when there is no key", async () => {
    setup({ getSession: vi.fn().mockRejectedValue(new ApiError("session not found", "not_found", 404, "r")) }, "/sessions/nope");
    expect(await screen.findByText("That call was not found.")).toBeVisible();
    expect(screen.getByRole("heading", { name: "call_nope", level: 1 })).toBeVisible();
  });

  it("keeps the clock of a live call counting while nothing else changes", async () => {
    const started = new Date(Date.now() - 65_000).toISOString();
    setup({ getSession: vi.fn().mockResolvedValue(session({ status: "active", ended_at: null, started_at: started })) });
    const first = (await screen.findByText(/^Live · /)).textContent;
    await waitFor(() => expect(screen.getByText(/^Live · /).textContent).not.toBe(first), { timeout: 3000 });
  });

  it("labels a call on an earlier version with the flow's name, not the current start step", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session({ flow_version_id: "v6" })), getSessionTraces: vi.fn().mockResolvedValue(traces()) });
    const list = await screen.findByRole("list", { name: "Transcript" });
    expect(list).toHaveTextContent("Front desk · 00:01");
    expect(list).not.toHaveTextContent("Reception ·");
    expect(within(figure("Path")).getByText("Front desk")).toBeVisible();
  });

  it("lays the page out like the canvas: a figures row, then the turns beside the transcript", async () => {
    setup({});
    await screen.findByRole("list", { name: "Transcript" });
    expect(screen.getByText(/^Started /)).toBeVisible();
    const terms = screen.getAllByRole("term").slice(0, 6).map((t) => t.textContent);
    expect(terms).toEqual(["Flow", "Path", "Duration", "Caller", "Turns", "Interrupted replies"]);
    expect(screen.getByRole("region", { name: "Turns" })).toBeVisible();
    expect(screen.getByRole("complementary", { name: "Transcript" })).toBeVisible();
  });

  it("shows the median reply when turns carry timing, and leaves it out when none do", async () => {
    const timed = turns.map((t, i) => ({ ...t, latencies: { voice_to_voice_ms: [300, 900, 600][i] } }));
    setup({ getSessionTurns: vi.fn().mockResolvedValue(timed) });
    expect(within(await screen.findByText("Median reply").then((e) => e.parentElement!)).getByText("600 ms")).toBeVisible();
  });

  it("has no median reply figure without timing", async () => {
    setup({});
    await screen.findByRole("list", { name: "Transcript" });
    expect(screen.queryByText("Median reply")).toBeNull();
  });

  it("lists the turns with what happened in them", async () => {
    const withTool = { ...turns[1], tools: [{ name: "crm.verify_identity", status: "succeeded", duration_ms: 160 }] };
    setup({ getSessionTurns: vi.fn().mockResolvedValue([turns[0], withTool, turns[2]]) });
    const region = await screen.findByRole("region", { name: "Turns" });
    const items = within(region).getAllByRole("listitem").map((li) => li.textContent);
    expect(items).toEqual([
      "00:01Hi, you're through. How can I help?",
      "00:06I can look into that.Tool crm.verify_identity · 160 ms",
      "00:43Let me checkInterrupted by the caller",
    ]);
    expect(within(region).queryByText(/not recorded yet/)).toBeNull();
  });

  it("lists two identical tool lines on one turn without duplicate React keys", async () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const twice = { ...turns[1], tools: [{ name: "lookup", status: "failed", duration_ms: 0 }, { name: "lookup", status: "failed", duration_ms: 0 }] };
    setup({ getSessionTurns: vi.fn().mockResolvedValue([turns[0], twice, turns[2]]) });
    const region = await screen.findByRole("region", { name: "Turns" });
    expect(within(region).getAllByText("Tool lookup failed")).toHaveLength(2);
    expect(spy.mock.calls.filter((c) => String(c[0]).includes("same key"))).toEqual([]);
    spy.mockRestore();
  });

  it("says when the flow ended the call", async () => {
    setup({ getSessionTraces: vi.fn().mockResolvedValue(traces(toBilling, span("call.end", "2026-10-12T14:04:12Z", { reason: "ended_by_flow", from_step: "b" }))) });
    expect(await screen.findByText("Ended by the flow (Billing)")).toBeVisible();
  });

  it("shows who called and why the call ended, in words", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session({ caller: "flow-test-0192aaaa", end_reason: "silent_caller" })) });
    expect(await screen.findByText("Console test")).toBeVisible();
    expect(screen.getByText("Caller went silent")).toBeVisible();
  });

  it("says an older call's caller was not recorded and leaves the ending as plain Ended", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session()) });
    expect(await screen.findByText("Not recorded")).toBeVisible();
    expect(screen.queryByText("Caller hung up")).toBeNull();
  });

  it("does not claim an ending for a call that is still live", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session({ status: "active", ended_at: null })) });
    await screen.findByText("Not recorded");
    expect(screen.queryByText("Ended by the flow")).toBeNull();
  });

  it("draws a waterfall for a reply that has stage offsets and keeps older replies as they were", async () => {
    const withOffsets = { ...turns[1], latencies: { stt_final_ms: 410, llm_start_ms: 520, llm_ttft_ms: 380, tts_start_ms: 905, tts_ttfb_ms: 260, voice_to_voice_ms: 1165 } };
    const older = { ...turns[2], latencies: { stt_final_ms: 410, llm_ttft_ms: 380, tts_ttfb_ms: 260, voice_to_voice_ms: 1165 } };
    setup({ getSessionTurns: vi.fn().mockResolvedValue([turns[0], withOffsets, older]) });
    const chart = await screen.findByRole("img", { name: "Heard 0 to 410 ms, Model 520 to 900 ms, Voice 905 to 1165 ms" });
    expect(chart).toBeVisible();
    expect(screen.getAllByRole("img", { name: /Heard/ })).toHaveLength(1);
    expect(screen.getAllByText("Replied in 1.2 s").length).toBe(2);
  });

  it("offers the live-call controls only while the call is active", async () => {
    setup({ getSession: vi.fn().mockResolvedValue(session({ status: "active", ended_at: null })) });
    expect(await screen.findByRole("region", { name: "Guide this call" })).toBeVisible();
  });

  it("hides the live-call controls for an ended call", async () => {
    setup({});
    await screen.findByText("Ended · 04:12");
    expect(screen.queryByRole("region", { name: "Guide this call" })).toBeNull();
  });

  it("refetches turns and traces once when the call leaves active", async () => {
    const getSession = vi
      .fn()
      .mockResolvedValueOnce(session({ status: "active", ended_at: null }))
      .mockResolvedValue(session());
    const getSessionTurns = vi.fn().mockResolvedValue(turns);
    const getSessionTraces = vi.fn().mockResolvedValue(traces(toBilling));
    const { user } = setup({ getSession, getSessionTurns, getSessionTraces, runSession: vi.fn().mockRejectedValue(new ApiError("gone", "not_found", 404, "")) });
    await user.click(await screen.findByRole("button", { name: "Ask for a reply" }));
    await screen.findByText("Ended · 04:12");
    await waitFor(() => expect(getSessionTurns).toHaveBeenCalledTimes(2));
    expect(getSessionTraces).toHaveBeenCalledTimes(2);
    await new Promise((r) => setTimeout(r, 50));
    expect(getSessionTurns).toHaveBeenCalledTimes(2);
    expect(getSessionTraces).toHaveBeenCalledTimes(2);
  });
});
