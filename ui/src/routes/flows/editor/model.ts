// Pure graph-editing rules for the agent designer. They mirror the server's
// validation (server/agent/graph.go): the server stays the authority, this
// keeps the canvas from offering what it would reject.

export const MAX_NODES = 8;
export const MAX_EDGES = 32;
const MAX_CONDITION = 512;

export type NodeConfig = Record<string, unknown>;
export type GraphNode = {
  id: string;
  name: string;
  config: NodeConfig;
  tools: string[];
  position: { x: number; y: number };
};
export type GraphEdge = { id: string; from: string; to: string; condition: string };
export type HandoffCue = { name: string; after_ms?: number };
export type GraphDraft = {
  entry: string;
  nodes: GraphNode[];
  edges: GraphEdge[];
  handoff_cue?: HandoffCue;
};

// Settings that belong to the call, not to one agent: only the entry node has them.
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
export function emptyFlow(): GraphDraft {
  return { entry: "", nodes: [], edges: [] };
}

/** "New step", then "New step 2", "New step 3"… so step names stay unique. */
export function uniqueName(g: GraphDraft, base: string): string {
  const taken = new Set(g.nodes.map((n) => slug(n.name)));
  if (!taken.has(slug(base))) return base;
  for (let i = 2; ; i++) if (!taken.has(slug(`${base} ${i}`))) return `${base} ${i}`;
}

const blank = (v: unknown) => !String(v ?? "").trim();
const nameIn = (g: GraphDraft, id: string) => g.nodes.find((n) => n.id === id)?.name.trim() || "Untitled step";

