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
  // A one-node agent exposes every selected tool, so its tool list needs no graph form.
  const plain = g.nodes.length === 1 && g.edges.length === 0 && !g.handoff_cue;
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

export function connectError(g: GraphDraft, from: string, to: string, condition: string): string | null {
  if (from === to) return "A node cannot hand off to itself.";
  if (!condition.trim()) return "A handoff needs a condition.";
  if (condition.length > MAX_CONDITION) return `The condition is limited to ${MAX_CONDITION} characters.`;
  if (g.edges.some((e) => e.from === from && e.to === to)) return "Those nodes are already connected that way.";
  if (g.edges.length >= MAX_EDGES) return `A graph has at most ${MAX_EDGES} handoffs.`;
  return null;
}

/** Adds the handoff, or returns the same graph when the server would reject it. */
export function connect(g: GraphDraft, from: string, to: string, condition: string): GraphDraft {
  if (connectError(g, from, to, condition)) return g;
  return { ...g, edges: [...g.edges, { id: newId(), from, to, condition: condition.trim() }] };
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
  return errors;
}
