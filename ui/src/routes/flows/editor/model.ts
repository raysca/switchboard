// Pure flow-editing rules for the flow designer. They mirror the server's
// validation (server/flow/flow.go): the server stays the authority, this
// keeps the canvas from offering what it would reject.

import type { ToolSelection } from "../../../api/client";

export const MAX_STEPS = 8;
const MAX_ROUTES = 32;
/** The server counts a greeting in bytes (server/flow/config.go). */
export const MAX_GREETING_BYTES = 500;
export const MAX_SUMMARY_PROMPT_BYTES = 2048;
export const STT_P99_RANGE = [100, 10000] as const;
const DEFAULT_MAX_TOKENS = 4000;
/** The server's limits for a flow's quiet-caller and audio settings (server/flow/call_behavior.go). */
export const IDLE_RANGE = { timeoutMs: [1000, 300000], prompts: [1, 10], textBytes: 200 } as const;
export const TURN_RANGE = { minWords: [1, 10], maxMuteS: [1, 60], speechTimeoutMs: [200, 3000], smartHoldMs: [500, 5000] } as const;
export const AUDIO_RANGE = { gain: [-30, 6], backgroundGain: [-60, 0] } as const;
export const byteLength = (text: string): number => new TextEncoder().encode(text).length;
const MAX_CONDITION = 512;

export type StepConfig = Record<string, unknown>;
export type DraftStep = {
  id: string;
  name: string;
  config: StepConfig;
  tools: string[];
  /** the step may hang up */
  end_call?: boolean;
  position: { x: number; y: number };
};
export type DraftRoute = { id: string; from: string; to: string; condition: string };
export type RouteCue = { name: string; after_ms?: number };
export type FlowDraft = {
  start: string;
  steps: DraftStep[];
  routes: DraftRoute[];
  route_cue?: RouteCue;
};

// Settings that belong to the call, not to one step: only the start step has them.
const CALL_LEVEL = ["stt", "vad_stop_ms", "stt_p99_ms", "recording", "context", "async_tools", "idle", "audio", "turn", "failover"] as const;

/** `tts.clean_text` is call-wide too but lives inside `tts`: the config without it. */
function withoutCleanText(config: StepConfig): StepConfig {
  const tts = asRecord(config.tts);
  if (!("clean_text" in tts)) return config;
  const { clean_text: _drop, ...rest } = tts;
  void _drop;
  return { ...config, tts: rest };
}

export function slug(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "");
}

function newId(): string {
  return Math.random().toString(36).slice(2, 10);
}

const asRecord = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};

export type StepTab = "basics" | "routes" | "tools" | "advanced";
type StepIssue = { tab: StepTab; message: string };

/** A flow with no steps yet: the editor shows "Start with one step." */
export function emptyFlow(): FlowDraft {
  return { start: "", steps: [], routes: [] };
}

/** "New step", then "New step 2", "New step 3"… so step names stay unique. */
export function uniqueName(g: FlowDraft, base: string): string {
  const taken = new Set(g.steps.map((n) => slug(n.name)));
  if (!taken.has(slug(base))) return base;
  for (let i = 2; ; i++) if (!taken.has(slug(`${base} ${i}`))) return `${base} ${i}`;
}

const blank = (v: unknown) => !String(v ?? "").trim();
const nameIn = (g: FlowDraft, id: string) => g.steps.find((n) => n.id === id)?.name.trim() || "Untitled step";

