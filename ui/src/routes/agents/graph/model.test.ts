import { describe, expect, it } from "vitest";
import {
  MAX_NODES,
  addDraftRoute,
  addNode,
  emptyFlow,
  flowIssues,
  fromAgentConfig,
  isUnreachable,
  removeNode,
  setEntry,
  slug,
  stepIssues,
  toPayload,
  uniqueName,
  validate,
} from "./model";

const stage = { provider: "google" };
const flat = {
  stt: { ...stage, model: "chirp" },
  llm: { ...stage, model: "gemini", system_prompt: "You are the front desk." },
  tts: { ...stage, voice: "v" },
  vad_stop_ms: 400,
};

describe("graph model", () => {
  it("reads a flat config as one entry node", () => {
    const g = fromAgentConfig(flat);
    expect(g.nodes).toHaveLength(1);
    expect(g.entry).toBe(g.nodes[0].id);
    expect(g.nodes[0].config).toEqual(flat);
    expect(validate(g)).toEqual([]);
  });

  it("round-trips a one-node draft back to the flat config", () => {
    expect(toPayload(fromAgentConfig(flat))).toEqual(flat);
  });

  it("reads a stored graph", () => {
    const stored = {
      entry: "front",
      nodes: [
        { id: "front", name: "Front", config: flat, position: { x: 1, y: 2 } },
        { id: "sales", name: "Sales", config: { llm: flat.llm, tts: flat.tts }, tools: ["srv/lookup"] },
      ],
      edges: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      handoff_cue: { name: "chime", after_ms: 200 },
    };
    const g = fromAgentConfig(stored);
    expect(g.nodes[0].position).toEqual({ x: 1, y: 2 });
    expect(g.nodes[1].tools).toEqual(["srv/lookup"]);
    expect(g.handoff_cue).toEqual({ name: "chime", after_ms: 200 });
    expect(toPayload(g)).toMatchObject({ entry: "front", edges: stored.edges, handoff_cue: stored.handoff_cue });
  });

  it("addNode copies the entry's llm and tts and stops at the maximum", () => {
    let g = fromAgentConfig(flat);
    g = addNode(g, "Sales");
    expect(g.nodes[1].name).toBe("Sales");
    expect((g.nodes[1].config.llm as { provider: string }).provider).toBe("google");
    expect((g.nodes[1].config.llm as { system_prompt?: string }).system_prompt).toBeUndefined();
    expect(g.nodes[1].config.stt).toBeUndefined();
    for (let i = 0; i < MAX_NODES + 2; i++) g = addNode(g, `Node ${i}`);
    expect(g.nodes).toHaveLength(MAX_NODES);
  });

  it("removeNode drops incident edges and refuses the entry", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    const sales = g.nodes[1].id;
    g = addDraftRoute(g, g.entry, sales);
    expect(g.edges).toHaveLength(1);
    expect(removeNode(g, g.entry)).toBe(g);
    const after = removeNode(g, sales);
    expect(after.nodes).toHaveLength(1);
    expect(after.edges).toHaveLength(0);
  });

  it("validate reports duplicate slugs and missing stage fields", () => {
    let g = addNode(addNode(fromAgentConfig(flat), "Sales Team"), "sales team");
    expect(validate(g).join(" ")).toMatch(/unique/);
    g = fromAgentConfig({ ...flat, llm: { provider: "google", model: "" } });
    expect(validate(g).join(" ")).toMatch(/LLM model/);
  });

  it("setEntry moves the call-level settings to the new entry", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    const sales = g.nodes[1].id;
    g = setEntry(g, sales);
    expect(g.entry).toBe(sales);
    expect(g.nodes[1].config.stt).toEqual(flat.stt);
    expect(g.nodes[1].config.vad_stop_ms).toBe(400);
    expect(g.nodes[0].config.stt).toBeUndefined();
    expect(g.nodes[0].config.vad_stop_ms).toBeUndefined();
    expect(validate(g)).toEqual([]);
  });

  it("toPayload keeps call-level settings off non-entry nodes", () => {
    let g = addNode(fromAgentConfig(flat), "Sales");
    g = { ...g, nodes: g.nodes.map((n, i) => (i === 1 ? { ...n, config: { ...n.config, stt: flat.stt, vad_stop_ms: 300 } } : n)) };
    const payload = toPayload(g) as { nodes: { config: Record<string, unknown> }[] };
    expect(payload.nodes[1].config.stt).toBeUndefined();
    expect(payload.nodes[1].config.vad_stop_ms).toBeUndefined();
  });

  it("needs only providers and a model, and never emits credential_id", () => {
    const g = addNode(fromAgentConfig(flat), "Sales");
    expect(validate(g)).toEqual([]);
    expect(JSON.stringify(toPayload(g))).not.toContain("credential");
    expect(validate(fromAgentConfig({ ...flat, stt: {} })).join(" ")).toMatch(/STT provider/);
  });

  it("slug matches the server's tool-name rule", () => {
    expect(slug("Sales Team!")).toBe("sales_team");
  });

  it("seeds a flat agent's entry node with the agent's selected MCP tools", () => {
    const g = fromAgentConfig(flat, ["srv/lookup"]);
    expect(g.nodes[0].tools).toEqual(["srv/lookup"]);
    // a one-node agent still publishes as the flat config: it exposes every selected tool anyway
    expect(toPayload(g)).toEqual(flat);
    const two = addNode(g, "Sales");
    const payload = toPayload(two) as { nodes: { tools?: string[] }[] };
    expect(payload.nodes[0].tools).toEqual(["srv/lookup"]);
    expect(payload.nodes[1].tools).toBeUndefined();
  });

  it("validate rejects a handoff sound name the server would reject", () => {
    const base = addNode(fromAgentConfig(flat), "Sales");
    expect(validate({ ...base, handoff_cue: { name: "Bad Name" } }).join(" ")).toMatch(/sound name/);
    expect(validate({ ...base, handoff_cue: { name: "chime", after_ms: 20000 } }).join(" ")).toMatch(/after/);
    expect(validate({ ...base, handoff_cue: { name: "chime", after_ms: 300 } })).toEqual([]);
  });
});