/** What one step still needs before it can take calls, by the panel tab that fixes it. */
export function stepIssues(g: GraphDraft, id: string): StepIssue[] {
  const n = g.nodes.find((x) => x.id === id);
  if (!n) return [];
  const issues: StepIssue[] = [];
  if (!slug(n.name)) issues.push({ tab: "basics", message: "Give the step a name." });
  else if (g.nodes.some((o) => o.id !== id && slug(o.name) === slug(n.name))) issues.push({ tab: "basics", message: "Another step has this name." });
  const llm = asRecord(n.config.llm);
  if (blank(llm.provider) || blank(llm.model)) issues.push({ tab: "basics", message: "Choose a model." });
  if (blank(asRecord(n.config.tts).provider)) issues.push({ tab: "basics", message: "Choose a voice." });
  for (const e of g.edges)
    if (e.from === id && blank(e.condition)) issues.push({ tab: "routes", message: `Give the route to ${nameIn(g, e.to)} a condition.` });
  for (const [cap, what] of [["llm", "model"], ["tts", "voice"], ["stt", "listening"]] as const) {
    const stage = asRecord(n.config[cap]);
    const fallbacks = Array.isArray(stage.fallbacks) ? stage.fallbacks.map(asRecord) : [];
    // The server allows a chain of 3 (the primary and 2 fallbacks) with no repeated entry (server/agent/fallback.go).
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
const SUMMARIZING = new Set(["google", "nvidia", "openai", "xai"]);

/** Problems that belong to the whole flow (header state line and Flow settings). */
export function flowIssues(g: GraphDraft): string[] {
  if (g.nodes.length === 0) return [];
  const start = g.nodes.find((n) => n.id === g.entry);
  if (!start) return ["Choose a start step."];
  const out: string[] = [];
  if (blank(asRecord(start.config.stt).provider)) out.push("Choose speech-to-text in Flow settings.");
  const rec = asRecord(start.config.recording);
  if (rec.enabled === true && rec.disclosure_confirmed !== true) out.push("Confirm that callers are told the call is recorded.");
  // Ranges from server/agent/config.go: 0 or unset means the server default.
  const vad = start.config.vad_stop_ms;
  if (typeof vad === "number" && vad !== 0 && (vad < 200 || vad > 2000)) out.push("Wait after the caller stops must be 200 to 2000 ms.");
  const ctx = asRecord(start.config.context);
  if (typeof ctx.max_messages === "number" && (ctx.max_messages < 0 || ctx.max_messages === 1)) out.push("Max messages must be at least 2.");
  if (typeof ctx.max_tokens === "number" && ctx.max_tokens < 0) out.push("Max tokens cannot be negative.");
  if (asRecord(ctx.summarization).enabled === true)
    for (const n of g.nodes) {
      const provider = String(asRecord(n.config.llm).provider ?? "");
      if (provider && !SUMMARIZING.has(provider))
        out.push(`${nameIn(g, n.id)} uses ${provider}, which cannot summarise older turns. Turn summaries off or change its model.`);
    }
  return out;
}

/** A step no route leads to; callers can never reach it. The start step is always reachable. */
export function isUnreachable(g: GraphDraft, id: string): boolean {
  return id !== g.entry && !g.edges.some((e) => e.to === id);
}

/** Names of the steps no path from the start step reaches (a step reached only from an unreachable step counts). */
export function unreachableSteps(g: GraphDraft): string[] {
  const seen = new Set<string>(g.nodes.some((n) => n.id === g.entry) ? [g.entry] : []);
  const queue = [...seen];
  while (queue.length > 0) {
    const id = queue.shift()!;
    for (const e of g.edges) if (e.from === id && !seen.has(e.to)) (seen.add(e.to), queue.push(e.to));
  }
  return g.nodes.filter((n) => !seen.has(n.id)).map((n) => n.name || "Untitled step");
}

/** A route drawn on the graph: no condition yet, the step panel asks for one. */
export function addDraftRoute(g: GraphDraft, from: string, to: string): GraphDraft {
  const known = (id: string) => g.nodes.some((n) => n.id === id);
  if (from === to || !known(from) || !known(to) || g.edges.length >= MAX_EDGES) return g;
  if (g.edges.some((e) => e.from === from && e.to === to)) return g;
  return { ...g, edges: [...g.edges, { id: newId(), from, to, condition: "" }] };
}

/**
 * Reads a stored agent config, flat or graph, as a draft. A flat agent exposes
 * every MCP tool selected on its version; as a graph's entry node it must list
 * them (selectedTools, "<mcp_server_id>/<remote_name>"), or adding a second
 * node would silently remove its tools.
 */
export function fromAgentConfig(raw: unknown, selectedTools: string[] = []): GraphDraft {
  const cfg = asRecord(raw);
  if (!Array.isArray(cfg.nodes)) {
    return {
      entry: "main",
      nodes: [{ id: "main", name: "main", config: cfg, tools: [...selectedTools], position: { x: 0, y: 0 } }],
      edges: [],
    };
  }
  const nodes = cfg.nodes.map((n, i): GraphNode => {
    const r = asRecord(n);
    const pos = asRecord(r.position);
    return {
      id: String(r.id ?? ""),
      name: String(r.name ?? ""),
      config: asRecord(r.config),
      tools: Array.isArray(r.tools) ? r.tools.map(String) : [],
      position: { x: Number(pos.x ?? i * 280), y: Number(pos.y ?? 0) },
    };
  });
  const edges = (Array.isArray(cfg.edges) ? cfg.edges : []).map((e): GraphEdge => {
    const r = asRecord(e);
    return { id: String(r.id), from: String(r.from), to: String(r.to), condition: String(r.condition ?? "") };
  });
  const cue = asRecord(cfg.handoff_cue);
  return {
    entry: String(cfg.entry ?? ""),
    nodes,
    edges,
    ...(cue.name ? { handoff_cue: { name: String(cue.name), ...(cue.after_ms ? { after_ms: Number(cue.after_ms) } : {}) } } : {}),
  };
}

/** The config to send: the flat form for a plain one-node agent, else the graph. */
export function toPayload(g: GraphDraft): unknown {
  // A stored flat agent opens as one step named "main" (fromAgentConfig) and goes back flat, exposing
  // every selected tool. A step the user has named needs the graph form, or its name is lost.
  const only = g.nodes[0];
  const plain = g.nodes.length === 1 && g.edges.length === 0 && !g.handoff_cue && only.id === "main" && only.name === "main";
  if (plain) return g.nodes[0].config;
  return {
    entry: g.entry,
    nodes: g.nodes.map((n) => {
      const config = { ...n.config };
      if (n.id !== g.entry) for (const k of CALL_LEVEL) delete config[k];
      return { id: n.id, name: n.name, config, ...(n.tools.length ? { tools: n.tools } : {}), position: n.position };
    }),
    edges: g.edges,
    ...(g.handoff_cue ? { handoff_cue: g.handoff_cue } : {}),
  };
}

/** A new node starts from the entry's model and voice (without its prompt) so it is valid at once. */
export function addNode(g: GraphDraft, name: string): GraphDraft {
  if (g.nodes.length >= MAX_NODES) return g;
  if (g.nodes.length === 0) {
    const id = newId();
    return { ...g, entry: id, nodes: [{ id, name, config: {}, tools: [], position: { x: 0, y: 0 } }] };
  }
  const entry = g.nodes.find((n) => n.id === g.entry);
  const llm = { ...asRecord(entry?.config.llm) };
  delete llm.system_prompt;
  const config: NodeConfig = { llm, tts: { ...asRecord(entry?.config.tts) } };
  const last = g.nodes[g.nodes.length - 1];
  const position = { x: (last?.position.x ?? 0) + 280, y: last?.position.y ?? 0 };
  return { ...g, nodes: [...g.nodes, { id: newId(), name, config, tools: [], position }] };
}

export function removeNode(g: GraphDraft, id: string): GraphDraft {
  if (id === g.entry || !g.nodes.some((n) => n.id === id)) return g;
  return {
    ...g,
    nodes: g.nodes.filter((n) => n.id !== id),
    edges: g.edges.filter((e) => e.from !== id && e.to !== id),
  };
}

export function updateNode(g: GraphDraft, id: string, patch: Partial<Omit<GraphNode, "id">>): GraphDraft {
  return { ...g, nodes: g.nodes.map((n) => (n.id === id ? { ...n, ...patch } : n)) };
}

export function updateEdge(g: GraphDraft, id: string, condition: string): GraphDraft {
  return { ...g, edges: g.edges.map((e) => (e.id === id ? { ...e, condition } : e)) };
}

export function removeEdge(g: GraphDraft, id: string): GraphDraft {
  return { ...g, edges: g.edges.filter((e) => e.id !== id) };
}

/** Makes a node the entry, moving the call-level settings with it. */
export function setEntry(g: GraphDraft, id: string): GraphDraft {
  if (id === g.entry || !g.nodes.some((n) => n.id === id)) return g;
  const old = g.nodes.find((n) => n.id === g.entry);
  const moved: NodeConfig = {};
  for (const k of CALL_LEVEL) if (old && k in old.config) moved[k] = old.config[k];
  return {
    ...g,
    entry: id,
    nodes: g.nodes.map((n) => {
      if (n.id === g.entry) {
        const config = { ...n.config };
        for (const k of CALL_LEVEL) delete config[k];
        return { ...n, config };
      }
      return n.id === id ? { ...n, config: { ...n.config, ...moved } } : n;
    }),
  };
}

/** Problems the server would reject, in plain words. Empty means publishable. */
export function validate(g: GraphDraft): string[] {
  const errors: string[] = [];
  if (g.nodes.length === 0 || g.nodes.length > MAX_NODES) errors.push(`A graph has 1 to ${MAX_NODES} nodes.`);
  if (!g.nodes.some((n) => n.id === g.entry)) errors.push("Choose an entry node.");
  const cue = g.handoff_cue;
  if (cue && !/^[a-z0-9_-]{1,32}$/.test(cue.name)) errors.push("The handoff sound name uses lower-case letters, digits, _ and - (up to 32).");
  if (cue?.after_ms !== undefined && (cue.after_ms < 0 || cue.after_ms > 10000)) errors.push("The handoff sound starts after 0 to 10000 ms.");
  const slugs = new Set<string>();
  for (const n of g.nodes) {
    const label = n.name.trim() || n.id;
    const s = slug(n.name);
    if (!s) errors.push(`Node ${label} needs a name with a letter or digit.`);
    else if (slugs.has(s)) errors.push(`Node names must be unique (${label}).`);
    slugs.add(s);
    const llm = asRecord(n.config.llm);
    const tts = asRecord(n.config.tts);
    const need = (ok: unknown, what: string) => {
      if (!String(ok ?? "").trim()) errors.push(`${label}: ${what} is required.`);
    };
    need(llm.provider, "LLM provider");
    need(llm.model, "LLM model");
    need(tts.provider, "TTS provider");
    if (n.id === g.entry) {
      const stt = asRecord(n.config.stt);
      need(stt.provider, "STT provider");
    }
  }
  for (const n of g.nodes)
    for (const i of stepIssues(g, n.id)) if (i.tab === "advanced") errors.push(`${nameIn(g, n.id)}: ${i.message}`);
  for (const e of g.edges) {
    if (blank(e.condition)) errors.push(`The route from ${nameIn(g, e.from)} to ${nameIn(g, e.to)} needs a condition.`);
    // The server counts bytes (server/agent/graph.go), not characters.
    else if (new TextEncoder().encode(e.condition).length > MAX_CONDITION)
      errors.push(`The route from ${nameIn(g, e.from)} to ${nameIn(g, e.to)} has a condition longer than ${MAX_CONDITION} bytes.`);
  }
  errors.push(...flowIssues(g).filter((m) => !m.startsWith("Choose speech-to-text")));
  return errors;
}
