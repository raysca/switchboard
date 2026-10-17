import { describe, expect, it } from "vitest";
import type { FlowDTO, SessionDTO, SessionTracesDTO, TraceSpanDTO, TurnDTO } from "../../api/client";
import { callerLabel, clock, endReasonText, endedByFlow, failedRoutes, formatMs, interruptedCount, medianReplyMs, mmss, replyMs, outcome, path, steps, stepsFor, transcript, turnRows, versionLabel } from "./callStats";

const flow = (config: Record<string, unknown> = {}): FlowDTO => ({
  id: "a1",
  name: "Front desk",
  description: "",
  enabled: true,
  archived: false,
  current_version: { id: "v7", number: 7, schema_version: 1, config, config_hash: "h", created_at: "", mcp_tools: [] },
  readiness: { state: "ready", reasons: [] },
  created_at: "",
  updated_at: "",
});
const graph = { start: "r", steps: [{ id: "r", name: "Reception" }, { id: "b", name: "Billing" }], routes: [] };
const session = (over: Partial<SessionDTO> = {}): SessionDTO => ({
  draft: false,
  caller: "",
  end_reason: "",
  id: "s1",
  flow_id: "a1",
  flow_version_id: "v7",
  config_hash: "abcdef123456",
  started_at: "2026-10-12T14:00:00Z",
  ended_at: "2026-10-12T14:04:12Z",
  status: "ended",
  turn_count: 3,
  safe_summary: "",
  ...over,
});
const span = (name: string, at: string, attributes: object): TraceSpanDTO => ({ id: `${name}-${at}`, name, started_at: at, ended_at: at, attributes });
const traces = (...spans: TraceSpanDTO[]): SessionTracesDTO => ({ session_id: "s1", spans, events: [], tool_calls: [] });
const turn = (n: number, started: string, ended: string, said: string, reply: string, interrupted = false): TurnDTO => ({
  id: `t${n}`,
  session_id: "s1",
  ordinal: n,
  started_at: started,
  ended_at: ended,
  transcript: said,
  assistant_text: reply,
  interrupted,
});
const toBilling = span("route.active", "2026-10-12T14:00:41Z", { from_step: "r", to_step: "b" });

describe("outcome", () => {
  it("says Live only for an active call, and shows an unknown status as it is", () => {
    expect(outcome(session({ status: "active" }))).toEqual({ label: "Live", live: true });
    expect(outcome(session({ status: "draining" }))).toEqual({ label: "Ending", live: false });
    expect(outcome(session({ status: "ended" }))).toEqual({ label: "Ended", live: false });
    expect(outcome(session({ status: "weird" }))).toEqual({ label: "weird", live: false });
  });
});

describe("versionLabel", () => {
  it("names the version only when it is the flow's current one", () => {
    expect(versionLabel(session(), flow())).toBe("v7");
    expect(versionLabel(session({ flow_version_id: "v6" }), flow())).toBe("earlier version");
    expect(versionLabel(session(), undefined)).toBe("earlier version");
  });
});

describe("clock", () => {
  it("formats minutes and seconds, and hours when there are some", () => {
    expect(mmss(252)).toBe("04:12");
    expect(mmss(3723)).toBe("1:02:03");
    expect(clock("2026-10-12T14:00:00Z", "2026-10-12T14:04:12Z", 0)).toBe("04:12");
  });
  it("counts up to now while the call is still running, and never goes negative", () => {
    const start = Date.parse("2026-10-12T14:00:00Z");
    expect(clock("2026-10-12T14:00:00Z", null, start + 65_000)).toBe("01:05");
    expect(clock("2026-10-12T14:00:00Z", null, start - 5_000)).toBe("00:00");
  });
});

describe("steps", () => {
  it("reads names and the start step from the current version", () => {
    const st = steps(flow(graph));
    expect(st.names.get("b")).toBe("Billing");
    expect(st.start).toBe("r");
  });
  it("is empty for a flat flow or an unknown one", () => {
    expect(steps(flow({ llm: {} })).names.size).toBe(0);
    expect(steps(undefined).names.size).toBe(0);
  });
});

describe("path", () => {
  const st = steps(flow(graph));
  it("lists the steps in the order the call visited them", () => {
    expect(path(traces(toBilling), st, "Front desk")).toEqual(["Reception", "Billing"]);
  });
  it("is the start step when there was no route, or the fallback when it is unknown", () => {
    expect(path(traces(), st, "Front desk")).toEqual(["Reception"]);
    expect(path(null, steps(undefined), "Front desk")).toEqual(["Front desk"]);
  });
  it("shows a step id that the current version no longer has, never blank", () => {
    const gone = span("route.active", "2026-10-12T14:00:41Z", { from_step: "r", to_step: "zz-deleted" });
    expect(path(traces(gone), st, "Front desk")).toEqual(["Reception", "zz-deleted"]);
  });
});

describe("failedRoutes", () => {
  it("returns the step ids of routes that failed", () => {
    const failed = span("route.failed", "2026-10-12T14:00:30Z", { from_step: "r", to_step: "b" });
    expect(failedRoutes(traces(failed, toBilling))).toEqual([{ from: "r", to: "b" }]);
    expect(failedRoutes(null)).toEqual([]);
  });
});