/** What one step still needs before it can take calls, by the panel tab that fixes it. */
export function stepIssues(g: FlowDraft, id: string): StepIssue[] {
  const n = g.steps.find((x) => x.id === id);
  if (!n) return [];
  const issues: StepIssue[] = [];
  if (!slug(n.name)) issues.push({ tab: "basics", message: "Give the step a name." });
  else if (g.steps.some((o) => o.id !== id && slug(o.name) === slug(n.name))) issues.push({ tab: "basics", message: "Another step has this name." });
  const llm = asRecord(n.config.llm);
  if (blank(llm.provider) || blank(llm.model)) issues.push({ tab: "basics", message: "Choose a model." });
  if (blank(asRecord(n.config.tts).provider)) issues.push({ tab: "basics", message: "Choose a voice." });
  if (byteLength(String(n.config.greeting ?? "").trim()) > MAX_GREETING_BYTES) issues.push({ tab: "basics", message: `Greeting is over ${MAX_GREETING_BYTES} bytes.` });
  for (const e of g.routes)
    if (e.from === id && blank(e.condition)) issues.push({ tab: "routes", message: `Give the route to ${nameIn(g, e.to)} a condition.` });
  for (const [cap, what] of [["llm", "model"], ["tts", "voice"], ["stt", "listening"]] as const) {
    const stage = asRecord(n.config[cap]);
    const fallbacks = Array.isArray(stage.fallbacks) ? stage.fallbacks.map(asRecord) : [];
    // The server allows a chain of 3 (the primary and 2 fallbacks) with no repeated entry (server/flow/fallback.go).
    if (fallbacks.length > MAX_FALLBACKS) issues.push({ tab: "advanced", message: `Use at most ${MAX_FALLBACKS} ${what} fallbacks.` });
    const key = (c: Record<string, unknown>) => ["provider", "model", "voice", "language"].map((k) => String(c[k] ?? "")).join("\u0000");
    const keys = [stage, ...fallbacks].map(key);
    if (fallbacks.length && new Set(keys).size < keys.length)
      issues.push({ tab: "advanced", message: `A ${what} fallback repeats the ${what} or another fallback.` });
  }
  const failover = asRecord(n.config.failover).on;
  if (Array.isArray(failover) && failover.some((c) => !FAILOVER_CLASSES.some((k) => k.id === c)))
    issues.push({ tab: "advanced", message: "Failover lists an unknown error class." });
  return issues;
}

/** The server's chain limit, primary excluded. */
export const MAX_FALLBACKS = 2;

/** The error classes server/flow/fallback.go lets a stage fail over on; the first three apply when none is set. */
export const FAILOVER_CLASSES = [
  { id: "timeout", label: "Timeouts" },
  { id: "server_error", label: "Server errors and dropped connections" },
  { id: "circuit_open", label: "The provider's circuit breaker is open" },
  { id: "client_error", label: "Rejected requests (bad request, bad key)" },
] as const;
export const DEFAULT_FAILOVER: readonly string[] = ["timeout", "server_error", "circuit_open"];
const SUMMARIZING = new Set(["google", "echo", "nvidia", "openai", "xai"]);

/** What the server caps, when it is known: a flow may only tighten it. */
type ServerLimits = { maxMuteMs?: number };