describe("flow authoring", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const two = () =>
    fromAgentConfig({
      entry: "a",
      nodes: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      edges: [],
    });

  it("starts a blank flow with no steps, and the first step added becomes the start step", () => {
    const g = addNode(emptyFlow(), "New step");
    expect(g.nodes).toHaveLength(1);
    expect(g.entry).toBe(g.nodes[0].id);
    expect(g.nodes[0].config).toEqual({});
  });

  it("names new steps uniquely", () => {
    const g = addNode(addNode(emptyFlow(), "New step"), "New step 2");
    expect(uniqueName(emptyFlow(), "New step")).toBe("New step");
    expect(uniqueName(g, "New step")).toBe("New step 3");
  });

  it("adds a draft route with an empty condition and refuses self, duplicate and unknown routes", () => {
    const g = addDraftRoute(two(), "a", "b");
    expect(g.edges).toEqual([expect.objectContaining({ from: "a", to: "b", condition: "" })]);
    expect(addDraftRoute(g, "a", "b")).toBe(g);
    expect(addDraftRoute(g, "a", "a")).toBe(g);
    expect(addDraftRoute(g, "a", "zzz")).toBe(g);
  });

  it("reports what a step still needs, by the tab that fixes it", () => {
    const blank = addNode(emptyFlow(), "New step");
    expect(stepIssues(blank, blank.entry)).toEqual([
      { tab: "basics", message: "Choose a model." },
      { tab: "basics", message: "Choose a voice." },
    ]);
    const routed = addDraftRoute(two(), "a", "b");
    expect(stepIssues(routed, "a")).toEqual([{ tab: "routes", message: "Give the route to Billing a condition." }]);
    expect(stepIssues(two(), "b")).toEqual([]);
  });

  it("flags duplicate and empty step names on the basics tab", () => {
    const g = { ...two(), nodes: two().nodes.map((n) => ({ ...n, name: n.id === "b" ? "reception" : n.name })) };
    expect(stepIssues(g, "b")).toContainEqual({ tab: "basics", message: "Another step has this name." });
    const unnamed = { ...two(), nodes: two().nodes.map((n) => ({ ...n, name: n.id === "b" ? "  " : n.name })) };
    expect(stepIssues(unnamed, "b")).toContainEqual({ tab: "basics", message: "Give the step a name." });
  });

  it("reports flow-level problems: speech-to-text and recording disclosure", () => {
    const noStt = addNode(emptyFlow(), "New step");
    expect(flowIssues(noStt)).toEqual(["Choose speech-to-text in Flow settings."]);
    const g = two();
    const rec = { ...g, nodes: g.nodes.map((n) => (n.id === "a" ? { ...n, config: { ...n.config, recording: { enabled: true, disclosure_confirmed: false } } } : n)) };
    expect(flowIssues(rec)).toEqual(["Confirm that callers are told the call is recorded."]);
    expect(flowIssues(g)).toEqual([]);
    expect(flowIssues(emptyFlow())).toEqual([]);
  });

  it("knows which steps no route leads to", () => {
    expect(isUnreachable(two(), "b")).toBe(true);
    expect(isUnreachable(two(), "a")).toBe(false);
    expect(isUnreachable(addDraftRoute(two(), "a", "b"), "b")).toBe(false);
  });

  it("blocks publishing while a route has no condition", () => {
    expect(validate(addDraftRoute(two(), "a", "b"))).toContain("The route from Reception to Billing needs a condition.");
  });
});