describe("transcript", () => {
  const st = steps(flow(graph));
  const turns = [
    turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "", "Hi, you're through. How can I help?"),
    turn(2, "2026-10-12T14:00:06Z", "2026-10-12T14:00:09Z", "My bill went up.", "I can look into that."),
    turn(3, "2026-10-12T14:00:43Z", "2026-10-12T14:00:45Z", "Put it back on?", "Let me check", true),
  ];

  it("labels each reply with the step that was active, and marks the route where it happened", () => {
    const lines = transcript(session(), turns, traces(toBilling), st, "Front desk");
    expect(lines).toEqual([
      { kind: "say", who: "step", label: "Reception", at: "00:01", text: "Hi, you're through. How can I help?", interrupted: false },
      { kind: "say", who: "caller", label: "Caller", at: "00:06", text: "My bill went up.", interrupted: false },
      { kind: "say", who: "step", label: "Reception", at: "00:09", text: "I can look into that.", interrupted: false },
      { kind: "route", at: "00:41", from: "Reception", to: "Billing" },
      { kind: "say", who: "caller", label: "Caller", at: "00:43", text: "Put it back on?", interrupted: false },
      { kind: "say", who: "step", label: "Billing", at: "00:45", text: "Let me check", interrupted: true },
    ]);
  });

  it("puts turns in order whatever order they arrive in", () => {
    const lines = transcript(session(), [...turns].reverse(), traces(toBilling), st, "Front desk");
    expect(lines.filter((l) => l.kind === "say").map((l) => (l as { at: string }).at)).toEqual(["00:01", "00:06", "00:09", "00:43", "00:45"]);
  });

  it("falls back to the flow's name when no step is known, and skips lines with no words", () => {
    const lines = transcript(session(), [turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:02Z", "  ", "Hello"), turn(2, "2026-10-12T14:00:05Z", "2026-10-12T14:00:05Z", "", "")], null, steps(undefined), "Front desk");
    expect(lines).toEqual([{ kind: "say", who: "step", label: "Front desk", at: "00:02", text: "Hello", interrupted: false }]);
  });

  it("keeps a route that happened after the last turn", () => {
    const late = span("route.active", "2026-10-12T14:03:00Z", { from_step: "b", to_step: "r" });
    const lines = transcript(session(), turns, traces(toBilling, late), st, "Front desk");
    expect(lines.at(-1)).toEqual({ kind: "route", at: "03:00", from: "Billing", to: "Reception" });
  });
});

describe("interruptedCount", () => {
  it("counts replies the caller cut off", () => {
    expect(interruptedCount([turn(1, "a", "a", "", "x", true), turn(2, "a", "a", "", "y"), turn(3, "a", "a", "", "z", true)])).toBe(2);
  });
});

describe("transcript: words the system put in the caller's mouth", () => {
  const st = steps(flow(graph));
  it("leaves out the internal prompts the server records as caller turns", () => {
    const lines = transcript(
      session(),
      [
        turn(1, "2026-10-12T14:00:42Z", "2026-10-12T14:00:43Z", "[async tool result: transfer_to_billing]", "Hello, this is billing."),
        turn(2, "2026-10-12T14:01:30Z", "2026-10-12T14:01:31Z", "[idle notice; system notice, not spoken by the user] The user has been silent.", "Are you still there?"),
        turn(3, "2026-10-12T14:01:40Z", "2026-10-12T14:01:41Z", "[inaudible] hello", "Yes."),
      ],
      traces(toBilling),
      st,
      "Front desk",
    );
    const callers = lines.filter((l) => l.kind === "say" && l.who === "caller").map((l) => (l as { text: string }).text);
    expect(callers).toEqual(["[inaudible] hello"]);
    expect(lines.filter((l) => l.kind === "say" && l.who === "step")).toHaveLength(3);
  });
});

describe("stepsFor", () => {
  it("does not claim the current start step for a call on an earlier version, but keeps the names", () => {
    const f = flow(graph);
    expect(stepsFor(session(), f).start).toBe("r");
    const old = stepsFor(session({ flow_version_id: "v6" }), f);
    expect(old.start).toBeUndefined();
    expect(old.names.get("b")).toBe("Billing");
    expect(path(traces(), old, "Front desk")).toEqual(["Front desk"]);
  });
});

describe("turnRows", () => {
  const turns = [
    {
      ...turn(2, "2026-10-12T14:00:06Z", "2026-10-12T14:00:09Z", "My bill went up.", "I can look into that."),
      tools: [
        { name: "crm.verify_identity", status: "succeeded", duration_ms: 160 },
        { name: "crm.lookup", status: "failed", duration_ms: 0 },
        { name: "crm.slow", status: "cancelled", duration_ms: 900 },
      ],
    },
    turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "", "Hi there."),
    turn(3, "2026-10-12T14:00:43Z", "2026-10-12T14:00:45Z", "[async tool result: transfer_to_billing]", "Let me check", true),
  ];
  it("lists each turn in order with the reply, and notes what happened in it", () => {
    expect(turnRows(session(), turns)).toEqual([
      { id: "t1", at: "00:01", text: "Hi there.", notes: [], latencies: undefined },
      { id: "t2", at: "00:06", text: "I can look into that.", notes: ["Tool crm.verify_identity · 160 ms", "Tool crm.lookup failed", "Tool crm.slow cancelled"], latencies: undefined },
      { id: "t3", at: "00:43", text: "Let me check", notes: ["Interrupted by the caller"], latencies: undefined },
    ]);
  });
  it("falls back to the caller's words, then to a plain note", () => {
    const rows = turnRows(session(), [turn(1, "2026-10-12T14:00:01Z", "2026-10-12T14:00:01Z", "Hello?", ""), turn(2, "2026-10-12T14:00:05Z", "2026-10-12T14:00:05Z", " ", "")]);
    expect(rows.map((r) => r.text)).toEqual(["Hello?", "No words recorded"]);
  });

  it("finds the step that ended the call", () => {
    const t = traces(span("call.end", "2026-10-12T14:04:12Z", { reason: "ended_by_flow", from_step: "b" }));
    expect(endedByFlow(t)).toEqual({ step: "b" });
    expect(endedByFlow(traces())).toBeNull();
    expect(endedByFlow(null)).toBeNull();
  });
  it("ignores a hangup ending and a span that is not call.end", () => {
    expect(endedByFlow(traces(span("call.end", "2026-10-12T14:04:12Z", { reason: "hangup", from_step: "b" })))).toBeNull();
    expect(endedByFlow(traces(span("route.active", "2026-10-12T14:04:12Z", { reason: "ended_by_flow", from_step: "b" })))).toBeNull();
  });
});