/** Problems that belong to the whole flow (header state line and Flow settings). */
export function flowIssues(g: FlowDraft, limits?: ServerLimits): string[] {
  if (g.steps.length === 0) return [];
  const start = g.steps.find((n) => n.id === g.start);
  if (!start) return ["Choose a start step."];
  const out: string[] = [];
  if (blank(asRecord(start.config.stt).provider)) out.push("Choose speech-to-text in Flow settings.");
  const rec = asRecord(start.config.recording);
  if (rec.enabled === true && rec.disclosure_confirmed !== true) out.push("Confirm that callers are told the call is recorded.");
  // Ranges from server/flow/config.go: 0 or unset means the server default.
  const vad = start.config.vad_stop_ms;
  if (typeof vad === "number" && vad !== 0 && (vad < 200 || vad > 2000)) out.push("Wait after the caller stops must be 200 to 2000 ms.");
  const ctx = asRecord(start.config.context);
  if (typeof ctx.max_messages === "number" && (ctx.max_messages < 0 || ctx.max_messages === 1)) out.push("Max messages must be at least 2.");
  if (typeof ctx.max_tokens === "number" && ctx.max_tokens < 0) out.push("Max tokens cannot be negative.");
  const stt99 = start.config.stt_p99_ms;
  if (typeof stt99 === "number" && stt99 !== 0 && (stt99 < STT_P99_RANGE[0] || stt99 > STT_P99_RANGE[1]))
    out.push(`Late-transcript wait must be between ${STT_P99_RANGE[0]} and ${STT_P99_RANGE[1]} ms.`);
  out.push(...callBehaviorIssues(start.config, limits));
  const summary = asRecord(ctx.summarization);
  if (Object.keys(summary).length > 0) {
    const maxTokens = typeof ctx.max_tokens === "number" && ctx.max_tokens > 0 ? ctx.max_tokens : DEFAULT_MAX_TOKENS;
    const trigger = summary.trigger_tokens;
    if ((typeof trigger === "number" && trigger < 0) || (typeof summary.keep_turns === "number" && summary.keep_turns < 0))
      out.push("Summarise-at and Keep the latest turns cannot be negative.");
    else if (typeof trigger === "number" && trigger >= maxTokens) out.push(`Summarise-at must be below Max tokens (${maxTokens}).`);
    if (typeof summary.prompt === "string" && byteLength(summary.prompt) > MAX_SUMMARY_PROMPT_BYTES) out.push(`Summary instructions are over ${MAX_SUMMARY_PROMPT_BYTES} bytes.`);
  }
  if (summary.enabled === true)
    for (const n of g.steps) {
      const provider = String(asRecord(n.config.llm).provider ?? "");
      if (provider && !SUMMARIZING.has(provider))
        out.push(`${nameIn(g, n.id)} uses ${provider}, which cannot summarise older turns. Turn summaries off or change its model.`);
    }
  return out;
}

const outside = (v: unknown, [lo, hi]: readonly [number, number]) => typeof v === "number" && (v < lo || v > hi);

function callBehaviorIssues(config: StepConfig, limits?: ServerLimits): string[] {
  const out: string[] = [];
  const idle = asRecord(config.idle);
  const t = idle.user_timeout_ms;
  if (typeof t === "number" && t !== 0 && outside(t, IDLE_RANGE.timeoutMs)) out.push("Wait for a silent caller must be 1 to 300 seconds, or 0 to never hang up.");
  if (typeof idle.max_prompts === "number" && (!Number.isInteger(idle.max_prompts) || outside(idle.max_prompts, IDLE_RANGE.prompts))) out.push("Ask this many times must be 1 to 10.");
  if (typeof idle.prompt === "string" && byteLength(idle.prompt) > IDLE_RANGE.textBytes) out.push(`What to say is over ${IDLE_RANGE.textBytes} bytes.`);
  if (typeof idle.goodbye === "string" && byteLength(idle.goodbye) > IDLE_RANGE.textBytes) out.push(`Goodbye is over ${IDLE_RANGE.textBytes} bytes.`);
  const turn = asRecord(config.turn);
  const words = turn.min_words;
  if (typeof words === "number" && (!Number.isInteger(words) || words < 0 || words > TURN_RANGE.minWords[1])) out.push(`Interrupt after must be ${TURN_RANGE.minWords[0]} to ${TURN_RANGE.minWords[1]} words.`);
  if (typeof turn.max_mute_ms === "number" && outside(turn.max_mute_ms, [TURN_RANGE.maxMuteS[0] * 1000, TURN_RANGE.maxMuteS[1] * 1000])) out.push(`Longest mute must be ${TURN_RANGE.maxMuteS[0]} to ${TURN_RANGE.maxMuteS[1]} seconds.`);
  if (outside(turn.speech_timeout_ms, TURN_RANGE.speechTimeoutMs)) out.push(`Wait for the caller to resume must be ${TURN_RANGE.speechTimeoutMs[0]} to ${TURN_RANGE.speechTimeoutMs[1]} ms.`);
  if (outside(turn.smart_hold_ms, TURN_RANGE.smartHoldMs)) out.push(`Smart hold must be ${TURN_RANGE.smartHoldMs[0]} to ${TURN_RANGE.smartHoldMs[1]} ms.`);
  if (typeof turn.max_mute_ms === "number" && limits?.maxMuteMs !== undefined && turn.max_mute_ms > limits.maxMuteMs) out.push(`Longest mute is above this server's limit of ${limits.maxMuteMs / 1000} seconds.`);
  const audio = asRecord(config.audio);
  if (outside(audio.output_gain_db, AUDIO_RANGE.gain)) out.push("Voice volume must be -30 to +6 dB.");
  const strength = asRecord(audio.denoise).strength;
  if (typeof strength === "number" && (strength <= 0 || strength > 1)) out.push("Noise reduction strength must be 1 to 100%.");
  if (outside(asRecord(audio.background).gain_db, AUDIO_RANGE.backgroundGain)) out.push("Background volume must be -60 to 0 dB.");
  return out;
}

