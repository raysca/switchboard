// Pure flow-editing rules for the flow designer. They mirror the server's
// validation (server/flow/flow.go): the server stays the authority, this
// keeps the canvas from offering what it would reject.

import type { ToolSelection } from "../../../api/client";

export const MAX_STEPS = 8;
export const MAX_ROUTES = 32;
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
const CALL_LEVEL = ["stt", "vad_stop_ms", "stt_p99_ms", "recording", "context", "async_tools"] as const;

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
export type StepIssue = { tab: StepTab; message: string };

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
  return issues;
}

/** The server's chain limit, primary excluded. */
export const MAX_FALLBACKS = 2;
const SUMMARIZING = new Set(["google", "echo", "nvidia", "openai", "xai"]);

/** Problems that belong to the whole flow (header state line and Flow settings). */
export function flowIssues(g: FlowDraft): string[] {
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
  if (asRecord(ctx.summarization).enabled === true)
    for (const n of g.steps) {
      const provider = String(asRecord(n.config.llm).provider ?? "");
      if (provider && !SUMMARIZING.has(provider))
        out.push(`${nameIn(g, n.id)} uses ${provider}, which cannot summarise older turns. Turn summaries off or change its model.`);
    }
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

/** A stored flat flow opens as one step named "main" and goes back flat; a step the user has named needs the flow form. */
function isFlat(g: FlowDraft): boolean {
  const only = g.steps[0];
  return g.steps.length === 1 && g.routes.length === 0 && !g.route_cue && only.id === "main" && only.name === "main";
}

/** The config to send: the flat form for a plain one-step flow, else the flow. */
export function toPayload(g: FlowDraft): unknown {
  // A stored flat flow opens as one step named "main" (fromFlowConfig) and goes back flat, exposing
  // every selected tool. A step the user has named needs the flow form, or its name is lost.
  if (isFlat(g)) return g.steps[0].config;
  return {
    start: g.start,
    steps: g.steps.map((n) => {
      const config = { ...n.config };
      if (n.id !== g.start) for (const k of CALL_LEVEL) delete config[k];
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
    steps: g.steps.map((n) => ({ ...n, tools: isFlat(g) ? [...keys] : n.tools.filter((t) => keys.includes(t)) })),
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
  return {
    ...g,
    start: id,
    steps: g.steps.map((n) => {
      if (n.id === g.start) {
        const config = { ...n.config };
        for (const k of CALL_LEVEL) delete config[k];
        return { ...n, config };
      }
      return n.id === id ? { ...n, config: { ...n.config, ...moved } } : n;
    }),
  };
}

/** Problems the server would reject, in plain words. Empty means publishable. */
export function validate(g: FlowDraft): string[] {
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
  errors.push(...flowIssues(g).filter((m) => !m.startsWith("Choose speech-to-text")));
  return errors;
}
