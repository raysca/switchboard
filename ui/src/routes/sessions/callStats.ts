import { routeSpan, spanAttrs, type FlowDTO, type SessionDTO, type SessionTracesDTO, type TurnDTO } from "../../api/client";

/** The call's state in words. The server writes only active, draining and ended; anything else is shown as it is. */
export function outcome(s: SessionDTO): { label: string; live: boolean } {
  switch (s.status) {
    case "active":
      return { label: "Live", live: true };
    case "draining":
      return { label: "Ending", live: false };
    case "ended":
      return { label: "Ended", live: false };
    default:
      return { label: s.status, live: false };
  }
}

/** The API has no version list (spec S2): a version's number is known only while it is the flow's current one. */
export function versionLabel(s: SessionDTO, flow: FlowDTO | undefined): string {
  const current = flow?.current_version;
  return current && current.id === s.flow_version_id ? `v${current.number}` : "earlier version";
}

export function mmss(seconds: number): string {
  const total = Math.max(0, Math.round(seconds));
  const h = Math.floor(total / 3600);
  const m = String(Math.floor((total % 3600) / 60)).padStart(2, "0");
  const s = String(total % 60).padStart(2, "0");
  return h > 0 ? `${h}:${m}:${s}` : `${m}:${s}`;
}

/** Elapsed call time. `now` only matters while the call has not ended. */
export function clock(startedAt: string, endedAt: string | null | undefined, now: number): string {
  const end = endedAt ? Date.parse(endedAt) : now;
  return mmss((end - Date.parse(startedAt)) / 1000);
}

export type Steps = { names: Map<string, string>; start: string | undefined };

/**
 * Like `steps`, but a call on an earlier version does not inherit the current start step: it may have
 * been a different step then, so such a call without routes is labelled with the flow's name instead.
 */
export function stepsFor(s: SessionDTO, flow: FlowDTO | undefined): Steps {
  const st = steps(flow);
  return flow?.current_version?.id === s.flow_version_id ? st : { names: st.names, start: undefined };
}