/** A step no route leads to; callers can never reach it. The start step is always reachable. */
export function isUnreachable(g: FlowDraft, id: string): boolean {
  return id !== g.start && !g.routes.some((e) => e.to === id);
}

/** Names of the steps no path from the start step reaches (a step reached only from an unreachable step counts). */
export function unreachableSteps(g: FlowDraft): string[] {
  const seen = new Set<string>(g.steps.some((n) => n.id === g.start) ? [g.start] : []);
  const queue = [...seen];
  while (queue.length > 0) {
    const id = queue.shift()!;
    for (const e of g.routes) if (e.from === id && !seen.has(e.to)) (seen.add(e.to), queue.push(e.to));
  }
  return g.steps.filter((n) => !seen.has(n.id)).map((n) => n.name || "Untitled step");
}

/** A route drawn on the flow: no condition yet, the step panel asks for one. */
export function addDraftRoute(g: FlowDraft, from: string, to: string): FlowDraft {
  const known = (id: string) => g.steps.some((n) => n.id === id);
  if (from === to || !known(from) || !known(to) || g.routes.length >= MAX_ROUTES) return g;
  if (g.routes.some((e) => e.from === from && e.to === to)) return g;
  return { ...g, routes: [...g.routes, { id: newId(), from, to, condition: "" }] };
}

/**
 * Reads a stored flow config, flat or flow, as a draft. A flat flow exposes
 * every MCP tool selected on its version; as a flow's start step it must list
 * them (selectedTools, "<mcp_server_id>/<remote_name>"), or adding a second
 * step would silently remove its tools.
 */
export function fromFlowConfig(raw: unknown, selectedTools: string[] = []): FlowDraft {
  const cfg = asRecord(raw);
  if (!Array.isArray(cfg.steps)) {
    return {
      start: "main",
      steps: [{ id: "main", name: "main", config: cfg, tools: [...selectedTools], position: { x: 0, y: 0 } }],
      routes: [],
    };
  }
  const steps = cfg.steps.map((n, i): DraftStep => {
    const r = asRecord(n);
    const pos = asRecord(r.position);
    return {
      id: String(r.id ?? ""),
      name: String(r.name ?? ""),
      config: asRecord(r.config),
      tools: Array.isArray(r.tools) ? r.tools.map(String) : [],
      end_call: r.end_call === true,
      position: { x: Number(pos.x ?? i * 280), y: Number(pos.y ?? 0) },
    };
  });
  const routes = (Array.isArray(cfg.routes) ? cfg.routes : []).map((e): DraftRoute => {
    const r = asRecord(e);
    return { id: String(r.id), from: String(r.from), to: String(r.to), condition: String(r.condition ?? "") };
  });
  const cue = asRecord(cfg.route_cue);
  return {
    start: String(cfg.start ?? ""),
    steps,
    routes,
    ...(cue.name ? { route_cue: { name: String(cue.name), ...(cue.after_ms ? { after_ms: Number(cue.after_ms) } : {}) } } : {}),
  };
}