describe("server limits", () => {
  it("counts a route condition in bytes, as the server does", () => {
    const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
    const g = fromAgentConfig({
      entry: "a",
      nodes: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready } },
        { id: "b", name: "Billing", config: ready },
      ],
      edges: [{ id: "e1", from: "a", to: "b", condition: "é".repeat(300) }],
    });
    expect("é".repeat(300).length).toBeLessThan(512);
    expect(validate(g)).toContain("The route from Reception to Billing has a condition longer than 512 bytes.");
  });
});

describe("what the server will accept", () => {
  const ready = { llm: { provider: "google", model: "gemini" }, tts: { provider: "google", voice: "v" } };
  const one = (start: Record<string, unknown> = {}, step: Record<string, unknown> = {}) =>
    fromAgentConfig({
      entry: "a",
      nodes: [
        { id: "a", name: "Reception", config: { stt: { provider: "google" }, ...ready, ...start } },
        { id: "b", name: "Billing", config: { ...ready, ...step } },
      ],
      edges: [{ id: "e1", from: "a", to: "b", condition: "invoice" }],
    });

  it("keeps a named single step as a graph so its name survives publishing", () => {
    const g = addNode(emptyFlow(), "Reception");
    const payload = toPayload(g) as { nodes: { name: string }[] };
    expect(payload.nodes.map((n) => n.name)).toEqual(["Reception"]);
  });

  it("flags turn-taking and memory values outside the server's ranges", () => {
    expect(flowIssues(one({ vad_stop_ms: 100 }))).toContain("Wait after the caller stops must be 200 to 2000 ms.");
    expect(flowIssues(one({ context: { max_messages: 1 } }))).toContain("Max messages must be at least 2.");
    expect(flowIssues(one({ context: { max_tokens: -5 } }))).toContain("Max tokens cannot be negative.");
    expect(validate(one({ vad_stop_ms: 2500 }))).toContain("Wait after the caller stops must be 200 to 2000 ms.");
  });

  it("flags summaries when a step's model provider cannot summarise", () => {
    const g = one({ context: { summarization: { enabled: true } } }, { llm: { provider: "groq", model: "llama" } });
    expect(flowIssues(g)).toContain("Billing uses groq, which cannot summarise older turns. Turn summaries off or change its model.");
  });

  it("flags too many fallbacks and a fallback that repeats the model", () => {
    const three = one({}, { llm: { ...ready.llm, fallbacks: [{ provider: "openai", model: "a" }, { provider: "openai", model: "b" }, { provider: "xai", model: "c" }] } });
    expect(stepIssues(three, "b")).toContainEqual({ tab: "advanced", message: "Use at most 2 model fallbacks." });
    const dup = one({}, { tts: { ...ready.tts, fallbacks: [{ provider: "google", voice: "v" }] } });
    expect(stepIssues(dup, "b")).toContainEqual({ tab: "advanced", message: "A voice fallback repeats the voice or another fallback." });
    expect(validate(dup)).toContain("Billing: A voice fallback repeats the voice or another fallback.");
  });
});