describe("reply time", () => {
  const withLat = (n: number, latencies?: unknown): TurnDTO => ({ ...turn(n, "2026-10-12T14:00:01Z", "2026-10-12T14:00:02Z", "hi", "yo"), latencies });
  it("is the caller-to-first-sound time, or model plus voice for a typed turn, and unknown without either", () => {
    expect(replyMs(withLat(1, { voice_to_voice_ms: 900, llm_ttft_ms: 1, tts_ttfb_ms: 1 }))).toBe(900);
    expect(replyMs(withLat(2, { llm_ttft_ms: 420, tts_ttfb_ms: 300 }))).toBe(720);
    expect(replyMs(withLat(3, { llm_ttft_ms: 420 }))).toBeNull();
    expect(replyMs(withLat(4, {}))).toBeNull();
    expect(replyMs(withLat(5))).toBeNull();
    expect(replyMs(withLat(6, "not an object"))).toBeNull();
  });
  it("has a median over the turns that have one, null when none do", () => {
    expect(medianReplyMs([])).toBeNull();
    expect(medianReplyMs([withLat(1, {}), withLat(2)])).toBeNull();
    expect(medianReplyMs([withLat(1, { voice_to_voice_ms: 900 }), withLat(2, { voice_to_voice_ms: 300 }), withLat(3, {})])).toBe(600);
    expect(medianReplyMs([withLat(1, { voice_to_voice_ms: 100 }), withLat(2, { voice_to_voice_ms: 900 }), withLat(3, { voice_to_voice_ms: 300 })])).toBe(300);
  });
  it("is written in milliseconds under a second and seconds above", () => {
    expect(formatMs(412)).toBe("412 ms");
    expect(formatMs(1250)).toBe("1.3 s");
  });
  it("notes the reply time on the turn row", () => {
    const rows = turnRows(session(), [withLat(1, { voice_to_voice_ms: 900 }), withLat(2, {})]);
    expect(rows[0].notes).toEqual(["Replied in 900 ms"]);
    expect(rows[1].notes).toEqual([]);
  });
});

describe("callerLabel", () => {
  it("shows the console's own test calls as such, anything else as written, and an unrecorded caller plainly", () => {
    expect(callerLabel("flow-test-0192aaaa")).toBe("Console test");
    expect(callerLabel("alice")).toBe("alice");
    expect(callerLabel("")).toBe("Not recorded");
  });
});

describe("endReasonText", () => {
  it("has words for every reason the server records, and names the step for a flow that ended the call", () => {
    const words: Record<string, string> = {
      caller_hung_up: "Caller hung up",
      silent_caller: "Caller went silent",
      agent_stuck: "Agent stopped responding",
      ended_by_flow: "Ended by the flow",
      time_limit: "Reached the time limit",
      connection_lost: "Connection lost",
      server_shutdown: "Server shutting down",
      error: "Ended with an error",
      setup_failed: "Could not start",
      server_restart: "Ended when the server restarted",
    };
    for (const [reason, text] of Object.entries(words)) expect(endReasonText(reason)).toBe(text);
    expect(endReasonText("ended_by_flow", "Billing")).toBe("Ended by the flow (Billing)");
    expect(endReasonText("")).toBe("Ended");
    expect(endReasonText("something_new")).toBe("Ended");
  });
});