/** A greeting of only spaces says nothing: it is not sent (the field keeps what was typed while editing). */
function withoutBlankGreeting(config: StepConfig): StepConfig {
  if (typeof config.greeting !== "string" || config.greeting.trim() !== "") return config;
  const { greeting: _blank, ...rest } = config;
  void _blank;
  return rest;
}

/** A stored flat flow opens as one step named "main" and goes back flat; a step the user has named needs the flow form. */
function isFlat(g: FlowDraft): boolean {
  const only = g.steps[0];
  return g.steps.length === 1 && g.routes.length === 0 && !g.route_cue && only.id === "main" && only.name === "main";
}

/** The config to send: the flat form for a plain one-step flow, else the flow. */
export function toPayload(g: FlowDraft): unknown {
  // A stored flat flow opens as one step named "main" (fromFlowConfig) and goes back flat, exposing
  // every selected tool. A step the user has named needs the flow form, or its name is lost.
  if (isFlat(g)) return withoutBlankGreeting(g.steps[0].config);
  return {
    start: g.start,
    steps: g.steps.map((n) => {
      let config = withoutBlankGreeting(n.config);
      if (n.id !== g.start) {
        config = { ...withoutCleanText(config) };
        for (const k of CALL_LEVEL) delete config[k];
      }
      return { id: n.id, name: n.name, config, ...(n.tools.length ? { tools: n.tools } : {}), ...(n.end_call && g.steps.length > 1 ? { end_call: true } : {}), position: n.position };
    }),
    routes: g.routes,
    ...(g.route_cue ? { route_cue: g.route_cue } : {}),
  };
}

const toolKey = (t: ToolSelection) => `${t.mcp_server_id}/${t.remote_name}`;

/**
 * The flow's tool selection changed: a tool no longer selected comes off every step that listed it
 * (the server refuses a step tool the flow has not selected), and a flat flow's only step can call every selected tool.
 * A flow in steps form keeps what each step chose; a new tool is not added to any of them.
 */
export function applySelection(g: FlowDraft, next: ToolSelection[]): FlowDraft {
  const keys = next.map(toolKey);
  return {
    ...g,
    steps: g.steps.map((n) => {
      const out = { ...n, tools: isFlat(g) ? [...keys] : n.tools.filter((t) => keys.includes(t)) };
      if (n.id === g.start && Array.isArray(n.config.async_tools)) {
        const { async_tools, ...rest } = n.config;
        const kept = (async_tools as string[]).filter((k) => keys.includes(k));
        out.config = kept.length ? { ...rest, async_tools: kept } : rest;
      }
      return out;
    }),
  };
}

/** A new step starts from the entry's model and voice (without its prompt) so it is valid at once. */
export function insertStep(g: FlowDraft, name: string): FlowDraft {
  if (g.steps.length >= MAX_STEPS) return g;
  if (g.steps.length === 0) {
    const id = newId();
    return { ...g, start: id, steps: [{ id, name, config: {}, tools: [], position: { x: 0, y: 0 } }] };
  }
  const entry = g.steps.find((n) => n.id === g.start);
  const llm = { ...asRecord(entry?.config.llm) };
  delete llm.system_prompt;
  const config: StepConfig = { llm, tts: { ...asRecord(entry?.config.tts) } };
  const last = g.steps[g.steps.length - 1];
  const position = { x: (last?.position.x ?? 0) + 280, y: last?.position.y ?? 0 };
  return { ...g, steps: [...g.steps, { id: newId(), name, config, tools: [], position }] };
}

export function removeStep(g: FlowDraft, id: string): FlowDraft {
  if (id === g.start || !g.steps.some((n) => n.id === id)) return g;
  return {
    ...g,
    steps: g.steps.filter((n) => n.id !== id),
    routes: g.routes.filter((e) => e.from !== id && e.to !== id),
  };
}