// The server records its own prompts as turns: after a route that happens while the call is idle, and
// when the caller has been silent. They are not the caller's words.
const SYSTEM_TURN = /^\[(async tool result|idle notice)/;

/** Step names and the start step, from the flow's current version. Older calls may name steps that are gone. */
export function steps(flow: FlowDTO | undefined): Steps {
  const cfg = flow?.current_version?.config as { start?: string; steps?: { id: string; name?: string }[] } | undefined;
  const names = new Map<string, string>();
  for (const n of cfg?.steps ?? []) names.set(n.id, n.name?.trim() || n.id);
  return { names, start: cfg?.start };
}

type Route = { at: number; from: string; to: string };

function spansNamed(traces: SessionTracesDTO | null | undefined, event: string): Route[] {
  return (traces?.spans ?? [])
    .flatMap((s) => {
      const r = routeSpan(s);
      return r?.event === event && r.to !== "" ? [{ at: Date.parse(s.started_at), from: r.from, to: r.to }] : [];
    })
    .sort((a, b) => a.at - b.at);
}

export function failedRoutes(traces: SessionTracesDTO | null | undefined): { from: string; to: string }[] {
  return spansNamed(traces, "failed").map(({ from, to }) => ({ from, to }));
}

export function path(traces: SessionTracesDTO | null | undefined, st: Steps, fallback: string): string[] {
  const label = (id: string) => st.names.get(id) ?? id;
  const routes = spansNamed(traces, "active");
  if (routes.length === 0) return [st.start ? label(st.start) : fallback];
  return [label(routes[0].from), ...routes.map((r) => label(r.to))];
}

export type CallLine =
  | { kind: "say"; who: "caller" | "step"; label: string; at: string; text: string; interrupted: boolean }
  | { kind: "route"; at: string; from: string; to: string };

/**
 * The words of a call, in order, each reply labelled with the step that was active when it was said.
 * A turn row does not name its step, so this uses the times of the route spans.
 */
export function transcript(session: SessionDTO, turns: TurnDTO[], traces: SessionTracesDTO | null | undefined, st: Steps, fallback: string): CallLine[] {
  const start = Date.parse(session.started_at);
  const at = (t: number) => mmss((t - start) / 1000);
  const label = (id: string) => st.names.get(id) ?? id;
  const routes = spansNamed(traces, "active");
  const activeAt = (t: number) => {
    let id = routes.length > 0 ? routes[0].from : (st.start ?? "");
    for (const r of routes) if (r.at <= t) id = r.to;
    return id ? label(id) : fallback;
  };

  const out: CallLine[] = [];
  let next = 0;
  const flush = (until: number) => {
    while (next < routes.length && routes[next].at <= until) {
      const r = routes[next++];
      out.push({ kind: "route", at: at(r.at), from: label(r.from), to: label(r.to) });
    }
  };

  const ordered = [...turns].sort((a, b) => a.ordinal - b.ordinal || Date.parse(a.started_at) - Date.parse(b.started_at));
  for (const t of ordered) {
    const spoken = t.transcript.trim();
    const said = SYSTEM_TURN.test(spoken) ? "" : spoken;
    const reply = t.assistant_text.trim();
    const began = Date.parse(t.started_at);
    const ended = t.ended_at ? Date.parse(t.ended_at) : began;
    if (said) {
      flush(began);
      out.push({ kind: "say", who: "caller", label: "Caller", at: at(began), text: said, interrupted: false });
    }
    if (reply) {
      flush(ended);
      out.push({ kind: "say", who: "step", label: activeAt(ended), at: at(ended), text: reply, interrupted: t.interrupted });
    }
  }
  flush(Infinity);
  return out;
}

export type TurnRow = { id: string; at: string; text: string; notes: string[]; latencies: unknown };

/** One row per turn: when, what the assistant said, and what happened in it (tool calls, being cut off). */
export function turnRows(session: SessionDTO, turns: TurnDTO[], traces: SessionTracesDTO | null | undefined): TurnRow[] {
  const start = Date.parse(session.started_at);
  const ordered = [...turns].sort((a, b) => a.ordinal - b.ordinal || Date.parse(a.started_at) - Date.parse(b.started_at));
  return ordered.map((t) => {
    const spoken = t.transcript.trim();
    const notes: string[] = [];
    const reply = replyMs(t);
    if (reply !== null) notes.push(`Replied in ${formatMs(reply)}`);
    if (t.interrupted) notes.push("Interrupted by the caller");
    for (const c of traces?.tool_calls ?? []) {
      if (c.turn_id !== t.id) continue;
      if (c.status === "failed" || c.status === "error") notes.push(`Tool ${c.remote_name} failed`);
      else if (c.ended_at) notes.push(`Tool ${c.remote_name} · ${Math.max(0, Date.parse(c.ended_at) - Date.parse(c.started_at))} ms`);
      else notes.push(`Tool ${c.remote_name} running`);
    }
    return {
      id: t.id,
      at: mmss((Date.parse(t.started_at) - start) / 1000),
      text: t.assistant_text.trim() || (SYSTEM_TURN.test(spoken) ? "" : spoken) || "No words recorded",
      notes,
      latencies: t.latencies,
    };
  });
}

const ms = (v: unknown): number | null => (typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : null);

/**
 * How long the caller waited for the reply: the end of their speech to the first sound, or for a typed turn
 * (which has no end of speech) the model's first token plus the voice's first audio. Null when not measured.
 */
export function replyMs(t: TurnDTO): number | null {
  const l = t.latencies;
  if (!l || typeof l !== "object") return null;
  const o = l as Record<string, unknown>;
  const total = ms(o.voice_to_voice_ms);
  if (total !== null) return total;
  const llm = ms(o.llm_ttft_ms);
  const tts = ms(o.tts_ttfb_ms);
  return llm !== null && tts !== null ? llm + tts : null;
}

export function medianReplyMs(turns: TurnDTO[]): number | null {
  const all = turns.map(replyMs).filter((v): v is number => v !== null).sort((a, b) => a - b);
  if (all.length === 0) return null;
  const mid = Math.floor(all.length / 2);
  return all.length % 2 ? all[mid] : (all[mid - 1] + all[mid]) / 2;
}

export function formatMs(v: number): string {
  return v < 1000 ? `${Math.round(v)} ms` : `${(v / 1000).toFixed(1)} s`;
}

export function interruptedCount(turns: TurnDTO[]): number {
  return turns.filter((t) => t.interrupted).length;
}

/** The step whose end_call tool ended the call, from the call.end span; null for any other ending. */
export function endedByFlow(traces: SessionTracesDTO | null | undefined): { step: string } | null {
  for (const s of traces?.spans ?? []) {
    const a = spanAttrs(s);
    if (s.name === "call.end" && a.reason === "ended_by_flow" && typeof a.from_step === "string") return { step: a.from_step };
  }
  return null;
}

/** Who the call was: the label the integration chose. The console's own test calls say so. */
export function callerLabel(caller: string): string {
  if (!caller) return "Not recorded";
  return caller.startsWith("flow-test-") ? "Console test" : caller;
}

const END_WORDS: Record<string, string> = {
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

/** Why a call ended, in words. An empty or unknown reason (an older call) is just "Ended". */
export function endReasonText(reason: string, step?: string): string {
  const text = END_WORDS[reason] ?? "Ended";
  return reason === "ended_by_flow" && step ? `${text} (${step})` : text;
}

export type Stage = { label: "Heard" | "Model" | "Voice"; start: number; end: number };

/**
 * Where a reply's time went, as windows from the moment the caller finished (or, for a typed turn, from the
 * model request): Heard, Model, Voice. Needs the offsets the server stores with the stage times; null for an
 * older row (the pills show those) and when fewer than two stages can be placed. Never a negative or reversed window.
 */
export function parseStages(latencies: unknown): { stages: Stage[]; total: number } | null {
  if (!latencies || typeof latencies !== "object") return null;
  const o = latencies as Record<string, unknown>;
  const stages: Stage[] = [];
  const heard = ms(o.stt_final_ms);
  if (heard !== null) stages.push({ label: "Heard", start: 0, end: heard });
  const place = (label: Stage["label"], start: unknown, took: unknown) => {
    const a = ms(start);
    const d = ms(took);
    if (a !== null && d !== null) stages.push({ label, start: a, end: a + d });
  };
  place("Model", o.llm_start_ms, o.llm_ttft_ms);
  place("Voice", o.tts_start_ms, o.tts_ttfb_ms);
  if (stages.length < 2) return null; // one bar is just the pills' figure
  const total = Math.max(...stages.map((s) => s.end), ms(o.voice_to_voice_ms) ?? 0);
  return { stages, total };
}