export function updateStep(g: FlowDraft, id: string, patch: Partial<Omit<DraftStep, "id">>): FlowDraft {
  return { ...g, steps: g.steps.map((n) => (n.id === id ? { ...n, ...patch } : n)) };
}

export function updateRoute(g: FlowDraft, id: string, condition: string): FlowDraft {
  return { ...g, routes: g.routes.map((e) => (e.id === id ? { ...e, condition } : e)) };
}

export function removeRoute(g: FlowDraft, id: string): FlowDraft {
  return { ...g, routes: g.routes.filter((e) => e.id !== id) };
}

/** Makes a step the entry, moving the call-level settings with it. */
export function setStart(g: FlowDraft, id: string): FlowDraft {
  if (id === g.start || !g.steps.some((n) => n.id === id)) return g;
  const old = g.steps.find((n) => n.id === g.start);
  const moved: StepConfig = {};
  for (const k of CALL_LEVEL) if (old && k in old.config) moved[k] = old.config[k];
  const clean = old ? asRecord(old.config.tts).clean_text : undefined;
  return {
    ...g,
    start: id,
    steps: g.steps.map((n) => {
      if (n.id === g.start) {
        const config = withoutCleanText({ ...n.config });
        for (const k of CALL_LEVEL) delete config[k];
        return { ...n, config };
      }
      if (n.id !== id) return n;
      const config = withoutCleanText({ ...n.config, ...moved });
      return { ...n, config: clean === undefined ? config : { ...config, tts: { ...asRecord(config.tts), clean_text: clean } } };
    }),
  };
}

/** Problems the server would reject, in plain words. Empty means publishable. */
export function validate(g: FlowDraft, limits?: ServerLimits): string[] {
  const errors: string[] = [];
  if (g.steps.length === 0 || g.steps.length > MAX_STEPS) errors.push(`A flow has 1 to ${MAX_STEPS} steps.`);
  if (!g.steps.some((n) => n.id === g.start)) errors.push("Choose a start step.");
  const cue = g.route_cue;
  if (cue && !/^[a-z0-9_-]{1,32}$/.test(cue.name)) errors.push("The route sound name uses lower-case letters, digits, _ and - (up to 32).");
  if (cue?.after_ms !== undefined && (cue.after_ms < 0 || cue.after_ms > 10000)) errors.push("The route sound starts after 0 to 10000 ms.");
  const slugs = new Set<string>();
  for (const n of g.steps) {
    const label = n.name.trim() || n.id;
    const s = slug(n.name);
    if (!s) errors.push(`Step ${label} needs a name with a letter or digit.`);
    else if (slugs.has(s)) errors.push(`Step names must be unique (${label}).`);
    slugs.add(s);
    const llm = asRecord(n.config.llm);
    const tts = asRecord(n.config.tts);
    const need = (ok: unknown, what: string) => {
      if (!String(ok ?? "").trim()) errors.push(`${label}: ${what} is required.`);
    };
    need(llm.provider, "LLM provider");
    need(llm.model, "LLM model");
    need(tts.provider, "TTS provider");
    if (n.id === g.start) {
      const stt = asRecord(n.config.stt);
      need(stt.provider, "STT provider");
    }
  }
  for (const n of g.steps)
    for (const i of stepIssues(g, n.id)) if (i.tab === "advanced") errors.push(`${nameIn(g, n.id)}: ${i.message}`);
  for (const e of g.routes) {
    if (blank(e.condition)) errors.push(`The route from ${nameIn(g, e.from)} to ${nameIn(g, e.to)} needs a condition.`);
    // The server counts bytes (server/flow/flow.go), not characters.
    else if (new TextEncoder().encode(e.condition).length > MAX_CONDITION)
      errors.push(`The route from ${nameIn(g, e.from)} to ${nameIn(g, e.to)} has a condition longer than ${MAX_CONDITION} bytes.`);
  }
  errors.push(...flowIssues(g, limits).filter((m) => !m.startsWith("Choose speech-to-text")));
  return errors;
}
